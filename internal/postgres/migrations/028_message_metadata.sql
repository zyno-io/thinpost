-- Schema 28: operator-visible submission metadata. New submissions record the
-- subject, sender domain and envelope recipient addresses in the submission
-- transaction. Earlier rows keep NULL metadata and no recipient rows ("not
-- recorded"); nothing is backfilled from encrypted source objects. Only core
-- operator classes and immutable pg_catalog functions are used: extension
-- routines are PUBLIC-executable and fail the collector and retirement
-- privilege audits.
--
-- messages.require_tls is renamed enhanced_protection (the per-message
-- "Enhanced Protection" policy; semantics are unchanged).
--
-- *_rev columns hold the domain with its labels reversed plus a trailing dot
-- (mail.example.com -> com.example.mail.), so "this domain or any subdomain"
-- is one indexable prefix: LIKE 'com.example.%'.
ALTER TABLE messages RENAME COLUMN require_tls TO enhanced_protection;
ALTER TABLE messages ADD COLUMN subject text;
ALTER TABLE messages ADD COLUMN recipient_count integer;
ALTER TABLE messages ADD COLUMN sender_domain text;
ALTER TABLE messages ADD COLUMN sender_domain_rev text;
ALTER TABLE messages ADD CONSTRAINT messages_metadata_recorded CHECK (
    (subject IS NULL) = (recipient_count IS NULL)
    AND (subject IS NULL) = (sender_domain IS NULL)
    AND (sender_domain IS NULL) = (sender_domain_rev IS NULL));
ALTER TABLE messages ADD CONSTRAINT messages_subject_size
    CHECK (subject IS NULL OR octet_length(subject) BETWEEN 1 AND 998);
ALTER TABLE messages ADD CONSTRAINT messages_recipient_count_range
    CHECK (recipient_count IS NULL OR recipient_count BETWEEN 1 AND 100);
ALTER TABLE messages ADD CONSTRAINT messages_sender_domain_size
    CHECK (sender_domain IS NULL OR (octet_length(sender_domain) BETWEEN 1 AND 253
        AND octet_length(sender_domain_rev) = octet_length(sender_domain) + 1));
ALTER TABLE messages ADD CONSTRAINT messages_id_app_key UNIQUE (id, app_id);

CREATE TABLE message_recipients (
    message_id text NOT NULL,
    app_id text NOT NULL,
    recipient_index integer NOT NULL CHECK (recipient_index BETWEEN 0 AND 99),
    address text NOT NULL CHECK (octet_length(address) BETWEEN 3 AND 998),
    address_lower text NOT NULL CHECK (octet_length(address_lower) BETWEEN 3 AND 998),
    domain_lower text NOT NULL CHECK (octet_length(domain_lower) BETWEEN 1 AND 998),
    domain_rev text NOT NULL CHECK (octet_length(domain_rev) = octet_length(domain_lower) + 1),
    PRIMARY KEY (message_id, recipient_index),
    FOREIGN KEY (message_id, app_id) REFERENCES messages(id, app_id)
);
CREATE INDEX message_recipients_address_idx ON message_recipients (app_id, address_lower text_pattern_ops);
CREATE INDEX message_recipients_domain_idx ON message_recipients (app_id, domain_lower text_pattern_ops);
CREATE INDEX message_recipients_domain_rev_idx ON message_recipients (app_id, domain_rev text_pattern_ops);
CREATE INDEX messages_sender_domain_idx ON messages (app_id, sender_domain_rev text_pattern_ops)
    WHERE sender_domain_rev IS NOT NULL;
CREATE INDEX messages_app_id_prefix_idx ON messages (app_id, id text_pattern_ops);
CREATE INDEX deliveries_id_prefix_idx ON deliveries (id text_pattern_ops);
CREATE INDEX messages_subject_search_idx ON messages
    USING gin (to_tsvector('simple'::regconfig, regexp_replace(subject, '[^[:alnum:]]+', ' ', 'g')))
    WHERE subject IS NOT NULL AND NOT enhanced_protection;
