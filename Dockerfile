# syntax=docker/dockerfile:1

# =============================================================================
# GoGate - Production Multi-Stage Dockerfile
# =============================================================================
# Build stages:
#   1. build    - Compile Go binary with optimizations
#   2. runtime  - Minimal runtime image with security hardening
#
# Build: docker build -t gogate:latest .
# Run:   docker run -p 8080:8080 -p 8081:8081 -p 9090:9090 gogate:latest
# =============================================================================

# -----------------------------------------------------------------------------
# Stage 1: Build
# -----------------------------------------------------------------------------
FROM golang:1.22-alpine AS build

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
    -o /gogate \
    ./cmd/gogate

# Verify binary was built
RUN test -f /gogate && /gogate -h || true

# -----------------------------------------------------------------------------
# Stage 2: Runtime (Distroless)
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Labels for container registry
LABEL org.opencontainers.image.title="GoGate"
LABEL org.opencontainers.image.description="L4/L7 Proxy & API Gateway with Service Discovery"
LABEL org.opencontainers.image.url="https://github.com/abd-ulbasit/gogate"
LABEL org.opencontainers.image.source="https://github.com/abd-ulbasit/gogate"
LABEL org.opencontainers.image.vendor="abd-ulbasit"
LABEL org.opencontainers.image.licenses="MIT"

# Copy timezone data and CA certificates for HTTPS
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary
COPY --from=build /gogate /gogate

# Copy default config (can be overridden via mount)
COPY config.example.yaml /etc/gogate/config.yaml

# Expose ports:
#   8080 - L4 TCP proxy
#   8081 - L7 HTTP proxy  
#   9090 - Admin API (health, stats, config)
#   9091 - Prometheus metrics
EXPOSE 8080 8081 9090 9091

# Health check - calls admin health endpoint
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/gogate", "-health-check"]

# Run as non-root user (distroless:nonroot UID=65532)
USER nonroot:nonroot

# Default entrypoint
ENTRYPOINT ["/gogate"]

# Default arguments (can be overridden)
CMD ["-config", "/etc/gogate/config.yaml"]
