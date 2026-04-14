// Package router maps unified (prefixed) tool names to the correct child MCP
// client and the original (un-prefixed) tool name.
//
// Naming convention:
//
//	<server_name>_<original_tool_name>
//
// Example:
//
//	config server name : "git"
//	child tool name    : "status"
//	unified name       : "git_status"
//
// The separator is always the first underscore that directly follows a known
// server-name prefix. This allows child tool names to themselves contain
// underscores (e.g. "git_read_file" where "git" is the server prefix and
// "read_file" is the original tool name).
//
// Thread safety:
//
//	All exported methods are safe for concurrent use. The routing table is
//	rebuilt atomically via a pointer swap protected by a mutex.
package router

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"

	"mcp-bridge/internal/logger"
	"mcp-bridge/internal/mcp"
)

// entry stores the resolved route for one unified tool name.
type entry struct {
	client       ChildClient
	originalName string // tool name as known by the child server
	tool         mcp.Tool
}

// ChildClient is the interface the router uses to call into a child server.
// Both *child.Client and *network.Client satisfy this interface.
type ChildClient interface {
	// CallTool forwards a tools/call to the server. toolName must be the
	// original (un-prefixed) name. headers contains MCP-layer HTTP headers
	// from the parent request (e.g. Mcp-Session-Id) to be forwarded.
	CallTool(ctx context.Context, toolName string, arguments map[string]any, headers map[string]string) (*mcp.ToolCallResult, error)

	// TerminateSession sends a session termination signal (DELETE) to the
	// server. For stdio clients this is a no-op. headers contains the
	// MCP-layer headers from the parent DELETE request.
	TerminateSession(ctx context.Context, headers map[string]string) error

	// Ready reports whether the client has completed the MCP handshake.
	Ready() bool
}

// Router aggregates tools from all child MCP clients and routes incoming
// tools/call requests to the appropriate child.
type Router struct {
	mu      sync.RWMutex
	table   map[string]*entry // unified name → route
	tools   []mcp.Tool        // merged list in insertion order
	clients []ChildClient     // deduplicated list for TerminateAll
}

// New creates an empty Router.
func New() *Router {
	return &Router{
		table: make(map[string]*entry),
	}
}

// Rebuild replaces the tool routing table for one server entirely.
// It is called after a successful tools/list (on startup and after restart).
// Tools from other servers are preserved.
func (r *Router) Rebuild(serverName string, tools []mcp.Tool, client ChildClient) {
	prefix := serverName + "_"

	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove all existing entries that belong to this server.
	for unified := range r.table {
		if strings.HasPrefix(unified, prefix) {
			delete(r.table, unified)
		}
	}

	// Add the new entries.
	for _, t := range tools {
		unified := prefix + t.Name
		exposed := mcp.Tool{
			Name:        unified,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
		r.table[unified] = &entry{
			client:       client,
			originalName: t.Name,
			tool:         exposed,
		}
	}

	// Track the client for TerminateAll (deduplicated).
	found := false
	for _, c := range r.clients {
		if c == client {
			found = true
			break
		}
	}
	if !found {
		r.clients = append(r.clients, client)
	}

	// Rebuild the ordered merged slice for tools/list responses.
	r.rebuildList()
	logger.L().Info("registered tools for server",
		zap.Int("count", len(tools)),
		zap.String("server", serverName),
	)
}

// RemoveServer removes all tools belonging to serverName from the table.
// Called when a server is permanently stopped.
func (r *Router) RemoveServer(serverName string) {
	prefix := serverName + "_"
	r.mu.Lock()
	defer r.mu.Unlock()
	for unified := range r.table {
		if strings.HasPrefix(unified, prefix) {
			delete(r.table, unified)
		}
	}
	r.rebuildList()
}

// Tools returns the current merged tool list (safe to call concurrently).
func (r *Router) Tools() []mcp.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]mcp.Tool, len(r.tools))
	copy(out, r.tools)
	return out
}

// Call routes a tools/call request with the unified tool name to the correct
// child server, strips the prefix, and returns the child's result.
// headers contains MCP-layer HTTP headers from the parent request to forward.
func (r *Router) Call(ctx context.Context, unifiedName string, arguments map[string]any, headers map[string]string) (*mcp.ToolCallResult, error) {
	r.mu.RLock()
	e, ok := r.table[unifiedName]
	r.mu.RUnlock()

	if !ok {
		return nil, &mcp.RPCError{
			Code:    mcp.CodeToolNotFound,
			Message: fmt.Sprintf("unknown tool %q", unifiedName),
		}
	}
	if !e.client.Ready() {
		return nil, &mcp.RPCError{
			Code:    mcp.CodeChildUnavailable,
			Message: fmt.Sprintf("child MCP server for tool %q is unavailable", unifiedName),
		}
	}

	return e.client.CallTool(ctx, e.originalName, arguments, headers)
}

// TerminateAll calls TerminateSession on every registered client.
// Used when the parent sends DELETE /mcp. Errors are logged, not returned.
func (r *Router) TerminateAll(ctx context.Context, headers map[string]string) {
	r.mu.RLock()
	clients := make([]ChildClient, len(r.clients))
	copy(clients, r.clients)
	r.mu.RUnlock()

	for _, c := range clients {
		if err := c.TerminateSession(ctx, headers); err != nil {
			logger.L().Error("TerminateSession failed",
				zap.Error(err),
			)
		}
	}
}

// rebuildList regenerates r.tools from r.table in a stable order.
// Must be called with r.mu held.
func (r *Router) rebuildList() {
	tools := make([]mcp.Tool, 0, len(r.table))
	for _, e := range r.table {
		tools = append(tools, e.tool)
	}
	// Sort by unified name for deterministic output.
	for i := 1; i < len(tools); i++ {
		for j := i; j > 0 && tools[j].Name < tools[j-1].Name; j-- {
			tools[j], tools[j-1] = tools[j-1], tools[j]
		}
	}
	r.tools = tools
}
