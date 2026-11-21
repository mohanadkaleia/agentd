package watchdog_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentd/internal/app"
	"agentd/internal/assignments"
	"agentd/internal/model/scripted"
	"agentd/internal/notify"
	"agentd/internal/policy"
	"agentd/internal/system"
	"agentd/internal/testutil"
	"agentd/internal/tools"
	"agentd/internal/watchdog"
)

type world struct {
	t     *testing.T
	clock *testutil.Clock
	app   *app.App
	id    string
}

func setup(t *testing.T, o watchdog.Obligation) *world {
	w := &world{t: t, clock: testutil.NewClock()}
	w.app = app.New(testutil.Open(t, testutil.DBPath(t)), scripted.Provider(nil), "scripted", tools.Registry{}, w.clock.Now)
	var err error
	w.id, err = w.app.Assignments.CreateResponsibility(context.Background(), assignments.NewResponsibility{
		Name: "brief", Objective: "send me a daily brief",
		Policy: policy.Policy{Limits: policy.Limits{MaxLLMSpendUSD: 5, BudgetPeriod: policy.Duration{Duration: 720 * time.Hour}}},
		Notifications: assignments.NotificationPolicy{MinSeverity: assignments.SeverityInfo,
			DedupWindow: policy.Duration{Duration: 24 * time.Hour}, MaxPerDay: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	o.AssignmentID = w.id
	if _, err := w.app.Watchdog.Add(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) tick() {
	w.t.Helper()
	if err := w.app.Watchdog.Tick(context.Background()); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) count(q string, args ...any) int {
	var n int
	if err := w.app.DB.QueryRow(q, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) remediations() int {
	return w.count(`SELECT COUNT(*) FROM tasks WHERE origin_key LIKE 'watchdog:%'`)
}

var daily = watchdog.Obligation{Description: "daily brief delivered", Every: 24 * time.Hour, Grace: 2 * time.Hour,
	Evidence: watchdog.Evidence{Delivered: "brief:"}}

func TestUndeliveredReportLeavesTheObligationUnmet(t *testing.T) {
	w := setup(t, daily)
	ctx := context.Background()
	w.clock.Advance(20 * time.Hour)
	n, err := w.app.Outbox.Notify(ctx, notify.Notification{AssignmentID: w.id, DedupKey: "brief:day1", Severity: assignments.SeverityInfo, Title: "Brief", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(7 * time.Hour)
	w.tick()
	w.tick()
	if got := w.remediations(); got != 1 {
		t.Fatalf("remediation tasks = %d, want 1 while the brief was produced but never delivered", got)
	}

	if err := w.app.Outbox.MarkDelivered(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	health, _ := watchdog.Status(ctx, w.app.DB, w.id, w.clock.Now())
	if health[0].Overdue || health[0].LastEvidence.IsZero() {
		t.Fatalf("health = %+v, want satisfied by the late delivery", health[0])
	}
	w.tick()
	if got := w.remediations(); got != 1 {
		t.Fatalf("remediation tasks = %d after the late evidence, want no new work", got)
	}
}

func TestOutageCatchesUpOnceAndEscalates(t *testing.T) {
	w := setup(t, daily)
	w.clock.Advance(4 * 24 * time.Hour)
	w.tick()
	w.tick()
	if got := w.remediations(); got != 1 {
		t.Fatalf("remediation tasks after 4 days down = %d, want one catch-up task", got)
	}
	if got := w.count(`SELECT COUNT(*) FROM notifications WHERE severity = 'action' AND dedup_key LIKE '%escalated'`); got != 1 {
		t.Fatalf("escalations = %d, want one for repeated misses", got)
	}
}

func TestRemediationIsOnePerMissedInterval(t *testing.T) {
	w := setup(t, daily)
	w.clock.Advance(27 * time.Hour)
	w.tick()
	w.app.DB.Exec(`UPDATE tasks SET status = 'completed'`)
	w.tick()
	if got := w.remediations(); got != 1 {
		t.Fatalf("remediations = %d, want no second task in the same interval", got)
	}
	w.app.DB.Exec(`UPDATE tasks SET status = 'blocked'`)
	w.clock.Advance(24 * time.Hour)
	w.tick()
	if got := w.remediations(); got != 1 {
		t.Fatal("a new task was spawned while the previous remediation was blocked")
	}
	w.app.DB.Exec(`UPDATE tasks SET status = 'completed'`)
	w.tick()
	if got := w.remediations(); got != 2 {
		t.Fatalf("remediations = %d, want a second one for the next missed interval", got)
	}
}

func TestStalledFeedNotifiesInsteadOfSpawningWork(t *testing.T) {
	w := setup(t, watchdog.Obligation{Description: "Gmail events keep arriving", Every: 6 * time.Hour,
		Evidence: watchdog.Evidence{Event: &watchdog.EventSource{Source: "gmail", Kind: "email.received"}}})
	w.clock.Advance(7 * time.Hour)
	w.tick()
	w.tick()
	if w.remediations() != 0 {
		t.Fatal("a stalled integration spawned an agent task")
	}
	if got := w.count(`SELECT COUNT(*) FROM notifications WHERE title LIKE 'Overdue:%'`); got != 1 {
		t.Fatalf("overdue notices = %d, want 1", got)
	}
}

func TestConcurrentTicksCreateOneRemediation(t *testing.T) {
	w := setup(t, daily)
	w.clock.Advance(27 * time.Hour)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.app.Watchdog.Tick(context.Background())
		}()
	}
	wg.Wait()
	if got := w.remediations(); got != 1 {
		t.Fatalf("remediations = %d, want 1", got)
	}
}

func TestPausedRevokedAndHaltedCreateNothing(t *testing.T) {
	w := setup(t, daily)
	ctx := context.Background()
	w.clock.Advance(27 * time.Hour)
	system.Halt(ctx, w.app.DB, w.clock.Now(), "user", "test")
	w.tick()
	system.Resume(ctx, w.app.DB, w.clock.Now(), "user")
	w.app.Assignments.Pause(ctx, w.id, assignments.PausedByBudget, "budget", "budget")
	w.tick()
	w.app.Assignments.Revoke(ctx, w.id, "user")
	w.tick()
	if w.remediations() != 0 || w.count(`SELECT COUNT(*) FROM notifications`) != 0 {
		t.Fatal("watchdog acted on a halted, paused or revoked responsibility")
	}
}

func TestObligationNeedsExactlyOneKindOfEvidence(t *testing.T) {
	w := setup(t, daily)
	_, err := w.app.Watchdog.Add(context.Background(), watchdog.Obligation{AssignmentID: w.id, Description: "x", Every: time.Hour,
		Evidence: watchdog.Evidence{Delivered: "a", Tool: "notify"}})
	if err == nil {
		t.Fatal("accepted an obligation with two kinds of evidence")
	}
}
