-- Schema 50: working default limits for new apps.
--
-- A new app used to start with every limit at zero, which refuses all
-- messages until an operator sets limits. New apps now start at: 60 messages
-- per minute with a burst of 100, 10,000 recipients per day, 1,000 pending
-- messages and 100,000 tracking events per day. The submission token bucket
-- starts full (100 x 60,000,000 token units) so the first messages are not
-- refused; it satisfies submission_bucket_valid with the burst default.
-- Zero still blocks all new messages and remains settable.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. ALTER COLUMN SET DEFAULT is catalog-only (no rewrite, no table scan)
-- and takes a brief ACCESS EXCLUSIVE lock on apps. Existing apps are NOT
-- changed: a zero may be an intentional block, so there is no backfill. A
-- replica that predates this release creates apps through the same defaults,
-- which are server-side, so mixed-version creation behaves the same.
-- Runtime grants: no privilege changes. No routine or extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
ALTER TABLE apps
  ALTER COLUMN submission_rate_per_minute SET DEFAULT 60,
  ALTER COLUMN submission_burst SET DEFAULT 100,
  ALTER COLUMN submission_tokens SET DEFAULT 6000000000,
  ALTER COLUMN recipient_daily_limit SET DEFAULT 10000,
  ALTER COLUMN pending_message_limit SET DEFAULT 1000,
  ALTER COLUMN tracking_daily_limit SET DEFAULT 100000;
