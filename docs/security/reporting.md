---
title: Reporting vulnerabilities
description: How to privately report a suspected security issue in Thinpost.
---

# Reporting vulnerabilities

Thinpost is pre-1.0. Security fixes land on `main` and ship in the next release. The canonical policy is [`SECURITY.md`](https://github.com/zyno-io/thinpost/blob/main/SECURITY.md) in the repository root.

## How to report

Report suspected vulnerabilities **privately**:

- Open a private report with [GitHub private vulnerability reporting](https://github.com/zyno-io/thinpost/security/advisories/new) (the repository's **Security** tab, then **Report a vulnerability**).
- Or email [oss@zyno.io](mailto:oss@zyno.io) if you cannot use GitHub.

Please include the affected version or commit, a minimal reproduction, and the impact you expect. Say how you would like to be credited, if at all.

## What not to do

- Do not open a public issue or pull request that describes an unfixed vulnerability.
- Do not include credentials, private keys, tokens, real message content or personal data in a report. Use synthetic data.
- Do not test against installations that you do not operate, and do not access, modify or delete other people's data.

## What to expect

The maintainers acknowledge reports, assess them and coordinate a fix and disclosure with you. Because Thinpost is pre-1.0 and community-maintained, response times are best effort.

## Scope

In scope: the control plane, the edge worker, the operator console, the admin and app APIs, the provider-compatible submission APIs, the control protocol, the Helm chart and the published container images.

Out of scope: weaknesses in your own deployment, such as exposed credentials, a missing network policy or a permissive PostgreSQL role, and issues that need an attacker who already holds the administrator role or direct access to the database, object store or keyring. If you are unsure, report it anyway.

See the [threat model](/security/threat-model) for the intended security properties and [data handling](/security/data-handling) for what Thinpost logs and stores.
