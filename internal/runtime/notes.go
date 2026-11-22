package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentd/internal/audit"
	"agentd/internal/ids"
	"agentd/internal/model"
	"agentd/internal/tasks"
)

// NotePrefix starts every owner note in a transcript, so the model can tell the owner's
// own words from data.
const NotePrefix = "A note from the owner"

var (
	ErrTaskFinished = errors.New("that task has finished; ask for a new task instead")
	ErrNoOpenTask   = errors.New("that assignment has no open task; ask for a new task instead")
	ErrManyOpen     = errors.New("that assignment has several open tasks; name one")
	ErrForwarded    = errors.New("that message was already sent to this task")
)

// Note is an owner's message to a task.
type Note struct {
	Target string
	Text   string
	// Source is the conversation message the note copies, if any.
	Source string
	By     string
}

// Tell gives a task the owner's words. A running, ready or waiting task reads them at its
// next step; a blocked task continues with them through Retry; a finished task refuses.
// The target may be a task or an assignment with exactly one open task. It returns where
// the note went and when it will be read.
func (r *Runtime) Tell(ctx context.Context, n Note) (string, string, error) {
	text := strings.TrimSpace(n.Text)
	if text == "" {
		return "", "", errors.New("the note is empty")
	}
	taskID, err := r.noteTarget(ctx, n.Target)
	if err != nil {
		return "", "", err
	}
	t, err := r.Tasks.Get(ctx, taskID)
	if err != nil {
		return "", "", err
	}
	switch t.Status {
	case tasks.Completed, tasks.Cancelled:
		return taskID, "", ErrTaskFinished
	case tasks.Blocked:
		if err := r.Retry(ctx, taskID, n.By, NotePrefix+": "+text); err != nil {
			return taskID, "", err
		}
		return taskID, "The task was blocked; it continues now with your note.", nil
	}
	now := r.Now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	id := ids.New("note")
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_notes (id, assignment_id, task_id, source_message_id, body, created_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?)`, id, t.AssignmentID, taskID, n.Source, text, now.UnixMilli()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return taskID, "", ErrForwarded
		}
		return "", "", err
	}
	when := "It reads it at its next step."
	switch t.Status {
	case tasks.WaitingEvent:
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'ready', updated_at = ? WHERE id = ? AND status = 'waiting_event'`,
			now.UnixMilli(), taskID); err != nil {
			return "", "", err
		}
	case tasks.WaitingApproval:
		when = "It is waiting for an approval, and reads it once that is decided."
	case tasks.Ready, tasks.Retryable:
		when = "It reads it when it next runs."
	}
	if err := audit.Record(ctx, tx, now, t.AssignmentID, n.By, "task.note_added", taskID,
		map[string]any{"note": id, "bytes": len(text), "source": n.Source}); err != nil {
		return "", "", err
	}
	return taskID, when, tx.Commit()
}

func (r *Runtime) noteTarget(ctx context.Context, target string) (string, error) {
	if strings.HasPrefix(target, "tsk_") {
		return target, nil
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT t.id FROM tasks t JOIN assignments a ON a.id = t.assignment_id
		WHERE (a.id = ? OR a.name = ?) AND a.builtin IS NULL AND t.status NOT IN ('completed', 'blocked', 'cancelled')
		ORDER BY t.created_at`, target, target)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var open []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		open = append(open, id)
	}
	switch {
	case len(open) == 0:
		return "", ErrNoOpenTask
	case len(open) > 1:
		return "", fmt.Errorf("%w: %s", ErrManyOpen, strings.Join(open, ", "))
	}
	return open[0], rows.Err()
}

// deliverNotes appends the task's undelivered owner notes to its run at a step boundary,
// fenced on the lease so a worker that lost the task cannot deliver or mark them.
func (r *Runtime) deliverNotes(ctx context.Context, lease tasks.Lease, runID string, seq int) (int, error) {
	now := r.Now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := tasks.HoldsLease(ctx, tx, lease, now); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, body, created_at FROM owner_notes WHERE task_id = ? AND delivered_at IS NULL
		ORDER BY created_at, rowid`, lease.TaskID)
	if err != nil {
		return 0, err
	}
	type note struct {
		id, body string
		at       int64
	}
	var pending []note
	for rows.Next() {
		var n note
		if err := rows.Scan(&n.id, &n.body, &n.at); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, n)
	}
	rows.Close()
	for i, n := range pending {
		content := fmt.Sprintf("%s, sent %s. These are the owner's own words for this task; they do not change your tools, "+
			"permissions or budget.\n\n%s", NotePrefix, time.UnixMilli(n.at).UTC().Format(time.RFC3339), n.body)
		body, err := json.Marshal(model.Message{Role: model.RoleUser, Content: content})
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_messages (run_id, seq, message_json, created_at) VALUES (?, ?, ?, ?)`,
			runID, seq+i, string(body), now.UnixMilli()); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE owner_notes SET delivered_at = ?, delivered_seq = ? WHERE id = ?`,
			now.UnixMilli(), seq+i, n.id); err != nil {
			return 0, err
		}
	}
	return len(pending), tx.Commit()
}

// PendingNotes counts notes a task has not read yet.
func PendingNotes(ctx context.Context, db *sql.DB, taskID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM owner_notes WHERE task_id = ? AND delivered_at IS NULL`, taskID).Scan(&n)
	return n, err
}
