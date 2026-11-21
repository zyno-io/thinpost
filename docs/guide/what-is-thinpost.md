---
title: What is Thinpost
description: What Thinpost is, who it is for, what it does not do, and how mature it is.
---

# What is Thinpost

Thinpost is a self-hosted email delivery platform for application email: receipts, password resets, notifications and the replies to them. You run it, you own the keys, and your applications talk to it over HTTPS using the same SDKs they would use against a hosted provider.

It has two processes:

- **Control plane** (`thinpost-control`). Authenticates apps, accepts and stores messages, signs them with DKIM, decides what to deliver and when, dispatches webhooks, and serves the operator console and admin API.
- **Edge worker** (`thinpost-edge`). Opens outbound SMTP connections from your sending IP addresses and receives bounces and inbound mail on port 25. It is deliberately small and has no database or object-storage credentials.

State lives in your PostgreSQL and in an S3-compatible object store. See [Architecture](/guide/architecture) for how the pieces fit.

## What you get

- **Provider-compatible submission APIs.** Postmark, Resend, SendGrid and Mailgun send routes, tested against each provider's official Node SDK. See [Sending with SDKs](/guide/sending-with-sdks).
- **Applications as security principals.** Each app has its own API keys, sender domains, quotas, webhooks, tracking defaults, suppression list and optional sending IP. A key can only act for its own app.
- **Sender-domain verification and DKIM signing.** You prove control of a domain with a DNS TXT record and Thinpost signs every message. See [Sender domains](/guide/domains).
- **Sending IPs.** Separate apps can send from separate addresses, so one app's reputation does not affect another's. See [Sending IPs](/guide/sending-ips).
- **Webhooks.** Signed, retried, replayable delivery events in Thinpost-native, Postmark, Resend or Mailgun format. See [Webhooks](/guide/webhooks).
- **Open and click tracking and one-click unsubscribe,** with encrypted self-contained links. See [Open and click tracking](/guide/tracking).
- **Suppressions** that stop mail to addresses that bounced, complained or unsubscribed. See [Suppressions](/guide/suppressions).
- **Inbound mail** for domains you own, delivered to your app as a webhook. See [Inbound mail](/guide/inbound).
- **An operator console** for everything above. See [Operator console](/guide/console).
- **Safe recovery.** An admission gate and coordinated-restore procedure keep a database or journal restore from silently re-sending mail. See [Recovery](/operations/recovery).

## What it is not

- **Not a hosted service.** There is no shared Thinpost cloud. You deploy and operate it.
- **Not a full clone of any provider.** Thinpost implements a documented subset of each provider's send API and webhook payloads. Templates, SendGrid substitutions and SendGrid webhooks, for example, are not available. Deviations are listed in [Provider compatibility](/reference/compatibility/).
- **Not a marketing platform.** It is built for transactional mail. It has no campaign tooling, list management or audience segmentation.
- **Not a mailbox or groupware server.** Inbound mail is delivered to your application, not stored for people to read in a mail client.
- **Not a deliverability guarantee.** Inbox placement depends on your sending reputation, DNS and content. Thinpost gives you the controls (dedicated IPs, DKIM, SPF alignment, complaint handling), not the outcome.

## Requirements in brief

You need Kubernetes (or a container runtime for local trials), PostgreSQL 17 or later, an S3-compatible object store, an OpenID Connect identity provider for operators, and at least one public IPv4 address with reverse DNS that you can use for outbound mail. See [Topology and prerequisites](/deploy/) for the full list.

## Maturity

Thinpost is pre-1.0 and runs in production for its maintainers. APIs and configuration may change between minor versions. Individual features vary in how long they have been in use; check the [roadmap](https://github.com/zyno-io/thinpost/blob/main/ROADMAP.md) for areas still being built and [Provider compatibility](/reference/compatibility/) for what each adapter does and does not cover.

## Next

- [Concepts](/guide/concepts): the vocabulary used throughout the docs.
- [Quickstart](/guide/quickstart): send a message on your own machine.
