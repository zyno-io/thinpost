# Patches to go-smtp

Base: [emersion/go-smtp](https://github.com/emersion/go-smtp) **v0.26.0**
(MIT, see `LICENSE`). Copied files: `backend.go`, `client.go`, `conn.go`,
`data.go`, `lengthlimit_reader.go`, `parse.go`, `server.go`, `smtp.go`.
Dropped: upstream `_test.go` files (their fixtures use real domains), `cmd/`,
`README.md`, `go.mod`, `go.sum`.

Every change is marked with a `// thinpost:` comment. Everything else is
byte-identical to upstream.

## Upstream tracking

Dependabot no longer tracks this code. Check upstream releases and security
advisories by hand and port what applies. v0.26.0 changed the BDAT code
around these patches (see P1 below) but not `data.go`. No upstream PR is
planned for now.

## P1: BDAT worker isolation (`conn.go`)

**What.** Each BDAT transaction's `Data` runs in a goroutine. The goroutine now
uses only locals captured when the transfer starts (`session`, `status`,
`rcpts`, and its own `result` and `done` channels) instead of reading
`c.session`, `c.bdatStatus`, `c.recipients` and `c.dataResult` from the `Conn`
when it finishes. `Conn.bdatDone` is closed when the worker exits, including
the panic path. `reset()` now:

1. under the connection lock: takes `bdatDone` and closes the pipe;
2. unlocks and waits for `done`; after 30 s it calls `c.Close()` and returns
   without resetting the session;
3. only then re-locks and calls `session.Reset()`.

**Why.** Upstream's `reset()` closed the pipe and returned without waiting. After
`RSET`, the old worker's result could land on the next BDAT transaction's LAST
reply (the new worker then blocks forever), and the worker's field accesses
raced the next transaction. Joining before `Session.Reset()` also matters
because the MX session is unsynchronized: a zero-length non-final `BDAT 0` lets
`RSET` arrive before the worker has entered `Data`, so resetting first would
race the worker reading the session's transaction state.

**Reproduce on unpatched code.** Copy upstream v0.26.0 `conn.go` over
`conn.go` and run:

```sh
CGO_ENABLED=1 go test -race -count=1 -run BDAT ./internal/thirdparty/gosmtp
CGO_ENABLED=1 go test -race -count=1 -run MXBDAT ./internal/bounce
```

Recorded result when the patch was added: both fail on unpatched code. Without
`-race` the gosmtp tests fail within the first ten iterations with
`second transaction's final reply is 554 "5.0.0 first transaction result"`
(the first transaction's result delivered to the second); with `-race` they
report `DATA RACE` between `Conn.reset` and the BDAT worker. Both pass on the
patched code.

## P3: exact-limit DATA (`data.go`)

**What.** the data reader returned `ErrDataTooLarge` as soon as its remaining
count hit zero, before reading the terminating `.` line. At zero it now keeps
decoding (through the same dot-decoder state machine) into a one-byte scratch
buffer: the end-of-data terminator returns `io.EOF`; any further decoded byte
returns `ErrDataTooLarge` and is not emitted. A transfer cut short still returns
`io.ErrUnexpectedEOF`, as upstream does. Once returned, `ErrDataTooLarge` is sticky: every later
Read returns it again, as upstream's did, so a reader that retries can never
reach a clean EOF on a truncated body.

**Why.** A DATA message of exactly `MaxMessageBytes` was refused while BDAT
accepted it, so a bounce of exactly the bounce backend's `MaxBytes` was
rejected over DATA. Sizes below and above the limit behave as before.

**Tests.** `data_limit_test.go` (exact limit, dot-stuffed exact limit, limit+1,
truncated, sticky error) and `internal/bounce` `TestMXDataExactMaxBytes` (16 MiB exact is
accepted, +1 is 552). The exact-limit tests fail on unpatched `data.go`.
