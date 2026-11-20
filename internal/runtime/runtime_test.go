package runtime_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/demo"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

type harness struct {
	t      *testing.T
	path   string
	clock  *testutil.Clock
	db     *sql.DB
	app    *app.App
	model  model.Provider
	calls  *atomic.Int32
	taskID string
	projID string
}

func newHarness(t *testing.T, script scripted.Provider) *harness {
	h := &harness{t: t, path: testutil.DBPath(t), clock: testutil.NewClock(), calls: &atomic.Int32{}}
	h.model = scripted.Provider(func(req model.Request) model.Message {
		h.calls.Add(1)
		return script(req)
	})
	h.boot()
	var err error
	h.projID, h.taskID, err = demo.Seed(context.Background(), h.app)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) boot() {
	h.t.Helper()
	db := testutil.Open(h.t, h.path)
	mailbox, err := tools.NewMailbox(context.Background(), db, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	h.db = db
	h.app = app.New(db, h.model, "scripted", tools.NewRegistry(tools.WebSearch{}, mailbox), h.clock.Now)
}

func (h *harness) restart() {
	h.t.Helper()
	if err := h.db.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.boot()
}

func (h *harness) drain() {
	h.t.Helper()
	for range 50 {
		worked, err := h.app.Runtime.RunOnce(context.Background(), "worker")
		if err != nil {
			h.t.Logf("attempt error: %v", err)
		}
		if !worked {
			return
		}
	}
	h.t.Fatal("runtime did not settle")
}

func (h *harness) task() tasks.Task {
	h.t.Helper()
	task, err := h.app.Tasks.Get(context.Background(), h.taskID)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *harness) expectStatus(want string) {
	h.t.Helper()
	if got := h.task().Status; got != want {
		h.t.Fatalf("task status = %s, want %s", got, want)
	}
}

func (h *harness) sent(recipient string) int {
	h.t.Helper()
	n, err := tools.SentCount(context.Background(), h.db, recipient)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) decideOnly(approve bool) {
	h.t.Helper()
	pending, err := h.app.Gateway.PendingApprovals(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	if len(pending) != 1 {
		h.t.Fatalf("pending approvals = %d, want 1", len(pending))
	}
	if err := h.app.Gateway.Decide(context.Background(), pending[0].ID, approve, "test"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) actionStatuses(tool string) []string {
	h.t.Helper()
	rows, err := h.db.Query(`SELECT status FROM actions WHERE tool_name = ? ORDER BY created_at`, tool)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestDemoCompletesWithApproval(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.drain()
	h.expectStatus(tasks.WaitingApproval)
	if h.sent(demo.AirlineRefunds) != 1 || h.sent(demo.AirlineClaims) != 0 {
		t.Fatal("pre-approved email should be sent, unapproved one held")
	}

	h.decideOnly(true)
	h.drain()
	h.expectStatus(tasks.Completed)
	if h.sent(demo.AirlineClaims) != 1 {
		t.Fatal("approved email was not sent")
	}
}

func TestCrashAfterSendDoesNotDuplicate(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.app.Gateway.AfterExecute = func(a gateway.Action) {
		if a.ToolName == "send_email" {
			goruntime.Goexit()
		}
	}

	crashed := make(chan struct{})
	go func() {
		defer close(crashed)
		h.app.Runtime.RunOnce(context.Background(), "doomed")
	}()
	<-crashed

	if got := h.actionStatuses("send_email"); len(got) != 1 || got[0] != gateway.Dispatching {
		t.Fatalf("after crash actions = %v, want one dispatching", got)
	}
	if h.sent(demo.AirlineRefunds) != 1 {
		t.Fatal("simulated provider should have received the email before the crash")
	}

	h.restart()
	h.clock.Advance(h.app.Runtime.LeaseTTL + time.Second)
	callsBefore := h.calls.Load()
	h.drain()
	h.expectStatus(tasks.WaitingApproval)

	if got := h.task().AttemptCount; got != 1 {
		t.Fatalf("attempts = %d, want 1 for the crashed lease", got)
	}
	if h.sent(demo.AirlineRefunds) != 1 {
		t.Fatalf("refunds email sent %d times, want exactly 1", h.sent(demo.AirlineRefunds))
	}
	if got := h.actionStatuses("send_email"); got[0] != gateway.Succeeded {
		t.Fatalf("reconciled action status = %s, want succeeded", got[0])
	}
	if h.calls.Load()-callsBefore != 1 {
		t.Fatalf("model calls after restart = %d, want 1 (no replay of checkpointed steps)", h.calls.Load()-callsBefore)
	}

	h.decideOnly(true)
	h.drain()
	h.expectStatus(tasks.Completed)
}

func TestRestartWhileWaitingForApproval(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.drain()
	h.expectStatus(tasks.WaitingApproval)

	h.restart()
	h.drain()
	h.expectStatus(tasks.WaitingApproval)

	h.decideOnly(true)
	h.restart()
	h.drain()
	h.expectStatus(tasks.Completed)
	if h.sent(demo.AirlineRefunds) != 1 || h.sent(demo.AirlineClaims) != 1 {
		t.Fatal("each email must be sent exactly once across restarts")
	}
}

func TestExpiredApprovalNeverExecutes(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.drain()
	h.clock.Advance(h.app.Gateway.ApprovalTTL + time.Second)

	pending, _ := h.app.Gateway.PendingApprovals(context.Background())
	if err := h.app.Gateway.Decide(context.Background(), pending[0].ID, true, "late"); !errors.Is(err, gateway.ErrApprovalExpired) {
		t.Fatalf("late approval err = %v, want ErrApprovalExpired", err)
	}
	h.drain()
	h.expectStatus(tasks.Completed)
	if h.sent(demo.AirlineClaims) != 0 {
		t.Fatal("expired approval still sent the email")
	}
	if got := h.actionStatuses("send_email"); got[1] != gateway.Denied {
		t.Fatalf("expired action status = %s, want denied", got[1])
	}
}

func TestExpirySweepWakesWaitingTask(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.drain()
	h.clock.Advance(h.app.Gateway.ApprovalTTL + time.Second)
	if n, err := h.app.Gateway.ExpireApprovals(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired %d approvals, err %v; want 1", n, err)
	}
	h.expectStatus(tasks.Ready)
	h.drain()
	if h.sent(demo.AirlineClaims) != 0 {
		t.Fatal("expired approval still sent the email")
	}
}

func TestRejectedApprovalIsDenied(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.drain()
	h.decideOnly(false)
	h.drain()
	h.expectStatus(tasks.Completed)
	if h.sent(demo.AirlineClaims) != 0 {
		t.Fatal("rejected email was sent")
	}
}

func TestPausedProjectTakesNoNewActions(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.drain()
	if err := h.app.Projects.SetStatus(context.Background(), h.projID, "paused"); err != nil {
		t.Fatal(err)
	}
	h.decideOnly(true)
	h.drain()
	h.expectStatus(tasks.Ready)
	if h.sent(demo.AirlineClaims) != 0 {
		t.Fatal("paused project sent an email")
	}
	h.app.Projects.SetStatus(context.Background(), h.projID, "active")
	h.drain()
	h.expectStatus(tasks.Completed)
}

func injectedScript(req model.Request) model.Message {
	tools := 0
	for _, m := range req.Messages {
		if m.Role == model.RoleTool {
			tools++
		}
	}
	switch tools {
	case 0:
		return model.Message{ToolCalls: []model.ToolCall{
			{ID: "a", Name: "transfer_money", Arguments: json.RawMessage(`{"amount":5000}`)},
			{ID: "b", Name: "send_email", Arguments: json.RawMessage(`{"to":"a@x.com, b@x.com","subject":"s","body":"b"}`)},
			{ID: "c", Name: "send_email", Arguments: json.RawMessage(`not json`)},
		}}
	case 3:
		return model.Message{ToolCalls: []model.ToolCall{{ID: "d", Name: "send_email",
			Arguments: json.RawMessage(`{"to":"refunds@airline.example","subject":"s","body":"b"}`)}}}
	default:
		return model.Message{Content: "done"}
	}
}

func TestUntrustedToolCallsAreDenied(t *testing.T) {
	h := newHarness(t, injectedScript)
	h.drain()
	h.expectStatus(tasks.Completed)
	if got := h.actionStatuses("transfer_money"); len(got) != 1 || got[0] != gateway.Denied {
		t.Fatalf("unknown tool actions = %v, want [denied]", got)
	}
	if got := h.actionStatuses("send_email"); len(got) != 3 || got[0] != gateway.Failed || got[1] != gateway.Failed || got[2] != gateway.Succeeded {
		t.Fatalf("send_email actions = %v, want [failed failed succeeded]", got)
	}
}

func TestCompletionWithoutEvidenceIsRejected(t *testing.T) {
	h := newHarness(t, func(model.Request) model.Message { return model.Message{Content: "All done!"} })
	h.app.Runtime.MaxSteps = 3
	h.drain()
	h.expectStatus(tasks.Blocked)
	if got := h.calls.Load(); got != 3 {
		t.Fatalf("model calls = %d, want 3 (rejected each time until step limit)", got)
	}
}

type flaky struct{}

func (flaky) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{}, errors.New("provider unavailable")
}

func TestModelFailuresRetryWithBackoffThenBlock(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.app.Runtime.Model = flaky{}
	h.drain()
	h.expectStatus(tasks.Retryable)
	if h.task().AttemptCount != 1 {
		t.Fatalf("attempts = %d, want 1", h.task().AttemptCount)
	}
	for range 2 {
		h.clock.Advance(time.Hour)
		h.drain()
	}
	h.expectStatus(tasks.Blocked)
}

type brokenMailbox struct{ *tools.Mailbox }

func (brokenMailbox) Execute(context.Context, tools.Invocation) (tools.Result, error) {
	return tools.Result{}, errors.New("connection reset")
}

func (brokenMailbox) Reconcile(context.Context, tools.Invocation) (tools.Result, bool, error) {
	return tools.Result{}, false, errors.New("provider API down")
}

func TestUnknownOutcomeBlocksInsteadOfRetrying(t *testing.T) {
	h := newHarness(t, demo.Script)
	mailbox, _ := tools.NewMailbox(context.Background(), h.db, 0)
	h.app.Gateway.Tools = tools.NewRegistry(tools.WebSearch{}, brokenMailbox{mailbox})
	h.drain()
	h.expectStatus(tasks.Blocked)
	if got := h.actionStatuses("send_email"); len(got) != 1 || got[0] != gateway.NeedsReconciliation {
		t.Fatalf("actions = %v, want [needs_reconciliation]", got)
	}
}

type refusing struct{}

func (refusing) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{Message: model.Message{Role: model.RoleAssistant}, StopReason: model.StopRefusal}, nil
}

func TestRefusalIsRetriedNotTreatedAsCompletion(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.app.Runtime.Model = refusing{}
	h.drain()
	h.expectStatus(tasks.Retryable)
	var msgs int
	h.db.QueryRow(`SELECT COUNT(*) FROM run_messages`).Scan(&msgs)
	if msgs != 2 {
		t.Fatalf("checkpointed messages = %d, want only system+user (refusal not persisted)", msgs)
	}
}
