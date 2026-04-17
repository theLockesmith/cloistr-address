# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go mod files first for caching
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY cmd/ cmd/
COPY internal/ internal/

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o cloistr-address ./cmd/address

# Runtime stage
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata postgresql16-client

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/cloistr-address .

# Copy migrations (optional, for debugging)
COPY db/ db/

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q --spider http://localhost:8080/health || exit 1

# Default port
EXPOSE 8080

# Run as non-root
RUN adduser -D -u 1000 appuser
USER appuser

ENTRYPOINT ["./cloistr-address"]
