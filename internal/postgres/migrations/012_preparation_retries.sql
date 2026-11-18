-- A bounded preparation lease counter is retained per submitted message.
ALTER TABLE messages
    ADD COLUMN prepare_attempts integer NOT NULL DEFAULT 0 CHECK (prepare_attempts BETWEEN 0 AND 12),
    ADD COLUMN prepare_next_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN prepare_failure text;

-- Existing claims already consumed one attempt. They must remain recoverable
-- if a pre-upgrade worker exits before publishing its result.
UPDATE messages SET prepare_attempts=1 WHERE state='preparing';

CREATE INDEX messages_preparation_ready_idx ON messages (prepare_next_at, accepted_at, id) WHERE state = 'queued';
CREATE INDEX messages_preparation_expired_idx ON messages (prepare_until, id) WHERE state = 'preparing';

-- A preparation failure belongs to the message; no delivery exists yet.
ALTER TABLE webhook_events
    DROP CONSTRAINT webhook_events_exactly_one_source,
    ALTER COLUMN delivery_id DROP NOT NULL,
    ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
        (delivery_id IS NOT NULL AND attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND outcome NOT IN ('bounce_received_unverified','preparation_failed')) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0) OR
        (delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND outcome = 'preparation_failed' AND smtp_code = 0)
    );
