---
title: Sender domains
description: Verify a sender domain by publishing the DKIM record Thinpost generates, rotate keys, and understand what Thinpost does and does not check in DNS.
---

# Sender domains

An app can only send from domains that are verified for it. Thinpost makes a DKIM key for each domain, and **publishing that key's DNS record is what verifies the domain**: the record proves you control the name, and it is also what lets receivers check Thinpost's signatures. SPF and DMARC are separate DNS work that you do alongside it.

## Verify a domain

1. **Add the domain.** In the [console](/guide/console) open the app, choose **Sending domains** and add the domain. Through the API this is `POST /api/v1/apps/{appId}/domains`. Thinpost generates an RSA-2048 key for the domain and keeps its private half; you never handle it.
2. **Publish the DKIM record.** Thinpost shows a TXT record name and value. Publish it at your authoritative DNS provider.

   | Record | Name | Value |
   | --- | --- | --- |
   | TXT | `thinpost1._domainkey.mail.example.com` | `v=DKIM1; k=rsa; p=<base64 public key>` |

   The value holds only the public key, so it is not a secret. The console and the API show it at any time, and adding the same domain again returns the same record. The record is about 410 characters. Most DNS providers split it into 255-character strings for you; if yours refuses it, copy it with **Split into 255-character strings** in the console, which gives `"first part" "second part"`.
3. **Verify.** Choose **Verify** (or `POST .../domains/{domain}/verify`). Thinpost looks the record up through the control plane's DNS resolver. When it finds this key, the domain becomes verified and the key becomes the one that signs its mail.

Adding, creating and activating keys and verifying a domain are audited.

Thinpost accepts the record when exactly one TXT record exists at that name, it is a plain RSA key for email (`k=rsa`, SHA-256, no other service), it is not in testing mode, and its `p=` value is this key. Whitespace inside `p=` and extra tags such as `n=` are fine. Otherwise verification answers `409` with a reason, and the console explains it:

| Reason | What it means |
| --- | --- |
| `not_found` | No TXT record at the name yet. DNS changes can take a while to spread. |
| `multiple_records` | More than one TXT record at the name. Keep exactly one. |
| `different_key` | A record exists but its key is not the one Thinpost made. Check that you copied the whole value. |
| `testing_mode` | The record has `t=y`, which tells receivers to ignore failures. Remove it. |
| `unsupported` | Not a standard DKIM record (another key type, a different hash, or another service). |

A failed DNS lookup is `503`; try again.

::: warning What verification does not prove
Verification shows that the resolver Thinpost uses returned the expected record. Thinpost does not validate DNSSEC. A verified domain does not mean SPF, DMARC or reverse DNS are configured, and the console says so. Domain removal is not available yet.
:::

Receiving-domain verification for [inbound mail](/guide/inbound) is separate and uses its own `_thinpost` TXT name. Sender domains no longer use that name.

## DKIM signing

Thinpost signs every message with the domain's active key, using the sender domain as the `d=` value so DKIM aligns with the `From` domain for DMARC, even under strict alignment (`adkim=s`). The selector is `thinpost1` for a domain's first key, then `thinpost2`, and so on. Selectors are unique for a domain across all apps that use it, and a retired selector is never reused.

Thinpost rechecks the DNS record before it uses a key: the selector must have a single TXT record with the matching public key. It never signs with a key that is not published right now, and it also rechecks that the app and domain are still active. Signing resolves public keys through DNS without DNSSEC validation.

The private key is stored in the database, encrypted with the [object encryption keyring](/deploy/object-storage), and bound to its row so a copy moved to another row will not open. A database dump alone cannot sign mail; the database plus the keyring can. There are no key files to mount and nothing to restart. Control needs the object encryption keyring to create or use keys.

### Domains verified before DKIM keys existed

A domain that was verified by an earlier release has no key until you create one. The console shows **No DKIM key yet** and a **Create DKIM key** button (API: `POST .../domains/{domain}/dkim-keys`). Publish the record it shows and choose **Verify**. Until the key is active:

- SMTP submission from the domain is refused with `451 4.3.5`, and the client keeps the message queued.
- The HTTP API still accepts the message, but it can't be prepared. After its retries (12 attempts) it shows as `preparation_failed`. Once the key is active, [retry its preparation](/operations/recovery).

A domain that was still waiting for its old `_thinpost` challenge when you upgraded gets a DKIM record instead; add it again or choose **Create DKIM key**.

### Rotate a key

Rotation never leaves a gap, and you decide when it switches:

1. **Create the next key.** Choose **Rotate key** (API: `POST .../domains/{domain}/dkim-keys`). Thinpost makes `thinpostN` and shows its record. The current key keeps signing.
2. **Publish the new record** at its own name, then choose **Check record** (the same verify call). When Thinpost sees it, the key shows as ready to switch.
3. **Switch.** Choose **Switch** (API: `POST .../dkim-keys/{selector}/activate`). Thinpost checks DNS again, then in one step makes the new key the signing key and retires the old one. The old private key is deleted from the database (older copies in backups stay sealed, and need the keyring to open).
4. **Keep the old record.** Leave the old selector's TXT record in DNS until the date the console shows, which is at least seven days, and at least the [retry policy](/operations/recovery)'s maximum message age plus two days. Mail signed with the old key can still be waiting to be retried, and receivers need the record to verify it.

To abandon a rotation before switching, choose **Cancel** (API: `DELETE .../dkim-keys/{selector}`); the new key is deleted and the current one is untouched. Only a key that has not been switched in can be cancelled, and a domain has at most one pending key. Delete a cancelled key's record from DNS.

See [Key and certificate rotation](/operations/rotation).

## SMTP and DKIM

