-- Retiring an expired terminal delivery removes its encrypted-object
-- reference without deleting tracking and webhook history. The separately
-- fenced orphan collector can later reclaim the ciphertext.
-- A narrowly scoped maintenance role can lock a delivery without permission
-- to change its state. The guard is inert and can only ever equal zero.
ALTER TABLE deliveries ADD COLUMN tracking_retirement_lock_guard smallint NOT NULL DEFAULT 0
    CHECK (tracking_retirement_lock_guard = 0);

ALTER TABLE tracking_objects
    ALTER COLUMN object_key DROP NOT NULL,
    ADD COLUMN retired_at timestamptz,
    ADD CONSTRAINT tracking_object_retirement_complete CHECK (
        (object_key IS NULL) = (retired_at IS NOT NULL)
    );

CREATE INDEX tracking_objects_unretired_expiry_idx
    ON tracking_objects (expires_at, delivery_id) WHERE object_key IS NOT NULL;

-- A separate maintenance role can retire a mapping, but cannot resurrect it
-- after collection or rewrite its retirement time, even after clock rollback.
CREATE FUNCTION prevent_tracking_mapping_revival() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    delivery_state text;
    owner_app text;
    audit_actor text;
    expected_generation text;
    gate_generation text;
    gate_mode text;
BEGIN
    IF OLD.retired_at IS NOT NULL AND
       (NEW.object_key IS NOT NULL OR NEW.retired_at IS DISTINCT FROM OLD.retired_at) THEN
        RAISE EXCEPTION 'retired tracking mapping cannot be revived';
    END IF;
    IF OLD.retired_at IS NULL AND NEW.retired_at IS NOT NULL THEN
        audit_actor := current_setting('thinpost.retirement_actor', true);
        expected_generation := current_setting('thinpost.retirement_generation', true);
        IF audit_actor IS NULL OR octet_length(audit_actor) < 1 OR octet_length(audit_actor) > 256 OR
           audit_actor <> btrim(audit_actor) OR audit_actor ~ '[[:cntrl:]]' OR
           expected_generation IS NULL OR expected_generation !~ '^[0-9a-f]{32}$' THEN
            RAISE EXCEPTION 'tracking retirement context is missing';
        END IF;
        SELECT generation,mode INTO gate_generation,gate_mode FROM runtime_gate WHERE singleton FOR SHARE;
        IF NOT FOUND OR gate_mode <> 'open' OR gate_generation IS DISTINCT FROM expected_generation THEN
            RAISE EXCEPTION 'tracking retirement gate is closed';
        END IF;
        SELECT d.state,m.app_id INTO delivery_state,owner_app FROM deliveries d
            JOIN messages m ON m.id=d.message_id WHERE d.id=OLD.delivery_id FOR SHARE OF d;
        IF NOT FOUND OR delivery_state NOT IN ('accepted','failed') OR OLD.expires_at>clock_timestamp() THEN
            RAISE EXCEPTION 'tracking mapping is not eligible for retirement';
        END IF;
        INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
            VALUES (replace(pg_catalog.gen_random_uuid()::text,'-',''),audit_actor,owner_app,
                'tracking.mapping.retired',OLD.delivery_id,jsonb_build_object('database_user',session_user));
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER tracking_object_retirement_monotonic
BEFORE UPDATE OF object_key, retired_at ON tracking_objects
FOR EACH ROW EXECUTE FUNCTION prevent_tracking_mapping_revival();
