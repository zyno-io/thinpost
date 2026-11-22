---
title: Provider compatibility
description: Which parts of the Postmark, Resend, SendGrid and Mailgun send APIs Thinpost implements, the SDK versions it is tested with, and how the four adapters compare.
---

# Provider compatibility

Thinpost accepts mail on routes shaped like the send APIs of four hosted providers. Point an official SDK at your Thinpost origin, use an app key as the credential, and send. This section lists exactly what each adapter accepts and where it differs from the provider.

::: warning Subset, not parity
Each adapter implements a documented subset of the provider's request fields. A field outside the subset is rejected with HTTP 400; it is never silently ignored. Thinpost does not reproduce every provider response, error code or limit. Test your own payloads before you switch.
:::

Per-provider pages:

- [Postmark](/reference/compatibility/postmark)
- [Resend](/reference/compatibility/resend)
- [SendGrid](/reference/compatibility/sendgrid)
- [Mailgun](/reference/compatibility/mailgun)

For setup and SDK snippets see [Sending with SDKs](/guide/sending-with-sdks).

## Tested SDKs

An automated contract suite runs the official Node SDKs against the real HTTP adapters.

| Provider | Package and version | Operations exercised | Endpoint override |
| --- | --- | --- | --- |
| Postmark | `postmark` 5.1.1 | `sendEmail`, `sendEmailBatch`, `getSuppressions` | `requestHost` and `useHttps` client options |
| Resend | `resend` 6.38.1 | `emails.send`, `batch.send` | `baseUrl` client option |
| SendGrid | `@sendgrid/mail` 8.1.6 | `send` (one or several personalizations), `sendMultiple` | `setDefaultRequest('baseUrl', ...)` |
| Mailgun | `mailgun.js` 14.1.1 | `messages.create` | `url` client option |

Other languages and versions that speak the same HTTP shapes should work for the supported fields, but are not part of the suite. The suite also verifies that Postmark, Resend and Mailgun [webhook](/reference/webhook-formats) payloads carry the same message ID the send call returned.

## Matrix

| Capability | Postmark | Resend | SendGrid | Mailgun |
| --- | --- | --- | --- | --- |
| Single send | `POST /email` | `POST /emails` | `POST /v3/mail/send` | `POST /v3/{domain}/messages` |
| Batch send | `POST /email/batch`, up to 100 messages, per-item results, not atomic | `POST /emails/batch`, up to 100 messages, atomic | Up to 100 personalizations in one request, atomic | Not available |
| Idempotency key | No | `Idempotency-Key`, 24 hours | No | No |
| Request encoding | JSON | JSON | JSON | `multipart/form-data` or URL-encoded |
| Auth | `X-Postmark-Server-Token` header | `Authorization: Bearer` | `Authorization: Bearer` | HTTP Basic, user `api` |
| Cc and Bcc | `Cc`, `Bcc` | `cc`, `bcc` | Per personalization `cc`, `bcc` | `cc`, `bcc` |
| Reply-To | `ReplyTo` | `reply_to` | `reply_to`, `reply_to_list` | `h:Reply-To` |
| Attachments | Base64 `Attachments` | Base64 `attachments` | Base64 `attachments` | Multipart `attachment` parts |
| Inline images | `ContentID` with `cid:` prefix | `content_id` | `content_id` | Multipart `inline` parts |
| Remote attachment URLs | Rejected | Rejected | n/a | n/a |
| Templates | No | No | No (no substitutions) | No |
| Tags and metadata | No | No | No (no categories, custom args) | No (no `o:tag`, `v:` variables) |
| Scheduled send | No | No | No | No |
| Custom headers | Five named headers only | Five named headers only | Five named headers only | Five named headers only, as `h:` fields |
| Open tracking | `TrackOpens` | `X-Thinpost-Track-Opens` header | `tracking_settings.open_tracking` | `o:tracking-opens` |
| Click tracking | `TrackLinks`: `HtmlOnly` or `None` | `X-Thinpost-Track-Clicks` header | `tracking_settings.click_tracking` with `enable_text: false` | `o:tracking-clicks`: `htmlonly` or `no` |
| Suppression API | `/message-streams/outbound/suppressions` routes | `GET /suppressions` | No | No |
| Webhook format | `Delivery`, `Bounce`, `Open`, `SubscriptionChange`, `SpamComplaint`; no signature | `email.*` events, Svix-signed | Not available | `delivered`, `temporary_fail`, `permanent_fail`, `opened`, `unsubscribed`, `complained`; Mailgun-style signature |
| Inbound mail | Thinpost-native signed webhook only | Same | Same | Same |

Inbound mail is not provider-shaped: it is delivered in the Thinpost `thinpost_inbound` webhook format ([Inbound mail](/guide/inbound)).

## Shared behavior

These rules apply to all four adapters.

### Authentication and routing

A missing, malformed, wrong or revoked key is 401. A key authorizes only its own app. Only `POST` is accepted on the send routes; another method is 405 with an `Allow` header. An unknown path is 404. Keys are limited to 512 bytes.

The `From` domain must be an active [sender domain](/guide/domains) of the app. Mailgun additionally requires the domain in the URL path to be an active sender domain of the app.

### Limits

