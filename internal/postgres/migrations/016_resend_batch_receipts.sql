-- The existing bounded idempotency marker owns the ordered batch response.
-- NULL retains the original single-message marker format. The marker's 24-hour
-- cleanup also removes its receipt list; no separate unbounded inbox is used.
ALTER TABLE idempotency_keys ADD COLUMN batch_receipt_ids jsonb;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_batch_receipt_shape
    CHECK (batch_receipt_ids IS NULL OR
        (jsonb_typeof(batch_receipt_ids) = 'array' AND jsonb_array_length(batch_receipt_ids) BETWEEN 1 AND 100
         AND octet_length(batch_receipt_ids::text) <= 4000));
