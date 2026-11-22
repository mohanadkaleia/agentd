package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"agentd/internal/audit"
	"agentd/internal/ids"
)

const (
	Ready           = "ready"
	Running         = "running"
	WaitingEvent    = "waiting_event"
	WaitingApproval = "waiting_approval"
	Retryable       = "retryable"
	Blocked         = "blocked"
	Completed       = "completed"
	Cancelled       = "cancelled"
)

// PriorityInteractive marks tasks a person is waiting on. The interactive worker claims only these.
const PriorityInteractive = 100

func Terminal(status string) bool {
	return status == Completed || status == Blocked || status == Cancelled
}

var (
	ErrLeaseLost = errors.New("task lease lost")
	ErrNotFound  = errors.New("task not found")
)

type Success struct {
	RequireSucceededTools []string `json:"require_succeeded_tools,omitempty"`
}

type NewTask struct {
	AssignmentID string
	ParentTaskID string
	OriginKey    string
	CoalesceKey  string
	Skills       []string
	NextRunAt    time.Time
	Title        string
	Objective    string
	Success      Success
	Priority     int
	MaxAttempts  int
	DependsOn    []string
	Depth        int
	AllowedTools []string
	MaxSteps     int
	DeadlineAt   time.Time
}

type Task struct {
	ID           string
	AssignmentID string
	ParentTaskID string
	Title        string
	Objective    string
	Status       string
	Success      Success
	AttemptCount int
	MaxAttempts  int
	Version      int64
	LeaseOwner   string
	Depth        int
	AllowedTools []string
	MaxSteps     int
	DeadlineAt   time.Time
	Skills       []string
	Result       json.RawMessage
}

func (t Task) Allows(tool string) bool {
	if t.AllowedTools == nil {
		return true
	}
	for _, name := range t.AllowedTools {
		if name == tool {
			return true
		}
	}
	return false
}

type Lease struct {
	TaskID    string
	Owner     string
	ExpiresAt time.Time
	Version   int64
}

type Release struct {
	Status       string
	NextRunAt    time.Time
	Result       any
	CountFailure bool
	FinishRun    string
	RunStatus    string
}

// Released describes a task leaving a worker's hands, as seen by OnRelease.
type Released struct {
	TaskID       string
	AssignmentID string
	Status       string
	Result       json.RawMessage
}

type Store struct {
	DB  *sql.DB
	Now func() time.Time
	// OnRelease runs inside the transaction that releases, blocks or cancels a task, so
	// whatever it records commits together with the task's new status.
	OnRelease func(ctx context.Context, tx *sql.Tx, r Released) error
}

func (s *Store) released(ctx context.Context, tx *sql.Tx, r Released) error {
	if s.OnRelease == nil {
		return nil
	}
	return s.OnRelease(ctx, tx, r)
}

