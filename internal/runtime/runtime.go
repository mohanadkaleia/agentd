package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/notify"
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
	ModelLocal  bool
	Fallbacks   []Candidate
	Outbox      *notify.Outbox
	Now         func() time.Time
	LeaseTTL    time.Duration
	MaxSteps    int
	BaseBackoff time.Duration
	ToolWait    time.Duration

	MaxRejectedCompletions int
	Housekeeping           func(context.Context) error
	HeartbeatEvery         time.Duration
	// Opener supplies the opening messages of tasks that are not ordinary assignment work,
	// such as conversation turns.
	Opener Opener

	wakeOnce sync.Once
	wake     chan struct{}
}

// An Opener writes the opening messages for the tasks it recognises, in place of the
// assignment's system prompt and the task objective. It is called once, when the task's run
// is created; local says whether the run's model keeps data on this machine.
type Opener interface {
	Opening(ctx context.Context, t tasks.Task, a assignments.Assignment, local bool) ([]model.Message, bool, error)
}

// interactivePoll bounds how long a turn written by another process waits to be claimed.
const interactivePoll = 200 * time.Millisecond

// Wake makes the interactive worker look for work now instead of at its next poll.
func (r *Runtime) Wake() {
	select {
	case r.wakeup() <- struct{}{}:
	default:
	}
}

func (r *Runtime) wakeup() chan struct{} {
	r.wakeOnce.Do(func() { r.wake = make(chan struct{}, 1) })
	return r.wake
}

const (
	rejectedCompletionPrefix = "Completion rejected:"
	maxToolBackoff           = 15 * time.Minute
)

func (r *Runtime) RunOnce(ctx context.Context, owner string) (worked bool, err error) {
	return r.runOnce(ctx, owner, r.Tasks.ClaimReady)
}

// RunOnceInteractive runs one task a person is waiting on, if there is one.
func (r *Runtime) RunOnceInteractive(ctx context.Context, owner string) (worked bool, err error) {
	return r.runOnce(ctx, owner, r.Tasks.ClaimInteractive)
}

func (r *Runtime) runOnce(ctx context.Context, owner string, claim func(context.Context, string, time.Duration) (*tasks.Lease, error)) (worked bool, err error) {
	lease, err := claim(ctx, owner, r.LeaseTTL)
	if err != nil || lease == nil {
		return false, err
	}
	defer func() {
		if p := recover(); p != nil {
			worked, err = true, r.contain(ctx, *lease, p)
		}
	}()
	return true, r.execute(ctx, *lease)
}

