CREATE TABLE model_reservations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    assignment_id TEXT NOT NULL REFERENCES assignments(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    estimate_micros INTEGER NOT NULL CHECK (estimate_micros >= 0),
    status TEXT NOT NULL CHECK (status IN ('reserved', 'settled', 'unknown')),
    created_at INTEGER NOT NULL,
    settled_at INTEGER
);

CREATE INDEX idx_model_reservations_open ON model_reservations(assignment_id, created_at) WHERE status != 'settled';
