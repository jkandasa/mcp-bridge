// Package network implements an MCP client that communicates with a remote
// MCP server over HTTP(S) using the MCP Streamable HTTP transport.
//
// The remote server may respond to POST requests with either:
//   - application/json  — a single JSON-RPC response body
//   - text/event-stream — an SSE stream containing zero or more
//     server-initiated notifications/requests followed by the final
//     JSON-RPC response for the originating request
//
// Session management (Mcp-Session-Id) is handled transparently:
//   - The session ID returned by the remote on initialize is stored and
//     sent on every subsequent request.
//   - Session IDs received from the parent client are forwarded to the
//     remote via the extraHeaders parameter on CallTool.
//
// Retry behaviour:
//   - If Initialize fails, a retryLoop goroutine retries at retryInterval.
//   - Every failure and every success is logged.
//
// Server-push (GET stream):
//   - After a successful initialize, a serverPushLoop goroutine opens a
//     GET connection to the remote to receive server-initiated notifications.
//   - notifications/tools/list_changed triggers an immediate tools/list refresh.
//   - All other notifications are logged at debug level.
//   - On disconnect, the loop reconnects after retryInterval.
package network

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"mcp-bridge/internal/logger"
	"mcp-bridge/internal/mcp"
)

const rediscoveryInterval = 5 * time.Minute

// Client is a network MCP client for one remote HTTP(S) MCP server.
type Client struct {
	name          string
	url           string
	configHeaders map[string]string // static headers from config (e.g. Authorization)
	retryInterval time.Duration
	httpClient    *http.Client

	idCounter atomic.Int64

	mu            sync.RWMutex
	tools         []mcp.Tool
	ready         bool
	sessionID     string             // Mcp-Session-Id returned by the remote on initialize
	sessionCancel context.CancelFunc // cancels the background loops for the current session

	// ToolsRefreshed is called (in a goroutine) after tools/list succeeds.
	ToolsRefreshed func(serverName string, tools []mcp.Tool)
}

// NewClient creates a Client. retryInterval controls reconnection cadence;
// requestTimeout is the per-request HTTP timeout.
func NewClient(name, url string, headers map[string]string, retryInterval, requestTimeout time.Duration) *Client {
	if retryInterval <= 0 {
		retryInterval = 30 * time.Second
	}
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	return &Client{
		name:          name,
		url:           url,
		configHeaders: headers,
		retryInterval: retryInterval,
		httpClient:    &http.Client{Timeout: requestTimeout},
	}
}

// ---------------------------------------------------------------------------
// Public interface (satisfies router.ChildClient)
// ---------------------------------------------------------------------------

// Ready reports whether the client has completed the MCP handshake.
func (c *Client) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// Initialize performs the MCP handshake with the remote server:
//  1. POST initialize — stores Mcp-Session-Id from response header if present.
//  2. POST notifications/initialized.
//  3. POST tools/list — caches the result.
//
// On failure, a retryLoop is started and nil is returned (non-fatal). The
// server will become ready once the retryLoop succeeds.
func (c *Client) Initialize(ctx context.Context) error {
	log := logger.L().With(zap.String("server", c.name))

	if err := c.doInitialize(ctx); err != nil {
		log.Warn("network server initialize failed; will retry",
			zap.String("url", c.url),
			zap.Duration("retry_interval", c.retryInterval),
			zap.Error(err),
		)
		go c.retryLoop(ctx)
		return nil
	}
	return nil
}

// CallTool sends tools/call to the remote server. toolName must be the
// original (un-prefixed) name. extraHeaders contains MCP-layer HTTP headers
// from the parent request (e.g. Mcp-Session-Id) to forward to the remote.
func (c *Client) CallTool(ctx context.Context, toolName string, arguments map[string]any, extraHeaders map[string]string) (*mcp.ToolCallResult, error) {
	c.mu.RLock()
	ready := c.ready
	c.mu.RUnlock()
	if !ready {
		return nil, fmt.Errorf("network server %q: not ready", c.name)
	}

	params := mcp.ToolCallParams{Name: toolName, Arguments: arguments}
	raw, _, err := c.doRequest(ctx, "tools/call", params, extraHeaders)
	if err != nil {
		return nil, err
	}

	var result mcp.ToolCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("network server %q: unmarshal tools/call result: %w", c.name, err)
	}
	return &result, nil
}

