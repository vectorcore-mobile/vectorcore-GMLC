ALTER TABLE location_requests ADD COLUMN qos_class INTEGER;
ALTER TABLE location_requests ADD COLUMN qos_horizontal_accuracy_meters REAL;
ALTER TABLE location_requests ADD COLUMN qos_vertical_accuracy_meters REAL;
ALTER TABLE location_requests ADD COLUMN qos_vertical_requested INTEGER NOT NULL DEFAULT 0 CHECK(qos_vertical_requested IN (0,1));
ALTER TABLE location_requests ADD COLUMN qos_response_time INTEGER;
