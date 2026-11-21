package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/skills"
	"agentd/internal/system"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

type Runtime struct {
	DB          *sql.DB
	Tasks       *tasks.Store
	Assignments *assignments.Store
	Gateway     *gateway.Gateway
	Tools       tools.Registry
	Skills      skills.Registry
	Model       model.Provider
	ModelID     string
	Now         func() time.Time
	LeaseTTL    time.Duration
	MaxSteps    int
	BaseBackoff time.Duration

	MaxRejectedCompletions int
	Housekeeping           func(context.Context) error
	HeartbeatEvery         time.Duration
}

const rejectedCompletionPrefix = "Completion rejected:"

func (r *Runtime) RunOnce(ctx context.Context, owner string) (bool, error) {
	lease, err := r.Tasks.ClaimReady(ctx, owner, r.LeaseTTL)
	if err != nil || lease == nil {
		return false, err
	}
	return true, r.execute(ctx, *lease)
}

func (r *Runtime) execute(parent context.Context, lease tasks.Lease) error {
	ctx, stopHeartbeat := r.heartbeat(parent, lease)
	defer stopHeartbeat()
	task, err := r.Tasks.Get(ctx, lease.TaskID)
	if err != nil {
		return r.fail(ctx, lease, task, err)
	}
	if _, err := r.Skills.Resolve(task.Skills); err != nil {
		return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Blocked, Result: map[string]string{"error": err.Error()}})
	}
	rn, err := r.openRun(ctx, task)
	if err != nil {
		return r.fail(ctx, lease, task, err)
	}
	if !task.DeadlineAt.IsZero() && !r.Now().Before(task.DeadlineAt) {
		return r.block(ctx, lease, rn.ID, map[string]string{"error": "deadline passed"})
	}
	maxSteps := r.MaxSteps
	if task.MaxSteps > 0 && task.MaxSteps < maxSteps {
		maxSteps = task.MaxSteps
	}
	for {
		if lease, err = r.Tasks.Renew(ctx, lease, r.LeaseTTL); err != nil {
			return err
		}
		msgs, err := r.loadMessages(ctx, rn.ID)
		if err != nil {
			return r.fail(ctx, lease, task, err)
		}
		if len(msgs) < 2 {
			return r.block(ctx, lease, rn.ID, map[string]string{"error": "run has no opening messages"})
		}

		if seq, pending := pendingCalls(msgs); len(pending) > 0 {
			for i, call := range pending {
				out, err := r.Gateway.Handle(ctx, gateway.Call{RunID: rn.ID, TaskID: task.ID, AssignmentID: task.AssignmentID, Seq: seq, Call: call, Lease: lease})
				if err != nil {
					return r.fail(ctx, lease, task, err)
				}
				switch out.Kind {
				case gateway.WaitApproval:
					return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.WaitingApproval})
				case gateway.WaitEvent:
					return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.WaitingEvent})
				case gateway.Paused:
					return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Ready})
				case gateway.Blocked:
					return r.block(ctx, lease, rn.ID, map[string]string{"error": "action outcome needs manual reconciliation", "detail": out.Content})
				}
				reply := model.Message{Role: model.RoleTool, ToolCallID: call.ID, Content: out.Content}
				if err := r.appendMessage(ctx, rn.ID, len(msgs)+i, reply); err != nil {
					return r.fail(ctx, lease, task, err)
				}
			}
			continue
		}

		last := msgs[len(msgs)-1]
		if last.Role == model.RoleAssistant {
			missing, err := r.missingEvidence(ctx, task)
			if err != nil {
				return r.fail(ctx, lease, task, err)
			}
			unseen, err := r.unseenEvents(ctx, task)
			if err != nil {
				return r.fail(ctx, lease, task, err)
			}
			if unseen > 0 {
				nudge := model.Message{Role: model.RoleUser, Content: fmt.Sprintf(
					"%d new event(s) were linked to this task while you worked. Call task_events to read them before finishing.", unseen)}
				if err := r.appendMessage(ctx, rn.ID, len(msgs), nudge); err != nil {
					return r.fail(ctx, lease, task, err)
				}
				continue
			}
			if len(missing) == 0 {
				return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Completed, Result: map[string]string{"summary": last.Content},
					FinishRun: rn.ID, RunStatus: "completed"})
			}
			if rejectedCompletions(msgs) >= r.MaxRejectedCompletions {
				return r.block(ctx, lease, rn.ID, map[string]string{
					"error": fmt.Sprintf("model claimed completion %d times without evidence of %v", r.MaxRejectedCompletions+1, missing)})
			}
			nudge := model.Message{Role: model.RoleUser, Content: fmt.Sprintf(
				"%s no successful %v action is recorded for this task. Continue working.", rejectedCompletionPrefix, missing)}
			if err := r.appendMessage(ctx, rn.ID, len(msgs), nudge); err != nil {
				return r.fail(ctx, lease, task, err)
			}
			continue
		}

		if rn.Steps >= maxSteps {
			return r.block(ctx, lease, rn.ID, map[string]string{"error": fmt.Sprintf("step limit of %d reached", maxSteps)})
		}
		if s, err := system.Get(ctx, r.DB); err != nil || s.Halted {
			if err != nil {
				return r.fail(ctx, lease, task, err)
			}
			return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Ready})
		}
		req := model.Request{Messages: msgs, Tools: r.Tools.Definitions(task.Allows)}
		res, err := r.reserve(ctx, task, rn.ID, req)
		if err != nil {
			return r.fail(ctx, lease, task, err)
		}
		if res.exhausted != "" {
			if err := r.Assignments.Pause(ctx, task.AssignmentID, assignments.PausedByBudget, res.exhausted, "budget"); err != nil && !errors.Is(err, assignments.ErrTerminated) {
				return r.fail(ctx, lease, task, err)
			}
			return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Ready})
		}
		if res.wait {
			return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Ready, NextRunAt: r.Now().Add(r.BaseBackoff)})
		}
		resp, err := r.Model.Complete(ctx, req)
		if err != nil {
			if merr := r.markReservation(context.WithoutCancel(ctx), r.DB, res.id, "unknown"); merr != nil {
				err = errors.Join(err, merr)
			}
			return r.fail(ctx, lease, task, fmt.Errorf("model: %w", err))
		}
		if resp.StopReason == model.StopRefusal || resp.StopReason == model.StopMaxTokens {
			if err := r.recordUsage(ctx, r.DB, task, rn.ID, res.id, resp); err != nil {
				return r.fail(ctx, lease, task, err)
			}
			return r.fail(ctx, lease, task, fmt.Errorf("model stopped with %s", resp.StopReason))
		}
		resp.Message.Role = model.RoleAssistant
		quarantineInvalidArguments(resp.Message.ToolCalls)
		if err := r.appendModelMessage(ctx, lease, task, &rn, len(msgs), res.id, resp); err != nil {
			return r.fail(ctx, lease, task, err)
		}
	}
}

