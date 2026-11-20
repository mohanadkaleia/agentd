package notify

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
	"agentd/internal/policy"
	"agentd/internal/tools"
)

const (
	Pending    = "pending"
	Delivered  = "delivered"
	Suppressed = "suppressed"
)

var projectDefaults = assignments.NotificationPolicy{
	MinSeverity: assignments.SeverityInfo,
	DedupWindow: policy.Duration{Duration: 24 * time.Hour},
	MaxPerDay:   20,
}

type Notification struct {
	AssignmentID string
	TaskID       string
	DedupKey     string
	Severity     assignments.Severity
	Title        string
	Body         string
}

type Result struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type Outbox struct {
	DB          *sql.DB
	Assignments *assignments.Store
	Now         func() time.Time
}

func (o *Outbox) Notify(ctx context.Context, n Notification) (Result, error) {
	a, err := o.Assignments.Get(ctx, n.AssignmentID)
	if err != nil {
		return Result{}, err
	}
	rules := projectDefaults
	if a.Notifications != nil {
		rules = *a.Notifications
	}
	now := o.Now()
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()

	var existing Result
	err = tx.QueryRowContext(ctx, `SELECT id, status, COALESCE(suppressed_reason, '') FROM notifications
		WHERE assignment_id = ? AND dedup_key = ? AND created_at > ? ORDER BY created_at DESC LIMIT 1`,
		n.AssignmentID, n.DedupKey, now.Add(-rules.DedupWindow.Duration).UnixMilli()).
		Scan(&existing.ID, &existing.Status, &existing.Reason)
	if err == nil {
		existing.Duplicate = true
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}

	res := Result{ID: ids.New("ntf"), Status: Pending}
	if n.Severity.Rank() < rules.MinSeverity.Rank() {
		res.Status, res.Reason = Suppressed, fmt.Sprintf("below minimum severity %s", rules.MinSeverity)
	} else {
		var today int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications
			WHERE assignment_id = ? AND status != 'suppressed' AND created_at > ?`,
			n.AssignmentID, now.Add(-24*time.Hour).UnixMilli()).Scan(&today); err != nil {
			return Result{}, err
		}
		if today >= rules.MaxPerDay {
			res.Status, res.Reason = Suppressed, fmt.Sprintf("daily limit of %d notifications reached", rules.MaxPerDay)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notifications (id, assignment_id, task_id, dedup_key, severity, title, body,
		status, suppressed_reason, created_at) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
		res.ID, n.AssignmentID, n.TaskID, n.DedupKey, n.Severity, n.Title, n.Body, res.Status, res.Reason, now.UnixMilli()); err != nil {
		return Result{}, err
	}
	if err := audit.Record(ctx, tx, now, n.AssignmentID, "agent", "notification."+res.Status, res.ID,
		map[string]string{"dedup_key": n.DedupKey, "severity": string(n.Severity)}); err != nil {
		return Result{}, err
	}
	return res, tx.Commit()
}

func (o *Outbox) Find(ctx context.Context, assignmentID, dedupKey string) (Result, bool, error) {
	var r Result
	err := o.DB.QueryRowContext(ctx, `SELECT id, status, COALESCE(suppressed_reason, '') FROM notifications
		WHERE assignment_id = ? AND dedup_key = ? ORDER BY created_at DESC LIMIT 1`, assignmentID, dedupKey).
		Scan(&r.ID, &r.Status, &r.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

type Tool struct {
	Outbox *Outbox
}

type toolArgs struct {
	Severity assignments.Severity `json:"severity"`
	Title    string               `json:"title"`
	Body     string               `json:"body"`
	DedupKey string               `json:"dedup_key"`
}

func (*Tool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name: "notify",
		Description: "Tell the user about something that warrants their attention. Use a stable dedup_key per underlying issue " +
			"(e.g. the thing and the date it happened) so the same issue is never reported twice.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"severity":{"type":"string","enum":["info","attention","action"]},
			"title":{"type":"string"},
			"body":{"type":"string"},
			"dedup_key":{"type":"string"}},
			"required":["severity","title","body","dedup_key"]}`),
	}
}

func (*Tool) Effect() tools.Effect { return tools.EffectWrite }

func (*Tool) Resource(args json.RawMessage) (string, error) {
	in, err := parse(args)
	return string(in.Severity), err
}

func parse(args json.RawMessage) (toolArgs, error) {
	var in toolArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return in, err
	}
	if in.Severity.Rank() == 0 {
		return in, fmt.Errorf("severity must be info, attention or action")
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.DedupKey) == "" {
		return in, fmt.Errorf("title and dedup_key are required")
	}
	return in, nil
}

func (t *Tool) Execute(ctx context.Context, call tools.Invocation) (tools.Result, error) {
	in, err := parse(call.Arguments)
	if err != nil {
		return tools.Result{}, fmt.Errorf("%w: %v", tools.ErrNotExecuted, err)
	}
	res, err := t.Outbox.Notify(ctx, Notification{
		AssignmentID: call.AssignmentID, TaskID: call.TaskID, DedupKey: in.DedupKey,
		Severity: in.Severity, Title: in.Title, Body: in.Body,
	})
	if err != nil {
		return tools.Result{}, err
	}
	out, _ := json.Marshal(res)
	return tools.Result{Output: out, ExternalID: res.ID}, nil
}

func (t *Tool) Reconcile(ctx context.Context, call tools.Invocation) (tools.Result, bool, error) {
	in, err := parse(call.Arguments)
	if err != nil {
		return tools.Result{}, false, nil
	}
	res, found, err := t.Outbox.Find(ctx, call.AssignmentID, in.DedupKey)
	if err != nil || !found {
		return tools.Result{}, false, err
	}
	out, _ := json.Marshal(res)
	return tools.Result{Output: out, ExternalID: res.ID}, true, nil
}
