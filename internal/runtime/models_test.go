package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"agentd/internal/model"
	"agentd/internal/policy"
	"agentd/internal/runtime"
	"agentd/internal/tasks"
	"agentd/internal/testutil/airline"
)

type outage struct {
	failAfter int32
	calls     atomic.Int32
	inner     model.Provider
}

func (f *outage) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	if f.calls.Add(1) > f.failAfter {
		return model.Response{}, errors.New("503 overloaded")
	}
	return f.inner.Complete(ctx, req)
}

func withModels(t *testing.T, h *harness, cloud model.Provider, local model.Provider, change func(*policy.Policy)) {
	t.Helper()
	h.app.Runtime.Model, h.app.Runtime.ModelID, h.app.Runtime.ModelLocal = cloud, "anthropic", false
	h.app.Runtime.Fallbacks = []runtime.Candidate{{ID: "ollama/qwen", Provider: local, Local: true}}
	if change != nil {
		p := airline.Policy
		change(&p)
		if _, err := h.app.Assignments.SetPolicy(context.Background(), h.assignmentID, p, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *harness) runModel() string {
	var id string
	h.db.QueryRow(`SELECT model_id FROM runs WHERE task_id = ?`, h.taskID).Scan(&id)
	return id
}

func TestOutageBeforeTheFirstReplyFallsBack(t *testing.T) {
	h := newHarness(t, airline.Script)
	withModels(t, h, &outage{failAfter: 0}, h.model, nil)
	h.drain()
	if got := h.runModel(); got != "ollama/qwen" {
		t.Fatalf("run model = %q, want switched to the fallback", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM audit_log WHERE operation = 'run.model_switched'`); got != 1 {
		t.Fatalf("model switches = %d, want 1", got)
	}
	if h.task().Status == tasks.Blocked || h.task().Status == tasks.Retryable {
		t.Fatalf("task = %s, want it to make progress on the fallback", h.task().Status)
	}
}

func TestNoSwitchAfterTheRunHasStarted(t *testing.T) {
	h := newHarness(t, airline.Script)
	withModels(t, h, &outage{failAfter: 1, inner: h.model}, h.model, nil)
	h.app.Runtime.RunOnce(context.Background(), "w")
	if got := h.runModel(); got != "anthropic" {
		t.Fatalf("run model = %q, want it pinned once a reply exists", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM audit_log WHERE operation = 'run.model_switched'`); got != 0 {
		t.Fatal("switched provider mid-conversation")
	}
}

func TestLocalOnlyAssignmentNeverReachesTheCloud(t *testing.T) {
	h := newHarness(t, airline.Script)
	cloud := &outage{failAfter: 1000, inner: h.model}
	local := &outage{failAfter: 0}
	withModels(t, h, cloud, local, func(p *policy.Policy) { p.Models.LocalOnly = true })
	h.app.Runtime.RunOnce(context.Background(), "w")
	if cloud.calls.Load() != 0 {
		t.Fatal("a local-only assignment called the cloud model")
	}
	if got := h.runModel(); got != "ollama/qwen" {
		t.Fatalf("run model = %q", got)
	}
}

func TestRunWithProviderDataStaysPinned(t *testing.T) {
	h := newHarness(t, airline.Script)
	h.app.Runtime.Model = &native{inner: h.model}
	h.drain()
	h.expectStatus(tasks.WaitingApproval)
	h.app.Runtime.ModelID = "something-else"
	h.decideOnly(true)
	h.drain()
	h.expectStatus(tasks.Blocked)
	if got := h.runModel(); got != "scripted" {
		t.Fatalf("run model = %q, want it left pinned", got)
	}
}

func TestNoAllowedModelBlocks(t *testing.T) {
	h := newHarness(t, airline.Script)
	withModels(t, h, h.model, h.model, func(p *policy.Policy) { p.Models.Allowed = []string{"anthropic/claude-haiku-5-5"} })
	h.drain()
	h.expectStatus(tasks.Blocked)
}

type native struct{ inner model.Provider }

func (n *native) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	resp, err := n.inner.Complete(ctx, req)
	resp.Message.Native = &model.Native{Provider: "test", Payload: []byte(`{}`)}
	return resp, err
}
