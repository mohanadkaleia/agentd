package system

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentd/internal/audit"
)

const haltedKey = "halted"

type Status struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason,omitempty"`
	Since  int64  `json:"since,omitempty"`
}

func Get(ctx context.Context, db *sql.DB) (Status, error) {
	var s Status
	err := db.QueryRowContext(ctx, `SELECT value, updated_at FROM system_state WHERE key = ?`, haltedKey).Scan(&s.Reason, &s.Since)
	if errors.Is(err, sql.ErrNoRows) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	s.Halted = true
	return s, nil
}

func Halt(ctx context.Context, db *sql.DB, now time.Time, actor, reason string) error {
	if reason == "" {
		reason = "emergency stop"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		haltedKey, reason, now.UnixMilli()); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, "", actor, "system.halted", "", map[string]string{"reason": reason}); err != nil {
		return err
	}
	return tx.Commit()
}

func Resume(ctx context.Context, db *sql.DB, now time.Time, actor string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM system_state WHERE key = ?`, haltedKey); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, "", actor, "system.resumed", "", nil); err != nil {
		return err
	}
	return tx.Commit()
}

const aliveKey = "serve.alive"

// Liveness is what a running `agentd serve` records about itself, so other processes
// can tell whether anything will act on what they write.
type Liveness struct {
	PID    int       `json:"pid"`
	Listen string    `json:"listen,omitempty"`
	At     time.Time `json:"at"`
}

// StaleAfter is how long a liveness record counts after it was written.
const StaleAfter = 20 * time.Second

func Beat(ctx context.Context, db *sql.DB, l Liveness) error {
	body, err := json.Marshal(l)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		aliveKey, string(body), l.At.UnixMilli())
	return err
}

// Stopped removes the liveness record when serve exits cleanly.
func Stopped(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `DELETE FROM system_state WHERE key = ?`, aliveKey)
	return err
}

// Alive returns the last liveness record and whether it is recent.
func Alive(ctx context.Context, db *sql.DB, now time.Time) (Liveness, bool, error) {
	var l Liveness
	var body string
	err := db.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, aliveKey).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false, nil
	}
	if err != nil {
		return l, false, err
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		return l, false, err
	}
	return l, now.Sub(l.At) < StaleAfter, nil
}
