CREATE TABLE criterion_evidence (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    assignment_id TEXT NOT NULL REFERENCES assignments(id),
    criterion_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('event', 'owner')),
    event_id TEXT REFERENCES events(id),
    actor TEXT NOT NULL,
    note TEXT,
    policy_version INTEGER NOT NULL,
    observed_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX idx_criterion_evidence_once ON criterion_evidence(assignment_id, criterion_id);
