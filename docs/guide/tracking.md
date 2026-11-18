---
title: Open and click tracking
description: How opens, clicks and one-click unsubscribe work, what the links look like, app defaults, key rotation and the limits of the data.
---

# Open and click tracking

Thinpost can track opens and HTML link clicks, chosen per message or as a per-app default, and can add a one-click unsubscribe link. Tracking is off unless you configure it and an app or message turns it on.

Tracking links are encrypted and self-contained. A click URL carries its original destination inside an authenticated ciphertext that only your Thinpost installation can read, so it can still redirect even if the database record or stored mapping is gone. New links never expire.

::: warning What an event means
Recipients, privacy proxies, security scanners and mail clients all fetch these URLs, so events do not prove a person read or clicked a message. Treat them as signals, not facts.
:::

## Turn it on

Tracking needs configuration on the control plane. Setting only some of the options fails startup.

| Setting | Meaning |
| --- | --- |
| `THINPOST_TRACKING_ORIGIN` | A stable HTTPS origin with no path, query or credentials, such as `https://t.example.com`. The host must be ASCII; give an international name in punycode. |
| `THINPOST_TRACKING_LINK_KEYS_FILE` | Absolute path to the key file for link encryption. |
| `THINPOST_TRACKING_EVENT_LIMIT` | A per-link event limit from 1 to 1,000. |

It also needs the submission fingerprint key, encrypted object storage, the signing catalog and the edge connection to be configured. The origin must keep working for as long as issued links might be used, so choose it once and keep its DNS, certificate and route in place. See [Configuration](/reference/configuration).

## Per-app defaults and per-message choices

An app can default opens and clicks on. Both are off by default. Set them in the console (app, **Tracking**) or with `PUT /api/v1/apps/{appId}/tracking-defaults`.

- **A message's explicit choice always wins**, on or off. A kind the request leaves unset takes the app default. How each SDK expresses it is in [Sending with SDKs](/guide/sending-with-sdks#tracking-per-message).
- **Only HTML bodies are tracked.** A default on a text-only message sends it untracked.
- **A default never causes a rejection.** If a defaulted message fails the size or link-rewrite check (more than 100 links, a link over 2,048 bytes, an oversized document), Thinpost drops the click default first, then the open default, and accepts the message with what remains. An explicit opt-in that does not fit is rejected.
- **The choice is resolved once**, when the message is accepted. A later default change never affects a message already accepted.
- **A runtime is required.** Turning a default on returns 409 on a replica without tracking configured. Turning one off is always allowed.

## What gets rewritten

Before signing, Thinpost rewrites each recipient's HTML. Absolute HTTP(S) links become tracked links, and an open-tracking pixel is added. Relative links, fragments, other schemes and URLs that contain credentials are left unchanged. Input size, link count and HTML complexity are bounded. HTML parsing can normalize markup.

Link paths look like this:

```text
/t/c/v2/<key-id>/<payload>   click
/t/o/v2/<key-id>/<payload>   open
/t/u/v2/<key-id>/<payload>   unsubscribe
```

The payload is an authenticated ciphertext (XChaCha20-Poly1305) holding the recipient token and, for clicks, the destination, padded to a multiple of 64 bytes. It is bound to your origin, the link kind and the key generation, so it cannot be moved to another origin or turned into a different kind of link. Any tampering, or any other failure, produces an empty 404. Opens and clicks answer 405 for methods other than GET and HEAD. Query parameters are rejected.

Because the URL holds no readable message content or recipient identity, access logs at rest reveal neither. A logged URL is still a working capability: anyone who can read the logs and reach your origin can replay it. That is inherent to mail links. Limiting retention of `/t/` request logs is an optional extra layer.

## One-click unsubscribe

Apps that opt in (console **Tracking** tab, or `PUT /api/v1/apps/{appId}/list-unsubscribe`) get an RFC 8058 `List-Unsubscribe` link and `List-Unsubscribe-Post: List-Unsubscribe=One-Click` header on every message, covered by the DKIM signature. A request that carries its own `List-Unsubscribe` headers is rejected while this is on. A new app has it off.

| Method | Behavior |
| --- | --- |
| `POST` with `List-Unsubscribe=One-Click` | Records the unsubscribe once and answers 200, including for repeats. 404 when the link is no longer valid. 503 when the service cannot record it, so mailbox providers and people retry. |
| `GET` | Never changes state, because scanners prefetch. It shows a page with a single button that POSTs to the same URL. |
| `HEAD` | 200, with no action. |

A successful unsubscribe adds the recipient to the app's [suppression list](/guide/suppressions) with reason `unsubscribe`, records an `unsubscribed` webhook event and leaves the delivery's state alone. The address comes from Thinpost's own record of the delivery, never from the request. Mailto unsubscribe is not implemented.

If you manage unsubscribes yourself, you can pass your own `List-Unsubscribe` and `List-Unsubscribe-Post` headers with the send request while Thinpost's managed link is off.

## Events, budgets and limits

For a valid link, analytics are best effort and bounded to one second of dependency work. A missing record, closed gate, exhausted budget or unavailable database never breaks the redirect or the pixel. HEAD requests resolve without recording anything.

Two limits bound what is stored and sent:

- a **per-link event cap** (`THINPOST_TRACKING_EVENT_LIMIT`), and
- a **per-app daily event budget** in UTC, which defaults to **zero**. Set it with `PUT /api/v1/apps/{appId}/tracking-quota` or in the console.

Beyond either limit, links keep resolving and an aggregate "not recorded" counter increases. Opens and clicks that are recorded appear in the message detail and as `opened` and `clicked` webhook events, which carry an opaque `tracking_link_id` and never the destination. Provider-format click events are not emulated yet. The console shows recorded events after the caps, so it can undercount. It shows no destinations, unique opens or rates.

An app pause stops webhook claims but does not revoke links or stop recording permitted events. Deleting a message does not revoke its redirect or pixel, because the link format does not depend on storage.

## Key rotation

The link key file is JSON with `seed` (the active 32-byte seed, base64) and optional `previous_seeds`. Rotate in two phases so a rolling deploy never meets a link it cannot read:

1. Add the new seed to `previous_seeds`, leave `seed` unchanged and roll out. Every replica can now read links under the new seed.
2. Set `seed` to the new seed, move the old seed into `previous_seeds`, remove the new seed from `previous_seeds`, and roll out.

New links use the new seed. Links under every retained seed keep working. A retained seed is a secret you must keep as long as you want its links to work. Removing a seed invalidates every link issued under it, including after a compromise. Every replica must receive the same keys. See [Key and certificate rotation](/operations/rotation).

## Next

- [Suppressions](/guide/suppressions)
- [Webhooks](/guide/webhooks)
