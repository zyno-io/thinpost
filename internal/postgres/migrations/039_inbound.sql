-- Schema 39: inbound mail (receiving domains, stored inbound messages, the
-- inbound webhook lane and per-app inbound limits). This is the whole inbound
-- schema; the Edge listener, the control intake and the dispatch lane that use
-- most of it arrive in later releases and need no further migration.
--
-- inbound_domains: a receiving domain has exactly one owning app at a time. A
-- pending claim (state 'pending') may exist for several apps; the unique partial
-- index admits one 'active' or 'disabled' owner per domain, so the first
-- verification wins and a later one fails. mx_status is informational (set by
-- an on-demand lookup), never a gate. The challenge digest is a SHA-256 of the
-- TXT value, as for sender domains.
-- inbound_messages: one row per accepted message. The raw MIME lives in object
-- storage under the inbound/ prefix (object_key); content_expires_at is the
-- logical retention limit enforced by readers. fingerprint is a keyed digest,
-- unique per app, that makes a sender retry a duplicate. No message body is
-- stored here.
-- inbound_recipients: one row per envelope recipient; one webhook event is
-- queued per recipient.
-- inbound_daily: counts per app, day and result (never an address or content).
-- apps: per-app inbound daily and pending limits (defaults 2000 and 500).
-- webhook_events: message_id becomes nullable; inbound_id and
-- inbound_recipient_index reference an inbound_recipients row; the source CHECK
-- gains an 'inbound_received' branch and every older branch now states
-- message_id IS NOT NULL and the inbound columns NULL explicitly.
-- webhook_deliveries: lane ('event' or 'inbound') separates the outbound-event
-- dispatch from the inbound lane; existing rows and every outbound insert are
-- 'event'. A partial queue index on the event lane keeps the outbound claim
-- independent of any inbound backlog.
-- webhook_subscriptions: the format CHECK admits 'thinpost_inbound' and
-- 'postmark_inbound'; 'postmark_inbound' is unsigned like 'postmark' (no secret).
-- Semantics for the four older formats are unchanged.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. The CREATE TABLE and CREATE INDEX statements on new tables take no lock
-- on existing data. The ALTER TABLE webhook_events takes ACCESS EXCLUSIVE and
-- reads the whole table under the closed gate: DROP NOT NULL is catalog-only,
-- the new columns have no default (no rewrite), the foreign key is validated
-- over all-NULL columns and the replaced CHECK is validated, so duration scales
-- with the size of webhook_events (one row per delivery event), as for
-- migrations 018, 022 and 035; review that size before deploying. The unique
-- index on webhook_events is partial and starts empty, but CREATE UNIQUE INDEX
-- still reads the whole table under a SHARE lock (the gate is closed, so no
-- writer waits on it). The ALTER TABLE webhook_deliveries ADD COLUMN ... NOT
-- NULL DEFAULT 'event' is catalog-only; webhook_deliveries is then scanned once
-- to validate the lane CHECK and once more for each of the four partial index
-- builds (CREATE INDEX takes a SHARE lock), all under the closed gate, so
-- duration scales with that table too. ALTER TABLE webhook_subscriptions takes
-- ACCESS EXCLUSIVE and rescans that small table for the replaced CHECKs. ALTER TABLE apps ADD COLUMN with
-- constant defaults needs no rewrite, but its CHECKs scan apps under ACCESS
-- EXCLUSIVE (a small table, so a brief lock). No drain is needed beyond the gate close. Schema-38 pods fail closed on
-- the new schema and are fenced by the closed gate; they never claim or insert
-- an inbound row, and every query that reads webhook_events.message_id joins
-- messages or filters the outbound lane, so nothing they run can see an inbound
-- event.
-- Grants: the runtime role gets SELECT, INSERT, UPDATE and DELETE on
-- inbound_domains (domains are deletable), SELECT, INSERT and UPDATE on
-- inbound_daily, and SELECT and INSERT on inbound_messages and
-- inbound_recipients. The existing table-level grants on webhook_events,
-- webhook_deliveries and apps cover the new columns. The collector role gets
-- column-level SELECT (object_key, content_expires_at) on inbound_messages so a
-- collector never deletes a live inbound object; the retirement role gets
-- nothing. No routine or extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
CREATE TABLE inbound_domains (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    app_id text NOT NULL REFERENCES apps(id),
    domain text NOT NULL CHECK (domain = lower(domain) AND octet_length(domain) BETWEEN 3 AND 253),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','active','disabled')),
    verification_hash bytea CHECK (octet_length(verification_hash) = 32),
    verified_at timestamptz,
    mx_status text CHECK (mx_status IN ('thinpost','mixed','other','none','null_mx','error')),
    mx_checked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT inbound_domains_app_domain_key UNIQUE (app_id, domain),
    CONSTRAINT inbound_domains_verified CHECK ((state = 'pending') = (verified_at IS NULL)),
    CONSTRAINT inbound_domains_pending_hash CHECK (state <> 'pending' OR verification_hash IS NOT NULL),
    CONSTRAINT inbound_domains_mx_checked CHECK ((mx_status IS NULL) = (mx_checked_at IS NULL))
);
CREATE UNIQUE INDEX inbound_domains_owner_idx ON inbound_domains (domain) WHERE state IN ('active','disabled');
CREATE TABLE inbound_messages (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    app_id text NOT NULL REFERENCES apps(id),
    domain text NOT NULL CHECK (domain = lower(domain) AND octet_length(domain) BETWEEN 3 AND 253),
    object_key text NOT NULL UNIQUE,
    size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 0 AND 16777216),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    content_expires_at timestamptz NOT NULL,
    mail_from text CHECK (mail_from IS NULL OR octet_length(mail_from) BETWEEN 1 AND 320),
    header_from text CHECK (header_from IS NULL OR octet_length(header_from) BETWEEN 1 AND 320),
    subject text CHECK (subject IS NULL OR octet_length(subject) <= 1024),
    message_id_header text CHECK (message_id_header IS NULL OR octet_length(message_id_header) <= 998),
    spf_result text NOT NULL DEFAULT 'none' CHECK (spf_result IN ('none','pass','fail','softfail','neutral','temperror','permerror')),
    dkim_result text NOT NULL DEFAULT 'none' CHECK (dkim_result IN ('none','pass','fail','temperror','permerror')),
    dmarc_result text NOT NULL DEFAULT 'none' CHECK (dmarc_result IN ('none','pass','fail','temperror','permerror')),
    correlated_message_id text,
    correlated_delivery_id text REFERENCES deliveries(id),
    correlation_header text CHECK (correlation_header IN ('in_reply_to','references')),
    from_matches_recipient boolean,
    enhanced_protection boolean NOT NULL DEFAULT false,
    client_ip inet,
    helo text CHECK (helo IS NULL OR octet_length(helo) <= 255),
    tls_version text CHECK (tls_version IS NULL OR octet_length(tls_version) <= 32),
    tls_cipher text CHECK (tls_cipher IS NULL OR octet_length(tls_cipher) <= 128),
    receiver text CHECK (receiver IS NULL OR octet_length(receiver) <= 255),
    -- Per-method results: SPF, up to 5 DKIM results, DMARC domain and policy, ARC presence.
    authentication jsonb CHECK (authentication IS NULL OR (jsonb_typeof(authentication) = 'object' AND pg_column_size(authentication) <= 16384)),
    CONSTRAINT inbound_messages_correlated_message_fk FOREIGN KEY (correlated_message_id, app_id) REFERENCES messages(id, app_id),
    CONSTRAINT inbound_messages_app_fingerprint_key UNIQUE (app_id, fingerprint),
    CONSTRAINT inbound_messages_id_app_key UNIQUE (id, app_id),
    CONSTRAINT inbound_messages_correlation CHECK (
        (correlated_message_id IS NULL AND correlated_delivery_id IS NULL AND correlation_header IS NULL AND from_matches_recipient IS NULL) OR
        (correlated_message_id IS NOT NULL AND correlation_header IS NOT NULL))
);
CREATE INDEX inbound_messages_app_time_idx ON inbound_messages (app_id, received_at DESC, id DESC);
CREATE TABLE inbound_recipients (
    inbound_id text NOT NULL,
    recipient_index integer NOT NULL CHECK (recipient_index >= 0),
    app_id text NOT NULL,
    address text NOT NULL CHECK (octet_length(address) BETWEEN 3 AND 320),
    PRIMARY KEY (inbound_id, recipient_index),
    CONSTRAINT inbound_recipients_message_fk FOREIGN KEY (inbound_id, app_id) REFERENCES inbound_messages(id, app_id),
    CONSTRAINT inbound_recipients_with_app_key UNIQUE (inbound_id, recipient_index, app_id)
);
CREATE TABLE inbound_daily (
    app_id text NOT NULL REFERENCES apps(id),
    day date NOT NULL,
    result text NOT NULL CHECK (result IN ('accepted','duplicate','tempfail_routing','tempfail_limit','tempfail_internal','correlated')),
    count integer NOT NULL CHECK (count BETWEEN 0 AND 1000000000),
    PRIMARY KEY (app_id, day, result)
);
ALTER TABLE apps
    ADD COLUMN inbound_daily_limit integer NOT NULL DEFAULT 2000 CHECK (inbound_daily_limit BETWEEN 1 AND 1000000),
    ADD COLUMN inbound_pending_limit integer NOT NULL DEFAULT 500 CHECK (inbound_pending_limit BETWEEN 1 AND 100000);
