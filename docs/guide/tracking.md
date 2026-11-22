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

It also needs the submission fingerprint key, encrypted object storage (its keyring also seals the DKIM keys kept in PostgreSQL) and the edge connection to be configured. The origin must keep working for as long as issued links might be used, so choose it once and keep its DNS, certificate and route in place. See [Configuration](/reference/configuration).

### Keep the console off the tracking hostname

Every recipient sees the tracking hostname in their links. If the console is served on it too, anyone who receives a message can find the console. To prevent that, give the console its own hostname and set `THINPOST_ADMIN_ORIGIN` to it, for example `https://thinpost-admin.example.com`. The console and operator API then answer only there. On the tracking hostname, and any other, the root and the admin paths return an empty 404, while tracking links keep working.

- The tracking origin is part of every issued link. Keep the tracking origin and move the console to a new hostname.
- Route both hostnames to the control service. Your identity provider needs the console's new redirect URI.
- Matching uses the request's hostname and ignores the port. That hostname comes from the `Host` header, or from the request line when it carries an absolute URI. The gateway must forward the hostname it routed on unchanged and must not let clients set it through `X-Forwarded-Host`. For defense in depth, also have the gateway send only `/t/` paths to Thinpost on the tracking hostname.
- This hides the console rather than protecting it; operator sign-in still guards it. Certificate Transparency logs publish the hostnames on publicly trusted certificates, so put the admin hostname under a wildcard certificate if it should not be easy to find. Never use one certificate that names both hostnames: anyone who connects to the tracking hostname can read it.
- Health probes use the pod address and keep working.
- Setting `THINPOST_ADMIN_ORIGIN` is also what lets apps use [their own tracking domains](#your-own-tracking-domain).

## Your own tracking domain

By default, tracked links and the open-tracking image point at your deployment's tracking origin. An app can use a name of its own instead, such as `links.example.com`, so recipients see the app's domain. It is optional, and a deployment has to offer it: see [What your gateway must provide](#what-your-gateway-must-provide).

**Set it up** on the Sending domains tab, in the tracking-domain panel under a verified domain (API: `PUT /api/v1/apps/{appId}/domains/{domain}/tracking-domain` with `{"name": "links.example.com"}`).

1. **Choose a name.** It must be a subdomain of the sending domain (not the sending domain itself), at most 253 characters, and not one of Thinpost's own hostnames (the tracking host or the console's). Several apps may use the same name. A sending domain has one tracking domain at a time, and setting a new name replaces the old one in one step. The sending domain must be verified. The API answers 409 when the deployment doesn't offer custom tracking domains.
2. **Point DNS at the tracking host.** Add a CNAME record for the name to the deployment's tracking host, which the console shows. A name behind a proxy (such as Cloudflare's orange cloud) works too: Thinpost only needs HTTPS to work. This differs from a [bounce domain](/guide/domains#your-own-bounce-domain), which must be DNS only.
3. **Your operator finishes the address.** They route `/t/` for your name to Thinpost and obtain a certificate for it ([below](#what-your-gateway-must-provide)).
4. **Check it.** Thinpost checks every 10 minutes for the first 48 hours while it waits, then every 6 hours, and you can choose **Check now** (API: `POST .../tracking-domain/check`). Once the name passes, it is **In use**.

**What decides.** Thinpost makes an HTTPS request to `https://<name>/t/h/v1/<random challenge>` with normal certificate validation, and the answer must be the [host self-check](#host-self-check) proof. That proves one thing: the name reaches *this deployment* over a valid certificate. It connects only to public addresses, follows no redirects, uses no proxy, and reads at most 512 bytes within 10 seconds (at most 4 seconds for each address it tries). The result is one of `ok`, `unreachable` (no connection, a timeout, a DNS error other than NXDOMAIN, or a 5xx or 429 answer, which is usually a transient gateway error), `certificate` (the certificate did not validate), `redirected` (the route answered with a redirect instead of serving `/t/`), `not_thinpost` (a 4xx other than 429, or not this deployment's proof) or `not_found` (the name no longer exists in DNS). The certificate's expiry is recorded when a connection completes.

**DNS is advice only.** Thinpost also looks at the name's DNS and reports `ok` (a CNAME to the tracking host, or the same addresses), `missing`, `wrong_target` (expected behind a proxy) or `lookup_failed`. This is shown so people can see what to change. It never decides whether the domain is used.

**What new mail does.** The origin is chosen for each message when it is prepared, from current state, never from a remembered one. A message from the sending domain (or one of the app's verified subdomains of it; a subdomain uses the most specific domain that is in use, and falls back to its parent's) uses the custom domain for body links and the open-tracking image only when **all** of these hold, and otherwise uses the deployment's tracking origin as before:

- the domain is active and an HTTPS check passed within the last 3 hours (active domains are rechecked every 30 minutes);
- the deployment has `THINPOST_ADMIN_ORIGIN` set, so the console can never answer on a customer's name.

**One-click unsubscribe keeps the deployment's origin.** The `List-Unsubscribe` link is a compliance function that has to outlive a customer's DNS, so it never uses the custom domain.

**States.** `pending` (no passing check yet), `active`, `failing` (it was active and its checks failed: an HTTPS check that is definitive, such as `certificate`, `redirected`, `not_thinpost` or `not_found`, fails it at once; `unreachable` only after two in a row; new mail uses the deployment's origin meanwhile) and `removed`. Changes of state are in the audit trail as `tracking_domain.added`, `activated`, `failing` and `removed`.

**Removing it** (API: `DELETE .../tracking-domain`) sends the next message with the deployment's origin. The row stays as a *removed* tombstone that is still checked once a day and listed for operators, because links already sent keep working only while the name's DNS, route and certificate stay in place. Keep them for as long as old emails matter. Adding the same name again revives it as pending.

Links are not bound to a hostname, so a link sent earlier opens at any name that still routes to the deployment, before and after a domain is added or removed.

## What your gateway must provide

Thinpost does not terminate TLS for customer names. For each custom tracking domain, the operator provides:

- **A route** for `https://<custom-domain>/t/` (GET, HEAD and POST) to the control service's HTTP port. Send only `/t/` paths if you can.
- **A publicly trusted certificate** for the name, renewed automatically by your tooling. Do not share a certificate with the console hostname: the certificate's names are visible to anyone who connects.
- **`Host` forwarded unchanged**, and never taken from a client's `X-Forwarded-Host`. With ingress-nginx, do not enable `use-forwarded-headers` and do not set `upstream-vhost`.
- **`THINPOST_ADMIN_ORIGIN` set**, or the deployment doesn't offer custom tracking domains at all.
- **Egress from the control plane** to public DNS and to the custom name on port 443 (a hairpin through your gateway). Control connects only to public addresses.
- **The order of setup:** the customer's CNAME, then your route and certificate (an HTTP-01 challenge needs the CNAME first), then Thinpost's check, then the domain is active. A proxied name needs no record that points at the tracking host; the proxy only has to reach your route.
- **After removal,** keep the route and certificate as long as old links matter.

`GET /api/v1/tracking-domains` (operator) lists every tracking domain of every app, with `state`, `dns_result`, `https_result`, `certificate_expires_at`, `checked_at` and removed tombstones, so your automation can find "DNS ok, waiting for HTTPS" names and issue certificates for them, renew before expiry and keep routes for removed names. There is no on-demand-TLS "ask" endpoint yet.

Any host that reverse-proxies `/t/h/` to your deployment passes the check. That is by design: the proof means "this deployment", and only an operator-added route can reach it. Only a domain an admin added under the app's verified sender domain is ever used for links.

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
/t/c/v3/<key-id>/<payload>   click
/t/o/v3/<key-id>/<payload>   open
/t/u/v3/<key-id>/<payload>   unsubscribe
```

The payload is an authenticated ciphertext (XChaCha20-Poly1305) holding the recipient token and, for clicks, the destination, padded to a multiple of 64 bytes. It is bound to the link kind and the key generation, so it cannot be turned into a different kind of link or replayed under another key generation. It is not tied to a hostname: the origin in the URL only says where the link points, and any hostname that routes to your Thinpost deployment serves any link, ignoring the `Host` header. (Links from before this format, `/v2/`, are not accepted.) Any tampering, or any other failure, produces an empty 404. Opens and clicks answer 405 for methods other than GET and HEAD. Query parameters are rejected.

### Host self-check

To confirm that a hostname reaches your deployment, Thinpost can request `GET /t/h/v1/<challenge>` (also `HEAD`), where the challenge is 32 random bytes in unpadded base64url. The answer is `200 text/plain` with `Cache-Control: no-store` and the body:

```text
thinpost-host-check v1 <key-id> <base64url(HMAC)>
```

The MAC is keyed by a secret derived from your tracking key seed, so only a deployment holding the tracking keys can produce it, and it covers the challenge only, not the hostname. A key id of any retained seed verifies, so a check keeps working through key rotation. Queries and malformed challenges get an empty 404.

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
- a **per-app daily event budget** in UTC, which defaults to **100,000** events for a new app (zero records nothing). Set it with `PUT /api/v1/apps/{appId}/tracking-quota` or in the console.

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
