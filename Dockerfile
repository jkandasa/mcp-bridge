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
      -X mcp-bridge/internal/version.Version=${VERSION} \
      -X mcp-bridge/internal/version.GitCommit=${GIT_COMMIT} \
      -X mcp-bridge/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/mcp-bridge \
    ./cmd/mcp-bridge

# ---- runtime stage ----
FROM scratch

# Copy the binary and a default config.
COPY --from=builder /out/mcp-bridge /usr/local/bin/mcp-bridge
COPY config.yaml /etc/mcp-bridge/config.yaml

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

ENTRYPOINT ["/usr/local/bin/mcp-bridge", "-config", "/etc/mcp-bridge/config.yaml"]
