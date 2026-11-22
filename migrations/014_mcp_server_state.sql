ALTER TABLE mcp_servers ADD COLUMN state TEXT NOT NULL DEFAULT 'connecting';
ALTER TABLE mcp_servers ADD COLUMN transport TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_servers ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_servers ADD COLUMN next_retry_at INTEGER;
ALTER TABLE mcp_servers ADD COLUMN state_since INTEGER;
ALTER TABLE mcp_servers ADD COLUMN restart_requested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mcp_servers ADD COLUMN scopes TEXT NOT NULL DEFAULT '';
