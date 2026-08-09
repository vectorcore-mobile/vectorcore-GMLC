CREATE TABLE IF NOT EXISTS delivery_subscriptions (
  id TEXT PRIMARY KEY, client_id TEXT NOT NULL REFERENCES lcs_clients(id),
  callback_url TEXT NOT NULL, callback_secret BLOB NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deliveries (
  id TEXT PRIMARY KEY, subscription_id TEXT NOT NULL REFERENCES delivery_subscriptions(id) ON DELETE CASCADE,
  payload BLOB NOT NULL, state TEXT NOT NULL, attempt_count INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TEXT, last_response_code INTEGER, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deliveries_state ON deliveries(state);
