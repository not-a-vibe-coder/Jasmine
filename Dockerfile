# Build Stage
FROM golang:1.24-alpine AS builder

WORKDIR /app
ENV GOTOOLCHAIN=auto

# Install build dependencies
RUN apk add --no-cache ca-certificates git

# Download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Compile static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/shipp cmd/bot/main.go

# Production Runner Stage
# Debian (glibc) rather than Alpine: the Telegram calling engine (ntgcalls) ships glibc wheels.
FROM python:3.12-slim-bookworm

WORKDIR /app

# CA certificates for HTTPS RPCs, bash for script validation, ffmpeg for voice notes and calls
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata bash ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# Telegram caller (only started when TG_CALLER_SESSION is set)
COPY caller/requirements.txt /app/caller/requirements.txt
RUN pip install --no-cache-dir -r /app/caller/requirements.txt
COPY caller/caller.py /app/caller/caller.py

# Copy compiled binary from builder
COPY --from=builder /app/bin/shipp /app/shipp
COPY docker-entrypoint.sh /app/docker-entrypoint.sh

# Default port for health check
EXPOSE 8080

ENTRYPOINT ["/app/docker-entrypoint.sh"]
