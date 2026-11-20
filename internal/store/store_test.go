package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssignmentsRebuildPreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateThrough(ctx, db, "004_budgets.sql"); err != nil {
		t.Fatal(err)
	}
	seed := []string{
		`INSERT INTO projects (id, name, goal, status, policy_json, created_at, updated_at) VALUES ('p1', 'refund', 'get refund', 'active', '{}', 1, 1)`,
		`INSERT INTO projects (id, name, goal, status, status_reason, policy_json, created_at, updated_at) VALUES ('p2', 'other', 'g', 'paused', 'LLM budget exhausted: spent $3', '{}', 1, 1)`,
		`INSERT INTO tasks (id, project_id, title, objective, status, created_at, updated_at) VALUES ('t1', 'p1', 't', 'o', 'ready', 1, 1)`,
		`INSERT INTO runs (id, task_id, model_id, status, started_at) VALUES ('r1', 't1', 'm', 'running', 1)`,
		`INSERT INTO actions (id, run_id, task_id, project_id, tool_name, arguments_json, arguments_hash, policy_version, status, idempotency_key, created_at, updated_at)
			VALUES ('a1', 'r1', 't1', 'p1', 'send_email', '{}', 'h', 1, 'succeeded', 'k1', 1, 1)`,
		`INSERT INTO event_routes (id, project_id, source, kind, title, objective, created_at) VALUES ('rt1', 'p1', 'gmail', 'email.received', 't', 'o', 1)`,
		`INSERT INTO schedules (id, project_id, title, objective, interval_ms, next_fire_at, created_at) VALUES ('s1', 'p1', 't', 'o', 1000, 1, 1)`,
		`INSERT INTO model_usage (project_id, task_id, run_id, model, input_tokens, output_tokens, cost_micros, created_at) VALUES ('p1', 't1', 'r1', 'm', 1, 1, 5, 1)`,
		`INSERT INTO audit_log (project_id, actor, operation, payload_json, created_at) VALUES ('p1', 'a', 'op', '{}', 1)`,
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"tasks", "actions", "event_routes", "schedules", "model_usage", "audit_log"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE assignment_id = 'p1'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s rows for p1 = %d (%v), want 1", table, n, err)
		}
	}
	var kind, objective, pausedBy string
	db.QueryRowContext(ctx, `SELECT kind, objective, COALESCE(paused_by, '') FROM assignments WHERE id = 'p2'`).Scan(&kind, &objective, &pausedBy)
	if kind != "project" || objective != "g" || pausedBy != "budget" {
		t.Fatalf("p2 = %s %q paused_by=%q", kind, objective, pausedBy)
	}
	var ddl string
	db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name = 'tasks'`).Scan(&ddl)
	if !strings.Contains(ddl, "REFERENCES \"assignments\"") && !strings.Contains(ddl, "REFERENCES assignments") {
		t.Fatalf("tasks foreign key was not retargeted:\n%s", ddl)
	}
	var fk int
	db.QueryRowContext(ctx, `SELECT foreign_keys FROM pragma_foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign keys were left off after the rebuild")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, assignment_id, title, objective, status, created_at, updated_at)
		VALUES ('t2', 'missing', 't', 'o', 'ready', 1, 1)`); err == nil {
		t.Fatal("foreign key enforcement is not active after the rebuild")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO assignments (id, kind, name, objective, status, policy_json, notification_json, created_at, updated_at)
		VALUES ('x', 'responsibility', 'n', 'o', 'completed', '{}', '{}', 1, 1)`); err == nil {
		t.Fatal("a responsibility was allowed to be completed")
	}
}
