---
title: Testing
description: The test suites, the fixture guards that keep them away from real systems, the helper scripts, and every THINPOST_TEST environment variable.
---

# Testing

Most tests are plain `go test`. Tests that need PostgreSQL, an S3 API, a disposable filesystem or Docker are opt-in: they skip unless you provide a fixture through an environment variable, and they refuse to run against anything that does not look disposable.

## Run the suites

```sh
go test ./...
go vet ./...
```

A green run without fixtures skips the database-backed tests. That is not integration coverage. Use the scripts below for it.

### Serial packages: `-p 1`

Database-backed packages share fixture setup that changes database-wide grants, such as the runtime role's privileges and the database's `PUBLIC` `TEMP` privilege. Running packages in parallel against one database races that setup and cleanup. CI therefore runs:

```sh
go test -p 1 ./...
```

Do the same when you run against a fixture database yourself.

### Race detector

CI also runs the SMTP-facing packages under `-race`. It needs cgo and a C compiler, unlike the CGO-free production build:

```sh
CGO_ENABLED=1 go test -race -count=1 ./internal/thirdparty/gosmtp ./internal/bounce ./internal/edgesession ./internal/controlsession
```

See [SMTP server](/contributing/smtp-server) for why these packages are raced.

## Fixture guards

A database or object-store fixture must be **disposable and local**. The guards enforce it, and a change must not weaken them:

- The pipeline fixture requires `THINPOST_TEST_DISPOSABLE_DB=1` and a database named `thinpost_fixture` on a numeric loopback address or a local Unix socket, with `sslmode=disable` and the `postgres` user. It creates a restricted runtime login and temporarily revokes the database's `PUBLIC` `TEMP` privilege, then restores it. Use an isolated server only.
- A loopback port forwarded to a shared database is not a safe fixture. Never point fixtures at a staging or production database, or at a tunnel to one.
- Other integration tests require a numeric loopback host and create and drop their own random schema.
- The S3 test endpoint must be a numeric loopback address and port. The test client ignores proxies and refuses to dial anything else. Credentials are synthetic.
- Hostnames are never accepted for fixtures, so DNS cannot redirect a test to a real server.

## Scripts

### `scripts/test-local-pipeline.py`

Runs the mail pipeline end to end against disposable local services. It starts PostgreSQL (`--postgres-version 17` or `18`, default 17) and an S3-compatible emulator in Docker on loopback ports, sets the variables below, runs the selected Go tests with `-race`, and removes its containers. It uses synthetic credentials and simulated SMTP. Docker is required.

```sh
python3 scripts/test-local-pipeline.py --postgres-version 17
python3 scripts/test-local-pipeline.py --postgres-version 18
```

The emulator does not prove the permissions, consistency, versioning, lifecycle or backup behavior of a real object store.

### `scripts/test-edge-image.py`

Exercises a built Edge image with synthetic identities on an internal Docker network. It never sends mail to the internet and reads no deployment credentials. It generates short-lived certificates, starts the image as a non-root, read-only container, and drives a synthetic control client through the control stream and a bounce.

```sh
python3 scripts/test-edge-image.py --image thinpost-edge:dev
```

`--protocol` selects the control protocol version that the image supports. It does not test Kubernetes, real DNS, a database or IP fencing.

### `scripts/test-helm-chart.sh`

Lints the chart in every variant (edge, control, hooks on and off, scaled to zero, additional sending IPs, CNI overrides) and asserts the rendering contract, including that only the migration Jobs mount the schema owner credentials. It needs only `helm` and POSIX tools and contacts no cluster.

```sh
sh scripts/test-helm-chart.sh
```

### Other scripts

- `scripts/verify-edge-routing.py` proves in a disposable local Docker lab that two network attachments route out of their own interfaces. It uses synthetic addresses and a privileged test container.
- `scripts/test-projected-certificates.py` tests Edge certificate rotation through a real Kubernetes Secret projection in a namespace that you name. It needs `kubectl` and a disposable namespace, and allows several minutes for projection.

### SDK contract tests

`tests/sdk-contract/` drives the real Postmark, Resend, SendGrid and Mailgun SDKs against a fixture server built from `tests/sdk-contract/fixture`. The test process blocks any connection that is not to the fixture. After changing an adapter, rebuild the fixture for your host, because an old Linux artifact cannot run on macOS:

```sh
go build -o tests/sdk-contract/fixture-server ./tests/sdk-contract/fixture
cd tests/sdk-contract
npm ci --ignore-scripts --no-audit --no-fund
THINPOST_SDK_FIXTURE_BINARY=./fixture-server npm test
```

`THINPOST_SDK_FIXTURE_BINARY` names the fixture executable and defaults to `./fixture-server`. The SDK versions are pinned in `tests/sdk-contract/package.json`.

