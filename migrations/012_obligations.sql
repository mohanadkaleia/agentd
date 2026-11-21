CREATE TABLE obligations (
    id TEXT PRIMARY KEY,
    assignment_id TEXT NOT NULL REFERENCES assignments(id),
    description TEXT NOT NULL,
    every_ms INTEGER NOT NULL CHECK (every_ms > 0),
    grace_ms INTEGER NOT NULL DEFAULT 0 CHECK (grace_ms >= 0),
    evidence_json TEXT NOT NULL CHECK (json_valid(evidence_json)),
    on_miss TEXT NOT NULL CHECK (on_miss IN ('task', 'notify')),
    skills_json TEXT CHECK (skills_json IS NULL OR json_valid(skills_json)),
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_obligations_assignment ON obligations(assignment_id);
