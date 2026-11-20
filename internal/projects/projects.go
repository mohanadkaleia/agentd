package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"agentd/internal/audit"
	"agentd/internal/ids"
	"agentd/internal/policy"
)

var ErrNotFound = errors.New("project not found")

type Project struct {
	ID            string
	Name          string
	Goal          string
	Status        string
	StatusReason  string
	PolicyVersion int64
	Policy        policy.Policy
}

type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s *Store) Create(ctx context.Context, name, goal string, p policy.Policy) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	id := ids.New("prj")
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, goal, status, policy_json, created_at, updated_at)
		VALUES (?, ?, ?, 'active', ?, ?, ?)`, id, name, goal, string(body), now.UnixMilli(), now.UnixMilli()); err != nil {
		return "", err
	}
	if err := audit.Record(ctx, tx, now, id, "user", "project.created", id, map[string]string{"name": name}); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Store) Get(ctx context.Context, id string) (Project, error) {
	var p Project
	var body string
	var reason sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id, name, goal, status, status_reason, policy_version, policy_json FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Goal, &p.Status, &reason, &p.PolicyVersion, &body)
	p.StatusReason = reason.String
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal([]byte(body), &p.Policy)
}

func (s *Store) SetStatus(ctx context.Context, id, status, reason, actor string) error {
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE projects SET status = ?, status_reason = NULLIF(?, ''), updated_at = ? WHERE id = ?`,
		status, reason, now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := audit.Record(ctx, tx, now, id, actor, "project."+status, id, map[string]string{"reason": reason}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetPolicy(ctx context.Context, id string, p policy.Policy, actor string) (int64, error) {
	if err := p.Validate(); err != nil {
		return 0, err
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
	err = tx.QueryRowContext(ctx, `UPDATE projects SET policy_json = ?, policy_version = policy_version + 1, updated_at = ?
		WHERE id = ? RETURNING policy_version`, string(body), now.UnixMilli(), id).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if err := audit.Record(ctx, tx, now, id, actor, "project.policy_updated", id, map[string]int64{"policy_version": version}); err != nil {
		return 0, err
	}
	return version, tx.Commit()
}
