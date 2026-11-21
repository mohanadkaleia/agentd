package outcomes_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/model/scripted"
	"agentd/internal/outcomes"
	"agentd/internal/scheduler"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

type world struct {
	t       *testing.T
	clock   *testutil.Clock
	app     *app.App
	project string
}

var refunded = assignments.EventEvidence{Source: "bank", Kind: "transaction.posted", Match: map[string]string{"merchant": "Airline", "amount": "120"}}

func setup(t *testing.T, criteria ...assignments.Criterion) *world {
	w := &world{t: t, clock: testutil.NewClock()}
	db := testutil.Open(t, testutil.DBPath(t))
	w.app = app.New(db, scripted.Provider(nil), "scripted", tools.Registry{}, w.clock.Now)
	var err error
	w.project, err = w.app.Assignments.CreateProject(context.Background(), assignments.NewProject{
		Name: "refund", Objective: "get the refund", Completion: assignments.Completion{Criteria: criteria}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) event(id string, payload map[string]any) {
	w.t.Helper()
	body, _ := json.Marshal(payload)
	if _, _, err := w.app.Scheduler.Ingest(context.Background(), scheduler.Event{Source: "bank", Kind: "transaction.posted", ExternalID: id, Payload: body}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) tick() {
	w.t.Helper()
	w.clock.Advance(time.Second)
	if err := w.app.Outcomes.Tick(context.Background()); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) status() string {
	a, err := w.app.Assignments.Get(context.Background(), w.project)
	if err != nil {
		w.t.Fatal(err)
	}
	return a.Status
}

func (w *world) count(q string, args ...any) int {
	var n int
	if err := w.app.DB.QueryRow(q, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestProjectCompletesOnceWhenTrustedEvidenceArrives(t *testing.T) {
	w := setup(t, assignments.Criterion{ID: "refund", Description: "Refund posted", Event: &refunded})
	w.event("tx-1", map[string]any{"merchant": "Airline", "amount": 80})
	w.tick()
	if w.status() != assignments.Active {
		t.Fatal("completed on a transaction that did not match")
	}
	w.event("tx-2", map[string]any{"merchant": "Airline", "amount": 120})
	w.tick()
	w.tick()
	if w.status() != assignments.Completed {
		t.Fatalf("status = %s, want completed once the matching transaction posted", w.status())
	}
	if n := w.count(`SELECT COUNT(*) FROM audit_log WHERE operation = 'project.completed'`); n != 1 {
		t.Fatalf("completions = %d, want 1", n)
	}
	if n := w.count(`SELECT COUNT(*) FROM notifications WHERE assignment_id = ?`, w.project); n != 1 {
		t.Fatalf("notifications = %d, want one completion notice", n)
	}
	progress, _ := outcomes.Criteria(context.Background(), w.app.DB, mustGet(t, w))
	if len(progress) != 1 || !progress[0].Verified || progress[0].Kind != "event" || progress[0].EventID == "" {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestEvidenceFromBeforeTheProjectDoesNotCount(t *testing.T) {
	w := setup(t)
	w.event("tx-old", map[string]any{"merchant": "Airline", "amount": 120})
	w.clock.Advance(time.Minute)
	later, err := w.app.Assignments.CreateProject(context.Background(), assignments.NewProject{Name: "again", Objective: "o",
		Completion: assignments.Completion{Criteria: []assignments.Criterion{{Description: "Refund posted", Event: &refunded}}}})
	if err != nil {
		t.Fatal(err)
	}
	w.project = later
	w.tick()
	if w.status() != assignments.Active {
		t.Fatal("an event from before the project existed completed it")
	}
}

func TestSentEmailAloneLeavesTheProjectOpenUntilTheOwnerConfirms(t *testing.T) {
	w := setup(t, assignments.Criterion{Description: "Refund received or formally refused in writing"})
	ctx := context.Background()
	if _, err := w.app.Tasks.Create(ctx, tasks.NewTask{AssignmentID: w.project, Title: "send dispute", Objective: "o"}); err != nil {
		t.Fatal(err)
	}
	w.app.DB.Exec(`UPDATE tasks SET status = 'completed'`)
	w.tick()
	if w.status() != assignments.Active {
		t.Fatal("a finished task completed the project")
	}
	if err := w.app.Outcomes.Confirm(ctx, w.project, "2", "cli", ""); err != outcomes.ErrUnknownCriterion {
		t.Fatalf("confirming an unknown criterion err = %v", err)
	}
	if err := w.app.Outcomes.Confirm(ctx, w.project, "1", "cli", "refund on statement"); err != nil {
		t.Fatal(err)
	}
	w.tick()
	if w.status() != assignments.Completed {
		t.Fatalf("status = %s, want completed after the owner confirmed", w.status())
	}
}

func TestUnresolvedActionAndPauseHoldCompletion(t *testing.T) {
	w := setup(t, assignments.Criterion{Description: "done"})
	ctx := context.Background()
	taskID, _ := w.app.Tasks.Create(ctx, tasks.NewTask{AssignmentID: w.project, Title: "t", Objective: "o"})
	now := w.clock.Now().UnixMilli()
	w.app.DB.Exec(`INSERT INTO runs (id, task_id, model_id, status, started_at) VALUES ('run_1', ?, 'm', 'blocked', ?)`, taskID, now)
	if _, err := w.app.DB.Exec(`INSERT INTO actions (id, run_id, task_id, assignment_id, tool_name, arguments_json, arguments_hash, resource,
		policy_version, status, idempotency_key, created_at, updated_at)
		VALUES ('act_1', 'run_1', ?, ?, 'gmail_send', '{}', 'h', '', 1, 'needs_reconciliation', 'run_1:1:c', ?, ?)`, taskID, w.project, now, now); err != nil {
		t.Fatal(err)
	}
	w.app.Outcomes.Confirm(ctx, w.project, "1", "cli", "")
	w.tick()
	if w.status() != assignments.Active {
		t.Fatal("completed while an action's outcome was unknown")
	}
	w.app.DB.Exec(`UPDATE actions SET status = 'succeeded'`)
	w.app.Assignments.Pause(ctx, w.project, assignments.PausedByUser, "", "cli")
	w.tick()
	if w.status() != assignments.Paused {
		t.Fatal("a paused project was completed")
	}
	w.app.Assignments.Resume(ctx, w.project, "cli")
	w.tick()
	if w.status() != assignments.Completed {
		t.Fatalf("status = %s, want completed once resumed with nothing unresolved", w.status())
	}
}

func TestPlainTextCriteriaStillLoad(t *testing.T) {
	var c assignments.Completion
	if err := json.Unmarshal([]byte(`{"criteria":["first","second"]}`), &c); err != nil {
		t.Fatal(err)
	}
	w := setup(t, c.Criteria...)
	a := mustGet(t, w)
	if len(a.Completion.Criteria) != 2 || a.Completion.Criteria[1].ID != "2" || a.Completion.Criteria[1].Description != "second" {
		t.Fatalf("criteria = %+v", a.Completion.Criteria)
	}
}

func mustGet(t *testing.T, w *world) assignments.Assignment {
	a, err := w.app.Assignments.Get(context.Background(), w.project)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
