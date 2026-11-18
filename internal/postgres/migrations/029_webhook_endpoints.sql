-- Webhook endpoints are created and edited through the operator API. A live
-- endpoint stores its signing secret sealed with the object keyring; the
-- catalog secret reference is retired. Catalog-era rows have no stored secret
-- and can never be delivered again, so they are retired here together with
-- their pending deliveries. Delivered and failed history is kept; recovery
-- holds are left to the recovery tooling.
ALTER TABLE webhook_subscriptions
    ADD COLUMN description text NOT NULL DEFAULT '' CHECK (octet_length(description) <= 256),
    ADD COLUMN outcomes text[] CHECK (outcomes IS NULL OR cardinality(outcomes) BETWEEN 1 AND 32),
    ADD COLUMN secret_ciphertext bytea CHECK (octet_length(secret_ciphertext) BETWEEN 1 AND 1024),
    ADD COLUMN secret_created_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN created_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN deleted_at timestamptz;

UPDATE webhook_attempts a SET state='unknown', finished_at=clock_timestamp()
    FROM webhook_deliveries w
    WHERE a.id=w.claim_token AND a.state='in_flight' AND w.state='in_flight';
UPDATE webhook_deliveries SET state='cancelled', claim_token=NULL, claim_until=NULL
    WHERE state IN ('queued','in_flight');
UPDATE webhook_subscriptions SET active=false, deleted_at=now();

ALTER TABLE webhook_subscriptions
    DROP COLUMN secret_ref,
    ADD CONSTRAINT webhook_subscriptions_live_secret CHECK (deleted_at IS NOT NULL OR secret_ciphertext IS NOT NULL),
    ADD CONSTRAINT webhook_subscriptions_deleted_inactive CHECK (deleted_at IS NULL OR NOT active);
