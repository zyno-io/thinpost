-- Schema 46: Thinpost generates and holds each sender domain's DKIM key.
--
-- Control generates an RSA-2048 key per app and sender domain, seals the
-- private half under the object encryption keyring (the AAD binds the row's
-- id, app, domain and selector) and publishes nothing itself: the customer
-- puts the public half in DNS, and finding that record is what verifies the
-- domain. The signing catalog and its mounted key files are gone; keys exist
-- only here. A domain verified before this schema has no key until one is
-- created and its record verified, and mail from it is refused until then.
-- At most one active and one pending (rotation) key exist per app and domain.
-- Switching retires the old key and shreds its private half (the shredded
-- CHECK), keeping its row so its selector is never reused and its public
-- record can stay in DNS until keep_record_until, for mail still queued.
-- Selectors (thinpostN) are unique per domain across apps: two apps on one
-- domain must not publish at the same name.
-- The _thinpost TXT challenge no longer verifies a sender domain, so any
-- pending challenge is dropped; those domains get a DKIM record to publish.
-- Inbound domains keep their own _thinpost challenge.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. The new table and its indexes are created empty (catalog-only).
-- Dropping sender_domains.verification_hash is catalog-only (no rewrite, no
-- scan) and sender_domains is tiny; already-verified domains stay verified.
-- Runtime grants: dkim_keys gets SELECT, INSERT, UPDATE and DELETE (DELETE only
-- removes a cancelled pending key); the collector and retirement roles get
-- nothing. No routine or extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
CREATE TABLE dkim_keys (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    app_id text NOT NULL,
    domain text NOT NULL,
    selector text NOT NULL CHECK (selector ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    public_key bytea NOT NULL CHECK (octet_length(public_key) BETWEEN 200 AND 1200),
    sealed_private_key bytea CHECK (sealed_private_key IS NULL OR octet_length(sealed_private_key) BETWEEN 64 AND 16384),
    state text NOT NULL CHECK (state IN ('pending','active','retired')),
    published_at timestamptz,
    activated_at timestamptz,
    retired_at timestamptz,
    keep_record_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT dkim_keys_domain_fk FOREIGN KEY (app_id, domain) REFERENCES sender_domains(app_id, domain),
    CONSTRAINT dkim_keys_shredded CHECK ((state = 'retired') = (sealed_private_key IS NULL)),
    CONSTRAINT dkim_keys_activated CHECK (state = 'pending' OR activated_at IS NOT NULL),
    CONSTRAINT dkim_keys_retired CHECK (state <> 'retired' OR (retired_at IS NOT NULL AND keep_record_until IS NOT NULL))
);
CREATE UNIQUE INDEX dkim_keys_active_idx ON dkim_keys (app_id, domain) WHERE state = 'active';
CREATE UNIQUE INDEX dkim_keys_pending_idx ON dkim_keys (app_id, domain) WHERE state = 'pending';
CREATE UNIQUE INDEX dkim_keys_selector_idx ON dkim_keys (domain, selector);
ALTER TABLE sender_domains DROP COLUMN verification_hash;