func (r *Runtime) contain(ctx context.Context, lease tasks.Lease, p any) error {
	cause := fmt.Errorf("panic: %v", p)
	stack := debug.Stack()
	task, err := r.Tasks.Get(ctx, lease.TaskID)
	if err != nil {
		return fmt.Errorf("%w\n%s", errors.Join(cause, err), stack)
	}
	return fmt.Errorf("%w\n%s", r.fail(ctx, lease, task, cause), stack)
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
	if errors.Is(err, errNoModel) {
		return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Blocked, Result: map[string]string{"error": err.Error()}})
	}
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
					rel := tasks.Release{Status: tasks.WaitingEvent}
					if out.Content != "" {
						rel.Result = map[string]string{"waiting": out.Content}
					}
					return r.Tasks.Release(ctx, lease, rel)
				case gateway.Paused:
					return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Ready})
				case gateway.Blocked:
					if r.Outbox != nil {
						if _, err := r.Outbox.Notify(ctx, notify.Notification{AssignmentID: task.AssignmentID, TaskID: task.ID,
							DedupKey: "unknown-outcome:" + task.ID, Severity: assignments.SeverityAction,
							Title: "Did it happen? A task is waiting for you",
							Body: fmt.Sprintf("%q could not tell whether %s took effect, so it will not retry it. Check, then: agentd resolve <action-id> happened|not-happened",
								task.Title, call.Name),
						}); err != nil {
							return r.fail(ctx, lease, task, err)
						}
					}
					return r.block(ctx, lease, rn.ID, map[string]string{"error": "action outcome needs manual reconciliation", "detail": out.Content})
				case gateway.Unavailable:
					return r.waitForTool(ctx, lease, task, rn.ID, out)
				}
				reply := model.Message{Role: model.RoleTool, ToolCallID: call.ID, Content: out.Content}
				if err := r.appendMessage(ctx, rn.ID, len(msgs)+i, reply); err != nil {
					return r.fail(ctx, lease, task, err)
				}
			}
			continue
		}
		if n, err := r.deliverNotes(ctx, lease, rn.ID, len(msgs)); err != nil {
			return r.fail(ctx, lease, task, err)
		} else if n > 0 {
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
		a, err := r.Assignments.Get(ctx, task.AssignmentID)
		if err != nil {
			return r.fail(ctx, lease, task, err)
		}
		provider, err := r.pinned(ctx, &rn, task.AssignmentID, a.Policy, msgs)
		if err != nil {
			return r.block(ctx, lease, rn.ID, map[string]string{"error": err.Error()})
		}
		offered := func(name string) bool {
			return task.Allows(name) && a.Policy.Offers(name) && tools.Ready(r.Tools[name]) && tools.Permits(r.Tools[name], a.Builtin)
		}
		req := model.Request{Messages: msgs, Tools: r.Tools.Definitions(offered)}
		res, err := r.reserve(ctx, task, rn.ID, provider, req)
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
		resp, err := provider.Complete(ctx, req)
		if err != nil {
			if merr := r.markReservation(context.WithoutCancel(ctx), r.DB, res.id, "unknown"); merr != nil {
				err = errors.Join(err, merr)
			}
			if ctx.Err() == nil {
				switched, ferr := r.fallback(ctx, &rn, task.AssignmentID, a.Policy, msgs, err)
				if ferr != nil {
					return r.fail(ctx, lease, task, ferr)
				}
				if switched {
					continue
				}
			}
			return r.fail(ctx, lease, task, fmt.Errorf("model: %w", err))
		}
		if resp.StopReason == model.StopRefusal || resp.StopReason == model.StopMaxTokens {
			if err := r.recordUsage(ctx, r.DB, task, rn, res.id, resp); err != nil {
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

func (r *Runtime) waitForTool(ctx context.Context, lease tasks.Lease, task tasks.Task, runID string, out gateway.Outcome) error {
	waited := r.Now().Sub(out.Since)
	if waited < r.ToolWait {
		delay := min(max(waited, r.BaseBackoff), maxToolBackoff)
		return r.Tasks.Release(ctx, lease, tasks.Release{Status: tasks.Ready, NextRunAt: r.Now().Add(delay),
			Result: map[string]string{"waiting": out.Content}})
	}
	if r.Outbox != nil {
		if _, err := r.Outbox.Notify(ctx, notify.Notification{AssignmentID: task.AssignmentID, TaskID: task.ID,
			DedupKey: "tool-unavailable:" + task.ID, Severity: assignments.SeverityAction,
			Title: "Task blocked: " + out.Content,
			Body:  fmt.Sprintf("%q waited %s for the tool to come back. Reconnect it, then run: agentd retry %s", task.Title, r.ToolWait, task.ID),
		}); err != nil {
			return r.fail(ctx, lease, task, err)
		}
	}
	return r.block(ctx, lease, runID, map[string]string{"error": fmt.Sprintf("%s; waited %s", out.Content, r.ToolWait)})
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

// Serve runs housekeeping, the given number of workers, and one more worker that only
// takes interactive tasks so a person is never queued behind long background work.
func (r *Runtime) Serve(ctx context.Context, owner string, workers int, poll time.Duration, log *slog.Logger) {
	done := make(chan struct{}, workers+2)
	go func() {
		defer func() { done <- struct{}{} }()
		for ctx.Err() == nil {
			if r.Housekeeping != nil {
				if err := r.housekeep(ctx); err != nil && ctx.Err() == nil {
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
	go func() {
		defer func() { done <- struct{}{} }()
		id := owner + "-interactive"
		for ctx.Err() == nil {
			worked, err := r.RunOnceInteractive(ctx, id)
			if err != nil && ctx.Err() == nil {
				log.Error("task attempt failed", "worker", id, "err", err)
			}
			if !worked {
				select {
				case <-ctx.Done():
				case <-r.wakeup():
				case <-time.After(min(poll, interactivePoll)):
				}
			}
		}
	}()
	for range workers + 2 {
		<-done
	}
}

func (r *Runtime) housekeep(ctx context.Context) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return r.Housekeeping(ctx)
}
