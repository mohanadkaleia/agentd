package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

type eventsWorld struct {
	t       *testing.T
	app     *app.App
	assign  string
	opening []string
	script  func(req model.Request) model.Message
}

func newEventsWorld(t *testing.T, allowTaskEvents bool) *eventsWorld {
	w := &eventsWorld{t: t}
	clock := testutil.NewClock()
	db := testutil.Open(t, testutil.DBPath(t))
	w.app = app.New(db, scripted.Provider(func(req model.Request) model.Message {
		if len(w.opening) == 0 {
			for _, m := range req.Messages[1:] {
				if m.Role == model.RoleUser {
					w.opening = append(w.opening, m.Content)
				}
			}
		}
		return w.script(req)
	}), "scripted", tools.NewRegistry(tools.WebSearch{}), clock.Now)
	rules := map[string]policy.Rule{"web_search": {Mode: policy.Allow}}
	if allowTaskEvents {
		rules["task_events"] = policy.Rule{Mode: policy.Allow}
	}
	ctx := context.Background()
	var err error
	w.assign, err = w.app.Assignments.CreateResponsibility(ctx, assignments.NewResponsibility{
		Name: "watch", Objective: "o",
		Policy: policy.Policy{Tools: rules, Limits: policy.Limits{MaxLLMSpendUSD: 1, BudgetPeriod: policy.Duration{Duration: 720 * time.Hour}}},
		Notifications: assignments.NotificationPolicy{MinSeverity: assignments.SeverityInfo,
			DedupWindow: policy.Duration{Duration: time.Hour}, MaxPerDay: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.app.Scheduler.AddRoute(ctx, scheduler.Route{AssignmentID: w.assign, Source: "feed", Kind: "changed",
		CoalesceField: "item", Title: "Look", Objective: "Look into it."}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *eventsWorld) event(id, note string) {
	payload, _ := json.Marshal(map[string]string{"item": "A", "note": note})
	ctx := context.Background()
	if _, _, err := w.app.Scheduler.Ingest(ctx, scheduler.Event{Source: "feed", Kind: "changed", ExternalID: id, Payload: payload}); err != nil {
		w.t.Fatal(err)
	}
	if err := w.app.Scheduler.Tick(ctx); err != nil {
		w.t.Fatal(err)
	}
}

func (w *eventsWorld) drain() {
	for range 30 {
		if worked, _ := w.app.Runtime.RunOnce(context.Background(), "w"); !worked {
			return
		}
	}
}

func (w *eventsWorld) onlyTask() tasks.Task {
	var id string
	if err := w.app.DB.QueryRow(`SELECT id FROM tasks`).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	task, _ := w.app.Tasks.Get(context.Background(), id)
	return task
}

func TestEventsGroupedBeforeStartAreInTheOpeningMessages(t *testing.T) {
	w := newEventsWorld(t, false)
	w.script = func(model.Request) model.Message { return model.Message{Content: "done"} }
	w.event("e1", "first")
	w.event("e2", "second")
	w.event("e3", "third")
	w.drain()

	if len(w.opening) != 2 || !strings.Contains(w.opening[1], "2 more event(s)") ||
		!strings.Contains(w.opening[1], "second") || !strings.Contains(w.opening[1], "third") {
		t.Fatalf("opening user messages = %q", w.opening)
	}
	if n, _ := scheduler.UnseenEvents(context.Background(), w.app.DB, w.onlyTask().ID); n != 0 {
		t.Fatalf("unseen after the run opened = %d", n)
	}
}

func TestEventArrivingMidRunMustBeReadBeforeFinishing(t *testing.T) {
	w := newEventsWorld(t, true)
	var sawNew string
	w.script = func(req model.Request) model.Message {
		last := req.Messages[len(req.Messages)-1]
		switch {
		case last.Role == model.RoleUser && strings.HasPrefix(last.Content, "Look into it."):
			w.event("e2", "arrived mid-run")
			return model.Message{Content: "done"}
		case last.Role == model.RoleUser && strings.Contains(last.Content, "Call task_events"):
			return model.Message{ToolCalls: []model.ToolCall{{ID: "te", Name: "task_events", Arguments: json.RawMessage(`{}`)}}}
		case last.Role == model.RoleTool:
			sawNew = last.Content
		}
		return model.Message{Content: "done"}
	}
	w.event("e1", "first")
	w.drain()

	if task := w.onlyTask(); task.Status != tasks.Completed {
		t.Fatalf("task = %s", task.Status)
	}
	if !strings.Contains(sawNew, "arrived mid-run") || !strings.Contains(sawNew, `"new":true`) || !strings.Contains(sawNew, "untrusted") {
		t.Fatalf("task_events result = %s", sawNew)
	}
	var tasksCount int
	w.app.DB.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasksCount)
	if tasksCount != 1 {
		t.Fatalf("tasks = %d, want the mid-run event grouped into the running task", tasksCount)
	}
}

func TestNoNudgeWhenPolicyDoesNotAllowReadingEvents(t *testing.T) {
	w := newEventsWorld(t, false)
	calls := 0
	w.script = func(req model.Request) model.Message {
		calls++
		if calls == 1 {
			w.event("e2", "mid-run")
		}
		return model.Message{Content: "done"}
	}
	w.event("e1", "first")
	w.drain()
	if task := w.onlyTask(); task.Status != tasks.Completed || calls != 1 {
		t.Fatalf("task = %s after %d calls; want completed in 1 (no nudge it could not act on)", task.Status, calls)
	}
}