ALTER TABLE webhook_events
    ALTER COLUMN message_id DROP NOT NULL,
    ADD COLUMN inbound_id text,
    ADD COLUMN inbound_recipient_index integer,
    ADD CONSTRAINT webhook_events_inbound_source_fk FOREIGN KEY (inbound_id, inbound_recipient_index, app_id) REFERENCES inbound_recipients(inbound_id, recipient_index, app_id),
    DROP CONSTRAINT webhook_events_exactly_one_source,
    ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome NOT IN ('bounce_received_unverified','preparation_failed','tracking_abandoned','complained','unsubscribed','inbound_received')) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome = 'preparation_failed' AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NOT NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome IN ('opened','clicked') AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome = 'tracking_abandoned' AND smtp_code = 0) OR
        (message_id IS NOT NULL AND delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NOT NULL AND inbound_id IS NULL AND inbound_recipient_index IS NULL AND outcome IN ('complained','unsubscribed') AND smtp_code = 0) OR
        (message_id IS NULL AND delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND feedback_id IS NULL AND inbound_id IS NOT NULL AND inbound_recipient_index IS NOT NULL AND outcome = 'inbound_received' AND smtp_code = 0)
    );
CREATE UNIQUE INDEX webhook_events_inbound_recipient_idx ON webhook_events (inbound_id, inbound_recipient_index) WHERE inbound_id IS NOT NULL;
ALTER TABLE webhook_deliveries
    ADD COLUMN lane text NOT NULL DEFAULT 'event' CHECK (lane IN ('event','inbound'));