func quarantineInvalidArguments(calls []model.ToolCall) {
	for i, c := range calls {
		if !json.Valid(c.Arguments) {
			calls[i].Arguments, _ = json.Marshal(string(c.Arguments))
		}
	}
}

func rejectedCompletions(msgs []model.Message) int {
	n := 0
	for i := len(msgs) - 1; i >= 0 && msgs[i].Role != model.RoleTool; i-- {
		if msgs[i].Role == model.RoleUser && strings.HasPrefix(msgs[i].Content, retryPrefix) {
			break
		}
		if msgs[i].Role == model.RoleUser && strings.HasPrefix(msgs[i].Content, rejectedCompletionPrefix) {
			n++
		}
	}
	return n
}

func pendingCalls(msgs []model.Message) (int, []model.ToolCall) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != model.RoleAssistant {
			continue
		}
		answered := map[string]bool{}
		for _, m := range msgs[i+1:] {
			if m.Role == model.RoleTool {
				answered[m.ToolCallID] = true
			}
		}
		var pending []model.ToolCall
		for _, c := range msgs[i].ToolCalls {
			if !answered[c.ID] {
				pending = append(pending, c)
			}
		}
		return i, pending
	}
	return -1, nil
}

func (r *Runtime) block(ctx context.Context, lease tasks.Lease, runID string, cause map[string]string) error {
	return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Blocked, Result: cause, FinishRun: runID, RunStatus: "blocked"})
}

func (r *Runtime) heartbeat(parent context.Context, lease tasks.Lease) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	every := r.HeartbeatEvery
	if every <= 0 {
		every = r.LeaseTTL / 3
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := r.Tasks.Renew(ctx, lease, r.LeaseTTL); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func (r *Runtime) fail(ctx context.Context, lease tasks.Lease, task tasks.Task, cause error) error {
	if errors.Is(cause, tasks.ErrLeaseLost) {
		return cause
	}
	if ctx.Err() != nil {
		r.Tasks.Release(context.WithoutCancel(ctx), lease, tasks.Release{Status: tasks.Ready})
		return cause
	}
	rel := tasks.Release{
		Status:       tasks.Retryable,
		NextRunAt:    r.Now().Add(r.BaseBackoff << task.AttemptCount),
		Result:       map[string]string{"error": cause.Error()},
		CountFailure: true,
	}
	if task.AttemptCount+1 >= task.MaxAttempts {
		rel.Status, rel.NextRunAt = tasks.Blocked, time.Time{}
	}
	if err := r.Tasks.Release(ctx, lease, rel); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (r *Runtime) Serve(ctx context.Context, owner string, workers int, poll time.Duration, log *slog.Logger) {
	done := make(chan struct{}, workers+1)
	go func() {
		defer func() { done <- struct{}{} }()
		for ctx.Err() == nil {
			if r.Housekeeping != nil {
				if err := r.Housekeeping(ctx); err != nil && ctx.Err() == nil {
					log.Error("housekeeping", "err", err)
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(poll):
			}
		}
	}()
	for i := range workers {
		id := fmt.Sprintf("%s-w%d", owner, i)
		go func() {
			defer func() { done <- struct{}{} }()
			for ctx.Err() == nil {
				worked, err := r.RunOnce(ctx, id)
				if err != nil && ctx.Err() == nil {
					log.Error("task attempt failed", "worker", id, "err", err)
				}
				if !worked {
					select {
					case <-ctx.Done():
					case <-time.After(poll):
					}
				}
			}
		}()
	}
	for range workers + 1 {
		<-done
	}
}