// TerminateSession sends DELETE to the remote server with the supplied
// MCP-layer headers. A 405 response (server does not support termination) is
// treated as success. Other non-2xx responses are logged as warnings.
func (c *Client) TerminateSession(ctx context.Context, headers map[string]string) error {
	log := logger.L().With(zap.String("server", c.name))

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil)
	if err != nil {
		return fmt.Errorf("network server %q: build DELETE request: %w", c.name, err)
	}
	c.applyHeaders(req, headers)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Warn("DELETE session failed", zap.Error(err))
		return nil // best-effort; don't propagate
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusMethodNotAllowed {
		log.Debug("remote server does not support session termination (405)")
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warn("DELETE session returned non-2xx", zap.Int("status", resp.StatusCode))
	} else {
		log.Info("session terminated", zap.Int("status", resp.StatusCode))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Initialization and retry
// ---------------------------------------------------------------------------

// doInitialize executes the full MCP handshake: initialize → notifications/initialized → tools/list.
func (c *Client) doInitialize(ctx context.Context) error {
	log := logger.L().With(zap.String("server", c.name))

	// 1. initialize
	initParams := mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      mcp.ClientInfo{Name: "go-mcp-bridge", Version: "1.0.0"},
	}
	raw, respHeaders, err := c.doRequest(ctx, "initialize", initParams, nil)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	var initResult mcp.InitializeResult
	if err := json.Unmarshal(raw, &initResult); err != nil {
		return fmt.Errorf("decode initialize result: %w", err)
	}

	// Store session ID if the remote provided one.
	if sid := respHeaders.Get(mcp.HeaderSessionID); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
		log.Debug("session established", zap.String("session_id", sid))
	}

	log.Info("connected to remote server",
		zap.String("url", c.url),
		zap.String("server", initResult.ServerInfo.Name),
		zap.String("version", initResult.ServerInfo.Version),
		zap.String("protocol", initResult.ProtocolVersion),
	)

	// 2. notifications/initialized (notification — no id, no response)
	if err := c.doNotify(ctx, "notifications/initialized"); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}

	// 3. tools/list
	if err := c.refreshTools(ctx); err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}

	// Cancel any background loops from a previous session before starting new
	// ones. This prevents goroutine accumulation when the connection drops and
	// reconnects multiple times.
	c.mu.Lock()
	if c.sessionCancel != nil {
		c.sessionCancel()
	}
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	c.sessionCancel = sessionCancel
	c.ready = true
	c.mu.Unlock()

	go c.rediscoveryLoop(sessionCtx)
	go c.serverPushLoop(sessionCtx)
	return nil
}

