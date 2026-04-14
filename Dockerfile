# ---- build stage ----
FROM golang:1.26-alpine AS builder

# Build-time version metadata injected by CI (or "dev" for local builds).
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Cache dependencies first (only re-run if go.mod/go.sum change).
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a statically-linked binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w \
      -X mcp-bridge/internal/version.version=${VERSION} \
      -X mcp-bridge/internal/version.gitCommit=${GIT_COMMIT} \
      -X mcp-bridge/internal/version.buildDate=${BUILD_DATE}" \
    -o /out/mcp-bridge \
    ./cmd

# ---- runtime stage ----
FROM alpine:3.23

# Install CA certificates for outbound TLS connections to remote MCP servers.
RUN apk add --no-cache ca-certificates tzdata

# Copy the binary.
COPY --from=builder /out/mcp-bridge /usr/local/bin/mcp-bridge

# OCI image labels.
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/jkandasa/mcp-bridge" \
      org.opencontainers.image.title="mcp-bridge" \
      org.opencontainers.image.description="A meta-MCP server that bridges multiple MCP servers"

# mcp-bridge listens on HTTP(S) — expose the default port.
EXPOSE 7575

# Config must be mounted at runtime, e.g.:
#   docker run -v /path/to/config.yaml:/etc/mcp-bridge/config.yaml mcp-bridge
ENTRYPOINT ["/usr/local/bin/mcp-bridge", "-config", "/etc/mcp-bridge/config.yaml"]
