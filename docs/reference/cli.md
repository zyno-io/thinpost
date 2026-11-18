---
title: Command line
description: Subcommands, flags and exit behavior of thinpost-control and thinpost-edge.
---

# Command line

Thinpost ships two binaries. Both double as their own maintenance tools: run with no arguments they start the service; run with a subcommand they perform one task and exit. In a container image the binaries are `thinpost-control` and `thinpost-edge`.

General behavior:

- A command exits `0` on success and `1` on any failure, printing a short error on standard error. Errors are deliberately generic: they never include database text, credentials, addresses or message data.
- Results are printed as JSON on standard output.
- Subcommands of `thinpost-control` read the database settings described in [Environment variables](/reference/configuration#database). They do not need the OIDC, object storage or control-bridge settings, except where noted.
- Both binaries refuse to run when the environment variable `GRPC_BINARY_LOG_FILTER` is set, because gRPC binary logs record recipients and message bodies.
- Flags use Go syntax: `--name value` or `--name=value`. Unknown flags and stray positional arguments are errors.
- Run maintenance commands with a database role that has the privileges the task needs (the schema owner for migrations and grants, a maintenance role for recovery); see [Database roles](/deploy/database).

## thinpost-control

```text
thinpost-control                       run the control plane
thinpost-control <subcommand> [flags]  run a maintenance command
```

An unknown first argument fails with `unknown command`.

| Subcommand | Purpose |
| --- | --- |
| [`migrate`](#migrate) | Apply schema migrations. |
| [`deploy-migrate`](#deploy-migrate) | Helm pre-upgrade hook: migrate, closing the admission gate only when a schema change needs it. |
| [`deploy-reopen`](#deploy-reopen) | Helm post-upgrade hook: reopen the gate after the rollout completes. |
| [`gate`](#gate) | Inspect, close or reopen the admission gate by hand. |
| [`recovery`](#recovery) | Coordinated restore: quarantine, inspect, report and reopen. |
| [`retry-policy`](#retry-policy) | Print the stored delivery retry policy. |
| [`bootstrap-database`](#bootstrap-database) | Prepare a dedicated database and its protected schema. |
| [`grant-runtime`](#grant-runtime) | Grant the runtime login role its privileges. |
| [`grant-collector`](#grant-collector) | Grant the object collector role its privileges. |
| [`grant-tracking-retirement`](#grant-tracking-retirement) | Grant the tracking retirement role its privileges. |
| [`retire-tracking`](#retire-tracking) | Retire expired tracking references in a batch. |

### migrate

```sh
thinpost-control migrate
```

Applies every pending schema migration to the database in `THINPOST_DATABASE_URL`, with a five-minute limit. It takes no flags; any argument is an unknown command. It prints nothing on success. A fresh database is created closed, and neither [`gate open`](#gate) nor the deploy hooks can open it: a new installation opens it once with `recovery begin`, `recovery quarantine` and `recovery open`, as described in [Database](/deploy/database#_4-open-the-gate-for-the-first-time). Routine upgrades use [`deploy-migrate`](#deploy-migrate) instead, which also manages the gate.

### deploy-migrate

```sh
thinpost-control deploy-migrate --actor deploy:<namespace>/<release>:r<revision> [--check]
```

The command the chart's pre-install and pre-upgrade hook runs. It migrates the schema to the version the image expects. A deploy that needs no schema change never closes the gate. When a change is needed it drains queued work, closes the gate under the given actor, migrates, and leaves the gate closed for [`deploy-reopen`](#deploy-reopen). It requires the schema owner's database credentials.

| Flag | Default | Description |
| --- | --- | --- |
| `--actor` | none | Required. Deploy actor of the form `deploy:<namespace>/<release>:r<revision>`. Recorded on every gate change. |
| `--drain-timeout` | `2m` | How long to wait for queued messages before closing the gate. Must not be negative. |
| `--timeout` | `8m` | Overall time limit. Must be positive. |
| `--check` | `false` | Report the plan without changing anything. |

Environment: `THINPOST_RUNTIME_ROLE` and `THINPOST_DEPLOYMENT_GENERATION` are required, in addition to the database settings. The result is one JSON object with the fields `action`, `from`, `to`, `gate` and `revision`. On failure, a fixed reason code is written to `/dev/termination-log` so Kubernetes shows it on the Job's pod; the process never writes database text there. See [Upgrades](/deploy/upgrades).

### deploy-reopen

```sh
thinpost-control deploy-reopen \
  --actor deploy:<namespace>/<release>:r<revision> \
  --namespace <namespace> --selector <label-selector> \
  --image <control-image> --replicas <n>
```

The post-install and post-upgrade hook. It reopens the gate only when this release closed it (a gate closed by an operator is never reopened) and only after every control pod runs the expected image and is Ready. It talks to the Kubernetes API with the pod's service account token and reads pod state only.

| Flag | Default | Description |
| --- | --- | --- |
| `--actor` | none | Required. The same actor the pre hook used. |
| `--namespace` | none | Required. Namespace of the control pods. |
| `--selector` | none | Required. Label selector that matches the control pods. |
| `--image` | none | Required. The exact control image the Deployment renders. |
| `--replicas` | `0` | Required, at least `1`. Number of Ready control pods to wait for. |
| `--container` | `control` | Name of the control container. |
| `--rollout-timeout` | `5m` | How long to wait for the rollout. |
| `--kube-dir` | `/run/thinpost/kube` | Directory holding the service account token and CA bundle. |
| `--timeout` | `8m` | Overall time limit. |

Environment: `THINPOST_RUNTIME_ROLE` and `THINPOST_DEPLOYMENT_GENERATION` are required. It prints one JSON object with `action`, `schema` and `revision`, and writes a reason code to the termination log on failure.

### gate

```sh
thinpost-control gate inspect
thinpost-control gate close --actor <operator> --expected-revision <rev> --generation <generation>
thinpost-control gate open  --actor <operator> --expected-revision <rev> --generation <generation> \
  --expected-schema <version> --role <runtime-role> --rollout-verified
```

The manual path for the admission gate outside a deploy: for an attended maintenance window, or to finish a deploy whose post hook failed. Every change is a compare-and-swap on the revision you inspected, so always run `inspect` first. All operations print the resulting gate status as JSON. The time limit is two minutes.

| Flag | Used by | Description |
| --- | --- | --- |
| `--actor` | `close`, `open` | Required. The responsible operator. |
| `--expected-revision` | `close`, `open` | Required, greater than zero. The revision `inspect` reported. |
| `--generation` | `close`, `open` | Required. The configured deployment generation (32 lowercase hex characters). |
| `--expected-schema` | `open` | Required. The applied schema version the gate must see. |
| `--role` | `open` | Required. The runtime login role whose privileges are verified. |
| `--rollout-verified` | `open` | Required. Asserts that every runtime replica runs the intended image on this schema and that older runtimes are fenced. The command cannot check this. |

`open` verifies the stored generation, the schema version and the role's privileges. It cannot leave `recovery` mode; use [`recovery open`](#recovery) for that. Failures name a category, never database text. See [Recovery and the admission gate](/operations/recovery).

### recovery

```sh
thinpost-control recovery <operation> [flags]
```

The coordinated-restore procedure. It requires a maintenance database role and records what you assert; it does not fence workers, isolate the database or verify your attestations. Never pass an attestation flag without having done the work. Procedure and rationale are in [Recovery and the admission gate](/operations/recovery). Operations:

| Operation | Purpose |
| --- | --- |
| `inspect` | Print the gate and recovery status. |
| `begin` | Enter recovery mode with a new generation and recovery ID. |
| `quarantine` | Place pending work on hold, in bounded batches; rerun until complete. |
| `report` | Page through the held entities. |
| `hold-history` | Page through the retained hold records of one entity. |
| `objects` | Read and authenticate stored objects referenced by the database. Also needs the [object storage](/reference/configuration#object-storage) variables. |
| `migrate-restored-schema` | Apply the missing migrations to a restored older schema while in recovery mode. |
| `open` | Leave recovery mode and open the gate. |

Flags, with the operations that require them:

| Flag | Default | Required by | Description |
| --- | --- | --- | --- |
| `--generation` | none | `report`, `objects`, `hold-history`, `migrate-restored-schema` (and expected by `begin`, `quarantine`, `open`) | New independent deployment generation: 32 lowercase hex characters. |
| `--recovery-id` | none | same as `--generation` | Recovery identifier: 32 lowercase hex characters. |
| `--actor` | none | `begin`, `quarantine`, `open`, `migrate-restored-schema` | Responsible operator. |
| `--expected-revision` | `0` | `begin`, `open`, `report`, `objects`, `hold-history`, `migrate-restored-schema` | The gate revision you inspected. Must be greater than zero. |
| `--workers-fenced` | `false` | `begin`, `open`, `objects`, `migrate-restored-schema` | Asserts old Edge workers, controllers, webhook dispatchers and collectors are fenced. |
| `--database-isolated` | `false` | `begin`, `open`, `objects`, `migrate-restored-schema` | Asserts the restored database is isolated from old runtimes. |
| `--evidence-reviewed` | `false` | `open` | Asserts the recovery evidence was reviewed. |
| `--objects-frozen` | `false` | `objects` | Asserts the restored bucket and keyring are stable and collectors are fenced. |
| `--limit` | `100` | `report`, `objects`, `hold-history` | Page size: `1` to `500` for `report`, `1` to `100` for `objects` and `hold-history`. |
| `--after-kind`, `--after-id`, `--after-secondary` | empty | none | Cursor for the next page of `report` (all three) or `objects` (`--after-kind` and `--after-id` only). Pass the previous page's `next` value. |
| `--kind`, `--entity-id`, `--secondary-id` | empty | `hold-history` (`--kind` and `--entity-id`) | The held entity whose history to read. `--secondary-id` is needed for webhooks. |
| `--after-recovery-id` | empty | none | Cursor for the next page of `hold-history`. |

State-changing operations print the resulting recovery status as JSON; `report`, `hold-history` and `objects` print one page as JSON. `objects` prints the page and then exits `1` when any reference failed to read or authenticate. Each operation checks that the database schema supports it and fails with `database schema check failed` otherwise. The time limit is five minutes.

### retry-policy

```sh
thinpost-control retry-policy inspect
```

Prints the stored delivery retry policy as JSON: `revision`, `changed_by` (when set), `max_attempts`, `initial_ms`, `max_delay_ms` and `max_age_ms`. `inspect` is the only argument. The policy is stored in the database and edited in the console (Operations); there is no command to change it. The control plane refuses to start when the old `THINPOST_DELIVERY_*` variables are set, see [Removed variables](/reference/configuration#removed-startup-fails).

### bootstrap-database

```sh
thinpost-control bootstrap-database --database <name> --schema <schema> --runtime-role <role> [--pooler-auth-role <role>]
```

Prepares one explicitly named, dedicated database: it limits the `PUBLIC` connection and temporary-table privileges, grants `CONNECT` to the specified roles and creates the protected application schema. It does not create tables, migrate or open the gate. Run it as the schema owner; the roles must already exist. See [Database](/deploy/database).

| Flag | Description |
| --- | --- |
| `--database` | Required. The dedicated database the connection must be to. |
| `--schema` | Required. Name of the protected application schema to create. |
| `--runtime-role` | Required. Existing runtime login role. |
| `--pooler-auth-role` | Optional. Existing role a connection pooler uses to authenticate. |

### grant-runtime

```sh
thinpost-control grant-runtime --role <role>
```

Grants the reviewed runtime operations to an existing login role. The database and role must already exist, `PUBLIC` must not have database `TEMP`, and unsafe existing grants or memberships are rejected rather than revoked. It is an explicit maintenance step, not part of startup migration. `--role` is required.

### grant-collector

```sh
thinpost-control grant-collector --role <role>
```

Grants the object collector's database privileges to an existing login role. `--role` is required.

### grant-tracking-retirement

```sh
thinpost-control grant-tracking-retirement --role <role>
```

Grants the privileges [`retire-tracking`](#retire-tracking) needs to an existing login role. `--role` is required.

The three `grant-*` commands print nothing on success; on failure they append the reason to the error. They use the schema owner's credentials and a five-minute limit.

### retire-tracking

```sh
thinpost-control retire-tracking --actor <identity> --generation <generation> [--limit 100]
```

Retires expired tracking references in one bounded batch and prints `{"retired": <count>}`. It retires references only: it does not delete stored objects, authenticate the claimed actor or make a restored database safe. It first checks the database schema and that the connecting role has exactly the tracking-retirement privileges.

| Flag | Default | Description |
| --- | --- | --- |
| `--actor` | none | Required. The authenticated operator or service identity, recorded in the audit log. |
| `--generation` | none | Required. The current deployment generation. |
| `--limit` | `100` | Maximum references in this batch, `1` to `100`. |

## thinpost-edge

```text
thinpost-edge                         run the Edge worker
thinpost-edge healthcheck             readiness probe
thinpost-edge healthcheck liveness    liveness probe
thinpost-edge journal-id <path>       print the journal identity
```

The Edge refuses to run as root. An unknown subcommand fails with `unknown edge command`.

### healthcheck

```sh
thinpost-edge healthcheck
thinpost-edge healthcheck liveness
```

Makes an HTTP `GET` to the Edge's loopback health listener (`127.0.0.1:8081`), ignoring proxy settings, with a two-second limit. It exits `0` only on `204 No Content`, and `1` otherwise. They are meant for exec probes.

| Probe | Path | Healthy when |
| --- | --- | --- |
| `healthcheck` | `/readyz` | The journal is readable and, when `THINPOST_EDGE_SOURCE_GATEWAYS` is set, every source's gateway has resolved. |
| `healthcheck liveness` | `/livez` | The journal alone is readable. A gateway that has not resolved delays readiness but never restarts the process. |

The journal check runs in the background every five seconds and is valid for fifteen, so a blocked filesystem turns the probe unhealthy rather than hanging it. It does not check free space, fencing, public SMTP reachability or the control session.

### journal-id

```sh
thinpost-edge journal-id /var/lib/thinpost/journal
```

Opens the journal at an absolute path and prints its 32-character identity, creating an empty journal if the file does not exist. Use it to find the value for the control plane's `THINPOST_CONTROL_JOURNAL_ID` before the Edge first starts. Run it only while no worker has the journal open.
