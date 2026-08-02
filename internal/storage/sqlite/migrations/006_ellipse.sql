ALTER TABLE location_results ADD COLUMN semi_major_meters REAL;
ALTER TABLE location_results ADD COLUMN semi_minor_meters REAL;
ALTER TABLE location_results ADD COLUMN orientation_degrees REAL;
ALTER TABLE location_results ADD COLUMN confidence_percent INTEGER;
