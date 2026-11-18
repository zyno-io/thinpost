-- Schema 32: per-app recipient suppression list. Plaintext addresses, like
-- message_recipients (schema 28): present in backups and WAL. Go validates every
-- value before insert so CHECK/unique DETAIL lines (which quote the row) never fire.
--
-- Deploy review (lock and rollout): CREATE TABLE and CREATE INDEX on a new, empty
-- table take no meaningful lock. The single ALTER TABLE webhook_events adds a
-- nullable column with no default (no rewrite). Its CHECK is NOT VALID: every
-- existing row is NULL, new rows are still checked, and no table scan happens.
-- The ALTER takes a brief ACCESS EXCLUSIVE lock under the closed deploy gate;
-- no drain is needed beyond the gate close.
-- No routine or extension is added; regex CHECKs are already used in
-- migration 018. Schema-31 pods fail closed on the new schema, and
-- schema-32 pods require the new table. The runtime role gets SELECT, INSERT
-- and DELETE on suppressions; collector and retirement roles get nothing.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
CREATE TABLE suppressions (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    app_id text NOT NULL REFERENCES apps(id),
    address text NOT NULL CHECK (octet_length(address) BETWEEN 3 AND 998),
    address_lower text NOT NULL CHECK (octet_length(address_lower) BETWEEN 3 AND 998),
    domain_lower text NOT NULL CHECK (octet_length(domain_lower) BETWEEN 1 AND 998),
    reason text NOT NULL CHECK (reason IN ('hard_bounce','spam_complaint','unsubscribe','manual')),
    origin text NOT NULL CHECK (origin IN ('recipient','api','console')),
    source_message_id text CHECK (source_message_id IS NULL OR source_message_id ~ '^[0-9a-f]{32}$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT suppressions_app_address_key UNIQUE (app_id, address_lower)
);
CREATE INDEX suppressions_app_created_idx ON suppressions (app_id, created_at DESC, id DESC);
CREATE INDEX suppressions_app_address_prefix_idx ON suppressions (app_id, address_lower text_pattern_ops);
CREATE INDEX suppressions_app_domain_idx ON suppressions (app_id, domain_lower);
ALTER TABLE webhook_events
    ADD COLUMN suppression_reason text,
    ADD CONSTRAINT webhook_events_suppression_reason_check
        CHECK (suppression_reason IS NULL OR suppression_reason IN ('hard_bounce','spam_complaint','unsubscribe','manual')) NOT VALID;
