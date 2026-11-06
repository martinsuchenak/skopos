# skopos server image, built from source. Releases publish a multi-arch image
# built from the release binaries instead (Dockerfile.release,
# ghcr.io/martinsuchenak/skopos). See docs/deployment/docker.md.
#
#   task docker
#   docker build --build-arg VERSION=$(git describe --tags --always) --build-arg COMMIT=$(git rev-parse --short HEAD) -t skopos .

# Frontend build stage
FROM oven/bun:1.4.4-alpine AS frontend
WORKDIR /app/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ .
RUN bun run build

# Go build stage
FROM golang:1.27.3-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/web/dist ./web/dist
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/martinsuchenak/skopos/build.Version=${VERSION} -X github.com/martinsuchenak/skopos/build.Commit=${COMMIT} -X github.com/martinsuchenak/skopos/build.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /app/bin/skopos .

# Runtime stage: keep in step with Dockerfile.release.
FROM alpine:3.24

# UID 1000: skopos.nomad runs Litestream as 1000:1000 on the same files.
# git: the code index's server-side refresher clones and pulls the
# registered workspaces (internal/codeindex/refresh.go).
RUN apk add --no-cache ca-certificates tzdata wget git \
    && adduser -D -u 1000 -h /app skopos

# On the PATH and outside /app, so a volume mounted at /app never shadows
# the binary with an older copy.
COPY --from=builder /app/bin/skopos /usr/local/bin/skopos
COPY --from=builder /app/LICENSE /usr/share/licenses/skopos/

WORKDIR /app
# Reference only: with no live skopos-config.toml the server runs from
# flags/env, and serve refuses to start keyless on a non-loopback bind unless
# SKOPOS_INSECURE_NO_API_KEY is explicitly set.
COPY --from=builder /app/skopos-config.example.toml .
RUN chown -R skopos:skopos /app

USER skopos

# The database (skopos.db) is written to /app; mount a volume there.
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["skopos"]
CMD ["serve"]
