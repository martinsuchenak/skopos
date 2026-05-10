
# Frontend build stage
FROM oven/bun:latest AS frontend
WORKDIR /app/web
COPY web/package.json web/bun.lockb* ./
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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/skopos .

# Runtime stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/bin/skopos .
COPY --from=builder /app/skopos-config.toml .
EXPOSE 8080
ENTRYPOINT ["./skopos"]
CMD ["serve"]
