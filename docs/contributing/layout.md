---
title: Repository layout
description: A map of the Thinpost repository, its top-level directories and the main Go packages.
---

# Repository layout

Thinpost is one Go module, `github.com/zyno-io/thinpost`, with a Vue console, a Helm chart, test tooling and this documentation site.

## Top-level directories

| Path | What it is |
| --- | --- |
| `cmd/thinpost-control/` | The control plane server and its maintenance subcommands (`migrate`, `gate`, `recovery`, `grant-*`, `retire-tracking` and others) |
| `cmd/thinpost-edge/` | The edge worker, plus `healthcheck` and `journal-id` subcommands |
| `internal/` | All Go packages. Nothing is importable from outside the module |
| `api/admin/openapi.yaml` | The operator (admin) API contract. The console client is generated from it |
| `api/app/openapi.yaml` | The read-only app API contract |
| `api/control/v1/` | The control protocol `.proto` and its generated Go code |
| `ui/` | The Vue operator console |
| `resources/helm/chart/` | The Helm chart, with `values.schema.json` |
| `scripts/` | Test and verification scripts, and `gen-proto.sh` |
| `tests/sdk-contract/` | Tests that drive the real Postmark, Resend, SendGrid and Mailgun SDKs against a fixture server |
| `docs/` | This VitePress site |
| `compose.yaml`, `examples/quickstart/` | The local evaluation stack from the [quickstart](/guide/quickstart), including a development-only sign-in service |
| `Dockerfile` | Multi-stage build with `control` and `edge` targets |

## Key Go packages

Packages under `internal/` group by responsibility.

**Process wiring**

| Package | Role |
| --- | --- |
| `controlapp` | Control process configuration, routing, console serving and certificate reload |
| `controlbridge` | Composes one session from the control plane to the edge |
| `edgeapp` | Edge process configuration |
| `deployhook` | The Helm hook steps that migrate the schema and reopen the gate |
| `envguard` | Refuses environments that would log message data |

**Admission and storage**

| Package | Role |
| --- | --- |
| `httpapi` | Provider-compatible submission adapters |
| `admin` | The operator API |
| `acceptance`, `submission` | Transactional acceptance of messages |
| `postgres` | Migrations, repositories, the gate, recovery, grants |
| `objectstore`, `objectkey`, `objectgc` | Encrypted object storage, key naming and the unscheduled orphan sweep |
| `apikey`, `operatorauth` | App API keys and operator token validation |

**Preparing and delivering mail**

| Package | Role |
| --- | --- |
| `preparation`, `message`, `signing`, `tracking` | MIME building, DKIM signing and tracking links |
| `dispatch`, `delivery`, `mailroute`, `recipientpolicy` | Delivery attempts, MX resolution and recipient policy |
| `egress`, `smtptransport` | The outbound destination policy and a single SMTP attempt |
| `webhook` | Webhook events, formats and dispatch |

**Control protocol**

| Package | Role |
| --- | --- |
| `controlsession`, `controllease`, `leaseproof`, `controltls` | The control-side session, database leases and signed proofs, and mTLS configuration |
| `edgesession`, `edgeprotocol`, `edgeworker`, `journal` | The edge-side stream handler, frame validation, delivery execution and attempt journal |

**Receiving mail**

| Package | Role |
| --- | --- |
| `bounce`, `bounceaddress`, `bounceingest` | The edge SMTP receiver and return-path tokens |
| `inbound`, `feedback` | Inbound mail for receiving domains and spam-complaint reports |

## Conventions

- Tests sit next to the code they test. Integration tests that need PostgreSQL or an S3 API skip when their environment variable is unset.
- The edge process has no configuration for database or object-store credentials. Keep it that way: new edge features must not need them.
- Generated files carry a header and are never edited by hand. See [Generated code](/contributing/generated-code).
