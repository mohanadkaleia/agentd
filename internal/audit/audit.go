package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

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
