package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/model"
	"agentd/internal/model/scripted"
	"agentd/internal/runtime"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

func TestAStaleWorkerCannotDeliverANote(t *testing.T) {
	ctx := context.Background()
	clock := testutil.NewClock()
	db := testutil.Open(t, testutil.DBPath(t))
	a := app.New(db, scripted.Provider(func(model.Request) model.Message { return model.Message{Content: "done"} }),
		"scripted", tools.Registry{}, clock.Now)
	project, _ := a.Assignments.CreateProject(ctx, assignments.NewProject{Name: "p", Objective: "o"})
	task, _ := a.Tasks.Create(ctx, tasks.NewTask{AssignmentID: project, Title: "t", Objective: "o"})
	stale, err := a.Tasks.ClaimReady(ctx, "old", 30*time.Second)
	if err != nil || stale == nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO runs (id, task_id, model_id, status, started_at) VALUES ('run1', ?, 'scripted', 'running', 0)`, task)
	if _, _, err := a.Runtime.Tell(ctx, runtime.Note{Target: task, Text: "mention XY123", By: "owner"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err := a.Tasks.ClaimReady(ctx, "new", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Runtime.DeliverNotes(ctx, *stale, "run1", 2); !errors.Is(err, tasks.ErrLeaseLost) {
		t.Fatalf("stale delivery = %v, want ErrLeaseLost", err)
	}
	if n, _ := runtime.PendingNotes(ctx, db, task); n != 1 {
		t.Fatalf("pending notes = %d; a stale worker marked the note delivered", n)
	}
}
