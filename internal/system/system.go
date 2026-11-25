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
	PID    int    `json:"pid"`
	Listen string `json:"listen,omitempty"`
	// TokenFile is where the API token is kept (never the token), so this user's other
	// commands can call the API. Empty when the token came from the environment.
	TokenFile string    `json:"token_file,omitempty"`
	At        time.Time `json:"at"`
	// MCPConfig is the MCP config file serve watches; it restarts itself when the file changes.
	MCPConfig string `json:"mcp_config,omitempty"`
	// Models and Tiers are serve's configured models, so other commands can check a choice.
	Models []ModelInfo       `json:"models,omitempty"`
	Tiers  map[string]string `json:"tiers,omitempty"`
	// Exe and Build identify the executable serve runs, so a newer build on disk shows.
	Exe     string    `json:"exe,omitempty"`
	Build   string    `json:"build,omitempty"`
	Started time.Time `json:"started,omitempty"`
}

type ModelInfo struct {
	ID    string `json:"id"`
	Alias string `json:"alias,omitempty"`
	Local bool   `json:"local"`
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

const launchKey = "serve.launch"

// Launch is how `agentd serve` was last started, so the chat can start it the same way.
// It holds command-line arguments only; secrets are never passed as arguments.
type Launch struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
}

// Launched records how serve was started. Unlike the liveness record it outlives serve.
func Launched(ctx context.Context, db *sql.DB, l Launch, now time.Time) error {
	body, err := json.Marshal(l)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO system_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		launchKey, string(body), now.UnixMilli())
	return err
}

// LastLaunch returns how serve was last started, if it ever was.
func LastLaunch(ctx context.Context, db *sql.DB) (Launch, bool, error) {
	var l Launch
	var body string
	err := db.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, launchKey).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false, nil
	}
	if err != nil {
		return l, false, err
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		return l, false, err
	}
	return l, len(l.Args) > 0, nil
}
