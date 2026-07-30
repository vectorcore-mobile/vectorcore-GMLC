ALTER TABLE location_requests ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE location_requests ADD COLUMN next_attempt_at TEXT;
ALTER TABLE location_requests ADD COLUMN failure_description TEXT NOT NULL DEFAULT '';
ALTER TABLE location_results ADD COLUMN latitude REAL;
ALTER TABLE location_results ADD COLUMN longitude REAL;
ALTER TABLE location_results ADD COLUMN ecgi BLOB;
ALTER TABLE location_results ADD COLUMN source TEXT NOT NULL DEFAULT '';
