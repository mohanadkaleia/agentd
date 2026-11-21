package assignments

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
	"agentd/internal/policy"
)

type Kind string

const (
	KindProject        Kind = "project"
	KindResponsibility Kind = "responsibility"
)

const (
	Active    = "active"
	Paused    = "paused"
	Completed = "completed"
	Cancelled = "cancelled"
	Revoked   = "revoked"
)

const (
	PausedByUser   = "user"
	PausedByBudget = "budget"
)

var (
	ErrNotFound   = errors.New("assignment not found")
	ErrWrongKind  = errors.New("operation does not apply to this kind of assignment")
	ErrNotPaused  = errors.New("assignment is not paused")
	ErrTerminated = errors.New("assignment is not in a state that allows this")
	ErrInvalid    = errors.New("invalid assignment")
)

type Severity string

const (
	SeverityInfo      Severity = "info"
	SeverityAttention Severity = "attention"
	SeverityAction    Severity = "action"
)

func (s Severity) Rank() int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityAttention:
		return 2
	case SeverityAction:
		return 3
	}
	return 0
}

type Completion struct {
	Criteria []Criterion `json:"criteria,omitempty"`
}

type Criterion struct {
	ID          string         `json:"id"`
	Description string         `json:"description"`
	Event       *EventEvidence `json:"event,omitempty"`
}

type EventEvidence struct {
	Source string            `json:"source"`
	Kind   string            `json:"kind"`
	Match  map[string]string `json:"match,omitempty"`
}

func (c *Criterion) UnmarshalJSON(b []byte) error {
	var text string
	if json.Unmarshal(b, &text) == nil {
		*c = Criterion{Description: text}
		return nil
	}
	type plain Criterion
	return json.Unmarshal(b, (*plain)(c))
}

func (c *Completion) normalize() error {
	seen := map[string]bool{}
	for i := range c.Criteria {
		cr := &c.Criteria[i]
		if cr.ID == "" {
			cr.ID = fmt.Sprint(i + 1)
		}
		if seen[cr.ID] {
			return fmt.Errorf("%w: duplicate completion criterion id %q", ErrInvalid, cr.ID)
		}
		seen[cr.ID] = true
		if strings.TrimSpace(cr.Description) == "" {
			return fmt.Errorf("%w: completion criterion %q needs a description", ErrInvalid, cr.ID)
		}
		if cr.Event != nil && (cr.Event.Source == "" || cr.Event.Kind == "") {
			return fmt.Errorf("%w: completion criterion %q: event evidence needs a source and kind", ErrInvalid, cr.ID)
		}
	}
	return nil
}

type NotificationPolicy struct {
	MinSeverity Severity        `json:"min_severity"`
	DedupWindow policy.Duration `json:"dedup_window"`
	MaxPerDay   int             `json:"max_per_day"`
}

func (n NotificationPolicy) Validate() error {
	if n.MinSeverity.Rank() == 0 {
		return fmt.Errorf("%w: notification min_severity must be info, attention or action", ErrInvalid)
	}
	if n.DedupWindow.Duration <= 0 {
		return fmt.Errorf("%w: notification dedup_window must be positive", ErrInvalid)
	}
	if n.MaxPerDay <= 0 {
		return fmt.Errorf("%w: notification max_per_day must be positive", ErrInvalid)
	}
	return nil
}

type Assignment struct {
	ID            string
	Kind          Kind
	Name          string
	Objective     string
	Status        string
	StatusReason  string
	PausedBy      string
	PolicyVersion int64
	Policy        policy.Policy
	Completion    *Completion
	Notifications *NotificationPolicy
	State         json.RawMessage
}

type NewProject struct {
	Name       string
	Objective  string
	Policy     policy.Policy
	Completion Completion
}

type NewResponsibility struct {
	Name          string
	Objective     string
	Policy        policy.Policy
	Notifications NotificationPolicy
}

type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s *Store) CreateProject(ctx context.Context, p NewProject) (string, error) {
	if err := p.Completion.normalize(); err != nil {
		return "", err
	}
	completion, err := json.Marshal(p.Completion)
	if err != nil {
		return "", err
	}
	return s.create(ctx, KindProject, p.Name, p.Objective, p.Policy, string(completion), nil)
}

func (s *Store) CreateResponsibility(ctx context.Context, r NewResponsibility) (string, error) {
	if err := validateResponsibilityPolicy(r.Policy); err != nil {
		return "", err
	}
	if err := r.Notifications.Validate(); err != nil {
		return "", err
	}
	notifications, err := json.Marshal(r.Notifications)
	if err != nil {
		return "", err
	}
	return s.create(ctx, KindResponsibility, r.Name, r.Objective, r.Policy, nil, string(notifications))
}

func validateResponsibilityPolicy(p policy.Policy) error {
	if p.Limits.MaxLLMSpendUSD <= 0 || p.Limits.BudgetPeriod.Duration <= 0 {
		return fmt.Errorf("%w: a responsibility needs limits.max_llm_spend_usd and limits.budget_period", ErrInvalid)
	}
	return nil
}