| Limit | Value |
| --- | --- |
| Request body, batch routes and multi-personalization SendGrid requests | 2 MiB |
| Request body, single-message requests | Base64 size of the attachment limit plus 4 MiB (11,184,812 bytes at the default) |
| Attachments per message | 20 |
| Total decoded attachment size | `THINPOST_MAX_ATTACHMENT_BYTES`: 5 MiB by default, at least 64 KiB, at most 5.5 MiB |
| Recipients per message (To, Cc and Bcc together) | 100, at least one To |
| Reply addresses | 10, on one header line of at most 998 bytes |
| JSON nesting | 100 levels |

A subject is required and must not be blank. A message needs a text body, an HTML body or both.

Single-message requests larger than 2 MiB share an in-flight memory budget. A request that cannot get budget within ten seconds is answered 503 with `Retry-After: 5`. A body that is not received in time is answered 408.

### Strict decoding

JSON bodies are rejected when they contain unknown fields, trailing data, invalid UTF-8, or repeated object field names (including escaped or case-folded equivalents, at any depth). In a Postmark batch only the affected item fails; in a Resend batch the whole request is rejected.

### Recipients

Every To, Cc and Bcc address is its own delivery, in that order. Duplicates across lists are kept as separate deliveries. Each copy carries a `To` header listing every To address, a `Cc` header when there are Cc addresses, and never a `Bcc` header. The daily recipient quota, the recipient allowlist and the [suppression list](/guide/suppressions) apply to Cc and Bcc recipients as they do to To.

A new app starts with 60 messages per minute (burst of 100), 10,000 recipients per day and 1,000 pending messages; an administrator can change them, and a limit of zero blocks all new messages. A request over a limit is answered 429 (see each provider page for the exact shape).

### Custom headers

Only these headers are accepted from the app, case-insensitively, each at most once:

- `Reply-To`
- `In-Reply-To` and `References`
- `List-Unsubscribe` and `List-Unsubscribe-Post`

Any other custom header is rejected, including `Cc` and `Bcc`, which would change the recipient set. `Message-ID` is always generated by Thinpost.

- `Reply-To` sets the same reply address list as the provider's own reply field. Sending both is 400.
- `List-Unsubscribe` is one or two `<uri>` items, each an `https:` URI without user information (at most 2,048 bytes) or a `mailto:` URI, on one printable-ASCII line of at most 998 bytes. `List-Unsubscribe-Post` must equal `List-Unsubscribe=One-Click` and requires `List-Unsubscribe`. If the app enables Thinpost-managed one-click unsubscribe, Thinpost's own headers replace the request's, and a recipient's unsubscribe is also forwarded to the request's own https link when it had one with `List-Unsubscribe-Post` (see [One-click unsubscribe](/guide/tracking#one-click-unsubscribe)).
- `In-Reply-To` is exactly one RFC 5322 message ID in `<left@right>` form. `References` is 1 to 100 message IDs separated by spaces or tabs.

The headers are stored encrypted with the message and covered by the DKIM signature.

### Message IDs

The send response returns the message ID in the provider's shape. All forms carry the same 128 bits.

| Provider | Field | Shape |
| --- | --- | --- |
| Postmark | `MessageID` | UUID text |
| Resend | `id` | UUID text |
| Mailgun | `id` | `<id@from-domain>`, with the 32-hex ID |
| SendGrid | `X-Message-Id` response header | 32 lowercase hex characters, only when the request produced exactly one message |

Thinpost's own webhook format and the admin API use the 32-hex form. The RFC 5322 `Message-ID` header of a message is `<{id}@{from-domain}>` with the 32-hex ID and the lowercased From domain, identical for every recipient's copy.

### Thinpost extensions

These are optional and are not provider fields.

- **Enhanced Protection**: `X-Thinpost-Enhanced-Protection: true` (or `false`) on any send request, including batches. It may appear at most once. It requires validated TLS on every delivery attempt. The removed header name `X-Thinpost-Require-TLS` is rejected with 400. Mailgun's `o:require-tls` sets the same policy; if either asks for it, the message requires it. See [Concepts](/guide/concepts).
- **Resend tracking headers**: `X-Thinpost-Track-Opens` and `X-Thinpost-Track-Clicks`. They are rejected with 400 on the other three adapters.
- **Hold**: `X-Thinpost-Hold: true` (or `false`) on any send request, including batches, at most once. The message is stored and counted but not sent until the app releases it, and a successful response carries `X-Thinpost-Hold: held`. No provider has an equivalent field (scheduled sends need a time), so there is no provider-native form. See [Holding messages](/guide/holding-messages).

### Tracking

Per-message tracking choices always win over the app default; an unset kind takes the default. Enabled tracking requires an HTML body, and is rejected with 400 when the server has no tracking runtime configured. Opting out (for example `TrackOpens: false`) is always accepted. Text-body tracking is never supported. See [Open and click tracking](/guide/tracking).

### Success means committed

A success response means the message is committed to the queue, not delivered. Use [webhooks](/guide/webhooks) or the message history for outcomes. If the outcome of a request is unknown (for example a timeout), a retry can duplicate it unless the adapter supports an idempotency key.

## Read-back routes

An app key can read its own messages through `GET /app/v1/messages/{id}` and its `/body` and `/attachments/{index}` routes, and release or cancel a held message through `POST /app/v1/messages/{id}/release` and `/cancel`. These are Thinpost routes, not part of any provider API. The same API has a native send route, `POST /app/v1/messages`, which is not a provider adapter: it takes Thinpost's own JSON and shares the limits, quotas, sender-domain checks and recipient rules of the provider routes. See [Sending with the native API](/guide/native-api) and the [App API](/reference/api/app).
