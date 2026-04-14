# mcp-bridge

[![SafeSkill 93/100](https://img.shields.io/badge/SafeSkill-93%2F100_Verified%20Safe-brightgreen)](https://safeskill.dev/scan/jkandasa-mcp-bridge)
A production-grade MCP bridge server that aggregates any number of MCP servers
— local stdio binaries **and** remote HTTP(S) network servers — and exposes
them all through a single unified HTTP(S) endpoint.

```
AI client (Claude Code, etc.)
    │  POST/DELETE /mcp  (Streamable HTTP transport)
    ▼
mcp-bridge  (:7575/mcp by default)
    ├── filesystem-mcp-server  (subprocess · stdio)
    ├── git-mcp-server         (subprocess · stdio)
    └── https://api.example.com/mcp  (network · HTTP/SSE)
```

Child servers speak either **stdio** (JSON-RPC over stdin/stdout) or
**Streamable HTTP** (the same MCP transport the parent uses). mcp-bridge
manages their lifecycle and re-exposes all their tools through one endpoint
that any MCP client can reach over the network.

---

## Features

- **Two server modes** — stdio subprocesses and remote HTTP(S) servers in the
  same config file; mix and match freely.
- **Full MCP Streamable HTTP transport** — POST, DELETE, and transparent
  `Mcp-Session-Id` / `MCP-Protocol-Version` header proxying.
- **SSE response handling** — remote servers may reply with
  `text/event-stream`; mcp-bridge parses the SSE stream and extracts the
  JSON-RPC result transparently.
- **Server-push notifications** — opens a long-lived GET stream to each
  remote server; `notifications/tools/list_changed` triggers an automatic
  `tools/list` refresh.
- **Tool namespacing** — tools are prefixed with the server name to avoid
  conflicts: `git_status`, `filesystem_read_file`, `remote_search`, etc.
- **Auto-restart** — crashed stdio subprocesses are restarted with exponential
  back-off (500 ms → 30 s cap).
- **Retry on connect** — unreachable network servers are retried in the
  background; configurable interval.
- **Periodic tool rediscovery** — `tools/list` is re-fetched every 5 minutes
  from all servers.
- **Bearer token auth** — optional incoming auth on the bridge endpoint.
- **TLS / HTTPS** — custom PEM certs or auto-generated self-signed cert.
- **Structured logging** — via [zap](https://github.com/uber-go/zap); raw
  JSON-RPC bodies logged at DEBUG level.

---

## Quick start

```bash
# Build
go build -trimpath -ldflags="-s -w" -o mcp-bridge ./cmd/mcp-bridge

# Create config
cp config_template.yaml config.yaml
$EDITOR config.yaml

# Run
./mcp-bridge -config config.yaml
```

### Minimal config (stdio only)

```yaml
server:
  addr: ":7575"

servers:
  - name: git
    command: /usr/local/bin/git-mcp-server
```

### Mixed config (stdio + network)

```yaml
server:
  addr: ":7575"
  log_level: "info"

servers:
  - name: git
    command: /usr/local/bin/git-mcp-server

  - name: remote
    url: http://remote-host:9000/mcp
    headers:
      Authorization: "Bearer secret-token"
    retry_interval: "30s"
    request_timeout: "30s"
```

---

## Configuration reference

### `server` section

| Field | Default | Description |
|-------|---------|-------------|
| `data_dir` | `./.mcp-bridge` | Directory for persistent data (TLS certs, etc.) |
| `addr` | `:7575` | TCP listen address (`[host]:port`) |
| `path` | `/mcp` | HTTP path for the MCP endpoint |
| `log_level` | `info` | Minimum log severity: `debug`, `info`, `warn`, `error` |
| `auth_token` | _(empty)_ | Bearer token required on every request; empty = open |
| `tls.cert_file` | _(empty)_ | Path to PEM certificate (requires `key_file`) |
| `tls.key_file` | _(empty)_ | Path to PEM private key (requires `cert_file`) |
| `tls.auto_cert` | `false` | Generate a self-signed P-256 cert in memory at startup |

### `servers` entries — stdio mode

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Unique prefix for this server's tools (no underscores) |
| `command` | yes | Path to the MCP server binary |
| `args` | no | Command-line arguments passed to the binary |
| `env` | no | `KEY=VALUE` pairs added to the child's environment |

### `servers` entries — network mode

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `name` | yes | — | Unique prefix for this server's tools (no underscores) |
| `url` | yes | — | HTTP(S) MCP endpoint, e.g. `http://host:9000/mcp` |
| `headers` | no | _(empty)_ | HTTP headers sent on every request (auth, API keys, etc.) |
| `retry_interval` | no | `30s` | Delay between reconnection attempts when unreachable |
| `request_timeout` | no | `30s` | Per-request HTTP timeout |

`command` and `url` are mutually exclusive — exactly one must be set per entry.

---

## MCP transport details

### What mcp-bridge exposes (parent-facing)

| HTTP method | Behaviour |
|-------------|-----------|
| `POST /mcp` | JSON-RPC dispatch: `initialize`, `tools/list`, `tools/call`, `ping`, notifications |
| `DELETE /mcp` | Session termination — forwarded to all registered servers |
| `GET /mcp` | `405 Method Not Allowed` — server-push SSE to the parent is not implemented |

### Header proxying

The following MCP-layer headers are extracted from every parent request and
forwarded to the downstream server (network mode only):

| Header | Direction | Purpose |
|--------|-----------|---------|
| `Mcp-Session-Id` | parent → remote | Session identity |
| `MCP-Protocol-Version` | parent → remote | Negotiated protocol version |
| `Last-Event-Id` | parent → remote | SSE stream resumption cursor |
| `Mcp-Session-Id` | remote → parent | Session ID assigned on `initialize` |

Transport-level headers (`Content-Type`, `Authorization`, etc.) are **not**
forwarded between parent and remote — only the MCP semantic headers above.

### SSE responses

When a remote network server responds with `Content-Type: text/event-stream`,
mcp-bridge reads the SSE stream line by line:

- Server-initiated notifications/requests that arrive before the final response
  are logged at DEBUG level.
- The event containing the matching JSON-RPC response is extracted and returned
  to the parent as a plain `application/json` response.

### Server-push notifications (GET stream)

After a successful `initialize` with a network server, mcp-bridge opens a
long-lived GET connection to receive server-initiated notifications:

- `notifications/tools/list_changed` → triggers an immediate `tools/list` refresh
- `notifications/resources/list_changed` → logged (no action)
- All other notifications → logged at DEBUG level
- On disconnect → reconnects after `retry_interval`

---

## Tool naming

Tools are namespaced as `<server_name>_<original_tool_name>`:

```
server name: "git"    original tool: "status"    → unified: "git_status"
server name: "remote" original tool: "web_search" → unified: "remote_web_search"
```

Server names must not contain underscores. Original tool names may contain
underscores freely.

---

## Client configuration

### Claude Code — plain HTTP

```json
{
  "mcpServers": {
    "bridge": {
      "url": "http://localhost:7575/mcp"
    }
  }
}
```

### Claude Code — HTTPS with auth

```json
{
  "mcpServers": {
    "bridge": {
      "url": "https://myserver:7575/mcp",
      "headers": {
        "Authorization": "Bearer your-secret-token"
      }
    }
  }
}
```

---

## Testing with curl

```bash
# tools/list
curl -s -X POST http://localhost:7575/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'

# tools/call
curl -s -X POST http://localhost:7575/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"git_status","arguments":{}}}'

# ping
curl -s -X POST http://localhost:7575/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":3,"method":"ping","params":{}}'

# terminate session
curl -s -X DELETE http://localhost:7575/mcp \
  -H "Mcp-Session-Id: abc123"
```

---

## Docker

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /mcp-bridge ./cmd/mcp-bridge

FROM alpine:3.19
COPY --from=builder /mcp-bridge /usr/local/bin/mcp-bridge
EXPOSE 7575
ENTRYPOINT ["mcp-bridge", "-config", "/etc/mcp-bridge/config.yaml"]
```

```bash
# Build the image
docker build -t mcp-bridge .

# Run — mount your config file
docker run --rm -p 7575:7575 \
  -v /path/to/config.yaml:/etc/mcp-bridge/config.yaml \
  mcp-bridge

# With a persistent data directory
docker run --rm -p 7575:7575 \
  -v /path/to/config.yaml:/etc/mcp-bridge/config.yaml \
  -v /path/to/data:/var/lib/mcp-bridge \
  mcp-bridge
```

---

## Architecture

```
Parent client (Claude Code, etc.)
        │
        │  POST /mcp  application/json
        │  DELETE /mcp  (session termination)
        ▼
┌─────────────────────────────────────────┐
│             mcp/Server                  │
│  • auth middleware                      │
│  • MCP header extraction & forwarding  │
│  • JSON-RPC dispatcher                  │
│    initialize / ping / tools/list       │
│    tools/call → router.Call()           │
│    DELETE    → router.TerminateAll()    │
└──────────────┬──────────────────────────┘
               │
        router.Router
        (unified name → client mapping)
               │
       ┌───────┴──────────┐
       │                  │
child.Client         network.Client
(stdio transport)    (HTTP/SSE transport)
       │                  │
  subprocess          remote server
  (auto-restart)      (retry on disconnect)
                      (GET push stream)
```

---

## What is not implemented

| Feature | Notes |
|---------|-------|
| GET /mcp SSE push to parent | Level 1 only: bridge consumes remote push internally |
| SSE stream resumption | `Last-Event-Id` is forwarded but bridge does not replay missed events |
| `resources/*`, `prompts/*` | Not advertised in capabilities; methods pass through POST transparently |
| Batch JSON-RPC | Single request per POST |
| String JSON-RPC IDs from stdio children | Only numeric IDs supported in stdio responses |
