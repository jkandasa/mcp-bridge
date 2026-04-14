# ---- build stage ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache dependencies first (only re-run if go.mod/go.sum change).
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a statically-linked binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/mcp-bridge \
    ./cmd/mcp-bridge

# ---- runtime stage ----
FROM scratch

# Copy the binary and a default config.
COPY --from=builder /out/mcp-bridge /usr/local/bin/mcp-bridge
COPY config.yaml /etc/mcp-bridge/config.yaml

# mcp-bridge listens on HTTP(S) — expose the default port.
EXPOSE 7575

ENTRYPOINT ["/usr/local/bin/mcp-bridge", "-config", "/etc/mcp-bridge/config.yaml"]
