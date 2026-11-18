-- A bounce is an untrusted receipt, never an SMTP delivery result. Each event
-- references exactly one attempt or one receipt on the same delivery.
ALTER TABLE bounce_receipts
    ADD CONSTRAINT bounce_receipts_id_delivery_unique UNIQUE (id, delivery_id);

ALTER TABLE webhook_events
    ALTER COLUMN attempt_id DROP NOT NULL,
    ADD COLUMN bounce_receipt_id text UNIQUE,
    ADD CONSTRAINT webhook_events_bounce_receipt_delivery_fk
        FOREIGN KEY (bounce_receipt_id, delivery_id)
        REFERENCES bounce_receipts (id, delivery_id),
    ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
        (attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND outcome <> 'bounce_received_unverified') OR
        (attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0)
    );
