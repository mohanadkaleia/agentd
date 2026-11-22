package runtime_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"agentd/internal/demo"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/overview"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

func withoutMail(*tools.Mailbox) tools.Registry { return tools.NewRegistry(tools.WebSearch{}) }

func (h *harness) nextRunAt() time.Time {
	h.t.Helper()
	var next int64
	if err := h.db.QueryRow(`SELECT COALESCE(next_run_at, 0) FROM tasks WHERE id = ?`, h.taskID).Scan(&next); err != nil {
		h.t.Fatal(err)
	}
	return time.UnixMilli(next)
}

func (h *harness) approveWithoutMail() {
	h.t.Helper()
	h.drain()
	h.expectStatus(tasks.WaitingApproval)
	h.tools = withoutMail
	h.restart()
	h.decideOnly(true)
}

func TestApprovedCallWaitsForMissingToolWithoutModelCalls(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.approveWithoutMail()
	calls, attempts := h.calls.Load(), h.task().AttemptCount

	h.drain()
	h.expectStatus(tasks.Ready)
	if !h.nextRunAt().After(h.clock.Now()) {
		t.Fatal("waiting task has no backoff")
	}
	if !strings.Contains(string(h.task().Result), `tool \"send_email\" is unavailable`) {
		t.Fatalf("task result = %s, want the waiting reason", h.task().Result)
	}
	o, err := overview.Get(context.Background(), h.app.Runtime, h.assignmentID)
	if err != nil || !strings.Contains(o.Next.What, `waiting (tool "send_email" is unavailable)`) {
		t.Fatalf("next expected = %q, %v; want the waiting reason", o.Next.What, err)
	}
	h.clock.Advance(time.Minute)
	h.drain()
	h.expectStatus(tasks.Ready)
	if h.calls.Load() != calls || h.task().AttemptCount != attempts {
		t.Fatalf("waiting made %d model calls and %d attempts, want none", h.calls.Load()-calls, h.task().AttemptCount-attempts)
	}
	if got := h.actionStatuses("send_email"); got[1] != gateway.PendingApproval {
		t.Fatalf("waiting action = %s, want it kept with its approval", got[1])
	}
	if n := h.count(`SELECT COUNT(*) FROM audit_log WHERE operation = 'action.tool_unavailable'`); n != 1 {
		t.Fatalf("tool_unavailable audit entries = %d, want 1 per wait", n)
	}

	h.tools = nil
	h.restart()
	h.clock.Advance(time.Hour)
	h.drain()
	h.expectStatus(tasks.Completed)
	if h.sent(demo.AirlineClaims) != 1 {
		t.Fatalf("claims email sent %d times, want 1", h.sent(demo.AirlineClaims))
	}
}

func TestMissingToolBlocksTaskAfterBoundedWait(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.approveWithoutMail()
	h.drain()
	h.clock.Advance(h.app.Runtime.ToolWait + time.Minute)
	h.drain()
	h.expectStatus(tasks.Blocked)
	if !strings.Contains(string(h.task().Result), "send_email") {
		t.Fatalf("blocked result = %s, want it to name the tool", h.task().Result)
	}
	notifications := `SELECT COUNT(*) FROM notifications WHERE dedup_key = ?`
	if n := h.count(notifications, "tool-unavailable:"+h.taskID); n != 1 {
		t.Fatalf("notifications = %d, want 1", n)
	}

	if err := h.app.Runtime.Retry(context.Background(), h.taskID, "test", ""); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.expectStatus(tasks.Ready)
	if n := h.count(notifications, "tool-unavailable:"+h.taskID); n != 1 {
		t.Fatalf("notifications after retry = %d, want still 1", n)
	}
}

