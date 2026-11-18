-- Inbound SMTP is untrusted. A receipt alone never changes delivery status.
CREATE TABLE bounce_receipts (
    id text PRIMARY KEY,
    delivery_id text NOT NULL REFERENCES deliveries(id),
    object_key text NOT NULL UNIQUE,
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (delivery_id, fingerprint)
);
