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

	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/projects"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

type Runtime struct {
	DB          *sql.DB
	Tasks       *tasks.Store
	Projects    *projects.Store
	Gateway     *gateway.Gateway
	Tools       tools.Registry
	Model       model.Provider
	ModelID     string
	Now         func() time.Time
	LeaseTTL    time.Duration
	MaxSteps    int
	BaseBackoff time.Duration

	MaxRejectedCompletions int
}

const rejectedCompletionPrefix = "Completion rejected:"

func (r *Runtime) RunOnce(ctx context.Context, owner string) (bool, error) {
	lease, err := r.Tasks.ClaimReady(ctx, owner, r.LeaseTTL)
	if err != nil || lease == nil {
		return false, err
	}
	return true, r.execute(ctx, *lease)
}

func (r *Runtime) execute(ctx context.Context, lease tasks.Lease) error {
	task, err := r.Tasks.Get(ctx, lease.TaskID)
	if err != nil {
		return r.fail(ctx, lease, task, err)
	}
	rn, err := r.openRun(ctx, task)
	if err != nil {
		return r.fail(ctx, lease, task, err)
	}
	for {
		if lease, err = r.Tasks.Renew(ctx, lease, r.LeaseTTL); err != nil {
			return err
		}
		msgs, err := r.loadMessages(ctx, rn.ID)
		if err != nil {
			return r.fail(ctx, lease, task, err)
		}

		if seq, pending := pendingCalls(msgs); len(pending) > 0 {
			for i, call := range pending {
				out, err := r.Gateway.Handle(ctx, gateway.Call{RunID: rn.ID, TaskID: task.ID, ProjectID: task.ProjectID, Seq: seq, Call: call})
				if err != nil {
					return r.fail(ctx, lease, task, err)
				}
				switch out.Kind {
				case gateway.WaitApproval:
					return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.WaitingApproval})
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
			if len(missing) == 0 {
				if err := r.finishRun(ctx, rn.ID, "completed", nil); err != nil {
					return r.fail(ctx, lease, task, err)
				}
				return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Completed, Result: map[string]string{"summary": last.Content}})
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

		if rn.Steps >= r.MaxSteps {
			return r.block(ctx, lease, rn.ID, map[string]string{"error": fmt.Sprintf("step limit of %d reached", r.MaxSteps)})
		}
		resp, err := r.Model.Complete(ctx, model.Request{Messages: msgs, Tools: r.Tools.Definitions()})
		if err != nil {
			return r.fail(ctx, lease, task, fmt.Errorf("model: %w", err))
		}
		if resp.StopReason == model.StopRefusal || resp.StopReason == model.StopMaxTokens {
			return r.fail(ctx, lease, task, fmt.Errorf("model stopped with %s", resp.StopReason))
		}
		resp.Message.Role = model.RoleAssistant
		quarantineInvalidArguments(resp.Message.ToolCalls)
		if err := r.appendModelMessage(ctx, &rn, len(msgs), resp.Message); err != nil {
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
	if err := r.finishRun(ctx, runID, "blocked", cause); err != nil {
		return err
	}
	return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Blocked, Result: cause})
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
	done := make(chan struct{}, workers)
	for i := range workers {
		id := fmt.Sprintf("%s-w%d", owner, i)
		go func() {
			defer func() { done <- struct{}{} }()
			for ctx.Err() == nil {
				if _, err := r.Gateway.ExpireApprovals(ctx); err != nil && ctx.Err() == nil {
					log.Error("expire approvals", "err", err)
				}
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
	for range workers {
		<-done
	}
}
