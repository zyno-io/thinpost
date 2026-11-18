---
title: Postmark compatibility
description: The Postmark send, batch and suppression routes Thinpost implements, the supported fields, responses, error mapping and every deviation from Postmark.
---

# Postmark compatibility

Tested with `postmark` 5.1.0 (`sendEmail`, `sendEmailBatch`, `getSuppressions`). See the [overview](/reference/compatibility/) for shared limits and behavior, and [Sending with SDKs](/guide/sending-with-sdks) for setup.

## Authentication

Send the app key in the `X-Postmark-Server-Token` header, exactly once. Other credential headers are not read. A missing, repeated, wrong or revoked key is 401.

## Routes

| Route | Purpose |
| --- | --- |
| `POST /email` | Send one message |
| `POST /email/batch` | Send up to 100 messages |
| `GET /message-streams/outbound/suppressions/dump` | List suppressions |
| `POST /message-streams/outbound/suppressions` | Add suppressions |
| `POST /message-streams/outbound/suppressions/delete` | Remove suppressions |

Everything else is 404. In particular there is no `/email/withTemplate`, `/email/batchWithTemplates`, bounce, stats or server API.

## Request fields

| Field | Notes |
| --- | --- |
| `From` | Required. A bare address or `Name <address>`. Its domain must be an active sender domain of the app. |
| `To` | Required. One string with comma-separated addresses. |
| `Cc`, `Bcc` | Optional comma-separated strings. |
| `Subject` | Required, not blank. |
| `TextBody`, `HtmlBody` | At least one is required. |
| `ReplyTo` | Comma-separated string, at most 10 addresses. |
| `TrackOpens` | Boolean. |
| `TrackLinks` | `None` or `HtmlOnly`. |
| `Attachments` | Array, see below. |
| `Headers` | Array of `{Name, Value}`, at most five entries, restricted to the [accepted headers](/reference/compatibility/#custom-headers). |

Any other field is rejected with 400. That includes `Tag`, `Metadata`, `MessageStream`, `TemplateId`, `TemplateModel`, and `InlineCss`. Because `MessageStream` is not accepted, a message cannot name a stream.

```bash
curl https://mail.example.com/email \
  -H "X-Postmark-Server-Token: tp_example_key" \
  -H "Content-Type: application/json" \
  -d '{
    "From": "Example <hello@example.com>",
    "To": "user@example.net",
    "Subject": "Welcome",
    "TextBody": "Hello.",
    "HtmlBody": "<p>Hello.</p>",
    "TrackOpens": true,
    "TrackLinks": "HtmlOnly"
  }'
```

### Attachments

Each entry has `Name`, `Content` (canonical base64), `ContentType`, and optionally `ContentID`, `ContentLength` and `Disposition`.

- `ContentType` is optional; when absent it is guessed from the file extension, falling back to `application/octet-stream`.
- `ContentID` makes the attachment inline and must start with `cid:`. Inline attachments require an HTML body and a unique content ID of letters, digits and `._@+-`, at most 128 bytes.
- `ContentLength`, when present, must equal the decoded size.
- `Disposition` is `inline` or `attachment` and must agree with whether `ContentID` is present.
- File names are at most 200 bytes, with no path separators or control characters.
- Remote URLs are not fetched; content must be base64.

### Tracking

`TrackOpens` and `TrackLinks` follow Postmark's per-message semantics, with these differences:

- `TextOnly` and `HtmlAndText` are rejected: text bodies are never rewritten.
- Enabling either kind without an `HtmlBody` is rejected.
- `false` and `None` are opt-outs that override an app default.
- Enabling tracking on a server without a tracking runtime is rejected.

See [Open and click tracking](/guide/tracking).

## Responses

A successful single send returns HTTP 200:

```json
{
  "To": "user@example.net",
  "SubmittedAt": "2026-01-01T12:00:00.0000000Z",
  "MessageID": "0b8e5a3c-3d1f-4a52-9c6e-1f2a3b4c5d6e",
  "ErrorCode": 0,
  "Message": "OK"
}
```

`To` echoes the request's `To` string. Postmark's `MessageID` is a UUID; here it is the same 128 bits as the 32-hex Thinpost ID with hyphens added. Cc and Bcc are not echoed.

### Batch

`POST /email/batch` takes a JSON array of one to 100 message objects, at most 2 MiB in all, and returns HTTP 200 with an ordered array of per-item results. An item result has the single-send shape on success or `{"ErrorCode": n, "Message": "..."}` on failure. Each item is committed on its own, so an interrupted request can leave some items accepted. There is no idempotency key; a retry can send duplicates.

An empty array, more than 100 items, a non-array body or an oversize body is a whole-request 400.

## Errors

Errors use the Postmark envelope `{"ErrorCode": n, "Message": "..."}`. For request-level errors `ErrorCode` repeats the HTTP status, unless noted. Thinpost does not use Postmark's own numeric error codes, with the exceptions below.

| HTTP | ErrorCode | Meaning |
| --- | --- | --- |
| 400 | 400 | Invalid or unsupported message, invalid Thinpost header, or unauthorized sender domain |
| 401 | 401 | Missing or invalid server token |
| 405 | 405 | Method not allowed |
| 408 | 408 | Request body not received |
| 422 | 406 | Every distinct recipient is suppressed |
| 429 | 429 | Recipient quota, pending-message limit or upload rate exceeded |
| 503 | 503 | Authentication or submission unavailable |

Per-item `ErrorCode` values in a batch:

| ErrorCode | Meaning |
| --- | --- |
| 300 | Invalid or unsupported item, or unauthorized sender domain |
| 406 | Every distinct recipient is suppressed |
| 10 | Key revoked while the batch was running |
| 100 | Any other failure, including a quota, rate or pending limit, or storage unavailable. The message text distinguishes the pending-limit and rate cases. |

The 406 response applies only when every distinct recipient is suppressed. A partly suppressed message is accepted, and the suppressed recipients are dropped at delivery time.

## Suppression routes

These routes authenticate with the same token header, act on the key's app, and answer 503 when suppression support is unavailable. They address the app's single suppression list through the `outbound` stream; any other stream is 422 with `ErrorCode` 1221. Addresses never appear in URLs.

### Dump

`GET /message-streams/outbound/suppressions/dump` accepts these query parameters (names are case-insensitive; each may appear once; unknown ones are rejected with 422):

| Parameter | Values |
| --- | --- |
| `SuppressionReason` | `HardBounce`, `SpamComplaint`, `ManualSuppression` |
| `Origin` | `Recipient`, `Customer`; `Admin` is accepted and matches nothing |
| `fromdate`, `todate` | `YYYY-MM-DD`, both inclusive |
| `count`, `offset` | Accepted and ignored: the dump is not paged |

The response is `{"Suppressions": [...]}`, each entry with `EmailAddress`, `SuppressionReason`, `Origin` and an RFC 3339 `CreatedAt`. A match of more than 10,000 entries is 422 (`ErrorCode` 1407); narrow the date range. Filtering by `EmailAddress` is not supported (422, `ErrorCode` 1409).

Reason mapping: `hard_bounce` is `HardBounce`, `spam_complaint` is `SpamComplaint`, and `manual` and `unsubscribe` are `ManualSuppression`. Origin mapping: recipient-originated entries are `Recipient`; entries added through the API or the console are `Customer`.

### Add and delete

`POST .../suppressions` and `POST .../suppressions/delete` take `{"Suppressions": [{"EmailAddress": "user@example.net"}]}` with at most 50 entries (more is 422, `ErrorCode` 1410). The response is HTTP 200 with one result per entry: `EmailAddress`, `Status` (`Suppressed`, `Deleted` or `Failed`) and `Message`. An invalid address fails only its own entry.

- Added entries have reason `manual` and origin `api`.
- Removal is idempotent but only applies to entries that are not recipient-originated. Removing a `hard_bounce`, `spam_complaint` or `unsubscribe` entry created by the recipient side, or any `spam_complaint`, returns `Failed` with the message "You do not have the required authority to change this suppression." Such entries can be removed in the console only.

Postmark's other suppression behavior (streams other than `outbound`, `EmailAddress` filtering) is not implemented. See [Suppressions](/guide/suppressions).

## Webhooks

Choose the Postmark format when you create a webhook endpoint and the IDs match what the send call returned. Postmark payloads are sent for delivery, bounce, open, subscription-change and spam-complaint events. Postmark does not sign webhooks; use Basic authentication or a custom header. Click events are not sent. See [Webhooks](/guide/webhooks) and [Webhook formats](/reference/webhook-formats).

## Deviations

- Error codes are Thinpost's, apart from 10, 300 and 406 in batches and 406 for all-suppressed single sends.
- No templates, tags, metadata, message streams, or inline CSS.
- Only `outbound` is a valid stream on the suppression routes.
- `TrackLinks` supports `None` and `HtmlOnly` only.
- Only five custom headers are accepted.
- To, Cc and Bcc together are limited to 100 recipients.
- A batch is not atomic and has no idempotency key.
