package notify_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/notify"
	"agentd/internal/system"
	"agentd/internal/testutil"
)

type phone struct {
	got  []notify.Message
	errs []error
}

func (p *phone) Send(_ context.Context, m notify.Message) (string, error) {
	if len(p.errs) > 0 {
		err := p.errs[0]
		p.errs = p.errs[1:]
		return "", err
	}
	p.got = append(p.got, m)
	return fmt.Sprint("ref-", len(p.got)), nil
}

func deliverer(o *notify.Outbox, clock *testutil.Clock, ch notify.Channel) *notify.Deliverer {
	return &notify.Deliverer{DB: o.DB, Channel: ch, Now: clock.Now, MaxAttempts: 3, Claim: 2 * time.Minute}
}

func deliver(t *testing.T, d *notify.Deliverer) {
	t.Helper()
	if err := d.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, o *notify.Outbox, id string) (string, bool) {
	t.Helper()
	var s string
	var failed bool
	if err := o.DB.QueryRow(`SELECT status, failed_at IS NOT NULL FROM notifications WHERE id = ?`, id).Scan(&s, &failed); err != nil {
		t.Fatal(err)
	}
	return s, failed
}

func TestPendingNotificationsAreDeliveredOnce(t *testing.T) {
	o, clock, id := setup(t)
	n := send(t, o, id, "k", assignments.SeverityAction)
	quiet := send(t, o, id, "minor", assignments.SeverityInfo)
	p := &phone{}
	d := deliverer(o, clock, p)
	deliver(t, d)
	deliver(t, d)
	if len(p.got) != 1 || p.got[0].ID != n.ID || p.got[0].Assignment != "watch" {
		t.Fatalf("delivered = %+v, want the one pending notification once", p.got)
	}
	if s, _ := status(t, o, n.ID); s != notify.Delivered {
		t.Fatalf("status = %s", s)
	}
	if s, _ := status(t, o, quiet.ID); s != notify.Suppressed {
		t.Fatalf("suppressed notification became %s", s)
	}
}

func TestTransientFailuresRetryWithBackoffThenGiveUp(t *testing.T) {
	o, clock, id := setup(t)
	send(t, o, id, "k", assignments.SeverityAction)
	down := errors.New("ntfy returned 503")
	p := &phone{errs: []error{down, down}}
	d := deliverer(o, clock, p)
	deliver(t, d)
	deliver(t, d)
	if len(p.errs) != 1 {
		t.Fatal("retried before the backoff elapsed")
	}
	clock.Advance(31 * time.Second)
	deliver(t, d)
	clock.Advance(61 * time.Second)
	deliver(t, d)
	if len(p.got) != 1 {
		t.Fatalf("delivered = %d after the outage, want 1", len(p.got))
	}

	gone := send(t, o, id, "other", assignments.SeverityAction)
	p.errs = []error{down, down, down, down}
	for range 5 {
		clock.Advance(time.Hour)
		deliver(t, d)
	}
	if s, failed := status(t, o, gone.ID); s != notify.Pending || !failed {
		t.Fatalf("after 3 failed attempts: status %s failed %v, want parked as failed", s, failed)
	}
	if len(p.errs) != 1 {
		t.Fatalf("attempts = %d, want MaxAttempts (3)", 4-len(p.errs))
	}
}

func TestPermanentFailureStopsImmediately(t *testing.T) {
	o, clock, id := setup(t)
	n := send(t, o, id, "k", assignments.SeverityAction)
	p := &phone{errs: []error{fmt.Errorf("%w: ntfy returned 403", notify.ErrPermanent)}}
	d := deliverer(o, clock, p)
	deliver(t, d)
	clock.Advance(2 * time.Hour)
	deliver(t, d)
	if _, failed := status(t, o, n.ID); !failed || len(p.got) != 0 {
		t.Fatalf("failed = %v, sent = %d; want one attempt then parked", failed, len(p.got))
	}
}

func TestHaltAndRevocationStopDelivery(t *testing.T) {
	o, clock, id := setup(t)
	n := send(t, o, id, "k", assignments.SeverityAction)
	p := &phone{}
	d := deliverer(o, clock, p)
	if err := system.Halt(context.Background(), o.DB, clock.Now(), "user", "test"); err != nil {
		t.Fatal(err)
	}
	deliver(t, d)
	if len(p.got) != 0 {
		t.Fatal("delivered while halted")
	}
	system.Resume(context.Background(), o.DB, clock.Now(), "user")
	if err := o.Assignments.Revoke(context.Background(), id, "user"); err != nil {
		t.Fatal(err)
	}
	deliver(t, d)
	if s, _ := status(t, o, n.ID); s != notify.Suppressed || len(p.got) != 0 {
		t.Fatalf("status = %s, sent = %d; want suppressed after revocation", s, len(p.got))
	}
}

func TestCrashAfterClaimRedeliversAfterTheClaimLapses(t *testing.T) {
	o, clock, id := setup(t)
	n := send(t, o, id, "k", assignments.SeverityAction)
	o.DB.Exec(`UPDATE notifications SET attempts = 1, next_attempt_at = ? WHERE id = ?`, clock.Now().Add(2*time.Minute).UnixMilli(), n.ID)
	p := &phone{}
	d := deliverer(o, clock, p)
	deliver(t, d)
	if len(p.got) != 0 {
		t.Fatal("delivered while another attempt held the claim")
	}
	clock.Advance(3 * time.Minute)
	deliver(t, d)
	if len(p.got) != 1 {
		t.Fatal("a claim left by a crash was never retried")
	}
}
