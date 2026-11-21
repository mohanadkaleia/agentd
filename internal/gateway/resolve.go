package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agentd/internal/audit"
)

var ErrNotUnresolved = errors.New("action does not need reconciliation")

type Unresolved struct {
	ID           string
	TaskID       string
	AssignmentID string
	ToolName     string
	Resource     string
	Arguments    string
	Detail       string
}

func (g *Gateway) Resolve(ctx context.Context, actionID string, happened bool, by, note string) error {
	now := g.Now()
	outcome, result := Failed, errorJSON("a person confirmed this action did not take effect: "+note)
	if happened {
		outcome, result = Succeeded, mustJSON(map[string]string{"status": "a person confirmed this action took effect", "note": note})
	}
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var assignmentID string
	err = tx.QueryRowContext(ctx, `UPDATE actions SET status = ?, result_json = ?, updated_at = ?
		WHERE id = ? AND status = 'needs_reconciliation' RETURNING assignment_id`,
		outcome, string(result), now.UnixMilli(), actionID).Scan(&assignmentID)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions WHERE id = ?`, actionID).Scan(&exists)
		if exists == 0 {
			return sql.ErrNoRows
		}
		return ErrNotUnresolved
	}
	if err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, assignmentID, by, "action.resolved_"+outcome, actionID, map[string]string{"note": note}); err != nil {
		return err
	}
	return tx.Commit()
}

func (g *Gateway) UnresolvedActions(ctx context.Context, assignmentID string) ([]Unresolved, error) {
	q := `SELECT id, task_id, assignment_id, tool_name, resource, arguments_json, COALESCE(result_json, '')
		FROM actions WHERE status = 'needs_reconciliation'`
	var args []any
	if assignmentID != "" {
		q, args = q+` AND assignment_id = ?`, append(args, assignmentID)
	}
	rows, err := g.DB.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unresolved
	for rows.Next() {
		var u Unresolved
		if err := rows.Scan(&u.ID, &u.TaskID, &u.AssignmentID, &u.ToolName, &u.Resource, &u.Arguments, &u.Detail); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (u Unresolved) String() string {
	return fmt.Sprintf("%s %s", u.ToolName, u.Resource)
}
