---
title: Admin API
description: The operator API that the console uses, rendered from its OpenAPI contract.
---

<script setup>
import { data } from './specs.data'
</script>

# Admin API

The control plane serves the operator API under `/api/v1`. The [console](/guide/console) is its main client, and it is the interface for managing apps, API keys, sender domains, sending IPs, webhooks, suppressions, inbound domains, messages and operations.

- **Authentication.** Every operation except `GET /api/v1/config` requires a bearer access token from your OpenID Connect provider, validated as described in [Environment variables](/reference/configuration#operator-authentication-oidc). Only tokens that carry the administrator role are accepted. API-key secrets are returned only when a key is created.
- **Submitting mail** is not part of this API. Applications send through the [provider-compatible routes](/reference/compatibility/) with an app API key, and read messages back with the [App API](/reference/api/app).
- **Contract.** The reference below is generated from the OpenAPI 3.1 contract in the repository. Download it as [YAML](/openapi/admin.yaml) or [JSON](/openapi/admin.json), or read [`api/admin/openapi.yaml`](https://github.com/zyno-io/thinpost/blob/main/api/admin/openapi.yaml) on GitHub. See [OpenAPI contracts](/reference/openapi).

<OASpec :spec="data.admin" />
