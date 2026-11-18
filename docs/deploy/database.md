---
title: Database
description: Create the owner, runtime and optional collector roles, bootstrap and migrate the schema, grant privileges, connect over verified TLS and open the admission gate.
---

# Database

Thinpost keeps application state, queues, leases and the admission gate in one PostgreSQL database. It needs **PostgreSQL 17 or newer** and a dedicated database that it owns.

The control plane refuses to start unless the schema is current, the runtime role holds only reviewed privileges and the connection is verified TLS. This page sets those up. Upgrades after the first install run the same steps for you through Helm hooks; see [Upgrades](/deploy/upgrades).

## Roles

| Role | Login | Used by | Holds |
| --- | --- | --- | --- |
| **Owner** | Yes | `bootstrap-database`, `migrate`, `grant-*`, `gate`, `recovery`, and the Helm hook Jobs | Owns the database and the application schema. Never mounted in the long-running Deployment. |
| **Runtime** | Yes | The control Deployment | Only the reviewed table and column privileges that `grant-runtime` applies. |
| **Collector** | Yes, optional | Your own object-collection tooling | Read access to object keys only, applied by `grant-collector`. Thinpost does not schedule a collector. |

The runtime login must be different from the owner, must not be a superuser, and must not be a role the owner can `SET ROLE` to.

The control plane checks the runtime role at startup, including every role it can reach with `SET ROLE`. It refuses to run if the role can create objects in the database or a schema, create temporary tables, read the recovery tables or reach an administrative role. The grant commands never revoke existing privileges. They reject unsafe ones, so audit any role you reuse and fix it yourself.

## Create the roles and database

Run once, as a PostgreSQL administrator:

```sql
CREATE ROLE thinpost_owner   LOGIN PASSWORD 'replace-with-a-long-random-password';
CREATE ROLE thinpost_runtime LOGIN PASSWORD 'replace-with-another-long-random-password';
CREATE DATABASE thinpost OWNER thinpost_owner;
```

If a connection pooler authenticates through a separate role, create that role too and pass it to `bootstrap-database` with `--pooler-auth-role`.

## Connection strings

Every Thinpost process builds its connection string from three pieces so credentials never appear in an environment variable or a log:

| Setting | Value |
| --- | --- |
| `THINPOST_DATABASE_BASE_URL` | A URL with no user information. |
| `THINPOST_DATABASE_USERNAME_FILE` | Absolute path to a file holding the user name. |
| `THINPOST_DATABASE_PASSWORD_FILE` | Absolute path to a file holding the password. |

Set all three or none, and do not also set `THINPOST_DATABASE_URL`. The files are read once at start and **not trimmed**, so they must not end in a newline. The chart mounts the runtime Secret for the Deployment and the owner Secret for the hook Jobs and sets the file variables itself. See [Helm: control](/deploy/helm-control).

A base URL looks like this:

```text
postgres://db.example.com:5432/thinpost?sslmode=verify-full&sslrootcert=/run/thinpost/db-ca/ca.crt&search_path=thinpost
```

The rules the control binary enforces:

- `sslmode=verify-full` is required. Any other mode is rejected unless `THINPOST_ALLOW_INSECURE_LOCAL_DB=1` and the host is a literal loopback address, which exists for disposable local development only.
- Use a single host, an explicit database name and at most one occurrence of each parameter.
- The accepted parameters are `sslmode`, `sslrootcert`, `default_query_exec_mode`, `connect_timeout`, `application_name` and `search_path`. Anything else is rejected, because driver options could override the verified host or TLS mode.
- `default_query_exec_mode` accepts `cache_statement`, `cache_describe`, `describe_exec` and `exec`. `simple_protocol` is rejected because it inlines subjects, recipients and search terms into SQL text that PostgreSQL can log.
- `search_path` takes one lowercase schema identifier, at most 63 characters, made of letters, digits and underscores that does not start with a digit. Use it to point every connection at the application schema.

### TLS and `sslrootcert`

The chart mounts the ConfigMap named by `database.caConfigMap` (default `thinpost-db-ca`, key `ca.crt`) at `/run/thinpost/db-ca`, so `sslrootcert=/run/thinpost/db-ca/ca.crt` works in both the Deployment and the hook Jobs. The bundle must be a ConfigMap, not a Secret, and must contain the CA that signed your server certificate.

The driver re-reads `sslrootcert` for every new connection, and connections are recycled every 30 minutes, so a rotated CA bundle takes effect without a restart. Keep the old CA in the bundle until the server presents a certificate from the new one.

## Deployment generation

`THINPOST_DEPLOYMENT_GENERATION` is a lowercase 32-character hexadecimal identifier that you generate and configure outside the database:

```sh
openssl rand -hex 16
```

The admission gate stores an expected generation. Control and the hooks fail closed if the stored value and the configured one differ, so a database restored from an older snapshot cannot silently resume work. Keep the value in the `thinpost-operator-config` ConfigMap, where both the Deployment and the hook Jobs read it. Change it only as part of a coordinated restore; see [Recovery](/operations/recovery).

