CREATE INDEX delivery_attempts_reconciliation_idx
    ON delivery_attempts (worker_id, id) INCLUDE (epoch)
    WHERE state = 'in_flight';
