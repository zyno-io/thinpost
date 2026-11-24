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

Adding, creating and activating keys, verifying a domain and [removing a domain](#remove-a-domain) are audited.

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
Verification shows that the resolver Thinpost uses returned the expected record. Thinpost does not validate DNSSEC. A verified domain does not mean SPF, DMARC or reverse DNS are configured; [check them](#check-spf-and-dmarc) separately.
:::

Receiving-domain verification for [inbound mail](/guide/inbound) is separate and uses its own `_thinpost` TXT name. Sender domains no longer use that name.

## DKIM signing

Thinpost signs every message with the domain's active key, using the sender domain as the `d=` value so DKIM aligns with the `From` domain for DMARC, even under strict alignment (`adkim=s`). The selector is `thinpost1` for a domain's first key, then `thinpost2`, and so on. Selectors are unique for a domain across all apps that use it, and a retired selector is never reused, also after the domain is removed and added again.

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

You do not add Thinpost to your own domain's SPF record. Every message uses `b+<token>@<bounce domain>` as its envelope sender, so receivers check SPF against the **bounce domain**. By default that is the HELO name of the sending IP (see [Sending IPs](/guide/sending-ips#dns-for-a-sending-ip)). If you want SPF to pass for your own domain too, give the sending domain [its own bounce domain](#your-own-bounce-domain). To see whether SPF passes for the IP your app sends from, use the [SPF and DMARC check](#check-spf-and-dmarc).

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

## Check SPF and DMARC

The **SPF and DMARC** panel under each verified domain that has a key checks when the Sending domains tab opens, and again when you choose **Check again** (API: `POST /api/v1/apps/{appId}/domains/{domain}/check-dns`) to see what receivers find. The control plane reads public DNS within 8 seconds. The check is advisory: nothing is stored, logged or audited, and it never affects verifying, signing or sending. Domains that are not verified can be checked through the API too.

**DMARC.** The panel shows the record found at `_dmarc.<domain>`, or, if the domain has none, at its organizational domain (by the public suffix list; that record's `sp=` applies if it has one). Problem tags are underlined in red and a missing tag is shown as a dashed `+` insertion. When something needs to change, an **Update:** line gives the corrected full record, and **Add:** gives the TXT record to create when there is none. Each has a Copy button. The status is one of:

- `ok`: `p=quarantine` or `p=reject` applies (shown as Enforced).
- `monitoring`: `p=none` applies. This is a fine place to start, and the console doesn't nag about it.
- `missing`: neither the domain nor its organizational domain has a record. The panel offers `v=DMARC1; p=none`.
- `multiple`: more than one `v=DMARC1` record at one name. Receivers ignore them all, so the panel lists them (up to 5, in `records`) and asks you to keep one. Thinpost doesn't look past them.
- `invalid`: no valid `p=`, or an invalid `sp=`, and no `rua=`. Receivers apply no DMARC to such a record. With `rua=` it counts as `p=none` (RFC 7489 6.6.3), and an invalid `aspf` is read as relaxed.
- `unavailable`: the lookup failed. Try again.

The corrected record keeps every tag that isn't wrong, as written: an invalid `p` becomes `p=none`, an invalid `sp`, `aspf`, `adkim` or `pct` and malformed tags are dropped, a missing `p=none` is added, and the rest is untouched. Dropping an invalid `sp` can make the record stricter than receivers treat it today (they read it as `p=none`, or apply no DMARC), so the panel then adds a warning such as "Applying this turns on p=reject." A record longer than 1024 bytes is shown cut, ending in `…`, and the panel offers no fix for it. Other TXT records at `_dmarc` (verification tokens, for example) are ignored, and a record is only a DMARC record when it starts with `v=DMARC1`.

**SPF.** Thinpost evaluates the app's sending IP exactly as a receiver would (RFC 7208, including `include:`, `redirect=` and the 10-lookup limit). The name checked is the envelope domain the domain's mail uses now: the [bounce domain](#your-own-bounce-domain) while it is active, was checked within 3 hours and last pointed at the sending IP's HELO name, otherwise the HELO name of the sending IP (marked Thinpost). The panel shows the result (Passes, Fails or Couldn't check), and for a pass whether it is Aligned with the domain under the DMARC record's `aspf` (relaxed when there is no usable record). A pass that is not aligned is fine, because DKIM aligns; a bounce domain aligns SPF too. If the sending worker hasn't reported an IP yet, the result is `no_sending_ip`.

Thinpost never needs an SPF `include:` on your From domain: the domain's own SPF record is not checked, and no Thinpost mail is checked against it. The panel never asks you to add or change one. If SPF fails, the fix is in the DNS records of the sending IP's HELO name, which your Thinpost administrator manages (see [DNS for a sending IP](/guide/sending-ips#dns-for-a-sending-ip)).

## DMARC

Thinpost does not publish your DMARC record, and nothing it does depends on it; the [check](#check-spf-and-dmarc) reports what receivers find. Messages are DKIM-signed with `d=` set to the exact `From` domain, so they align on DKIM even when your DMARC record asks for strict alignment (`adkim=s`). Publish a DMARC record on the sender domain, starting with `p=none` and aggregate reporting, and tighten it as you confirm all your mail authenticates.

By default SPF alignment depends on the bounce domain, which belongs to the sending IP and is not your domain, so mail passes DMARC through DKIM. With [your own bounce domain](#your-own-bounce-domain), SPF also aligns under DMARC's usual relaxed check. If your DMARC record asks for strict SPF (`aspf=s`), DKIM still carries it.

## Remove a domain

Remove a domain when an app should stop sending from it. In the console, open the app's **Sending domains** and choose **Remove domain** from the row's **More actions** menu. For a verified domain you type its name to confirm. Through the API this is `DELETE /api/v1/apps/{appId}/domains/{domain}`.

Removal is per app. Another app that uses the same domain is not affected, and its key keeps signing.

### What happens

- **New mail is refused at once.** The HTTP API answers `422 message_rejected`, and SMTP answers `550 5.7.1` under the default From policy. If the app's SMTP From policy leaves a verified `From` alone but rewrites an unverified one, mail from the removed domain is now rewritten like any other unverified address. See [SMTP](/guide/smtp#the-from-address).
- **Mail that is not prepared yet fails.** A message that is still queued, being prepared, or held and released later cannot be signed. It ends as `preparation_failed` with the reason `signing_key` and sends the usual `preparation_failed` webhook, without going through the 12 retries first. After you [add the domain again](#add-a-removed-domain-again), [retry its preparation](/operations/recovery).
- **Mail that is already prepared is still delivered.** It is signed and queued already, so it keeps being sent and retried. Its Return-Path falls back to the sending IP's HELO name from its next attempt, because the bounce domain is gone. Tracking links in it keep working while the tracking domain's DNS and certificate stay in place.
- **The active DKIM key is retired.** Its private key is deleted from the database, as in a [rotation switch](#rotate-a-key). Its record must stay in DNS until the keep date, because of the prepared mail above.
- **A pending DKIM key is deleted**, as **Cancel** deletes it. It never signed, so its record can go at once.
- **The bounce domain is deleted.** Subdomains that used it now use their own, then the next one up, then the HELO name.
- **The tracking domain becomes a removed tombstone**, as when you [remove it](/guide/tracking#your-own-tracking-domain) yourself. It is still checked once a day and listed for operators.

Messages, recipients, events, suppressions and the audit trail are kept. A paused app can remove a domain.

If the app's [SMTP From policy](/guide/smtp#the-from-address) sends as the domain, removal is refused with `409` and the reason `smtp_from_policy`. Change the policy on the SMTP tab first.

### Clean up DNS

The response lists what you can delete, and the console keeps the same list on screen after the removal.

| Record | When to delete it |
| --- | --- |
| `thinpostN._domainkey.<domain>` of the retired key | After the keep date. It is at least seven days, and at least the [retry policy](/operations/recovery)'s maximum message age plus two days. |
| `thinpostN._domainkey.<domain>` of a pending key | Now. It never signed. |
| The bounce domain's CNAME | After the keep date, because mail prepared before the removal still uses the HELO name, and bounces to the old name are still accepted until then. |
| The tracking domain's DNS, route and certificate | Keep them while old links matter, as for any removed tracking domain. |
| Your own SPF and DMARC records | They are yours, and Thinpost does not publish them. Keep a DMARC record while subdomains still send, because they inherit it. |

Another app may use the same bounce or tracking name, or the same SPF and DMARC records. Keep any record another app still uses.

If the app is paused for a long time after the removal, a delivery that was never attempted can go out after the keep date. The same is true after a rotation.

### Add a removed domain again

Adding the domain again brings it back as a new, unverified domain with a new key and a new record. A selector that was retired is never reused, so the new selector is not one that signed before. A pending key that was deleted can have its selector given out again, as after **Cancel**. While an old key is still inside its keep window, the console shows it as "Old record thinpostN can stay until ...". The bounce and tracking domains are not restored.

### API

`DELETE /api/v1/apps/{appId}/domains/{domain}` takes no query string or body. It answers:

| Status | Meaning |
| --- | --- |
| `200` | The domain is removed. The body has `name`, `removed_at`, `dkim_records` (each with `selector`, `dns_name` and `keep_until`, which is absent for a key that never signed), and, when they were removed, `bounce_domain`, `tracking_domain` and `keep_records_until`. |
| `400` | The domain name is not valid, or the request has a query string or a body. |
| `404` | The app, or the domain, is unknown, or the domain is already removed. Repeating a removal is a `404`. |
| `409` | `{"error":"Conflict","reason":"smtp_from_policy"}`: the app's SMTP From policy sends as this domain. |
| `503` | The control plane is not accepting changes right now, or the store failed. Try again. |

## Next

- [Sending IPs](/guide/sending-ips)
- [Suppressions](/guide/suppressions)
