-- One encrypted mapping object is shared by a recipient's tracking links.
-- Expiry and event budget are explicit inputs, not an implicit retention policy.
CREATE TABLE tracking_objects (
    delivery_id text PRIMARY KEY REFERENCES deliveries(id) CHECK (delivery_id ~ '^[0-9a-f]{32}$'),
    object_key text NOT NULL UNIQUE CHECK (object_key ~ '^tracking/v1/[0-9]{10}/[0-9a-f]{32}$' AND right(object_key, 32) = delivery_id),
    expires_at timestamptz NOT NULL,
    event_limit integer NOT NULL CHECK (event_limit BETWEEN 1 AND 1000),
    -- Runtime uses this inert update privilege only to acquire row locks.
    lock_guard smallint NOT NULL DEFAULT 0 CHECK (lock_guard = 0),
    revoked_at timestamptz
);
CREATE INDEX tracking_objects_expiry_idx ON tracking_objects (expires_at, delivery_id);

-- Only hashes of random bearer tokens are stored in PostgreSQL. The link ID
-- is a separate opaque identifier suitable for app-scoped event correlation.
CREATE TABLE tracking_links (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    delivery_id text NOT NULL REFERENCES tracking_objects(delivery_id),
    kind text NOT NULL CHECK (kind IN ('open', 'click')),
    event_count integer NOT NULL DEFAULT 0 CHECK (event_count BETWEEN 0 AND 1000),
    UNIQUE (id, delivery_id, kind)
);
CREATE UNIQUE INDEX tracking_links_single_open_idx ON tracking_links (delivery_id) WHERE kind = 'open';

CREATE TABLE tracking_events (
    id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
    link_id text NOT NULL,
    delivery_id text NOT NULL,
    kind text NOT NULL,
    outcome text NOT NULL,
    occurred_at timestamptz NOT NULL,
    CHECK ((kind = 'open' AND outcome = 'opened') OR (kind = 'click' AND outcome = 'clicked')),
    UNIQUE (id, delivery_id, outcome),
    FOREIGN KEY (link_id, delivery_id, kind) REFERENCES tracking_links(id, delivery_id, kind)
);
CREATE INDEX tracking_events_link_time_idx ON tracking_events (link_id, occurred_at, id);

ALTER TABLE webhook_events
    ADD COLUMN tracking_event_id text UNIQUE,
    ADD CONSTRAINT webhook_events_tracking_source_fk
        FOREIGN KEY (tracking_event_id, delivery_id, outcome)
        REFERENCES tracking_events(id, delivery_id, outcome),
    DROP CONSTRAINT webhook_events_exactly_one_source,
    ADD CONSTRAINT webhook_events_exactly_one_source CHECK (
        (delivery_id IS NOT NULL AND attempt_id IS NOT NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome NOT IN ('bounce_received_unverified','preparation_failed')) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NOT NULL AND tracking_event_id IS NULL AND outcome = 'bounce_received_unverified' AND smtp_code = 0) OR
        (delivery_id IS NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NULL AND outcome = 'preparation_failed' AND smtp_code = 0) OR
        (delivery_id IS NOT NULL AND attempt_id IS NULL AND bounce_receipt_id IS NULL AND tracking_event_id IS NOT NULL AND outcome IN ('opened','clicked') AND smtp_code = 0)
    );