func (s *Store) create(ctx context.Context, kind Kind, name, objective string, p policy.Policy, completion, notifications any) (string, error) {
	if err := p.Validate(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	id := ids.New(map[Kind]string{KindProject: "prj", KindResponsibility: "rsp"}[kind])
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO assignments (id, kind, name, objective, status, policy_json,
		completion_json, notification_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, ?)`,
		id, kind, name, objective, string(body), completion, notifications, now.UnixMilli(), now.UnixMilli()); err != nil {
		return "", err
	}
	if err := audit.Record(ctx, tx, now, id, "user", string(kind)+".created", id, map[string]string{"name": name}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) Get(ctx context.Context, id string) (Assignment, error) {
	var a Assignment
	var policyJSON, state string
	var reason, pausedBy, completion, notifications sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id, kind, name, objective, status, status_reason, paused_by,
		policy_version, policy_json, completion_json, notification_json, state_json
		FROM assignments WHERE id = ?`, id).
		Scan(&a.ID, &a.Kind, &a.Name, &a.Objective, &a.Status, &reason, &pausedBy,
			&a.PolicyVersion, &policyJSON, &completion, &notifications, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.StatusReason, a.PausedBy, a.State = reason.String, pausedBy.String, json.RawMessage(state)
	if completion.Valid {
		a.Completion = &Completion{}
		if err := json.Unmarshal([]byte(completion.String), a.Completion); err != nil {
			return a, err
		}
		if err := a.Completion.normalize(); err != nil {
			return a, err
		}
	}
	if notifications.Valid {
		a.Notifications = &NotificationPolicy{}
		if err := json.Unmarshal([]byte(notifications.String), a.Notifications); err != nil {
			return a, err
		}
	}
	return a, json.Unmarshal([]byte(policyJSON), &a.Policy)
}

func (s *Store) Pause(ctx context.Context, id, by, reason, actor string) error {
	guard := `status = 'active'`
	if by == PausedByUser {
		guard = `status IN ('active', 'paused')`
	}
	return s.transition(ctx, id, actor, "paused", guard,
		`status = 'paused', paused_by = ?, status_reason = NULLIF(?, '')`, by, reason)
}

func (s *Store) ResumeBudgetPause(ctx context.Context, id, actor string) error {
	return s.transition(ctx, id, actor, "resumed", `status = 'paused' AND paused_by = 'budget'`,
		`status = 'active', paused_by = NULL, status_reason = NULL`)
}

func (s *Store) Resume(ctx context.Context, id, actor string) error {
	err := s.transition(ctx, id, actor, "resumed", `status = 'paused'`,
		`status = 'active', paused_by = NULL, status_reason = NULL`)
	if errors.Is(err, ErrTerminated) {
		return ErrNotPaused
	}
	return err
}

func (s *Store) Complete(ctx context.Context, id, actor string) error {
	return s.end(ctx, id, actor, KindProject, Completed)
}

func (s *Store) Cancel(ctx context.Context, id, actor string) error {
	return s.end(ctx, id, actor, KindProject, Cancelled)
}

func (s *Store) Revoke(ctx context.Context, id, actor string) error {
	return s.end(ctx, id, actor, KindResponsibility, Revoked)
}

func (s *Store) end(ctx context.Context, id, actor string, kind Kind, status string) error {
	a, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if a.Kind != kind {
		return ErrWrongKind
	}
	return s.transition(ctx, id, actor, status, `status IN ('active', 'paused')`,
		`status = ?, paused_by = NULL, status_reason = NULL`, status)
}

func (s *Store) transition(ctx context.Context, id, actor, operation, guard, set string, args ...any) error {
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM assignments WHERE id = ?`, id).Scan(&kind); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE assignments SET `+set+`, updated_at = ? WHERE id = ? AND `+guard,
		append(args, now.UnixMilli(), id)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTerminated
	}
	if err := audit.Record(ctx, tx, now, id, actor, kind+"."+operation, id, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetPolicy(ctx context.Context, id string, p policy.Policy, actor string) (int64, error) {
	a, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if a.Kind == KindResponsibility {
		if err := validateResponsibilityPolicy(p); err != nil {
			return 0, err
		}
	}
	if err := p.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var version int64
	if err := tx.QueryRowContext(ctx, `UPDATE assignments SET policy_json = ?, policy_version = policy_version + 1, updated_at = ?
		WHERE id = ? RETURNING policy_version`, string(body), now.UnixMilli(), id).Scan(&version); err != nil {
		return 0, err
	}
	if err := audit.Record(ctx, tx, now, id, actor, string(a.Kind)+".policy_updated", id, map[string]int64{"policy_version": version}); err != nil {
		return 0, err
	}
	return version, tx.Commit()
}

func (s *Store) SetState(ctx context.Context, id string, state json.RawMessage, actor string) error {
	if !json.Valid(state) || len(state) == 0 || state[0] != '{' {
		return fmt.Errorf("%w: state must be a JSON object", ErrInvalid)
	}
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE assignments SET state_json = ?, updated_at = ? WHERE id = ?`, string(state), now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := audit.Record(ctx, tx, now, id, actor, "assignment.state_updated", id, map[string]json.RawMessage{"state": state}); err != nil {
		return err
	}
	return tx.Commit()
}
