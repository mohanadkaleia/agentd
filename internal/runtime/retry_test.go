package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentd/internal/demo"
	"agentd/internal/gateway"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/overview"
	"agentd/internal/runtime"
	"agentd/internal/tasks"
	"agentd/internal/tools"
)

func TestUnknownOutcomeCanBeResolvedAndTheSameRunContinues(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	working := h.app.Gateway.Tools
	mailbox, _ := tools.NewMailbox(ctx, h.db, 0)
	broken := tools.Registry{}
	for k, v := range working {
		broken[k] = v
	}
	broken["send_email"] = brokenMailbox{mailbox}
	h.app.Gateway.Tools = broken
	h.drain()
	h.expectStatus(tasks.Blocked)

	o, err := overview.Get(ctx, h.app.Runtime, h.assignmentID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.Next.What, "needs you: confirm whether send_email refunds@airline.example happened") {
		t.Fatalf("next = %q", o.Next.What)
	}

	if err := h.app.Runtime.Retry(ctx, h.taskID, "test", ""); !errors.Is(err, runtime.ErrUnresolvedWork) {
		t.Fatalf("retry before resolving = %v, want ErrUnresolvedWork", err)
	}
	unresolved, _ := h.app.Gateway.UnresolvedActions(ctx, "")
	if len(unresolved) != 1 {
		t.Fatalf("unresolved = %d", len(unresolved))
	}
	if err := h.app.Gateway.Resolve(ctx, unresolved[0].ID, true, "test", "I can see it in the airline portal"); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Gateway.Resolve(ctx, unresolved[0].ID, true, "test", ""); !errors.Is(err, gateway.ErrNotUnresolved) {
		t.Fatalf("second resolve = %v", err)
	}

	h.app.Gateway.Tools = working
	if err := h.app.Runtime.Retry(ctx, h.taskID, "test", ""); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.expectStatus(tasks.WaitingApproval)
	if got := h.count(`SELECT COUNT(*) FROM runs`); got != 1 {
		t.Fatalf("runs = %d, want 1 (retry continues the same run)", got)
	}
	if h.sent(demo.AirlineRefunds) != 0 {
		t.Fatal("the action resolved as 'happened' was executed again")
	}
	if got := h.actionStatuses("send_email"); got[0] != gateway.Succeeded {
		t.Fatalf("resolved action = %v", got)
	}
}

func TestRetryAfterFalseCompletionClaimsDeliversTheNote(t *testing.T) {
	h := newHarness(t, demo.Script)
	ctx := context.Background()
	h.app.Runtime.Model = scripted.Provider(func(req model.Request) model.Message {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleUser && strings.Contains(last.Content, "Their note: use the refunds desk") {
			body, _ := json.Marshal(map[string]string{"to": demo.AirlineRefunds, "subject": "s", "body": "b"})
			return model.Message{ToolCalls: []model.ToolCall{{ID: "x", Name: "send_email", Arguments: body}}}
		}
		return model.Message{Content: "all done"}
	})
	h.drain()
	h.expectStatus(tasks.Blocked)

	if err := h.app.Runtime.Retry(ctx, h.taskID, "test", "use the refunds desk"); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.expectStatus(tasks.Completed)
	if h.sent(demo.AirlineRefunds) != 1 {
		t.Fatal("the retried task did not act on the note")
	}
	if err := h.app.Runtime.Retry(ctx, h.taskID, "test", ""); !errors.Is(err, runtime.ErrNotBlocked) {
		t.Fatalf("retrying a completed task = %v, want ErrNotBlocked", err)
	}
}
