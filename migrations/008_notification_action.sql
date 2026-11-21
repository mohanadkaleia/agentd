ALTER TABLE notifications ADD COLUMN action_id TEXT;
CREATE INDEX idx_notifications_action ON notifications(action_id) WHERE action_id IS NOT NULL;
