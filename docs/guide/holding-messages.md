---
title: Holding messages
description: Submit a message on hold so Thinpost stores it without sending, then release it for delivery or cancel it through the app API.
---

# Holding messages

A message submitted **on hold** is validated, stored and counted like any accepted message, but Thinpost does not prepare or send it. Your app then **releases** it, which queues it for normal delivery exactly once, or **cancels** it, which ends it without sending. A message that is neither released nor cancelled within seven days **expires** and is never sent.

Use a hold when something must happen between submission and sending: an approval step, a review queue, or a send that waits for another system to confirm.

## Submit on hold

Add `X-Thinpost-Hold: true` to any send request. It works on every provider route (`POST /email`, `/email/batch`, `/emails`, `/emails/batch`, `/v3/mail/send` and `/v3/{domain}/messages`) and applies to every message of a batch.

```sh
curl https://thinpost.example.com/emails \
  -H 'Authorization: Bearer <app key>' \
  -H 'Content-Type: application/json' \
  -H 'X-Thinpost-Hold: true' \
  -d '{"from":"billing@example.com","to":"customer@example.net","subject":"Your invoice","text":"..."}'
```

The response is the provider's normal accepted response, with the message ID in the provider's shape, plus one response header:

```http
HTTP/1.1 200 OK
Content-Type: application/json
X-Thinpost-Hold: held

{"id":"3f2a9c1e-8d4b-4f6a-9e21-7c5b0a4d1e93"}
```

**Check for `X-Thinpost-Hold: held`.** Thinpost releases before this feature ignore the request header and send the message at once, and so would any proxy that strips the header. If the response does not carry it, treat the message as sent.

The header takes `true` or `false` (case-insensitive) and may appear at most once; anything else is answered with 400. `false` is the same as leaving it out. It is part of the idempotency fingerprint, so a Resend `Idempotency-Key` reused with a different hold choice is a 409 conflict.

A held message goes through every check an accepted message does: authentication, sender-domain authorization, recipient rules, the Postmark all-suppressed rejection, the submission rate, the daily recipient quota and the pending-message limit. It is charged against the submission rate and the daily recipient quota **when it is submitted**, not when it is released, and it counts toward the app's pending-message limit until it is cancelled, expires or finishes delivery.

## Release

Release a held message with the app API, using any active key of the app that sent it:

```sh
curl -X POST https://thinpost.example.com/app/v1/messages/3f2a9c1e8d4b4f6a9e217c5b0a4d1e93/release \
  -H 'Authorization: Bearer <app key>'
```

The message ID may be the 32-hex form or the provider's rendering of it, as for `GET /app/v1/messages/{id}`. The request has no body or query string. The answer is `200` with the same body as `GET /app/v1/messages/{id}`:

```json
{
  "message": {
    "id": "3f2a9c1e8d4b4f6a9e217c5b0a4d1e93",
    "state": "queued",
    "status": "queued",
    "accepted_at": "2026-11-18T09:12:44.103Z",
    "delivery_count": 0
  },
  "deliveries": [],
  "hold": {
    "expires_at": "2026-11-25T09:12:44.103Z",
    "resolved_at": "2026-11-18T10:02:05.871Z",
    "resolution": "released"
  }
}
```

(Some `message` fields are left out here.) Release queues the message for preparation and delivery like a freshly accepted one. Everything that normally happens before an SMTP attempt happens after release too: the app must still be active and the sender domain verified, suppressed recipients are dropped, and the recipient rules are checked again before each attempt. The `Date` header of the sent mail is the release time.

Release is **idempotent**. Releasing a message that is already released answers `200` with its current state, and never queues a second copy. Concurrent releases, and a release racing a cancel or the expiry, are serialized on the message: exactly one of them takes effect.

## Cancel

Cancel a held message to end it without sending:

```sh
curl -X POST https://thinpost.example.com/app/v1/messages/3f2a9c1e8d4b4f6a9e217c5b0a4d1e93/cancel \
  -H 'Authorization: Bearer <app key>'
```

The answer is `200` with the message, now in state `cancelled`, and `hold.resolution` `cancelled`. Cancelling a message that is already cancelled is also `200`. A cancelled message no longer counts toward the pending-message limit. Its content stays stored, like any other message. Cancel applies only to held messages; there is no way to stop a message that was not submitted on hold.

## Expiry

A held message expires **seven days** after it was accepted (`hold.expires_at`). Thinpost checks for expired holds every minute and moves each one to the terminal state `hold_expired`. It is never sent, and it stops counting toward the pending-message limit. A release or cancel that arrives after `expires_at` is refused with `409 hold_expired`, even if the minute's check has not run yet.

Expiry runs while the app is paused, too: pausing an app does not extend its holds.

## Errors

Release and cancel answer `404` for an unknown message, a malformed ID and a message of another app, exactly like the read routes, and `401` for a missing, wrong or revoked key (including a key of a paused app). A message that cannot be released or cancelled is `409` with a reason:

| `error` | Release | Cancel |
| --- | --- | --- |
| `not_held` | The message was not submitted on hold | Same |
| `released` | (not used: a repeat release is `200`) | The message was already released |
| `cancelled` | The message was cancelled | (not used: a repeat cancel is `200`) |
| `hold_expired` | The hold expired | Same |
| `recovery_hold` | The message was quarantined by a [database recovery](/operations/recovery) | Same |

```json
{"error":"hold_expired"}
```

`503` means the database is unavailable or the admission gate is closed, for example during an upgrade; retry later. Because release and cancel are idempotent, retrying after an ambiguous failure is safe.

## States, events and audit

| State | Status | Meaning |
| --- | --- | --- |
| `on_hold` | `on_hold` | Stored and waiting for release |
| `queued` and later | as for any message | Released and being prepared or sent |
| `cancelled` | `cancelled` | Cancelled by the app; terminal |
| `hold_expired` | `expired` | Not released in time; terminal |

Each transition creates a message-level [webhook event](/guide/webhooks#events) with no delivery and SMTP code 0: `held` when the message is accepted, then `released`, `cancelled` or `hold_expired`. Native endpoints receive all four. A Resend-format endpoint receives `hold_expired` as `email.failed`; Postmark and Mailgun formats receive none of them.

A release or cancel that changes the message records an audit event (`message.hold.released` or `message.hold.cancelled`) with the actor `key:<key ID>`. A repeat that changes nothing is not audited. The control plane also logs one line per transition, with IDs only:

```text
message app=<id> message=<id> status=held
message app=<id> message=<id> status=released
message app=<id> message=<id> status=cancelled
message app=<id> message=<id> status=hold_expired
```

The console shows the hold in message lists and details. Operators cannot release or cancel a held message: the hold belongs to the app.

## Next

- [App API reference](/reference/api/app)
- [Webhooks](/guide/webhooks)
- [Sending with SDKs](/guide/sending-with-sdks)
