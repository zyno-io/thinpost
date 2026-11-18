-- Existing mappings have no verified post-send lifetime. Keep the value NULL so
-- the updated sender holds them until an operator has reviewed their history.
ALTER TABLE tracking_objects
    ADD COLUMN post_send_validity_seconds integer
        CHECK (post_send_validity_seconds BETWEEN 1 AND 31536000);
