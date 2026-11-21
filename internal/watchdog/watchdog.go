package watchdog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"agentd/internal/assignments"
	"agentd/internal/audit"
	"agentd/internal/ids"
	"agentd/internal/notify"
	"agentd/internal/system"
	"agentd/internal/tasks"
)

const (
	OnMissTask   = "task"
	OnMissNotify = "notify"
	originPrefix = "watchdog:"
)

type Evidence struct {
	Delivered string       `json:"delivered,omitempty"`
	Tool      string       `json:"tool,omitempty"`
	Event     *EventSource `json:"event,omitempty"`
}

type EventSource struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
}

func (e Evidence) validate() error {
	set := 0
	if e.Delivered != "" {
		set++
	}
	if e.Tool != "" {
		set++
	}
	if e.Event != nil {
		if e.Event.Source == "" || e.Event.Kind == "" {
			return fmt.Errorf("%w: event evidence needs a source and kind", assignments.ErrInvalid)
		}
		set++
	}
	if set != 1 {
		return fmt.Errorf("%w: an obligation needs exactly one kind of evidence: delivered, tool or event", assignments.ErrInvalid)
	}
	return nil
}

func (e Evidence) String() string {
	switch {
	case e.Delivered != "":
		return fmt.Sprintf("a delivered notification keyed %q…", e.Delivered)
	case e.Tool != "":
		return fmt.Sprintf("a successful %s", e.Tool)
	case e.Event != nil:
		return fmt.Sprintf("a %s/%s event", e.Event.Source, e.Event.Kind)
	}
	return "evidence"
}

type Obligation struct {
	ID           string
	AssignmentID string
	Description  string
	Every        time.Duration
	Grace        time.Duration
	Evidence     Evidence
	OnMiss       string
	Skills       []string
	CreatedAt    time.Time
}

type Health struct {
	Obligation
	LastEvidence time.Time
	DueAt        time.Time
	Overdue      bool
	Missed       int
}

type Watchdog struct {
	DB            *sql.DB
	Tasks         *tasks.Store
	Outbox        *notify.Outbox
	Now           func() time.Time
	EscalateAfter int
}

