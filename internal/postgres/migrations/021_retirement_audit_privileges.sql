-- Only the schema owner writes retirement audits. The retirement login must
-- not receive general INSERT on audit_events. The trigger fixes search_path to
-- pg_catalog and quotes its own table's schema when accessing application data.
CREATE OR REPLACE FUNCTION prevent_tracking_mapping_revival() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
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
        EXECUTE format('SELECT generation,mode FROM %I.runtime_gate WHERE singleton FOR SHARE', TG_TABLE_SCHEMA)
            INTO gate_generation,gate_mode;
        IF gate_mode IS DISTINCT FROM 'open' OR gate_generation IS DISTINCT FROM expected_generation THEN
            RAISE EXCEPTION 'tracking retirement gate is closed';
        END IF;
        EXECUTE format('SELECT d.state,m.app_id FROM %I.deliveries d JOIN %I.messages m ON m.id=d.message_id WHERE d.id=$1 FOR SHARE OF d', TG_TABLE_SCHEMA, TG_TABLE_SCHEMA)
            INTO delivery_state,owner_app USING OLD.delivery_id;
        IF delivery_state IS NULL OR delivery_state NOT IN ('accepted','failed') OR OLD.expires_at>clock_timestamp() THEN
            RAISE EXCEPTION 'tracking mapping is not eligible for retirement';
        END IF;
        EXECUTE format('INSERT INTO %I.audit_events(id,actor_id,app_id,action,resource_id,details) VALUES (replace(pg_catalog.gen_random_uuid()::text,''-'',''''),$1,$2,''tracking.mapping.retired'',$3,jsonb_build_object(''database_user'',session_user))', TG_TABLE_SCHEMA)
            USING audit_actor,owner_app,OLD.delivery_id;
    END IF;
    RETURN NEW;
END
$$;

REVOKE ALL ON FUNCTION prevent_tracking_mapping_revival() FROM PUBLIC;
