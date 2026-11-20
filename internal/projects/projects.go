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
	PolicyVersion int64
	Policy        policy.Policy
}

type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s *Store) Create(ctx context.Context, name, goal string, p policy.Policy) (string, error) {
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
	err := s.DB.QueryRowContext(ctx, `SELECT id, name, goal, status, policy_version, policy_json FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Goal, &p.Status, &p.PolicyVersion, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal([]byte(body), &p.Policy)
}

func (s *Store) SetStatus(ctx context.Context, id, status string) error {
	now := s.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE projects SET status = ?, updated_at = ? WHERE id = ?`, status, now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := audit.Record(ctx, tx, now, id, "user", "project."+status, id, nil); err != nil {
		return err
	}
	return tx.Commit()
}
