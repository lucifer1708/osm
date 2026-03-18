# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Install build tools (needed for modernc.org/sqlite pure-Go, no CGO required)
RUN apk add --no-cache git

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a statically-linked binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o osm .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates needed for TLS calls to S3 endpoints
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S osm && adduser -S osm -G osm

WORKDIR /app

# Copy binary
COPY --from=builder /build/osm .

# Copy templates and static assets (read at runtime)
COPY --chown=osm:osm templates/ ./templates/
COPY --chown=osm:osm static/    ./static/

# Data directory — SQLite DB lives here; mount a volume to persist it
RUN mkdir -p /data && chown osm:osm /data

USER osm

EXPOSE 8080

ENV PORT=8080 \
    DB_PATH=/data/osm.db

ENTRYPOINT ["./osm"]
