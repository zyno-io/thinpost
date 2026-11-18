-- A zero limit disables new submissions until an operator configures the app.
-- One fixed UTC-day counter per application keeps quota state bounded.
ALTER TABLE apps ADD COLUMN recipient_daily_limit integer NOT NULL DEFAULT 0 CHECK (recipient_daily_limit BETWEEN 0 AND 1000000);
ALTER TABLE apps ADD COLUMN recipient_quota_day date NOT NULL DEFAULT (clock_timestamp() AT TIME ZONE 'UTC')::date;
ALTER TABLE apps ADD COLUMN recipient_quota_used integer NOT NULL DEFAULT 0 CHECK (recipient_quota_used >= 0);
ALTER TABLE audit_events ADD COLUMN details jsonb NOT NULL DEFAULT '{}'::jsonb;
