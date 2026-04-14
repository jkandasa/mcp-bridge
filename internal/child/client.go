// Package child implements an MCP client that communicates with a child
// MCP server process over its stdin/stdout using the JSON-RPC 2.0 / MCP
// stdio transport protocol.
//
// Reader loop lifetime
// --------------------
// The reader loop is started once per call to Initialize (i.e. once at
// startup and once after each process restart). It is started BEFORE the
// first write to the child so that no response can be lost due to a race
// between goroutine scheduling and a fast-responding child.
//
// A small "loop ready" synchronisation channel is used: the caller blocks
// on it until the reader goroutine has entered its scan loop, then writes
// the first request.
package child

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"mcp-bridge/internal/logger"
	"mcp-bridge/internal/mcp"
)

const (
	callTimeout         = 30 * time.Second
	rediscoveryInterval = 5 * time.Minute
)

// Client is an MCP client for one child server process.
type Client struct {
	name    string
	process *Process
	appCtx  context.Context // application-level context; used in OnRestart

	idCounter atomic.Int64

	mu        sync.RWMutex
	tools     []mcp.Tool
	ready     bool
	readerGen uint64 // incremented on each new reader loop; old loops use this to avoid clobbering pending entries

	pendingMu sync.Mutex
	pending   map[int64]chan *mcp.Response

	// ToolsRefreshed is called (in a goroutine) after tools/list succeeds.
	ToolsRefreshed func(serverName string, tools []mcp.Tool)
}

// NewClient creates a Client for the given Process. appCtx is the
// application-level context; it is used in the auto-restart callback so that
// the re-initialisation goroutine is cancelled on shutdown rather than leaking.
func NewClient(name string, process *Process, appCtx context.Context) *Client {
	c := &Client{
		name:    name,
		process: process,
		appCtx:  appCtx,
		pending: make(map[int64]chan *mcp.Response),
	}
	process.OnRestart = func() {
		log := logger.L().With(zap.String("child", name))
		log.Info("process restarted – re-initialising")
		c.mu.Lock()
		c.ready = false
		c.mu.Unlock()
		if err := c.Initialize(c.appCtx); err != nil {
			log.Error("re-initialise failed", zap.Error(err))
		}
	}
	return c
}

// Initialize performs the MCP handshake with the child server:
//  1. Starts the reader loop and waits until it is ready to receive.
//  2. Sends initialize + notifications/initialized.
//  3. Calls tools/list and caches the result.
func (c *Client) Initialize(ctx context.Context) error {
	log := logger.L().With(zap.String("child", c.name))

	stdin, stdout := c.process.Pipes()
	if stdin == nil || stdout == nil {
		return fmt.Errorf("child %q: process not started", c.name)
	}

	// Bump the reader generation so any previous reader loop (still draining
	// its old pipe on restart) knows it is superseded and must not error-out
	// the pending entries that belong to the new reader.
	c.mu.Lock()
	c.readerGen++
	myGen := c.readerGen
	c.mu.Unlock()

	// Start the reader loop and block until it has entered its scan loop.
	// This guarantees the goroutine is reading before we send the first
	// request, so no response can arrive before the loop is ready.
	loopReady := make(chan struct{})
	go c.readerLoop(stdout, loopReady, myGen)
	select {
	case <-loopReady:
	case <-ctx.Done():
		return ctx.Err()
	}

	initParams := mcp.InitializeParams{
		ProtocolVersion: mcp.ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      mcp.ClientInfo{Name: "go-mcp-bridge", Version: "1.0.0"},
	}
	var initResult mcp.InitializeResult
	if err := c.call(ctx, stdin, "initialize", initParams, &initResult); err != nil {
		return fmt.Errorf("child %q: initialize: %w", c.name, err)
	}
	log.Info("connected to child server",
		zap.String("server", initResult.ServerInfo.Name),
		zap.String("version", initResult.ServerInfo.Version),
		zap.String("protocol", initResult.ProtocolVersion),
	)

	if err := c.notify(stdin, "notifications/initialized"); err != nil {
		return fmt.Errorf("child %q: notifications/initialized: %w", c.name, err)
	}

	if err := c.refreshTools(ctx, stdin); err != nil {
		return fmt.Errorf("child %q: tools/list: %w", c.name, err)
	}

	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()

	go c.rediscoveryLoop(ctx)
	return nil
}

// Tools returns a snapshot of the currently cached tool list.
func (c *Client) Tools() []mcp.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]mcp.Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// Ready reports whether the client has completed the MCP handshake.
func (c *Client) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// TerminateSession is a no-op for stdio clients — the stdio transport has no
// session concept. It exists to satisfy the router.ChildClient interface.
func (c *Client) TerminateSession(_ context.Context, _ map[string]string) error {
	return nil
}

