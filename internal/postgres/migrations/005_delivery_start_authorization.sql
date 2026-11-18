-- Existing attempts may have reached SMTP before this protocol existed.
-- Conservatively mark them authorized without inventing a descriptor.
ALTER TABLE delivery_attempts
    ADD COLUMN start_authorized boolean NOT NULL DEFAULT true,
    ADD COLUMN start_descriptor_hash bytea,
    ADD COLUMN start_session_id text,
    ADD CONSTRAINT start_descriptor_complete CHECK (
        (start_descriptor_hash IS NULL) = (start_session_id IS NULL) AND
        (start_descriptor_hash IS NULL OR octet_length(start_descriptor_hash) = 32) AND
        (start_authorized OR (start_descriptor_hash IS NULL AND start_session_id IS NULL))
    );
