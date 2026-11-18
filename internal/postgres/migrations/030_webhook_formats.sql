-- Schema 30: provider-compatible webhook formats. Each endpoint has a payload
-- and signature format fixed at creation; existing endpoints are 'thinpost'.
-- Postmark-compatible endpoints are unsigned, as Postmark's are, and store no
-- secret. Optional HTTP Basic credentials and custom request headers are
-- sealed with the object keyring; only the user name and header names are kept
-- in plaintext, for display. The event filter is renamed: it holds event types
-- of the endpoint's format (outcome names for 'thinpost'). webhook_events
-- .delivery_state records the delivery state an event produced, so a final
-- failure is distinguishable from a retried one; earlier events keep NULL.
-- No routine or extension is installed and no table is rewritten.
ALTER TABLE webhook_subscriptions RENAME COLUMN outcomes TO events;
ALTER TABLE webhook_subscriptions
    ADD COLUMN format text NOT NULL DEFAULT 'thinpost' CHECK (format IN ('thinpost', 'postmark', 'resend', 'mailgun')),
    ADD COLUMN auth_ciphertext bytea CHECK (octet_length(auth_ciphertext) BETWEEN 1 AND 16384),
    ADD COLUMN auth_username text CHECK (octet_length(auth_username) BETWEEN 1 AND 256),
    ADD COLUMN auth_header_names text[] CHECK (cardinality(auth_header_names) BETWEEN 1 AND 5),
    DROP CONSTRAINT webhook_subscriptions_live_secret,
    ADD CONSTRAINT webhook_subscriptions_live_secret
        CHECK (deleted_at IS NOT NULL OR format = 'postmark' OR secret_ciphertext IS NOT NULL),
    ADD CONSTRAINT webhook_subscriptions_unsigned_format
        CHECK (format <> 'postmark' OR secret_ciphertext IS NULL),
    ADD CONSTRAINT webhook_subscriptions_auth_summary
        CHECK ((auth_ciphertext IS NULL) = (auth_username IS NULL AND auth_header_names IS NULL)),
    ADD CONSTRAINT webhook_subscriptions_deleted_no_secrets
        CHECK (deleted_at IS NULL OR (secret_ciphertext IS NULL AND auth_ciphertext IS NULL));
ALTER TABLE webhook_events ADD COLUMN delivery_state text;
