ALTER TABLE tasks ADD COLUMN model_tier TEXT CHECK (model_tier IS NULL OR model_tier IN ('fast', 'default', 'strong'));
ALTER TABLE tasks ADD COLUMN model_override TEXT;
ALTER TABLE conversations ADD COLUMN model TEXT;
ALTER TABLE runs ADD COLUMN requested_model TEXT;
ALTER TABLE event_routes ADD COLUMN tier TEXT CHECK (tier IS NULL OR tier IN ('fast', 'default', 'strong'));
ALTER TABLE schedules ADD COLUMN tier TEXT CHECK (tier IS NULL OR tier IN ('fast', 'default', 'strong'));
ALTER TABLE obligations ADD COLUMN tier TEXT CHECK (tier IS NULL OR tier IN ('fast', 'default', 'strong'));
