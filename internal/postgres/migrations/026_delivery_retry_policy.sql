-- The runtime gate serializes policy changes with every admitted transaction.
ALTER TABLE runtime_gate
    ADD COLUMN retry_max_attempts integer NOT NULL DEFAULT 128 CHECK (retry_max_attempts BETWEEN 1 AND 1000),
    ADD COLUMN retry_initial_ms bigint NOT NULL DEFAULT 1800000 CHECK (retry_initial_ms BETWEEN 60000 AND 86400000),
    ADD COLUMN retry_max_delay_ms bigint NOT NULL DEFAULT 10800000 CHECK (retry_max_delay_ms BETWEEN 60000 AND 86400000),
    ADD COLUMN retry_max_age_ms bigint NOT NULL DEFAULT 432000000 CHECK (retry_max_age_ms BETWEEN 3600000 AND 2592000000),
    ADD CHECK (retry_max_delay_ms >= retry_initial_ms AND retry_max_age_ms >= retry_initial_ms);

-- Maintenance-owned audit: runtime may neither read nor alter these records.
CREATE TABLE delivery_retry_policy_events (
    revision bigint PRIMARY KEY,
    actor text NOT NULL,
    old_policy jsonb NOT NULL,
    new_policy jsonb NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
REVOKE ALL ON delivery_retry_policy_events FROM PUBLIC;
