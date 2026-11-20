ALTER TABLE tasks ADD COLUMN depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN allowed_tools_json TEXT CHECK (allowed_tools_json IS NULL OR json_valid(allowed_tools_json));
ALTER TABLE tasks ADD COLUMN max_steps INTEGER;
ALTER TABLE tasks ADD COLUMN deadline_at INTEGER;
ALTER TABLE tasks ADD COLUMN wake_pending INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_tasks_parent ON tasks(parent_task_id);
