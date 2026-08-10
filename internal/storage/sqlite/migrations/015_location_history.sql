CREATE TABLE IF NOT EXISTS location_history (
  id TEXT PRIMARY KEY,
  target_kind TEXT NOT NULL, target_value TEXT NOT NULL,
  shape TEXT NOT NULL, latitude REAL, longitude REAL, uncertainty_meters REAL,
  recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_location_history_target ON location_history(target_kind, target_value, recorded_at);
