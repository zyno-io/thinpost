CREATE TABLE runtime_gate (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    -- Runtime may be granted UPDATE on this inert column solely to take
    -- PostgreSQL row-share locks, without permission to change gate state.
    lock_guard smallint NOT NULL DEFAULT 0 CHECK (lock_guard = 0),
    generation text,
    mode text NOT NULL DEFAULT 'closed' CHECK (mode IN ('closed','recovery','open')),
    recovery_id text,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    quarantine_complete boolean NOT NULL DEFAULT false,
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by text
);
INSERT INTO runtime_gate(singleton) VALUES (true);

CREATE TABLE recovery_holds (
    recovery_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('message','delivery','webhook')),
    entity_id text NOT NULL,
    secondary_id text NOT NULL DEFAULT '',
    original_state text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (recovery_id,kind,entity_id,secondary_id)
);

CREATE TABLE recovery_events (
    id text PRIMARY KEY,
    recovery_id text NOT NULL,
    generation text NOT NULL,
    action text NOT NULL,
    actor text NOT NULL,
    revision bigint NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
