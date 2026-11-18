-- Earlier authorized attempts may have reached SMTP before journal identities
-- existed. Preserve their unknown identity and treat them as recovery evidence.
ALTER TABLE delivery_attempts
    ADD COLUMN start_journal_id text,
    ADD CONSTRAINT start_journal_identity_valid CHECK (
        start_journal_id IS NULL OR
        (start_authorized AND start_session_id IS NOT NULL AND start_journal_id ~ '^[0-9a-f]{32}$')
    );
