-- Null disables collection. Only a separately authorized maintenance identity
-- may advance the horizon, and only after old writers have been fenced.
ALTER TABLE runtime_gate ADD COLUMN object_gc_horizon timestamptz
    CHECK (object_gc_horizon IS NULL OR isfinite(object_gc_horizon));

CREATE FUNCTION prevent_object_gc_horizon_rewind() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.object_gc_horizon IS NOT NULL AND
       (NEW.object_gc_horizon IS NULL OR NEW.object_gc_horizon < OLD.object_gc_horizon) THEN
        RAISE EXCEPTION 'object collection horizon cannot move backwards';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER runtime_gate_object_gc_horizon_monotonic
BEFORE UPDATE OF object_gc_horizon ON runtime_gate
FOR EACH ROW EXECUTE FUNCTION prevent_object_gc_horizon_rewind();
