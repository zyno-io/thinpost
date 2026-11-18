-- Schema 33: console retry policy, operations backlog index, tracking engagement
-- index and system-scoped audit rows.
--
-- The retry policy moves from runtime_gate (maintenance-written, serialized by
-- the gate row lock) to delivery_retry_policy, which the runtime role may read
-- and update with a revision check. runtime_gate keeps its reviewed grants
-- (SELECT and UPDATE (lock_guard) only). The four CHECKs and the multi-column
-- CHECK on the dropped columns go with them. delivery_retry_policy_events stays
-- maintenance-only historical evidence and receives no new writes; console
-- changes are audited in audit_events.
--
-- audit_events.app_id becomes nullable for system-scoped operator actions
-- (retry_policy.updated now, feedback-loop actions later). The foreign key is
-- kept, so a non-null value must still name an app.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. CREATE TABLE and the INSERT touch one new row. ALTER TABLE runtime_gate
-- DROP COLUMN rewrites nothing (the table has one row) and ALTER COLUMN DROP
-- NOT NULL on audit_events is a catalog-only change; both take a brief ACCESS
-- EXCLUSIVE lock. The two CREATE INDEX statements are not CONCURRENT: they block
-- writes to tracking_links (one row per tracked link) and webhook_deliveries
-- (one row per event and subscription) for the duration of the build, which is
-- short at the current size and safe under the closed gate. No drain is needed
-- beyond the gate close. Schema-32 pods read the dropped columns and fail on the
-- new schema; they are already fenced by the closed gate. Schema-33 pods require
-- the new table. The runtime role gets SELECT and UPDATE on
-- delivery_retry_policy; collector and retirement roles get nothing.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
CREATE TABLE delivery_retry_policy (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    max_attempts integer NOT NULL CHECK (max_attempts BETWEEN 1 AND 1000),
    initial_ms bigint NOT NULL CHECK (initial_ms BETWEEN 60000 AND 86400000),
    max_delay_ms bigint NOT NULL CHECK (max_delay_ms BETWEEN 60000 AND 86400000),
    max_age_ms bigint NOT NULL CHECK (max_age_ms BETWEEN 3600000 AND 2592000000),
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by text,
    CHECK (max_delay_ms >= initial_ms AND max_age_ms >= initial_ms)
);
INSERT INTO delivery_retry_policy (max_attempts, initial_ms, max_delay_ms, max_age_ms, changed_by)
    SELECT retry_max_attempts, retry_initial_ms, retry_max_delay_ms, retry_max_age_ms, 'migration:033'
    FROM runtime_gate WHERE singleton;
ALTER TABLE runtime_gate
    DROP COLUMN retry_max_attempts,
    DROP COLUMN retry_initial_ms,
    DROP COLUMN retry_max_delay_ms,
    DROP COLUMN retry_max_age_ms;
ALTER TABLE audit_events ALTER COLUMN app_id DROP NOT NULL;
-- Operations backlog counts (the predicate in the query must match exactly).
CREATE INDEX webhook_deliveries_unfinished_idx ON webhook_deliveries (state, next_attempt_at)
    WHERE state IN ('queued', 'in_flight', 'recovery_hold');
-- Per-delivery engagement sums in message detail.
CREATE INDEX tracking_links_delivery_idx ON tracking_links (delivery_id);
