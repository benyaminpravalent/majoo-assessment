# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
# Pinned to the Go version this project was developed and tested against, so a
# new toolchain release cannot change the build under us.
FROM golang:1.24.3-alpine AS build

WORKDIR /src

# Copy the manifests first. This layer only changes when a dependency changes,
# so the module download is cached across ordinary source edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary, which is what lets the runtime stage
# be `scratch`-adjacent. The version is stamped into main.version so a running
# container can report exactly what it is.
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/blogapi ./cmd/api && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/blogapi-migrate ./cmd/migrate

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
# distroless/static is about 2 MB and contains no shell, no package manager and
# no libc — so a remote-code-execution bug has almost nothing to work with, and
# there is no apk database to accumulate CVEs between rebuilds. The trade-off is
# that you cannot `docker exec` a shell into it; use `kubectl debug` with an
# ephemeral container instead.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# distroless/static already ships a CA bundle at /etc/ssl/certs, so a TLS
# connection to a managed PostgreSQL instance works without copying one in.

COPY --from=build /out/blogapi /usr/local/bin/blogapi
COPY --from=build /out/blogapi-migrate /usr/local/bin/blogapi-migrate

# The distroless "nonroot" tag runs as uid 65532. Stating it explicitly means a
# base-image change cannot quietly promote the container to root.
USER 65532:65532

EXPOSE 8080

# There is no curl or wget in this image to probe with, so the binary probes
# itself: `blogapi -health-check` performs a local GET against /healthz and
# exits non-zero if it fails. In Kubernetes, prefer an httpGet probe and delete
# this instruction — the kubelet should do the asking.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/blogapi", "-health-check"]

ENTRYPOINT ["/usr/local/bin/blogapi"]
