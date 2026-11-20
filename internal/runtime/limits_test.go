package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	p, _ := h.app.Projects.Get(ctx, h.projID)
	if p.Status != "paused" || !strings.Contains(p.StatusReason, "LLM budget exhausted") {
		t.Fatalf("project = %s (%q), want paused for budget", p.Status, p.StatusReason)
	}
	h.expectStatus(tasks.Ready)
	if got := h.count(`SELECT SUM(cost_micros) FROM model_usage WHERE project_id = ?`, h.projID); got != 3_000_000 {
		t.Fatalf("recorded spend = %d micros, want 3000000", got)
	}

	raised := demo.Policy
	raised.Limits.MaxLLMSpendUSD = 10
	if _, err := h.app.Projects.SetPolicy(ctx, h.projID, raised, "test"); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Projects.SetStatus(ctx, h.projID, "active", "", "test"); err != nil {
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
	if got := h.count(`SELECT policy_version FROM projects WHERE id = ?`, h.projID); got != 2 {
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
	if _, err := h.app.Projects.SetPolicy(ctx, h.projID, limited, "test"); err != nil {
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
	next, err := h.app.Tasks.Create(ctx, tasks.NewTask{ProjectID: h.projID, Title: "again", Objective: "send again"})
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
