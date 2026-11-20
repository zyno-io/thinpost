---
title: SMTP server
description: The patched go-smtp copy inside the main module, how to change it, how to track upstream fixes, and the race-detector CI steps that guard it.
---

# SMTP server

The Edge receiver speaks SMTP through a patched copy of [emersion/go-smtp](https://github.com/emersion/go-smtp) that lives in this repository as `internal/thirdparty/gosmtp/` (`package smtp`). It is a plain package in the main module, not a vendored dependency, so `go build`, `go test ./...` and `go vet ./...` cover it like any other code.

## Why it is a copy

Two upstream behaviours affect mail we accept, and neither can be fixed from the session side (a third patch adds an option the SMTP submission listeners need):

- **BDAT worker isolation.** After `RSET`, a BDAT transaction's worker could still be running and could deliver its result to the next transaction's final reply, with data races on the connection state.
- **Exact-limit DATA.** A DATA message of exactly the size limit was refused, while BDAT accepted it. A bounce of exactly the receiver's maximum size was therefore rejected.

- **DATA-only mode.** The submission and internal listeners set `Server.DisableCHUNKING`: EHLO omits CHUNKING and a BDAT is answered `502` and the connection is closed, leaving the command loop so bytes already buffered after it are never parsed as commands. This keeps the session's `Data` call synchronous on the connection goroutine, avoids the BDAT worker, and avoids discarding a chunk of up to 4 GiB outside the drain limits. Port 25 keeps CHUNKING.

`internal/thirdparty/gosmtp/PATCHES.md` records the base version, every patch (what, why, where) and how to reproduce the BDAT failure on unpatched code.

## Changing the copy

- Keep upstream code byte-identical except for the patches. Mark each change with a short `// thinpost:` comment so it is easy to re-apply to a newer upstream.
- Update `PATCHES.md` in the same change.
- Upstream test files are not copied, because their fixtures use real domains that the leak check rejects. Write tests in this package with `example.com`, `example.net` and `example.org` only.
- Keep the MIT `LICENSE` file next to the code. `LICENSING.md` describes the vendored code.

## Tracking upstream

Dependabot does not see this copy. Check the upstream releases and security advisories by hand when you review dependencies, and port anything relevant into the copy. There is no plan to offer the patches upstream for now.

## Race-detector steps

The BDAT bug was a data race, so these packages run under `-race` in CI: `internal/thirdparty/gosmtp`, `internal/bounce`, `internal/edgesession`, `internal/controlsession` and `cmd/thinpost-edge` (whose load test runs concurrent submissions beside port 25 traffic).

CI runs the step inside the `go` job, with the job's disposable PostgreSQL and S3 services, so the database-backed control session tests are raced too. It gates the image publish.

Run it locally with the command on the [Testing](/contributing/testing#race-detector) page.
