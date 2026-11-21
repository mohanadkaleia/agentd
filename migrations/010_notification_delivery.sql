ALTER TABLE notifications ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notifications ADD COLUMN next_attempt_at INTEGER;
ALTER TABLE notifications ADD COLUMN last_error TEXT;
ALTER TABLE notifications ADD COLUMN failed_at INTEGER;
ALTER TABLE notifications ADD COLUMN channel_ref TEXT;
