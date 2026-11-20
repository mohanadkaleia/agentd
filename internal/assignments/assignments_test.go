package assignments_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/policy"
	"agentd/internal/testutil"
)

func store(t *testing.T) *assignments.Store {
	clock := testutil.NewClock()
	return &assignments.Store{DB: testutil.Open(t, testutil.DBPath(t)), Now: clock.Now}
}

func day(d time.Duration) policy.Duration { return policy.Duration{Duration: d} }

var watchPolicy = policy.Policy{Limits: policy.Limits{MaxLLMSpendUSD: 2, BudgetPeriod: day(30 * 24 * time.Hour)}}

var calm = assignments.NotificationPolicy{MinSeverity: assignments.SeverityAttention, DedupWindow: day(24 * time.Hour), MaxPerDay: 3}

func TestProjectAndResponsibilityLifecycles(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	project, err := s.CreateProject(ctx, assignments.NewProject{Name: "p", Objective: "finish a thing",
		Completion: assignments.Completion{Criteria: []string{"thing finished"}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.CreateResponsibility(ctx, assignments.NewResponsibility{Name: "r", Objective: "watch a thing",
		Policy: watchPolicy, Notifications: calm})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Revoke(ctx, project, "test"); !errors.Is(err, assignments.ErrWrongKind) {
		t.Fatalf("revoking a project: %v, want ErrWrongKind", err)
	}
	if err := s.Complete(ctx, resp, "test"); !errors.Is(err, assignments.ErrWrongKind) {
		t.Fatalf("completing a responsibility: %v, want ErrWrongKind", err)
	}

	if err := s.Pause(ctx, resp, assignments.PausedByUser, "vacation", "test"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Get(ctx, resp)
	if r.Status != assignments.Paused || r.PausedBy != assignments.PausedByUser || r.StatusReason != "vacation" {
		t.Fatalf("paused responsibility = %+v", r)
	}
	if err := s.Resume(ctx, resp, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, resp, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(ctx, resp, "test"); !errors.Is(err, assignments.ErrNotPaused) {
		t.Fatalf("resuming a revoked responsibility: %v", err)
	}
	if err := s.Pause(ctx, resp, assignments.PausedByUser, "", "test"); !errors.Is(err, assignments.ErrTerminated) {
		t.Fatalf("pausing a revoked responsibility: %v", err)
	}

	if err := s.Complete(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Get(ctx, project)
	if p.Kind != assignments.KindProject || p.Status != assignments.Completed || p.Completion == nil || p.Notifications != nil {
		t.Fatalf("project = %+v", p)
	}
	r, _ = s.Get(ctx, resp)
	if r.Kind != assignments.KindResponsibility || r.Status != assignments.Revoked || r.Notifications.MaxPerDay != 3 {
		t.Fatalf("responsibility = %+v", r)
	}
}

func TestResponsibilitiesMustHaveAPeriodicBudget(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	if _, err := s.CreateResponsibility(ctx, assignments.NewResponsibility{Name: "r", Objective: "o",
		Policy: policy.Policy{Limits: policy.Limits{MaxLLMSpendUSD: 2}}, Notifications: calm}); err == nil {
		t.Fatal("responsibility without a budget period was accepted")
	}
	if _, err := s.CreateResponsibility(ctx, assignments.NewResponsibility{Name: "r", Objective: "o",
		Policy: watchPolicy, Notifications: assignments.NotificationPolicy{MinSeverity: "loud"}}); err == nil {
		t.Fatal("invalid notification policy was accepted")
	}
	id, err := s.CreateResponsibility(ctx, assignments.NewResponsibility{Name: "r", Objective: "o", Policy: watchPolicy, Notifications: calm})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPolicy(ctx, id, policy.Policy{}, "test"); err == nil {
		t.Fatal("removing a responsibility's budget was accepted")
	}
}

func TestStateIsDurableAndMustBeAnObject(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	id, _ := s.CreateResponsibility(ctx, assignments.NewResponsibility{Name: "r", Objective: "o", Policy: watchPolicy, Notifications: calm})
	if err := s.SetState(ctx, id, json.RawMessage(`[1,2]`), "test"); err == nil {
		t.Fatal("non-object state was accepted")
	}
	if err := s.SetState(ctx, id, json.RawMessage(`{"baseline":42}`), "test"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Get(ctx, id)
	if string(r.State) != `{"baseline":42}` {
		t.Fatalf("state = %s", r.State)
	}
}
