---
title: Upgrades
description: How Thinpost schema migrations run during a Helm upgrade, how the admission gate closes and reopens, what failures leave behind, and why not to roll back.
---

# Upgrades

A Thinpost upgrade is a Helm upgrade of the control release and, separately, of the edge release. The control release carries the database migration: two Helm hook Jobs run the control image's `deploy-migrate` and `deploy-reopen` commands around the rollout, with the **admission gate** closed for the short time the schema is changing. The edge release has no hooks and no database access.

You need nothing besides a normal `helm upgrade` for most upgrades. Read this page before the first one, because the failure modes are different from an ordinary application.

## The short version

- Upgrade by moving **forward**. **Never roll back with Helm.** A rollback puts old pods on a newer schema.
- A deploy that needs no schema change never touches the gate.
- If a hook fails, the table below says what state you are in and what to do. Re-running the upgrade is the usual fix.
- Set a Helm or HelmRelease timeout of at least 15 minutes, and turn off automatic rollback.

## What the hooks do

The control chart renders two Jobs. They exist only for `workload=control` with `control.migrations.enabled` (the default), at least one replica and a real `control.image`. See [Helm: control](/deploy/helm-control#hooks).

| Hook | Runs | Command |
| --- | --- | --- |
| `thinpost-control-migrate` | `pre-install`, `pre-upgrade`, before any pod changes | `thinpost-control deploy-migrate` |
| `thinpost-control-gate-reopen` | `post-install`, `post-upgrade`, after Helm's wait succeeds | `thinpost-control deploy-reopen` |

Both run as the schema owner and share one **actor**, `deploy:<namespace>/<release>:r<revision>`. The pre hook records this actor when it closes the gate, and the post hook reopens only a gate closed by its own actor. Only the Jobs mount the owner Secret `thinpost-db-owner`; the long-running Deployment never does.

Helm deletes each Job before the next hook creates it, so the last Job stays for its logs.

### The pre hook

1. **Preflight.** It reads the database and refuses to continue in these cases: the migration ledger does not exist (a new database is set up by hand, see [Database](/deploy/database)), the database is newer than the image or a recorded migration was modified, the gate is in `recovery`, the schema is older than the oldest version a deploy may migrate (27), a pending migration is marked manual, or the gate's stored generation differs from `THINPOST_DEPLOYMENT_GENERATION`.
2. **No change needed.** If the schema already matches the image, it exits with action `current` and never touches the gate. A closure left by an earlier attempt of the same release is re-adopted, see below.
3. **Drain.** When crossing schema 28, it waits up to two minutes for `queued` and `preparing` messages to finish, because older source objects cannot be read after that schema. This applies only to upgrades from before schema 28.
4. **Close.** It closes the gate with a compare-and-swap on the gate revision. The row lock waits up to 30 seconds for runtime transactions that were already admitted.
5. **Migrate.** It re-checks under the closure, then applies the migrations, **re-applies the runtime grants** and runs the privilege check in **one transaction**, advances the gate revision and records a `deploy_migrate_<from>_to_<to>` event.

The closed gate fences old pods. Their SQL fails against the new schema, and an old pod that restarts fails its schema check. Nothing scales them down. The new Deployment's pods are created only after the pre hook succeeds, so every new pod starts on the target schema.

While the gate is closed, submissions, preparation, SMTP claims and webhook dispatch pause, and history and content reads fail. The window is short, well under a minute for typical migrations, but grows with the size of the `messages` and `deliveries` tables. Accepted mail is not lost: submissions during the window are refused, and queued work resumes when the gate reopens.

### The post hook

After Helm's own wait succeeds, the post hook waits up to five minutes until exactly `control.replicas` pods are Running and Ready on the Deployment's exact image string, with none terminating or in the `Unknown` phase. It checks the runtime role's privileges and then reopens the gate. The pre hook closes the gate only when the schema had to change, so on most deploys the post hook finds nothing it owns and does nothing.

### Every upgrade re-runs both hooks

Any `helm upgrade` runs both hooks, including a values-only change. They are no-ops when the schema matches and the gate is open. They also finish a pending reopen from an interrupted run: the pre hook re-adopts a closure left by an earlier attempt of the same release, and the post hook reopens it after the rollout.

An operator's own closure, made with `thinpost-control gate close`, is migrated under but never reopened by a deploy.

## Reasons a hook refuses or fails

A failed hook writes only a fixed reason code to the pod's termination message. Read it with `kubectl describe pod` on the Job's pod, or the Job's logs. The hooks never print database error text, parameters, addresses or subjects.

| Reason | Meaning |
| --- | --- |
| `ledger_missing` | The database has no migration ledger. Run the first-install steps in [Database](/deploy/database). |
| `database_newer_or_unknown` | The database has a migration this image does not know. Deploy a newer image. |
| `migration_checksum_mismatch` | A recorded migration differs from the one in the image. |
| `recovery_in_progress` | The gate is in recovery mode. Finish the [recovery](/operations/recovery). |
| `below_migration_floor` | The schema is older than a deploy may migrate. Migrate it by hand first. |
| `manual_migration_required` | A pending migration must not be applied by a deploy. |
| `generation_mismatch`, `generation_missing` | The stored generation differs from `THINPOST_DEPLOYMENT_GENERATION`, or none is stored. |
| `drain_timeout`, `unprepared_messages` | Messages did not finish preparing in time. Re-run when the queue is calmer. |
| `gate_close_failed`, `stale_deploy_closure` | The gate changed concurrently, or an earlier closure is older than 24 hours. |
| `migrate_failed` | The migration transaction failed and rolled back. |
| `rollout_timeout`, `kubernetes_unavailable` | The post hook could not confirm the rollout within five minutes. |
| `runtime_privileges_invalid`, `gate_reopen_failed` | The runtime role has unsafe privileges, or the reopen failed. |
| `schema_not_current` | The post hook ran while the schema was behind the image. |

## Failure handling

| Failure point | Schema | Gate | Recovery |
| --- | --- | --- | --- |
| Preflight refusal | Unchanged | Unchanged | Fix the cause and run the upgrade again. |
| Drain or close times out | Unchanged | Open | Run the upgrade again. |
| Messages pending under closure, or the migrate and grant transaction fails | Unchanged | Restored by fail-back | Investigate, then run the upgrade again. |
| Job killed before the commit | Unchanged | Closed by the deploy | Run the upgrade again, which re-adopts the closure, or open the gate by hand. |
| Job killed after the commit, or the Helm apply or wait fails | At the target version | Closed by the deploy | Fix the cause and run the upgrade again: a no-op migration, the rollout and the reopen. |
| Post hook fails | At the target version | Closed by the deploy | Run the upgrade again, or open the gate by hand once you verify the rollout. |

**Fail-back** reopens the gate only if this run closed it, the schema is verified unchanged and the commit outcome is not ambiguous. It uses a fresh 30-second context so that a termination signal cannot strand the gate. If the outcome is ambiguous, the gate stays closed.

A closure left by an earlier attempt of the same release, less than 24 hours old, is adopted by re-stamping the actor so that this revision's post hook reopens it. Adoption changes only the actor and revision. The closure time stays what it was, so the 24-hour limit measures from the original closure and repeated adoption cannot extend it.

### Inspect and open by hand

To see what a deploy would do without changing anything, run the pre hook in check mode, using a dummy actor. It needs the hook's environment: the owner database settings, `THINPOST_RUNTIME_ROLE` and `THINPOST_DEPLOYMENT_GENERATION`.

```sh
thinpost-control deploy-migrate --actor deploy:check/manual:r0 --check
```

To finish a deploy whose post hook failed, or to run an attended window, use the `gate` commands with the owner credential. Always inspect first, because every change is a compare-and-swap on the revision:

```sh
thinpost-control gate inspect
thinpost-control gate open --actor <operator> --expected-revision <revision from inspect> \
  --generation <configured generation> --expected-schema <applied schema version> \
  --role <runtime role> --rollout-verified
```

`gate open` verifies the stored generation, the applied schema version and the runtime role's privileges. `--rollout-verified` attests that every runtime replica runs the intended image on this schema and that older runtimes are fenced. It cannot leave `recovery` mode; use the `recovery` commands for that.

Alert when a deploy-owned closure lasts more than 30 minutes. See [Monitoring](/operations/monitoring).

## Never roll back with Helm

A Helm rollback, or any tool's automatic rollback, puts old pods against the newer schema. They cannot run there, and the older image does not migrate backward.

- Do not use `helm rollback`.
- Do not use `helm upgrade --atomic` (`--rollback-on-failure` in Helm 4).
- If a GitOps controller applies the release through a `HelmRelease` resource, set the install and upgrade **remediation retries to 0** and disable remediation of the last failure, so a failed upgrade is not undone automatically.

Fix forward instead: correct the cause and run the upgrade again. A failed deploy leaves the gate closed only when the schema may have changed, and the table above says what to do.

Thinpost takes **no backup before a migration**. If you must return to the previous state, restore PostgreSQL with point-in-time recovery to a moment before the gate was closed (the `occurred_at` of the `gate_close` event), then follow the [recovery](/operations/recovery) procedure. Keep your PostgreSQL backup and write-ahead log archive healthy before every upgrade.

## Helm and HelmRelease timeouts

The hooks make an upgrade slower than a plain rollout, and Helm waits for them. Set the timeout above the sum of the hook limits.

| Phase | Limit |
| --- | --- |
| Pre hook | 2 minute drain, 30 second close wait, 60 second migration lock wait and an 8 minute overall limit. The Job deadline is `control.migrations.activeDeadlineSeconds` (default 600) with no retry. |
| Rollout | Helm's own wait for the Deployment. |
| Post hook | 5 minute rollout wait and an 8 minute overall limit. The same Job deadline applies. |

Use at least **15 minutes** in total, 20 for headroom:

```sh
helm upgrade --install thinpost-control zyno/thinpost --version <chart version> \
  --namespace thinpost-control --values values-control.yaml \
  --wait --timeout 20m
```

For a `HelmRelease` resource, set `spec.timeout` the same way. A timeout shorter than the hooks makes Helm report a failure while the Jobs are still working, and with remediation on, it can try to roll back.

## Upgrading the edge

The edge release is independent. Its Deployment uses `Recreate`, so an upgrade stops the old pod and starts a new one, and sending pauses briefly. Bounce senders retry. The control plane reconnects and the edge replays any results it has not had acknowledged.

Control and edge may run different versions while you roll one after the other, and either order is safe unless a release note says otherwise. A rollout across several clusters is not atomic, so keep changes compatible across overlapping versions. Once a receiving domain is live, treat the edge as roll-forward only: an edge that predates inbound mail answers `550` for such a domain, which is permanent for the sender. See [Helm: Edge](/deploy/helm-edge).

### Schema 43 and the edge's policy settings

Schema 43 adds the console test-send columns on `messages` (catalog-only, no table rewrite). It also moves the recipient allowlist and the egress policy to the control plane alone. The edge no longer reads `THINPOST_RECIPIENT_POLICY`, `THINPOST_RECIPIENT_ALLOWLIST_FILE`, `THINPOST_EDGE_INFRASTRUCTURE_CIDRS` or `THINPOST_EDGE_ROUTE_EXCEPTIONS_FILE`, and the chart no longer maps them into the edge. An edge that still sets them starts normally and ignores them. An older edge keeps enforcing its own copy of both, so the two versions can overlap in either order; control still handles the `RECIPIENT_POLICY_DENIED` outcome an older edge reports. Before you upgrade the edge, make sure the network around it allows only outbound TCP 25 to public destinations, because the edge no longer checks destinations itself (see the [threat model](/security/threat-model#outbound-smtp-egress-policy)).

These jobs never initialize or reset a database, a journal or a keyring.

## Next

- [Helm: control](/deploy/helm-control)
- [Database](/deploy/database)
- [Recovery](/operations/recovery)
