// Package ask lets a task ask the owner a question and wait for the answer. The call stays
// open while the question is unanswered, the task waits without model calls, and the
// owner's answer becomes the tool result.
package ask

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/ids"
	"agentd/internal/model"
	"agentd/internal/notify"
	"agentd/internal/tools"
)

const (
	Name           = "ask_owner"
	Open           = "open"
	Answered       = "answered"
	Expired        = "expired"
	Withdrawn      = "withdrawn"
	defaultTimeout = 72 * time.Hour
	// MaxOpen is how many questions one assignment may have waiting at once.
	MaxOpen = 3
)

var (
	ErrNotOpen  = errors.New("that question is no longer open")
	ErrNotFound = errors.New("no such question")
)

type Question struct {
	ID           string     `json:"id"`
	ActionID     string     `json:"action_id"`
	AssignmentID string     `json:"assignment_id"`
	TaskID       string     `json:"task_id"`
	Question     string     `json:"question"`
	Choices      []string   `json:"choices,omitempty"`
	Context      string     `json:"context,omitempty"`
	Default      string     `json:"default,omitempty"`
	Status       string     `json:"status"`
	Answer       string     `json:"answer,omitempty"`
	AnsweredVia  string     `json:"answered_via,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	AnsweredAt   *time.Time `json:"answered_at,omitempty"`
}

type Tool struct {
	DB     *sql.DB
	Outbox *notify.Outbox
	Now    func() time.Time
}

type args struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
	Context  string   `json:"context"`
	Default  string   `json:"default"`
	Timeout  string   `json:"timeout"`
}

func (t *Tool) Definition() model.ToolDefinition {
	return model.ToolDefinition{Name: Name,
		Description: "Ask the owner a question and wait for the answer, which comes back as this tool's result. " +
			"Use it for a fact only the owner has, a choice with real consequences, or an unclear instruction. " +
			"Not to ask permission for a tool (approvals do that) and not for progress updates.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
"question":{"type":"string"},
"choices":{"type":"array","items":{"type":"string"},"description":"optional answers to pick from"},
"context":{"type":"string","description":"what the owner needs to know to answer"},
"default":{"type":"string","description":"used if no answer arrives in time"},
"timeout":{"type":"string","description":"how long to wait, like \"24h\"; default 72h"}},"required":["question"]}`)}
}

func (t *Tool) Effect() tools.Effect { return tools.EffectWrite }

func (t *Tool) Resource(raw json.RawMessage) (string, error) {
	var in args
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Question) == "" {
		return "", errors.New("question is required")
	}
	if in.Timeout != "" {
		if d, err := time.ParseDuration(in.Timeout); err != nil || d < time.Minute {
			return "", errors.New("timeout must be a duration of a minute or more, like \"24h\"")
		}
	}
	return "", nil
}

func (t *Tool) NoResource() bool { return true }

// DefaultRate limits how often one assignment may ask, unless its policy says otherwise.
func (t *Tool) DefaultRate() (int, time.Duration) { return 5, 24 * time.Hour }

// Execute asks once per action. While the question is open it reports that the call is
// not ready, which leaves the action open and the task waiting; once answered or expired,
// the answer is the result.
func (t *Tool) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	q, err := byAction(ctx, t.DB, call.ActionID)
	if errors.Is(err, sql.ErrNoRows) {
		return t.ask(ctx, call)
	}
	if err != nil {
		return tools.Result{}, err
	}
	return outcome(q)
}

func outcome(q Question) (tools.Result, error) {
	switch q.Status {
	case Open:
		return tools.Result{}, fmt.Errorf("an answer from you to %q (%s): %w", q.Question, q.ID, tools.ErrNotReady)
	case Answered:
		out, _ := json.Marshal(map[string]string{"answer": q.Answer,
			"note": "This is the owner's own answer to your question."})
		return tools.Result{Output: out, ExternalID: q.ID}, nil
	case Expired:
		out := map[string]any{"answer": q.Default, "expired": true,
			"note": fmt.Sprintf("No answer by %s.", q.ExpiresAt.UTC().Format(time.RFC3339))}
		if q.Default != "" {
			out["note"] = fmt.Sprintf("No answer by %s, so your default applies.", q.ExpiresAt.UTC().Format(time.RFC3339))
		}
		b, _ := json.Marshal(out)
		return tools.Result{Output: b, ExternalID: q.ID}, nil
	}
	return tools.Result{}, fmt.Errorf("%w: the question was withdrawn", tools.ErrNotExecuted)
}

