package notify_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/notify"
	"agentd/internal/policy"
	"agentd/internal/testutil"
	"agentd/internal/tools"
)

func setup(t *testing.T) (*notify.Outbox, *testutil.Clock, string) {
	clock := testutil.NewClock()
	db := testutil.Open(t, testutil.DBPath(t))
	store := &assignments.Store{DB: db, Now: clock.Now}
	id, err := store.CreateResponsibility(context.Background(), assignments.NewResponsibility{
		Name: "watch", Objective: "o",
		Policy: policy.Policy{Limits: policy.Limits{MaxLLMSpendUSD: 1, BudgetPeriod: policy.Duration{Duration: 720 * time.Hour}}},
		Notifications: assignments.NotificationPolicy{
			MinSeverity: assignments.SeverityAttention,
			DedupWindow: policy.Duration{Duration: 24 * time.Hour},
			MaxPerDay:   2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &notify.Outbox{DB: db, Assignments: store, Now: clock.Now}, clock, id
}

func send(t *testing.T, o *notify.Outbox, id, key string, sev assignments.Severity) notify.Result {
	t.Helper()
	r, err := o.Notify(context.Background(), notify.Notification{AssignmentID: id, DedupKey: key, Severity: sev, Title: key, Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDuplicateAlertsAreCollapsedWithinTheWindow(t *testing.T) {
	o, clock, id := setup(t)
	first := send(t, o, id, "thing-x:2026-11-20", assignments.SeverityAction)
	again := send(t, o, id, "thing-x:2026-11-20", assignments.SeverityAction)
	if first.Status != notify.Pending || !again.Duplicate || again.ID != first.ID {
		t.Fatalf("first=%+v again=%+v, want the repeat collapsed into the first", first, again)
	}
	clock.Advance(25 * time.Hour)
	later := send(t, o, id, "thing-x:2026-11-20", assignments.SeverityAction)
	if later.Duplicate || later.ID == first.ID {
		t.Fatalf("after the window = %+v, want a fresh notification", later)
	}
}

func TestSeverityFloorAndDailyCap(t *testing.T) {
	o, _, id := setup(t)
	if r := send(t, o, id, "minor", assignments.SeverityInfo); r.Status != notify.Suppressed {
		t.Fatalf("info below the attention floor = %+v", r)
	}
	send(t, o, id, "a", assignments.SeverityAttention)
	send(t, o, id, "b", assignments.SeverityAttention)
	third := send(t, o, id, "c", assignments.SeverityAction)
	if third.Status != notify.Suppressed || third.Reason == "" {
		t.Fatalf("third alert in a day = %+v, want suppressed by the daily cap", third)
	}
}

func TestToolReconcilesInsteadOfResending(t *testing.T) {
	o, _, id := setup(t)
	tool := &notify.Tool{Outbox: o}
	args, _ := json.Marshal(map[string]string{"severity": "action", "title": "t", "body": "b", "dedup_key": "k"})
	call := tools.Invocation{AssignmentID: id, Arguments: args, IdempotencyKey: "run:1:c"}
	if _, found, _ := tool.Reconcile(context.Background(), call); found {
		t.Fatal("found a notification that was never sent")
	}
	if _, err := tool.Execute(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := tool.Reconcile(context.Background(), call); !found {
		t.Fatal("reconcile missed the sent notification")
	}
	var n int
	o.DB.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&n)
	if n != 1 {
		t.Fatalf("notifications = %d", n)
	}
	for _, bad := range []string{`{"severity":"loud","title":"t","body":"b","dedup_key":"k"}`, `{"severity":"info","title":"t","body":"b"}`} {
		if _, err := tool.Resource(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
