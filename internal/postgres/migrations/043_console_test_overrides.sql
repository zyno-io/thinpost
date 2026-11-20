-- Schema 43: console test sends and their operator-only overrides.
--
-- console_test marks a message submitted by an administrator through the
-- console test send (adapter 'console'); it is true for every such message and
-- false for all others. limits_override and recipient_policy_override record
-- the two operator-only options of that route: skip (and do not count) the
-- app's sending limits, and skip the deployment recipient allowlist. A
-- message that is not a console test never carries an override, enforced by
-- messages_overrides_console. limits_override also excludes the message from
-- the app's pending-message count; recipient_policy_override is read again at
-- claim time so the dispatch recheck honours the same decision.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. ALTER TABLE messages takes ACCESS EXCLUSIVE. The three ADD COLUMN are
-- NOT NULL with a constant default (false), which PostgreSQL 11 and later
-- record in the catalog without rewriting the table, so the statement does
-- not scan or rewrite messages. The CHECK is NOT VALID, so no row is read.
-- That is sound because every existing row has false in all three columns and
-- satisfies the predicate; new and updated rows are checked. No index is
-- created: the app pending count keeps using messages_app_pending_v2_idx and
-- filters limits_override on the few matching rows. No drain is needed beyond
-- the gate close.
-- Rollout: schema-42 pods fail closed on the new schema and are fenced by the
-- closed gate until the reopen waits for every pod to run the new image, so no
-- older pod admits work after an override row exists. An older pod does not
-- write the columns (they default to false) and does not read them.
-- Grants: the runtime role's table-level SELECT, INSERT, UPDATE on messages
-- cover the new columns: no grant change. No routine or extension is
-- installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
ALTER TABLE messages
    ADD COLUMN console_test boolean NOT NULL DEFAULT false,
    ADD COLUMN limits_override boolean NOT NULL DEFAULT false,
    ADD COLUMN recipient_policy_override boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT messages_overrides_console
        CHECK (console_test OR NOT (limits_override OR recipient_policy_override)) NOT VALID;
