ALTER TABLE task_events ADD COLUMN seen_at INTEGER;
UPDATE task_events SET seen_at = linked_at;
