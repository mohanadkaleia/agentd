package mcp

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentd/internal/audit"
)

const (
	StateConnecting  = "connecting"
	StateReady       = "ready"
	StateNeedsReview = "needs_review"
	StateNeedsAuth   = "needs_auth"
	StateFailed      = "failed"
	StateDisabled    = "disabled"
)

type ServerStatus struct {
	Server      string         `json:"server"`
	State       string         `json:"state"`
	Transport   string         `json:"transport"`
	LastError   string         `json:"last_error,omitempty"`
	NextRetryAt *time.Time     `json:"next_retry_at,omitempty"`
	Since       *time.Time     `json:"since,omitempty"`
	Scopes      string         `json:"scopes,omitempty"`
	Tools       map[string]int `json:"tools"`
}

// setState records a server's state; a change of state, other than to connecting, is audited.
func (st *store) setState(ctx context.Context, server, state, transport, lastError string, nextRetry time.Time) error {
	now := st.now()
	var retry any
	if !nextRetry.IsZero() {
		retry = nextRetry.UnixMilli()
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT state FROM mcp_servers WHERE server = ?`, server).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_servers (server, seen_at, state, transport, last_error, next_retry_at, state_since)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (server) DO UPDATE SET state = excluded.state, transport = excluded.transport, last_error = excluded.last_error,
			next_retry_at = excluded.next_retry_at,
			state_since = CASE WHEN mcp_servers.state = excluded.state THEN mcp_servers.state_since ELSE excluded.state_since END`,
		server, now.UnixMilli(), state, transport, lastError, retry, now.UnixMilli()); err != nil {
		return err
	}
	if state != previous && state != StateConnecting {
		if err := audit.Record(ctx, tx, now, "", "mcp", "mcp.server_"+state, server,
			map[string]string{"server": server, "error": lastError}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (st *store) takeRestart(ctx context.Context, server string) bool {
	res, err := st.db.ExecContext(ctx, `UPDATE mcp_servers SET restart_requested = 0 WHERE server = ? AND restart_requested = 1`, server)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// RequestRestart asks the running agentd to reconnect a server and forget its failures.
func RequestRestart(ctx context.Context, db *sql.DB, server string) error {
	res, err := db.ExecContext(ctx, `UPDATE mcp_servers SET restart_requested = 1 WHERE server = ?`, server)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func RecordScopes(ctx context.Context, db *sql.DB, now time.Time, server, scopes string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO mcp_servers (server, seen_at, scopes) VALUES (?, ?, ?)
		ON CONFLICT (server) DO UPDATE SET scopes = excluded.scopes`, server, now.UnixMilli(), scopes)
	return err
}

func ListServers(ctx context.Context, db *sql.DB) ([]ServerStatus, error) {
	rows, err := db.QueryContext(ctx, `SELECT server, state, transport, last_error, next_retry_at, state_since, scopes
		FROM mcp_servers ORDER BY server`)
	if err != nil {
		return nil, err
	}
	out := []ServerStatus{}
	for rows.Next() {
		var s ServerStatus
		var retry, since sql.NullInt64
		if err := rows.Scan(&s.Server, &s.State, &s.Transport, &s.LastError, &retry, &since, &s.Scopes); err != nil {
			rows.Close()
			return nil, err
		}
		if retry.Valid {
			t := time.UnixMilli(retry.Int64)
			s.NextRetryAt = &t
		}
		if since.Valid {
			t := time.UnixMilli(since.Int64)
			s.Since = &t
		}
		s.Tools = map[string]int{}
		out = append(out, s)
	}
	rows.Close()
	for i := range out {
		counts, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM mcp_tools WHERE server = ? GROUP BY status`, out[i].Server)
		if err != nil {
			return nil, err
		}
		for counts.Next() {
			var status string
			var n int
			if counts.Scan(&status, &n) == nil {
				out[i].Tools[status] = n
			}
		}
		counts.Close()
		if s := &out[i]; s.State == StateReady || s.State == StateNeedsReview {
			s.State = StateReady
			if s.Tools[StatusNew]+s.Tools[StatusChanged] > 0 {
				s.State = StateNeedsReview
			}
		}
	}
	return out, nil
}
