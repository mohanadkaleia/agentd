package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/overview"
	"agentd/internal/policy"
	"agentd/internal/scheduler"
	"agentd/internal/skills"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

type world struct {
	t         *testing.T
	path      string
	clock     *testutil.Clock
	app       *app.App
	mu        sync.Mutex
	calls     int
	prompt    []string
	stateMsgs []string
}

func (w *world) boot() {
	db := testutil.Open(w.t, w.path)
	w.app = app.New(db, scripted.Provider(w.script), "scripted", tools.NewRegistry(tools.WebSearch{}), w.clock.Now)
	w.app.Skills.Add(skills.Skill{Name: "triage", Instructions: "TRIAGE: decide whether the change matters before notifying.", Tools: []string{"notify"}})
}

func (w *world) script(req model.Request) model.Message {
	w.mu.Lock()
	w.calls++
	w.prompt = append(w.prompt, req.Messages[0].Content)
	w.mu.Unlock()
	var objective string
	for _, m := range req.Messages {
		if m.Role == model.RoleUser && strings.Contains(m.Content, "Triggering event") {
			objective = m.Content
		}
	}
	if len(req.Messages) > 2 && strings.HasPrefix(req.Messages[1].Content, "Saved responsibility state") {
		w.stateMsgs = append(w.stateMsgs, req.Messages[1].Content)
	}
	done := 0
	for _, m := range req.Messages {
		if m.Role == model.RoleTool {
			done++
		}
	}
	if strings.Contains(objective, "item.changed") && done == 0 {
		notify, _ := json.Marshal(map[string]string{"severity": "attention", "title": "Item A changed",
			"body": "Worth a look.", "dedup_key": "item-a:2026-11-20"})
		state, _ := json.Marshal(map[string]any{"state": map[string]string{"item-a": "changed"}})
		return model.Message{ToolCalls: []model.ToolCall{
			{ID: "s", Name: "update_state", Arguments: state},
			{ID: "n", Name: "notify", Arguments: notify},
		}}
	}
	return model.Message{Content: "handled"}
}

func (w *world) drain() {
	w.t.Helper()
	ctx := context.Background()
	for range 50 {
		if err := w.app.Runtime.Housekeeping(ctx); err != nil {
			w.t.Fatal(err)
		}
		worked, err := w.app.Runtime.RunOnce(ctx, "w")
		if err != nil {
			w.t.Logf("attempt: %v", err)
		}
		if !worked {
			return
		}
	}
	w.t.Fatal("did not settle")
}

