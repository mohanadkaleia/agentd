package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agentd/internal/audit"
	"agentd/internal/model"
)

var (
	ErrNotBlocked     = errors.New("task is not blocked")
	ErrUnresolvedWork = errors.New("task has an action whose outcome is unknown; resolve it first")
)

const retryPrefix = "A person reviewed this task and asked you to continue."

func (r *Runtime) Retry(ctx context.Context, taskID, by, note string) error {
	now := r.Now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status, assignmentID string
	if err := tx.QueryRowContext(ctx, `SELECT status, assignment_id FROM tasks WHERE id = ?`, taskID).Scan(&status, &assignmentID); err != nil {
		return err
	}
	if status != "blocked" {
		return ErrNotBlocked
	}
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions WHERE task_id = ? AND status = 'needs_reconciliation'`, taskID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return ErrUnresolvedWork
	}

	var runID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE task_id = ? ORDER BY started_at DESC, rowid DESC LIMIT 1`, taskID).Scan(&runID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if runID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'running', step_count = 0, finished_at = NULL, error_json = NULL
			WHERE id = ?`, runID); err != nil {
			return err
		}
		if err := appendRetryNote(ctx, tx, runID, note, now.UnixMilli()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET coalesce_key = NULL
		WHERE id = ? AND coalesce_key IS NOT NULL AND EXISTS (
			SELECT 1 FROM tasks other WHERE other.id != tasks.id AND other.assignment_id = tasks.assignment_id
			  AND other.coalesce_key = tasks.coalesce_key AND other.status NOT IN ('completed', 'blocked', 'cancelled'))`,
		taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'ready', attempt_count = 0, next_run_at = NULL, deadline_at = NULL,
		result_json = NULL, version = version + 1, updated_at = ? WHERE id = ? AND status = 'blocked'`, now.UnixMilli(), taskID); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, assignmentID, by, "task.retried", taskID, map[string]string{"note": note}); err != nil {
		return err
	}
	return tx.Commit()
}

func appendRetryNote(ctx context.Context, tx *sql.Tx, runID, note string, at int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT message_json FROM run_messages WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return err
	}
	var msgs []model.Message
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return err
		}
		var m model.Message
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			rows.Close()
			return err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if _, pending := pendingCalls(msgs); len(pending) > 0 {
		return nil
	}
	content := retryPrefix
	if strings.TrimSpace(note) != "" {
		content += " Their note: " + note
	}
	body, err := json.Marshal(model.Message{Role: model.RoleUser, Content: content})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO run_messages (run_id, seq, message_json, created_at) VALUES (?, ?, ?, ?)`,
		runID, len(msgs), string(body), at)
	if err != nil {
		return fmt.Errorf("append retry note: %w", err)
	}
	return nil
}
