---
title: Sender domains
description: Verify a sender domain, publish DKIM keys, and understand what Thinpost does and does not check in DNS.
---

# Sender domains

An app can only send from domains an administrator has verified for it. Verification proves the app's owner controls the name. Making the mail authenticate, through DKIM, SPF and DMARC, is separate DNS work that you do alongside it.

## Verify a domain

1. **Request the domain.** In the [console](/guide/console) open the app, choose **Sending domains** and add the domain. Through the API this is `POST /api/v1/apps/{appId}/domains`.
2. **Publish the challenge.** Thinpost returns a TXT record name and value. Publish it at your authoritative DNS provider.

   | Record | Name | Value |
   | --- | --- | --- |
   | TXT | `_thinpost.mail.example.com` | `thinpost-verification=...` (shown once) |

   The value is returned only when it is issued, because Thinpost stores only its hash. Requesting a new challenge for a pending domain replaces the old value.
3. **Verify.** Choose **Verify** (or `POST .../domains/{domain}/verify`). Thinpost looks the record up through the control plane's DNS resolver and activates the domain when it sees the matching value.

Challenge creation and successful verification are audited.

::: warning What verification does not prove
Verification shows that the resolver Thinpost uses returned the expected value. Thinpost does not validate DNSSEC. An active domain does not mean DKIM, SPF, DMARC or reverse DNS are configured, and the console says so. Domain removal is not available yet.
:::

The same TXT name is shared with receiving-domain verification for [inbound mail](/guide/inbound). Thinpost accepts any matching value among the TXT records at that name, so one domain can carry both challenges.

## DKIM signing

Thinpost signs every message with a DKIM key you provide, using the sender domain as the `d=` value so DKIM aligns with the `From` domain for DMARC.

### Provide a key

Keys are RSA, at least 2048 and at most 8192 bits, in unencrypted PKCS#1 or PKCS#8 PEM form. The upper bound keeps signed messages small. Create one per sender domain and selector:

```sh
openssl genrsa -out mail1.pem 2048
openssl rsa -in mail1.pem -pubout -outform DER | openssl base64 -A
```

The second command prints the base64 public key for the DNS record.

The control plane reads a **signing catalog**, a JSON file that maps each app and verified sender domain to a selector and the path of its mounted private key:

```json
[
  {
    "app_id": "0123456789abcdef0123456789abcdef",
    "domain": "mail.example.com",
    "selector": "mail1",
    "key_path": "/run/secrets/thinpost/mail1.pem"
  }
]
```

Mount the catalog and the key files from your secret store. The path of the catalog is `THINPOST_CONTROL_SIGNING_CATALOG`; in Kubernetes the Helm chart wires it for you ([Helm: control](/deploy/helm-control)).

### Publish the public key

Publish a TXT record at `<selector>._domainkey.<domain>`:

| Record | Name | Value |
| --- | --- | --- |
| TXT | `mail1._domainkey.mail.example.com` | `v=DKIM1; k=rsa; p=<base64 public key>` |

Thinpost rechecks the DNS record before it uses a key: the selector must have a single TXT record that contains the matching public key and permits SHA-256 signing. It also rechecks that the app and domain are still active. Signing resolves public keys through DNS without DNSSEC validation.

### Rotation

The catalog and keys load at process startup, so changing them requires restarting the control replicas. Thinpost is not a key manager, so:

1. Create a new key with a new selector and publish its public record.
2. Update the catalog to use the new selector and restart.
3. Keep the old selector's public record published until every message signed with it has aged out of retries and recipients' queues.

See [Key and certificate rotation](/operations/rotation).

## SPF and bounces

You do not add Thinpost to your own domain's SPF record. Every message uses `b+<token>@<bounce domain>` as its envelope sender, so receivers check SPF against the **bounce domain**, which belongs to the sending IP. See [Sending IPs](/guide/sending-ips#dns-for-a-sending-ip).

## DMARC

Thinpost does not publish or check your DMARC policy. A DKIM-signed message from a verified domain aligns on DKIM. Publish a DMARC record on the sender domain, starting with `p=none` and aggregate reporting, and tighten it as you confirm all your mail authenticates.

## Next

- [Sending IPs](/guide/sending-ips)
- [Suppressions](/guide/suppressions)