func (s *Store) Create(ctx context.Context, t NewTask) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := s.CreateTx(ctx, tx, t)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) CreateTx(ctx context.Context, tx *sql.Tx, t NewTask) (string, error) {
	success, err := json.Marshal(t.Success)
	if err != nil {
		return "", err
	}
	if t.MaxAttempts == 0 {
		t.MaxAttempts = 3
	}
	var next, deadline, allowed, maxSteps, skills any
	if len(t.Skills) > 0 {
		body, err := json.Marshal(t.Skills)
		if err != nil {
			return "", err
		}
		skills = string(body)
	}
	if !t.NextRunAt.IsZero() {
		next = t.NextRunAt.UnixMilli()
	}
	if !t.DeadlineAt.IsZero() {
		deadline = t.DeadlineAt.UnixMilli()
	}
	if t.AllowedTools != nil {
		body, err := json.Marshal(t.AllowedTools)
		if err != nil {
			return "", err
		}
		allowed = string(body)
	}
	if t.MaxSteps > 0 {
		maxSteps = t.MaxSteps
	}
	id := ids.New("tsk")
	now := s.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, assignment_id, parent_task_id, origin_key, coalesce_key, skills_json,
		title, objective, success_json, status, priority, max_attempts, next_run_at, depth, allowed_tools_json, max_steps,
		deadline_at, created_at, updated_at)
		VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, 'ready', ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (origin_key) WHERE origin_key IS NOT NULL DO NOTHING`,
		id, t.AssignmentID, t.ParentTaskID, t.OriginKey, t.CoalesceKey, skills, t.Title, t.Objective, string(success),
		t.Priority, t.MaxAttempts, next, t.Depth, allowed, maxSteps, deadline, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE origin_key = ?`, t.OriginKey).Scan(&id)
		return id, err
	}
	for _, dep := range t.DependsOn {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_dependencies (task_id, depends_on_id) VALUES (?, ?)`, id, dep); err != nil {
			return "", err
		}
	}
	if err := audit.Record(ctx, tx, now, t.AssignmentID, "scheduler", "task.created", id,
		map[string]string{"title": t.Title, "origin": t.OriginKey}); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) FindByOrigin(ctx context.Context, originKey string) (string, bool, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM tasks WHERE origin_key = ?`, originKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func (s *Store) Get(ctx context.Context, id string) (Task, error) {
	var t Task
	var success string
	var owner, parent, allowed, result, skills sql.NullString
	var maxSteps, deadline sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT id, assignment_id, parent_task_id, title, objective, status, success_json,
		attempt_count, max_attempts, version, lease_owner, depth, allowed_tools_json, max_steps, deadline_at, result_json, skills_json
		FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.AssignmentID, &parent, &t.Title, &t.Objective, &t.Status, &success,
			&t.AttemptCount, &t.MaxAttempts, &t.Version, &owner, &t.Depth, &allowed, &maxSteps, &deadline, &result, &skills)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.LeaseOwner, t.ParentTaskID = owner.String, parent.String
	t.MaxSteps = int(maxSteps.Int64)
	if deadline.Valid {
		t.DeadlineAt = time.UnixMilli(deadline.Int64)
	}
	if result.Valid {
		t.Result = json.RawMessage(result.String)
	}
	if skills.Valid {
		if err := json.Unmarshal([]byte(skills.String), &t.Skills); err != nil {
			return t, err
		}
	}
	if allowed.Valid {
		if err := json.Unmarshal([]byte(allowed.String), &t.AllowedTools); err != nil {
			return t, err
		}
		if t.AllowedTools == nil {
			t.AllowedTools = []string{}
		}
	}
	return t, json.Unmarshal([]byte(success), &t.Success)
}

const WorkerOriginPrefix = "delegate:"

func (s *Store) Workers(ctx context.Context, parentID string) ([]Task, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM tasks WHERE parent_task_id = ? AND origin_key LIKE ?
		ORDER BY created_at, id`, parentID, WorkerOriginPrefix+"%")
	if err != nil {
		return nil, err
	}
	var idsList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		idsList = append(idsList, id)
	}
	rows.Close()
	out := make([]Task, 0, len(idsList))
	for _, id := range idsList {
		t, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func wakeParent(ctx context.Context, tx *sql.Tx, now time.Time, taskID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks
		SET wake_pending = 1,
		    status = CASE WHEN status = 'waiting_event' THEN 'ready' ELSE status END,
		    updated_at = ?
		WHERE id = (SELECT parent_task_id FROM tasks WHERE id = ?)`, now.UnixMilli(), taskID)
	return err
}

func (s *Store) ClaimReady(ctx context.Context, owner string, ttl time.Duration) (*Lease, error) {
	return s.claim(ctx, owner, ttl, math.MinInt32)
}

// ClaimInteractive claims only tasks with at least PriorityInteractive.
func (s *Store) ClaimInteractive(ctx context.Context, owner string, ttl time.Duration) (*Lease, error) {
	return s.claim(ctx, owner, ttl, PriorityInteractive)
}

