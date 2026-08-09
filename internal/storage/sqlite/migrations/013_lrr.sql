CREATE TABLE IF NOT EXISTS deferred_location_subscriptions (
  lcs_reference_number INTEGER PRIMARY KEY, client_id TEXT NOT NULL REFERENCES lcs_clients(id),
  target_kind TEXT NOT NULL, target_value TEXT NOT NULL,
  state TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS location_reports (
  id TEXT PRIMARY KEY,
  lcs_reference_number INTEGER REFERENCES deferred_location_subscriptions(lcs_reference_number),
  location_event INTEGER NOT NULL, target_kind TEXT, target_value TEXT,
  raw_gad BLOB, ecgi BLOB, accuracy_fulfilment INTEGER, age_of_location_estimate INTEGER,
  raw_velocity_estimate BLOB, eutran_positioning_data BLOB, received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_location_reports_received_at ON location_reports(received_at);
