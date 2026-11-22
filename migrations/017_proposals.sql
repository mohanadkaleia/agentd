CREATE TABLE proposal_applied (
    action_id TEXT PRIMARY KEY REFERENCES actions(id),
    object_id TEXT NOT NULL,
    applied_at INTEGER NOT NULL
);