func (t *Tool) ask(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	var in args
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	timeout := defaultTimeout
	if in.Timeout != "" {
		timeout, _ = time.ParseDuration(in.Timeout)
	}
	now := t.Now()
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return tools.Result{}, err
	}
	defer tx.Rollback()
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM questions WHERE assignment_id = ? AND status = 'open'`,
		call.AssignmentID).Scan(&open); err != nil {
		return tools.Result{}, err
	}
	if open >= MaxOpen {
		return tools.Result{}, fmt.Errorf("%w: %d questions to the owner are already waiting; wait for an answer first",
			tools.ErrNotExecuted, open)
	}
	choices, _ := json.Marshal(nonNil(in.Choices))
	q := Question{ID: ids.New("qst"), ActionID: call.ActionID, AssignmentID: call.AssignmentID, TaskID: call.TaskID,
		Question: strings.TrimSpace(in.Question), Choices: in.Choices, Context: in.Context, Default: in.Default, Status: Open,
		CreatedAt: now, ExpiresAt: now.Add(timeout)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO questions (id, action_id, assignment_id, task_id, question, choices_json, context,
		default_answer, status, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), 'open', ?, ?)`,
		q.ID, q.ActionID, q.AssignmentID, q.TaskID, q.Question, string(choices), q.Context, q.Default,
		now.UnixMilli(), q.ExpiresAt.UnixMilli()); err != nil {
		return tools.Result{}, err
	}
	if err := audit.Record(ctx, tx, now, q.AssignmentID, "agent", "question.asked", q.ID,
		map[string]string{"action_id": q.ActionID, "task_id": q.TaskID}); err != nil {
		return tools.Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return tools.Result{}, err
	}
	if t.Outbox != nil {
		if _, err := t.Outbox.Notify(ctx, notify.Notification{AssignmentID: q.AssignmentID, TaskID: q.TaskID, ActionID: q.ActionID,
			DedupKey: "question:" + q.ID, Severity: assignments.SeverityAction, Title: "Question: " + q.Question,
			Body: "Answer with: agentd answer " + q.ID + " \"...\""}); err != nil {
			return tools.Result{}, err
		}
	}
	return outcome(q)
}

// Reconcile answers from the questions table: an asked question that is still open is
// asked again, which finds it and waits.
func (t *Tool) Reconcile(ctx context.Context, call tools.Invocation) (tools.Result, tools.Finding, error) {
	q, err := byAction(ctx, t.DB, call.ActionID)
	if errors.Is(err, sql.ErrNoRows) {
		return tools.Result{}, tools.NotHappened, nil
	}
	if err != nil {
		return tools.Result{}, tools.Unknown, err
	}
	if q.Status == Open {
		return tools.Result{}, tools.NotHappened, nil
	}
	res, err := outcome(q)
	if err != nil {
		return tools.Result{}, tools.NotHappened, nil
	}
	return res, tools.Happened, nil
}

// Answer records the owner's answer once and wakes the task that asked.
func Answer(ctx context.Context, db *sql.DB, now time.Time, id, answer, via string) (Question, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return Question{}, errors.New("the answer is empty")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Question{}, err
	}
	defer tx.Rollback()
	q, err := byID(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return q, ErrNotFound
	}
	if err != nil {
		return q, err
	}
	if q.Status != Open {
		return q, fmt.Errorf("%w (%s)", ErrNotOpen, q.Status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE questions SET status = 'answered', answer = ?, answered_via = ?, answered_at = ?
		WHERE id = ? AND status = 'open'`, answer, via, now.UnixMilli(), id); err != nil {
		return q, err
	}
	if err := wake(ctx, tx, now, q.TaskID); err != nil {
		return q, err
	}
	if err := audit.Record(ctx, tx, now, q.AssignmentID, via, "question.answered", id, map[string]int{"bytes": len(answer)}); err != nil {
		return q, err
	}
	q.Status, q.Answer, q.AnsweredVia = Answered, answer, via
	return q, tx.Commit()
}

func wake(ctx context.Context, tx *sql.Tx, now time.Time, taskID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks SET wake_pending = 1,
		status = CASE WHEN status = 'waiting_event' THEN 'ready' ELSE status END, updated_at = ?
		WHERE id = ? AND status NOT IN ('completed', 'blocked', 'cancelled')`, now.UnixMilli(), taskID)
	return err
}