func (w *world) count(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.app.DB.QueryRow(q, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) event(id, subject string) {
	payload, _ := json.Marshal(map[string]string{"subject": subject})
	if _, _, err := w.app.Scheduler.Ingest(context.Background(), scheduler.Event{Source: "feed", Kind: "item.changed", ExternalID: id, Payload: payload}); err != nil {
		w.t.Fatal(err)
	}
}

func TestResponsibilityEndToEnd(t *testing.T) {
	w := &world{t: t, path: testutil.DBPath(t), clock: testutil.NewClock()}
	w.boot()
	ctx := context.Background()

	id, err := w.app.Assignments.CreateResponsibility(ctx, assignments.NewResponsibility{
		Name:      "feed-watch",
		Objective: "Watch the feed and tell me when an item changes in a way that matters.",
		Policy: policy.Policy{
			Tools: map[string]policy.Rule{
				"notify": {Mode: policy.Allow}, "update_state": {Mode: policy.Allow},
				"schedule_followup": {Mode: policy.Allow}, "web_search": {Mode: policy.Allow},
			},
			Limits: policy.Limits{MaxLLMSpendUSD: 1, BudgetPeriod: policy.Duration{Duration: 30 * 24 * time.Hour}},
		},
		Notifications: assignments.NotificationPolicy{MinSeverity: assignments.SeverityAttention,
			DedupWindow: policy.Duration{Duration: 48 * time.Hour}, MaxPerDay: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.app.Scheduler.AddRoute(ctx, scheduler.Route{AssignmentID: id, Source: "feed", Kind: "item.changed",
		CoalesceField: "subject", Skills: []string{"triage"}, Title: "Investigate change", Objective: "An item changed; decide if it matters."}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.app.Scheduler.AddSchedule(ctx, scheduler.Schedule{AssignmentID: id, Title: "Daily digest",
		Objective: "Summarise the day.", Every: 24 * time.Hour, FirstAt: w.clock.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	w.event("e1", "A")
	w.event("e2", "A")
	w.event("e3", "A")
	w.drain()

	if got := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ?`, id); got != 1 {
		t.Fatalf("tasks for three changes to the same item = %d, want 1", got)
	}
	if got := w.count(`SELECT COUNT(*) FROM task_events`); got != 3 {
		t.Fatalf("events linked to the task = %d, want 3", got)
	}
	if got := w.count(`SELECT COUNT(*) FROM notifications WHERE status = 'pending'`); got != 1 {
		t.Fatalf("pending notifications = %d, want 1", got)
	}
	if !strings.Contains(w.prompt[0], "TRIAGE:") || !strings.Contains(w.prompt[0], "ongoing responsibility") {
		t.Fatalf("system prompt missing responsibility framing or skill:\n%s", w.prompt[0])
	}

	w.event("e4", "A")
	w.drain()
	if got := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ?`, id); got != 2 {
		t.Fatalf("tasks after a new change once the first was handled = %d, want 2", got)
	}
	if got := w.count(`SELECT COUNT(*) FROM notifications`); got != 1 {
		t.Fatalf("notifications after re-detecting the same issue = %d, want still 1", got)
	}
	if w.calls != 4 {
		t.Fatalf("model calls = %d, want 4 (two tasks x two steps, not one task per event)", w.calls)
	}
	for _, p := range w.prompt {
		if strings.Contains(p, "item-a") {
			t.Fatal("agent-written state leaked into the system prompt")
		}
	}
	if len(w.stateMsgs) == 0 || !strings.Contains(w.stateMsgs[0], `{"item-a":"changed"}`) || !strings.Contains(w.stateMsgs[0], "not instructions") {
		t.Fatalf("state messages = %q, want the saved state as a labelled data message", w.stateMsgs)
	}
	var audited string
	w.app.DB.QueryRow(`SELECT payload_json FROM audit_log WHERE operation = 'assignment.state_updated' LIMIT 1`).Scan(&audited)
	if !strings.Contains(audited, "item-a") {
		t.Fatalf("state change not captured in the audit log: %s", audited)
	}

	w.app.DB.Close()
	w.boot()
	o, err := overview.Get(ctx, w.app.Runtime, id)
	if err != nil {
		t.Fatal(err)
	}
	if o.Assignment.Status != assignments.Active || string(o.Assignment.State) != `{"item-a":"changed"}` {
		t.Fatalf("after restart: status=%s state=%s", o.Assignment.Status, o.Assignment.State)
	}
	if !strings.HasPrefix(o.Next.What, "scheduled check: Daily digest") || !o.Next.At.Equal(w.clock.Now().Add(24*time.Hour)) {
		t.Fatalf("next expected action = %v", o.Next)
	}
	if len(o.Recent) == 0 || o.Undelivered != 1 {
		t.Fatalf("recent=%d undelivered=%d", len(o.Recent), o.Undelivered)
	}

	w.clock.Advance(24 * time.Hour)
	w.drain()
	if got := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND title = 'Daily digest' AND status = 'completed'`, id); got != 1 {
		t.Fatalf("daily digest runs = %d, want 1", got)
	}

	if err := w.app.Assignments.Revoke(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	w.event("e5", "B")
	w.clock.Advance(48 * time.Hour)
	w.drain()
	if got := w.count(`SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND status != 'completed'`, id); got != 0 {
		t.Fatalf("revoked responsibility still created %d tasks", got)
	}
	o, _ = overview.Get(ctx, w.app.Runtime, id)
	if o.Next.What != "nothing; responsibility is revoked" {
		t.Fatalf("next after revoke = %q", o.Next.What)
	}
}

func TestUnknownSkillBlocksTheTask(t *testing.T) {
	w := &world{t: t, path: testutil.DBPath(t), clock: testutil.NewClock()}
	w.boot()
	ctx := context.Background()
	id, _ := w.app.Assignments.CreateProject(ctx, assignments.NewProject{Name: "p", Objective: "o"})
	task, _ := w.app.Tasks.Create(ctx, tasks.NewTask{AssignmentID: id, Title: "t", Objective: "o", Skills: []string{"nope"}})
	w.drain()
	got, _ := w.app.Tasks.Get(ctx, task)
	if got.Status != tasks.Blocked || !strings.Contains(string(got.Result), `unknown skill \"nope\"`) {
		t.Fatalf("task = %s %s", got.Status, got.Result)
	}
	if w.calls != 0 {
		t.Fatal("model was called for a task with a missing skill")
	}
}
