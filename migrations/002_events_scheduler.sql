ALTER TABLE tasks ADD COLUMN origin_key TEXT;
CREATE UNIQUE INDEX idx_tasks_origin ON tasks(origin_key) WHERE origin_key IS NOT NULL;

CREATE TABLE events (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    external_id TEXT,
    kind TEXT NOT NULL,
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    received_at INTEGER NOT NULL,
    processed_at INTEGER,
    routed_count INTEGER
);

CREATE UNIQUE INDEX idx_events_dedup ON events(source, external_id) WHERE external_id IS NOT NULL;
CREATE INDEX idx_events_pending ON events(received_at) WHERE processed_at IS NULL;

CREATE TABLE event_routes (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    source TEXT NOT NULL,
    kind TEXT NOT NULL,
    match_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(match_json)),
    title TEXT NOT NULL,
    objective TEXT NOT NULL,
    success_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(success_json)),
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_event_routes_match ON event_routes(source, kind);

CREATE TABLE schedules (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    title TEXT NOT NULL,
    objective TEXT NOT NULL,
    success_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(success_json)),
    interval_ms INTEGER NOT NULL CHECK (interval_ms > 0),
    next_fire_at INTEGER NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_schedules_due ON schedules(enabled, next_fire_at);
