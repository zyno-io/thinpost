---
title: Concepts
description: The vocabulary Thinpost uses for apps, keys, domains, sending IPs, messages, deliveries, Enhanced Protection and the admission gate.
---

# Concepts

## Apps

An **app** is the unit of isolation. It owns its API keys, sender domains, receiving domains, webhook endpoints, tracking defaults, suppression list, quotas and (optionally) a sending IP. A key for one app cannot read or send for another, and a message ID from another app answers 404, the same as one that does not exist.

Administrators create and pause apps in the [console](/guide/console) or the admin API. A **paused** app accepts no new submissions and starts no new SMTP attempts or webhook claims; work already in flight can finish, and queued mail is retained. Resuming makes the retained work and any unrevoked keys eligible again.

## Operators and administrators

An **operator** is a person or tool using the admin API and console. Operators sign in through your OpenID Connect provider and must carry the configured administrator role. App keys are never accepted as operator credentials. See [Operator authentication](/deploy/operator-auth).

## API keys

Each app has one or more **API keys**. A key is shown once, when it is issued; the database stores only a digest. Revoking a key takes effect for new requests. The key is presented in whatever form the provider SDK expects: a Postmark server token header, or a bearer token for Resend and SendGrid, or the Mailgun HTTP Basic password.

## SMTP credentials

An **SMTP credential** is a login for sending an app's mail over SMTP, for software that has no HTTP API, such as printers and scanners. Its username is the credential's ID and its password is shown once, when it is created. The database stores only a digest, and credentials are limited to 10 active per app. A credential can only send mail for its own app. It can't read messages, call the API or sign in to the console. Revoking a credential stops it signing in; messages it already sent keep its name.

An app also has a **From address policy** for SMTP mail: accept only From addresses on the app's verified sender domains, move the From address onto one of those domains (the original address stays in the name part, so `scanner@office.example.net` becomes `scanner=office.example.net@example.com`), or always send from one fixed address. When the policy replaces a From address, the message records the address the client sent as its **original From**.

::: warning Not available yet
The SMTP listener is not part of Thinpost yet. The admin API can already create and revoke credentials and set the From policy, but no mail can be sent over SMTP until the listener ships, and the console shows the SMTP tab only on a deployment that reports `smtp_submission` in its [runtime features](/guide/console#smtp). Nothing here changes how the HTTP APIs work.
:::

## Sender domains

A **sender domain** is a domain an app may use in its `From` address. An administrator requests it, publishes a TXT record that Thinpost generates, and asks Thinpost to verify it. Verification only proves control of the name; DKIM, SPF and DMARC are separate DNS setup. See [Sender domains](/guide/domains).

## Receiving domains

A **receiving domain** is a domain whose mail Thinpost accepts for an app. It is verified the same way, then its MX record is pointed at Thinpost. See [Inbound mail](/guide/inbound).

## Sending IPs

A **sending IP** is a named public address the edge worker can send from. Each has a HELO name, a bounce domain and a set of assigned apps. An app with no assignment uses the edge's primary address. See [Sending IPs](/guide/sending-ips).

## Messages and deliveries

A **message** is one accepted submission: the original content, sender and recipients. Thinpost assigns it a 128-bit ID, shown as 32 lowercase hex characters in the console, admin API and native webhooks. Provider APIs render the same ID in their own shape (UUID text, or `<id@domain>` for Mailgun).

A **delivery** is one recipient of a message. Every To, Cc and Bcc address becomes its own delivery with its own state, SMTP attempts, retries and webhook events. A message's overall status is derived from its deliveries. Bcc recipients are not listed in any copy's headers.

Typical delivery states run from queued, through one or more SMTP attempts, to accepted (the remote server took the message) or failed. Acceptance means the receiving server accepted it. It does not mean the mailbox received it or that a person read it.

A message can also be submitted **on hold**: it is stored and counted, but has no deliveries until the app releases it. The app can cancel it instead, and a hold that is not released within seven days expires. See [Holding messages](/guide/holding-messages).

## Enhanced Protection

**Enhanced Protection** (EP) is a per-message flag, set with the `X-Thinpost-Enhanced-Protection: true` header (or Mailgun's `o:require-tls`). It does two things:

- **Transport.** Every delivery attempt requires STARTTLS with a valid certificate, and Thinpost never falls back to cleartext. Temporary failures are retried; a final failure is reported plainly.
- **Operator access.** The console hides the subject in lists, detail and search, and records an audit event before revealing a subject, reading a body, downloading an attachment or viewing delivery headers.

Without EP, TLS is used when the remote server offers it and its certificate verifies, and the message is otherwise delivered in cleartext.

## The admission gate

The **admission gate** is a database-backed switch that lets Thinpost stop all state-producing work (submission, preparation, SMTP claims, webhook dispatch) at once, and requires a matching deployment generation to open. It exists so that a restore, or a deploy that changes the schema, cannot race with live traffic. It starts closed on a new database and is opened explicitly by a maintenance command. See [Recovery and the admission gate](/operations/recovery).

## Trusted zone and edge zone

Thinpost separates what must be trusted from what must be exposed:

- The **trusted zone** runs the control plane, PostgreSQL and object storage. It holds every secret that matters: DKIM keys, the object-encryption keyring, API key digests.
- The **edge zone** runs the edge worker. It faces the public internet on port 25, so it holds almost nothing: a journal volume, its own TLS identity and the certificates for its SMTP names.

The control plane connects to the edge, never the reverse. The edge cannot reach your database or object store, and a compromised edge cannot read stored messages. See [Architecture](/guide/architecture).

## Next

- [Architecture](/guide/architecture)
- [Quickstart](/guide/quickstart)
