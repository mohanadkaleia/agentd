package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentd/internal/demo"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/policy"
	"agentd/internal/tasks"
)

type priced struct {
	inner model.Provider
	cost  int64
}

func (p priced) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	resp, err := p.inner.Complete(ctx, req)
	resp.Model = "test-model"
	resp.Usage = model.Usage{InputTokens: 100, OutputTokens: 10, CostMicros: p.cost}
	return resp, err
}

func (h *harness) count(query string, args ...any) int64 {
	h.t.Helper()
	var n int64
	if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestBudgetPausesProjectAndResumesSameRun(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.app.Runtime.Model = priced{inner: h.model, cost: 1_500_000}
	ctx := context.Background()

	h.drain()
	p, _ := h.app.Assignments.Get(ctx, h.assignmentID)
	if p.Status != "paused" || !strings.Contains(p.StatusReason, "LLM budget exhausted") {
		t.Fatalf("project = %s (%q), want paused for budget", p.Status, p.StatusReason)
	}
	h.expectStatus(tasks.Ready)
	if got := h.count(`SELECT SUM(cost_micros) FROM model_usage WHERE assignment_id = ?`, h.assignmentID); got != 3_000_000 {
		t.Fatalf("recorded spend = %d micros, want 3000000", got)
	}

	raised := demo.Policy
	raised.Limits.MaxLLMSpendUSD = 10
	if _, err := h.app.Assignments.SetPolicy(ctx, h.assignmentID, raised, "test"); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Assignments.Resume(ctx, h.assignmentID, "test"); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.decideOnly(true)
	h.drain()

	h.expectStatus(tasks.Completed)
	if got := h.count(`SELECT COUNT(*) FROM runs`); got != 1 {
		t.Fatalf("runs = %d, want 1 (pausing must not restart the run)", got)
	}
	if h.sent(demo.AirlineRefunds) != 1 {
		t.Fatal("refunds email was resent after the budget pause")
	}
	if got := h.count(`SELECT policy_version FROM assignments WHERE id = ?`, h.assignmentID); got != 2 {
		t.Fatalf("policy_version = %d, want 2", got)
	}
}

func emailScript(n int) func(model.Request) model.Message {
	return func(req model.Request) model.Message {
		done := 0
		for _, m := range req.Messages {
			if m.Role == model.RoleTool {
				done++
			}
		}
		if done < n {
			body, _ := json.Marshal(map[string]string{"to": demo.AirlineRefunds, "subject": fmt.Sprint("note ", done), "body": "b"})
			return model.Message{ToolCalls: []model.ToolCall{{ID: fmt.Sprint("e", done), Name: "send_email", Arguments: body}}}
		}
		return model.Message{Content: "sent what I could"}
	}
}

func TestToolRateLimitDeniesThenRecovers(t *testing.T) {
	h := newHarness(t, emailScript(3))
	ctx := context.Background()
	limited := demo.Policy
	limited.Limits.ToolRates = map[string]policy.RateLimit{"send_email": {Max: 2, Per: policy.Duration{Duration: 24 * time.Hour}}}
	if _, err := h.app.Assignments.SetPolicy(ctx, h.assignmentID, limited, "test"); err != nil {
		t.Fatal(err)
	}

	h.drain()
	h.expectStatus(tasks.Completed)
	if h.sent(demo.AirlineRefunds) != 2 {
		t.Fatalf("emails sent = %d, want 2", h.sent(demo.AirlineRefunds))
	}
	statuses := h.actionStatuses("send_email")
	if len(statuses) != 3 || statuses[2] != gateway.Denied {
		t.Fatalf("send_email actions = %v, want the third denied", statuses)
	}
	var reason string
	h.db.QueryRow(`SELECT result_json FROM actions WHERE status = 'denied'`).Scan(&reason)
	if !strings.Contains(reason, "rate limit reached: 2 send_email per 24h") {
		t.Fatalf("denial reason = %s", reason)
	}

	h.clock.Advance(25 * time.Hour)
	next, err := h.app.Tasks.Create(ctx, tasks.NewTask{AssignmentID: h.assignmentID, Title: "again", Objective: "send again"})
	if err != nil {
		t.Fatal(err)
	}
	h.app.Runtime.Model = scripted.Provider(emailScript(1))
	h.drain()
	if task, _ := h.app.Tasks.Get(ctx, next); task.Status != tasks.Completed {
		t.Fatalf("next task = %s", task.Status)
	}
	if h.sent(demo.AirlineRefunds) != 3 {
		t.Fatal("sending did not resume after the window passed")
	}
}

func TestRefusedResponsesStillCountTowardBudget(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.app.Runtime.Model = priced{inner: refusing{}, cost: 700}
	h.drain()
	if got := h.count(`SELECT SUM(cost_micros) FROM model_usage`); got != 700 {
		t.Fatalf("spend after a refusal = %d, want 700", got)
	}
}

func TestPeriodicBudgetPausesThenAutoResumes(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	periodic := demo.Policy
	periodic.Limits.MaxLLMSpendUSD = 3
	periodic.Limits.BudgetPeriod = policy.Duration{Duration: 7 * 24 * time.Hour}
	if _, err := h.app.Assignments.SetPolicy(ctx, h.assignmentID, periodic, "test"); err != nil {
		t.Fatal(err)
	}
	h.app.Runtime.Model = priced{inner: h.model, cost: 1_500_000}
	h.drain()
	if a, _ := h.app.Assignments.Get(ctx, h.assignmentID); a.PausedBy != "budget" {
		t.Fatalf("paused_by = %q, want budget", a.PausedBy)
	}

	h.clock.Advance(6 * 24 * time.Hour)
	if err := h.app.Runtime.ResumeWithinBudget(ctx); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.app.Assignments.Get(ctx, h.assignmentID); a.Status != "paused" {
		t.Fatal("resumed before the spend left the budget window")
	}

	h.clock.Advance(2 * 24 * time.Hour)
	if err := h.app.Runtime.ResumeWithinBudget(ctx); err != nil {
		t.Fatal(err)
	}
	if a, _ := h.app.Assignments.Get(ctx, h.assignmentID); a.Status != "active" {
		t.Fatalf("status after the window rolled = %s, want active", a.Status)
	}

	if err := h.app.Assignments.Pause(ctx, h.assignmentID, "user", "", "test"); err != nil {
		t.Fatal(err)
	}
	h.app.Runtime.ResumeWithinBudget(ctx)
	if a, _ := h.app.Assignments.Get(ctx, h.assignmentID); a.Status != "paused" {
		t.Fatal("budget housekeeping resumed an assignment the user paused")
	}
}

type estimated struct {
	estimate int64
	cost     int64
	entered  chan struct{}
	release  chan struct{}
	fail     bool
	calls    atomic.Int32
}

func (e *estimated) MaxCostMicros(model.Request) int64 { return e.estimate }

func (e *estimated) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	if e.calls.Add(1) == 1 && e.entered != nil {
		e.entered <- struct{}{}
		<-e.release
	}
	if e.fail {
		return model.Response{}, errors.New("connection reset after the request was sent")
	}
	return model.Response{Model: "test-model", Message: model.Message{Content: "done"},
		Usage: model.Usage{CostMicros: e.cost}}, nil
}