// Tick expires unanswered questions (the task continues with its default or with no
// answer), reminds once at half the timeout, and withdraws questions whose task ended.
func (t *Tool) Tick(ctx context.Context) error {
	now := t.Now()
	open, err := list(ctx, t.DB, `status = 'open'`)
	if err != nil {
		return err
	}
	for _, q := range open {
		var status string
		if err := t.DB.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, q.TaskID).Scan(&status); err != nil {
			return err
		}
		switch {
		case status == "completed" || status == "blocked" || status == "cancelled":
			if err := t.settle(ctx, q, Withdrawn, now); err != nil {
				return err
			}
		case !now.Before(q.ExpiresAt):
			if err := t.settle(ctx, q, Expired, now); err != nil {
				return err
			}
		case now.Sub(q.CreatedAt) >= q.ExpiresAt.Sub(q.CreatedAt)/2:
			if err := t.remind(ctx, q, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Tool) settle(ctx context.Context, q Question, status string, now time.Time) error {
	tx, err := t.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE questions SET status = ?, answered_at = ? WHERE id = ? AND status = 'open'`,
		status, now.UnixMilli(), q.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if status == Expired {
		if err := wake(ctx, tx, now, q.TaskID); err != nil {
			return err
		}
	}
	if err := audit.Record(ctx, tx, now, q.AssignmentID, "ask", "question."+status, q.ID, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *Tool) remind(ctx context.Context, q Question, now time.Time) error {
	res, err := t.DB.ExecContext(ctx, `UPDATE questions SET reminded_at = ? WHERE id = ? AND reminded_at IS NULL`, now.UnixMilli(), q.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 || t.Outbox == nil {
		return nil
	}
	_, err = t.Outbox.Notify(ctx, notify.Notification{AssignmentID: q.AssignmentID, TaskID: q.TaskID, DedupKey: "question-reminder:" + q.ID,
		Severity: assignments.SeverityAction, Title: "Still waiting for your answer: " + q.Question,
		Body: fmt.Sprintf("Without an answer by %s the task continues without one. agentd answer %s \"...\"",
			q.ExpiresAt.Local().Format("01-02 15:04"), q.ID)})
	return err
}

// OpenFor lists the open questions of one assignment, or of all when assignmentID is empty.
func OpenFor(ctx context.Context, db *sql.DB, assignmentID string) ([]Question, error) {
	if assignmentID == "" {
		return list(ctx, db, `status = 'open'`)
	}
	return list(ctx, db, `status = 'open' AND assignment_id = ?`, assignmentID)
}

func Get(ctx context.Context, db *sql.DB, id string) (Question, error) {
	q, err := byID(ctx, db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return q, ErrNotFound
	}
	return q, err
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const columns = `id, action_id, assignment_id, task_id, question, choices_json, context, COALESCE(default_answer, ''), status,
	COALESCE(answer, ''), COALESCE(answered_via, ''), created_at, expires_at, answered_at`

func scan(row interface{ Scan(...any) error }) (Question, error) {
	var q Question
	var choices string
	var created, expires int64
	var answered sql.NullInt64
	err := row.Scan(&q.ID, &q.ActionID, &q.AssignmentID, &q.TaskID, &q.Question, &choices, &q.Context, &q.Default, &q.Status,
		&q.Answer, &q.AnsweredVia, &created, &expires, &answered)
	json.Unmarshal([]byte(choices), &q.Choices)
	q.CreatedAt, q.ExpiresAt = time.UnixMilli(created), time.UnixMilli(expires)
	if answered.Valid {
		at := time.UnixMilli(answered.Int64)
		q.AnsweredAt = &at
	}
	return q, err
}

func byID(ctx context.Context, q querier, id string) (Question, error) {
	return scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM questions WHERE id = ?`, id))
}

func byAction(ctx context.Context, q querier, actionID string) (Question, error) {
	return scan(q.QueryRowContext(ctx, `SELECT `+columns+` FROM questions WHERE action_id = ?`, actionID))
}

func list(ctx context.Context, q querier, where string, args ...any) ([]Question, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+columns+` FROM questions WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		q, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
