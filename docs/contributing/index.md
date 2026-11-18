---
title: Contributing
description: Toolchain, how to build and test Thinpost, and what a pull request needs.
---

# Contributing

Thanks for helping. This page covers the toolchain, the build and test commands, and what a pull request needs. See [Repository layout](/contributing/layout) for where code lives and [Testing](/contributing/testing) for the fixtures that integration tests use.

## Toolchain

| Tool | Version | Needed for |
| --- | --- | --- |
| Go | The version in `go.mod` (1.26 or newer). CI and the container images build with Go 1.27 | The control plane, the edge worker, tests |
| Node.js | The version in `ui/.node-version` (26) | The operator console, the SDK contract tests |
| Docker | Recent | Container images, the local pipeline and Edge image tests |
| Helm 3 | Recent | Chart lint and chart tests |
| Python 3 | Any current 3.x | The scripts under `scripts/` |
| `protoc` | 36.2 | Regenerating the control protocol. See [Generated code](/contributing/generated-code) |

PostgreSQL is not needed on your machine. The test scripts start disposable PostgreSQL and S3-compatible containers.

## Build

```sh
go build ./cmd/thinpost-control ./cmd/thinpost-edge
```

The Dockerfile builds separate non-root images:

```sh
docker build --target control -t thinpost-control:dev .
docker build --target edge -t thinpost-edge:dev .
```

The control image also builds the operator console from the checked-in OpenAPI contract in a separate Node stage and serves it on the same origin as the API. The edge image has no console.

## Test

For Go changes, run the tests for the packages you touched, and the whole suite when a change crosses package boundaries:

```sh
go test ./...
go vet ./...
gofmt -l .
```

Database-backed tests skip unless a disposable database is configured, so a green plain `go test` run is not integration coverage. To run the integration suites:

```sh
python3 scripts/test-local-pipeline.py --postgres-version 18
```

CI runs `go test -p 1 ./...` against disposable PostgreSQL 17 and 18, because fixture setup changes database-wide grants and packages must not race each other. Details, guards and every test environment variable are on the [Testing](/contributing/testing) page.

For console changes, run these from `ui/`:

```sh
npm ci --no-audit --no-fund
npm test
npm run build
```

See [Console development](/contributing/console-dev).

For adapter or SDK changes, run the [SDK contract tests](/contributing/testing#sdk-contract-tests). For chart changes, run `sh scripts/test-helm-chart.sh`.

## Pull requests

A pull request should:

- do one thing, with a description that says why;
- include tests for behavior it adds or fixes, using the existing fixtures rather than new ad-hoc ones;
- pass `go vet`, `gofmt`, the Go tests, the console tests and build, and the chart checks that CI runs;
- keep generated code in sync: regenerate protobuf output and the console's API client when you change `api/`, and do not hand-edit generated files;
- update the documentation under `docs/` when it changes behavior, configuration, chart values, the API or an operational procedure. CI also checks that the configuration and Helm values references match the code;
- never edit an applied database migration. Add a new version instead. Review the runtime grant list for every migration;
- keep message content, addresses, subjects, secrets and search terms out of logs, errors, audit rows and URLs. See [Data handling](/security/data-handling);
- use synthetic data in tests and docs: `example.com` and the RFC 5737 ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), never real addresses, hosts or credentials.

Never point a test fixture at a shared or production database, or at a forwarded port to one. The fixture guards exist to prevent that, and a change must not weaken them.

## Reporting problems

Report bugs and request features in the issue tracker. For a suspected vulnerability, use [private reporting](/security/reporting) instead.

## License

Thinpost is MIT-licensed. By contributing you agree that your contribution is licensed under the same terms.
