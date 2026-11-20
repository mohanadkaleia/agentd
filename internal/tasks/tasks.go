package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentd/internal/audit"
	"agentd/internal/ids"
)

const (
	Ready           = "ready"
	Running         = "running"
	WaitingApproval = "waiting_approval"
	Retryable       = "retryable"
	Blocked         = "blocked"
	Completed       = "completed"
)

var (
	ErrLeaseLost = errors.New("task lease lost")
	ErrNotFound  = errors.New("task not found")
)

type Success struct {
	RequireSucceededTools []string `json:"require_succeeded_tools,omitempty"`
}

type NewTask struct {
	ProjectID    string
	ParentTaskID string
	OriginKey    string
	NextRunAt    time.Time
	Title        string
	Objective    string
	Success      Success
	Priority     int
	MaxAttempts  int
	DependsOn    []string
}

type Task struct {
	ID           string
	ProjectID    string
	Title        string
	Objective    string
	Status       string
	Success      Success
	AttemptCount int
	MaxAttempts  int
	Version      int64
	LeaseOwner   string
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
}

type Store struct {
	DB  *sql.DB
	Now func() time.Time
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
	var next any
	if !t.NextRunAt.IsZero() {
		next = t.NextRunAt.UnixMilli()
	}
	id := ids.New("tsk")
	now := s.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, project_id, parent_task_id, origin_key, title, objective, success_json,
		status, priority, max_attempts, next_run_at, created_at, updated_at)
		VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, 'ready', ?, ?, ?, ?, ?)
		ON CONFLICT (origin_key) WHERE origin_key IS NOT NULL DO NOTHING`,
		id, t.ProjectID, t.ParentTaskID, t.OriginKey, t.Title, t.Objective, string(success),
		t.Priority, t.MaxAttempts, next, now.UnixMilli(), now.UnixMilli())
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
	if err := audit.Record(ctx, tx, now, t.ProjectID, "scheduler", "task.created", id,
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
	var owner sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id, project_id, title, objective, status, success_json, attempt_count, max_attempts, version, lease_owner
		FROM tasks WHERE id = ?`, id).
		Scan(&t.ID, &t.ProjectID, &t.Title, &t.Objective, &t.Status, &success, &t.AttemptCount, &t.MaxAttempts, &t.Version, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.LeaseOwner = owner.String
	return t, json.Unmarshal([]byte(success), &t.Success)
}

func (s *Store) ClaimReady(ctx context.Context, owner string, ttl time.Duration) (*Lease, error) {
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
		RETURNING id, project_id`, ms, ms)
	if err != nil {
		return nil, err
	}
	var blocked [][2]string
	for exhausted.Next() {
		var id, project string
		if err := exhausted.Scan(&id, &project); err != nil {
			exhausted.Close()
			return nil, err
		}
		blocked = append(blocked, [2]string{id, project})
	}
	exhausted.Close()
	for _, b := range blocked {
		if err := audit.Record(ctx, tx, now, b[1], "scheduler", "task.blocked", b[0], map[string]string{"reason": "lease expired too many times"}); err != nil {
			return nil, err
		}
	}

	lease := Lease{Owner: owner, ExpiresAt: time.UnixMilli(ms + ttl.Milliseconds())}
	var project string
	err = tx.QueryRowContext(ctx, `UPDATE tasks
		SET attempt_count = attempt_count + CASE WHEN status = 'running' THEN 1 ELSE 0 END,
		    status = 'running', lease_owner = ?, lease_until = ?, version = version + 1, updated_at = ?
		WHERE id = (
			SELECT t.id FROM tasks t JOIN projects p ON p.id = t.project_id
			WHERE p.status = 'active'
			  AND ((t.status IN ('ready', 'retryable') AND (t.next_run_at IS NULL OR t.next_run_at <= ?))
			       OR (t.status = 'running' AND t.lease_until < ?))
			  AND NOT EXISTS (
				SELECT 1 FROM task_dependencies d JOIN tasks dep ON dep.id = d.depends_on_id
				WHERE d.task_id = t.id AND dep.status != 'completed')
			ORDER BY t.priority DESC, t.created_at, t.id
			LIMIT 1)
		RETURNING id, project_id, version`,
		owner, lease.ExpiresAt.UnixMilli(), ms, ms, ms).Scan(&lease.TaskID, &project, &lease.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, now, project, owner, "task.leased", lease.TaskID, nil); err != nil {
		return nil, err
	}
	return &lease, tx.Commit()
}

func (s *Store) Renew(ctx context.Context, l Lease, ttl time.Duration) (Lease, error) {
	until := s.Now().Add(ttl)
	res, err := s.DB.ExecContext(ctx, `UPDATE tasks SET lease_until = ?, updated_at = ?
		WHERE id = ? AND lease_owner = ? AND version = ? AND status = 'running'`,
		until.UnixMilli(), s.Now().UnixMilli(), l.TaskID, l.Owner, l.Version)
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
	var project string
	err = tx.QueryRowContext(ctx, `UPDATE tasks
		SET status = ?, lease_owner = NULL, lease_until = NULL, version = version + 1,
		    next_run_at = ?, result_json = COALESCE(?, result_json),
		    attempt_count = attempt_count + ?, updated_at = ?
		WHERE id = ? AND lease_owner = ? AND version = ? AND status = 'running'
		RETURNING project_id`,
		r.Status, next, result, failures, now.UnixMilli(), l.TaskID, l.Owner, l.Version).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, now, project, l.Owner, "task."+r.Status, l.TaskID, r.Result); err != nil {
		return err
	}
	return tx.Commit()
}