func (w *Watchdog) Add(ctx context.Context, o Obligation) (string, error) {
	if o.Description == "" || o.Every <= 0 || o.Grace < 0 {
		return "", fmt.Errorf("%w: an obligation needs a description and a positive interval", assignments.ErrInvalid)
	}
	if err := o.Evidence.validate(); err != nil {
		return "", err
	}
	if o.OnMiss == "" {
		o.OnMiss = OnMissTask
		if o.Evidence.Event != nil {
			o.OnMiss = OnMissNotify
		}
	}
	if o.OnMiss != OnMissTask && o.OnMiss != OnMissNotify {
		return "", fmt.Errorf("%w: on_miss must be task or notify", assignments.ErrInvalid)
	}
	evidence, _ := json.Marshal(o.Evidence)
	var skills any
	if len(o.Skills) > 0 {
		b, _ := json.Marshal(o.Skills)
		skills = string(b)
	}
	id := ids.New("obl")
	now := w.Now()
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO obligations (id, assignment_id, description, every_ms, grace_ms, evidence_json, on_miss, skills_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, o.AssignmentID, o.Description, o.Every.Milliseconds(), o.Grace.Milliseconds(),
		string(evidence), o.OnMiss, skills, now.UnixMilli()); err != nil {
		return "", err
	}
	if err := audit.Record(ctx, tx, now, o.AssignmentID, "user", "obligation.created", id, map[string]string{"description": o.Description}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func Status(ctx context.Context, db *sql.DB, assignmentID string, now time.Time) ([]Health, error) {
	obligations, err := load(ctx, db, `assignment_id = ?`, assignmentID)
	if err != nil {
		return nil, err
	}
	out := make([]Health, 0, len(obligations))
	for _, o := range obligations {
		h, err := health(ctx, db, o, now)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

func (w *Watchdog) Tick(ctx context.Context) error {
	if s, err := system.Get(ctx, w.DB); err != nil || s.Halted {
		return err
	}
	obligations, err := load(ctx, w.DB, `assignment_id IN (SELECT id FROM assignments WHERE status = 'active')`)
	if err != nil {
		return err
	}
	now := w.Now()
	for _, o := range obligations {
		h, err := health(ctx, w.DB, o, now)
		if err != nil {
			return err
		}
		if !h.Overdue {
			continue
		}
		if err := w.remediate(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watchdog) remediate(ctx context.Context, h Health) error {
	since := "it was set up"
	if !h.LastEvidence.IsZero() {
		since = h.LastEvidence.UTC().Format(time.RFC3339)
	}
	streak := fmt.Sprintf("%s%s:%d", originPrefix, h.ID, h.DueAt.UnixMilli())
	if h.Missed >= w.EscalateAfter {
		if _, err := w.Outbox.Notify(ctx, notify.Notification{AssignmentID: h.AssignmentID, DedupKey: streak + ":escalated",
			Severity: assignments.SeverityAction, Title: "Repeatedly missed: " + h.Description,
			Body: fmt.Sprintf("%d intervals have passed without %s (last seen: %s). It needs your help.", h.Missed, h.Evidence, since)}); err != nil {
			return err
		}
	}
	key := fmt.Sprintf("%s:%d", streak, h.Missed)
	if h.OnMiss == OnMissNotify {
		_, err := w.Outbox.Notify(ctx, notify.Notification{AssignmentID: h.AssignmentID, DedupKey: key,
			Severity: assignments.SeverityAttention, Title: "Overdue: " + h.Description,
			Body: fmt.Sprintf("Expected %s every %s; none since %s.", h.Evidence, h.Every, since)})
		return err
	}
	var open int
	if err := w.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE assignment_id = ? AND substr(origin_key, 1, ?) = ?
		AND status NOT IN ('completed', 'cancelled')`, h.AssignmentID, len(originPrefix+h.ID+":"), originPrefix+h.ID+":").Scan(&open); err != nil {
		return err
	}
	if open > 0 {
		return nil
	}
	_, err := w.Tasks.Create(ctx, tasks.NewTask{
		AssignmentID: h.AssignmentID, OriginKey: key, Skills: h.Skills,
		Title: "Overdue: " + h.Description,
		Objective: fmt.Sprintf("This obligation is overdue: %s. It is satisfied by %s, expected every %s; there has been none since %s. "+
			"Do what is needed to satisfy it now. If you cannot, notify the user why.", h.Description, h.Evidence, h.Every, since),
	})
	return err
}

func health(ctx context.Context, db *sql.DB, o Obligation, now time.Time) (Health, error) {
	h := Health{Obligation: o}
	var last sql.NullInt64
	var err error
	switch e := o.Evidence; {
	case e.Delivered != "":
		err = db.QueryRowContext(ctx, `SELECT MAX(delivered_at) FROM notifications WHERE assignment_id = ? AND status = 'delivered'
			AND substr(dedup_key, 1, ?) = ?`, o.AssignmentID, len(e.Delivered), e.Delivered).Scan(&last)
	case e.Tool != "":
		err = db.QueryRowContext(ctx, `SELECT MAX(updated_at) FROM actions WHERE assignment_id = ? AND tool_name = ? AND status = 'succeeded'`,
			o.AssignmentID, e.Tool).Scan(&last)
	case e.Event != nil:
		err = db.QueryRowContext(ctx, `SELECT MAX(received_at) FROM events WHERE source = ? AND kind = ?`, e.Event.Source, e.Event.Kind).Scan(&last)
	}
	if err != nil {
		return h, err
	}
	base := o.CreatedAt
	if last.Valid {
		h.LastEvidence = time.UnixMilli(last.Int64)
		if h.LastEvidence.After(base) {
			base = h.LastEvidence
		}
	}
	h.DueAt = base.Add(o.Every)
	if late := now.Sub(h.DueAt.Add(o.Grace)); late > 0 {
		h.Overdue, h.Missed = true, int(late/o.Every)+1
	}
	return h, nil
}

func load(ctx context.Context, db *sql.DB, where string, args ...any) ([]Obligation, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, assignment_id, description, every_ms, grace_ms, evidence_json, on_miss,
		COALESCE(skills_json, ''), created_at FROM obligations WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Obligation
	for rows.Next() {
		var o Obligation
		var every, grace, created int64
		var evidence, skills string
		if err := rows.Scan(&o.ID, &o.AssignmentID, &o.Description, &every, &grace, &evidence, &o.OnMiss, &skills, &created); err != nil {
			return nil, err
		}
		o.Every, o.Grace, o.CreatedAt = time.Duration(every)*time.Millisecond, time.Duration(grace)*time.Millisecond, time.UnixMilli(created)
		if err := json.Unmarshal([]byte(evidence), &o.Evidence); err != nil {
			return nil, err
		}
		if skills != "" {
			if err := json.Unmarshal([]byte(skills), &o.Skills); err != nil {
				return nil, err
			}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