// retryLoop retries doInitialize at retryInterval until it succeeds or ctx is cancelled.
func (c *Client) retryLoop(ctx context.Context) {
	log := logger.L().With(zap.String("server", c.name))
	ticker := time.NewTicker(c.retryInterval)
	defer ticker.Stop()

	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			attempt++
			if err := c.doInitialize(ctx); err != nil {
				log.Warn("network server initialize retry failed",
					zap.String("url", c.url),
					zap.Int("attempt", attempt),
					zap.Error(err),
				)
			} else {
				log.Info("network server connected after retry",
					zap.String("url", c.url),
					zap.Int("attempts", attempt),
				)
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Background loops
// ---------------------------------------------------------------------------

// rediscoveryLoop periodically refreshes the tool list from the remote server.
func (c *Client) rediscoveryLoop(ctx context.Context) {
	log := logger.L().With(zap.String("server", c.name))
	ticker := time.NewTicker(rediscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.RLock()
			ready := c.ready
			c.mu.RUnlock()
			if !ready {
				continue
			}
			if err := c.refreshTools(ctx); err != nil {
				log.Warn("periodic tools/list failed", zap.Error(err))
			} else {
				log.Info("tools refreshed")
			}
		}
	}
}

// serverPushLoop opens a long-lived GET connection to the remote server and
// processes server-initiated SSE notifications.
func (c *Client) serverPushLoop(ctx context.Context) {
	log := logger.L().With(zap.String("server", c.name))

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := c.runPushStream(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("server push stream disconnected; reconnecting",
				zap.Duration("retry_interval", c.retryInterval),
				zap.Error(err),
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(c.retryInterval):
		}
	}
}

// runPushStream opens one GET connection and reads SSE events until it closes.
func (c *Client) runPushStream(ctx context.Context) error {
	log := logger.L().With(zap.String("server", c.name))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("build GET request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	c.applyHeaders(req, nil)

	// Use a client without a timeout for the long-lived GET stream.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusMethodNotAllowed {
		log.Debug("remote server does not support GET push stream (405)")
		return nil // not an error; server simply doesn't offer this
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET stream returned HTTP %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		return fmt.Errorf("GET stream returned unexpected Content-Type %q", ct)
	}

	log.Info("server push stream open")

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)

	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			// Blank line = end of one SSE event block.
			if len(dataLines) > 0 {
				c.handlePushEvent(ctx, strings.Join(dataLines, "\n"))
				dataLines = dataLines[:0]
			}
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			dataLines = append(dataLines, data)
		case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, ":"):
			// event type, event id, comment — no action needed
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read push stream: %w", err)
	}
	return nil // clean EOF
}

// handlePushEvent processes one SSE data payload from the server push stream.
func (c *Client) handlePushEvent(ctx context.Context, data string) {
	log := logger.L().With(zap.String("server", c.name))

	var req mcp.Request
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		log.Warn("invalid JSON in push event", zap.String("data", data), zap.Error(err))
		return
	}

	log.Debug("push event received", zap.String("method", req.Method))

	switch req.Method {
	case "notifications/tools/list_changed":
		log.Info("tools list changed notification received; refreshing tools")
		if err := c.refreshTools(ctx); err != nil {
			log.Warn("tools refresh after notification failed", zap.Error(err))
		} else {
			log.Info("tools refreshed after notification")
		}
	case "notifications/resources/list_changed":
		log.Info("resources list changed notification received (no action)")
	default:
		log.Debug("unhandled push notification", zap.String("method", req.Method))
	}
}

// ---------------------------------------------------------------------------
// Tool management
// ---------------------------------------------------------------------------

func (c *Client) refreshTools(ctx context.Context) error {
	log := logger.L().With(zap.String("server", c.name))

	raw, _, err := c.doRequest(ctx, "tools/list", nil, nil)
	if err != nil {
		return err
	}

	var result mcp.ToolsListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("decode tools/list result: %w", err)
	}

	c.mu.Lock()
	c.tools = result.Tools
	c.mu.Unlock()

	log.Info("tools discovered", zap.Int("count", len(result.Tools)))

	if c.ToolsRefreshed != nil {
		go c.ToolsRefreshed(c.name, result.Tools)
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP transport helpers
// ---------------------------------------------------------------------------

// doRequest sends a JSON-RPC request to the remote server and returns the
// decoded result bytes and the HTTP response headers.
// extraHeaders contains MCP-layer headers from the parent to forward.
func (c *Client) doRequest(ctx context.Context, method string, params any, extraHeaders map[string]string) (json.RawMessage, http.Header, error) {
	log := logger.L().With(zap.String("server", c.name))

	id := c.idCounter.Add(1)
	mcpID := mcp.NumberID(id)
	req := &mcp.Request{
		JSONRPC: mcp.JSONRPC,
		ID:      &mcpID,
		Method:  method,
	}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal params: %w", err)
		}
		req.Params = raw
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	log.Debug("network request", zap.String("body", string(body)))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	c.applyHeaders(httpReq, extraHeaders)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("POST %s: %w", c.url, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, httpResp.Header, fmt.Errorf("HTTP %d: %s", httpResp.StatusCode, strings.TrimSpace(string(b)))
	}

	ct := httpResp.Header.Get("Content-Type")
	var rpcResp *mcp.Response

	switch {
	case strings.HasPrefix(ct, "application/json"):
		var r mcp.Response
		if err := json.NewDecoder(httpResp.Body).Decode(&r); err != nil {
			return nil, httpResp.Header, fmt.Errorf("decode JSON response: %w", err)
		}
		rpcResp = &r

	case strings.HasPrefix(ct, "text/event-stream"):
		rpcResp, err = parseSSE(httpResp.Body, id)
		if err != nil {
			return nil, httpResp.Header, fmt.Errorf("parse SSE response: %w", err)
		}

	default:
		return nil, httpResp.Header, fmt.Errorf("unexpected Content-Type %q", ct)
	}

	respBytes, _ := json.Marshal(rpcResp)
	log.Debug("network response", zap.String("body", string(respBytes)))

	if rpcResp.Error != nil {
		return nil, httpResp.Header, rpcResp.Error
	}
	return rpcResp.Result, httpResp.Header, nil
}

