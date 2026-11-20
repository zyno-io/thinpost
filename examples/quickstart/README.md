# Docker Compose quickstart

A complete, disposable Thinpost stack on one machine. **For evaluation only**:
the sign-in service has no password, the database and object store use plain
loopback connections, and every generated secret sits in one Docker volume.

```sh
docker compose up -d --build --wait
examples/quickstart/smoke.sh        # needs curl, jq and openssl on the host
```

The first start builds the images (several minutes); later starts reuse them.
For command-line clients, copy the local root certificate out of the Caddy
container and pass it explicitly:
`docker compose cp caddy:/caddy-root/caddy-root.crt ./quickstart-ca.pem`, then
`curl --cacert quickstart-ca.pem ...`. Prefer that to importing the root into
your system or browser trust store: it is an unrestricted CA whose private key
lives in a Docker volume, so anything that can read that volume could issue
certificates your machine would trust. If you import it anyway, remove it again
after `docker compose down -v`.
Tear everything down, including generated secrets and data, with
`docker compose down -v`.

| Where | What |
| --- | --- |
| <https://localhost:8443> | Operator console. Accept the local certificate warning once (Caddy's internal CA), then use the one-button development sign-in. |
| <http://localhost:8025> | Mailpit: every message Thinpost delivers lands here. |
| `https://localhost:8443/email` | Postmark-compatible submission endpoint (`X-Postmark-Server-Token`). Resend, SendGrid and Mailgun paths work too. |

The stack seeds one app, **Quickstart**, with the sender domain
`quickstart.example` (DKIM selector `mail1`) and the recipient
`demo@quickstart.example`. Create an API key for the app in the console
(Applications), publish the domain's ownership TXT in the local zone with
`docker compose run --rm --no-deps --entrypoint sh pki-init /qs/zone-add.sh <name> <value>`
(the name may be the relative `_thinpost` or the full `_thinpost.quickstart.example` the console
shows, with or without a trailing dot),
verify the domain in the console, then send:

```sh
curl -sk https://localhost:8443/email \
  -H "X-Postmark-Server-Token: $KEY" -H 'Content-Type: application/json' \
  -d '{"From":"hello@quickstart.example","To":"demo@quickstart.example","Subject":"Hi","TextBody":"Hello"}'
```

`smoke.sh` does all of that, plus the dev login, and also checks the delivered
message's `DKIM-Signature` against the local DNS and reads `delivered` back from
the operator API. It then sends over SMTP on 587 (STARTTLS) and 465 (implicit
TLS) with a freshly created SMTP credential, which it revokes again on exit,
checks that Bcc and the client's Received header were removed and that the
Message-ID is Thinpost's, and verifies the DKIM signature of those messages too.

## Sending over SMTP

The Edge listens on 587 and 465 of its sending IP (`THINPOST_EDGE_SUBMISSION=true`
in `edge.env`), with the same certificate as port 25 (`smtp.quickstart.example`).
Those ports exist only on the internal `edgepub` network, so use the `tools-smtp`
helper, which sits on that network and has the quickstart CA:

1. In the console, open the app's **SMTP** tab (name the primary sending IP under
   **Operations → Sending IPs** first if it asks: address `11.42.51.10`, HELO name
   and bounce domain `smtp.quickstart.example`) and create a credential.
2. Send a message (the recipient must be `demo@quickstart.example`):

```sh
printf 'From: hello@quickstart.example\r\nTo: demo@quickstart.example\r\nSubject: Hi\r\n\r\nHello\r\n' |
  docker compose run --rm --no-deps -T --entrypoint curl tools-smtp -sS --ssl-reqd \
    --cacert /pki/ca.crt --resolve smtp.quickstart.example:587:11.42.51.10 \
    smtp://smtp.quickstart.example:587 --mail-from hello@quickstart.example \
    --mail-rcpt demo@quickstart.example --user "$USERNAME:$PASSWORD" -T -
```

Use `smtps://smtp.quickstart.example:465` (and `--resolve ...:465:...`) for implicit TLS.
The message appears in Mailpit once delivered.

## What runs

