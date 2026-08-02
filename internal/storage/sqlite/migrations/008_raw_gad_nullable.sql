-- raw_gad was NOT NULL, but an additional_information (ECGI-only) PLA
-- outcome has no Location-Estimate to store, so CompleteRequest always sent
-- NULL for it — violating this constraint and permanently stranding those
-- requests in the locating state. SQLite can't drop a column constraint
-- in place, so the table is recreated.
CREATE TABLE location_results_new (
  request_id TEXT PRIMARY KEY REFERENCES location_requests(id) ON DELETE CASCADE,
  raw_gad BLOB,
  shape TEXT NOT NULL,
  created_at TEXT NOT NULL,
  latitude REAL,
  longitude REAL,
  ecgi BLOB,
  source TEXT NOT NULL DEFAULT '',
  uncertainty_meters REAL,
  semi_major_meters REAL,
  semi_minor_meters REAL,
  orientation_degrees REAL,
  confidence_percent INTEGER,
  age_of_location_estimate INTEGER,
  accuracy_fulfilment INTEGER,
  raw_velocity_estimate BLOB,
  eutran_positioning_data BLOB
);
INSERT INTO location_results_new (request_id, raw_gad, shape, created_at, latitude, longitude, ecgi, source, uncertainty_meters, semi_major_meters, semi_minor_meters, orientation_degrees, confidence_percent, age_of_location_estimate, accuracy_fulfilment, raw_velocity_estimate, eutran_positioning_data)
  SELECT request_id, raw_gad, shape, created_at, latitude, longitude, ecgi, source, uncertainty_meters, semi_major_meters, semi_minor_meters, orientation_degrees, confidence_percent, age_of_location_estimate, accuracy_fulfilment, raw_velocity_estimate, eutran_positioning_data
  FROM location_results;
DROP TABLE location_results;
ALTER TABLE location_results_new RENAME TO location_results;
