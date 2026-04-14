# Changelog

All notable changes to mcp-bridge are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions correspond to Git tags (`v1.x.y`).

---

## [v1.0.0] — Unreleased

### Added
- Initial release of mcp-bridge.
- Bridges multiple MCP servers (stdio subprocess and remote HTTP/HTTPS) behind
  a single unified MCP HTTP endpoint.
- **Local server mode**: define exec commands and HTTP requests directly in
  config as MCP tools — no external process required. Per-tool timeout
  overrides a per-server default (both default to 30 s).
  - Exec tools: stdout and stderr returned as separate content items;
    non-zero exit → `isError: true`.
  - HTTP tools: response status code + body returned as content;
    non-2xx → `isError: true`.
- Tool namespacing: `<server>_<tool>`.
- MCP Streamable HTTP transport with SSE support.
- Session management (`Mcp-Session-Id`) proxied transparently.
- Server-push (long-lived GET stream) with automatic `tools/list` refresh on
  `notifications/tools/list_changed`.
- Configurable retry and request timeout per network server.
- `insecure` option for network MCP servers: set `insecure: true` to skip TLS
  certificate verification (useful for self-signed certificates).
- Optional Bearer token authentication on the bridge endpoint.
- TLS support: custom cert/key files or auto-generated self-signed certificate.
- Structured logging via `go.uber.org/zap`.
- Version details always printed at startup before config is loaded.
- `version` subcommand: `mcp-bridge version` prints version, git commit, build
  date, Go version, compiler, platform, and architecture.
- Build-time version stamping via `-ldflags -X` (`internal/version` package).
- GitHub Actions **release** workflow: triggered on `v1.*` tags; builds
  binaries for linux/amd64, linux/arm64, linux/armv7, windows/amd64,
  darwin/amd64, darwin/arm64; builds and pushes a multi-arch container image
  to GHCR; creates a GitHub Release with all binaries attached.
- GitHub Actions **devel** workflow: triggered on every push to `main`; runs
  `go vet`, builds a native binary, then builds and pushes the container image
  tagged as `main` to GHCR.
- OCI image labels (`org.opencontainers.image.*`) embedded in the container.
