-- A single bounded counter per app limits event and webhook amplification
-- across replicas. Zero disables event recording until an operator sets a
-- limit; links still resolve. No recipient or request data is retained here.
ALTER TABLE apps ADD COLUMN tracking_daily_limit integer NOT NULL DEFAULT 0
    CHECK (tracking_daily_limit BETWEEN 0 AND 1000000);
ALTER TABLE apps ADD COLUMN tracking_quota_day date NOT NULL DEFAULT (clock_timestamp() AT TIME ZONE 'UTC')::date;
ALTER TABLE apps ADD COLUMN tracking_quota_used integer NOT NULL DEFAULT 0
    CHECK (tracking_quota_used BETWEEN 0 AND 1000000);
ALTER TABLE apps ADD COLUMN tracking_quota_suppressed integer NOT NULL DEFAULT 0
    CHECK (tracking_quota_suppressed BETWEEN 0 AND 1000000000);
