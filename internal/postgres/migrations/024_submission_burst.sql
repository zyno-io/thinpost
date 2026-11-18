-- A zero pair disables uploads until an operator configures the application.
-- Units are 1/60,000,000 of a message, allowing exact microsecond refill.
ALTER TABLE apps ADD COLUMN submission_rate_per_minute integer NOT NULL DEFAULT 0
  CHECK (submission_rate_per_minute BETWEEN 0 AND 60000);
ALTER TABLE apps ADD COLUMN submission_burst integer NOT NULL DEFAULT 0
  CHECK (submission_burst BETWEEN 0 AND 10000);
ALTER TABLE apps ADD COLUMN submission_tokens bigint NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN submission_refilled_at timestamptz NOT NULL DEFAULT clock_timestamp();
ALTER TABLE apps ADD CONSTRAINT submission_bucket_valid CHECK (
  ((submission_rate_per_minute = 0 AND submission_burst = 0) OR
   (submission_rate_per_minute > 0 AND submission_burst > 0)) AND
  submission_tokens BETWEEN 0 AND submission_burst::bigint * 60000000
);
