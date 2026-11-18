-- Schema 37: per-app open and click tracking defaults (both off).
--
-- Read with the API key at authentication. Acceptance applies a default only
-- to an HTML message whose request left that kind unset, only when the
-- tracking runtime is configured, and drops it if the message would otherwise
-- fail the size/rewrite preflight. The resolved choice is written to the
-- existing source-object Tracking field (unchanged format), so a later change
-- never affects accepted messages and no reader fence is needed.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. ADD COLUMN ... NOT NULL DEFAULT false is catalog-only (no rewrite, no
-- scan) and takes a brief ACCESS EXCLUSIVE lock on the small apps table, as
-- migration 35 did. No drain is needed beyond the gate close. Schema-36 pods
-- fail closed on the new schema and are fenced by the closed gate until the
-- reopen, which waits for every pod to run the new image. The runtime role's
-- table-level SELECT, INSERT, UPDATE on apps covers both columns: no grant
-- change. Collector and retirement roles get nothing.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
ALTER TABLE apps
    ADD COLUMN default_track_opens boolean NOT NULL DEFAULT false,
    ADD COLUMN default_track_clicks boolean NOT NULL DEFAULT false;
