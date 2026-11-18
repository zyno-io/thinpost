FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/thinpost-control ./cmd/thinpost-control && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/thinpost-edge ./cmd/thinpost-edge

# The operator console is built from the checked-in OpenAPI contract; the
# generated client is never taken from the build context. Only the control
# image receives its dist output.
FROM --platform=$BUILDPLATFORM node:26-alpine AS ui-build
RUN mkdir -p /src/api/admin /src/ui && chown -R node:node /src
USER node
WORKDIR /src/ui
COPY --chown=node:node ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY --chown=node:node api/admin/openapi.yaml /src/api/admin/openapi.yaml
COPY --chown=node:node ui/ ./
RUN npm run build

FROM gcr.io/distroless/static-debian13:nonroot AS runtime
USER 65532:65532

FROM runtime AS edge
COPY --from=build /out/thinpost-edge /usr/local/bin/thinpost-edge
ENTRYPOINT ["/usr/local/bin/thinpost-edge"]

FROM runtime AS control
COPY --from=build /out/thinpost-control /usr/local/bin/thinpost-control
# Root-owned, so the non-root runtime user can read but not modify it.
COPY --from=ui-build --chown=0:0 /src/ui/dist/ /usr/share/thinpost/ui/
ENV THINPOST_UI_DIR=/usr/share/thinpost/ui
ENTRYPOINT ["/usr/local/bin/thinpost-control"]
