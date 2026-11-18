---
title: SendGrid compatibility
description: The SendGrid v3 Mail Send route Thinpost implements, supported fields and personalizations, responses, error mapping and every deviation from SendGrid.
---

# SendGrid compatibility

Tested with `@sendgrid/mail` 8.1.6 (`send` with one or several personalizations, and `sendMultiple`). See the [overview](/reference/compatibility/) for shared limits and behavior, and [Sending with SDKs](/guide/sending-with-sdks) for setup.

## Authentication

Send `Authorization: Bearer <app key>` exactly once. A missing, malformed, wrong or revoked key is 401. The SendGrid SDK warns when a key does not start with `SG.`; Thinpost keys do not, and the request is still sent.

## Routes

| Route | Purpose |
| --- | --- |
| `POST /v3/mail/send` | Send one message, or one message per personalization |

Everything else is 404. SendGrid's other v3 APIs (templates, suppressions, stats, webhooks, marketing) do not exist here, and there is no SendGrid suppression API.

## Request fields

| Field | Notes |
| --- | --- |
| `from` | Required object `{email, name}`. Its domain must be an active sender domain of the app. |
| `reply_to` | Object `{email, name}`. |
| `reply_to_list` | Non-empty array of such objects, at most 10. Not combinable with `reply_to`. |
| `personalizations` | Required, 1 to 100 objects, see below. |
| `subject` | Required unless every personalization sets its own. |
| `content` | Array of `{type, value}`. `type` is `text/plain` or `text/html`, each at most once. |
| `attachments` | Array, see below. |
| `tracking_settings` | See below. |
| `headers` | Object of string values, at most five entries, restricted to the [accepted headers](/reference/compatibility/#custom-headers). |

Any other field is rejected with 400, including `categories`, `custom_args`, `send_at`, `batch_id`, `template_id`, `asm`, `ip_pool_name` and `mail_settings`. Every object inside the supported fields is also decoded strictly: an unknown key is a 400. There are no substitutions or dynamic template data.

A message needs at least one of `text/plain` and `text/html`. The order of `content` entries does not matter.

### Personalizations

Each personalization becomes its own message. Supported keys:

| Key | Notes |
| --- | --- |
| `to` | Required, array of `{email, name}`. |
| `cc`, `bcc` | Arrays of `{email, name}`. An empty array is 400. |
| `from` | Optional `{email, name}` overriding the root `from` for this message. |
| `subject` | Optional string overriding the root `subject`. `null` is 400. |

Other keys (`headers`, `substitutions`, `custom_args`, `dynamic_template_data`, `send_at`) are rejected. Root-level `cc` and `bcc` are rejected; recipients are per personalization.

Within one message, To, Cc and Bcc together are limited to 100 addresses. Across the whole request the limit is 1,000 recipients. Shared content, attachments, reply addresses and tracking settings apply to every message. Each distinct sender domain, including per-personalization `from` overrides, must be an active sender domain of the app.

```bash
curl https://mail.example.com/v3/mail/send \
  -H "Authorization: Bearer tp_example_key" \
  -H "Content-Type: application/json" \
  -d '{
    "from": { "email": "hello@example.com", "name": "Example" },
    "personalizations": [
      { "to": [{ "email": "one@example.net" }], "subject": "Welcome, one" },
      { "to": [{ "email": "two@example.net" }], "subject": "Welcome, two" }
    ],
    "content": [{ "type": "text/plain", "value": "Hello." }]
  }'
```

### Attachments

Each entry has `filename`, `content` (canonical base64), and optionally `type`, `disposition` (`inline` or `attachment`) and `content_id`. Empty content is rejected. A `content_id` makes the attachment inline, which requires `text/html` content and a unique ID of letters, digits and `._@+-`, at most 128 bytes; `disposition` must agree. When `type` is absent it is guessed from the extension.

### Tracking

`tracking_settings` accepts only:

```json
{
  "open_tracking": { "enable": true },
  "click_tracking": { "enable": true, "enable_text": false }
}
```

- `open_tracking.enable` must be a boolean; no other key (such as `substitution_tag`) is accepted.
- `click_tracking.enable: true` requires `enable_text: false` to be present. `enable_text: true` is rejected, because text bodies are never rewritten.
- `enable: false` opts out and overrides the app default.
- `subscription_tracking`, `ganalytics` and other settings are rejected.

Enabling either kind requires `text/html` content and a configured tracking runtime. See [Open and click tracking](/guide/tracking).

## Responses

Success is HTTP 202 with an empty body. A request with exactly one personalization also carries an `X-Message-Id` header holding the 32-hex message ID. A request with several personalizations creates several messages, is committed atomically, and returns no `X-Message-Id`; use [webhooks](/guide/webhooks) or the admin API to learn the IDs.

The 202 is returned only after every message in the request has committed. A single personalization uses the single-message path and may be as large as the single-message body limit; several personalizations are limited to 2 MiB in all, and a request over that is 400.

The SDK's `sendMultiple` sends one personalization per recipient, so it creates one message per recipient and returns no ID.

## Idempotency

There is no idempotency key. If the outcome of a request is unknown, retrying can duplicate every message in it.

## Errors

Errors use SendGrid's envelope, with a message only:

```json
{ "errors": [{ "message": "invalid or unsupported message" }] }
```

Thinpost does not fill in SendGrid's `field` or `help` keys.

| HTTP | Meaning |
| --- | --- |
| 400 | Invalid or unsupported request, invalid Thinpost header, over-limit recipients or size, or unauthorized sender domain |
| 401 | Missing or invalid key |
| 405 | Method not allowed |
| 408 | Request body not received |
| 429 | Recipient quota, pending-message limit or upload rate exceeded (several personalizations report `batch quota exceeded`) |
| 503 | Authentication or submission unavailable |

A message whose recipients are all suppressed is accepted and the recipients drop at delivery time; there is no 4xx for it.

## Webhooks

SendGrid-format webhooks are not available. Use the Thinpost-native format, or the Postmark, Resend or Mailgun formats, which are independent of the send adapter. See [Webhooks](/guide/webhooks) and [Webhook formats](/reference/webhook-formats).

## Deviations

- No templates, substitutions, categories, custom arguments, scheduled sends, IP pools, mail settings, unsubscribe groups or SendGrid event webhooks.
- `content` is restricted to `text/plain` and `text/html`.
- Click tracking requires `enable_text: false`; text tracking is not supported.
- Success carries `X-Message-Id` only for single-message requests.
- Error bodies contain only `message`.
- No idempotency key.
- Only Mail Send exists; no other SendGrid v3 API is implemented.
