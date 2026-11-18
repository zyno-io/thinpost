---
title: Monitoring and health
description: Health endpoints, mail readiness semantics, what Thinpost logs, and the alerts worth setting up.
---

# Monitoring and health

Thinpost exposes health endpoints on both processes, a read-only operations snapshot in the admin API and the console, and privacy-restricted logs. It does not expose a metrics endpoint, so alerting builds on probes, the operations snapshot and log lines.

## Control plane endpoints

The control plane serves these on its plain HTTP listener, and on the optional in-cluster HTTPS listener when [it is enabled](/deploy/helm-control#the-https-listener).

| Path | Success | Meaning |
| --- | --- | --- |
| `/livez` | `204` | The process is running and serving HTTP. It checks nothing else. Use it for a liveness probe. |
| `/operator-readyz` | `204` | Recent database checks succeeded, so the operator API and console can work. `503` otherwise. |
| `/readyz` | `204` | Mail readiness. `503` when submission is not configured or when the database gate does not admit this replica. |

### Operator readiness

`/operator-readyz` reflects the result of a background database check that runs on a bounded interval, not once per request. A check that stalls expires its own success, so a hung database turns the endpoint to `503` instead of leaving a stale `204`. It does not assert mail, object-storage or identity-provider readiness.

### Mail readiness

`/readyz` is `204` only when all of these hold:

- Submission is configured on the replica.
- The database [admission gate](/operations/recovery#the-admission-gate) is open and its generation matches the replica's `THINPOST_DEPLOYMENT_GENERATION`. The check is cached and refreshed in the background.

It stays ready when the Edge worker is down. Accepted messages are durably queued, so submission keeps working and deliveries resume when the Edge reconnects. Readiness therefore tells you whether the replica can admit mail, not whether mail is flowing. It does not prove object-storage permissions or delivery.

Without submission configured, `/readyz` always returns `503`. Operator-only replicas, which have no mail runtime, are expected to be unready on it, so probe `/operator-readyz` for them.

::: tip Probe an unknown path with care
The console falls back to `index.html` for extensionless paths, so an unknown path can return `200`. Probe only the paths above.
:::

## Edge worker endpoints

The Edge worker listens on `127.0.0.1:8081` inside its pod for local probes.

| Path | Success | Meaning |
| --- | --- | --- |
| `/livez` | `204` | The journal is healthy. A source-address neighbour that has not resolved yet never fails it. |
| `/readyz` | `204` | The journal is healthy and the configured sending-IP neighbours resolved. |

Because the endpoints are loopback-only, the image provides `thinpost-edge healthcheck` for readiness and `thinpost-edge healthcheck liveness` for liveness, to use as exec probes.

## The operations snapshot

`GET /api/v1/operations` returns, in one read-only transaction with a short statement timeout:

- the gate mode, revision, who changed it and when, and whether its generation matches;
- each Edge worker lease and whether it is live by the database clock;
- message, delivery and webhook backlogs, including the oldest due time and counts of uncertain and held work;
- today's inbound counters and the retry policy.

It works while the gate is closed, returns counts and IDs only, and is not audited. `GET /api/v1/operations/attention` lists messages that need an operator. The console's Operations page shows the same data and refreshes while visible.

A live worker lease means the Edge holds an unexpired lease. It does not prove SMTP reachability.

## Recommended alerts

| Alert | Signal | Why |
| --- | --- | --- |
| Admission gate closed by a deploy for more than 30 minutes | `gate.mode` is `closed` and `changed_by` starts with `deploy:` | A deploy closes the gate for a short window. A longer closure means the post-deploy reopen did not happen. See [Recovery](/operations/recovery). |
| Gate closed by an operator for longer than planned | `gate.mode` is `closed` or `recovery` | Mail is not being admitted or sent. |
| Generation mismatch | `gate.generation_matches` is `false` | Replicas fail closed until the configured and stored generations agree. |
| Edge disconnected | No worker with `live: true` for more than a few minutes | Deliveries are queued and not sent. |
| Delivery backlog | `deliveries.oldest_due_at` older than your tolerance, or `queued_due` growing | Sending is stalled or slower than submission. |
| Message preparation failing | `messages.preparation_failed` greater than zero | A message is held and needs an operator. |
| Uncertain deliveries | `deliveries.uncertain` greater than zero | An SMTP attempt has an unknown outcome and needs an operator decision. |
| Webhook backlog | `webhooks.oldest_due_at` growing | Receivers are failing or the dispatcher is stalled. |
| Control replica not ready | `/operator-readyz` or `/readyz` returns `503` for several probes | Database trouble, missing generation or configuration. |
| Certificate expiry | Expiry of the control client, Edge and SMTP certificates | The control session closes before a verified certificate chain expires. See [Rotation](/operations/rotation). |
| Journal volume filling | Volume usage on the Edge journal | The Edge records every attempt in its journal and cannot do that on a full volume. |

Poll the operations snapshot with an administrator token on an interval of about a minute. Keep the polling identity separate from human operators.

## Logs

Both processes write to stdout and stderr. Logs carry no message content and no addresses. The full rules are in [Data handling](/security/data-handling#what-is-logged).

Lifecycle lines contain only 32-hex IDs and enumerated values:

```text
message app=<id> message=<id> status=accepted
message app=<id> message=<id> status=prepared
message app=<id> message=<id> status=preparation_failed reason=<enum>
message app=<id> message=<id> status=preparation_lease_lost
delivery app=<id> message=<id> delivery=<id> status=<outcome> smtp=<code> ip=<name>
```

`ip` is the name of the sending IP, `primary` for the Edge's primary address when it has no name, and `other` for anything that is not a valid name. Alert on `preparation_failed` and on spikes in non-success delivery outcomes, and use the IDs to look up the message in the console.

Rules that keep logs safe to ship to a shared log system:

- Remote SMTP reply text is discarded. Only the stage and reply code are logged.
- HTTP panics log a route class and a stack, never the URL.
- Both binaries refuse to start when `GRPC_BINARY_LOG_FILTER` is set, because gRPC binary logs record recipients and MIME.
- Never enable client tracing (S3 client trace, database driver tracers, HTTP client dumps).

Deploy hook Jobs log fixed reason codes, migration numbers and SQLSTATEs. They never log database message text.

### PostgreSQL logging

Set `log_parameter_max_length=0` and `log_parameter_max_length_on_error=0` on the database, and do not use `log_statement=all` or an audit extension that logs parameters. Otherwise bind values, such as subjects, recipients and search terms, reach the server log. If you load `auto_explain`, set `auto_explain.log_parameter_max_length=0`. Constraint-violation `DETAIL` lines can quote row values, so consider `log_error_verbosity=terse`. Thinpost rejects `default_query_exec_mode=simple_protocol` for the same reason. Deploys do not check these server settings.
