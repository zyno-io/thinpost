---
title: Sending with the native API
description: Send a message with Thinpost's own JSON route, POST /app/v1/messages, using an app API key, idempotency keys and body fields for hold and tracking.
---

# Sending with the native API

`POST /app/v1/messages` is Thinpost's own send route. It takes one message as strict JSON and answers with the message ID. Use it when you are not migrating from Postmark, Resend, SendGrid or Mailgun, or when you want options such as [hold](/guide/holding-messages), tracking and Enhanced Protection as plain body fields instead of provider-specific shapes.

It is a submission like the provider routes. It goes through the same acceptance path, so the same [sender-domain verification](/guide/domains), recipient rules, [suppression list](/guide/suppressions), submission rate, daily recipient quota and pending-message limit apply. There is no separate or looser path.

## Send a message

Authenticate with a submission API key of the app, as on the provider routes.

```sh
curl https://thinpost.example.com/app/v1/messages \
  -H 'Authorization: Bearer <app key>' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: invoice-1042' \
  -d '{
    "from": "Billing <billing@example.com>",
    "to": ["customer@example.net"],
    "subject": "Your invoice",
    "text": "Your invoice is attached.",
    "html": "<p>Your invoice is attached.</p>"
  }'
```

A new message is answered with `201`:

```http
HTTP/1.1 201 Created
Content-Type: application/json
Location: /app/v1/messages/3f2a9c1e8d4b4f6a9e217c5b0a4d1e93

{"id":"3f2a9c1e8d4b4f6a9e217c5b0a4d1e93","accepted_at":"2026-11-18T09:12:44.103Z"}
```

As everywhere in Thinpost, success means the message is committed to the queue, not delivered. Follow it with [webhooks](/guide/webhooks), or read it back with `GET /app/v1/messages/{id}` (see the [App API reference](/reference/api/app)).

## Request body

The body is JSON with these fields. At least one of `text` and `html` is required.

| Field | Type | Notes |
| --- | --- | --- |
| `from` | string | An address, optionally with a display name: `Name <sender@example.com>`. Its domain must be an active, verified sender domain of the app. |
| `to` | array of strings | At least one. `to`, `cc` and `bcc` together hold at most 100 addresses. Each is a delivery. |
| `cc`, `bcc` | array of strings | Optional. A `Bcc` header is never added to the message. |
| `reply_to` | array of strings | Optional, at most 10. |
| `subject` | string | At most 998 UTF-8 bytes, with no control characters other than tab. |
| `text`, `html` | string | The bodies. |
| `headers` | object | Only `List-Unsubscribe`, `List-Unsubscribe-Post`, `In-Reply-To` and `References`, each once. Use `reply_to`, not a `Reply-To` header. |
| `attachments` | array | Up to 20 objects with `filename`, base64 `content` and optionally `content_type` and `content_id`. The decoded total is limited by the operator, 5 MiB by default. |
| `enhanced_protection` | boolean | Require TLS to the recipient's server. See [Concepts](/guide/concepts#enhanced-protection). |
| `tracking` | object | `{"opens": true, "clicks": true}`. Either key is optional. Overrides the app default for that kind. Enabling a kind needs an HTML body and a tracking runtime on the deployment. Opting out is always accepted. See [Open and click tracking](/guide/tracking). |
| `hold` | boolean | Accept on hold: stored and counted, not sent until released. See [Holding messages](/guide/holding-messages). |

Once the sending worker has reported its host name, Thinpost adds one `Received` header to every message, from any HTTP route: `Received: by <mail host> (Thinpost) with HTTP id <message id>; <acceptance time>`. It names no client address or host name, and the DKIM signature does not cover it. (Thinpost cannot see whether the TLS terminator in front of it was used, so it says `HTTP`.)

The parser is strict. A duplicate or unknown field, trailing data after the JSON value, a `null` where an object is expected, or a recipient field that is not an array is a `400`. `from` is a string, not an object. The console's test-send options (`bypass_limits` and `bypass_recipient_policy`) belong to the operator console alone: sending either field here, even as `false`, is a `400`.

