CREATE TABLE owner_notes (
    id TEXT PRIMARY KEY,
    assignment_id TEXT NOT NULL REFERENCES assignments(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    source_message_id TEXT,
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    delivered_at INTEGER,
    delivered_seq INTEGER
);

CREATE INDEX idx_owner_notes_pending ON owner_notes(task_id, created_at) WHERE delivered_at IS NULL;
CREATE UNIQUE INDEX idx_owner_notes_forwarded ON owner_notes(source_message_id, task_id) WHERE source_message_id IS NOT NULL;

CREATE TABLE questions (
    id TEXT PRIMARY KEY,
    action_id TEXT NOT NULL UNIQUE REFERENCES actions(id),
    assignment_id TEXT NOT NULL REFERENCES assignments(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    question TEXT NOT NULL,
    choices_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(choices_json)),
    context TEXT NOT NULL DEFAULT '',
    default_answer TEXT,
    status TEXT NOT NULL CHECK (status IN ('open', 'answered', 'expired', 'withdrawn')),
    answer TEXT,
    answered_via TEXT,
    source_message_id TEXT,
    created_at INTEGER NOT NULL,
    answered_at INTEGER,
    expires_at INTEGER NOT NULL,
    reminded_at INTEGER
);

CREATE INDEX idx_questions_open ON questions(status, expires_at);
CREATE INDEX idx_questions_assignment ON questions(assignment_id, status);
