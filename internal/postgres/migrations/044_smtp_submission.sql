-- Schema 44: SMTP submission credentials, per-app From policy and the columns
-- that record how a message arrived.
--
-- smtp_credentials holds one SMTP login of an app: a 32-hex ID (the SMTP
-- username), an operator-chosen name and the SHA-256 digest of the secret,
-- which is shown once at creation and never stored. Revocation is a timestamp;
-- the row stays so that messages keep naming the credential that sent them.
-- smtp_replay_secret holds the one deployment-wide secret that keys the
-- replay digests of submitted messages; it is written once and sealed under the
-- object keyring version active at creation. The apps columns hold the From
-- policy applied to submitted mail (off, rewrite to a domain, or one fixed
-- address). messages gains the credential that submitted it and the From the
-- client originally sent when the policy replaced it; an empty subject is
-- allowed only for a message submitted with a credential (raw mail may have no
-- Subject). edge_source_reports.submission_ports records the submission ports
-- the Edge reported listening on, for the console's availability badge only.
-- Nothing in this schema is written by a release that predates it.
--
-- Deploy review (lock and rollout): the pre hook closes the gate before this
-- runs. Both CREATE TABLE statements touch no existing data. The apps ALTER adds
-- columns with constant defaults (catalog-only) and validates its CHECK in one
-- scan of the small apps table. The messages ALTER adds its two nullable columns
-- as a catalog-only change, and its three constraints are NOT VALID, so no row
-- of messages is read: the composite foreign key (every existing row is NULL in
-- the referencing column), messages_original_from (every existing row is NULL)
-- and the re-added messages_subject_size are sound against old rows because
-- each predicate holds for them (the new subject predicate is looser than the
-- old one); new and updated rows are checked. The same applies to
-- messages_smtp_not_console (smtp_credential_id is NULL on every existing row).
-- The edge_source_reports ADD COLUMN is catalog-only. No drain is needed beyond
-- the gate close. Rollout: schema-43 pods fail closed on the new schema and are
-- fenced by the closed gate until the reopen waits for every pod to run the new
-- image. Runtime grants: smtp_credentials gets SELECT, INSERT and UPDATE
-- (revoked_at, last_used_at); smtp_replay_secret gets SELECT and INSERT; the
-- existing table-level grants on apps, messages and edge_source_reports cover
-- the new columns. The collector and retirement roles get nothing. No routine
-- or extension is installed.
-- Recovery: the recovery chain stops at 28, so this is applied by the normal
-- deploy after "recovery open". Roll forward only.
CREATE TABLE smtp_credentials (
  id text PRIMARY KEY CHECK (id ~ '^[0-9a-f]{32}$'),
  app_id text NOT NULL REFERENCES apps(id),
  name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 128),
  digest bytea NOT NULL UNIQUE CHECK (octet_length(digest) = 32),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  revoked_at timestamptz,
  last_used_at timestamptz,
  CONSTRAINT smtp_credentials_id_app_key UNIQUE (id, app_id),
  CONSTRAINT smtp_credentials_revoked_after_created CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);
CREATE INDEX smtp_credentials_app_idx ON smtp_credentials (app_id, created_at, id);
-- Written once and never updated: sealed under the object-keyring version active at creation.
CREATE TABLE smtp_replay_secret (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  sealed bytea NOT NULL CHECK (octet_length(sealed) BETWEEN 32 AND 512),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
ALTER TABLE apps
  ADD COLUMN smtp_from_mode text NOT NULL DEFAULT 'off' CHECK (smtp_from_mode IN ('off','domain','address')),
  ADD COLUMN smtp_from_domain text CHECK (smtp_from_domain IS NULL OR (smtp_from_domain ~ '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$' AND octet_length(smtp_from_domain) BETWEEN 1 AND 253)),
  ADD COLUMN smtp_from_address text CHECK (smtp_from_address IS NULL OR (
      smtp_from_address ~ '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+(\.[A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+)*@[a-z0-9]([a-z0-9.-]*[a-z0-9])?$'
      AND octet_length(smtp_from_address) BETWEEN 3 AND 254
      AND octet_length(split_part(smtp_from_address, '@', 1)) <= 64)),
  ADD COLUMN smtp_from_only_unauthorized boolean NOT NULL DEFAULT true,
  ADD CONSTRAINT apps_smtp_from_policy CHECK (
    (smtp_from_mode <> 'domain'  OR smtp_from_domain  IS NOT NULL) AND
    (smtp_from_mode <> 'address' OR smtp_from_address IS NOT NULL));
ALTER TABLE messages
  ADD COLUMN smtp_credential_id text,
  ADD COLUMN original_from_address text,
  -- Ties the credential to the message's own app, as message_recipients does (028_message_metadata.sql).
  ADD CONSTRAINT messages_smtp_credential_fk FOREIGN KEY (smtp_credential_id, app_id) REFERENCES smtp_credentials(id, app_id) NOT VALID,
  ADD CONSTRAINT messages_original_from CHECK (original_from_address IS NULL OR (smtp_credential_id IS NOT NULL AND octet_length(original_from_address) BETWEEN 3 AND 998)) NOT VALID,
  DROP CONSTRAINT messages_subject_size,
  ADD CONSTRAINT messages_subject_size CHECK (subject IS NULL OR octet_length(subject) BETWEEN 1 AND 998 OR (subject = '' AND smtp_credential_id IS NOT NULL)) NOT VALID,
  ADD CONSTRAINT messages_smtp_not_console CHECK (smtp_credential_id IS NULL OR NOT console_test) NOT VALID;
ALTER TABLE edge_source_reports
  ADD COLUMN submission_ports integer[] CHECK (submission_ports IS NULL OR cardinality(submission_ports) <= 8);