The options that provider routes take as `X-Thinpost-*` request headers (`X-Thinpost-Hold`, `X-Thinpost-Enhanced-Protection`, `X-Thinpost-Track-Opens` and `X-Thinpost-Track-Clicks`) are body fields here. Sending one of those headers on this route is a `400`, so a copied header cannot be silently ignored.

The route has one spelling: a percent-encoded path is a `404`, and a query string is a `400`. Only `POST` is served.

## Idempotency

Send an `Idempotency-Key` header (1 to 256 bytes of UTF-8, no NUL) to make a retry safe. For 24 hours, a repeat of the same request with the same key, from the same app, on this route, is answered `200` with the original body and nothing new is queued:

```http
HTTP/1.1 200 OK
Location: /app/v1/messages/3f2a9c1e8d4b4f6a9e217c5b0a4d1e93

{"id":"3f2a9c1e8d4b4f6a9e217c5b0a4d1e93","accepted_at":"2026-11-18T09:12:44.103Z"}
```

Reusing a key with a different request is a `409` with `idempotency_conflict`. Without a key, a retry after a lost response can send the message twice, so set one whenever your client retries. A `408` means the body did not arrive in time and is safe to retry with the same key.

Keys are scoped to this route. The same string on a provider route is a different key.

## Errors

Errors are `{"error": "<code>"}`. Generic failures carry the HTTP status text (`Bad Request`, `Unauthorized`, `Not Found`, `Service Unavailable`). Refusals a caller can act on carry a stable code:

| Status | `error` | Meaning |
| --- | --- | --- |
| 400 | `Bad Request` | Malformed, oversized or unsupported request, a query string, or an `X-Thinpost-*` option header. |
| 401 | `Unauthorized` | Missing, malformed, wrong or revoked key, or the app is paused. A paused app's keys stop authenticating. |
| 404 | `Not Found` | A percent-encoded path. |
| 405 | `Method Not Allowed` | Only `POST` is served. |
| 408 | `Request Timeout` | The body was not received in time. |
| 409 | `idempotency_conflict` | The `Idempotency-Key` was used with a different request. |
| 422 | `message_rejected` | Acceptance refused the message: for example a `from` domain that is not an active verified sender domain of the app, a recipient the operator's recipient policy does not allow, or a `List-Unsubscribe` header on an app with Thinpost-managed unsubscribe. |
| 429 | `recipient_quota_exceeded`, `submission_rate_exceeded`, `pending_limit_exceeded` | A limit is reached. A new app has a submission rate of zero, which refuses everything with `submission_rate_exceeded` until an administrator sets one. |
| 503 | `Service Unavailable` | Submission, authentication, the database or object storage is unavailable, or the admission gate is closed. A busy request budget adds `Retry-After: 5`. |

A `422` is a refusal of this message, not of your key. Retrying it unchanged gets the same answer. A `429` or `503` can succeed later.

## Holding a message

Set `"hold": true` to accept the message on hold. The `201` then carries `X-Thinpost-Hold: held`. Check for it before you rely on the hold. Release or cancel the message as described in [Holding messages](/guide/holding-messages).

## Exposing the route

Reads and the release and cancel actions of the app API are meant for in-cluster callers. Sending is different. A deployment that exposes the provider submission routes to senders on a public ingress must also route `POST /app/v1/messages` to the control plane for senders to reach it. Expose only that path, not `/app/`.

## Not covered

The native route sends one message per request. There is no batch form yet. Managing apps, keys and domains stays in the [Admin API](/reference/api/admin) and the console.

## Next

- [App API reference](/reference/api/app): the OpenAPI contract for this route.
- [Holding messages](/guide/holding-messages)
- [Provider compatibility](/reference/compatibility/): the provider-shaped routes, which share the same limits.
