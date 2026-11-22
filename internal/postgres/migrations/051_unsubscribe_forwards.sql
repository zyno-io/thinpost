-- Schema 51: forward a one-click unsubscribe to the sender's own link.
--
-- When an app has Thinpost one-click unsubscribe on and a message carries its
-- own List-Unsubscribe headers, Thinpost replaces them with its own per-
-- delivery link. If the original had an https link and a List-Unsubscribe-Post
-- header, preparation records a row here, one per delivery, holding only the
-- link's host. The link itself is NOT copied: it can carry a per-recipient
-- token, and it is already in the stored message. When the recipient
-- unsubscribes, the row is queued; a control worker then re-reads the delivery's
-- stored source, re-derives the link, requires its host to equal target_host,
-- and sends the RFC 8058 one-click POST to it, with retries, through the same
-- public-destination checks as webhooks. A row exists only for a delivery whose
-- original message had its own List-Unsubscribe headers:
--   armed         forwardable, the recipient has not unsubscribed yet
--   queued        the recipient unsubscribed; waiting for (the next) attempt
--   in_flight     claimed by a worker (claim_until is the lease)
--   delivered     the sender's endpoint answered 2xx
--   failed        gave up (attempts used, a permanent refusal, the stored
--                 message no longer yields the same host, or the app was deleted)
--   not_forwarded the original could not be forwarded; reason says why
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. The new table and indexes are created empty (catalog-only); no existing
-- table changes. Existing mail is unaffected: no row exists, so an unsubscribe
-- from an older delivery is recorded exactly as before. A replica that predates
-- this release neither writes nor reads the table (its unsubscribe handler does
-- not queue a forward), so deploy the release to every replica.
-- Runtime grants: unsubscribe_forwards gets SELECT, INSERT and UPDATE; the
-- collector and retirement roles get nothing. No routine or extension is
-- installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
CREATE TABLE unsubscribe_forwards (
    delivery_id text PRIMARY KEY REFERENCES deliveries(id),
    state text NOT NULL CHECK (state IN ('armed','queued','in_flight','delivered','failed','not_forwarded')),
    reason text CHECK (reason IN ('mailto_only','no_one_click_post','unusable_link')),
    target_host text CHECK (target_host IS NULL OR octet_length(target_host) BETWEEN 1 AND 253),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 24),
    next_attempt_at timestamptz,
    claim_token text CHECK (claim_token IS NULL OR claim_token ~ '^[0-9a-f]{32}$'),
    claim_until timestamptz,
    last_status integer CHECK (last_status IS NULL OR last_status BETWEEN 100 AND 599),
    last_failure text CHECK (last_failure IS NULL OR last_failure IN
        ('blocked_destination','resolution_failed','connection_failed','tls_failed','timeout','http_status','rejected','invalid_link','target_changed','source_unavailable','app_deleted','unknown')),
    requested_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT unsubscribe_forwards_host CHECK ((state = 'not_forwarded') = (target_host IS NULL)),
    CONSTRAINT unsubscribe_forwards_reason CHECK ((state = 'not_forwarded') = (reason IS NOT NULL)),
    CONSTRAINT unsubscribe_forwards_queued CHECK (state <> 'queued' OR next_attempt_at IS NOT NULL),
    CONSTRAINT unsubscribe_forwards_claim CHECK ((state = 'in_flight') = (claim_token IS NOT NULL AND claim_until IS NOT NULL)),
    CONSTRAINT unsubscribe_forwards_finished CHECK ((state IN ('delivered','failed')) = (finished_at IS NOT NULL) OR state = 'not_forwarded')
);
CREATE INDEX unsubscribe_forwards_due_idx ON unsubscribe_forwards (next_attempt_at) WHERE state = 'queued';
CREATE INDEX unsubscribe_forwards_lease_idx ON unsubscribe_forwards (claim_until) WHERE state = 'in_flight';
