-- Schema 31: the From address and display name are recorded with the other
-- operator-visible submission metadata (schema 28), in plaintext, for the
-- console and provider-format webhooks. Earlier rows keep NULL ("not
-- recorded"); nothing is backfilled from encrypted source objects. The columns
-- are nullable with no default, so no table is rewritten, and the added CHECKs
-- are satisfied by every existing row (all NULL); one ALTER TABLE validates
-- them in a single scan of messages, under the closed deploy gate. No routine or extension is
-- installed. sender_address is the bare address as submitted (case preserved);
-- sender_name is the RFC 2047-decoded display name and is NULL when absent.
ALTER TABLE messages
    ADD COLUMN sender_address text,
    ADD COLUMN sender_name text,
    ADD CONSTRAINT messages_sender_address_size
        CHECK (sender_address IS NULL OR octet_length(sender_address) BETWEEN 3 AND 998),
    ADD CONSTRAINT messages_sender_name_size
        CHECK (sender_name IS NULL OR octet_length(sender_name) BETWEEN 1 AND 998),
    ADD CONSTRAINT messages_sender_name_needs_address
        CHECK (sender_name IS NULL OR sender_address IS NOT NULL),
    ADD CONSTRAINT messages_sender_address_needs_domain
        CHECK (sender_address IS NULL OR sender_domain IS NOT NULL);