// CallTool forwards a tools/call to the child server. toolName must be
// the original (un-prefixed) name. headers contains MCP-layer HTTP headers
// from the parent request; they are ignored for stdio transport.
func (c *Client) CallTool(ctx context.Context, toolName string, arguments map[string]any, _ map[string]string) (*mcp.ToolCallResult, error) {
	c.mu.RLock()
	ready := c.ready
	c.mu.RUnlock()
	if !ready {
		return nil, fmt.Errorf("child %q: not ready", c.name)
	}

	stdin, _ := c.process.Pipes()
	if stdin == nil {
		return nil, fmt.Errorf("child %q: process not running", c.name)
	}

	params := mcp.ToolCallParams{Name: toolName, Arguments: arguments}
	var result mcp.ToolCallResult
	if err := c.call(ctx, stdin, "tools/call", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// call sends a JSON-RPC request and blocks until the response arrives.
// params may be nil, in which case the params field is omitted from the wire
// message (not marshalled as JSON null).
func (c *Client) call(ctx context.Context, stdin io.Writer, method string, params any, result any) error {
	id := c.idCounter.Add(1)

	mcpID := mcp.NumberID(id)
	req := &mcp.Request{
		JSONRPC: mcp.JSONRPC,
		ID:      &mcpID,
		Method:  method,
	}
	// Only set Params when a value is actually provided to avoid sending
	// "params":null which some strict MCP servers reject.
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = raw
	}

	respCh := make(chan *mcp.Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	line, err := json.Marshal(req)
	if err != nil {
		c.removePending(id)
		return err
	}
	line = append(line, '\n')

	if _, err := stdin.Write(line); err != nil {
		c.removePending(id)
		return fmt.Errorf("write to child stdin: %w", err)
	}
	logger.L().Debug("child request",
		zap.String("child", c.name),
		zap.String("body", string(line[:len(line)-1])), // strip trailing newline
	)

	timeout := time.NewTimer(callTimeout)
	defer timeout.Stop()

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && resp.Result != nil {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-timeout.C:
		c.removePending(id)
		return fmt.Errorf("timeout waiting for response to %q (id=%d)", method, id)
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	}
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (c *Client) notify(stdin io.Writer, method string) error {
	req := &mcp.Request{JSONRPC: mcp.JSONRPC, Method: method}
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = stdin.Write(line)
	return err
}

// readerLoop continuously reads newline-delimited JSON-RPC messages from
// stdout and dispatches responses to pending call() waiters.
//
// loopReady is closed as soon as the goroutine has entered its scan loop,
// signalling to Initialize that it is safe to begin writing requests.
//
// gen is the reader generation assigned at spawn time. If the process
// restarts before this reader exits, a newer reader with a higher generation
// will take ownership of pending entries. This reader must NOT drain the
// pending map on exit when it has been superseded, to avoid erroring out
// requests that are already being served by the new reader.
func (c *Client) readerLoop(stdout io.Reader, loopReady chan struct{}, gen uint64) {
	log := logger.L().With(zap.String("child", c.name))
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)

	// Signal readiness immediately — the scanner is now blocking on the pipe.
	close(loopReady)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		log.Debug("child response", zap.String("body", string(line)))

		var resp mcp.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			log.Warn("invalid JSON from child", zap.Error(err))
			continue
		}
		if resp.ID == nil {
			// Server-initiated notification — ignore.
			continue
		}

		var rawID int64
		idBytes, _ := json.Marshal(resp.ID)
		if err := json.Unmarshal(idBytes, &rawID); err != nil {
			log.Warn("non-numeric response id, skipping")
			continue
		}

		c.pendingMu.Lock()
		ch, ok := c.pending[rawID]
		if ok {
			delete(c.pending, rawID)
		}
		c.pendingMu.Unlock()

		if ok {
			ch <- &resp
		} else {
			log.Warn("response for unknown request id", zap.Int64("id", rawID))
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		log.Error("reader loop error", zap.Error(err))
	}

	// Only drain pending entries if we are still the current reader generation.
	// If a newer reader has already taken over (process restarted), leave the
	// pending map alone — the new reader will dispatch or drain those entries.
	c.mu.RLock()
	current := c.readerGen
	c.mu.RUnlock()
	if gen != current {
		log.Debug("reader loop superseded by newer generation; skipping pending drain",
			zap.Uint64("gen", gen), zap.Uint64("current", current))
		return
	}

	// Unblock any pending callers so they don't block until their own timeout.
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		ch <- &mcp.Response{
			JSONRPC: mcp.JSONRPC,
			Error: &mcp.RPCError{
				Code:    mcp.CodeChildUnavailable,
				Message: fmt.Sprintf("child %q: reader loop exited", c.name),
			},
		}
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
}

func (c *Client) refreshTools(ctx context.Context, stdin io.Writer) error {
	log := logger.L().With(zap.String("child", c.name))
	var result mcp.ToolsListResult
	if err := c.call(ctx, stdin, "tools/list", nil, &result); err != nil {
		return err
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

func (c *Client) rediscoveryLoop(ctx context.Context) {
	log := logger.L().With(zap.String("child", c.name))
	ticker := time.NewTicker(rediscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.process.RestartCh():
			return
		case <-ticker.C:
			stdin, _ := c.process.Pipes()
			if stdin == nil {
				continue
			}
			c.mu.RLock()
			ready := c.ready
			c.mu.RUnlock()
			if !ready {
				continue
			}
			if err := c.refreshTools(ctx, stdin); err != nil {
				log.Warn("periodic tools/list failed", zap.Error(err))
			}
		}
	}
}

func (c *Client) removePending(id int64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}
