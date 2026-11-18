---
title: Object storage
description: Provision the S3-compatible bucket for Thinpost, grant the control credential, generate the object keyring, set the lifecycle rule and learn lock semantics.
---

# Object storage

The control plane stores original submissions, signed per-recipient MIME, received bounce bodies, encrypted tracking mappings and inbound messages in one S3-compatible bucket. It encrypts every object before upload. The edge worker never sees the bucket: it receives only the bytes of the delivery it is asked to send.

## What the bucket needs

| Requirement | Why |
| --- | --- |
| A private bucket, reachable over TLS | The client always uses TLS. `THINPOST_ALLOW_INSECURE_LOCAL_S3=1` permits plain HTTP only for a loopback endpoint with a loopback database, for disposable local development. |
| Conditional create | Uploads send `If-None-Match: *`, so an existing key is never overwritten. |
| Strong read-after-write and read-after-delete consistency for `HEAD` | Commits verify object presence with `HEAD`; see [HEAD and lock semantics](#head-and-lock-semantics). Test your store against this requirement. |
| Versioning optional | A lifecycle rule handles versioned buckets; see below. |

The object names are `submissions/`, `final/`, `bounces/`, `tracking/` and `inbound/`, followed by `v1/<unix seconds>/<32 hex characters>`. The names carry no addresses, domains or app identifiers.

## Credentials and permissions

The control Deployment reads the credential from the Secret `thinpost-object-storage`, with keys `access-key` and `secret-key`:

```sh
kubectl -n thinpost-control create secret generic thinpost-object-storage \
  --from-literal=access-key='<access key>' \
  --from-literal=secret-key='<secret key>'
```

The chart maps these to `THINPOST_S3_ACCESS_KEY` and `THINPOST_S3_SECRET_KEY`. Grant the credential access to **this bucket only** with these operations:

| Operation | Used for |
| --- | --- |
| `PutObject` | Creating objects. |
| `GetObject` | Reading content, preparing messages, webhook rendering, tracking. |
| `HeadObject` | Presence checks before commit. On Amazon S3, [`HEAD` requires `s3:GetObject`](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html). |

**Do not grant delete.** Control replicas never delete an object. Retention is the bucket's job. A deletion-capable identity belongs only to a separately fenced collector that you run yourself, and it must not receive the keyring.

## Control settings

Put these in the `thinpost-operator-config` ConfigMap (see [Helm: control](/deploy/helm-control)):

```yaml
THINPOST_S3_ENDPOINT: s3.example.com
THINPOST_S3_BUCKET: thinpost-messages
THINPOST_S3_REGION: us-east-1
THINPOST_OBJECT_KEYRING_FILE: /run/thinpost/object-keys/keyring.json
```

- `THINPOST_S3_ENDPOINT` is `host` or `host:port`, with no scheme, path, query or credentials.
- `THINPOST_S3_REGION` is required: 1 to 64 characters of lowercase letters, digits and hyphens. Set it to the bucket's signing region. The client uses it directly and needs no bucket-location permission.
- `THINPOST_OBJECT_KEYRING_FILE` is an absolute path inside the mounted keyring Secret.

If any S3 variable is set, all of them must be. Control starts without S3 only when none is set, which leaves message handling off. Submission, preparation and the edge bridge each refuse to start without it.

## The keyring

Every object is encrypted under a key from a keyring you provide. The file is a JSON object with an `active` version and a `keys` map from version to a standard-base64 encoding of a **distinct 32-byte random key**:

```json
{
  "active": "key-1",
  "keys": {
    "key-1": "<base64 of 32 random bytes>"
  }
}
```

Generate a key and build the Secret:

```sh
KEY=$(openssl rand -base64 32)
printf '{"active":"v1","keys":{"v1":"%s"}}' "$KEY" > keyring.json
kubectl -n thinpost-control create secret generic thinpost-object-keyring \
  --from-file=keyring.json=keyring.json
shred -u keyring.json 2>/dev/null || rm -f keyring.json
```

Rules:

- Version IDs use ASCII letters, digits, hyphen and underscore, at most 64 characters, and appear unencrypted in each object header. Do not put anything sensitive in them.
- At most 32 versions, and the file may not exceed 16 KiB. The active version must be present. Duplicate versions and duplicate key material are rejected.
- Use independent key material for each environment.
- The process loads the file at startup and fails on a missing, malformed or incomplete one. There is no plaintext fallback.
- Every control replica must be able to decrypt every object it may read.

Objects use a versioned envelope with a random salt. HKDF-SHA-256 derives a per-write AES-256-GCM key, and both the version header and the exact object key are authenticated. A modified object, or one copied to a different key, fails to decrypt.

The keyring also seals small database secrets, such as webhook signing secrets. **Never remove a key version while any object or sealed value might still use it.** Keep the keyring in a backup you can restore with the bucket.

To rotate, add a new version while keeping the old one active, roll the replicas and confirm each has the larger keyring. Then switch `active` to the new version and roll again. Keep every old version. See [Rotation](/operations/rotation).

## Inbound retention: the lifecycle rule

Inbound messages are stored byte for byte under `inbound/`, one object per message. Each message has a content expiry 30 days after receipt. Thinpost enforces it from the database clock: after expiry the console and the webhook renderer refuse to read the content, and an object that the bucket has already removed counts as expired content, not a storage failure.

Because the control credential cannot delete, **you must create a lifecycle rule** that removes the objects. Without it, expired inbound content stays in the bucket. The rule below deletes current versions after 31 days (one day past the application's expiry) and cleans up versions, delete markers and incomplete uploads after one day:

```json
{
  "Rules": [
    {
      "ID": "thinpost-inbound-expiry",
      "Status": "Enabled",
      "Filter": { "Prefix": "inbound/" },
      "Expiration": { "Days": 31 },
      "NoncurrentVersionExpiration": { "NoncurrentDays": 1 },
      "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 1 }
    },
    {
      "ID": "thinpost-inbound-delete-markers",
      "Status": "Enabled",
      "Filter": { "Prefix": "inbound/" },
      "Expiration": { "ExpiredObjectDeleteMarker": true }
    }
  ]
}
```

Apply it with any S3 client, for example:

```sh
aws s3api put-bucket-lifecycle-configuration \
  --endpoint-url https://s3.example.com \
  --bucket thinpost-messages \
  --lifecycle-configuration file://lifecycle.json
```

The two rules are separate because S3 does not allow an expired delete-marker action in the same element as a day count. Your store's syntax may differ slightly. The `inbound/` prefix is the only one with a rule: Thinpost does not yet delete submissions, final MIME, bounce bodies or tracking objects, so size the bucket for them.

::: warning Inbound is not swept by a collector
Do not point a collector at `inbound/`. Its reference check counts every inbound row, expired or not, so it would never remove expired content, and the control credential cannot delete. The lifecycle rule is the mechanism.
:::

## HEAD and lock semantics

Before committing a new submission, a final MIME object or a bounce reference, the control plane takes a transaction-scoped PostgreSQL advisory lock on each object and checks that it exists with an S3 `HEAD`. Locks are taken in a consistent order. A missing object or a failed `HEAD` aborts the transaction.

This relies on your store giving a correct answer to `HEAD` immediately after a write and immediately after a delete. A store with eventual consistency can commit a reference to an object that is not durable yet, or accept a reference to one that has just been removed. A `HEAD` proves existence, not the integrity of the ciphertext.

New object keys include their issue time. After the presence check under the lock, a writer compares the issue time with PostgreSQL's clock and refuses to create a reference more than an hour later. It also refuses keys at or behind the durable collection horizon, which only a maintenance operation can advance and which never moves backward. Together these stop a delayed delete from racing a new reference. Keep the clocks of your database and nodes synchronized.

Do not enable object deletion, other than the lifecycle rule above, until you have tested coordinated database and bucket backup and restore. A restored database cannot detect its own rollback; see [Recovery](/operations/recovery).

## Capacity

An attachment is stored once in the encrypted source object and again inside each recipient's signed MIME object. Attachment bytes are not deduplicated across recipients, so storage grows with attachment size times recipient count.

## Next

- [Operator authentication](/deploy/operator-auth)
- [Helm: control](/deploy/helm-control)
- [Rotation](/operations/rotation)
