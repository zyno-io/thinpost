-- Schema 41: message_recipients.kind
-- records whether an envelope recipient was a To, Cc or Bcc address. Cc and
-- Bcc recipients are accepted only through each provider's own fields and each
-- one is its own delivery, indexed after the To recipients (To, then Cc, then
-- Bcc). Messages whose stored source is version 2 (written from this release)
-- list every To and Cc recipient in each copy's headers and never a Bcc header;
-- the kind column is operator-visible metadata and is not used to build mail.
--
-- Existing rows are all To recipients, so the column is added with DEFAULT
-- 'to' and the default is dropped right after: every existing row
-- keeps 'to', and every writer must now name the kind. The recipient_count of a
-- message becomes the whole envelope count (still 1 to 100).
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. ALTER TABLE takes ACCESS EXCLUSIVE on message_recipients (one row per
-- envelope recipient of every accepted message). ADD COLUMN with a constant
-- DEFAULT is catalog-only (no rewrite) and DROP DEFAULT is catalog-only; the
-- CHECK is validated in one scan of the table under the closed gate, as
-- migrations 28 and 31 did, so the duration scales with that table: review its
-- size before deploying. No drain is needed beyond the gate close. Schema-40
-- pods fail closed on the new schema and are fenced by the closed gate until the
-- reopen waits for every pod to run the new image, so no older reader ever sees
-- a version 2 source or Cc/Bcc recipients. The runtime role's existing
-- SELECT, INSERT on message_recipients covers the column: no grant change. No
-- routine or extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
ALTER TABLE message_recipients
    ADD COLUMN kind text NOT NULL DEFAULT 'to',
    ADD CONSTRAINT message_recipients_kind CHECK (kind IN ('to', 'cc', 'bcc'));
ALTER TABLE message_recipients ALTER COLUMN kind DROP DEFAULT;
