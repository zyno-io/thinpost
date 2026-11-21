---
title: Environment variables
description: Every THINPOST_* environment variable read by the control plane, the Edge worker, the control bridge and the maintenance commands.
---

# Environment variables

Thinpost is configured through `THINPOST_*` environment variables. This page lists every variable the binaries read, grouped by the process that reads it. In a Helm deployment the chart sets most of them from [chart values](/reference/helm-values), mounted Secrets and ConfigMaps; see [Deploy](/deploy/) for the resources it expects.

Conventions used in the tables:

- **Required** means the process refuses to start (or the command fails) when the value is missing or invalid. "Conditional" means the variable is required only when the feature or group it belongs to is enabled; the description says when.
- **Path** values name a file inside the container. Where a path must be absolute, the description says so.
- **Duration** values use Go syntax such as `500ms`, `5s` or `2m`.
- **List** values are comma-separated unless stated otherwise.
- Startup errors are deliberately generic. They never include secret values, addresses or file contents.

## Control plane

The `thinpost-control` binary with no subcommand runs the control plane: the operator API, the optional console, the submission and app APIs, webhook dispatch and (when configured) the [control bridge](#control-bridge) to the Edge. Subcommands are listed in the [command-line reference](/reference/cli) and read the groups noted below.

### Process and listeners

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_LISTEN` | `host:port` | `127.0.0.1:8080` | Optional | Address of the plain HTTP listener. |
| `THINPOST_DEPLOYMENT_GENERATION` | 32 lowercase hex characters | none | Required | Identifies this deployment generation (a 128-bit value). It must change when a database is restored into a new, independent deployment. The control plane and the `deploy-migrate` and `deploy-reopen` commands read it. |
| `THINPOST_CONTROL_TLS_LISTEN` | `host:port` | `:8443` when TLS is configured | Optional | Address of an additional HTTPS listener beside the plain one. Must differ from `THINPOST_LISTEN`. Setting any of the three `THINPOST_CONTROL_TLS_*` variables enables the HTTPS listener. |
| `THINPOST_CONTROL_TLS_CERT` | path | none | Conditional | Absolute path of the certificate for the HTTPS listener. Required when the HTTPS listener is enabled. The certificate and key are re-read when they change, without a restart. |
| `THINPOST_CONTROL_TLS_KEY` | path | none | Conditional | Absolute path of the private key for the HTTPS listener. Required when the HTTPS listener is enabled. |
| `THINPOST_UI_DIR` | path | none (console not served) | Optional | Absolute path of the built operator console. When set, the console is served on the same origin as the operator API. |
| `THINPOST_CONSOLE_CONNECT_ORIGINS` | list of origins | none | Optional | Extra `https://host[:port]` origins the console may fetch, added to its Content-Security-Policy `connect-src`. Use it when the identity provider's token endpoint is on a different origin than the issuer. The issuer's own origin is always allowed. Each entry must be a bare HTTPS origin with no path, query, wildcard or trailing slash. |
| `THINPOST_ADMIN_ORIGIN` | HTTPS origin | none | Optional | The only hostname that serves the operator console and operator API (`/api/v1/`), for example `https://thinpost-admin.example.com`. On any other hostname those routes, including the site root, return an empty 404; tracking links, health endpoints and the sending APIs answer on every hostname. Matching uses the request's hostname (from the `Host` header or an absolute request URI) and ignores the port. Must be a bare `https://host[:port]` origin with no path, query or trailing slash; give an international name in punycode. When unset, the console answers on every hostname. |
| `THINPOST_MAX_ATTACHMENT_BYTES` | integer (bytes) | `5242880` (5 MiB) | Optional | Limit on the total decoded attachment bytes of one submitted message. Must be a plain integer from `65536` to `5767168` (5.5 MiB). |

### Database

The control plane and every maintenance subcommand (`migrate`, `recovery`, `retry-policy`, `deploy-migrate`, `deploy-reopen`, `gate`, `grant-*`, `retire-tracking` and `bootstrap-database`) read these. Provide either `THINPOST_DATABASE_URL`, or `THINPOST_DATABASE_BASE_URL` with the two credential files.

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_DATABASE_URL` | PostgreSQL URL | none | Conditional | Complete connection URL, with credentials, for a `postgres://` or `postgresql://` database. Required unless the base URL form is used. Only these query parameters are accepted, each at most once: `sslmode`, `sslrootcert`, `default_query_exec_mode`, `connect_timeout`, `application_name` and `search_path`. `sslmode` must be `verify-full` (see `THINPOST_ALLOW_INSECURE_LOCAL_DB`). `default_query_exec_mode` may be `cache_statement`, `cache_describe`, `describe_exec` or `exec`; the simple protocol is refused because it inlines values into SQL text. `search_path` must be a single schema name of lowercase letters, digits and underscores, starting with a letter or underscore. The URL must name a database. |
| `THINPOST_DATABASE_BASE_URL` | PostgreSQL URL | none | Conditional | Connection URL without credentials. Use it together with `THINPOST_DATABASE_USERNAME_FILE` and `THINPOST_DATABASE_PASSWORD_FILE`; the process builds the full URL from the file contents. Mutually exclusive with `THINPOST_DATABASE_URL`. Subject to the same parameter rules. |
| `THINPOST_DATABASE_USERNAME_FILE` | path | none | Conditional | Absolute path of a regular file holding the database user name (at most 256 bytes, UTF-8). Required with the base URL. |
| `THINPOST_DATABASE_PASSWORD_FILE` | path | none | Conditional | Absolute path of a regular file holding the database password (at most 1024 bytes, UTF-8). Required with the base URL. |
| `THINPOST_ALLOW_INSECURE_LOCAL_DB` | `1` | unset | Optional | When exactly `1`, allows a connection without `sslmode=verify-full`, but only to a loopback IP address. For disposable local databases only. |

### Operator authentication (OIDC)

The control plane validates bearer access tokens from any OpenID Connect provider. The console signs operators in as a public client. See [Operator authentication](/deploy/operator-auth) for how to set up a provider.

The issuer, audience, JWKS URL, administrator role, client ID and scopes are required, as is one of the two scope-check settings. The control plane refuses to start with "operator authentication configuration is incomplete" when one is missing or invalid.

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_OIDC_ISSUER` | HTTPS URL | none | Required | The token issuer. Tokens must carry exactly this `iss`. Must be an `https://` URL without credentials, query or fragment. |
| `THINPOST_OIDC_AUDIENCE` | string | none | Required | The `aud` that tokens must carry: the API resource, not the console's client ID. It must differ from `THINPOST_OIDC_CLIENT_ID`. Must not contain whitespace. |
| `THINPOST_OIDC_JWKS_URL` | HTTPS URL | none | Required | Location of the issuer's signing keys. Keys are fetched over HTTPS without following redirects and refreshed every five minutes. Keys whose `use` is anything other than `sig` (or absent) are refused. |
| `THINPOST_OIDC_ADMIN_ROLE` | string | none | Required | The role value that grants administrator access. A token is an administrator only when the role claim contains this value and the scope condition below is met. Must not contain whitespace. |
| `THINPOST_OIDC_ADMIN_SCOPE` | string | none | Conditional | A scope the token must carry, read from the standard space-delimited `scope` claim and from `scp` (a space-delimited string or an array). Required unless `THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL` is `true`; setting both is an error. Must not contain whitespace. |
| `THINPOST_OIDC_ADMIN_SCOPE_OPTIONAL` | `true` | unset | Conditional | Set to exactly `true` to drop the required-scope check. The administrator role alone then grants access, although the token must still carry a non-empty `scope` or `scp` claim, which excludes ID tokens. Any value other than `true` is an error. |
| `THINPOST_OIDC_ROLES_CLAIM` | claim name or dotted path | `roles` | Optional | Claim that holds the roles. Either a top-level claim name or a dotted path into nested objects, such as `realm_access.roles`. The whole string is tried as a top-level claim first; the path is walked only when the claim is absent. The value may be a string (one role) or an array of strings. Must not contain whitespace or empty path segments, at most 256 characters. |
| `THINPOST_OIDC_ALGORITHMS` | list | `RS256` | Optional | Accepted JWS signature algorithms, separated by commas and/or whitespace. Allowed values: `RS256`, `ES256`, `EdDSA`, `PS256`. Empty entries and duplicates are errors. |
| `THINPOST_OIDC_CLIENT_ID` | string | none | Required | Client ID of the console's public OIDC client, published to the console through `/api/v1/config`. Printable text, at most 256 characters, with no surrounding whitespace. |
| `THINPOST_OIDC_SCOPES` | whitespace-separated list | none | Required | Scopes the console requests at sign-in. One to sixteen entries, each without spaces and at most 256 characters, and `openid` must be among them. |

### Object storage

All of these are optional as a group. Leaving every one of them unset runs the control plane without object storage; it then cannot hold message content, so submission, webhooks, tracking, the content views and the control bridge are unavailable. Setting any one of them requires the whole group (except the insecure switch).

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_S3_ENDPOINT` | `host[:port]` | none | Conditional | S3-compatible endpoint as a bare host with optional port: no scheme, user, path or query. Connections use HTTPS. |
| `THINPOST_S3_BUCKET` | string | none | Conditional | Bucket for message content, tracking mappings and other objects. |
| `THINPOST_S3_REGION` | string | none | Conditional | Region name: lowercase letters, digits and hyphens, at most 64 characters. |
| `THINPOST_S3_ACCESS_KEY` | string | none | Conditional | Access key. |
| `THINPOST_S3_SECRET_KEY` | string | none | Conditional | Secret key. |
| `THINPOST_OBJECT_KEYRING_FILE` | path | none | Conditional | Absolute path of the object-encryption keyring: a JSON object with an `active` key ID and a `keys` map of key ID to a base64-encoded 256-bit key. At most 16 KiB. The active ID must exist in `keys` and keys must be distinct. Every stored object, and every sealed webhook secret, is encrypted with it. |
| `THINPOST_ALLOW_INSECURE_LOCAL_S3` | `1` | unset | Optional | When exactly `1`, connects to the object store over plain HTTP. Allowed only when the endpoint host is a loopback IP address and the database host is a loopback IP address too. For disposable local setups only. |

### Submission, signing and the app API

The submission API (`POST /emails` and the provider-compatible routes) and the app API are enabled by a fingerprint key. They also need object storage, a signing catalog and the control bridge; the control plane refuses to start when any of those is missing.

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_CONTROL_FINGERPRINT_KEYS_FILE` | path | none | Conditional | JSON array of 1 to 8 base64-encoded keys (each 32 to 1024 bytes decoded, file at most 16 KiB). The first key computes new idempotency fingerprints; the rest only verify existing ones. Enables the submission API. Setting it together with `THINPOST_CONTROL_FINGERPRINT_KEY_FILE` is an error. |
| `THINPOST_CONTROL_FINGERPRINT_KEY_FILE` | path | none | Conditional | Single-key form of the fingerprint key: a regular file of 32 to 1024 raw bytes. Enables the submission API when the keyring form is not set. The secret must outlive the idempotency window. |
| `THINPOST_CONTROL_SIGNING_CATALOG` | path | none | Conditional | JSON array (at most 1024 entries) of `{"app_id", "domain", "selector", "key_path"}` objects (`app_id` is the owning app's 32-character lowercase hex ID) that maps DKIM selectors to RSA private keys in PEM files (PKCS#8 or PKCS#1) at absolute paths. Required for message preparation; enables the preparation loop. Unknown fields are rejected. Rotation requires a restart. |

### Link and open tracking

Tracking is enabled when any of the first three variables is set; then all three are required, together with a fingerprint key and the dependencies named above (object storage, signing catalog, control bridge). See [Tracking](/guide/tracking).

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_TRACKING_ORIGIN` | HTTPS origin | none | Conditional | Public origin that tracking links and pixels use, for example `https://track.example.com`. Must be `https://host[:port]` with no path (a single `/` is allowed), query, fragment or credentials, at most 512 characters. |
| `THINPOST_TRACKING_LINK_KEYS_FILE` | path | none | Conditional | Absolute path of the link-key file: a JSON object with a base64 `seed` (the current key seed) and an optional `previous_seeds` array of base64 32-byte seeds (at most 4096) that keep old links valid. At most 1 MiB. An all-zero seed is refused. |
| `THINPOST_TRACKING_EVENT_LIMIT` | integer | none | Conditional | Maximum number of events recorded per tracked link. Integer from `1` to `1000`. |
| `THINPOST_TRACKING_QUEUE_WINDOW` | duration | none | Rejected | Not supported. Setting it makes startup fail: tracking links do not expire. |
| `THINPOST_TRACKING_POST_SEND_VALIDITY` | duration | none | Rejected | Not supported. Setting it makes startup fail: tracking links do not expire. |

### Webhook destinations

By default webhook URLs must be public HTTPS endpoints. These two variables add a narrow exception for in-cluster receivers. See [Webhooks](/guide/webhooks).

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_WEBHOOK_INTERNAL_DESTINATIONS` | list of URLs | none (public HTTPS only) | Optional | Exact `http` or `https` URLs that webhook endpoints may use despite being cluster-internal, at most 16. Each must be in canonical form, with a host of the form `<service>.<namespace>.svc.cluster.local`, no credentials or fragment, and no explicit default port. A URL matches only exactly (scheme, host, port, path and query). Requires `THINPOST_SERVICE_CIDRS`. |
| `THINPOST_SERVICE_CIDRS` | list of CIDRs | none | Conditional | The cluster Service ranges. An internal destination must resolve inside one of these at connection time. Each entry must be a canonical CIDR in wholly private address space, no broader than `/12` (IPv4) or `/108` (IPv6), and must not contain a cloud metadata address. At most 8. |

### Recipient policy

Read by the control plane when the control bridge is enabled. The control plane is the sole authority for it: the Edge worker does not read it.

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_RECIPIENT_POLICY` | `unrestricted` or `allowlist` | none | Conditional | Explicit envelope-recipient policy. `unrestricted` allows any recipient. `allowlist` allows only the addresses in `THINPOST_RECIPIENT_ALLOWLIST_FILE`. Any other value, or no value, is an error wherever the policy is loaded. |
| `THINPOST_RECIPIENT_ALLOWLIST_FILE` | path | none | Conditional | Required with `allowlist`, and rejected with `unrestricted`. A regular text file of at most 64 KiB with one mailbox address per line; blank lines and lines starting with `#` are ignored. At least one and at most 512 addresses. Local parts are case-sensitive, domains are not. |

### Control bridge

The bridge is the control plane's client side of the [control protocol](/reference/control-protocol). It is disabled unless `THINPOST_CONTROL_EDGE_TARGET` is set. When it is set, every other variable in this group is required, except the optional route-exceptions file. When it is not set, none of them may be set, or startup fails. The bridge requires object storage and an explicit [recipient policy](#recipient-policy).

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_CONTROL_EDGE_TARGET` | `host:port` | none (bridge disabled) | Optional | Address of the Edge worker's control listener. The host must be a literal private or loopback IP address. |
| `THINPOST_CONTROL_EDGE_DNS` | DNS name | none | Conditional | Server name the control plane verifies in the Edge's control certificate. |
| `THINPOST_CONTROL_EDGE_IDENTITY` | URI | none | Conditional | The exact URI identity (SPIFFE-style, in the certificate's URI SANs) the Edge must present. |
| `THINPOST_CONTROL_WORKER_ID` | string | none | Conditional | Identifier of the Edge worker. Must equal the worker's `THINPOST_EDGE_WORKER_ID`. |
| `THINPOST_CONTROL_JOURNAL_ID` | 32 lowercase hex characters | none | Conditional | Identity of the Edge journal the control plane is bound to. The Edge prints it with `thinpost-edge journal-id`. |
| `THINPOST_CONTROL_INSTANCE_ID` | string | none | Conditional | Identifier of this control instance in the control protocol. |
| `THINPOST_CONTROL_IDENTITY` | URI | none | Conditional | The control plane's own URI identity. It must appear exactly once in the URI SANs of `THINPOST_CONTROL_CLIENT_CERT`, and the Edge must list it in `THINPOST_EDGE_CONTROLLER_IDENTITIES`. |
| `THINPOST_CONTROL_CLIENT_CERT` | path | none | Conditional | Client certificate (PEM) the control plane presents to the Edge. Its key also signs the lease proofs the Edge verifies. |
| `THINPOST_CONTROL_CLIENT_KEY` | path | none | Conditional | Private key (PEM) for the client certificate. |
| `THINPOST_CONTROL_PRIVATE_CA` | path | none | Conditional | PEM bundle of the private CA that signs the Edge's control certificate. |
| `THINPOST_CONTROL_BOUNCE_ACTIVE_KEY` | string | none | Conditional | Version name of the key in the bounce keyring that signs new bounce addresses. Must exist in the keyring. |
| `THINPOST_CONTROL_BOUNCE_KEYS_FILE` | path | none | Conditional | JSON object mapping key version (at most 64 characters) to a base64 key of at least 32 bytes. At most 32 keys and 16 KiB. Older versions remain to verify existing bounce addresses. |
| `THINPOST_CONTROL_INFRASTRUCTURE_CIDRS` | list of CIDRs | none | Conditional | Ranges the control plane's route policy treats as infrastructure and refuses as mail destinations. Must include every source address the Edge reports (control takes them from the Edge's report, not from its own settings): the control plane refuses an Edge whose Ready reports a source outside these ranges, or one a route exception names, and never routes mail to a reported source. The control plane is the only component that applies this policy. |
| `THINPOST_CONTROL_ROUTE_EXCEPTIONS_FILE` | path | none | Optional | Route-exception file, in the [format below](#route-exceptions-file). |

## Edge worker

The `thinpost-edge` binary reads these when it starts. See the [command-line reference](/reference/cli#thinpost-edge) for its subcommands. It reads no recipient policy and no egress policy: the control plane is the sole authority for both. An Edge whose environment still sets `THINPOST_RECIPIENT_POLICY`, `THINPOST_RECIPIENT_ALLOWLIST_FILE`, `THINPOST_EDGE_INFRASTRUCTURE_CIDRS` or `THINPOST_EDGE_ROUTE_EXCEPTIONS_FILE` from an earlier release starts normally and ignores them. So does one that sets `THINPOST_EDGE_BOUNCE_DOMAIN` or `THINPOST_EDGE_ADDITIONAL_BOUNCE_DOMAINS`: the Edge accepts bounces at each listener's own name and has no other bounce domain. The Edge refuses to run as root.

### Identity and listeners

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_EDGE_WORKER_ID` | string | none | Required | Identifier of this worker: 1 to 128 letters, digits, `-` and `_`. |
| `THINPOST_EDGE_SMTP_HOSTNAME` | DNS name | none | Required | Primary SMTP name, the first address's. The Edge greets with it, uses it as the outbound HELO name and presents a certificate for it. Lowercase ASCII, at most 219 characters. |
| `THINPOST_EDGE_SMTP_LISTEN` | `ipv4:25` | none | Required | Address of the SMTP listener. Must be a literal IPv4 address on port `25`. It is also the primary outbound source address. |
| `THINPOST_EDGE_CONTROL_LISTEN` | `host:port` | none | Required | Address of the private control listener, which only the control plane connects to. The host must be a literal private or loopback IP address. |
| `THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES` | list of IPv4 addresses | none | Optional | Further public IPv4 addresses to listen on and send from, at most 15. Each must be a public address and differ from every other source. |
| `THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES` | list of DNS names | none | Conditional | Exactly one name per additional source, in the same order. Each is lowercase, at most 219 characters, unique and different from the primary name. Required with additional sources and rejected without them. |
| `THINPOST_EDGE_SMTP_ADDITIONAL_CERT_ROOT` | path | none | Conditional | Directory holding the TLS certificates of the additional sources: `<root>/<N>/tls.crt` and `tls.key`, numbered from `0` in source order. Required with additional sources; must be a clean absolute path. |
| `THINPOST_EDGE_SOURCE_GATEWAYS` | list of IPv4 addresses | none | Optional | Gateway of each source, primary first, one per source. When set, the Edge announces each source address to its gateway and reports ready only after every gateway resolves. When unset, there is no announcement and no readiness gate on it. |

### SMTP submission

Authenticated SMTP submission for apps that send raw mail. It is off by default. Port 25 is never affected: it keeps CHUNKING and offers no AUTH.

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_EDGE_SUBMISSION` | `true` or `false` | `false` | Optional | `true` opens port 587 (STARTTLS) and port 465 (implicit TLS) on every sending address. Each address presents its own listener certificate, the same one it uses on port 25. Any other value is an error. The Edge reports both ports to the control plane so the console can show whether they are available. |
| `THINPOST_EDGE_SUBMISSION_INTERNAL_LISTEN` | `ip:port` | none | Optional | An extra in-cluster listener (STARTTLS) for clients that should not use the public addresses. The host must be a literal private or loopback IP, and the port must not be `25` or the control port. It is never reported to the control plane or shown in the console. It works without `THINPOST_EDGE_SUBMISSION`. |
| `THINPOST_EDGE_SUBMISSION_INTERNAL_HOSTNAME` | DNS name | none | Conditional | Name the internal listener greets with and its certificate is issued for. Required with the internal listener. |
| `THINPOST_EDGE_SUBMISSION_INTERNAL_CERT` | path | none | Conditional | Certificate of the internal listener. Required with the internal listener. It is checked against the CA below and must use a different key from the control certificate. |
| `THINPOST_EDGE_SUBMISSION_INTERNAL_KEY` | path | none | Conditional | Private key for the internal listener's certificate. Required with the internal listener. |
| `THINPOST_EDGE_SUBMISSION_INTERNAL_CA` | path | none | Conditional | PEM bundle of the cluster-local CA that issued the internal certificate. Required with the internal listener. It is read on every handshake, so a rotated CA needs no restart. |

Submission listeners have their own connection limits, separate from port 25: the public ports together allow 64 connections in total and 16 per source address, with a connection rate of one per 20 ms globally (burst 200) and one per 200 ms per source (burst 30). The internal listener allows 64 in total and 32 per source. Authentication is limited to three attempts per connection and a burst of ten failed attempts per source address (refilling one per minute), and messages are limited to 7.5 MiB after decoding.

### Control channel and certificates

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_EDGE_CONTROL_CA` | path | none | Required | PEM bundle of the private CA that signs the control plane's client certificate. |
| `THINPOST_EDGE_CONTROL_CERT` | path | none | Required | Certificate the Edge presents on the control listener. |
| `THINPOST_EDGE_CONTROL_KEY` | path | none | Required | Private key for the control certificate. |
| `THINPOST_EDGE_CONTROLLER_IDENTITIES` | list of URIs | none | Required | URI identities the Edge accepts from the control plane. At least one; empty entries are an error. |
| `THINPOST_EDGE_SMTP_CERT` | path | none | Required | Certificate for the public SMTP listener (STARTTLS) covering the primary name. |
| `THINPOST_EDGE_SMTP_KEY` | path | none | Required | Private key for the SMTP certificate. |

### Journal and clock

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_EDGE_JOURNAL_PATH` | path | none | Required | Journal file on persistent single-writer storage. |
| `THINPOST_EDGE_JOURNAL_CAPACITY` | integer | none | Required | Maximum number of messages the journal holds. `1` to `100000`. |
| `THINPOST_EDGE_JOURNAL_RESERVE_BYTES` | integer (bytes) | none | Required | Bytes the journal keeps in reserve. `1048576` (1 MiB) to `1073741824` (1 GiB). |
| `THINPOST_EDGE_CLOCK_MARGIN` | duration | none | Required | Clock-skew margin for lease validation. Greater than `0` and at most `30s`. |

### Inbound SMTP rate limits

These bound new connections to the public listeners before any SMTP processing. All listeners share one limit. The Edge also caps concurrent connections at 64 in total and 8 per source address.

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_EDGE_SMTP_GLOBAL_INTERVAL` | duration | `100ms` | Optional | Interval at which the global connection budget refills. `10ms` to `1h`. |
| `THINPOST_EDGE_SMTP_GLOBAL_BURST` | integer | `100` | Optional | Global connection burst. `1` to `1000`. |
| `THINPOST_EDGE_SMTP_SOURCE_INTERVAL` | duration | `1s` | Optional | Interval at which the budget of one remote address (an IPv4 address or an IPv6 /64) refills. `10ms` to `1h`. |
| `THINPOST_EDGE_SMTP_SOURCE_BURST` | integer | `12` | Optional | Connection burst for one remote address (an IPv4 address or an IPv6 /64). `1` to `1000`. |

### Outbound SMTP stage timeouts

Each timeout must be between `1s` and `30m`.

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_EDGE_SMTP_CONNECT_TIMEOUT` | duration | `5m` | Optional | Waiting for the TCP connection to the destination. |
| `THINPOST_EDGE_SMTP_GREETING_TIMEOUT` | duration | `5m` | Optional | Waiting for the destination's greeting. |
| `THINPOST_EDGE_SMTP_COMMAND_TIMEOUT` | duration | `5m` | Optional | Each SMTP command and its reply. |
| `THINPOST_EDGE_SMTP_DATA_INIT_TIMEOUT` | duration | `2m` | Optional | The reply to `DATA`. |
| `THINPOST_EDGE_SMTP_DATA_BLOCK_TIMEOUT` | duration | `3m` | Optional | Each block of message data. |
| `THINPOST_EDGE_SMTP_DATA_FINAL_TIMEOUT` | duration | `10m` | Optional | The reply after the final `.`. |

## Route exceptions file

`THINPOST_CONTROL_ROUTE_EXCEPTIONS_FILE` points at a text file that lets a specific route through the infrastructure deny ranges. It is not a general allowlist. Each non-blank line that does not start with `#` holds three whitespace-separated fields:

```text
recipient@example.com mx.example.com 203.0.113.25
```

The recipient mailbox, the MX name and the numeric IP address must all match for a delivery to use the exception. At most 128 entries, file at most 16 KiB, and a configured file must contain at least one entry.

## Maintenance commands

| Variable | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `THINPOST_RUNTIME_ROLE` | string | none | Required | Name of the PostgreSQL login role the control plane runs as. The `deploy-migrate` and `deploy-reopen` commands read it, together with `THINPOST_DEPLOYMENT_GENERATION`, and verify that role's privileges before migrating and before reopening the admission gate. |

Every `thinpost-control` subcommand also reads the [database variables](#database). `recovery objects` additionally reads the [object storage variables](#object-storage). Both binaries refuse to run when `GRPC_BINARY_LOG_FILTER` is set to any value, because gRPC binary logs record recipients and message bodies. The `deploy-reopen` command additionally uses the in-cluster Kubernetes service environment.

## Removed (startup fails)

Delivery retry behavior is stored in the database and edited in the console (Operations). The control plane refuses to start when any of these variables is set, so an old value cannot silently disagree with the database.

| Variable | Replacement |
| --- | --- |
| `THINPOST_DELIVERY_MAX_ATTEMPTS` | Edit the retry policy in the console. |
| `THINPOST_DELIVERY_RETRY_INITIAL_DELAY` | Edit the retry policy in the console. |
| `THINPOST_DELIVERY_RETRY_MAX_DELAY` | Edit the retry policy in the console. |
| `THINPOST_DELIVERY_RETRY_MAX_AGE` | Edit the retry policy in the console. |

## Removed (ignored)

The Edge reports its own sending addresses and their HELO names when it connects, and the control plane uses that report instead of settings of its own. These variables from earlier releases are no longer read. They are ignored if set, not refused, so old values can stay in place while new pods roll out.

- `THINPOST_CONTROL_SMTP_SOURCE`: the Edge's source addresses come from its report.
- `THINPOST_CONTROL_BOUNCE_DOMAIN`: the return-path domain is the HELO name of the address the message is sent from, as reported.
- `THINPOST_INBOUND_MX_HOST`: the inbound mail host is the HELO name of the Edge's main address, as reported.