[SMTP submission](/guide/smtp) uses the same verified domains and the same DKIM keys. Whatever the From policy, the domain a message is sent from must have an active DKIM key with a matching DNS record, or Thinpost answers `451 4.3.5` and the client keeps the message queued until it is fixed. If the policy moves or replaces the sender address, its target domain must also be a **verified** sender domain of the app, with the same `451 4.3.5` if it is not. Under the default policy, a `From` on an unverified domain is refused with `550 5.7.1`. The SMTP tab warns you when the chosen domain would not be signed yet.

## SPF and bounces

You do not add Thinpost to your own domain's SPF record. Every message uses `b+<token>@<bounce domain>` as its envelope sender, so receivers check SPF against the **bounce domain**. By default that is the HELO name of the sending IP (see [Sending IPs](/guide/sending-ips#dns-for-a-sending-ip)). If you want SPF to pass for your own domain too, give the sending domain [its own bounce domain](#your-own-bounce-domain).

## Your own bounce domain

A bounce domain is a name under your sending domain, such as `bounces.example.com`, that Thinpost uses as the return address (the SMTP envelope sender, also called the Return-Path) instead of the sending IP's HELO name. Receivers then check SPF against a name in your own domain, which lines up with the `From` domain for DMARC's usual (relaxed) SPF check. It is optional. Without it, mail is sent exactly as before.

**Set it up** on the Sending domains tab, in the bounce-domain panel under a verified domain (API: `PUT /api/v1/apps/{appId}/domains/{domain}/bounce-domain` with `{"name": "bounces.example.com"}`).

1. **Choose a name.** It must be a subdomain of the sending domain (not the sending domain itself) and not used for anything else. It can't be a name Thinpost uses itself, or a [receiving domain](/guide/inbound) that is active or disabled. Several apps may use the same name. A sending domain has at most one bounce domain, and setting a new name replaces the old one in one step.
2. **Add one CNAME record** at your DNS provider: `bounces.example.com` pointing at the HELO name of the IP the app sends from. The console shows the exact name (it is the name of the app's [sending IP](/guide/sending-ips), or of the default IP when the app has none). The name is shown only once public DNS confirms it, because Thinpost does not trust the sending worker's report on its own.
3. **Make it DNS only.** The record must be **DNS only, not proxied**. If your DNS provider offers a proxy for records (Cloudflare's orange cloud, for example), turn it off: a proxied record answers with the proxy's addresses and no CNAME, so the check reports `not_cname`.
4. **Check it.** Thinpost looks at the record every 10 minutes for the first 48 hours while it waits, then every 6 hours, and you can choose **Check now** (API: `POST .../bounce-domain/check`). When the CNAME points at the expected name, the domain is **In use**.

**What mail does.** The return address is decided for each delivery at the moment it is claimed, from the current state, never from a remembered one. A message from the sending domain (or from one of the app's verified subdomains of it: a subdomain uses the most specific *active* bounce domain, so if its own is pending or failing it falls back to the parent's) uses the custom domain only when **all** of these hold, and otherwise uses the HELO name as before:

- the domain is active and a check passed within the last 3 hours (active domains are rechecked every 30 minutes);
- the CNAME still points at the HELO name of the IP that delivery is sent from, so changing the app's sending IP stops using the domain at once, until the CNAME is updated;
- the name is not a receiving domain, and not the name of any of the sending worker's addresses;
- the sending worker accepts bounces at any domain.

If a record is removed or changed, mail falls back to the standard bounce address on its own and nothing is lost. A domain whose checks keep failing shows **Not in use** with the reason, and is checked again every 10 minutes for a day, then hourly. After you change the app's sending IP the console shows the new name to point at, and the domain is rechecked within about a minute.

**States and reasons.** `pending` (waiting for a first passing check), `active`, `failing`. The last result is one of: `ok`; `missing` (the name doesn't exist); `not_cname` (it exists but isn't a CNAME, which is also what a proxied record looks like); `other_ip` (it points at another of Thinpost's sending names, so the app now sends from a different IP); `wrong_target` (it points elsewhere, including a CNAME that goes through another name first: point it straight at the HELO name); `lookup_failed` and `no_sending_ip` (Thinpost couldn't tell; an active domain is used until two checks in a row fail this way). Changes of state are in the audit trail as `bounce_domain.added`, `activated`, `failing` and `removed`.

**Removing it** (API: `DELETE .../bounce-domain`) sends the next delivery with the standard address. Bounces already on their way to the old name are still received, as long as its DNS still points at Thinpost's sending IP; delete the CNAME when your longest retry window has passed.

**DMARC.** The bounce domain gives **relaxed** SPF alignment only: `bounces.example.com` and `example.com` share an organizational domain. If your DMARC record asks for strict SPF alignment (`aspf=s`), SPF does not align, and DMARC still passes through DKIM, which signs with the exact `From` domain. Feedback-loop reports and other bounces to the custom name arrive and are matched the same way as before; the [feedback mailbox](/guide/suppressions#spam-complaints-and-feedback-loops) address does not change.

## DMARC

Thinpost does not publish or check your DMARC policy. Messages are DKIM-signed with `d=` set to the exact `From` domain, so they align on DKIM even when your DMARC record asks for strict alignment (`adkim=s`). Publish a DMARC record on the sender domain, starting with `p=none` and aggregate reporting, and tighten it as you confirm all your mail authenticates.

By default SPF alignment depends on the bounce domain, which belongs to the sending IP and is not your domain, so mail passes DMARC through DKIM. With [your own bounce domain](#your-own-bounce-domain), SPF also aligns under DMARC's usual relaxed check. If your DMARC record asks for strict SPF (`aspf=s`), DKIM still carries it.

## Next

- [Sending IPs](/guide/sending-ips)
- [Suppressions](/guide/suppressions)
