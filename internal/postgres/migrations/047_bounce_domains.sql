-- Schema 47: custom bounce (Return-Path) domains.
--
-- An app can give a verified sender domain its own bounce domain, a strict
-- subdomain of it (bounces.example.com) that is a CNAME to the HELO name of the
-- app's sending IP. Control checks that CNAME and, at claim time only, puts the
-- domain in the Return-Path when every live condition holds (the domain is
-- active, was checked recently, still points at the HELO name of the address
-- the mail leaves from, and is not a receiving domain). Otherwise the HELO name
-- is used, exactly as before. A domain may be held by several apps, as for
-- sender_domains: (app_id, domain) is unique, not domain.
-- The state machine is pending -> active <-> failing. A check in flight is
-- check_until in the future (a lease), not a state. check_token is the lease
-- holder's token: a result is written only if the token still matches, and a
-- manual check takes a fresh token, so a stale in-flight check is dropped.
-- delivery_attempts.return_path_domain records which custom domain an attempt
-- used (NULL: the HELO name) for support; it is not a log field.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. The new table and indexes are created empty (catalog-only). The new
-- delivery_attempts column is nullable with no default (catalog-only, no
-- rewrite) and its CHECK is NOT VALID, so no table scan runs; new rows are
-- checked. Existing mail is unaffected: no row exists, so every claim uses the
-- HELO name until a customer adds a domain.
-- Runtime grants: bounce_domains gets SELECT, INSERT, UPDATE and DELETE; the
-- collector and retirement roles get nothing. delivery_attempts already grants
-- INSERT and UPDATE to the runtime role. No routine or extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
CREATE TABLE bounce_domains (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    app_id text NOT NULL,
    sender_domain text NOT NULL,
    domain text NOT NULL CHECK (domain = lower(domain) AND octet_length(domain) BETWEEN 4 AND 219
        AND octet_length(domain) > octet_length(sender_domain) + 1
        AND right(domain, octet_length(sender_domain) + 1) = '.' || sender_domain),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','active','failing')),
    target text,
    last_result text CHECK (last_result IN ('ok','missing','not_cname','other_ip','wrong_target','lookup_failed','no_sending_ip')),
    observed_target text CHECK (observed_target IS NULL OR octet_length(observed_target) <= 253),
    checked_at timestamptz,
    last_ok_at timestamptz,
    verified_at timestamptz,
    failures integer NOT NULL DEFAULT 0 CHECK (failures >= 0),
    next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    check_token text CHECK (check_token IS NULL OR check_token ~ '^[0-9a-f]{32}$'),
    check_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT bounce_domains_sender_fk FOREIGN KEY (app_id, sender_domain) REFERENCES sender_domains(app_id, domain),
    CONSTRAINT bounce_domains_app_domain_key UNIQUE (app_id, domain),
    CONSTRAINT bounce_domains_app_sender_key UNIQUE (app_id, sender_domain),
    CONSTRAINT bounce_domains_ok CHECK (state = 'pending' OR (target IS NOT NULL AND verified_at IS NOT NULL AND last_ok_at IS NOT NULL)),
    CONSTRAINT bounce_domains_checked CHECK ((last_result IS NULL) = (checked_at IS NULL))
);
CREATE INDEX bounce_domains_due_idx ON bounce_domains (next_check_at);
CREATE INDEX bounce_domains_domain_idx ON bounce_domains (domain);
ALTER TABLE delivery_attempts ADD COLUMN return_path_domain text,
    ADD CONSTRAINT delivery_attempts_return_path_domain_check
        CHECK (return_path_domain IS NULL OR octet_length(return_path_domain) BETWEEN 4 AND 219) NOT VALID;
