---
title: Recovery and the admission gate
description: How the admission gate, holds and the coordinated restore procedure protect against duplicate or lost mail after a database or journal restore.
---

# Recovery and the admission gate

A restore can erase evidence of work Thinpost already did. The admission gate, holds and the recovery commands exist so that a restored installation does not resend mail it already sent, redeliver webhooks, or quietly lose track of what happened. Backups and point-in-time recovery (PITR) of PostgreSQL are yours to provide: Thinpost does not take backups, including before a deploy migration.

::: warning Operator-driven procedure
The recovery commands record what you assert (workers fenced, database isolated, evidence reviewed). They cannot verify those facts. Never pass the attestation flags without having done the work, and rehearse the procedure before you need it.
:::

## Why a restore is not a failover

Ordinary control-plane failover preserves the committed database and the Edge journal. The Edge drains its old session, replays journal results, and the replacement holds missing older attempts as uncertain before asking for work.

A restore is different:

- A delivery that was queued in the snapshot may already have been accepted by the recipient.
- A webhook in the snapshot may already have been delivered.
- Submissions, idempotency records, suppressions and unsubscribes created after the snapshot are gone.
- The journal identity detects a different new volume, but a restored or copied snapshot keeps the same identity. Neither it nor a flag stored in the restored database can detect its own rollback, and a cloned worker may still be sending.

## The admission gate

The database holds one gate row with an expected deployment generation, a state (`closed`, `recovery` or `open`), a recovery identifier and a revision.

