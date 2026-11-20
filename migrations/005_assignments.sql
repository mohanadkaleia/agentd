-- agentd:foreign_keys=off
CREATE TABLE projects_new (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('project', 'responsibility')),
    name TEXT NOT NULL,
    objective TEXT NOT NULL,
    status TEXT NOT NULL,
    status_reason TEXT,
    paused_by TEXT CHECK (paused_by IS NULL OR paused_by IN ('user', 'budget')),
    policy_version INTEGER NOT NULL DEFAULT 1,
    policy_json TEXT NOT NULL CHECK (json_valid(policy_json)),
    completion_json TEXT CHECK (completion_json IS NULL OR json_valid(completion_json)),
    notification_json TEXT CHECK (notification_json IS NULL OR json_valid(notification_json)),
    state_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(state_json)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK (
        (kind = 'project'
            AND status IN ('active', 'paused', 'completed', 'cancelled')
            AND completion_json IS NOT NULL AND notification_json IS NULL)
        OR (kind = 'responsibility'
            AND status IN ('active', 'paused', 'revoked')
            AND completion_json IS NULL AND notification_json IS NOT NULL)
    ),
    CHECK ((status = 'paused') = (paused_by IS NOT NULL))
);

INSERT INTO projects_new (id, kind, name, objective, status, status_reason, paused_by,
                          policy_version, policy_json, completion_json, created_at, updated_at)
SELECT id, 'project', name, goal, status, status_reason,
       CASE WHEN status != 'paused' THEN NULL
            WHEN status_reason LIKE 'LLM budget exhausted%' THEN 'budget'
            ELSE 'user' END,
       policy_version, policy_json, '{}', created_at, updated_at
FROM projects;

DROP TABLE projects;
ALTER TABLE projects_new RENAME TO projects;
ALTER TABLE projects RENAME TO assignments;

ALTER TABLE tasks RENAME COLUMN project_id TO assignment_id;
ALTER TABLE actions RENAME COLUMN project_id TO assignment_id;
ALTER TABLE event_routes RENAME COLUMN project_id TO assignment_id;
ALTER TABLE schedules RENAME COLUMN project_id TO assignment_id;
ALTER TABLE model_usage RENAME COLUMN project_id TO assignment_id;
ALTER TABLE audit_log RENAME COLUMN project_id TO assignment_id;

DROP INDEX idx_tasks_project;
CREATE INDEX idx_tasks_assignment ON tasks(assignment_id);
DROP INDEX idx_model_usage_project;
CREATE INDEX idx_model_usage_assignment ON model_usage(assignment_id, created_at);
CREATE INDEX idx_audit_assignment ON audit_log(assignment_id, created_at);

ALTER TABLE tasks ADD COLUMN coalesce_key TEXT;
ALTER TABLE tasks ADD COLUMN skills_json TEXT CHECK (skills_json IS NULL OR json_valid(skills_json));
CREATE UNIQUE INDEX idx_tasks_open_coalesce ON tasks(assignment_id, coalesce_key)
    WHERE coalesce_key IS NOT NULL AND status NOT IN ('completed', 'blocked', 'cancelled');

ALTER TABLE event_routes ADD COLUMN coalesce_field TEXT;
ALTER TABLE event_routes ADD COLUMN skills_json TEXT CHECK (skills_json IS NULL OR json_valid(skills_json));
ALTER TABLE schedules ADD COLUMN skills_json TEXT CHECK (skills_json IS NULL OR json_valid(skills_json));

CREATE TABLE task_events (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    event_id TEXT NOT NULL REFERENCES events(id),
    linked_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, event_id)
);

CREATE TABLE notifications (
    id TEXT PRIMARY KEY,
    assignment_id TEXT NOT NULL REFERENCES assignments(id),
    task_id TEXT REFERENCES tasks(id),
    dedup_key TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('info', 'attention', 'action')),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'delivered', 'suppressed')),
    suppressed_reason TEXT,
    created_at INTEGER NOT NULL,
    delivered_at INTEGER
);

CREATE INDEX idx_notifications_dedup ON notifications(assignment_id, dedup_key, created_at);
CREATE INDEX idx_notifications_pending ON notifications(status, created_at) WHERE status = 'pending';
