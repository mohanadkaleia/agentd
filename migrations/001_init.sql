CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    goal TEXT NOT NULL,
    status TEXT NOT NULL
        CHECK (status IN ('active', 'paused', 'completed', 'cancelled')),
    policy_version INTEGER NOT NULL DEFAULT 1,
    policy_json TEXT NOT NULL CHECK (json_valid(policy_json)),
    budget_micros INTEGER NOT NULL DEFAULT 0 CHECK (budget_micros >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    parent_task_id TEXT REFERENCES tasks(id),
    title TEXT NOT NULL,
    objective TEXT NOT NULL,
    success_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(success_json)),
    status TEXT NOT NULL
        CHECK (status IN ('ready', 'running', 'waiting_event', 'waiting_approval',
                          'retryable', 'blocked', 'completed', 'cancelled')),
    priority INTEGER NOT NULL DEFAULT 0,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    next_run_at INTEGER,
    lease_owner TEXT,
    lease_until INTEGER,
    version INTEGER NOT NULL DEFAULT 0,
    result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_tasks_claim ON tasks(status, next_run_at, priority);
CREATE INDEX idx_tasks_project ON tasks(project_id);

CREATE TABLE task_dependencies (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    depends_on_id TEXT NOT NULL REFERENCES tasks(id),
    PRIMARY KEY (task_id, depends_on_id),
    CHECK (task_id != depends_on_id)
);

CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    model_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running', 'completed', 'blocked')),
    step_count INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL,
    finished_at INTEGER,
    error_json TEXT CHECK (error_json IS NULL OR json_valid(error_json))
);

CREATE INDEX idx_runs_task ON runs(task_id, status);

CREATE TABLE run_messages (
    run_id TEXT NOT NULL REFERENCES runs(id),
    seq INTEGER NOT NULL,
    message_json TEXT NOT NULL CHECK (json_valid(message_json)),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE actions (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    project_id TEXT NOT NULL REFERENCES projects(id),
    tool_name TEXT NOT NULL,
    arguments_json TEXT NOT NULL,
    arguments_hash TEXT NOT NULL,
    resource TEXT NOT NULL DEFAULT '',
    policy_version INTEGER NOT NULL,
    status TEXT NOT NULL
        CHECK (status IN ('prepared', 'pending_approval', 'dispatching', 'succeeded',
                          'failed', 'denied', 'needs_reconciliation')),
    idempotency_key TEXT NOT NULL UNIQUE,
    approval_id TEXT,
    external_id TEXT,
    result_json TEXT CHECK (result_json IS NULL OR json_valid(result_json)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_actions_task ON actions(task_id, status);

CREATE TABLE approvals (
    id TEXT PRIMARY KEY,
    action_id TEXT NOT NULL UNIQUE REFERENCES actions(id),
    arguments_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    requested_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    decided_at INTEGER,
    decided_by TEXT
);

CREATE INDEX idx_approvals_pending ON approvals(status, expires_at);

CREATE TABLE audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT,
    actor TEXT NOT NULL,
    operation TEXT NOT NULL,
    object_id TEXT,
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    created_at INTEGER NOT NULL
);
