# Contributing to Thinpost

Thanks for helping. The full contributor guide lives in the docs:

- Site: https://zyno-io.github.io/thinpost/contributing/
- Source: [docs/contributing/](docs/contributing/)

## Quick start

```sh
# Go services (the integration tests need PostgreSQL and an S3 endpoint;
# see the contributing guide for the disposable fixture setup)
go build ./...
go vet ./...
go test -p 1 ./...

# Operator console
cd ui && npm ci && npm test && npm run build

# Documentation site
npm ci --prefix docs
npm run --prefix docs docs:dev      # preview
npm run --prefix docs docs:build    # fails on dead links

# Chart and repository checks
sh scripts/test-helm-chart.sh
python3 scripts/leak-check.py
```

## Before you open a pull request

- Run the checks for the parts you changed, and `gofmt -l .` (it must print
  nothing).
- Update the docs, the OpenAPI contracts under `api/` and the chart values
  reference together with the code.
- Regenerate protobuf output with `sh scripts/gen-proto.sh` when you change
  `api/control/v1/control.proto`.
- Never include secrets, tokens, private keys or non-public hostnames or
  addresses in code, tests, docs or issues. Use reserved example names and
  documentation address ranges.
- Report security issues privately; see [SECURITY.md](SECURITY.md).

Licensing is described in [LICENSING.md](LICENSING.md).
