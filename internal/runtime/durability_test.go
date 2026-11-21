package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentd/internal/demo"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

type slowMailbox struct {
	*tools.Mailbox
	during func()
}

func (s slowMailbox) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	s.during()
	return s.Mailbox.Execute(ctx, call)
}

func withTool(h *harness, name string, tool tools.Tool) {
	registry := tools.Registry{}
	for k, v := range h.app.Gateway.Tools {
		registry[k] = v
	}
	registry[name] = tool
	h.app.Gateway.Tools = registry
}

func TestSlowToolKeepsItsLeaseAndRunsOnce(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	h.app.Runtime.HeartbeatEvery = 5 * time.Millisecond
	mailbox, _ := tools.NewMailbox(ctx, h.db, 0)
	var intruder *tasks.Lease
	slowed := false
	withTool(h, "send_email", slowMailbox{mailbox, func() {
		if slowed {
			return
		}
		slowed = true
		h.clock.Advance(3 * h.app.Runtime.LeaseTTL)
		time.Sleep(60 * time.Millisecond)
		intruder, _ = h.app.Tasks.ClaimReady(ctx, "intruder", h.app.Runtime.LeaseTTL)
	}})
	h.drain()
	if intruder != nil {
		t.Fatal("another worker claimed the task while its tool was still running")
	}
	if h.sent(demo.AirlineRefunds) != 1 {
		t.Fatalf("refunds email sent %d times", h.sent(demo.AirlineRefunds))
	}
}

type stealingTool struct {
	*tools.Mailbox
	steal func()
}

func (s stealingTool) Resource(args json.RawMessage) (string, error) {
	s.steal()
	return s.Mailbox.Resource(args)
}

func TestLostLeaseStopsDispatch(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	mailbox, _ := tools.NewMailbox(ctx, h.db, 0)
	withTool(h, "send_email", stealingTool{mailbox, func() {
		h.db.Exec(`UPDATE tasks SET version = version + 1, lease_owner = 'someone-else' WHERE status = 'running'`)
	}})
	worked, err := h.app.Runtime.RunOnce(ctx, "w")
	if !worked || !errors.Is(err, tasks.ErrLeaseLost) {
		t.Fatalf("RunOnce = %v, %v; want ErrLeaseLost", worked, err)
	}
	if h.sent(demo.AirlineRefunds) != 0 {
		t.Fatal("a worker that lost its lease still dispatched the tool")
	}
	if got := h.actionStatuses("send_email"); len(got) != 1 || got[0] != gateway.Prepared {
		t.Fatalf("action = %v, want it left prepared for the new lease holder", got)
	}
}

func TestLostLeaseStopsModelStepWrite(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	h.app.Runtime.Model = priced{inner: scripted.Provider(func(req model.Request) model.Message {
		h.db.Exec(`UPDATE tasks SET version = version + 1, lease_owner = 'someone-else' WHERE status = 'running'`)
		return demo.Script(req)
	}), cost: 900}
	if _, err := h.app.Runtime.RunOnce(ctx, "w"); !errors.Is(err, tasks.ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if got := h.count(`SELECT COUNT(*) FROM run_messages`); got != 2 {
		t.Fatalf("run messages = %d, want only the opening two", got)
	}
	if got := h.count(`SELECT COALESCE(SUM(cost_micros), 0) FROM model_usage`); got != 900 {
		t.Fatalf("recorded spend = %d, want the call that was made to still count", got)
	}
}

func TestReclaimedTaskResumesItsLatestRunEvenIfMarkedFinished(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.drain()
	h.decideOnly(true)
	h.db.Exec(`UPDATE runs SET status = 'completed', finished_at = 1`)
	h.drain()
	h.expectStatus(tasks.Completed)
	if got := h.count(`SELECT COUNT(*) FROM runs`); got != 1 {
		t.Fatalf("runs = %d, want the existing run resumed", got)
	}
	if h.sent(demo.AirlineRefunds) != 1 || h.sent(demo.AirlineClaims) != 1 {
		t.Fatal("side effects were repeated")
	}
}

func TestApprovalDecidedBeforeTheTaskSleepsStillWakesIt(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	lease, _ := h.app.Tasks.ClaimReady(ctx, "w", time.Minute)
	h.db.Exec(`INSERT INTO runs (id, task_id, model_id, status, started_at) VALUES ('run_x', ?, 'm', 'running', 1)`, h.taskID)
	args, _ := json.Marshal(map[string]string{"to": demo.AirlineClaims, "subject": "s", "body": "b"})
	out, err := h.app.Gateway.Handle(ctx, gateway.Call{RunID: "run_x", TaskID: h.taskID, AssignmentID: h.assignmentID, Seq: 2,
		Call: model.ToolCall{ID: "c", Name: "send_email", Arguments: args}, Lease: *lease})
	if err != nil || out.Kind != gateway.WaitApproval {
		t.Fatalf("handle = %+v, %v", out, err)
	}
	h.decideOnly(true)
	if err := h.app.Tasks.Release(ctx, *lease, tasks.Release{Status: tasks.WaitingApproval}); err != nil {
		t.Fatal(err)
	}
	h.expectStatus(tasks.Ready)
}
