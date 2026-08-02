ALTER TABLE location_results ADD COLUMN uncertainty_meters REAL;
CREATE INDEX IF NOT EXISTS idx_audit_events_created ON audit_events(created_at);
