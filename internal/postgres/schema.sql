-- Initial development schema. Apply through a versioned migration system before deployment.
CREATE TABLE apps (
    id text PRIMARY KEY,
    name text NOT NULL,
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE api_keys (
    id text PRIMARY KEY,
    digest bytea NOT NULL UNIQUE CHECK (octet_length(digest) = 32),
    app_id text NOT NULL REFERENCES apps(id),
    name text NOT NULL DEFAULT 'API key',
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE audit_events (
    id text PRIMARY KEY,
    actor_id text NOT NULL,
    app_id text NOT NULL REFERENCES apps(id),
    action text NOT NULL,
    resource_id text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sender_domains (
    app_id text NOT NULL REFERENCES apps(id),
    domain text NOT NULL CHECK (domain = lower(domain)),
    active boolean NOT NULL DEFAULT true,
    PRIMARY KEY (app_id, domain)
);
CREATE TABLE messages (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES apps(id),
    object_key text NOT NULL UNIQUE,
    require_tls boolean NOT NULL,
    state text NOT NULL DEFAULT 'queued',
    prepare_token text,
    prepare_until timestamptz,
    accepted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_queue_idx ON messages (accepted_at) WHERE state = 'queued';
CREATE INDEX messages_app_history_idx ON messages (app_id, accepted_at DESC, id DESC);
CREATE TABLE deliveries (
    id text PRIMARY KEY,
    message_id text NOT NULL REFERENCES messages(id),
    recipient_index integer NOT NULL CHECK (recipient_index >= 0),
    final_object_key text NOT NULL UNIQUE,
    state text NOT NULL DEFAULT 'queued',
    current_attempt_id text,
    retry_after timestamptz NOT NULL DEFAULT now(),
    UNIQUE (message_id, recipient_index)
);
CREATE INDEX deliveries_queue_idx ON deliveries (id) WHERE state = 'queued';
CREATE TABLE delivery_attempts (
    id text PRIMARY KEY,
    delivery_id text NOT NULL REFERENCES deliveries(id),
    worker_id text NOT NULL,
    epoch bigint NOT NULL CHECK (epoch > 0),
    state text NOT NULL DEFAULT 'in_flight',
    outcome text,
    smtp_code integer,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX delivery_attempts_delivery_idx ON delivery_attempts (delivery_id, started_at);
CREATE TABLE webhook_subscriptions (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES apps(id),
    endpoint_url text NOT NULL,
    secret_ref text NOT NULL,
    active boolean NOT NULL DEFAULT true,
    UNIQUE (id, app_id)
);
CREATE INDEX webhook_subscriptions_app_idx ON webhook_subscriptions (app_id) WHERE active;
CREATE TABLE webhook_events (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES apps(id),
    message_id text NOT NULL REFERENCES messages(id),
    delivery_id text NOT NULL REFERENCES deliveries(id),
    attempt_id text NOT NULL UNIQUE REFERENCES delivery_attempts(id),
    outcome text NOT NULL,
    smtp_code integer NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, app_id)
);
CREATE TABLE webhook_deliveries (
    event_id text NOT NULL,
    subscription_id text NOT NULL,
    app_id text NOT NULL,
    state text NOT NULL DEFAULT 'queued',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claim_token text,
    claim_until timestamptz,
    delivered_at timestamptz,
    PRIMARY KEY (event_id, subscription_id),
    FOREIGN KEY (event_id, app_id) REFERENCES webhook_events(id, app_id),
    FOREIGN KEY (subscription_id, app_id) REFERENCES webhook_subscriptions(id, app_id)
);
CREATE INDEX webhook_deliveries_queue_idx ON webhook_deliveries (next_attempt_at) WHERE state = 'queued';
CREATE TABLE webhook_attempts (
    id text PRIMARY KEY,
    event_id text NOT NULL,
    subscription_id text NOT NULL,
    state text NOT NULL DEFAULT 'in_flight',
    http_status integer,
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    FOREIGN KEY (event_id, subscription_id) REFERENCES webhook_deliveries(event_id, subscription_id)
);
CREATE TABLE idempotency_keys (
    app_id text NOT NULL REFERENCES apps(id),
    adapter text NOT NULL,
    key text NOT NULL,
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    message_id text NOT NULL REFERENCES messages(id) DEFERRABLE INITIALLY DEFERRED,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (app_id, adapter, key)
);
CREATE INDEX idempotency_expiry_idx ON idempotency_keys (expires_at);
CREATE TABLE controller_leases (
    worker_id text PRIMARY KEY,
    controller_id text NOT NULL,
    epoch bigint NOT NULL CHECK (epoch > 0),
    expires_at timestamptz NOT NULL
);
