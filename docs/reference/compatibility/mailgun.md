---
title: Mailgun compatibility
description: The Mailgun messages route Thinpost implements, supported form fields and options, attachments, responses, error mapping and every deviation from Mailgun.
---

# Mailgun compatibility

Tested with `mailgun.js` 14.1.0 (`messages.create`, with `form-data`). See the [overview](/reference/compatibility/) for shared limits and behavior, and [Sending with SDKs](/guide/sending-with-sdks) for setup.

## Authentication

HTTP Basic with the user name `api` and the app key as the password, in a single `Authorization` header. Any other user name, a missing header or a wrong or revoked key is 401.

## Routes

| Route | Purpose |
| --- | --- |
| `POST /v3/{domain}/messages` | Send one message |

Everything else is 404. There is no batch sending, MIME-message route, domain, route, event, stats or template API.

`{domain}` must be a valid host name and an active [sender domain](/guide/domains) of the app, in addition to the `from` domain being one. A path domain that is not authorized for the app is 400. The domain is lowercased, and need not equal the `from` domain.

## Request encoding

The body is `multipart/form-data` or `application/x-www-form-urlencoded`. Attachments require multipart. Any other content type is 400. Every field value must be valid UTF-8.

## Form fields

| Field | Notes |
| --- | --- |
| `from` | Required, exactly once. |
| `to` | Required. Repeatable; each value may be a comma-separated address list. |
| `cc`, `bcc` | Optional, repeatable like `to`. |
| `subject` | Required, not blank, exactly once. |
| `text`, `html` | At least one is required, each at most once. |
| `h:Reply-To` | Reply addresses, at most 10. |
| `h:In-Reply-To`, `h:References`, `h:List-Unsubscribe`, `h:List-Unsubscribe-Post` | See [custom headers](/reference/compatibility/#custom-headers). |
| `o:require-tls` | `yes`, `true`, `no` or `false`. |
| `o:skip-verification` | `no` or `false` only. |
| `o:tracking-opens` | `yes` or `no`. |
| `o:tracking-clicks` | `htmlonly` or `no`. |
| `attachment`, `inline` | Multipart file parts, see below. |

Option values are case-insensitive. Any other field is rejected with 400. That includes `template`, `t:*`, `v:*` variables, `h:` headers other than the five above, `recipient-variables`, `message` (raw MIME), `o:tag`, `o:deliverytime`, `o:dkim`, `o:testmode`, `o:tracking`, and every other `o:` option (the supported ones are listed above). The `h:` prefix is matched in lowercase; the header name after it is case-insensitive.

```bash
curl --user api:tp_example_key \
  https://mail.example.com/v3/example.com/messages \
  -F from="Example <hello@example.com>" \
  -F to=user@example.net \
  -F subject="Welcome" \
  -F text="Hello." \
  -F html="<p>Hello.</p>" \
  -F o:tracking-opens=yes \
  -F o:tracking-clicks=htmlonly \
  -F attachment=@report.pdf
```

### Attachments

Multipart parts named `attachment` are ordinary files; parts named `inline` are inline images referenced as `cid:` URLs.

- The part must have a `Content-Disposition: form-data` header with a `name` and a `filename`, and nothing else. A part with a `Content-Transfer-Encoding` header is rejected.
- For `inline`, the file name is the content ID, so reference it as `cid:logo.png`. Inline files require an `html` body, and the content ID must be unique and use letters, digits and `._@+-`.
- The part's `Content-Type` is used when present, otherwise the type is guessed from the extension, falling back to `application/octet-stream`.
- At most 20 files, and the total size limit in the [overview](/reference/compatibility/#limits) applies.
- A non-file field that carries a `filename` is rejected.

### Enhanced Protection

`o:require-tls=yes` requires validated TLS on every delivery attempt and is equivalent to `X-Thinpost-Enhanced-Protection: true`. If either asks for it, the message requires it. `o:skip-verification=yes` is rejected, so certificate verification can never be skipped. Optional TLS (no `o:require-tls`) still verifies a certificate when the remote server offers STARTTLS.

### Tracking

- `o:tracking-opens=yes` and `o:tracking-clicks=htmlonly` enable a kind; `no` opts out and overrides the app default.
- Any other click value, including `yes`, `pass` and `true`, is rejected, because text bodies are never rewritten.
- Enabling either kind requires an `html` body and a configured tracking runtime.
- The `o:tracking` master switch is rejected.

See [Open and click tracking](/guide/tracking).

## Response

Success is HTTP 200:

```json
{ "id": "<0b8e5a3c3d1f4a529c6e1f2a3b4c5d6e@example.com>", "message": "Queued. Thank you." }
```

`id` is the RFC 5322 `Message-ID` Thinpost puts on the message: the 32-hex ID, then `@` and the lowercased `from` domain, in angle brackets. It is the same ID as in Mailgun-format webhooks, so no mapping is needed. Replies can use it for `h:In-Reply-To` and `h:References`.

## Idempotency

There is no idempotency key. If the outcome of a request is unknown, retrying can duplicate the message.

## Errors

Errors use the Mailgun envelope `{"message": "..."}`. Thinpost's messages are its own text, not Mailgun's.

| HTTP | Meaning |
| --- | --- |
| 400 | Invalid or unsupported fields, invalid Thinpost header, or unauthorized sender or path domain |
| 401 | Missing or invalid credentials |
| 405 | Method not allowed |
| 408 | Request body not received |
| 429 | Recipient quota, pending-message limit or upload rate exceeded |
| 503 | Authentication or submission unavailable |

A message whose recipients are all suppressed is accepted and the recipients drop at delivery time; there is no 4xx for it.

## Webhooks

Choose the Mailgun format and handlers written for Mailgun keep working: payloads carry a `signature` object over a fresh token, and the message ID matches the `id` returned at send time. Events are `delivered`, `temporary_fail`, `permanent_fail`, `opened`, `unsubscribed` and `complained`. The signature does not cover `event-data`, so add HTTP authentication where integrity matters. See [Webhooks](/guide/webhooks) and [Webhook formats](/reference/webhook-formats).

## Deviations

- Only the messages route exists. No batch sending, recipient variables, templates, tags, scheduled delivery, test mode or raw MIME.
- Only the five `h:` headers above are accepted.
- `o:skip-verification` cannot be enabled.
- Click tracking supports `htmlonly` and `no` only.
- Attachments require multipart encoding and the strict part headers above.
- No idempotency key.
