---
title: Data handling
description: What Thinpost logs, audits, stores and encrypts, how Enhanced Protection changes operator access, and what retention you must provide.
---

# Data handling

Thinpost keeps message content out of logs, URLs and telemetry, encrypts stored content, and audits the reads that expose protected content. This page lists where message data lives so that you can apply your own retention and access controls.

## What is logged

Subjects, recipient and sender addresses and search terms never go to stdout, stderr, outbound-call logging or an error reporter. The control plane logs one line per message lifecycle event, containing only 32-hex IDs (app, message, delivery) and enumerated statuses. The line formats are in [Monitoring](/operations/monitoring#logs).

Rules the code follows:

- **SMTP errors** carry the stage and the reply code only. The remote reply text is discarded because it usually echoes the recipient.
- **Sending IPs** appear by name, or as `primary` or `other`, never as an address.
- **Panics** log a route class and a stack, never the URL or the panic value. The bounce receiver replaces the SMTP library's default logger, which prints peer addresses.
- **Search** uses `POST /api/v1/apps/{appId}/messages/search` with the term in a JSON body, so it never appears in an access log, a URL or a cursor. A query string on that route is rejected.
- **Tracing is refused.** Both binaries refuse to start when `GRPC_BINARY_LOG_FILTER` is set. Do not enable client tracing for the S3 client, the database driver or HTTP clients.
- **The console** keeps search terms, subjects, addresses and message content only in memory. It never writes them to a URL, browser storage, history state, logs or toasts.
- **Deploy hooks** log fixed reason codes, migration numbers and SQLSTATEs.

A tracking or unsubscribe URL is a working capability for one recipient. A logged URL reveals nothing beyond that one action, but keep access logs for the tracking origin restricted.

### Your part: PostgreSQL logging

PostgreSQL can log bind values unless you configure it not to. Set `log_parameter_max_length=0` and `log_parameter_max_length_on_error=0`, and do not use `log_statement=all` or parameter-logging audit extensions for this database. Set `auto_explain.log_parameter_max_length=0` if you load `auto_explain`, and consider `log_error_verbosity=terse`, because constraint-violation `DETAIL` lines can quote row values. Thinpost rejects `default_query_exec_mode=simple_protocol`, which would inline values into SQL text.

## What is stored, and where

| Data | Where | Protection |
| --- | --- | --- |
| Original submission (body, attachments, headers). For SMTP mail, the raw message as the client sent it | Object store | Encrypted by the control plane before upload |
| Signed MIME per recipient | Object store | Encrypted. Attachments are stored again in each recipient's object |
| Received bounces and inbound messages | Object store | Encrypted. Inbound objects live under the `inbound/` prefix |
| Recipient addresses, sender domain, From address and name, subject | PostgreSQL, plaintext | Database access control. Enhanced Protection subjects are withheld from console reads |
| API keys | PostgreSQL | A digest only. The secret is shown once at creation |
| SMTP credentials | PostgreSQL | A SHA-256 digest only. The password is shown once at creation. The credential ID (its username), name and last-used time are plaintext |
| The From address a client sent over SMTP, when the app's policy replaced it (`original_from`) | PostgreSQL, plaintext | Database access control, like the other sender metadata. Shown in the console even for Enhanced Protection messages |
| Replay digests of SMTP mail | PostgreSQL | A keyed HMAC-SHA-256 of the app, sender, recipients, `Message-ID` and message bytes, so the database alone cannot confirm a guess. Kept 24 hours for a message with a `Message-ID` and 10 minutes for one with only a `Date`, then removed. A message with neither has none |
| The key of those digests | PostgreSQL | Generated once per deployment and sealed with the object keyring, never returned. See [Rotation](/operations/rotation#object-keyring) |
| Webhook signing secrets | PostgreSQL | Sealed with the object keyring, never returned |
| Delivery and webhook history, suppressions | PostgreSQL | Contains IDs, states and codes. Suppression audit rows record the reason and origin but never the address |
| Edge journal | A volume on the Edge | Attempt metadata only, with no content |

Because recipients and subjects are plaintext in PostgreSQL, **database backups and the WAL archive also hold them**. Apply your retention, encryption and deletion policy to backups as well.

## SMTP submission

Mail sent over [SMTP](/guide/smtp) is stored like any other submission, with these additions:

- **Credentials.** Only a SHA-256 digest of each password is stored. The username (an ID), the name you gave it and its last-used time are plaintext. The password crosses the edge inside TLS and is never logged, and no log line, error or reply contains it.
- **The original From.** When the app's From policy replaces a sender's address, the address the client sent is kept as the message's `original_from`, in plaintext, and shown in the console.
- **Raw source.** The message is stored as received, with the decisions Thinpost made (recipients and their kinds, the effective From). The client's `Message-ID` and any headers Thinpost later removes remain in the stored original. The client's IP address is not stored.
- **Retry protection.** To recognize a retry, Thinpost stores a keyed digest for 24 hours (a message with a `Message-ID`) or 10 minutes (only a `Date`), and nothing for a message with neither. Expired digests are removed by the periodic idempotency cleanup.

## Encryption at rest

The control plane encrypts every object before upload. Each object uses a versioned envelope with its own random salt. HKDF-SHA-256 derives a per-write AES-256-GCM key from a key in the object keyring, and GCM supplies its own random nonce. The key version and the exact object key are authenticated, so a modified object, or one copied to a different key, fails to decrypt. There is no plaintext fallback, and a missing key version fails the read.

The keyring also seals small database secrets. Never drop a key version while an object, a sealed value or a backup still uses it. Back the keyring up with the bucket. See [Rotation](/operations/rotation#object-keyring).

Encryption protects stored content. It does not replace access controls, TLS or backup procedures, and a process that holds the keyring can read everything. Keep the keyring away from any identity that only needs to list or delete objects.

Enable your database's and object store's own at-rest encryption too. Thinpost does not manage it.

## Enhanced Protection

Enhanced Protection (EP) is a per-message flag with two effects.

**Transport.** Every delivery attempt requires STARTTLS with a valid certificate and never falls back to cleartext.

**Operator access.** For EP messages only:

- The subject is withheld in SQL for lists, detail and search, and reported as masked. A reveal needs a confirmation.
- Revealing the subject, reading the body, downloading an attachment and viewing delivery headers each write an audit record. The record commits **before** any data is returned. It holds the actor, app, action and message or delivery IDs, never the subject.

For messages that are not EP, content reads are not audited. Metadata reads, such as list, detail and search, are not audited for any message. Recipients, the From address and the sender domain are not masked even for EP messages. If you need an access trail for every read, front the API with your own audit logging.

EP is a request-time choice by the sender. It limits who sees content in the console and how it travels. It does not change the Edge's ability to see the message in flight.

## Audit events

Operator actions that change state, such as creating an app, issuing or revoking a key or an SMTP credential, changing an app's SMTP From policy, pausing an app, changing suppressions, tracking defaults, the retry policy and feedback-loop settings, write audit events in the same database transaction. Records name IDs and old and new values, never addresses or subjects. An app's release or cancel of a [held message](/guide/holding-messages) is audited the same way, with the actor `key:<key ID>`. An administrator's console test send is audited as `message.test.submitted` in the transaction that commits the message: the actor is the administrator's subject, the resource is the message ID, and the only details are `recipient_count` and the two operator option flags `bypass_limits` and `bypass_recipient_policy`, with no subject, domain or addresses. Audit rows live in PostgreSQL. Protect the table, retain it according to your policy, and use database privileges to prevent tampering. Thinpost does not provide tamper-evident storage.

## Retention

Thinpost does not apply a retention period to outbound message content or metadata. They stay in PostgreSQL and the object store until you remove them. Decide your retention and plan how to enforce it with database jobs and bucket lifecycle rules. A sweep that deletes objects exists but is not scheduled. See [Maintenance](/operations/maintenance#object-collection). Deletion in a versioned bucket can leave older versions, so configure noncurrent-version expiry as well.

What Thinpost does limit:

- **Inbound content** is kept for 30 days. After that, the console and webhook renderer refuse to read it, by the database clock, whether or not your storage rule has run. A lifecycle rule on the `inbound/` prefix removes the objects, and Thinpost's application credential does not need delete permission. The metadata row stays.
- **Idempotency markers** expire after 24 hours and are cleaned up periodically.
- **Tracking.** Retiring a mapping ends analytics for a link but not its redirect. See [Maintenance](/operations/maintenance#tracking-retirement).
- **Webhook deletion** is a tombstone. The sealed secret and authentication are removed and history stays.

## Webhooks and tracking

Webhook payloads carry IDs, event types and the data a format defines. Thinpost sends them to endpoints you choose, signed where the format supports it, so treat receivers as part of your data boundary. See [Webhook formats](/reference/webhook-formats).

Open and click tracking is off unless configured and enabled per app or per message. Tracking events record that a link was fetched, along with technical metadata. Mail clients, proxies and scanners also fetch links, so events are signals. Tracking adds data about recipients, so check it against your privacy obligations.

## Your responsibilities

- Restrict database, object-store, backup and keyring access, and keep the keyring in a separate failure domain from the bucket.
- Configure PostgreSQL logging as above.
- Set retention for outbound content, metadata, audit rows and backups.
- Keep log pipelines free of content, and restrict tracking-origin access logs.
- Review who holds the administrator role.
