ALTER TABLE deliveries
    ADD COLUMN bounce_key_version text,
    ADD COLUMN bounce_token_hash bytea UNIQUE,
    ADD CONSTRAINT bounce_address_complete CHECK (
        (bounce_key_version IS NULL) = (bounce_token_hash IS NULL) AND
        (bounce_token_hash IS NULL OR octet_length(bounce_token_hash) = 32)
    );