| Service | Role |
| --- | --- |
| `control`, `edge` | The release `control` and `edge` image targets of the repository `Dockerfile`. |
| `postgres`, `s3` | PostgreSQL 18 and the MiniStack S3 emulator. |
| `tools-smtp`, `tools-dns` | Helpers for `smoke.sh` (profile `tools`): `curl` on the Edge's public network for SMTP submission, and DKIM verification through the local DNS. |
| `mailpit` | The "remote" mail server; offers STARTTLS with a certificate for `mx.quickstart.example`. |
| `coredns` | Authoritative DNS for `quickstart.example` (MX, SPF, DKIM, ownership TXT). |
| `devidp` | Development-only OpenID Connect issuer, `examples/quickstart/devidp`. Refuses to start without `--insecure-dev-only`; built only by this stack's Dockerfile, never by a release target. |
| `caddy` | HTTPS front door on one origin: `/idp/*` to `devidp`, everything else to `control`. |
| `pki-init`, `db-init` | One-shot: generate keys, certificates and the zone; then `bootstrap-database`, `migrate`, `grant-runtime` and open the admission gate (`recovery begin`, `quarantine`, `open`). |
| `ctlnet` | Holds one network namespace for `postgres`, `s3` and `control`, and a firewall rule that drops new connections from the Edge's network. |

## Why it is shaped this way

- **Loopback database and S3.** The control plane accepts an unencrypted
  database or object store only at a numeric loopback address, so `postgres`,
  `s3` and `control` share the `ctlnet` network namespace and talk over
  `127.0.0.1`.
- **HTTPS-only issuer.** The issuer is `https://localhost:8443/idp`; control
  fetches the JWKS at `https://caddy:8443/idp/jwks` and trusts Caddy's root
  (published into the `caddy-root` volume) through `SSL_CERT_FILE`. The browser,
  console and issuer share one origin, so the console's CSP `'self'` covers it.
- **Operator tokens.** `devidp` signs RS256 access tokens with a `scope` claim
  containing `thinpost-admin`, a `roles` array containing `Thinpost.Admin`, and
  audience `thinpost-api`, which differs from the console client ID
  `thinpost-console` (see `docs/operator-auth.md`).
- **Edge source address.** The Edge's SMTP source must be public, not a
  documentation range, and inside the control plane's infrastructure CIDRs. It is
  `11.42.51.10` on an `internal: true` Docker network (`11.42.51.0/24`), so
  nothing is routed off the host. The control listener is on a second
  internal network (`10.99.51.0/24`), where control reaches it by its numeric
  private address.
- **Route exception.** Mailpit is on that same range (`11.42.51.25`), which the
  control plane refuses as a destination by default. `route-exceptions` (read by control) allows exactly
  `demo@quickstart.example` at `mx.quickstart.example` / `11.42.51.25`.
- **No Internet route, and a fenced Edge.** The `front` network (control,
  postgres, s3, devidp, coredns) and both Edge networks are `internal`. Only
  `caddy` and `mailpit` also join a separate `publish` network, so their ports
  can be published on `127.0.0.1`. The Edge shares a network with `ctlnet`, but
  it never connects into it (control dials the Edge), so `ctlnet` drops new
  connections from `10.99.51.0/24`: the Edge cannot reach S3 (`:4566`) or the
  control plane's plain HTTP (`:8080`). Docker has no per-port rule between
  containers on one network, hence the in-namespace rule (`NET_ADMIN` on that
  pause container only).
- **S3 survives restarts.** MiniStack keeps data in memory unless both
  `S3_PERSIST` and `PERSIST_STATE` are set; they are, with named volumes.
- **Signing catalog needs the app ID at start-up**, so `db-init` seeds the
  Quickstart app with a fixed ID and non-zero quotas (new apps refuse all
  submissions until quotas are set) instead of creating it through the API.
- **The release control image has no shell**, and it needs the Edge journal ID
  in its environment. A static `busybox` copied into the `pki` volume runs
  `run-control.sh`, which only reads generated values and `exec`s the binary.

## Image pinning

Every third-party image (PostgreSQL, Caddy, Mailpit, CoreDNS, MiniStack, the
Go and distroless bases) is pinned by multi-arch index digest, with the
human-readable tag kept in a comment. Dependabot updates the pins; do not
replace them with floating tags.

## Files

`compose.yaml` (repository root), `Dockerfile` (tools and `devidp` stages),
`Caddyfile`, `Corefile`, `control.env`, `edge.env`, `route-exceptions`,
`recipients.txt`, `pki-init.sh`, `initdb.sh` / `init.sql`, `db-init.sh`,
`run-control.sh`, `zone-add.sh`, `smoke.sh`, `devidp/`.

Secrets are generated at first start into the `pki` volume and are never
committed.

## Troubleshooting

`docker compose ps -a` and `docker compose logs <service>`. The Edge and
control plane restart on failure while their dependencies come up. If you
changed an env file, recreate the service (`docker compose up -d --force-recreate control`).
To start over: `docker compose down -v`.
