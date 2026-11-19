---
title: OpenAPI contracts
description: Where to find and download the machine-readable OpenAPI contracts for the Thinpost admin and app APIs.
---

# OpenAPI contracts

Thinpost describes its HTTP APIs with OpenAPI 3.1 documents in the repository under `api/`. They are MIT-licensed, so you can generate clients and mocks from them.

| Contract | Served at | Rendered reference | Download | Source |
| --- | --- | --- | --- | --- |
| Admin (operator) API | `/api/v1` | [Admin API](/reference/api/admin) | [YAML](/openapi/admin.yaml), [JSON](/openapi/admin.json) | [`api/admin/openapi.yaml`](https://github.com/zyno-io/thinpost/blob/main/api/admin/openapi.yaml) |
| App API | `/app/v1` | [App API](/reference/api/app) | [YAML](/openapi/app.yaml), [JSON](/openapi/app.json) | [`api/app/openapi.yaml`](https://github.com/zyno-io/thinpost/blob/main/api/app/openapi.yaml) |

The downloads are the same contracts as the source files. The only difference is that the app download includes copies of the message schemas it shares with the admin contract, because the source file references them by relative path.

Two interfaces are described elsewhere:

- The **submission routes** are the Postmark-, Resend-, SendGrid- and Mailgun-compatible endpoints and follow the providers' own APIs. See [Provider compatibility](/reference/compatibility/).
- The **control protocol** between the control plane and the Edge is gRPC. See [Control protocol](/reference/control-protocol).

The prose pages explain behavior that a schema cannot express. When a page and the contract disagree, the contract and the handlers in the source are authoritative; please open an issue.