## Run the maintenance commands

The commands below are subcommands of the control image. They need the owner credential. The simplest way to run them is `docker run` from a host that can reach PostgreSQL:

```sh
export THINPOST_IMAGE=ghcr.io/zyno-io/thinpost-control:<version>
export THINPOST_DATABASE_URL='postgres://thinpost_owner:<password>@db.example.com:5432/thinpost?sslmode=verify-full&sslrootcert=/ca/ca.crt&search_path=thinpost'

thinpost() {
  docker run --rm -e THINPOST_DATABASE_URL -v "$PWD/db-ca.crt:/ca/ca.crt:ro" "$THINPOST_IMAGE" "$@"
}
```

A Kubernetes Job that uses the same image, the owner Secret and the CA ConfigMap works equally well. Pass the three credential-file variables instead of `THINPOST_DATABASE_URL`.

## First install

Do these once, in order.

### 1. Bootstrap the database

Connect as the database owner and name the expected database, the application schema and the runtime role:

```sh
thinpost bootstrap-database --database thinpost --schema thinpost --runtime-role thinpost_runtime
```

`bootstrap-database` runs in one transaction. It creates the application schema, or verifies that the owner already owns it, revokes `PUBLIC` `CONNECT` and `TEMPORARY` on the database and grants `CONNECT` to the runtime role. It rejects a different database, an owner hidden behind `SET ROLE`, a schema owned by someone else, and unsafe `PUBLIC` creation rights on the schema. The schema may not be `public` or start with `pg_`. It does not migrate or open the gate, and it keeps your other database grants.

### 2. Migrate

```sh
thinpost migrate
```

`migrate` applies every embedded migration under an advisory lock and verifies the recorded checksums. It never runs at startup, and an applied migration is never edited. On a new database it creates the schema and the admission gate in the **closed** state.

### 3. Grant the runtime role

```sh
thinpost grant-runtime --role thinpost_runtime
```

This grants schema usage and the reviewed operations on each table, and column-level `UPDATE (lock_guard)` on the gate table (PostgreSQL needs an update privilege to take a row lock; the column is inert). It refuses to commit when the role has unsafe privileges or memberships.

### 4. Open the gate for the first time

A new installation starts closed, and the runtime can never open the gate itself. Open it once with the recovery commands, using your generation. On an empty database there are no old workers, controllers or evidence, so the attestations are true by construction. Do not pass them on a database that holds data you have not reviewed.

```sh
export GENERATION=<the value of THINPOST_DEPLOYMENT_GENERATION>
export RECOVERY_ID=$(openssl rand -hex 16)

thinpost recovery inspect
thinpost recovery begin --actor "$USER" --expected-revision <revision from inspect> \
  --generation "$GENERATION" --recovery-id "$RECOVERY_ID" \
  --workers-fenced --database-isolated
thinpost recovery quarantine --actor "$USER" --generation "$GENERATION" --recovery-id "$RECOVERY_ID"
thinpost recovery inspect
thinpost recovery open --actor "$USER" --expected-revision <revision from inspect> \
  --generation "$GENERATION" --recovery-id "$RECOVERY_ID" \
  --workers-fenced --database-isolated --evidence-reviewed
```

Each command prints the gate status as JSON. `quarantine` holds existing nonterminal work, which is nothing on an empty database. After `open`, the gate is open for the stored generation.

The recovery commands exist for restoring from a backup, and they are described in [Recovery](/operations/recovery). A first install needs only the four lines above.

### 5. Check

```sh
thinpost gate inspect
```

The output shows the mode `open`, your generation and the revision. When the control Deployment starts, it verifies the schema, the runtime privileges and the generation, and `/operator-readyz` reports whether its database checks succeed.

## Later changes

After the first install, schema upgrades run in the control chart's pre-upgrade hook as the owner role. The hook closes the gate, applies the migrations, **re-applies the runtime grants** and checks the privileges in one transaction, and a post-upgrade hook reopens the gate once every pod runs the new image. You do not run `migrate` or `grant-runtime` by hand again. See [Upgrades](/deploy/upgrades).

## Collector role (optional)

If you run your own tooling that lists and deletes orphaned objects, give it a separate login and grant it the narrow read access it needs:

```sh
thinpost grant-collector --role thinpost_collector
```

The role must already exist as a login with `CONNECT`. The command grants schema usage, reads of the migration ledger and the gate, and read access to only the object-key column of each reference table. It commits nothing if the role has unsafe inherited privileges. The collector cannot advance the collection horizon, which stays a separate maintenance operation. Read [Object storage](/deploy/object-storage) before you enable any deletion.

## Backups

Thinpost takes no backup before a migration. Run PostgreSQL point-in-time recovery for the whole database, together with versioned or otherwise recoverable bucket contents and a copy of the keyrings, so that you can restore a consistent state. The coordinated procedure is in [Recovery](/operations/recovery).

## Next

- [Object storage](/deploy/object-storage)
- [Helm: control](/deploy/helm-control)
- [Upgrades](/deploy/upgrades)