- Configure `THINPOST_DEPLOYMENT_GENERATION` outside the database, as a lowercase 32-character random hex string. The generation stored in the database must match it.
- Migration creates the gate **closed**. The runtime can never initialize or open it. A fresh installation opens it once with the recovery commands before it accepts work; see [Database](/deploy/database#_4-open-the-gate-for-the-first-time).
- Operational mutations require a matching generation and an open gate: submission, preparation, SMTP claims and start authorization, webhook dispatch, and other state-producing background work. Transactions take a shared gate lock before other locks. Closing takes an exclusive lock and waits for earlier transactions.
- Missing state, a mismatch or an unavailable database fails closed on every replica.
- Maintenance commands change the gate with compare-and-swap checks on generation, recovery ID and revision, and record the operator and each transition. A completed identical open can be retried. Interrupted quarantine can be resumed. Holds stay held when the gate opens.
- Closing the gate cannot retract an SMTP permit or an HTTP webhook request already issued. Network and process fencing remain necessary.

The operations snapshot and the console show the gate state. See [Monitoring](/operations/monitoring).

### Deploys and the gate

The control chart's pre-upgrade hook closes the gate, migrates the schema and applies the runtime grants in one transaction. The post-upgrade hook reopens the gate after every new pod is Ready. Hooks reopen only a gate that they closed. A closure made by an operator is migrated under but never reopened by a deploy. The mechanism and the failure table are in [Upgrades](/deploy/upgrades).

A deploy migration takes no backup. If a migration must be undone, restore your own PITR backup to a time before the deploy's `gate_close` event, then follow the coordinated restore below. Never roll back with Helm: older images reject a newer schema, and there is no schema downgrade.

### Manual gate commands

For an attended window, or to finish a deploy whose post hook failed, use the `gate` commands with the maintenance credential. Each change is a compare-and-swap on `--expected-revision`, so inspect first:

```sh
thinpost-control gate inspect
thinpost-control gate close --actor <operator> --expected-revision <inspected-revision> --generation <configured-generation>
thinpost-control gate open --actor <operator> --expected-revision <inspected-revision> --generation <configured-generation> --expected-schema <applied-version> --role <runtime-role> --rollout-verified
```

`gate open` verifies the stored generation, the applied schema version and the runtime role's privileges. `--rollout-verified` attests that every runtime replica runs the intended image on this schema and that older runtimes are fenced. The command is idempotent for the same actor at the following revision and fails with a category name, never database text. It cannot leave `recovery` mode; use `recovery open` for that.

## Holds

A hold stops automatic progress on work whose state cannot be trusted. Nothing releases a hold automatically.

| Hold | Cause | Release |
| --- | --- | --- |
| Uncertain delivery | An SMTP attempt has no recorded outcome, for example the connection dropped between permit and result | The console and API list it but do not release or resend it. Establish the outcome from your own records first; a resend can duplicate mail. |
| Recovery hold | Work that was in flight when a restore was quarantined | Resolve each held item after reviewing the recovery report. |
| Recipient policy hold | A recipient policy denied the address before SMTP | After correcting the allowlist on every control replica and the Edge, call `POST /api/v1/apps/{appId}/messages/{messageId}/deliveries/{deliveryId}/retry-policy-hold`. The release is audited and needs an active app and an open gate. |
| Preparation failed | A message failed 12 preparation claims | Fix the cause, then `POST /api/v1/apps/{appId}/messages/{messageId}/retry-preparation`. Audited, needs an open gate, resets the claim budget. |

Preparation retries use jittered exponential backoff capped at six hours. A claim holds a two-minute lease that the worker renews every 30 seconds for up to 30 minutes. If it loses the lease it stops without writing a failure and logs `status=preparation_lease_lost`. Objects uploaded by unsuccessful claims can stay unreferenced in the bucket.

If a recipient-policy release response is lost, reload the delivery before acting again. Repeating after a new hold can trigger another cycle.

## Coordinated restore

1. **Stop and fence.** Stop admission and fence every old control replica, Edge worker, webhook dispatcher and collector. Revoke their database access before exposing restored data. If you cannot prove an old Edge is stopped, block its SMTP egress at a boundary it cannot bypass.
2. **Preserve evidence.** Keep the available database, journal, object-storage and audit evidence. Restore into an isolated environment that normal runtime credentials cannot reach. Choose a new `THINPOST_DEPLOYMENT_GENERATION` before restarting workloads.
3. **Enter recovery.** Initialize the restored database in recovery mode with the new generation. Never trust a restored `open` flag.
4. **Quarantine.** Quarantine the whole nonterminal submission lifecycle: unprepared messages, queued and in-flight deliveries, retry schedules and pending or in-flight webhook deliveries. Original states and links are kept as evidence. Nothing is resent or deleted.
5. **Reconcile.** Compare the recovery report with the retained Edge journal and other evidence. Verify the object inventory with the restored keyring. Assess restored API keys, revocations, suppressions, bounce correlations and external webhook effects.
6. **New workers.** Provision new worker identities and journals after preserving old evidence. Do not reset journal epochs or reuse an identity while an old worker could return. Confirm all old workers are fenced before moving any public IP.
7. **Reopen.** Record the report and responsible operator, then open the exact recovery revision. Reopening permits new work. Historical holds remain held until you resolve them. Any exceptional redrive must be a deliberate decision to accept duplicate risk.

The generation and this procedure prevent an accidental reopen when followed. They do not detect an unnoticed rollback of all independent state together; that needs an independent monotonic witness or retained external evidence. Lost acceptance and idempotency history can also cause duplicate submissions from producers that retry.

### Object collection horizon

The gate stores a monotonic object-collection horizon. A restored database can hold an older horizon. Before reopening, fence collectors and delayed storage requests, reconcile the horizon with the restored bucket and backup history, and confirm no older in-flight delete can touch a newly referenced object. Restoring into an isolated new bucket namespace is one way to get that separation.

## Recovery commands

Run these only with an isolated maintenance database credential. Connection settings come from `THINPOST_DATABASE_URL` or the projected credential files described in [Database](/deploy/database); credentials are never printed. Generate distinct random 128-bit lowercase hex values for the generation and recovery ID, and store the generation separately from the SQL backup. The values below are placeholders.

```sh
thinpost-control recovery inspect
thinpost-control recovery begin --generation <new-generation> --recovery-id <new-recovery-id> --actor <operator> --expected-revision <inspected-revision> --workers-fenced --database-isolated
thinpost-control recovery quarantine --generation <new-generation> --recovery-id <new-recovery-id> --actor <operator>
thinpost-control recovery inspect
thinpost-control recovery report --generation <new-generation> --recovery-id <new-recovery-id> --expected-revision <inspected-revision> --limit 100
thinpost-control recovery hold-history --generation <new-generation> --recovery-id <new-recovery-id> --expected-revision <inspected-revision> --kind <kind> --entity-id <entity-id> --limit 100
thinpost-control recovery objects --generation <new-generation> --recovery-id <new-recovery-id> --expected-revision <inspected-revision> --limit 100 --workers-fenced --database-isolated --objects-frozen
thinpost-control recovery open --generation <new-generation> --recovery-id <new-recovery-id> --actor <operator> --expected-revision <inspected-revision> --workers-fenced --database-isolated --evidence-reviewed
```

`quarantine` works in bounded batches and can be re-run after a timeout. No command redrives held records.

### The report

`recovery report` returns hold identifiers, their original states and recorded times, never message contents or recipient addresses. For a held delivery it also reports the restored message ID, its current state and the attempt pointer. When the pointed-to attempt exists it includes the worker ID, epoch, state, start-authorization flag and journal ID.

A pointer with no matching attempt, a missing delivery or a missing journal ID needs investigation. None of them proves that an SMTP start did not occur. Compare these identifiers with the independently preserved Edge journal. The restored database can itself be stale.

Pagination:

- If `next` is present, pass its `kind`, `entity_id` and `secondary_id` as `--after-kind`, `--after-id` and `--after-secondary`. Omit `--after-secondary` when it is empty.
- Keep the generation, recovery ID and revision identical across pages. A completed quarantine and an unchanged gate revision are required. New pending work, a re-run that changes the revision, or opening the gate stops pagination. Restart the report after re-quarantining and inspecting the new revision.
- Treat the output as operational data and keep it with your other recovery evidence.

`recovery hold-history` returns the retained recovery IDs, original states and times for one entity listed in the current recovery. For webhooks, include `--secondary-id` from the report. Start without `--after-recovery-id`, then pass `next` for later pages. Results sort by recovery ID, not by time. Missing or conflicting records do not establish that work was never sent.

If an existing hold appears after quarantine, `report`, `objects` and `open` fail until a new quarantine pass records it. If an entity already recorded in the current recovery becomes active, quarantine fails instead of silently releasing it.

### The object inventory

`recovery objects` reads and authenticates every selected object with the restored bucket's read credential and the restored keyring, using the same `THINPOST_S3_*` and `THINPOST_OBJECT_KEYRING_FILE` settings as the mail runtime. It discards the plaintext and returns a count, failed reference kinds and IDs, and a cursor. It prints no object keys, bodies or credentials, and it exits non-zero when a page has failures.

- Begin without cursor arguments. While `next` is non-null, pass its `kind` and `id` as `--after-kind` and `--after-id`.
- Keep every page, skip none, and require zero failures on each. A page covers at most 100 references.
- Fix missing objects, damaged ciphertext, missing key versions and storage access failures, then rerun the complete scan from the beginning.
- Inbound messages past their content expiry are skipped, so a restore does not fail on objects that were legitimately deleted.

Each page takes the gate lock and needs a completed quarantine with the same generation, recovery ID and revision. It does not hold a snapshot across pages and cannot stop a privileged actor from changing data. Keep the database, bucket and keyring stable throughout. `recovery open` does not enforce that the inventory completed.

The attestation flags mean:

- `--workers-fenced`: old Edge workers, controllers, webhook dispatchers and collectors are fenced.
- `--database-isolated`: the restored database cannot be reached by normal runtime credentials.
- `--objects-frozen`: the bucket and keyring are stable for the scan.
- `--evidence-reviewed`: the report and external evidence were reviewed.

### Restoring an older schema

`recovery migrate-restored-schema` applies the fixed recovery-chain migrations to a restored database of an older supported schema while the gate is in recovery. See [Maintenance commands](/operations/maintenance#restored-schemas) for when and how.

## After a restore: what to re-check

A restore reverts every table to the snapshot. Review these:

- **Suppressions.** Suppressions, hard-bounce entries and unsubscribes added after the snapshot are lost. Mailbox providers do not resend an unsubscribe, and audit rows hold no addresses to rebuild the list, so re-add suppressions from your apps' own records before sending.
- **Spam-complaint intake.** Complaints and counters committed after the snapshot are lost and feedback loops do not resend them. Check the feedback mailbox and reporter list in the console. Rotate the mailbox only if the restored token is unknown.
- **Retry policy.** It reverts to the snapshot's value. Check it on the Operations page.
- **Tracking defaults and budgets.** Per-app defaults revert. Check each app's Tracking tab.
- **Receiving domains and limits.** Inbound domains verified after the snapshot must be verified again. Limits that were raised may be back at earlier values. Mail accepted after the snapshot is lost from Thinpost, although apps keep webhooks they already received.
- **Sending IPs.** The pool and per-app assignments revert, and so does each attempt's recorded source. An app assigned to an IP after the snapshot is back on the primary. Check every app's Sending IP on Operations.
- **Sealed secrets.** Webhook signing secrets are readable only with the matching object keyring. Restore the keyring with the database.

## Reference

Object presence checks use HEAD and do not verify ciphertext integrity or key availability. Use the object inventory for that. Keep the objects and keyring that the restored database references. A database that references an object collected from a bucket that was not restored with it does not get the ciphertext back.
