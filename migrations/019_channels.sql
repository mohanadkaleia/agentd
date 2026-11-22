CREATE TABLE channels (
    name TEXT PRIMARY KEY,
    sender_id TEXT,
    chat_id TEXT,
    level TEXT NOT NULL DEFAULT 'summary' CHECK (level IN ('minimal', 'summary', 'full')),
    widen INTEGER NOT NULL DEFAULT 0 CHECK (widen IN (0, 1)),
    decide INTEGER NOT NULL DEFAULT 1 CHECK (decide IN (0, 1)),
    paired_at INTEGER,
    revoked_at INTEGER,
    update_offset INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE TABLE channel_codes (
    code_hash TEXT PRIMARY KEY,
    channel TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at INTEGER
);
