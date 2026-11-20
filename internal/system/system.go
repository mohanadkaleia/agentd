package system

import (
	"context"
	"database/sql"
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
