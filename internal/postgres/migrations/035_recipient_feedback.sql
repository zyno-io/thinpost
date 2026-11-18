-- Schema 35: recipient feedback (U2 one-click unsubscribe now, complaints later)
-- and the per-app one-click opt-in.
--
-- recipient_feedback records at most one unsubscribe (and, later, one spam
-- complaint) per delivery. It holds IDs, kind, channel and time only: never an
-- address, report body or request body. The suppressed address stays in
-- message_recipients and suppressions.
--
-- webhook_events gains feedback_id, a unique reference to the feedback row, and
-- the source constraint gains a sixth branch for the outcomes 'complained' and
-- 'unsubscribed' (a complaint outcome is admitted here so the later complaint
-- migration needs no further constraint swap). Branch one excludes both
-- outcomes. As in migration 022, a historical row with either outcome would
-- change meaning, so the migration refuses to run over one.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. CREATE TABLE touches no existing data. The ALTER TABLE webhook_events
-- takes ACCESS EXCLUSIVE and reads the whole table more than once under the
-- closed gate: the new UNIQUE index build, validation of the foreign key (every
-- feedback_id is NULL) and validation of the replaced CHECK. webhook_events
-- holds one row per delivery event, so duration scales with its size, as for
-- migrations 018 and 022; review that size before deploying. The new column has
-- no default (no rewrite). ALTER TABLE apps ADD COLUMN ... DEFAULT false NOT
-- NULL is a catalog-only change. No drain is needed beyond the gate close.
-- Schema-34 pods fail closed on the new schema and are fenced by the closed gate;
-- schema-35 pods require the new table and column. The runtime role gets SELECT
-- and INSERT on recipient_feedback (the UPDATE on suppressions that
-- precedence upserts need was granted with schema 34); collector and retirement
-- roles get nothing.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
CREATE TABLE recipient_feedback (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    delivery_id text NOT NULL REFERENCES deliveries(id),
    kind text NOT NULL CHECK (kind IN ('spam_complaint','unsubscribe')),
    channel text NOT NULL CHECK (channel IN ('arf','one_click')),
    reporter_domain text CHECK (reporter_domain IS NULL OR octet_length(reporter_domain) BETWEEN 1 AND 253),
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (delivery_id, kind),
    UNIQUE (id, delivery_id)
);
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM webhook_events WHERE outcome IN ('complained','unsubscribed')) THEN
        RAISE EXCEPTION 'historical feedback outcome needs review';
    END IF;
END $$;
ALTER TABLE webhook_events
    ADD COLUMN feedback_id text UNIQUE,
    ADD CONSTRAINT webhook_events_feedback_source_fk FOREIGN KEY (feedback_id, delivery_id) REFERENCES recipient_feedback(id, delivery_id),
    DROP CONSTRAINT webhook_events_exactly_one_source,
    ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
        (delivery_id IS NOT NULL AND attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND outcome NOT IN ('bounce_received_unverified','preparation_failed','tracking_abandoned','complained','unsubscribed')) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0) OR
        (delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND outcome = 'preparation_failed' AND smtp_code = 0) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NOT NULL AND feedback_id IS NULL AND outcome IN ('opened','clicked') AND smtp_code = 0) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND outcome = 'tracking_abandoned' AND smtp_code = 0) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NOT NULL AND outcome IN ('complained','unsubscribed') AND smtp_code = 0)
    );
ALTER TABLE apps ADD COLUMN list_unsubscribe boolean NOT NULL DEFAULT false;
