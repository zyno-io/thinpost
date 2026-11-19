-- Schema 42: messages submitted on hold (X-Thinpost-Hold: true).
--
-- A held message is accepted with state 'on_hold' instead of 'queued'.
-- hold_expires_at is set exactly when a message was submitted on hold (at
-- acceptance, seven days later); hold_resolved_at records when the hold ended:
-- release (the state moves to 'queued' and the message continues as any other),
-- cancel (state 'cancelled') or expiry (state 'hold_expired'). Both terminal
-- states clear messages.pending. The partial index serves the expiry sweep.
-- webhook_events: the message-level branch of the source CHECK, which admitted
-- only preparation_failed, now also admits held, released, cancelled and
-- hold_expired, and the attempt branch excludes them.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. ALTER TABLE messages takes ACCESS EXCLUSIVE: both ADD COLUMN are
-- nullable without a default (catalog-only, no rewrite) and the three CHECKs
-- are NOT VALID, so no row is read. That is sound because every existing row
-- has NULL in both new columns and a state outside 'on_hold', 'cancelled' and
-- 'hold_expired' (no earlier schema writes them), so it satisfies all three
-- predicates; new and updated rows are checked. CREATE INDEX on messages is a
-- plain build of a partial index that matches no existing row, but it still
-- reads the whole table once. It runs in the same transaction as the ALTER
-- TABLE above, so messages stays under that ACCESS EXCLUSIVE lock until
-- commit (and Migrate already holds runtime_gate ACCESS EXCLUSIVE with the
-- gate closed): every reader and writer of messages waits for the build, so
-- its duration scales with the size of messages. Review that size before
-- deploying, as for migrations 14 and 28. The ALTER TABLE
-- webhook_events replaces the source CHECK with a NOT VALID one, as migration
-- 32 did for its CHECK, so the table is not scanned: no existing row carries
-- any of the four new outcomes, so every row that satisfied the old CHECK
-- satisfies the new one; new rows are checked. No drain is needed beyond the
-- gate close.
-- Rollout: schema-41 pods fail closed on the new schema and are fenced by the
-- closed gate until the reopen waits for every pod to run the new image, so no
-- older pod admits work while an on_hold row exists. Independently of that
-- fence, an older binary could not send a held message: its preparation claim
-- selects only state 'queued', and deliveries (the only rows an SMTP claim
-- reads) are created by preparation, so an on_hold row is never prepared or
-- dispatched. Its pending count includes held rows (pending defaults to true),
-- which is conservative, and its webhook claim cancels, without sending, a
-- queued delivery whose outcome its formats do not know.
-- Grants: the runtime role's table-level SELECT, INSERT, UPDATE on messages and
-- webhook_events cover the new columns: no grant change. No routine or
-- extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
ALTER TABLE messages
    ADD COLUMN hold_expires_at timestamptz,
    ADD COLUMN hold_resolved_at timestamptz,
    ADD CONSTRAINT messages_hold_resolved_needs_hold
        CHECK (hold_resolved_at IS NULL OR hold_expires_at IS NOT NULL) NOT VALID,
    ADD CONSTRAINT messages_hold_states_need_hold
        CHECK (state NOT IN ('on_hold', 'cancelled', 'hold_expired') OR hold_expires_at IS NOT NULL) NOT VALID,
    ADD CONSTRAINT messages_hold_open_unresolved
        CHECK (state <> 'on_hold' OR hold_resolved_at IS NULL) NOT VALID;
CREATE INDEX messages_hold_expiry_idx ON messages (hold_expires_at, id) WHERE state = 'on_hold';
ALTER TABLE webhook_events
    DROP CONSTRAINT webhook_events_exactly_one_source,
    ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome NOT IN ('bounce_received_unverified','preparation_failed','tracking_abandoned','complained','unsubscribed','inbound_received','held','released','cancelled','hold_expired')) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome IN ('preparation_failed','held','released','cancelled','hold_expired') AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NOT NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome IN ('opened','clicked') AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome = 'tracking_abandoned' AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NOT NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome IN ('complained','unsubscribed') AND smtp_code = 0) OR
        (message_id IS NULL AND delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NOT NULL AND inbound_recipient_index IS NOT NULL AND outcome = 'inbound_received' AND smtp_code = 0)
    ) NOT VALID;
