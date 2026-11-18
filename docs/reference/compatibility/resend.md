---
title: Resend compatibility
description: The Resend send and batch routes Thinpost implements, supported fields, idempotency, responses, error mapping and every deviation from Resend.
---

# Resend compatibility

Tested with `resend` 6.37.0 (`emails.send`, `batch.send`). See the [overview](/reference/compatibility/) for shared limits and behavior, and [Sending with SDKs](/guide/sending-with-sdks) for setup.

## Authentication

Send `Authorization: Bearer <app key>` exactly once. The scheme is case-insensitive. A missing, malformed, wrong or revoked key is 401.

## Routes

| Route | Purpose |
| --- | --- |
| `POST /emails` | Send one message |
| `POST /emails/batch` | Send up to 100 messages atomically |
| `GET /suppressions` | List the app's suppressions |

Everything else is 404. There are no retrieve, update, cancel, domain, audience, contact or broadcast routes.

## Request fields

| Field | Notes |
| --- | --- |
| `from` | Required. Its domain must be an active sender domain of the app. |
| `to` | Required. A string or an array of strings. |
| `cc`, `bcc` | A string or an array. An empty array is treated as absent. |
| `subject` | Required, not blank. |
| `text`, `html` | At least one is required. |
| `reply_to` | A string or an array, at most 10 addresses. |
| `attachments` | Array, see below. |
| `headers` | Object of string values, at most five entries, restricted to the [accepted headers](/reference/compatibility/#custom-headers). |

Any other field is rejected with 400. That includes `tags`, `scheduled_at`, `template` and `react`.

```bash
curl https://mail.example.com/emails \
  -H "Authorization: Bearer tp_example_key" \
  -H "Idempotency-Key: order-1001-welcome" \
  -H "Content-Type: application/json" \
  -d '{
    "from": "Example <hello@example.com>",
    "to": ["user@example.net"],
    "subject": "Welcome",
    "html": "<p>Hello.</p>"
  }'
```

### Attachments

Each entry has `filename`, `content` (canonical base64 string), and optionally `content_type` and `content_id`. Only these keys are accepted, so a `path` (remote URL) is rejected. Setting `content_id` makes the attachment inline, which requires an `html` body and a unique content ID of letters, digits and `._@+-`, at most 128 bytes. When `content_type` is absent it is guessed from the file extension.

Content must be canonical base64; anything else is 400. The contract suite sends a `Buffer`, which the SDK encodes.

### Tracking

Resend has no per-message tracking fields, so Thinpost adds two request headers. Each is `true` or `false`; an absent header leaves the kind to the app default.

| Header | Effect |
| --- | --- |
| `X-Thinpost-Track-Opens` | Open tracking |
| `X-Thinpost-Track-Clicks` | HTML link click tracking |

They apply to the one message, or to every item of a batch. Enabling either requires an `html` body; a batch where any item lacks HTML is rejected as a whole. In the SDK, pass them in the request options `headers`. The same headers are rejected with 400 on the other providers' routes.

## Idempotency

`Idempotency-Key` is optional, at most 256 bytes, valid UTF-8, and may appear once. It is scoped to the app and to the Resend adapter and lasts 24 hours.

- A retry with an identical request returns the original response, including after the original was accepted.
- A retry whose request differs from the original is 409.
- On `/emails/batch` the key identifies the whole ordered batch. A key used by a single send cannot be reused for a batch or the reverse (409).

Without a key, an ambiguous response can lead to a duplicate on retry.

## Responses

A single send returns HTTP 200:

```json
{ "id": "0b8e5a3c-3d1f-4a52-9c6e-1f2a3b4c5d6e" }
```

A batch returns HTTP 200 with the IDs in request order:

```json
{ "data": [{ "id": "0b8e5a3c-3d1f-4a52-9c6e-1f2a3b4c5d6e" }, { "id": "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f" }] }
```

The IDs are UUID text of the same 128 bits as the 32-hex Thinpost ID.

## Batch

`POST /emails/batch` takes a JSON array of one to 100 message objects, at most 2 MiB in all. Each item has the fields above and at most 50 recipients (To, Cc and Bcc together). The whole request is validated before anything is stored, and all messages, quota use and the ordered result commit in one database transaction: an invalid or unauthorized item, an exhausted quota or a failed commit accepts none of them. The `X-Thinpost-Enhanced-Protection` and tracking headers apply to every item.

Tracking and Enhanced Protection headers are request-wide; an item cannot set its own.

## Errors

Errors use the Resend envelope:

```json
{ "statusCode": 400, "message": "invalid or unsupported message", "name": "Bad Request" }
```

`name` is the HTTP status text. Thinpost does not reproduce Resend's named error types or validation messages.

| HTTP | Meaning |
| --- | --- |
| 400 | Invalid or unsupported message, invalid idempotency key, invalid Thinpost header, or unauthorized sender domain |
| 401 | Missing or invalid key |
| 405 | Method not allowed |
| 408 | Request body not received |
| 409 | Idempotency conflict |
| 429 | Recipient quota, pending-message limit or upload rate exceeded (batches report `batch quota exceeded`) |
| 503 | Authentication or submission unavailable |

A message whose recipients are all suppressed is accepted and the recipients drop at delivery time; there is no 4xx for it.

## Suppression list

`GET /suppressions` authenticates with the same bearer key and acts on the key's app. Parameters (names case-insensitive, each once, unknown ones 422):

| Parameter | Values |
| --- | --- |
| `limit` | 1 to 100, default 20 |
| `after`, `before` | A suppression ID; not combinable |
| `origin` | `bounce`, `complaint`, `manual` |

The response is newest first:

```json
{
  "object": "list",
  "has_more": false,
  "data": [
    {
      "id": "0b8e5a3c-3d1f-4a52-9c6e-1f2a3b4c5d6e",
      "email": "user@example.net",
      "origin": "bounce",
      "source_id": "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f",
      "created_at": "2026-01-01T12:00:00Z"
    }
  ]
}
```

`origin` maps `hard_bounce` to `bounce`, `spam_complaint` to `complaint`, and `manual` and `unsubscribe` to `manual`. `source_id` is the originating message ID or `null`. A cursor that names a deleted entry is 422; restart the listing. Routes that address one entry by address (get, remove, add) are not implemented, because they would put an address in a URL. See [Suppressions](/guide/suppressions).

## Webhooks

Choose the Resend format and handlers using the Resend SDK's `webhooks.verify` keep working: payloads are Svix-signed and the `email_id` matches the `id` returned at send time. A per-delivery event lists only that delivery's address in `data.to` (the Bcc address for a Bcc delivery); message-level events list the To recipients. Resend has no per-email unsubscribe event, so none is sent. See [Webhooks](/guide/webhooks) and [Webhook formats](/reference/webhook-formats).

## Deviations

- No tags, scheduled sends, templates, React rendering, or attachment URLs.
- No retrieve, cancel, update or domain management routes.
- Tracking is set by Thinpost headers, not by Resend fields.
- Error `name` is the HTTP status text, not a Resend error type.
- One `Idempotency-Key` covers a whole batch.
- A `Reply-To` entry in `headers` is accepted, as long as `reply_to` is not also set.
