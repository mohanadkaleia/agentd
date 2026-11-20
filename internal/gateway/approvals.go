package gateway

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"agentd/internal/audit"
	"agentd/internal/ids"
)

var (
	ErrApprovalNotPending = errors.New("approval is not pending")
	ErrApprovalExpired    = errors.New("approval has expired")
)

type Approval struct {
	ID            string
	ActionID      string
	ProjectID     string
	ToolName      string
	Arguments     string
	ArgumentsHash string
	Status        string
	ExpiresAt     time.Time
}

func (g *Gateway) checkApproval(ctx context.Context, a *Action) (Outcome, bool, error) {
	var ap Approval
	var expires int64
	err := g.DB.QueryRowContext(ctx, `SELECT id, arguments_hash, status, expires_at FROM approvals WHERE action_id = ?`, a.ID).
		Scan(&ap.ID, &ap.ArgumentsHash, &ap.Status, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return g.requestApproval(ctx, a)
	}
	if err != nil {
		return Outcome{}, false, err
	}
	switch ap.Status {
	case "approved":
		if ap.ArgumentsHash != a.ArgumentsHash {
			out, err := g.finish(ctx, a, a.Status, Denied, errorJSON("approval does not match action arguments"), "")
			return out, false, err
		}
		return Outcome{}, true, nil
	case "pending":
		if !g.Now().Before(time.UnixMilli(expires)) {
			if _, err := g.ExpireApprovals(ctx); err != nil {
				return Outcome{}, false, err
			}
			out, err := g.finish(ctx, a, a.Status, Denied, errorJSON("approval expired"), "")
			return out, false, err
		}
		return Outcome{Kind: WaitApproval}, false, nil
	case "rejected":
		out, err := g.finish(ctx, a, a.Status, Denied, errorJSON("rejected by user"), "")
		return out, false, err
	default:
		out, err := g.finish(ctx, a, a.Status, Denied, errorJSON("approval expired"), "")
		return out, false, err
	}
}

func (g *Gateway) requestApproval(ctx context.Context, a *Action) (Outcome, bool, error) {
	now := g.Now()
	id := ids.New("apr")
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return Outcome{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO approvals (id, action_id, arguments_hash, status, requested_at, expires_at)
		VALUES (?, ?, ?, 'pending', ?, ?)`, id, a.ID, a.ArgumentsHash, now.UnixMilli(), now.Add(g.ApprovalTTL).UnixMilli()); err != nil {
		return Outcome{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'pending_approval', approval_id = ?, updated_at = ?
		WHERE id = ? AND status = ?`, id, now.UnixMilli(), a.ID, a.Status)
	if err != nil {
		return Outcome{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Outcome{}, false, ErrConflict
	}
	if err := audit.Record(ctx, tx, now, a.ProjectID, "gateway", "approval.requested", id, map[string]string{"action_id": a.ID}); err != nil {
		return Outcome{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Outcome{}, false, err
	}
	a.Status = PendingApproval
	return Outcome{Kind: WaitApproval}, false, nil
}

func (g *Gateway) Decide(ctx context.Context, approvalID string, approve bool, by string) error {
	now := g.Now()
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, actionID, projectID string
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT ap.status, ap.action_id, ap.expires_at, a.project_id
		FROM approvals ap JOIN actions a ON a.id = ap.action_id WHERE ap.id = ?`, approvalID).
		Scan(&status, &actionID, &expires, &projectID)
	if err != nil {
		return err
	}
	if status != "pending" {
		return ErrApprovalNotPending
	}
	decision, result := "rejected", error(nil)
	if approve {
		decision = "approved"
	}
	if !now.Before(time.UnixMilli(expires)) {
		decision, result = "expired", ErrApprovalExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_at = ?, decided_by = ? WHERE id = ?`,
		decision, now.UnixMilli(), by, approvalID); err != nil {
		return err
	}
	if err := wakeTask(ctx, tx, now, actionID); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, projectID, by, "approval."+decision, approvalID, map[string]string{"action_id": actionID}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return result
}

func (g *Gateway) ExpireApprovals(ctx context.Context) (int, error) {
	now := g.Now()
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `UPDATE approvals SET status = 'expired', decided_at = ?, decided_by = 'system'
		WHERE status = 'pending' AND expires_at <= ? RETURNING id, action_id`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return 0, err
	}
	var expired [][2]string
	for rows.Next() {
		var id, action string
		if err := rows.Scan(&id, &action); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, [2]string{id, action})
	}
	rows.Close()
	for _, e := range expired {
		if err := wakeTask(ctx, tx, now, e[1]); err != nil {
			return 0, err
		}
		if err := audit.Record(ctx, tx, now, "", "system", "approval.expired", e[0], map[string]string{"action_id": e[1]}); err != nil {
			return 0, err
		}
	}
	return len(expired), tx.Commit()
}

func (g *Gateway) PendingApprovals(ctx context.Context) ([]Approval, error) {
	rows, err := g.DB.QueryContext(ctx, `SELECT ap.id, ap.action_id, a.project_id, a.tool_name, a.arguments_json, ap.arguments_hash, ap.status, ap.expires_at
		FROM approvals ap JOIN actions a ON a.id = ap.action_id
		WHERE ap.status = 'pending' ORDER BY ap.requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var ap Approval
		var expires int64
		if err := rows.Scan(&ap.ID, &ap.ActionID, &ap.ProjectID, &ap.ToolName, &ap.Arguments, &ap.ArgumentsHash, &ap.Status, &expires); err != nil {
			return nil, err
		}
		ap.ExpiresAt = time.UnixMilli(expires)
		out = append(out, ap)
	}
	return out, rows.Err()
}

func wakeTask(ctx context.Context, tx *sql.Tx, now time.Time, actionID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'ready', next_run_at = NULL, updated_at = ?
		WHERE id = (SELECT task_id FROM actions WHERE id = ?) AND status = 'waiting_approval'`, now.UnixMilli(), actionID)
	return err
}
