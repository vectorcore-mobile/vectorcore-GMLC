CREATE TABLE IF NOT EXISTS request_serving_nodes (
  request_id TEXT PRIMARY KEY REFERENCES location_requests(id) ON DELETE CASCADE,
  node_type TEXT NOT NULL, mme_host TEXT NOT NULL, mme_realm TEXT NOT NULL,
  source TEXT NOT NULL, resolved_at TEXT NOT NULL
);
