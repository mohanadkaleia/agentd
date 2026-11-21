package outcomes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/notify"
)

var ErrUnknownCriterion = errors.New("no such completion criterion")

type Progress struct {
	Criterion  assignments.Criterion
	Verified   bool
	Kind       string
	EventID    string
	Actor      string
	Note       string
	ObservedAt time.Time
}

func Criteria(ctx context.Context, db *sql.DB, a assignments.Assignment) ([]Progress, error) {
	if a.Completion == nil {
		return nil, nil
	}
	out := make([]Progress, 0, len(a.Completion.Criteria))
	for _, c := range a.Completion.Criteria {
		p := Progress{Criterion: c}
		var event, note sql.NullString
		var at int64
		err := db.QueryRowContext(ctx, `SELECT kind, event_id, actor, note, observed_at FROM criterion_evidence
			WHERE assignment_id = ? AND criterion_id = ?`, a.ID, c.ID).Scan(&p.Kind, &event, &p.Actor, &note, &at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		default:
			p.Verified, p.EventID, p.Note, p.ObservedAt = true, event.String, note.String, time.UnixMilli(at)
		}
		out = append(out, p)
	}
	return out, nil
}

type Evaluator struct {
	DB          *sql.DB
	Assignments *assignments.Store
	Outbox      *notify.Outbox
	Now         func() time.Time
}

func (e *Evaluator) Confirm(ctx context.Context, assignmentID, criterionID, actor, note string) error {
	a, err := e.Assignments.Get(ctx, assignmentID)
	if err != nil {
		return err
	}
	if a.Kind != assignments.KindProject {
		return assignments.ErrWrongKind
	}
	if a.Status != assignments.Active && a.Status != assignments.Paused {
		return assignments.ErrTerminated
	}
	if _, ok := find(a, criterionID); !ok {
		return ErrUnknownCriterion
	}
	return e.record(ctx, a, criterionID, "owner", "", actor, note)
}

func (e *Evaluator) Tick(ctx context.Context) error {
	rows, err := e.DB.QueryContext(ctx, `SELECT id, created_at FROM assignments
		WHERE kind = 'project' AND status = 'active' AND completion_json IS NOT NULL`)
	if err != nil {
		return err
	}
	type project struct {
		id      string
		created int64
	}
	var projects []project
	for rows.Next() {
		var p project
		if err := rows.Scan(&p.id, &p.created); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, p)
	}
	rows.Close()
	for _, p := range projects {
		a, err := e.Assignments.Get(ctx, p.id)
		if err != nil {
			return err
		}
		if err := e.evaluate(ctx, a, p.created); err != nil {
			return err
		}
	}
	return nil
}

func (e *Evaluator) evaluate(ctx context.Context, a assignments.Assignment, since int64) error {
	progress, err := Criteria(ctx, e.DB, a)
	if err != nil || len(progress) == 0 {
		return err
	}
	verified := 0
	for _, p := range progress {
		if !p.Verified && p.Criterion.Event != nil {
			eventID, err := e.matchingEvent(ctx, *p.Criterion.Event, since)
			if err != nil {
				return err
			}
			if eventID != "" {
				if err := e.record(ctx, a, p.Criterion.ID, "event", eventID, "evaluator", ""); err != nil {
					return err
				}
				p.Verified = true
			}
		}
		if p.Verified {
			verified++
		}
	}
	if verified < len(progress) {
		return nil
	}
	var unresolved int
	if err := e.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions
		WHERE assignment_id = ? AND status IN ('needs_reconciliation', 'dispatching')`, a.ID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		return nil
	}
	if err := e.Assignments.Complete(ctx, a.ID, "evaluator"); err != nil {
		if errors.Is(err, assignments.ErrTerminated) {
			return nil
		}
		return err
	}
	_, err = e.Outbox.Notify(ctx, notify.Notification{AssignmentID: a.ID, DedupKey: "project-completed",
		Severity: assignments.SeverityInfo, Title: "Project completed: " + a.Name,
		Body: fmt.Sprintf("All %d completion criteria are verified.", len(progress))})
	return err
}

func (e *Evaluator) matchingEvent(ctx context.Context, want assignments.EventEvidence, since int64) (string, error) {
	rows, err := e.DB.QueryContext(ctx, `SELECT id, payload_json FROM events
		WHERE source = ? AND kind = ? AND received_at >= ? ORDER BY received_at, id`, want.Source, want.Kind, since)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return "", err
		}
		if matches(payload, want.Match) {
			return id, nil
		}
	}
	return "", rows.Err()
}

func matches(payload string, want map[string]string) bool {
	var fields map[string]any
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return len(want) == 0
	}
	for k, v := range want {
		got, ok := fields[k]
		if !ok {
			return false
		}
		if s, isString := got.(string); isString {
			if s != v {
				return false
			}
		} else if fmt.Sprint(got) != v {
			return false
		}
	}
	return true
}

func (e *Evaluator) record(ctx context.Context, a assignments.Assignment, criterionID, kind, eventID, actor, note string) error {
	now := e.Now()
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO criterion_evidence (assignment_id, criterion_id, kind, event_id, actor, note, policy_version, observed_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?) ON CONFLICT (assignment_id, criterion_id) DO NOTHING`,
		a.ID, criterionID, kind, eventID, actor, note, a.PolicyVersion, now.UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err := audit.Record(ctx, tx, now, a.ID, actor, "criterion.verified", a.ID,
		map[string]string{"criterion": criterionID, "kind": kind, "event_id": eventID, "note": note}); err != nil {
		return err
	}
	return tx.Commit()
}

func find(a assignments.Assignment, id string) (assignments.Criterion, bool) {
	if a.Completion != nil {
		for _, c := range a.Completion.Criteria {
			if c.ID == id {
				return c, true
			}
		}
	}
	return assignments.Criterion{}, false
}
