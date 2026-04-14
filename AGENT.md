# mcp-bridge — Agent Instructions

This file is the authoritative guide for any AI coding agent working on this repository. Read it before making any changes.

---

## What this project is

**mcp-bridge** is a production-grade MCP (Model Context Protocol) bridge server written in Go. It:

1. Manages local stdio MCP server subprocesses AND remote HTTP(S) MCP servers defined in `config.yaml`
2. Aggregates all their tools under namespaced names (`<server>_<tool>`)
3. Exposes the unified tool set over a single HTTP(S) **Streamable HTTP MCP** endpoint (`/mcp` by default)

Parent clients (e.g. Claude Code) connect to mcp-bridge as a single MCP server. mcp-bridge fans out tool calls to the correct backend.

---

## Repository layout

```
cmd/main.go                     entry point — config, TLS, router wiring, signal handling
internal/
  config/config.go              YAML config types, Load(), validation, defaults
  logger/logger.go              zap initialisation (Init, L, Sync)
  tlsutil/selfsigned.go         SelfSigned() — in-memory ECDSA cert; FromFiles()
  version/version.go            build-time version metadata (singleton via sync.Once)
  child/
    process.go                  subprocess lifecycle, auto-restart with exponential back-off
    client.go                   stdio MCP client — Initialize, CallTool, readerLoop
  network/
    client.go                   HTTP/SSE MCP client — Initialize, CallTool, retryLoop, server-push
  router/router.go              ChildClient interface, routing table, TerminateAll
  mcp/
    protocol.go                 JSON-RPC types, MCP header constants, error codes
    server.go                   HTTP server: POST/GET(405)/DELETE, auth, dispatch
config_template.yaml            annotated config reference (both stdio and network examples)
config.yaml                     local working config — gitignored, not committed
Dockerfile                      multi-stage build (golang:1.26-alpine → alpine:3.23); EXPOSE 7575
Makefile                        build / run / vet / fmt / tidy / clean / version
.github/workflows/
  release.yml                   triggered on v1.* tags; builds binaries + container image + GitHub Release
  devel.yml                     triggered on main branch push; builds binaries + container image + devel pre-release
go.mod                          module: mcp-bridge; Go 1.26
CHANGELOG.md                    keep-a-changelog format; update before tagging a release
```

---

## Non-negotiable rules

- **All logging goes to stderr via zap** — never use `fmt.Print*` or the stdlib `log` package. Use `logger.L()` everywhere.
- **stdout is reserved** for MCP protocol messages in stdio child processes. Never write to stdout from the bridge itself.
- **Dependencies**: only stdlib + `go.uber.org/zap` + `gopkg.in/yaml.v3`. Do not add third-party MCP framework libraries.
- **After every backend change** run `go vet ./...` and `go build ./...` and fix all errors before finishing.
- **Format**: run `gofmt -w .` on any file you touch.
- Build flags: `-trimpath -ldflags="-s -w -X mcp-bridge/internal/version.version=... -X ..."` (enforced in Makefile and Dockerfile).
- **ldflags variable names are lowercase** (`version`, `gitCommit`, `buildDate`) — they map to unexported package-level vars in `internal/version/version.go`.

---

## MCP transport — key facts

### Streamable HTTP (what mcp-bridge exposes to parents)
- Single endpoint URL, three HTTP methods: `POST` (all JSON-RPC), `GET` (405 — not implemented), `DELETE` (session termination)
- Client sends `Accept: application/json, text/event-stream` on every POST
- Server responds with `application/json` (single response) or `text/event-stream` (SSE)
- `Mcp-Session-Id` assigned on `initialize`; client echoes on all subsequent requests
- `MCP-Protocol-Version` header required after init

### stdio transport (child processes)
- Newline-delimited JSON-RPC over stdin/stdout pipes
- No session, no SSE — those concepts exist only in Streamable HTTP
- Child stderr is forwarded to the bridge's stderr unchanged

### Header proxying
mcp-bridge extracts `Mcp-Session-Id`, `MCP-Protocol-Version`, and `Last-Event-Id` from the parent's request and forwards them to the remote network server. The constants are in `internal/mcp/protocol.go`.

---

## Configuration

### Config file location
Default: `config.yaml` in the working directory. Override with `-config /path/to/config.yaml`.

`config.yaml` is **gitignored** — never commit it. The annotated template is `config_template.yaml`.

### Key fields

```yaml
server:
  addr: ":7575"          # default listen address
  path: "/mcp"           # MCP endpoint path
  log_level: "info"      # debug | info | warn | error
  auth_token: ""         # optional Bearer token; empty = no auth
  data_dir: ".mcp-bridge"
  tls:
    auto_cert: false     # generate self-signed cert in memory
    cert_file: ""        # path to PEM cert (use with key_file)
    key_file: ""         # path to PEM key

servers:
  - name: myserver       # must be unique, no underscores
    command: /path/to/binary   # stdio mode
    args: []
    env:
      - KEY=VALUE

  - name: remote         # network mode
    url: http://host:9000/mcp
    headers:
      Authorization: "Bearer token"
    retry_interval: "30s"
    request_timeout: "30s"
    insecure: false      # set true to skip TLS cert verification (self-signed certs)
```

