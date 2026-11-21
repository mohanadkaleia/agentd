package tasks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/tasks"
	"agentd/internal/testutil"
)

const ttl = 30 * time.Second

type fixture struct {
	clock       *testutil.Clock
	assignments *assignments.Store
	tasks       *tasks.Store
	project     string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db := testutil.Open(t, testutil.DBPath(t))
	clock := testutil.NewClock()
	f := &fixture{
		clock:       clock,
		assignments: &assignments.Store{DB: db, Now: clock.Now},
		tasks:       &tasks.Store{DB: db, Now: clock.Now},
	}
	var err error
	f.project, err = f.assignments.CreateProject(context.Background(), assignments.NewProject{Name: "p", Objective: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) newTask(t *testing.T, nt tasks.NewTask) string {
	t.Helper()
	nt.AssignmentID = f.project
	if nt.Title == "" {
		nt.Title = "t"
	}
	nt.Objective = "o"
	id, err := f.tasks.Create(context.Background(), nt)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) claim(t *testing.T, owner string) *tasks.Lease {
	t.Helper()
	l, err := f.tasks.ClaimReady(context.Background(), owner, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (f *fixture) status(t *testing.T, id string) tasks.Task {
	t.Helper()
	task, err := f.tasks.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestClaimIsExclusive(t *testing.T) {
	f := setup(t)
	id := f.newTask(t, tasks.NewTask{})

	l := f.claim(t, "a")
	if l == nil || l.TaskID != id {
		t.Fatalf("first claim = %+v, want task %s", l, id)
	}
	if again := f.claim(t, "b"); again != nil {
		t.Fatalf("second claim got %+v while lease is live", again)
	}
	if got := f.status(t, id); got.Status != tasks.Running || got.LeaseOwner != "a" {
		t.Fatalf("task = %s owned by %q, want running owned by a", got.Status, got.LeaseOwner)
	}
}

func TestClaimOrdersByPriority(t *testing.T) {
	f := setup(t)
	f.newTask(t, tasks.NewTask{Title: "low"})
	high := f.newTask(t, tasks.NewTask{Title: "high", Priority: 5})
	if l := f.claim(t, "a"); l.TaskID != high {
		t.Fatalf("claimed %s, want high-priority %s", l.TaskID, high)
	}
}

func TestExpiredLeaseIsReclaimedAndCounted(t *testing.T) {
	f := setup(t)
	id := f.newTask(t, tasks.NewTask{})
	first := f.claim(t, "a")

	f.clock.Advance(ttl + time.Second)
	second := f.claim(t, "b")
	if second == nil || second.TaskID != id {
		t.Fatal("expired lease was not reclaimed")
	}
	if got := f.status(t, id); got.AttemptCount != 1 || got.LeaseOwner != "b" {
		t.Fatalf("attempts=%d owner=%q, want 1 and b", got.AttemptCount, got.LeaseOwner)
	}

	ctx := context.Background()
	if _, err := f.tasks.Renew(ctx, *first, ttl); !errors.Is(err, tasks.ErrLeaseLost) {
		t.Fatalf("stale renew err = %v, want ErrLeaseLost", err)
	}
	if err := f.tasks.Release(ctx, *first, tasks.Release{Status: tasks.Completed}); !errors.Is(err, tasks.ErrLeaseLost) {
		t.Fatalf("stale release err = %v, want ErrLeaseLost", err)
	}
	if got := f.status(t, id); got.Status != tasks.Running {
		t.Fatalf("stale owner changed status to %s", got.Status)
	}
}

func TestRenewKeepsLeaseAlive(t *testing.T) {
	f := setup(t)
	f.newTask(t, tasks.NewTask{})
	l := f.claim(t, "a")

	f.clock.Advance(ttl - time.Second)
	if _, err := f.tasks.Renew(context.Background(), *l, ttl); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(ttl - time.Second)
	if other := f.claim(t, "b"); other != nil {
		t.Fatal("renewed lease was stolen")
	}
}

func TestExpiredLeaseCannotBeRenewed(t *testing.T) {
	f := setup(t)
	f.newTask(t, tasks.NewTask{})
	l := f.claim(t, "a")
	f.clock.Advance(ttl + time.Second)
	if _, err := f.tasks.Renew(context.Background(), *l, ttl); !errors.Is(err, tasks.ErrLeaseLost) {
		t.Fatalf("renew after expiry err = %v, want ErrLeaseLost", err)
	}
	if other := f.claim(t, "b"); other == nil {
		t.Fatal("expired lease was revived instead of being reclaimable")
	}
}

func TestRepeatedLeaseExpiryBlocksTask(t *testing.T) {
	f := setup(t)
	id := f.newTask(t, tasks.NewTask{MaxAttempts: 2})
	f.claim(t, "a")
	f.clock.Advance(ttl + time.Second)
	f.claim(t, "b")
	f.clock.Advance(ttl + time.Second)

	if l := f.claim(t, "c"); l != nil {
		t.Fatalf("claimed %s after attempts were exhausted", l.TaskID)
	}
	if got := f.status(t, id); got.Status != tasks.Blocked {
		t.Fatalf("status = %s, want blocked", got.Status)
	}
}

func TestReleaseTransitions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.newTask(t, tasks.NewTask{})

	l := f.claim(t, "a")
	retryAt := f.clock.Now().Add(time.Minute)
	if err := f.tasks.Release(ctx, *l, tasks.Release{Status: tasks.Retryable, NextRunAt: retryAt, CountFailure: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, id); got.Status != tasks.Retryable || got.AttemptCount != 1 || got.LeaseOwner != "" {
		t.Fatalf("after retryable: %+v", got)
	}
	if early := f.claim(t, "a"); early != nil {
		t.Fatal("retryable task claimed before next_run_at")
	}

	f.clock.Advance(time.Minute)
	l = f.claim(t, "a")
	if l == nil {
		t.Fatal("retryable task not claimed after backoff")
	}
	if err := f.tasks.Release(ctx, *l, tasks.Release{Status: tasks.WaitingApproval}); err != nil {
		t.Fatal(err)
	}
	if waiting := f.claim(t, "a"); waiting != nil {
		t.Fatal("task waiting for approval was claimed")
	}
	if err := f.tasks.Release(ctx, *l, tasks.Release{Status: tasks.Completed}); !errors.Is(err, tasks.ErrLeaseLost) {
		t.Fatalf("double release err = %v, want ErrLeaseLost", err)
	}
}

func TestPausedProjectIsNotClaimed(t *testing.T) {
	f := setup(t)
	f.newTask(t, tasks.NewTask{})
	if err := f.assignments.Pause(context.Background(), f.project, assignments.PausedByUser, "", "test"); err != nil {
		t.Fatal(err)
	}
	if l := f.claim(t, "a"); l != nil {
		t.Fatal("claimed a task in a paused project")
	}
	if err := f.assignments.Resume(context.Background(), f.project, "test"); err != nil {
		t.Fatal(err)
	}
	if l := f.claim(t, "a"); l == nil {
		t.Fatal("task not claimable after resume")
	}
}

func TestDependenciesGateClaims(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first := f.newTask(t, tasks.NewTask{Title: "first"})
	second := f.newTask(t, tasks.NewTask{Title: "second", Priority: 9, DependsOn: []string{first}})

	l := f.claim(t, "a")
	if l.TaskID != first {
		t.Fatalf("claimed %s before its dependency", l.TaskID)
	}
	if err := f.tasks.Release(ctx, *l, tasks.Release{Status: tasks.Completed}); err != nil {
		t.Fatal(err)
	}
	if l := f.claim(t, "a"); l == nil || l.TaskID != second {
		t.Fatal("dependent task not claimable after dependency completed")
	}
}

func TestChildFinishingWhileParentRunsIsNotLost(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	parent := f.newTask(t, tasks.NewTask{Title: "parent", Priority: 1})
	child := f.newTask(t, tasks.NewTask{Title: "child", ParentTaskID: parent, Depth: 1})

	parentLease := f.claim(t, "p")
	childLease := f.claim(t, "c")
	if parentLease.TaskID != parent || childLease.TaskID != child {
		t.Fatal("unexpected claim order")
	}
	if err := f.tasks.Release(ctx, *childLease, tasks.Release{Status: tasks.Completed}); err != nil {
		t.Fatal(err)
	}
	if err := f.tasks.Release(ctx, *parentLease, tasks.Release{Status: tasks.WaitingEvent}); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, parent).Status; got != tasks.Ready {
		t.Fatalf("parent = %s, want ready (the child's wakeup arrived before it started waiting)", got)
	}
}

func TestChildFinishingWakesWaitingParent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	parent := f.newTask(t, tasks.NewTask{Title: "parent", Priority: 1})
	child := f.newTask(t, tasks.NewTask{Title: "child", ParentTaskID: parent, Depth: 1})

	parentLease := f.claim(t, "p")
	if err := f.tasks.Release(ctx, *parentLease, tasks.Release{Status: tasks.WaitingEvent}); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, parent).Status; got != tasks.WaitingEvent {
		t.Fatalf("parent = %s, want waiting_event", got)
	}
	childLease := f.claim(t, "c")
	if childLease == nil || childLease.TaskID != child {
		t.Fatal("child not claimable while parent waits")
	}
	if err := f.tasks.Release(ctx, *childLease, tasks.Release{Status: tasks.Blocked}); err != nil {
		t.Fatal(err)
	}
	if got := f.status(t, parent).Status; got != tasks.Ready {
		t.Fatalf("parent = %s, want ready after child finished", got)
	}
}
