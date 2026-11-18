-- Schema 34: SMTP stage and enhanced status of each attempt, for automatic
-- hard-bounce suppression (S5). Both columns are nullable, written only when an
-- Edge reports them with a 4xx/5xx reply, and hold no reply text: the stage is
-- MAIL, RCPT or DATA, and the enhanced code is digits only (RFC 3463).
--
-- Deploy review (lock and rollout): two nullable columns with no default add
-- without a rewrite. Both CHECKs are NOT VALID: every existing row is NULL, new
-- rows are still checked, and delivery_attempts is not scanned. The ALTER takes a
-- brief ACCESS EXCLUSIVE lock under the closed deploy gate; no drain is needed
-- beyond the gate close. delivery_attempts already grants the runtime role
-- table-level UPDATE, which covers the new columns; the runtime role also gains
-- UPDATE (reason, origin, source_message_id) on suppressions so a
-- recipient-originated entry can replace a weaker one (a manual entry becomes
-- hard_bounce). No other privilege changes; collector and retirement roles get
-- nothing. Schema-33 pods fail closed on the new schema (their schema check rejects
-- an unknown migration) and are fenced by the closed gate; schema-34 pods
-- require the new columns.
-- Edge/control protocol: no version bump. The new DeliveryResult fields are
-- additive; see docs/control-protocol.md.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
ALTER TABLE delivery_attempts
    ADD COLUMN smtp_stage text,
    ADD COLUMN smtp_enhanced text,
    ADD CONSTRAINT delivery_attempts_smtp_stage_check
        CHECK (smtp_stage IS NULL OR smtp_stage IN ('MAIL','RCPT','DATA')) NOT VALID,
    ADD CONSTRAINT delivery_attempts_smtp_enhanced_check
        CHECK (smtp_enhanced IS NULL OR smtp_enhanced ~ '^[245]\.[0-9]{1,3}\.[0-9]{1,3}$') NOT VALID;
