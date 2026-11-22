package notify

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/system"
)

var ErrPermanent = errors.New("delivery failed permanently")

type Message struct {
	ID         string
	Assignment string
	Severity   assignments.Severity
	Title      string
	Body       string
}

type Channel interface {
	Send(ctx context.Context, m Message) (string, error)
}

type Deliverer struct {
	DB      *sql.DB
	Channel Channel
	// Covered reports that another path (a paired chat channel) already brings every
	// notification to the owner's phone; then a push would only repeat it.
	Covered     func(context.Context) (bool, error)
	Now         func() time.Time
	MaxAttempts int
	Claim       time.Duration
}

func (d *Deliverer) DeliverDue(ctx context.Context) error {
	if d.Channel == nil {
		return nil
	}
	if s, err := system.Get(ctx, d.DB); err != nil || s.Halted {
		return err
	}
	if err := d.suppressEnded(ctx); err != nil {
		return err
	}
	for range 20 {
		m, attempts, ok, err := d.claim(ctx)
		if err != nil || !ok {
			return err
		}
		if d.Covered != nil {
			covered, err := d.Covered(ctx)
			if err != nil {
				return err
			}
			if covered {
				if err := d.settle(ctx, m, attempts, "sent through a paired chat channel", nil); err != nil {
					return err
				}
				continue
			}
		}
		ref, err := d.Channel.Send(ctx, m)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := d.settle(ctx, m, attempts, ref, err); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deliverer) suppressEnded(ctx context.Context) error {
	now := d.Now()
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `UPDATE notifications SET status = 'suppressed', suppressed_reason = 'assignment ended before delivery'
		WHERE status = 'pending' AND assignment_id IN (SELECT id FROM assignments WHERE status IN ('revoked', 'cancelled'))
		RETURNING id, assignment_id`)
	if err != nil {
		return err
	}
	var ended [][2]string
	for rows.Next() {
		var id, assignment string
		if err := rows.Scan(&id, &assignment); err != nil {
			rows.Close()
			return err
		}
		ended = append(ended, [2]string{id, assignment})
	}
	rows.Close()
	for _, e := range ended {
		if err := audit.Record(ctx, tx, now, e[1], "delivery", "notification.suppressed", e[0], nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *Deliverer) claim(ctx context.Context) (Message, int, bool, error) {
	now := d.Now()
	var m Message
	var attempts int
	err := d.DB.QueryRowContext(ctx, `UPDATE notifications SET attempts = attempts + 1, next_attempt_at = ?
		WHERE id = (SELECT id FROM notifications
			WHERE status = 'pending' AND failed_at IS NULL AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
			ORDER BY created_at, id LIMIT 1)
		RETURNING id, severity, title, body, attempts,
			(SELECT name FROM assignments WHERE assignments.id = notifications.assignment_id)`,
		now.Add(d.Claim).UnixMilli(), now.UnixMilli()).Scan(&m.ID, &m.Severity, &m.Title, &m.Body, &attempts, &m.Assignment)
	if errors.Is(err, sql.ErrNoRows) {
		return m, 0, false, nil
	}
	return m, attempts, err == nil, err
}

func (d *Deliverer) settle(ctx context.Context, m Message, attempts int, ref string, sendErr error) error {
	now := d.Now()
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var assignmentID string
	switch {
	case sendErr == nil:
		err = tx.QueryRowContext(ctx, `UPDATE notifications SET status = 'delivered', delivered_at = ?, channel_ref = NULLIF(?, ''), last_error = NULL
			WHERE id = ? AND status = 'pending' RETURNING assignment_id`, now.UnixMilli(), ref, m.ID).Scan(&assignmentID)
		if err == nil {
			err = audit.Record(ctx, tx, now, assignmentID, "delivery", "notification.delivered", m.ID, nil)
		}
	case errors.Is(sendErr, ErrPermanent) || attempts >= d.MaxAttempts:
		err = tx.QueryRowContext(ctx, `UPDATE notifications SET failed_at = ?, last_error = ?
			WHERE id = ? AND status = 'pending' RETURNING assignment_id`, now.UnixMilli(), sendErr.Error(), m.ID).Scan(&assignmentID)
		if err == nil {
			err = audit.Record(ctx, tx, now, assignmentID, "delivery", "notification.delivery_failed", m.ID,
				map[string]any{"attempts": attempts, "error": sendErr.Error()})
		}
	default:
		_, err = tx.ExecContext(ctx, `UPDATE notifications SET next_attempt_at = ?, last_error = ? WHERE id = ? AND status = 'pending'`,
			now.Add(backoff(attempts)).UnixMilli(), sendErr.Error(), m.ID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func backoff(attempts int) time.Duration {
	wait := 30 * time.Second
	for range attempts - 1 {
		wait *= 2
		if wait >= time.Hour {
			return time.Hour
		}
	}
	return wait
}
