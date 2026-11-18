-- New signed links carry their own destination and have no expiry. Existing
-- expiring mappings keep their original lifetime and send-window checks.
ALTER TABLE tracking_objects ALTER COLUMN expires_at DROP NOT NULL;
ALTER TABLE tracking_objects ADD CONSTRAINT tracking_indefinite_no_send_window
    CHECK (expires_at IS NOT NULL OR post_send_validity_seconds IS NULL);

-- Expiry retirement must never silently remove indefinite analytics mappings.
ALTER TABLE tracking_objects ADD CONSTRAINT tracking_indefinite_no_retirement
    CHECK (expires_at IS NOT NULL OR retired_at IS NULL);
