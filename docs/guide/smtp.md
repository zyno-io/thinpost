---
title: SMTP submission
description: Send mail through Thinpost over SMTP from printers, scripts and legacy apps with a per-app credential, and learn the limits, reply codes and From rules.
---

# SMTP submission

Some things that send email cannot call an HTTP API: printers and scanners, NAS boxes, cron jobs, monitoring tools, older applications, and your own mail servers that need a smarthost. They all speak SMTP. Thinpost accepts mail from them on ports 465 and 587, with a username and password that belong to one [app](/guide/concepts).

Mail sent this way is a normal Thinpost message. It uses the same sender-domain checks, [suppression list](/guide/suppressions), recipient rules, submission rate, daily recipient quota and pending-message limit as the HTTP APIs, and it shows up in the console and fires the same webhooks.

If your software can call an HTTP API, use the [native API](/guide/native-api) or a provider SDK instead. They give you more control and clearer errors.

## Turn it on

Two things must happen before a client can send:

1. **An operator enables submission on the edge.** Set `THINPOST_EDGE_SUBMISSION=true`, or `smtpSubmission.enabled: true` in the Helm chart. See [Helm: Edge](/deploy/helm-edge#smtp-submission) and the [router rules](/deploy/direct-public-networking) for ports 465 and 587. Port 25 is unchanged and never offers AUTH.
2. **You create a credential for the app.** In the [console](/guide/console#smtp), open the app's **SMTP** tab and choose **Create credential**. The username and password are shown once, so copy them into the sending software before you close the dialog. An app can have 10 active credentials, so give each device or application its own and revoke one without touching the others.

The SMTP tab follows the control plane's `smtp_submission` feature, so it can be visible even while submission is still off at the edge. Its port badges show which ports Thinpost last saw listening, and it tells you the server name.

## Connection settings

| Setting | Value |
| --- | --- |
| Server | The name shown on the app's SMTP tab. It is the HELO name of the app's [sending IP](/guide/sending-ips), which already has a valid certificate. |
| Port 465 | Implicit TLS (also called SSL/TLS or SMTPS). Recommended. |
| Port 587 | STARTTLS. Set the client to **require** it. |
| Username | The credential's username, a 32-character lowercase hex ID. |
| Password | The credential's password, `tps_` followed by 43 characters. |
| Authentication | `PLAIN` or `LOGIN`. |

Thinpost offers AUTH only after TLS is up, so a correctly configured client never sends its password in clear text. Set the client to require TLS: a client that sends AUTH too early has already put the bytes on the network by the time Thinpost refuses it. A client that tries to sign in on port 587 before STARTTLS gets `523 5.7.10 TLS is required`, and one that skips signing in gets `530 5.7.0 Authentication required`. Use the server name rather than an IP address, so the certificate matches. Mail servers inside your cluster can use the [internal listener](/deploy/helm-edge#smtp-submission), which your operator sets up separately.

### Generic client settings

Most devices and applications ask for the same five things:

```text
SMTP server:      <server name from the SMTP tab>
Port:             465
Security:         SSL/TLS
Username:         <credential username>
Password:         <credential password>
```

Use 587 with STARTTLS only if the device cannot do implicit TLS. Leave "authenticate before sending" or "SMTP-AUTH" switched on.

### msmtp

```text
# ~/.msmtprc
defaults
tls on
# port 465 uses implicit TLS
tls_starttls off

account thinpost
host smtp.example.com
port 465
auth on
user 0123456789abcdef0123456789abcdef
password tps_REPLACE_WITH_THE_PASSWORD
from alerts@example.com

account default : thinpost
```

For port 587, set `port 587` and `tls_starttls on`. msmtp verifies the certificate against the system trust store by default.

### Postfix as a relay

On a server that already runs Postfix, relay everything through Thinpost:

```text
# Postfix main configuration file
relayhost = [smtp.example.com]:465
smtp_tls_wrappermode = yes
smtp_tls_security_level = secure
smtp_tls_CAfile = /etc/ssl/certs/ca-certificates.crt
tls_append_default_CA = no
smtp_sasl_auth_enable = yes
smtp_sasl_security_options = noanonymous
smtp_sasl_password_maps = hash:/etc/postfix/sasl_passwd
```

```text
# /etc/postfix/sasl_passwd
# Key it exactly as relayhost is written, then run: postmap /etc/postfix/sasl_passwd
[smtp.example.com]:465 0123456789abcdef0123456789abcdef:tps_REPLACE_WITH_THE_PASSWORD
```

`secure` makes Postfix verify the certificate against the CA file (the path shown is the Debian and Ubuntu bundle, so use your system's), and the host in `relayhost` must be the server name on the SMTP tab so the name matches. For port 587 use `relayhost = [smtp.example.com]:587`, leave out the wrapper mode, key the `sasl_passwd` entry as `[smtp.example.com]:587`, and run `postmap` again. Postfix queues and retries on its own, which is what you want: see [Retries and duplicate protection](#retries-and-duplicate-protection).

### Testing with curl

```sh
# The username and password from the SMTP tab.
SMTP_USER=0123456789abcdef0123456789abcdef
read -rs SMTP_PASSWORD
curl --ssl-reqd smtps://smtp.example.com:465 \
  --user "$SMTP_USER:$SMTP_PASSWORD" \
  --mail-from alerts@example.com --mail-rcpt customer@example.net \
  -T message.eml
```

`read -rs` asks for the password without echoing it or keeping it in your shell history. `message.eml` is a complete message with at least `From`, `To`, `Subject` and a blank line before the body.

## The From address

Thinpost only sends from domains that are [verified for the app](/guide/domains). Devices often cannot be told to use one, so each app has a **From address policy** on the SMTP tab. It decides what happens to the `From` of every SMTP message. A message that has no `From` header uses the SMTP `MAIL FROM` address as its `From`, under the same rules.

| Policy | What it does |
| --- | --- |
| **Only accept verified addresses** (the default) | Accepts a `From` on one of the app's verified sender domains, as an exact match. Anything else is refused with `550 5.7.1 Sender address not permitted`. |
| **Send from one of my domains** | Rewrites the `From` onto a verified domain you choose and keeps the original address in the part before the `@`, so senders stay distinguishable. |
| **Always send from one address** | Replaces the `From` with one fixed address on a verified domain. |

The two rewriting policies can leave a `From` alone when its domain is already verified for the app ("Leave addresses on my verified domains alone"). The display name is always kept, and the `Sender` header is removed when the `From` changes, and also when there are several `Sender` headers or one is not a single address on a verified domain, even if the `From` is unchanged.

For an app whose domain is `example.com`:

| Policy | Client sends | Message goes out as |
| --- | --- | --- |
| Only accept verified addresses | `Alerts <noreply@example.com>` | `Alerts <noreply@example.com>` |
| Only accept verified addresses | `scanner@office.example.net` | Refused: `550 5.7.1` |
| Send from `example.com` | `Printer <scanner@office.example.net>` | `Printer <scanner=office.example.net@example.com>` |
| Send from `example.com` | `Printer <"team ops"@office.example.net>` | `Printer <team-ops=office.example.net-a0091e7b@example.com>` |
| Send from `example.com` | `Printer <scanner@example.com>` | `Printer <scanner@example.com>` |
| Always send from `noreply@example.com` | `Printer <scanner@office.example.net>` | `Printer <noreply@example.com>` |

The address the client sent is kept on the message as its **original From** and shown in the console. Other headers, including `Reply-To`, pass through untouched, so replies can still reach the device if it sets one.

Addresses in the `From` and the sender must be ASCII.

### How "Send from one of my domains" writes the address

The new address is always `<name>=<original domain>@<your domain>`, for example `scanner@office.example.net` becomes `scanner=office.example.net@example.com`. The original domain is lowercased. When the original domain is already the target domain, the address is just `<name>@<your domain>`.

The name part is made safe to use as an address:

- Letters, digits, `.`, `_` and `-` are kept. Every other character, including spaces, quotes, `=` and `+`, becomes `-`. (`=` is the separator and `+` is often read as a tag, so neither is kept.)
- Runs of dots become one dot, and dots at the start or end are dropped.
- If anything was changed, or the result would be too long (64 characters for the name part, 254 for the whole address), a dash and an 8-character hash are added at the end, and the text before it is shortened to fit. The hash is the first 8 hexadecimal digits of the SHA-256 of the original address (name as written, domain lowercased), so the same original address always produces the same new address, and two different originals that clean up alike are very unlikely to collide. Example: `"team ops"@office.example.net` becomes `team-ops=office.example.net-a0091e7b@example.com`.

This policy never refuses a message because the name cannot be reused. It still answers `451 4.3.5` while the chosen domain is not verified or cannot be DKIM-signed yet, and refuses a message that has no usable `From` or `MAIL FROM` address.

Two things need to be in place for the target domain, or Thinpost answers `451 4.3.5 Sender configuration is not ready, try again later` and the client keeps the message queued:

- the domain is a **verified** sender domain of the app, and
- it has a **DKIM signing key** that is published in DNS. See [Sender domains](/guide/domains#smtp-and-dkim).

The SMTP tab warns you when the chosen domain would not be signed yet.

## Limits

| Limit | Value |
| --- | --- |
| Message size | 7.5 MiB (7,864,320 bytes) once decoded, headers included. Thinpost also checks that the final signed message fits, so a message close to the limit can be refused. |
| Recipients per message | 100 `RCPT TO` commands. Duplicates count, and the 101st is refused with `452 4.5.3`. |
| Header block | 56 KiB and 200 header fields |
| Line length | 998 characters, in headers and in the body |
| Header characters | ASCII only. Encode non-ASCII text in a header (a `Subject` with an accent, a display name) as an [RFC 2047](https://www.rfc-editor.org/rfc/rfc2047) encoded word, as every mail library does. |
| Body | 8-bit text is accepted and passed on as written. NUL bytes and a bare carriage return are refused. Use `quoted-printable` or `base64` for the widest compatibility. |
| Addresses | ASCII only. Thinpost does not offer SMTPUTF8. Recipients must be plain addresses. A quoted local part that isn't a plain name is refused: `"a b"@example.com` gets `501 5.1.3`, and quoting that hides characters such as `"\"ops\""@example.com` gets `553 5.1.3`. Quotes around a plain name (`"ops"@example.com`) are harmless and accepted. |
| Connection | A connection allows three sign-in attempts and 100 messages. A client must sign in within 2 minutes of connecting, and the connection is closed 30 minutes after it was opened. A single message transfer that takes longer than 10 minutes, or stalls, is cut and the connection closed. |

On top of these, the app's own [limits](/guide/console#app-limits) apply: the submission rate, the daily recipient quota and the pending-message limit. A new app starts with all three at zero, which blocks everything until an administrator raises them.

Thinpost buffers a message in memory while it is received, so a heavily loaded edge may ask a client to try again later.

## Reply codes

Thinpost replies with standard codes, so any SMTP client or relay will queue and retry on a `4xx` and bounce on a `5xx`. Success is only reported after the message is committed, the same as for the HTTP APIs.

| Reply | When |
| --- | --- |
| `250 2.0.0 OK: queued as <id>` | The message was accepted. `<id>` is the Thinpost message ID. |
| `250 2.0.0 OK: held as <id>` | The message was accepted [on hold](#holding-a-message). |
| `235 2.0.0 Authentication succeeded` | Sign-in worked. |
| `535 5.7.8 Authentication credentials invalid` | Wrong, unknown or revoked credential. Repeated failures from one address are blocked for a while. |
| `530 5.7.0 Authentication required` | `MAIL` before signing in. |
| `523 5.7.10 TLS is required` | AUTH before STARTTLS on port 587. |
| `454 4.7.0 Temporary authentication failure` | Sign-in could not be checked right now. Try again. |
| `451 4.4.5 Sending rate limit reached, try again later` | The app's submission rate is used up. |
| `452 4.3.1 Sending limit reached, try again later` | The app's daily recipient quota is used up. |
| `451 4.3.1 Too many messages are queued, try again later` | The app's pending-message limit is reached. |
| `451 4.3.5 Sender configuration is not ready, try again later` | The domain the message will be sent from, after the From policy, is not verified or cannot be DKIM-signed yet. Under the default policy an unverified `From` gets `550 5.7.1` instead, but a missing DKIM key gets this under any policy. |
| `550 5.7.1 Sender address not permitted` | The `From` is not allowed by the app's From policy. |
| `550 5.7.1 Sending is paused for this app` | An administrator paused the app. |
| `550 5.7.1 Recipient not permitted` | A recipient is blocked by the deployment's recipient allowlist. The whole message is refused. |
| `550 5.7.10 Encryption required` | The message asks for Enhanced Protection without an encrypted connection. |
| `552 5.3.4 Message size exceeds the limit` (or `Max message size exceeded` when `SIZE=` is declared too big, or `Message too large`) | Over the size limit, or a header block over 56 KiB or 200 fields. |
| `554 5.6.0 Message headers are invalid` | A header is malformed, `X-Thinpost-*` has a bad value, or there is no usable sender. |
| `554 5.6.0 Message content not supported` | Non-ASCII or NUL bytes in headers, a line over 998 characters, a bare carriage return or NUL in the body. |
| `554 5.6.0 Message is empty` | No data. |
| `452 4.5.3` (`Maximum limit of 100 recipients reached`) | More than 100 `RCPT TO` commands. |
| `501 5.1.3` or `553 5.1.3 Quoted addresses are not supported` | A `RCPT TO` address whose local part isn't a plain name (spaces, escaped quotes and similar). Use a plain address. |
| `451 4.3.0 Temporary failure, try again later` or `Temporary delivery failure` | Thinpost could not decide right now: the database, storage or the control connection is unavailable. |
| `421 4.3.2` or `421 4.7.0`, then the connection closes | The service is going away, or the client misbehaved too often on this connection. |

The text after the code can differ slightly from the table, so match on the code. A `4xx` means retry. Very rarely the message was in fact accepted and only the final reply was lost, and then the retry is answered with the original ID if the message has a `Message-ID` or a `Date` (see below). A `5xx` means the message will not be accepted as it is.

## Retries and duplicate protection

SMTP clients retry when they do not see the final reply, for example when a connection drops after Thinpost accepted a message. Without help, that would send the message twice. Thinpost recognizes the retry and answers `250` with the **original** message ID instead of creating a second message.

How a retry is recognized depends on what the message carries:

| The message has | A retry is recognized for | Notes |
| --- | --- | --- |
| A valid `Message-ID` | 24 hours | The same app, sender, recipients, `Message-ID` and exact bytes. |
| No `Message-ID`, but a `Date` | 10 minutes | The same app, sender, recipients and exact bytes. Two messages that differ in `Date`, such as two runs of a cron job, are different messages. |
| Neither | Never | Every submission is a new message. A lost reply can cause a duplicate. |

Real mail servers and libraries always add both. **If you generate messages yourself, add a `Message-ID` header.** A retry of an accepted message is answered even if the app has been paused or its limits changed since, so a client that was waiting on a lost reply is not stuck.

A message with the same `Message-ID` but different content is a new message.

## Holding a message

Add this header to hold a message instead of sending it:

```text
X-Thinpost-Hold: true
```

Thinpost accepts and stores the message, answers `250 2.0.0 OK: held as <id>`, and waits. The app then releases or cancels it with the [app API](/guide/holding-messages#release) and an API key of the app, exactly as for any other held message. Held mail expires after seven days. A held message goes through all the same checks, and counts against the same limits, as one sent right away. See [Holding messages](/guide/holding-messages#holding-over-smtp).

The header takes `true` or `false`, once. Anything else is refused with `554 5.6.0`.

## What Thinpost changes in your message

Thinpost sends the message you submit, with a few changes that keep it safe and deliverable:

- **Removed:** `Bcc`, `Resent-Bcc`, `Return-Path`, every `Received` header you or your software added (they reveal your internal hosts and addresses), any ARC headers, `Authentication-Results`, every `X-Thinpost-*` header, and `Sender` when the `From` was changed (or when it is not a single address on a verified domain).
- **Replaced:** `Message-ID`. Thinpost uses its own, `<message-id@your-domain>`, so replies and bounces can be matched. Your client's `Message-ID` is used for retry protection only. The consequence: a client that wants to thread a later message under this one has no way to know the new ID, unless it reads it from the reply and builds `<id@domain>` itself.
- **Added:** a `DKIM-Signature` for the sender domain, a `Date` when there is none, and, once the sending worker has reported its host name, one `Received` header from Thinpost. The `Received` header names Thinpost's mail host, `with ESMTPSA` (authenticated over TLS), Thinpost's message ID and the time Thinpost accepted the message, for example `Received: by mx.example.net (Thinpost) with ESMTPSA id <message-id>; Fri, 20 Nov 2026 12:00:00 +0000`. It never contains your IP address or host name, and the DKIM signature does not cover it. A `Date` the client sent is always kept, including on held mail. Line endings are normalized to CRLF.
- **Kept:** everything else, including `Reply-To`, `In-Reply-To`, `References`, custom headers, MIME structure and attachments. A DKIM signature the client added is kept on a best-effort basis: it is dropped when the changes above would break it, and Thinpost may also drop one that would still verify. Thinpost's own signature for the sender domain is always added, so this doesn't affect delivery.

[Open and click tracking](/guide/tracking) and one-click unsubscribe are **not** applied to SMTP mail. The message is exactly what the client wrote, so Thinpost does not rewrite links or add a tracking pixel.

## Recipients and Bcc

The SMTP envelope (`RCPT TO`) decides who gets the message. Thinpost sends one copy per recipient, which is why a `Bcc` header is removed and nobody sees the others. A recipient that appears in the `To` or `Cc` header is recorded as that kind, and every other envelope address is recorded as `bcc`. The same address given twice, in any letter case of the domain, is one recipient.

The [suppression list](/guide/suppressions) and the deployment's recipient allowlist apply as usual. Unlike a suppression, which only skips that one recipient, an allowlist violation refuses the whole message.

## Enhanced Protection

Add `X-Thinpost-Enhanced-Protection: true` to require a verified, encrypted connection on every delivery attempt, as described in [Concepts](/guide/concepts). The connection to Thinpost is already encrypted. The header is removed before sending.

## Troubleshooting

| What you see | What to check |
| --- | --- |
| Client cannot connect | The router allows TCP 465 and 587 to the sending IP, and the server name matches the one on the SMTP tab. The tab's port badges show what Thinpost last saw listening. |
| Certificate error | Connect by the server name, not an IP address. The certificate belongs to that name. |
| `530 5.7.0 Authentication required` | The client did not sign in. On 587, make STARTTLS required, because AUTH is only offered after it. |
| `535 5.7.8` | The username or password is wrong, or the credential was revoked. A new credential's password is only shown once. Create another if it is lost. |
| Connection closed with `421` after sign-in errors | Too many failed attempts. Wait a few minutes. Fix the password, because repeated attempts keep the block going. |
| `550 5.7.1 Sender address not permitted` | The `From` is on a domain the app has not verified. Verify it, or choose one of the other From policies. |
| `451 4.3.5` | The sender domain needs to be verified or needs a published DKIM key. |
| `4xx` rate, quota or queue replies | The client should retry. If they never clear, an administrator needs to raise the app's [limits](/guide/console#app-limits). |
| `554 5.6.0` | A header or the body breaks the [limits](#limits). Check for non-ASCII header text and for very long lines. |
| Sent twice | The client retried without a `Message-ID`. Add one, see [Retries](#retries-and-duplicate-protection). |

Not seeing the message at all? Search for it in the app's **Messages** tab, where SMTP mail has an **SMTP** badge and the credential's name. The edge also logs aggregate counters for submission, described in [Monitoring](/operations/monitoring#logs).

## Next

- [Operator console](/guide/console#smtp)
- [Sender domains](/guide/domains)
- [Holding messages](/guide/holding-messages)
- [Threat model](/security/threat-model)
