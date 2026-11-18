-- Schema 40: named sending IPs for the Edge, with per-app assignment.
--
-- sending_ips is the administrator's record of one outbound address: a slug
-- name (logged, never the address), the IPv4 address, the HELO name the Edge
-- greets with and the bounce domain whose SPF must list the address. Which
-- addresses exist is Helm's decision and which the Edge can bind is reported
-- by the Edge at session start (edge_source_reports); a row names an address,
-- it does not create one. The Edge's primary address (its first declared
-- source) is the default for every app. A row may name the primary; without
-- one the primary uses the Edge hostname and the global bounce domain, exactly
-- as before this schema. apps.sending_ip_id pins an app to a row (NULL = the
-- primary). delivery_attempts.source_address records the address the attempt
-- was asked to send from (NULL = the primary, or an Edge that does not report
-- sources).
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. Both CREATE TABLE statements touch no existing data. ADD COLUMN of a
-- nullable column without a default on apps and delivery_attempts is
-- catalog-only (no rewrite). The foreign key from apps to the new, empty
-- sending_ips validates against an all-NULL column in one scan of the small
-- apps table under the closed gate. CREATE INDEX on apps is a plain build of a
-- partial index with no rows. No drain is needed beyond the gate close.
-- Schema-39 pods fail closed on the new schema and are fenced by the closed
-- gate until the reopen. Existing apps keep sending_ip_id NULL (the primary), so
-- upgrading changes no behavior. Runtime grants: sending_ips gets SELECT, INSERT, UPDATE and
-- DELETE (the console edits and deletes unused rows); edge_source_reports gets
-- SELECT, INSERT and UPDATE; the existing table-level grants on apps and
-- delivery_attempts cover the new columns. The collector and retirement roles
-- get nothing. No routine or extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
CREATE TABLE sending_ips (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$' AND name NOT IN ('primary', 'other', 'removed')),
    address inet NOT NULL UNIQUE CHECK (family(address) = 4 AND masklen(address) = 32),
    helo_name text NOT NULL CHECK (helo_name = lower(helo_name) AND octet_length(helo_name) BETWEEN 3 AND 253),
    bounce_domain text NOT NULL CHECK (bounce_domain = lower(bounce_domain) AND octet_length(bounce_domain) BETWEEN 3 AND 219),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE apps ADD COLUMN sending_ip_id text REFERENCES sending_ips(id);
CREATE INDEX apps_sending_ip_idx ON apps (sending_ip_id) WHERE sending_ip_id IS NOT NULL;
ALTER TABLE delivery_attempts ADD COLUMN source_address inet;
-- The last Ready each worker reported. NULL arrays mean a worker that reports
-- no sources (a legacy Edge). Only the live lease holder may write its row.
CREATE TABLE edge_source_reports (
    worker_id text PRIMARY KEY,
    epoch bigint NOT NULL CHECK (epoch > 0),
    addresses inet[],
    bounce_domains text[],
    reported_at timestamptz NOT NULL,
    CHECK ((addresses IS NULL) = (bounce_domains IS NULL))
);