func (s *Store) claim(ctx context.Context, owner string, ttl time.Duration, minPriority int) (*Lease, error) {
	now := s.Now()
	ms := now.UnixMilli()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	exhausted, err := tx.QueryContext(ctx, `UPDATE tasks
		SET status = 'blocked', lease_owner = NULL, lease_until = NULL, version = version + 1,
		    attempt_count = attempt_count + 1,
		    result_json = json_object('error', 'lease expired too many times'), updated_at = ?
		WHERE status = 'running' AND lease_until < ? AND attempt_count + 1 >= max_attempts
		RETURNING id, assignment_id`, ms, ms)
	if err != nil {
		return nil, err
	}
	var blocked [][2]string
	for exhausted.Next() {
		var id, assignment string
		if err := exhausted.Scan(&id, &assignment); err != nil {
			exhausted.Close()
			return nil, err
		}
		blocked = append(blocked, [2]string{id, assignment})
	}
	exhausted.Close()
	for _, b := range blocked {
		if err := wakeParent(ctx, tx, now, b[0]); err != nil {
			return nil, err
		}
		if err := audit.Record(ctx, tx, now, b[1], "scheduler", "task.blocked", b[0], map[string]string{"reason": "lease expired too many times"}); err != nil {
			return nil, err
		}
		if err := s.released(ctx, tx, Released{TaskID: b[0], AssignmentID: b[1], Status: Blocked,
			Result: json.RawMessage(`{"error":"lease expired too many times"}`)}); err != nil {
			return nil, err
		}
	}

	lease := Lease{Owner: owner, ExpiresAt: time.UnixMilli(ms + ttl.Milliseconds())}
	var assignment string
	err = tx.QueryRowContext(ctx, `UPDATE tasks
		SET attempt_count = attempt_count + CASE WHEN status = 'running' THEN 1 ELSE 0 END,
		    status = 'running', lease_owner = ?, lease_until = ?, version = version + 1, wake_pending = 0, updated_at = ?
		WHERE id = (
			SELECT t.id FROM tasks t JOIN assignments a ON a.id = t.assignment_id
						WHERE a.status = 'active'
			  AND NOT EXISTS (SELECT 1 FROM system_state WHERE key = 'halted')
			  AND t.priority >= ?
			  AND ((t.status IN ('ready', 'retryable') AND (t.next_run_at IS NULL OR t.next_run_at <= ?))
			       OR (t.status = 'running' AND t.lease_until < ?))
			  AND NOT EXISTS (
				SELECT 1 FROM task_dependencies d JOIN tasks dep ON dep.id = d.depends_on_id
				WHERE d.task_id = t.id AND dep.status != 'completed')
			ORDER BY t.priority DESC, t.created_at, t.id
			LIMIT 1)
		RETURNING id, assignment_id, version`,
		owner, lease.ExpiresAt.UnixMilli(), ms, minPriority, ms, ms).Scan(&lease.TaskID, &assignment, &lease.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, now, assignment, owner, "task.leased", lease.TaskID, nil); err != nil {
		return nil, err
	}
	return &lease, tx.Commit()
}

func runError(r Release) any {
	if r.RunStatus != "blocked" || r.Result == nil {
		return nil
	}
	body, err := json.Marshal(r.Result)
	if err != nil {
		return nil
	}
	return string(body)
}

