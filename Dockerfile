
# Frontend build stage
FROM oven/bun:latest AS frontend
WORKDIR /app/web
COPY web/package.json web/bun.lock* ./
RUN bun install
COPY web/ .
RUN bun run build

# Go build stage
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/martinsuchenak/skopos/build.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev) -X github.com/martinsuchenak/skopos/build.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /app/bin/skopos .

# Runtime stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates wget
WORKDIR /app
COPY --from=builder /app/bin/skopos .
COPY --from=builder /app/skopos-config.example.toml ./skopos-config.toml
RUN adduser -D -h /app skopos && chown -R skopos:skopos /app
USER skopos
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["./skopos"]
CMD ["serve"]
