CREATE TABLE mcp_tools (
    server TEXT NOT NULL,
    tool TEXT NOT NULL,
    definition_hash TEXT NOT NULL,
    definition_json TEXT NOT NULL CHECK (json_valid(definition_json)),
    status TEXT NOT NULL CHECK (status IN ('new', 'accepted', 'changed', 'removed')),
    accepted_hash TEXT,
    accepted_json TEXT CHECK (accepted_json IS NULL OR json_valid(accepted_json)),
    accepted_at INTEGER,
    accepted_by TEXT,
    first_seen_at INTEGER NOT NULL,
    changed_at INTEGER NOT NULL,
    PRIMARY KEY (server, tool)
);

CREATE TABLE mcp_servers (
    server TEXT PRIMARY KEY,
    instructions TEXT NOT NULL DEFAULT '',
    seen_at INTEGER NOT NULL
);

ALTER TABLE actions ADD COLUMN definition_hash TEXT;
