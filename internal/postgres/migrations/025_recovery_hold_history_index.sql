-- Bound lookup by held entity and page deterministically by recovery ID.
-- Existing hold records and their original states are not modified.
CREATE INDEX recovery_hold_history_idx ON recovery_holds (kind, entity_id, secondary_id, recovery_id);
