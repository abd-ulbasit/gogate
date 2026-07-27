# syntax=docker/dockerfile:1

# =============================================================================
# Sluice - Production Multi-Stage Dockerfile
# =============================================================================
# Build stages:
#   1. build    - Compile Go binary with optimizations
#   2. runtime  - Minimal runtime image with security hardening
#
# Build: docker build -t sluice:latest .
# Run:   docker run -p 8080:8080 -p 8081:8081 -p 9090:9090 sluice:latest
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: Build
# -----------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /src

# Copy go mod files first (better layer caching)
COPY go.mod go.sum* ./
RUN go mod download && go mod verify

# Copy source code
COPY . .

# Build arguments for version injection
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME

# Build with optimizations:
#   - CGO disabled for static binary
#   - Strip debug info (-s -w) for smaller binary
#   - Version info via ldflags
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildTime=${BUILD_TIME}" \
    -trimpath \
    -o /sluice \
    ./cmd/sluice

# Verify binary was built
RUN test -f /sluice && /sluice -h || true

# -----------------------------------------------------------------------------
# Stage 2: Runtime (Distroless)
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Labels for container registry
LABEL org.opencontainers.image.title="Sluice"
LABEL org.opencontainers.image.description="L4/L7 Proxy & API Gateway with Service Discovery"
LABEL org.opencontainers.image.url="https://github.com/abd-ulbasit/sluice"
LABEL org.opencontainers.image.source="https://github.com/abd-ulbasit/sluice"
LABEL org.opencontainers.image.vendor="abd-ulbasit"
LABEL org.opencontainers.image.licenses="MIT"

# Copy timezone data and CA certificates for HTTPS
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary
COPY --from=build /sluice /sluice

# Copy default config (can be overridden via mount)
COPY config.example.yaml /etc/sluice/config.yaml

# Expose ports:
#   8080 - L4 TCP proxy
#   8081 - L7 HTTP proxy  
#   9090 - Admin API (health, stats, config)
#   9091 - Prometheus metrics
EXPOSE 8080 8081 9090 9091

# Health check - calls admin health endpoint
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/sluice", "-health-check"]

# Run as non-root user (distroless:nonroot UID=65532)
USER nonroot:nonroot

# Default entrypoint
ENTRYPOINT ["/sluice"]

# Default arguments (can be overridden)
CMD ["-config", "/etc/sluice/config.yaml"]