### Validation rules (enforced by `config.Load`)
- `server.name` must be unique and must not contain underscores
- Exactly one of `command` (stdio) or `url` (network) per server entry — they are mutually exclusive
- `retry_interval` and `request_timeout` must be valid positive Go duration strings
- `tls.cert_file` and `tls.key_file` must be set together
- At least one server must be configured

---

## Architecture decisions

### Tool namespacing
Every tool is exposed as `<server_name>_<original_tool_name>`. The router strips the prefix before forwarding. Server names must not contain underscores so the prefix boundary is unambiguous.

### Session ID handling
mcp-bridge is a pure proxy for `Mcp-Session-Id`:
- Network server returns session ID on `initialize` → stored per client, sent on every subsequent request to that remote
- Parent sends session ID → forwarded to the remote via `extraHeaders` in `CallTool`
- Parent sends `DELETE /mcp` → `TerminateAll` forwards it as `DELETE <remote-url>` with the session header

### Network client retry
On initial connection failure: `Initialize` returns nil (non-fatal) and a `retryLoop` starts in the background. Every failure and every success is logged. Retry interval is configurable per server.

### Goroutine lifecycle — network client
`doInitialize` assigns a **per-session context** (`sessionCtx`) derived from the app context. Each successful (re)connect cancels the previous session context before spawning new `rediscoveryLoop` and `serverPushLoop` goroutines. This prevents goroutine accumulation across reconnects.

### Goroutine lifecycle — stdio client
`client.readerLoop` tracks a **generation counter** (`readerGen`). Each `Initialize` increments the counter and passes the generation to the new reader goroutine. On exit, an old reader skips the pending-map drain if a newer generation has taken over, avoiding spurious errors on in-flight requests.

`NewClient` accepts an `appCtx context.Context` that is stored on the client and used in the `OnRestart` callback, ensuring restart goroutines are cancelled on shutdown.

### GET /mcp — Level 1 only
mcp-bridge opens a long-lived GET stream **to each network remote** to receive `notifications/tools/list_changed` and trigger tool refresh internally. It does **not** expose a GET push stream to the parent (returns 405). Full fan-out to parent is deferred.

---

## Security notes

- Bearer token comparison uses `crypto/subtle.ConstantTimeCompare` to prevent timing attacks
- Incoming POST body is limited to 4 MiB via `io.LimitReader` (`maxRequestBodyBytes` in `server.go`)
- Self-signed cert covers `localhost`, `127.0.0.1`, and `::1` as SANs

---

## Default port

**7575** — used everywhere: `config.go` defaults, `config_template.yaml`, `README.md`, `Dockerfile`, `cmd/main.go`.

---

## Common tasks

### Build
```sh
make build
```

### Check version
```sh
./mcp-bridge version
```

### Run locally
```sh
make run
# or
./mcp-bridge -config config.yaml
```

### Vet + format
```sh
make vet
make fmt
```

### Docker
```sh
docker build -t mcp-bridge .
docker run -p 7575:7575 -v $(pwd)/config.yaml:/etc/mcp-bridge/config.yaml mcp-bridge
```

### Release
1. Update `CHANGELOG.md` — change `Unreleased` to the release date under `## [vX.Y.Z]`
2. Commit and push to `main`
3. Tag: `git tag v1.x.y && git push origin v1.x.y`
4. The `release.yml` workflow handles the rest automatically

---

## Known limitations (intentional, not bugs)

- `GET /mcp` from parent returns 405 — server-push SSE fan-out to parent is not implemented
- Batch JSON-RPC requests are not supported
- `resources/*` and `prompts/*` MCP methods are not implemented
- `router.RemoveServer` exists but is never called — dynamic server removal is not wired up

---

## What has been audited and fixed

The following issues were identified by audit and fixed (as of the initial standalone repo commit):

| ID | File | Description |
|---|---|---|
| BUG-1 | `child/client.go` | Reader generation counter prevents old loops from clobbering pending entries after restart |
| BUG-2 | `network/client.go` | Per-session context cancellation prevents goroutine leak on reconnect |
| BUG-3 | `mcp/server.go` | Constant-time bearer token comparison |
| BUG-4 | `mcp/server.go` | 4 MiB body size cap via `io.LimitReader` |
| BUG-6 | `child/client.go` | `appCtx` stored on Client; `OnRestart` no longer uses `context.Background()` |
| INCON-1 | `router/router.go` | All logging uses zap (stdlib `log` removed) |
| INCON-5 | `Dockerfile` | Base image matches `go.mod` Go version |
| INCON-6 | `tlsutil/selfsigned.go` | `127.0.0.1` and `::1` IP SANs added to self-signed cert |