## Environment variables

Every variable below is read only by tests and test scripts. None of them affects the control or edge process.

### Database

| Variable | Meaning |
| --- | --- |
| `THINPOST_TEST_DATABASE_URL` | PostgreSQL URL of a disposable database. Database-backed tests skip when it is unset. Use `postgres://postgres:<password>@127.0.0.1:<port>/thinpost_fixture?sslmode=disable`. Set by `test-local-pipeline.py`. |
| `THINPOST_TEST_DISPOSABLE_DB` | Set to `1` to attest that the database is disposable. Tests that create roles, change grants or run the pipeline fixture fail without it. Set by `test-local-pipeline.py`. |

### Object storage

| Variable | Meaning |
| --- | --- |
| `THINPOST_TEST_S3_ENDPOINT` | `host:port` of a local S3 API emulator, numeric loopback only. When unset, pipeline tests use an in-memory object store. Set by `test-local-pipeline.py`. |
| `THINPOST_TEST_S3_SECURE` | Set to `1` to run the recovery object-inventory command test against a real TLS S3 service instead of an emulator. The test creates and removes a randomly named bucket. |
| `THINPOST_TEST_S3_ALLOW_REMOTE` | Must be `1` for a secure run against a non-loopback endpoint, so a real-service run never happens by accident. |
| `THINPOST_TEST_S3_SECURE_ENDPOINT` | Must equal `THINPOST_TEST_S3_ENDPOINT` for a secure run; it confirms the exact service you intend to use. |
| `THINPOST_TEST_S3_ACCESS_KEY`, `THINPOST_TEST_S3_SECRET_KEY` | Credentials for the secure run. They must be able to create and delete the bucket. Never use production credentials. |

### Filesystem

These tests fill a filesystem on purpose. Give them a small dedicated mount, never a shared host or a live journal volume.

| Variable | Meaning |
| --- | --- |
| `THINPOST_TEST_DISPOSABLE_FULL_FILESYSTEM` | Directory on a disposable, bounded filesystem for the out-of-space tests of the journal and the edge worker. Tests skip when unset. |
| `THINPOST_TEST_RESERVE_64M` | Set to `1` together with the variable above to run the larger variant that uses a 64 MiB reserve and can write up to 128 MiB. The test refuses to run unless the directory is on its own filesystem of at most 128 MiB. |
| `THINPOST_TEST_DISPOSABLE_RESERVE_DIR` | Directory on a disposable filesystem for the storage-reserve lifecycle test. |

### DNS

| Variable | Meaning |
| --- | --- |
| `THINPOST_TEST_LIVE_DNS` | Set to `1` to run tests that query real DNS through the system resolver, for example the null MX of `example.com`. They send DNS queries only and never connect to the names. |
| `THINPOST_TEST_LIVE_NODATA_NAME`, `THINPOST_TEST_LIVE_NXDOMAIN_NAME` | A name that exists but has no MX records, and a name that does not exist. Both are needed for the live negative-answer test. |
| `THINPOST_TEST_DNS_CLUSTER_SERVER` | Set to `1` to run a synthetic DNS server on TCP and UDP port 1053 for up to three minutes. Run it in an isolated lab. |
| `THINPOST_TEST_DNS_CLUSTER_ADDRESS` | Private IPv4 address of that server. Enables the client tests: slow-primary fallback, truncated UDP falling back to TCP, and a denied peer. Loopback and public addresses are rejected. |

### Edge image and certificates

| Variable | Meaning |
| --- | --- |
| `THINPOST_TEST_EDGE_IMAGE_GENERATE` | Directory where a test writes freshly generated synthetic certificates and keys. Set by `test-edge-image.py` and `test-projected-certificates.py`. |
| `THINPOST_TEST_EDGE_IMAGE_FIXTURE` | Directory holding those certificates, mounted into the client container. |
| `THINPOST_TEST_EDGE_IMAGE_JOURNAL` | The 32-hex journal identity that the Edge image under test reports. |
| `THINPOST_TEST_EDGE_IMAGE_PROTOCOL` | Control protocol version for the fixture client. The test accepts the versions it knows. |
| `THINPOST_TEST_PROJECTED_CERTIFICATES` | Directory with a projected Secret that the Edge rotation test watches. Set by `test-projected-certificates.py`. |

## Adding tests

- Skip with a clear message when a fixture is absent, and fail when a guard condition is violated. Do not silently pass.
- Keep fixtures synthetic: generated keys, `example.com`, `.invalid` names and RFC 5737 addresses.
- A test that creates a schema removes it. Use a random suffix so that serial and repeated runs do not collide.
- A new `THINPOST_TEST_*` variable needs a row in this page.
