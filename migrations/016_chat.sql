ALTER TABLE assignments ADD COLUMN builtin TEXT;
CREATE UNIQUE INDEX idx_assignments_builtin ON assignments(builtin) WHERE builtin IS NOT NULL;

CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'archived')),
    summary_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(summary_json)),
    summary_through_seq INTEGER NOT NULL DEFAULT 0,
    summary_task_id TEXT REFERENCES tasks(id),
    summary_target_seq INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE conversation_messages (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    seq INTEGER NOT NULL,
    author TEXT NOT NULL CHECK (author IN ('owner', 'agent', 'system')),
    kind TEXT NOT NULL CHECK (kind IN ('text', 'proposal', 'approval', 'question', 'notification', 'report',
                                       'command_result', 'error', 'notice')),
    body_json TEXT NOT NULL CHECK (json_valid(body_json)),
    ref_kind TEXT,
    ref_id TEXT,
    channel TEXT NOT NULL DEFAULT '',
    client_msg_id TEXT,
    turn_task_id TEXT REFERENCES tasks(id),
    created_at INTEGER NOT NULL,
    UNIQUE (conversation_id, seq)
);

CREATE UNIQUE INDEX idx_conversation_messages_client ON conversation_messages(conversation_id, client_msg_id)
    WHERE client_msg_id IS NOT NULL;
CREATE UNIQUE INDEX idx_conversation_messages_card ON conversation_messages(conversation_id, kind, ref_id)
    WHERE ref_id IS NOT NULL;
CREATE INDEX idx_conversation_messages_turn ON conversation_messages(turn_task_id);

CREATE TABLE conversation_reads (
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    channel TEXT NOT NULL,
    seen_seq INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (conversation_id, channel)
);
