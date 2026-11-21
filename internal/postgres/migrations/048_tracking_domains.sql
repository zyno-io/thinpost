-- Schema 48: custom tracking domains.
--
-- An app can give a verified sender domain its own tracking domain, a strict
-- subdomain of it (links.example.com) that routes to this deployment's
-- tracking handler. New links in that app's mail use https://<domain> in place
-- of the deployment's tracking origin, but only while the domain is active and
-- was checked recently, and only when this deployment has custom domains
-- enabled (THINPOST_ADMIN_ORIGIN is set). Otherwise links use the global
-- tracking origin, exactly as before. A domain may be held by several apps, as
-- for sender_domains: (app_id, domain) is unique, not domain.
--
-- Activation rests on the HTTPS check alone: an HTTPS request to the domain,
-- with normal certificate validation, must return a proof only this deployment
-- can make. The DNS check (is the name a CNAME to the tracking host?) is
-- recorded for people to read and never blocks activation, so a proxied name
-- (a CDN in front) works. Tracking links are not bound to any hostname, so a
-- link sent earlier opens at any name that still routes to this deployment.
-- The state machine is pending -> active <-> failing, and removed is a
-- tombstone: it is never used for links, but it is still checked daily and
-- listed for operators, because the route and certificate must stay while old
-- links matter. Adding the same name again revives the row as pending. A check
-- in flight is check_until in the future (a lease), not a state; check_token is
-- the lease holder's token and a result is written only if it still matches.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. The new table and indexes are created empty (catalog-only); no existing
-- table changes. Existing mail is unaffected: no row exists, so every message
-- uses the global tracking origin until a customer adds a domain.
-- Runtime grants: tracking_domains gets SELECT, INSERT, UPDATE and DELETE; the
-- collector and retirement roles get nothing. No routine or extension is
-- installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
CREATE TABLE tracking_domains (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    app_id text NOT NULL,
    sender_domain text NOT NULL,
    domain text NOT NULL CHECK (domain = lower(domain) AND octet_length(domain) BETWEEN 4 AND 253
        AND octet_length(domain) > octet_length(sender_domain) + 1
        AND right(domain, octet_length(sender_domain) + 1) = '.' || sender_domain),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','active','failing','removed')),
    dns_result text CHECK (dns_result IN ('ok','missing','wrong_target','lookup_failed')),
    https_result text CHECK (https_result IN ('ok','unreachable','certificate','redirected','not_thinpost','not_found')),
    observed_target text CHECK (observed_target IS NULL OR octet_length(observed_target) <= 253),
    certificate_expires_at timestamptz,
    checked_at timestamptz,
    last_ok_at timestamptz,
    verified_at timestamptz,
    removed_at timestamptz,
    failures integer NOT NULL DEFAULT 0 CHECK (failures >= 0),
    next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    check_token text CHECK (check_token IS NULL OR check_token ~ '^[0-9a-f]{32}$'),
    check_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT tracking_domains_sender_fk FOREIGN KEY (app_id, sender_domain) REFERENCES sender_domains(app_id, domain),
    CONSTRAINT tracking_domains_app_domain_key UNIQUE (app_id, domain),
    CONSTRAINT tracking_domains_ok CHECK (state IN ('pending','removed') OR (verified_at IS NOT NULL AND last_ok_at IS NOT NULL)),
    CONSTRAINT tracking_domains_removed CHECK ((state = 'removed') = (removed_at IS NOT NULL)),
    CONSTRAINT tracking_domains_checked CHECK ((dns_result IS NULL) = (checked_at IS NULL) AND (https_result IS NULL) = (checked_at IS NULL))
);
-- One live (not removed) tracking domain per sender domain. Removed rows are
-- tombstones and may be several.
CREATE UNIQUE INDEX tracking_domains_app_sender_key ON tracking_domains (app_id, sender_domain) WHERE state <> 'removed';
CREATE INDEX tracking_domains_due_idx ON tracking_domains (next_check_at);
CREATE INDEX tracking_domains_domain_idx ON tracking_domains (domain);
