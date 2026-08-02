ALTER TABLE location_results ADD COLUMN age_of_location_estimate INTEGER;
ALTER TABLE location_results ADD COLUMN accuracy_fulfilment INTEGER;
ALTER TABLE location_results ADD COLUMN raw_velocity_estimate BLOB;
ALTER TABLE location_results ADD COLUMN eutran_positioning_data BLOB;
