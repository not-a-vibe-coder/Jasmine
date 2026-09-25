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
FROM alpine:3.21

WORKDIR /app

# Install CA certificates for HTTPS / TLS RPCs
RUN apk --no-cache add ca-certificates tzdata

# Copy compiled binary from builder
COPY --from=builder /app/bin/shipp /app/shipp

# Default port for health check
EXPOSE 8080

ENTRYPOINT ["/app/shipp"]
