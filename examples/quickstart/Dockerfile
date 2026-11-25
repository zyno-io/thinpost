# Images used only by the Docker Compose quickstart (see ../../compose.yaml).
# Nothing here is part of a release: the release images are the `control` and
# `edge` targets of the repository Dockerfile.

# Development-only OpenID Connect issuer. Refuses to start without
# --insecure-dev-only.
# golang:1.27-alpine (multi-arch index digest)
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS devidp-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY examples/quickstart/devidp/ examples/quickstart/devidp/
RUN CGO_ENABLED=0 go build -trimpath -o /out/devidp ./examples/quickstart/devidp

# gcr.io/distroless/static-debian13:nonroot
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS devidp
COPY --from=devidp-build /out/devidp /usr/local/bin/devidp
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/devidp"]

# Shell tooling for one-shot initialisation, the shared network namespace
# holder and the smoke test's helpers. It carries the release binaries (from
# the `control` and `edge` build contexts that compose supplies) so that the
# init steps run exactly the shipped commands.
# postgres:18-alpine
FROM postgres:18-alpine@sha256:b6972e1115a4ff8eba5d7f7d19184926d1e16d7d1136ae1e1a7ebf286da0f015 AS tools
RUN apk add --no-cache openssl curl jq busybox-static su-exec py3-dkimpy iptables
COPY --from=control /usr/local/bin/thinpost-control /usr/local/bin/thinpost-control
COPY --from=edge /usr/local/bin/thinpost-edge /usr/local/bin/thinpost-edge
ENTRYPOINT []
