---
layout: home
hero:
  name: Thinpost
  text: Self-hosted transactional email
  tagline: Send through the Postmark, Resend, SendGrid or Mailgun SDK you already use. Keep delivery, keys and message content on infrastructure you run.
  actions:
    - theme: brand
      text: What is Thinpost
      link: /guide/what-is-thinpost
    - theme: alt
      text: Quickstart
      link: /guide/quickstart
    - theme: alt
      text: Deploy
      link: /deploy/
features:
  - title: Drop-in provider APIs
    details: Submission routes for Postmark, Resend, SendGrid and Mailgun, with matching webhook formats. Point an existing SDK at Thinpost and keep your application code.
  - title: Apps, keys and sender domains
    details: Every application is its own security principal with individual API keys, verified sender domains, quotas and webhooks. Administrators manage them in a console or over an admin API.
  - title: Split control plane and SMTP edge
    details: A Go control plane owns keys, queues, signing and storage. A small edge worker only speaks SMTP. It holds no database or object-storage credentials and never dials into your trusted network.
  - title: Durable and auditable
    details: PostgreSQL for state, S3-compatible storage for encrypted message bodies, an on-disk journal at the edge, and an admission gate for coordinated restores.
  - title: Webhooks, tracking, suppressions, inbound
    details: Signed webhooks with replay, opt-in open and click tracking, one-click unsubscribe, bounce and complaint suppression, and inbound mail delivered to your app.
  - title: Privacy by default
    details: Logs carry IDs and enumerated states, never addresses or subjects. Enhanced Protection forces validated TLS and audits every read of message content.
---

::: warning Status
Thinpost is pre-1.0. It runs in production for its maintainers. APIs and configuration may change between minor versions, so read the release notes before upgrading.
:::

## Where to go next

- **New here?** Read [What is Thinpost](/guide/what-is-thinpost), then run the [Quickstart](/guide/quickstart) to send a message on your own machine.
- **Migrating an app?** See [Sending with SDKs](/guide/sending-with-sdks).
- **Running it for real?** Start with [Topology and prerequisites](/deploy/).
- **Looking something up?** The [Reference](/reference/configuration) section covers configuration, the HTTP API and provider compatibility.

Thinpost is MIT licensed and developed in the open at [github.com/zyno-io/thinpost](https://github.com/zyno-io/thinpost).
