CREATE TABLE mcp_polls (
    server TEXT NOT NULL,
    poll TEXT NOT NULL,
    seen_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(seen_json)),
    primed INTEGER NOT NULL DEFAULT 0,
    last_success_at INTEGER,
    last_error TEXT NOT NULL DEFAULT '',
    failures INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (server, poll)
);