// doNotify sends a JSON-RPC notification (no id, expects 202 Accepted).
func (c *Client) doNotify(ctx context.Context, method string) error {
	req := &mcp.Request{JSONRPC: mcp.JSONRPC, Method: method}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build notification request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.applyHeaders(httpReq, nil)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("POST notification %s: %w", method, err)
	}
	defer httpResp.Body.Close()
	_, _ = io.Copy(io.Discard, httpResp.Body)

	// 202 Accepted is the expected response for notifications.
	// Some servers return 200 — accept both.
	if httpResp.StatusCode != http.StatusAccepted && httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("notification %s: HTTP %d", method, httpResp.StatusCode)
	}
	return nil
}

// applyHeaders sets headers on an outgoing HTTP request in this order:
//  1. Stored session ID (set by the remote on initialize).
//  2. Config-level static headers (e.g. Authorization from config.yaml).
//  3. Extra (MCP-layer) headers forwarded from the parent client.
//
// Later entries override earlier ones for the same header name.
func (c *Client) applyHeaders(req *http.Request, extra map[string]string) {
	// 1. Session ID stored from the remote's initialize response.
	c.mu.RLock()
	sid := c.sessionID
	c.mu.RUnlock()
	if sid != "" {
		req.Header.Set(mcp.HeaderSessionID, sid)
	}

	// 2. Static config headers.
	for k, v := range c.configHeaders {
		req.Header.Set(k, v)
	}

	// 3. MCP-layer headers from the parent request (forwarded as-is).
	for k, v := range extra {
		req.Header.Set(k, v)
	}
}

// ---------------------------------------------------------------------------
// SSE parser
// ---------------------------------------------------------------------------

// parseSSE reads an SSE stream and returns the JSON-RPC response whose id
// matches requestID. Server-initiated notifications/requests that arrive
// before the final response are logged and discarded.
func parseSSE(body io.Reader, requestID int64) (*mcp.Response, error) {
	log := logger.L()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)

	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			// Blank line = end of SSE event.
			if len(dataLines) == 0 {
				continue
			}
			data := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]

			var resp mcp.Response
			if err := json.Unmarshal([]byte(data), &resp); err != nil {
				log.Warn("SSE: invalid JSON in data field", zap.String("data", data))
				continue
			}

			// If this message has no id it is a server-initiated notification.
			if resp.ID == nil {
				var req mcp.Request
				if jsonErr := json.Unmarshal([]byte(data), &req); jsonErr == nil {
					log.Debug("SSE: server notification before response",
						zap.String("method", req.Method))
				}
				continue
			}

			// Check whether this response matches our request id.
			idBytes, _ := json.Marshal(resp.ID)
			var numID int64
			if err := json.Unmarshal(idBytes, &numID); err == nil && numID == requestID {
				return &resp, nil
			}
			// Mismatched id — log and continue (shouldn't happen in practice).
			log.Warn("SSE: response id mismatch", zap.String("got", string(idBytes)))

		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			dataLines = append(dataLines, data)

		case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, ":"):
			// event type, event id, comment — no action needed
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without a matching response (id=%d)", requestID)
}
