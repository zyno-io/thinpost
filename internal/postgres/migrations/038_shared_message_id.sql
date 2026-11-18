-- Schema 38: messages prepared from this schema on carry one shared RFC 5322
-- Message-ID, <{messages.id}@{messages.sender_domain}>, in every recipient's
-- copy and across retries. shared_message_id records which form a message
-- used: earlier messages keep their immutable per-delivery header
-- (<{delivery id}@{sender_domain}>) for life. The flag is set only by
-- CommitPreparation, in the transaction that creates the delivery rows, and
-- never returns to false. Readers (feedback-loop correlation, native
-- webhooks, the admin API) derive the header from the flag, never from a
-- timestamp.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. ADD COLUMN ... NOT NULL DEFAULT false is catalog-only (no rewrite).
-- The CHECK is satisfied by every existing row (all false) and is validated
-- in the same ALTER TABLE in one scan of messages, under the closed gate and
-- a brief ACCESS EXCLUSIVE lock, as migration 31 did. No drain is needed
-- beyond the gate close. Schema-37 pods fail closed on the new schema and are
-- fenced by the closed gate until the reopen, which waits for every pod to run
-- the new image; messages still preparing at the close have their lease
-- expire and are re-prepared by the new image in the shared form. The runtime
-- role's table-level grant on messages covers the column: no grant change.
-- The collector keeps its column-level SELECT (object_key). No routine or
-- extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open".
ALTER TABLE messages
    ADD COLUMN shared_message_id boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT messages_shared_message_id_domain
        CHECK (NOT shared_message_id OR sender_domain IS NOT NULL);
