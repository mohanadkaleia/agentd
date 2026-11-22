package gateway_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/system"
	"agentd/internal/testutil"
	"agentd/internal/testutil/airline"
	"agentd/internal/tools"
)

type fixture struct {
	t            *testing.T
	app          *app.App
	clock        *testutil.Clock
	without      *gateway.Gateway
	taskID       string
	assignmentID string
}

func newFixture(t *testing.T) *fixture {
	clock := testutil.NewClock()
	db := testutil.Open(t, testutil.DBPath(t))
	mailbox, err := tools.NewMailbox(context.Background(), db, 0)
	if err != nil {
		t.Fatal(err)
	}
	idle := scripted.Provider(func(model.Request) model.Message { return model.Message{Content: "done"} })
	a := app.New(db, idle, "scripted", tools.NewRegistry(mailbox), clock.Now)
	assignmentID, taskID, err := airline.Seed(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (id, task_id, model_id, status, started_at) VALUES ('run_1', ?, 'scripted', 'running', 0)`, taskID); err != nil {
		t.Fatal(err)
	}
	without := *a.Gateway
	without.Tools = tools.Registry{}
	for name, tool := range a.Gateway.Tools {
		if name != "send_email" {
			without.Tools[name] = tool
		}
	}
	return &fixture{t: t, app: a, clock: clock, without: &without, taskID: taskID, assignmentID: assignmentID}
}

func (f *fixture) call(to string) gateway.Call {
	args, _ := json.Marshal(map[string]string{"to": to, "subject": "Refund", "body": "Please refund."})
	return gateway.Call{RunID: "run_1", TaskID: f.taskID, AssignmentID: f.assignmentID, Seq: 1,
		Call: model.ToolCall{ID: "call_" + to, Name: "send_email", Arguments: args}}
}

func (f *fixture) handle(g *gateway.Gateway, c gateway.Call) gateway.Outcome {
	f.t.Helper()
	out, err := g.Handle(context.Background(), c)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) status(c gateway.Call) string {
	f.t.Helper()
	var status string
	if err := f.app.DB.QueryRow(`SELECT status FROM actions WHERE idempotency_key = ?`,
		"run_1:1:"+c.Call.ID).Scan(&status); err != nil {
		f.t.Fatal(err)
	}
	return status
}

func (f *fixture) sent(to string) int {
	f.t.Helper()
	n, err := tools.SentCount(context.Background(), f.app.DB, to)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestPreparedActionWaitsWhileToolIsMissing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.call(airline.AirlineRefunds)
	if err := system.Halt(ctx, f.app.DB, f.clock.Now(), "test", "hold"); err != nil {
		t.Fatal(err)
	}
	if out := f.handle(f.app.Gateway, c); out.Kind != gateway.Paused || f.status(c) != gateway.Prepared {
		t.Fatalf("halted handle = %v / %s, want paused with a prepared action", out.Kind, f.status(c))
	}
	if err := system.Resume(ctx, f.app.DB, f.clock.Now(), "test"); err != nil {
		t.Fatal(err)
	}

	out := f.handle(f.without, c)
	if out.Kind != gateway.Unavailable || out.Since.IsZero() {
		t.Fatalf("missing tool outcome = %+v, want unavailable with a start time", out)
	}
	if f.status(c) != gateway.Prepared || f.sent(airline.AirlineRefunds) != 0 {
		t.Fatalf("action = %s, sent = %d; want prepared and nothing sent", f.status(c), f.sent(airline.AirlineRefunds))
	}
	f.clock.Advance(time.Minute)
	if again := f.handle(f.without, c); !again.Since.Equal(out.Since) {
		t.Fatalf("wait restarted: since %v then %v", out.Since, again.Since)
	}

	if out := f.handle(f.app.Gateway, c); out.Kind != gateway.Continue || f.status(c) != gateway.Succeeded {
		t.Fatalf("tool back: outcome %v, action %s; want dispatched", out.Kind, f.status(c))
	}
	if f.sent(airline.AirlineRefunds) != 1 {
		t.Fatalf("sent %d, want exactly 1", f.sent(airline.AirlineRefunds))
	}
}

func TestApprovedActionWaitsWhileToolIsMissing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.call(airline.AirlineClaims)
	if out := f.handle(f.app.Gateway, c); out.Kind != gateway.WaitApproval {
		t.Fatalf("outcome = %v, want wait for approval", out.Kind)
	}
	pending, err := f.app.Gateway.PendingApprovals(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending approvals = %d, %v", len(pending), err)
	}
	if err := f.app.Gateway.Decide(ctx, pending[0].ID, true, "test"); err != nil {
		t.Fatal(err)
	}

	if out := f.handle(f.without, c); out.Kind != gateway.Unavailable {
		t.Fatalf("outcome = %v, want unavailable", out.Kind)
	}
	if f.status(c) != gateway.PendingApproval || f.sent(airline.AirlineClaims) != 0 {
		t.Fatalf("action = %s, sent = %d; want still approved and unsent", f.status(c), f.sent(airline.AirlineClaims))
	}

	if out := f.handle(f.app.Gateway, c); out.Kind != gateway.Continue || f.sent(airline.AirlineClaims) != 1 {
		t.Fatalf("tool back: outcome %v, sent %d; want one dispatch under the existing approval", out.Kind, f.sent(airline.AirlineClaims))
	}
}

func TestInterruptedDispatchWithMissingToolNeedsReconciliation(t *testing.T) {
	f := newFixture(t)
	c := f.call(airline.AirlineRefunds)
	f.handle(f.app.Gateway, c)
	if _, err := f.app.DB.Exec(`UPDATE actions SET status = 'dispatching' WHERE idempotency_key = ?`, "run_1:1:"+c.Call.ID); err != nil {
		t.Fatal(err)
	}

	if out := f.handle(f.without, c); out.Kind != gateway.Blocked {
		t.Fatalf("outcome = %v, want blocked", out.Kind)
	}
	if f.status(c) != gateway.NeedsReconciliation {
		t.Fatalf("action = %s, want needs_reconciliation", f.status(c))
	}
}
