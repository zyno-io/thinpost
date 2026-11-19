---
title: App API
description: The message API for an app's API key, reads and the release or cancel of held messages, rendered from its OpenAPI contract.
---

<script setup>
import { data } from './specs.data'
</script>

# App API

The app API reads back messages that an app submitted, and releases or cancels messages the app [submitted on hold](/guide/holding-messages). It is served by the control plane under `/app/v1`, on the same listener as the submission routes, and only when submission is configured.

- **Authentication.** The bearer token is a submission API key of the app. The key identifies the app: a message that belongs to another app is reported as not found.
- **Intended callers.** The API is meant for in-cluster services, so a deployment does not have to expose `/app/` on its public ingress.
- **Methods.** Reads are `GET`; `release` and `cancel` are `POST` with no body. A query string is rejected. Errors are `{"error": "<status text>"}`, except that a release or cancel that cannot apply answers `409` with a reason code such as `{"error": "hold_expired"}`.
- **Audit.** Reads of an Enhanced Protection message's content, and every release or cancel that changes a message, are audited with the actor `key:<key ID>`.
- **Contract.** The reference below is generated from the OpenAPI 3.1 contract in the repository. Its message schemas are shared with the [Admin API](/reference/api/admin); the download bundles them so the file stands alone. Download it as [YAML](/openapi/app.yaml) or [JSON](/openapi/app.json), or read [`api/app/openapi.yaml`](https://github.com/zyno-io/thinpost/blob/main/api/app/openapi.yaml) on GitHub. See [OpenAPI contracts](/reference/openapi).

<OASpec :spec="data.app" />
