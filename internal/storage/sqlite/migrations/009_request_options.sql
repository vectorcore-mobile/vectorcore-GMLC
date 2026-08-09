ALTER TABLE location_requests ADD COLUMN location_type INTEGER NOT NULL DEFAULT 0;
ALTER TABLE location_requests ADD COLUMN priority INTEGER;
