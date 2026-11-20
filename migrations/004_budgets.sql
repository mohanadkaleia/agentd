ALTER TABLE actions ADD COLUMN dispatched_at INTEGER;
CREATE INDEX idx_actions_rate ON actions(project_id, tool_name, dispatched_at);

ALTER TABLE projects ADD COLUMN status_reason TEXT;

CREATE TABLE model_usage (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id TEXT NOT NULL REFERENCES projects(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    model TEXT NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cost_micros INTEGER NOT NULL CHECK (cost_micros >= 0),
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_model_usage_project ON model_usage(project_id);