func budgeted(t *testing.T, h *harness, usd float64) {
	t.Helper()
	p := demo.Policy
	p.Limits.MaxLLMSpendUSD = usd
	if _, err := h.app.Assignments.SetPolicy(context.Background(), h.assignmentID, p, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWorkersCannotOvercommitBudget(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	budgeted(t, h, 1)
	provider := &estimated{estimate: 600_000, cost: 300_000, entered: make(chan struct{}), release: make(chan struct{})}
	h.app.Runtime.Model = provider
	if _, err := h.app.Tasks.Create(ctx, tasks.NewTask{AssignmentID: h.assignmentID, Title: "second", Objective: "o"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.app.Runtime.RunOnce(ctx, "w1")
	}()
	<-provider.entered

	if _, err := h.app.Runtime.RunOnce(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("model calls = %d while one call held $0.60 of a $1 budget; want the second worker to wait", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM tasks WHERE status = 'ready' AND next_run_at > ?`, h.clock.Now().UnixMilli()); got != 1 {
		t.Fatalf("deferred ready tasks = %d, want the second worker's task deferred", got)
	}

	close(provider.release)
	<-done
	h.clock.Advance(time.Minute)
	h.drain()
	if got := provider.calls.Load(); got < 2 {
		t.Fatalf("model calls = %d, want the waiting worker to run once the first call settled at $0.30", got)
	}
	if got := h.count(`SELECT COALESCE(SUM(cost_micros), 0) FROM model_usage`); got > 1_000_000+provider.estimate {
		t.Fatalf("spend = %d micros, more than one call past the $1 budget", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM model_reservations WHERE status != 'settled'`); got != 0 {
		t.Fatalf("open reservations = %d, want all settled", got)
	}
}

func TestFailedModelCallKeepsItsReservation(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	budgeted(t, h, 1)
	h.app.Runtime.Model = &estimated{estimate: 400_000, fail: true}
	if _, err := h.app.Runtime.RunOnce(ctx, "w"); err == nil {
		t.Fatal("want the model error")
	}
	if got := h.count(`SELECT COUNT(*) FROM model_reservations WHERE status = 'unknown' AND estimate_micros = 400000`); got != 1 {
		t.Fatalf("unknown reservations = %d, want the failed call's estimate kept", got)
	}
	a, _ := h.app.Assignments.Get(ctx, h.assignmentID)
	if b, _ := h.app.Runtime.Budget(ctx, a); b.ReservedMicros != 400_000 {
		t.Fatalf("reserved = %d, want 400000 still counted", b.ReservedMicros)
	}
}
