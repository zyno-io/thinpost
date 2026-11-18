---
title: Webhooks
description: Receive signed delivery, bounce, open, unsubscribe and complaint events, in Thinpost-native, Postmark, Resend or Mailgun format.
---

# Webhooks

Thinpost posts events about your mail to HTTPS endpoints you register per app. Each endpoint has a **format**: Thinpost-native JSON, or a Postmark-, Resend- or Mailgun-shaped payload so a handler written for that provider keeps working. SendGrid webhooks are not available.

Webhooks run in the control plane and need the object-encryption keyring, because endpoint secrets are sealed with it. Without the keyring the endpoint API returns 503 and the dispatcher does not start.

## Create an endpoint

Administrators manage endpoints in the [console](/guide/console) (app, **Webhooks**) or the admin API (`/api/v1/apps/{appId}/webhooks`). Every change is audited. Audit entries record the endpoint ID only, never its URL or secret.

An endpoint has:

- a **format**, fixed at creation (create a new endpoint to change it);
- an **HTTPS URL**;
- an optional description;
- an optional **event filter**, using the format's own event names (an unfiltered endpoint receives every event its format can represent);
- optional [HTTP authentication](#http-authentication).

Only events created after an endpoint is committed are queued for it. An app can have at most ten enabled endpoints.

### Managing an endpoint

| Action | Queued or retrying deliveries | In-flight attempt |
| --- | --- | --- |
| Edit URL or description | Sent to the new URL at their next attempt | Completes against the old URL |
| Edit authentication | Sent with the new credentials at their next attempt | Completes with the old credentials |
| Edit filter | Already-queued deliveries stay queued | Unaffected |
| Rotate secret | Signed with the new secret at their next attempt | Was signed with the old secret |
| Disable | Paused; they stay queued and resume on enable | May complete |
| Enable | Resume | n/a |
| Delete | Become cancelled and are never replayed | May still reach the receiver |

Delete is a tombstone: the sealed secret and authentication are removed and delivery history is kept, shown as a deleted endpoint. Disabling or deleting an endpoint means its failed deliveries cannot be replayed.

## Events

The native format has one event per delivery outcome. Each delivery attempt creates an event, queued only for that app's active endpoints, in the same transaction that records the attempt result. Native events carry identifiers, the outcome, the SMTP code and times. They never carry bodies, subjects or addresses.

| Outcome | Meaning |
| --- | --- |
| `accepted` | The remote server accepted the message |
| `rejected` | The remote server rejected it, temporarily or permanently |
| `route_rejected` | A rejection while the connection was being set up |
| `null_mx`, `domain_not_found` | Thinpost decided locally from DNS that the domain cannot receive mail. No SMTP was attempted and the SMTP code is zero |
| `retry_expired` | The retry lifetime ran out before another attempt |
| `suppressed` | The recipient is on the app's [suppression list](/guide/suppressions); nothing was sent. Carries `suppression_reason` |
| `uncertain` | An attempt may or may not have reached the recipient and needs an operator |
| `recipient_policy_denied` | The recipient was denied by policy before sending, such as a recipient allowlist; the delivery is held until an operator retries it |
| `preparation_failed` | The message could not be prepared after bounded retries; it has a message ID but no delivery |
| `opened`, `clicked` | A tracking pixel or link was fetched ([tracking](/guide/tracking)) |
| `unsubscribed` | A recipient used one-click unsubscribe |
| `complained` | A mailbox provider reported a spam complaint |
| `bounce_received_unverified` | Mail arrived at a delivery's return path. It is **not** an authenticated bounce and must not by itself suppress an address |
| `tracking_unavailable`, `tracking_abandoned` | Tracking-related holds and an operator's decision to fail one |

Native events also carry `delivery_state` (so a receiver can tell a retried 4xx from a final one) and `message_id_header`, the RFC 5322 `Message-ID` with angle brackets. Parsers that reject unknown JSON fields need to accept both.

The `message_id` in native events is the 32-character lowercase hex ID. Provider formats render it in the provider's shape, so choose the format of the SDK your app sends with and the IDs match what its send call returned.

### Provider formats

| Thinpost event | Postmark | Resend | Mailgun |
| --- | --- | --- | --- |
| `accepted` | `Delivery` | `email.delivered` | `delivered` |
| temporary `rejected` | not sent | `email.delivery_delayed` | `temporary_fail` |
| permanent failure | `Bounce` | `email.bounced` | `permanent_fail` |
| `suppressed` | not sent | `email.suppressed` | `permanent_fail` |
| `opened` | `Open` | `email.opened` | `opened` |
| `unsubscribed` | `SubscriptionChange` | not sent | `unsubscribed` |
| `complained` | `SpamComplaint` | `email.complained` | `complained` |
| `clicked` | not yet | not yet | not yet |

An event a format cannot represent is never queued for that endpoint. Fields Thinpost does not record are omitted, not invented or set to null. The mapping, field lists and deviations are in [Webhook formats](/reference/webhook-formats).

::: info Enhanced Protection
A provider payload carries only what an operator can read in the console without an audited content read. Subjects of Enhanced Protection messages are never sent, and no format includes bodies. Provider payloads can include recipients and non-Enhanced-Protection subjects, so use Enhanced Protection for sensitive messages.
:::

## Where Thinpost will send

An endpoint URL must satisfy all of these:

- HTTPS on port 443 (an explicit `:443` is dropped when stored). Query strings are allowed. Fragments, user information, control characters and URLs over 2,048 bytes are rejected.
- A DNS **host name**, not an IP literal. Single-label hosts and the suffixes `localhost`, `.local`, `.internal`, `.svc` and `.home.arpa` are rejected.
- Every address the name resolves to must be **public unicast**. This is checked at connection time, on every attempt, not only when the endpoint is saved. Private, loopback, link-local (including cloud metadata), carrier-grade NAT, documentation and other special-purpose ranges are refused, and a name that resolves to any one of them fails. The resolved address is pinned for the connection and TLS verifies the original name.
- Redirects are not followed and no proxy is used.

### Internal destinations

A receiver inside your cluster that has no public name can be allowed explicitly. The operator lists exact URLs in `THINPOST_WEBHOOK_INTERNAL_DESTINATIONS`, together with the cluster's Service ranges in `THINPOST_SERVICE_CIDRS`. Both are empty by default, which leaves every rule above unchanged. An internal destination:

- must be an exact URL match on a `<service>.<namespace>.svc.cluster.local` name;
- relaxes only HTTPS, port 443 and the public-address rule, for that URL only, and still requires every resolved address to fall inside the configured Service ranges;
- must use the native format with no credentials, because plain HTTP is not encrypted in transit. Native events carry identifiers and outcomes, never message content or addresses;
- must also be allowed by your network policy, since the application check does not open the network.

Create or change such an endpoint with the admin API. The console form requires a public HTTPS URL.

## Verify signatures

### Thinpost native

Each request carries `X-Thinpost-Event-ID` (a stable ID to deduplicate on), `X-Thinpost-Timestamp` (Unix seconds) and `X-Thinpost-Signature: v1=<hex>`, where the signature is `HMAC-SHA256(secret, timestamp + "." + event_id + "." + raw_body)`. The secret begins `tpwh_`, and the whole string including the prefix is the HMAC key.

```js
import { createHmac, timingSafeEqual } from "node:crypto";

export function verify(secret, headers, rawBody, toleranceSeconds = 300) {
  const timestamp = headers["x-thinpost-timestamp"];
  const eventId = headers["x-thinpost-event-id"];
  const given = (headers["x-thinpost-signature"] ?? "").replace(/^v1=/, "");
  if (Math.abs(Date.now() / 1000 - Number(timestamp)) > toleranceSeconds) return false;
  const expected = createHmac("sha256", secret)
    .update(`${timestamp}.${eventId}.${rawBody}`)
    .digest("hex");
  const a = Buffer.from(expected);
  const b = Buffer.from(given);
  return a.length === b.length && timingSafeEqual(a, b);
}
```

Use the raw request body, a constant-time comparison and a short timestamp window.

### Other formats

| Format | Secret | How to verify |
| --- | --- | --- |
| Resend | `whsec_` plus base64 | Resend/Svix headers `svix-id`, `svix-timestamp` and `svix-signature`. Use `resend.webhooks.verify` or any Svix library. |
| Mailgun | 64 hex characters | Mailgun's `signature` object over a fresh token. It does not cover `event-data`, so also use HTTP authentication where integrity matters. |
| Postmark | none | Postmark does not sign. Use Basic authentication or a custom header, as the console reminds you. |

Secrets are generated by Thinpost, shown **once** when created or rotated, and never returned again. A lost secret means rotating it. Rotation replaces the secret immediately with no overlap window, so update the receiver first if it can accept both, or expect retries until you do.

### HTTP authentication

Any format can add HTTP Basic credentials and up to five custom headers to every delivery. Passwords and header values are write-only: sealed, never returned and never logged. Reserved header names (for example `Host`, `Content-Type`, and the `X-Thinpost-`, `Svix-`, `Webhook-` and `X-PM-` prefixes) are refused. Updating authentication replaces it entirely, so re-enter every value. Setting it to empty removes it.

## Delivery, retries and replay

Delivery is **at least once**. Deduplicate on the event ID: a crash after your receiver accepts an event but before Thinpost records the result produces a duplicate.

- Thinpost makes up to **12 attempts** on any non-2xx response, with exponential backoff capped at six hours, about 20 hours in total. This policy applies to every format. Postmark's drop-on-4xx and Mailgun's "406 means stop" are not emulated.
- Thinpost records the HTTP status and attempt state, never response bodies or secrets.
- An in-flight attempt that never reports (a crashed replica) is marked unknown and retried.
- An administrator can **replay** a failed delivery from the console or the API, while its app and endpoint are active. Replay retains previous attempts and the event ID and resets the retry counter. The receiver must still deduplicate, because an earlier unknown attempt may have succeeded.

### Test events

The console's **Send test** posts a signed event in the endpoint's format and shows the result. Native endpoints receive `outcome: "test"`. Provider formats receive their primary delivered event with all-zero IDs, the recipient `test@example.com` and the header `X-Thinpost-Test: true`. A strict handler may answer 404 for the unknown ID, which the result shows. Test events are never queued or retried and do not appear in delivery history.

## Inbound mail endpoints

The `thinpost_inbound` format delivers mail an app receives, rather than events about sent mail. See [Inbound mail](/guide/inbound).

## Next

- [Webhook formats](/reference/webhook-formats): payload and field details.
- [Suppressions](/guide/suppressions)