func HoldsLease(ctx context.Context, tx *sql.Tx, l Lease, now time.Time) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks
		WHERE id = ? AND lease_owner = ? AND version = ? AND status = 'running' AND lease_until > ?`,
		l.TaskID, l.Owner, l.Version, now.UnixMilli()).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) Renew(ctx context.Context, l Lease, ttl time.Duration) (Lease, error) {
	now := s.Now()
	until := now.Add(ttl)
	res, err := s.DB.ExecContext(ctx, `UPDATE tasks SET lease_until = ?, updated_at = ?
		WHERE id = ? AND lease_owner = ? AND version = ? AND status = 'running' AND lease_until > ?`,
		until.UnixMilli(), now.UnixMilli(), l.TaskID, l.Owner, l.Version, now.UnixMilli())
	if err != nil {
		return l, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return l, ErrLeaseLost
	}
	l.ExpiresAt = until
	return l, nil
}

func (s *Store) Release(ctx context.Context, l Lease, r Release) error {
	var result, next any
	if r.Result != nil {
		body, err := json.Marshal(r.Result)
		if err != nil {
			return err
		}
		result = string(body)
	}
	if !r.NextRunAt.IsZero() {
		next = r.NextRunAt.UnixMilli()
	}
	failures := 0
	if r.CountFailure {
		failures = 1
	}
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var assignment, status string
	err = tx.QueryRowContext(ctx, `UPDATE tasks
				SET status = CASE WHEN ? IN ('waiting_event', 'waiting_approval') AND wake_pending = 1 THEN 'ready' ELSE ? END,
		    lease_owner = NULL, lease_until = NULL, version = version + 1,
		    next_run_at = ?, result_json = COALESCE(?, result_json),
		    attempt_count = attempt_count + ?, updated_at = ?
		WHERE id = ? AND lease_owner = ? AND version = ? AND status = 'running'
		RETURNING assignment_id, status`,
		r.Status, r.Status, next, result, failures, now.UnixMilli(), l.TaskID, l.Owner, l.Version).Scan(&assignment, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if r.FinishRun != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, finished_at = ?, error_json = ? WHERE id = ?`,
			r.RunStatus, now.UnixMilli(), runError(r), r.FinishRun); err != nil {
			return err
		}
	}
	if Terminal(status) {
		if err := wakeParent(ctx, tx, now, l.TaskID); err != nil {
			return err
		}
	}
	if err := audit.Record(ctx, tx, now, assignment, l.Owner, "task."+status, l.TaskID, r.Result); err != nil {
		return err
	}
	var body json.RawMessage
	if result != nil {
		body = json.RawMessage(result.(string))
	}
	if err := s.released(ctx, tx, Released{TaskID: l.TaskID, AssignmentID: assignment, Status: status, Result: body}); err != nil {
		return err
	}
	return tx.Commit()
}

// Cancel stops a task that has not finished. A worker holding it loses its lease at the
// next renewal. Actions that were prepared or waiting for approval are denied; an action
// already dispatched keeps its own outcome.
func (s *Store) Cancel(ctx context.Context, id, actor string) error {
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var assignment string
	err = tx.QueryRowContext(ctx, `UPDATE tasks SET status = 'cancelled', lease_owner = NULL, lease_until = NULL,
		version = version + 1, updated_at = ?
		WHERE id = ? AND status NOT IN ('completed', 'blocked', 'cancelled')
		RETURNING assignment_id`, now.UnixMilli(), id).Scan(&assignment)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status = 'rejected', decided_at = ?, decided_by = ?
		WHERE status = 'pending' AND action_id IN (SELECT id FROM actions WHERE task_id = ?)`, now.UnixMilli(), actor, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'denied', updated_at = ?
		WHERE task_id = ? AND status IN ('prepared', 'pending_approval')`, now.UnixMilli(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'blocked', finished_at = ?, error_json = '{"error":"cancelled"}'
		WHERE task_id = ? AND status = 'running'`,
		now.UnixMilli(), id); err != nil {
		return err
	}
	if err := wakeParent(ctx, tx, now, id); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, assignment, actor, "task.cancelled", id, nil); err != nil {
		return err
	}
	if err := s.released(ctx, tx, Released{TaskID: id, AssignmentID: assignment, Status: Cancelled}); err != nil {
		return err
	}
	return tx.Commit()
}