func TestCrashedSendWithMissingToolBlocksUntilProviderConfirms(t *testing.T) {
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

	h.tools = withoutMail
	h.restart()
	h.clock.Advance(h.app.Runtime.LeaseTTL + time.Second)
	h.drain()
	h.expectStatus(tasks.Blocked)
	if got := h.actionStatuses("send_email"); got[0] != gateway.NeedsReconciliation {
		t.Fatalf("action = %s, want needs_reconciliation", got[0])
	}

	h.tools = nil
	h.restart()
	h.clock.Advance(2 * h.app.Gateway.RecheckEvery)
	confirmed, err := h.app.Gateway.Recheck(context.Background())
	if err != nil || len(confirmed) != 1 {
		t.Fatalf("recheck confirmed %v, err %v; want the task", confirmed, err)
	}
	if h.sent(demo.AirlineRefunds) != 1 {
		t.Fatalf("refunds sent %d times, want 1", h.sent(demo.AirlineRefunds))
	}
}

type panickingMailbox struct{ *tools.Mailbox }

func (panickingMailbox) Execute(context.Context, tools.Invocation) (tools.Result, error) {
	panic("tool bug")
}

func TestPanickingToolFailsOnlyItsAttempt(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.tools = func(mb *tools.Mailbox) tools.Registry { return tools.NewRegistry(panickingMailbox{mb}) }
	h.restart()

	worked, err := h.app.Runtime.RunOnce(context.Background(), "worker")
	if !worked || err == nil || !strings.Contains(err.Error(), "tool bug") {
		t.Fatalf("RunOnce = %v, %v; want the panic reported as an error", worked, err)
	}
	h.expectStatus(tasks.Retryable)
	if h.task().AttemptCount != 1 {
		t.Fatalf("attempts = %d, want the panic counted once", h.task().AttemptCount)
	}

	h.tools = nil
	h.restart()
	h.clock.Advance(time.Hour)
	h.drain()
	h.expectStatus(tasks.WaitingApproval)
	if h.sent(demo.AirlineRefunds) != 1 {
		t.Fatalf("refunds sent %d times, want 1", h.sent(demo.AirlineRefunds))
	}
}

func TestServeSurvivesPanickingHousekeeping(t *testing.T) {
	h := newHarness(t, demo.Script)
	h.app.Runtime.Housekeeping = func(context.Context) error { panic("housekeeping bug") }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	h.app.Runtime.Serve(ctx, "serve", 1, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.expectStatus(tasks.WaitingApproval)
}

type unruledTool struct{ tools.WebSearch }

func (unruledTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: "unruled", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func TestModelIsOfferedOnlyToolsThePolicyCanAllow(t *testing.T) {
	var offered []string
	h := newHarness(t, func(req model.Request) model.Message {
		if offered == nil {
			for _, d := range req.Tools {
				offered = append(offered, d.Name)
			}
		}
		return demo.Script(req)
	})
	h.tools = func(mb *tools.Mailbox) tools.Registry { return tools.NewRegistry(tools.WebSearch{}, mb, unruledTool{}) }
	h.restart()
	h.drain()
	if !slices.Contains(offered, "send_email") || !slices.Contains(offered, "web_search") || slices.Contains(offered, "unruled") {
		t.Fatalf("offered tools = %v, want the policy's tools and not unruled", offered)
	}
}

type unreviewedSearch struct{ tools.WebSearch }

func (unreviewedSearch) Pin() (string, string) { return "", "not reviewed yet" }

func TestUnreviewedPinnedToolsAreNotOffered(t *testing.T) {
	var offered []string
	h := newHarness(t, func(req model.Request) model.Message {
		if offered == nil {
			for _, d := range req.Tools {
				offered = append(offered, d.Name)
			}
		}
		return demo.Script(req)
	})
	h.tools = func(mb *tools.Mailbox) tools.Registry { return tools.NewRegistry(unreviewedSearch{}, mb) }
	h.restart()
	h.drain()
	if slices.Contains(offered, "web_search") || !slices.Contains(offered, "send_email") {
		t.Fatalf("offered = %v, want send_email but not the unreviewed web_search", offered)
	}
}