CREATE INDEX webhook_deliveries_event_queue_idx ON webhook_deliveries (next_attempt_at, event_id) WHERE lane = 'event' AND state = 'queued';
CREATE INDEX webhook_deliveries_inbound_queue_idx ON webhook_deliveries (next_attempt_at, event_id) WHERE lane = 'inbound' AND state = 'queued';
CREATE INDEX webhook_deliveries_inbound_claim_idx ON webhook_deliveries (claim_until) WHERE lane = 'inbound' AND state = 'in_flight';
CREATE INDEX webhook_deliveries_inbound_pending_idx ON webhook_deliveries (app_id) WHERE lane = 'inbound' AND state IN ('queued','in_flight','recovery_hold');
ALTER TABLE webhook_subscriptions
    DROP CONSTRAINT webhook_subscriptions_format_check,
    ADD CONSTRAINT webhook_subscriptions_format_check CHECK (format IN ('thinpost', 'postmark', 'resend', 'mailgun', 'thinpost_inbound', 'postmark_inbound')),
    DROP CONSTRAINT webhook_subscriptions_live_secret,
    ADD CONSTRAINT webhook_subscriptions_live_secret
        CHECK (deleted_at IS NOT NULL OR format IN ('postmark', 'postmark_inbound') OR secret_ciphertext IS NOT NULL),
    DROP CONSTRAINT webhook_subscriptions_unsigned_format,
    ADD CONSTRAINT webhook_subscriptions_unsigned_format
        CHECK (format NOT IN ('postmark', 'postmark_inbound') OR secret_ciphertext IS NULL);
