package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type Entry struct {
	ID           int64           `json:"id"`
	At           time.Time       `json:"at"`
	AssignmentID string          `json:"assignment_id,omitempty"`
	Actor        string          `json:"actor"`
	Operation    string          `json:"operation"`
	ObjectID     string          `json:"object_id,omitempty"`
	Payload      json.RawMessage `json:"payload"`
}

type Filter struct {
	AfterID      int64
	AssignmentID string
	Since        time.Time
	Limit        int
}

// List returns audit entries oldest first.
func List(ctx context.Context, db *sql.DB, f Filter) ([]Entry, error) {
	q := `SELECT id, created_at, COALESCE(assignment_id, ''), actor, operation, COALESCE(object_id, ''), payload_json
		FROM audit_log WHERE id > ?`
	args := []any{f.AfterID}
	if f.AssignmentID != "" {
		q, args = q+" AND assignment_id = ?", append(args, f.AssignmentID)
	}
	if !f.Since.IsZero() {
		q, args = q+" AND created_at >= ?", append(args, f.Since.UnixMilli())
	}
	q, args = q+" ORDER BY id LIMIT ?", append(args, f.Limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var at int64
		var payload string
		if err := rows.Scan(&e.ID, &at, &e.AssignmentID, &e.Actor, &e.Operation, &e.ObjectID, &payload); err != nil {
			return nil, err
		}
		e.At, e.Payload = time.UnixMilli(at), json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func Record(ctx context.Context, ex Execer, now time.Time, assignmentID, actor, operation, objectID string, payload any) error {
	if payload == nil {
		payload = struct{}{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = ex.ExecContext(ctx, `INSERT INTO audit_log (assignment_id, actor, operation, object_id, payload_json, created_at)
		VALUES (NULLIF(?, ''), ?, ?, NULLIF(?, ''), ?, ?)`,
		assignmentID, actor, operation, objectID, string(body), now.UnixMilli())
	return err
}
