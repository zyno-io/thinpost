-- Operator abandonment is a delivery event with no SMTP attempt. Preserve
-- historical attempt events and their existing constraint behavior.
-- Refuse a historical collision instead of relabeling its meaning.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM webhook_events WHERE outcome = 'tracking_abandoned') THEN
        RAISE EXCEPTION 'historical tracking_abandoned outcome needs review';
    END IF;
END $$;
ALTER TABLE webhook_events
    DROP CONSTRAINT webhook_events_exactly_one_source,
    ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
        (delivery_id IS NOT NULL AND attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome NOT IN ('bounce_received_unverified','preparation_failed','tracking_abandoned')) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND tracking_event_id IS NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0) OR
        (delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome = 'preparation_failed' AND smtp_code = 0) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NOT NULL AND outcome IN ('opened','clicked') AND smtp_code = 0) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome = 'tracking_abandoned' AND smtp_code = 0)
    );
