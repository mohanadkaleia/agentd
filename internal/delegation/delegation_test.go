package delegation_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
	t       *testing.T
	path    string
	clock   *testutil.Clock
	app     *app.App
	script  scripted.Provider
	project string

	mu        sync.Mutex
	toolsSeen map[string][]string
}

func newHarness(t *testing.T, script scripted.Provider) *harness {
	h := &harness{t: t, path: testutil.DBPath(t), clock: testutil.NewClock(), script: script, toolsSeen: map[string][]string{}}
	h.boot()
	var err error
	h.project, err = h.app.Projects.Create(context.Background(), "research", "Compare options.", demo.Policy)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) boot() {
	db := testutil.Open(h.t, h.path)
	mailbox, err := tools.NewMailbox(context.Background(), db, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	recording := scripted.Provider(func(req model.Request) model.Message {
		var names []string
		for _, d := range req.Tools {
			names = append(names, d.Name)
		}
		h.mu.Lock()
		h.toolsSeen[objectiveOf(req)] = names
		h.mu.Unlock()
		return h.script(req)
	})
	h.app = app.New(db, recording, "scripted", tools.NewRegistry(tools.WebSearch{}, mailbox), h.clock.Now)
}

func (h *harness) restart() {
	h.app.DB.Close()
	h.boot()
}

func (h *harness) task(nt tasks.NewTask) string {
	h.t.Helper()
	nt.ProjectID = h.project
	if nt.Title == "" {
		nt.Title = "task"
	}
	id, err := h.app.Tasks.Create(context.Background(), nt)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) step() bool {
	h.t.Helper()
	worked, err := h.app.Runtime.RunOnce(context.Background(), "w")
	if err != nil {
		h.t.Logf("attempt: %v", err)
	}
	return worked
}

func (h *harness) drain() {
	h.t.Helper()
	for range 100 {
		if !h.step() {
			return
		}
	}
	h.t.Fatal("runtime did not settle")
}

func (h *harness) get(id string) tasks.Task {
	h.t.Helper()
	task, err := h.app.Tasks.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *harness) actions(tool string) []string {
	h.t.Helper()
	rows, err := h.app.DB.Query(`SELECT status FROM actions WHERE tool_name = ? ORDER BY created_at, id`, tool)
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

func (h *harness) actionResult(tool string) string {
	var result sql.NullString
	h.app.DB.QueryRow(`SELECT result_json FROM actions WHERE tool_name = ? ORDER BY created_at DESC LIMIT 1`, tool).Scan(&result)
	return result.String
}

func objectiveOf(req model.Request) string {
	for _, m := range req.Messages {
		if m.Role == model.RoleUser {
			return m.Content
		}
	}
	return ""
}

func toolTurns(req model.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == model.RoleTool {
			n++
		}
	}
	return n
}

func call(id, name string, args any) model.ToolCall {
	body, _ := json.Marshal(args)
	return model.ToolCall{ID: id, Name: name, Arguments: body}
}

func isWorker(req model.Request) bool {
	return strings.HasPrefix(objectiveOf(req), "You are a helper agent")
}

func researchScript(req model.Request) model.Message {
	if isWorker(req) {
		if toolTurns(req) == 0 {
			return model.Message{ToolCalls: []model.ToolCall{call("s", "web_search", map[string]string{"query": objectiveOf(req)})}}
		}
		return model.Message{Content: "Findings for: " + objectiveOf(req)[strings.LastIndex(objectiveOf(req), "\n")+1:]}
	}
	switch toolTurns(req) {
	case 0:
		return model.Message{ToolCalls: []model.ToolCall{
			call("d1", "delegate", map[string]any{"title": "North", "objective": "Research north district", "allowed_tools": []string{"web_search"}}),
			call("d2", "delegate", map[string]any{"title": "South", "objective": "Research south district", "allowed_tools": []string{"web_search"}}),
		}}
	case 2:
		return model.Message{ToolCalls: []model.ToolCall{call("a", "await_workers", map[string]any{})}}
	default:
		return model.Message{Content: "Compared both districts."}
	}
}

func TestParentWaitsForWorkersAndResumesAfterRestart(t *testing.T) {
	h := newHarness(t, researchScript)
	parent := h.task(tasks.NewTask{Title: "Plan", Objective: "Plan the apartment search"})

	h.step()
	if got := h.get(parent).Status; got != tasks.WaitingEvent {
		t.Fatalf("parent after delegating = %s, want waiting_event", got)
	}
	children, _ := h.app.Tasks.Children(context.Background(), parent)
	if len(children) != 2 || children[0].Depth != 1 || !children[0].Allows("web_search") || children[0].Allows("send_email") {
		t.Fatalf("children = %+v", children)
	}

	h.restart()
	h.drain()

	if got := h.get(parent).Status; got != tasks.Completed {
		t.Fatalf("parent = %s, want completed", got)
	}
	result := h.actionResult("await_workers")
	if !strings.Contains(result, "Findings for: Research north district") || !strings.Contains(result, "Findings for: Research south district") {
		t.Fatalf("parent did not receive worker results: %s", result)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for objective, names := range h.toolsSeen {
		if strings.HasPrefix(objective, "You are a helper agent") && (len(names) != 1 || names[0] != "web_search") {
			t.Fatalf("worker was offered tools %v, want only web_search", names)
		}
	}
}

func TestWorkerCannotEscalatePermissions(t *testing.T) {
	h := newHarness(t, func(req model.Request) model.Message {
		if toolTurns(req) == 0 {
			return model.Message{ToolCalls: []model.ToolCall{
				call("x1", "send_email", map[string]string{"to": demo.AirlineRefunds, "subject": "s", "body": "b"}),
				call("x2", "delegate", map[string]any{"title": "t", "objective": "o", "allowed_tools": []string{"send_email"}}),
			}}
		}
		return model.Message{Content: "done"}
	})
	worker := h.task(tasks.NewTask{Objective: "worker", Depth: 1, AllowedTools: []string{"web_search", "delegate"}})
	h.drain()

	if got := h.get(worker).Status; got != tasks.Completed {
		t.Fatalf("worker = %s", got)
	}
	if got := h.actions("send_email"); len(got) != 1 || got[0] != gateway.Denied {
		t.Fatalf("send_email actions = %v, want [denied]", got)
	}
	if got := h.actions("delegate"); len(got) != 1 || got[0] != gateway.Failed {
		t.Fatalf("delegate actions = %v, want [failed]", got)
	}
	if n, _ := tools.SentCount(context.Background(), h.app.DB, demo.AirlineRefunds); n != 0 {
		t.Fatal("worker sent an email outside its allowlist")
	}
	if children, _ := h.app.Tasks.Children(context.Background(), worker); len(children) != 0 {
		t.Fatal("escalating delegation created a task")
	}
}

func TestDepthAndActiveWorkerLimits(t *testing.T) {
	delegations := func(n int) scripted.Provider {
		return func(req model.Request) model.Message {
			if toolTurns(req) > 0 || isWorker(req) {
				return model.Message{Content: "done"}
			}
			var calls []model.ToolCall
			for i := range n {
				calls = append(calls, call(fmt.Sprintf("d%d", i), "delegate",
					map[string]any{"title": fmt.Sprint(i), "objective": "o", "allowed_tools": []string{"web_search"}}))
			}
			return model.Message{ToolCalls: calls}
		}
	}

	h := newHarness(t, delegations(1))
	deep := h.task(tasks.NewTask{Objective: "deep", Depth: 2})
	h.step()
	if got := h.actions("delegate"); len(got) != 1 || got[0] != gateway.Failed {
		t.Fatalf("depth-2 delegate = %v, want [failed]", got)
	}
	if children, _ := h.app.Tasks.Children(context.Background(), deep); len(children) != 0 {
		t.Fatal("delegation beyond max depth created a task")
	}

	h = newHarness(t, delegations(4))
	top := h.task(tasks.NewTask{Objective: "top"})
	h.step()
	statuses := map[string]int{}
	for _, s := range h.actions("delegate") {
		statuses[s]++
	}
	if statuses[gateway.Succeeded] != 3 || statuses[gateway.Failed] != 1 {
		t.Fatalf("delegate statuses = %v, want 3 succeeded and 1 failed", statuses)
	}
	if children, _ := h.app.Tasks.Children(context.Background(), top); len(children) != 3 {
		t.Fatalf("active workers = %d, want 3", len(children))
	}
}

func TestBlockedWorkerIsReportedToParent(t *testing.T) {
	h := newHarness(t, func(req model.Request) model.Message {
		if isWorker(req) {
			return model.Message{Content: "never reached"}
		}
		switch toolTurns(req) {
		case 0:
			return model.Message{ToolCalls: []model.ToolCall{call("d", "delegate",
				map[string]any{"title": "slow", "objective": "o", "allowed_tools": []string{"web_search"}, "deadline_hours": 1})}}
		case 1:
			return model.Message{ToolCalls: []model.ToolCall{call("a", "await_workers", map[string]any{})}}
		}
		return model.Message{Content: "handled the failed worker"}
	})
	parent := h.task(tasks.NewTask{Objective: "parent"})
	h.step()
	if got := h.get(parent).Status; got != tasks.WaitingEvent {
		t.Fatalf("parent = %s, want waiting_event", got)
	}
	h.clock.Advance(2 * time.Hour)
	h.drain()

	if got := h.get(parent).Status; got != tasks.Completed {
		t.Fatalf("parent = %s, want completed", got)
	}
	if result := h.actionResult("await_workers"); !strings.Contains(result, `"status":"blocked"`) || !strings.Contains(result, "deadline passed") {
		t.Fatalf("await result = %s, want the blocked worker and its reason", result)
	}
}
