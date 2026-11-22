---
title: Maintenance commands
description: The thinpost-control maintenance subcommands, the database roles they need, tracking-reference retirement and restored-schema migration.
---

# Maintenance commands

`thinpost-control` is the control plane's server binary. With a subcommand it runs a one-shot maintenance task and exits. Every subcommand needs database access, but none prints connection credentials.

## Credentials

Run maintenance commands with an isolated maintenance credential, not the runtime credential. Provide it in one of two ways:

- `THINPOST_DATABASE_URL`, a full URL.
- `THINPOST_DATABASE_BASE_URL` (a URL without credentials) plus `THINPOST_DATABASE_USERNAME_FILE` and `THINPOST_DATABASE_PASSWORD_FILE`. Set all three or none, and not together with `THINPOST_DATABASE_URL`. The files are read once at start without trimming.

Database URLs require `sslmode=verify-full`. The maintenance connection's `search_path` must name the application schema. See [Database](/deploy/database).

Commands never print database error text, because it can quote data. Failures return a fixed category.

## Commands

| Command | Credential | Purpose |
| --- | --- | --- |
| `bootstrap-database` | Database owner | Verifies or creates the protected schema, revokes `PUBLIC` connect and temporary access, grants connect to the runtime role. Does not migrate or open the gate. |
| `migrate` | Schema owner | Applies pending migrations under an advisory lock and verifies recorded checksums. An applied migration is never edited. |
| `grant-runtime --role <role>` | Schema owner | Applies the reviewed runtime privileges and checks them. |
| `grant-collector --role <role>` | Schema owner | Grants a pre-existing collector login its read-only reference privileges. |
| `grant-tracking-retirement --role <role>` | Schema owner | Grants a pre-existing login the privileges for tracking retirement. |
| `retire-tracking` | Retirement login | Retires one bounded batch of expired tracking references. |
| `gate inspect \| close \| open` | Maintenance | Manual gate control. See [Recovery](/operations/recovery#manual-gate-commands). |
| `recovery ...` | Maintenance | Restore recovery. See [Recovery](/operations/recovery#recovery-commands). |
| `retry-policy inspect` | Maintenance | Prints the stored delivery retry policy and revision. Edit the policy in the console. |
| `deploy-migrate`, `deploy-reopen` | Schema owner | Run by the chart's Helm hook Jobs. See [Upgrades](/deploy/upgrades). `deploy-migrate --check` prints the plan without changing anything. |

### Database bootstrap

Create a database owned by the maintenance login, plus the runtime login, then connect as the owner and name the expected database, schema and runtime role:

```sh
thinpost-control bootstrap-database --database thinpost --schema thinpost --runtime-role thinpost_runtime
```

If a pooler needs its own authentication role to connect, add `--pooler-auth-role` with that existing role. Bootstrap is transactional and rejects a different database, an owner hidden behind `SET ROLE`, a schema owned by another role and unsafe `PUBLIC` schema creation access. Other existing grants are preserved, so audit them separately.

### Migrations and grants

```sh
thinpost-control migrate
thinpost-control grant-runtime --role thinpost_runtime
```

A pending migration over an existing gate takes an exclusive lock and refuses to proceed unless the gate is closed. That drains admitted transactions first. Control requires the current schema at startup, and an older image rejects a newer schema with no downgrade. Routine upgrades use the chart's hooks instead of these commands.

The grant commands check the applied migration checksums, serialize with migrations and apply their grants transactionally. They reject a role whose effective or `SET ROLE` privileges could:

- change the recovery gate or evidence,
- create schemas or temporary tables, or
- otherwise fail the startup gate check.

They do not remove unexpected existing grants or role memberships, so audit and revoke unsafe privileges yourself first. `PUBLIC` and inherited grants count. PostgreSQL commonly grants `TEMPORARY` to `PUBLIC`.

The runtime role gets no DDL and no write access to recovery evidence. Verify the actual login and its inherited grants after provisioning.

For transaction-mode PgBouncer with named prepared statements disabled, use `default_query_exec_mode=exec` in the database URL. Test your pooler and its TLS separately.

## Tracking retirement

Tracking links keep redirecting without any database record, because the destination is inside the signed link. The database keeps a mapping so analytics can be recorded. Retirement ends that analytics mapping after a link has expired and its delivery is final.

The `retire-tracking` command retires one bounded batch:

```sh
thinpost-control grant-tracking-retirement --role thinpost_retirement
thinpost-control retire-tracking --actor <operator-or-service> --generation <current-generation> --limit 100
```

- The login must be separate from the runtime login and needs database connect. The grant command rejects unsafe existing, inherited or `SET ROLE`-reachable privileges rather than removing them.
- `--limit` is 1 to 100 (default 100). The command prints the number of references retired and does not delete objects. A fenced object sweep reclaims ciphertext later. See [Object storage](/deploy/object-storage).
- It verifies the schema, the login's effective privileges, and the audit trigger function's reviewed body, owner, search path, execution grants and trigger configuration. The check is a point-in-time check, and schema owners remain trusted to protect these objects.
- It needs the open gate and the current generation. Each retirement commits with an app-scoped audit record that carries the claimed `--actor` and the actual database login. You must authenticate the actor yourself, for example through the job that runs the command.
- It rechecks terminal state and the database clock, so it refuses to retire a mapping early. Queued, in-flight, uncertain and held deliveries keep their mappings. Links with no expiry are never retired.
- It does not run on a schedule. Run it from your own scheduler.

Retirement does not revoke a link. A retired link keeps redirecting, and click events for it are no longer recorded.

## Object collection

Thinpost does not delete outbound message objects. A collector that sweeps orphaned objects exists in the codebase. It is not started by the runtime and requires:

- separately scoped database and object-store identities, with `grant-collector` for the database side;
- every older writer upgraded and fenced;
- your own scheduling, monitoring and recovery controls, and tested coordinated bucket and database restore.

The collector must use the same bucket as the writers and its credentials must not reach ordinary control replicas. It must not mount the object keyring. Verify your store's HEAD, read-after-write, delete and versioning behavior first. On Amazon S3, `HEAD` needs `s3:GetObject`, which also lets the identity read the ciphertext.

Inbound message objects are never swept by this collector. Delete them with a lifecycle rule on the `inbound/` prefix. See [Object storage](/deploy/object-storage).

## Restored schemas

`recovery migrate-restored-schema` brings a restored database that is on an older supported schema forward to the point the recovery chain can work with. It is only for restores.

- It requires the gate in recovery mode, a maintenance login with schema migration rights, and checksum-verified restored schemas.
- It takes the migration and gate locks, checks the exact generation, recovery ID and gate revision, applies the fixed missing migrations, advances the revision and records the original schema version and actor, all in one transaction. A failure rolls the whole sequence back. An identical retry with the same actor and expected revision succeeds only when that receipt exists.
- It does not open the gate, quarantine records, grant runtime privileges or show that the database and bucket backups are coordinated. Later migrations are left to the normal deploy, so held webhook work is not touched by them.
- It does not bypass checksum or gate failures. A restored database from before the supported range needs a separately reviewed upgrade.

After fencing and isolating, enter recovery and run:

```sh
thinpost-control recovery inspect
thinpost-control recovery migrate-restored-schema --generation <new-generation> --recovery-id <new-recovery-id> --actor <operator> --expected-revision <inspected-revision> --workers-fenced --database-isolated
thinpost-control recovery inspect
thinpost-control recovery quarantine --generation <new-generation> --recovery-id <new-recovery-id> --actor <operator>
```

The flags attest that you fenced the workers and isolated the database; the command cannot check. Use the revision that the migration returns for later steps.

1. Recovery migrates the restored schema to the recovery chain's last version and quarantines.
2. `recovery open` reopens the gate at that version.
3. A normal deploy runs the pre hook, which closes the gate, applies the remaining migrations and grants in one transaction, and reopens after the new pods are Ready.

`grant-runtime` and `retry-policy inspect` require exactly the current schema, so they cannot run before step 3. Control images that need a newer schema refuse to start until that deploy runs.

A historical `tracking_abandoned` outcome blocks one of the chained migrations and needs separate investigation. Do not delete or relabel evidence to make the migration pass. Defaults added by newer migrations, such as per-app tracking and submission budgets, start at zero in a database restored through this chain until schema 50 changes the defaults for new apps (existing apps keep their zero), and legacy tracking mappings have no assumed post-send lifetime. Review them before reopening admission.
