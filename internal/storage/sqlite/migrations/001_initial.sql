CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS lcs_clients (
  id TEXT PRIMARY KEY, credential_hash BLOB NOT NULL, enabled INTEGER NOT NULL CHECK(enabled IN (0,1)), created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS client_services (
  client_id TEXT NOT NULL REFERENCES lcs_clients(id) ON DELETE CASCADE, service TEXT NOT NULL, PRIMARY KEY(client_id, service)
);
CREATE TABLE IF NOT EXISTS client_target_prefixes (
  client_id TEXT NOT NULL REFERENCES lcs_clients(id) ON DELETE CASCADE, prefix TEXT NOT NULL, PRIMARY KEY(client_id, prefix)
);
CREATE TABLE IF NOT EXISTS location_requests (
  id TEXT PRIMARY KEY, client_id TEXT NOT NULL REFERENCES lcs_clients(id), idempotency_key TEXT NOT NULL,
  service TEXT NOT NULL, target_kind TEXT NOT NULL, target_value TEXT NOT NULL, state TEXT NOT NULL,
  failure_code TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  UNIQUE(client_id, idempotency_key)
);
CREATE TABLE IF NOT EXISTS location_results (
  request_id TEXT PRIMARY KEY REFERENCES location_requests(id) ON DELETE CASCADE, raw_gad BLOB NOT NULL,
  shape TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT, client_id TEXT, type TEXT NOT NULL, detail TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_location_requests_state ON location_requests(state);
CREATE INDEX IF NOT EXISTS idx_audit_events_request ON audit_events(request_id, id);
