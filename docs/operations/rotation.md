---
title: Key and certificate rotation
description: Rotate Thinpost DKIM keys, the bounce, idempotency, tracking and object keyrings, and the control mTLS credentials without breaking in-flight mail.
---

# Key and certificate rotation

Thinpost reads most keys and certificates at process start, and every control replica needs the same key material. The pattern is the same for every secret that protects existing data: **add the new key first, roll everything, switch the active key, roll again, and keep old keys for as long as anything depends on them.** Deleting an old key breaks whatever it protected.

Thinpost is not a key manager for the secrets you provide. Create and store those in your secret system, mount them as files, and keep them out of source control, logs and documentation. DKIM keys are the exception: Thinpost generates and holds them itself.

| Secret | Loaded | Rotation | Keep old keys while |
| --- | --- | --- | --- |
| [DKIM signing key](#dkim-keys) | Database | Create a new key, publish it, switch | Its DNS record stays until mail signed with it can no longer be retried (the console shows the date) |
| [Bounce keyring](#bounce-keyring) | Startup | Add version, switch active | A delivery bearing it can still retry or bounce |
| [Idempotency fingerprint key](#idempotency-fingerprint-key) | Startup | Prepend to the key list | The 24-hour idempotency window |
| [Tracking link keys](#tracking-link-keys) | Startup | Two-phase seeds | Indefinitely, for links already sent |
| [Object keyring](#object-keyring) | Startup | Add version, switch active | Any object, sealed secret or backup that uses it, including the SMTP retry secret |
| [SMTP credentials](#smtp-credentials-and-an-edge-compromise) | Database | Create a new credential, revoke the old one | Not applicable |
| [Control mTLS](#control-mtls-certificates) | Edge: each handshake. Control client: startup | Renew leaves, restart control | Not applicable |
| [Control HTTPS listener](#control-https-listener-and-database-ca) | Each handshake | Renew the Secret | Not applicable |

## DKIM keys

Thinpost generates each sender domain's DKIM key, seals it with the [object keyring](#object-keyring) and keeps it in the database, so rotating one needs no files, restart or deploy. It is a manual change, in the console or the API, that you control:

1. **Create the next key.** In the app's **Sending domains**, choose **Rotate key** (`POST /api/v1/apps/{appId}/domains/{domain}/dkim-keys`). Thinpost makes a key with the next selector (`thinpost2`, and so on) and shows its record. The current key keeps signing, and only one new key can be waiting at a time.
2. **Publish the record** at `<new-selector>._domainkey.<domain>` and choose **Check record**. Thinpost looks it up and marks the key ready to switch. Thinpost rechecks DNS before it signs and never signs with a key whose record is missing or does not match.
3. **Switch** when you are ready (`POST .../dkim-keys/{selector}/activate`). Thinpost checks DNS once more and, in one transaction, makes the new key the signing key and retires the old one. **The old private key is deleted from the database** (copies in backups stay sealed and need the keyring to open) and its selector is never used again. Messages signed with the old key before the switch still verify while its record stays in DNS.
4. **Keep the old record in DNS** until the date the console shows (`keep_record_until`): at least 7 days, and at least the delivery [retry policy](/operations/recovery)'s maximum message age plus 2 days. Messages signed with the old key may still be in the retry queue.

To back out before switching, **Cancel** deletes the pending key (`DELETE .../dkim-keys/{selector}`). A pending key can also simply be left in place.

The private keys are sealed with the object keyring, so keep every keyring version that sealed a key you still use: a version that is missing makes that key unopenable and mail from the domain stops signing. When you [rotate the object keyring](#object-keyring), existing DKIM keys stay sealed under the version that was active when they were created; only a new key uses the new active version, so keep old versions until those keys are rotated out.

[Removing a sender domain](/guide/domains#remove-a-domain) retires its active key the same way as a switch: the private key is deleted, the selector is never used again, and its record stays in DNS until the same keep date.

See [Sender domains](/guide/domains#rotate-a-key) for the DNS records and the checks.

## Bounce keyring

Each delivery gets an opaque return path, `b+<token>@<bounce domain>`, derived with a key from the bounce keyring. Bounces that arrive later are matched by trying the retained versions.

The keyring is a JSON object mapping a version ID to a base64-encoded secret of at least 32 bytes. Version IDs are at most 64 characters and the file holds at most 32 versions. The active version is `THINPOST_CONTROL_BOUNCE_ACTIVE_KEY`.

```json
{
  "v1": "BASE64_OF_32_OR_MORE_RANDOM_BYTES",
  "v2": "BASE64_OF_32_OR_MORE_RANDOM_BYTES"
}
```

The placeholders above are not usable keys.

1. Add the new version to the file and keep the active version unchanged. Roll the control replicas.
2. Set `THINPOST_CONTROL_BOUNCE_ACTIVE_KEY` to the new version and roll again. New return paths use the new version. Old ones keep matching.
3. Remove an old version only when no delivery that carries it can still retry or bounce. Removing a version makes bounces to its addresses unmatchable.

The duplicate-detection keys for inbound mail are derived from the same keyring, one per version. Rotating the active version starts a new duplicate window for retries that straddle the rotation. Older versions are still consulted.

## Idempotency fingerprint key

The key that fingerprints submission requests lets Thinpost recognize an identical retry that carries the same `Idempotency-Key`. Losing it, or removing it too early, makes a retry look like a new request and can produce a duplicate send.

Set either `THINPOST_CONTROL_FINGERPRINT_KEY_FILE` (one key) or `THINPOST_CONTROL_FINGERPRINT_KEYS_FILE` (a list); setting both fails startup. The list file is a JSON array of one to eight standard-base64 keys, each at least 32 bytes. The first key creates new fingerprints and every key verifies existing ones.

```json
[
  "BASE64_OF_NEW_KEY",
  "BASE64_OF_PREVIOUS_KEY"
]
```

1. Switch from the single-key file to the list file, with the current key first.
2. To rotate, put the new key first and keep the previous key in the list. Roll the control replicas.
3. Drop a key after the idempotency window has passed. The window is 24 hours for Resend `Idempotency-Key`. Expired markers are cleaned up periodically.

Operator-only replicas do not mount this key.

## Tracking link keys

Tracking and unsubscribe links are encrypted under seeds from `THINPOST_TRACKING_LINK_KEYS_FILE`. A link keeps working as long as its seed is in the file, so **retired seeds are kept indefinitely**. Removing a seed invalidates every link issued under it, which is also the way to respond to a compromise.

Rotate in two phases so a rolling deploy never meets a link it cannot read:

1. Add the new seed to `previous_seeds` and leave `seed` unchanged. Roll out. Every replica can now decrypt links under the new seed.
2. Set `seed` to the new seed, move the old seed into `previous_seeds`, and remove the new seed from `previous_seeds` (duplicates are rejected). Roll out. New links use the new seed and links under every retained seed keep working.

The file format, key derivation and constraints are in [Tracking](/guide/tracking#key-rotation). Keep the original origin, DNS, certificate and route operational for URLs that are already out in the world.

## Object keyring

The object keyring encrypts stored messages and seals small secrets such as webhook signing secrets and DKIM private keys. It is set with `THINPOST_OBJECT_KEYRING_FILE`. The file is a JSON object with an `active` version and a `keys` map. Each value is the standard base64 encoding of a distinct random 32-byte key. IDs are ASCII letters, digits, hyphen and underscore, at most 64 characters, with at most 32 versions.

```json
{
  "active": "version_2",
  "keys": {
    "version_1": "BASE64_OF_32_RANDOM_BYTES",
    "version_2": "BASE64_OF_32_RANDOM_BYTES"
  }
}
```

1. Add the new key while keeping the old version active. Roll all control replicas and verify that each has the expanded keyring.
2. Switch `active` to the new version and roll the replicas again.
3. Keep every old version for as long as any stored object, sealed secret or backup was written with it. Changing or deleting an old key makes that data unreadable. An unavailable or invalid key fails the read; there is no plaintext fallback.

Rotation does not re-encrypt existing objects. Back up the keyring together with the bucket. After a restore, the keyring that the database and objects reference must be restored with them, and you can authenticate all of it with the [object inventory](/operations/recovery#the-object-inventory).

The SMTP retry-protection secret is sealed with this keyring too. Thinpost generates it once, the first time it handles an SMTP submission that carries a `Message-ID` or `Date`, seals it under the keyring version that is active at that moment, and **never re-seals it**. Treat it like a webhook signing secret: keep that keyring version for as long as you run SMTP submission, even after you switch `active`. If the version is missing, a control replica cannot open the secret and answers SMTP submissions with a temporary failure, instead of inventing a new secret that would make retries look like new messages. A maintenance command to re-seal it under a newer version is not available yet.

Rotating one webhook signing secret is separate: do it from the app's Webhooks tab or the API, and the new secret is sealed with the active key.

## SMTP credentials and an edge compromise

Clients send an SMTP credential's password through the edge, so a compromised edge may have seen every credential used while it was compromised (see the [threat model](/security/threat-model#smtp-submission-credentials)). It cannot see credentials that were not used.

To rotate one credential, create a new one on the app's SMTP tab, switch the device or software to it, then revoke the old one. Revoking takes effect for new sign-ins and for any message the credential submits afterwards, including from connections that are already open.

If you suspect the edge was compromised:

1. Rebuild or replace the edge, and replace its control certificates as for any edge compromise ([Control PKI](/deploy/control-pki)).
2. The edge holds the private keys of the public SMTP certificates, so **reissue every public SMTP certificate with a new key** (the primary's and each additional address's Secret), revoking the old certificates at your CA where possible, and rotate the internal listener's certificate if you use it. Do this before reissuing credentials, or a still-trusted stolen key can capture the new passwords.
3. For every app with SMTP credentials, **revoke every credential and create new ones**. An app may have at most 10 unrevoked credentials: if it has 5 or fewer you can create the replacements first, otherwise revoke before creating. Each credential must be replaced on the device that uses it.
4. Review recent SMTP messages in the console for mail you do not recognize.
5. Check that each sending IP's HELO name in the edge configuration is one you chose. The SMTP tab and inbound MX host only show names that public DNS confirms both ways, but confirm the names your devices and MX records use before handing out new credentials.
6. Rotate the feedback mailbox (Operations, Feedback loops), since its address was handed to mailbox providers while the edge may have been compromised.

The SMTP retry secret and the credential digests never reached the edge, so they do not need rotating.

## Control mTLS certificates

The control plane dials the Edge over mutual TLS, with a private CA and distinct identities for each side expressed as URI names. See [Control PKI](/deploy/control-pki).

**Leaf certificates.** Renew by replacing the mounted Secret with a new leaf for the same identity.

- The Edge loads its control and SMTP certificates on startup and on every new handshake, so renewed certificates apply without a restart. It validates the pair first and rejects a mismatched, partially written or untrusted update, so a bad update fails the new handshake instead of serving stale or unchecked material. Existing connections keep their certificate. TLS session resumption is disabled, so a change cannot be bypassed.
- The control and SMTP keys must be different. The Edge rejects a pair that reuses a key.
- The control process reads its client certificate and key once at startup. Restart control replicas after renewal, for example with a reloader that watches the Secret. Without a restart the old certificate keeps working only until it expires.
- The control plane signs controller leases with its client key, and the Edge verifies them against the authenticated peer certificate, so a renewed leaf must carry the same URI identity.
- Each side ends its session before the verified peer chain expires, with a clock-skew margin. After a gap, the next connection must pass TLS and identity verification again. A leaf that expires before replacement stops delivery until it is replaced and control reconnects.

**Private CA.** The trust roots and authorization policy are fixed for the life of each process. Rotating the CA is a coordinated restart:

1. Publish a bundle containing both the old and new CA to the Edge and the control plane, and restart both.
2. Issue new leaves from the new CA and roll them as above.
3. Remove the old CA from the bundles and restart both again.

Monitor certificate expiry on both sides and on the SMTP certificates.

## Control HTTPS listener and database CA

- When the optional in-cluster HTTPS listener is enabled, each handshake checks the certificate and key files and reloads them on change. An unreadable or half-written replacement is ignored with one log line and the previous certificate keeps serving. A renewed cert-manager Secret applies without a restart.
- The database driver re-reads `sslrootcert` for every new connection and connections are recycled every 30 minutes, so a rotated CA bundle is picked up without a restart. Database credentials supplied as files are read at startup, so roll the process after changing them.

## Next

- [Recovery and the admission gate](/operations/recovery)
- [Control PKI](/deploy/control-pki)
