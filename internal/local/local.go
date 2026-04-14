// Package local implements an MCP client that serves tools defined directly
// in the config file, without requiring an external process or network server.
//
// Each tool is either an exec command (runs a local binary) or an HTTP request
// (calls a remote URL). Tools are fixed at config time; callers cannot supply
// additional arguments.
//
// The Client satisfies the router.ChildClient interface and is always Ready.
package local

import (
	"context"
	"fmt"
	"time"

	"mcp-bridge/internal/config"
	"mcp-bridge/internal/mcp"
)

// Client exposes a set of locally-defined tools as an MCP client.
type Client struct {
	name           string
	tools          []config.LocalTool
	defaultTimeout time.Duration

	// toolIndex maps tool name → LocalTool for O(1) lookup in CallTool.
	toolIndex map[string]*config.LocalTool

	// ToolsRefreshed is called once during Initialize with the full tool list.
	// Wire this to router.Rebuild in main.go.
	ToolsRefreshed func(serverName string, tools []mcp.Tool)
}

// NewClient creates a local Client. defaultTimeout is used for any tool that
// does not declare its own timeout.
func NewClient(name string, tools []config.LocalTool, defaultTimeout time.Duration) *Client {
	idx := make(map[string]*config.LocalTool, len(tools))
	for i := range tools {
		idx[tools[i].Tool] = &tools[i]
	}
	return &Client{
		name:           name,
		tools:          tools,
		defaultTimeout: defaultTimeout,
		toolIndex:      idx,
	}
}

// Initialize builds the MCP tool list from config and fires ToolsRefreshed.
// It always succeeds — there is no remote connection to establish.
func (c *Client) Initialize(_ context.Context) error {
	mcpTools := make([]mcp.Tool, 0, len(c.tools))
	for _, t := range c.tools {
		mcpTools = append(mcpTools, mcp.Tool{
			Name:        t.Tool,
			Description: t.Description,
			InputSchema: emptyObjectSchema(),
		})
	}
	if c.ToolsRefreshed != nil {
		c.ToolsRefreshed(c.name, mcpTools)
	}
	return nil
}

// CallTool executes the named tool and returns its result.
// toolName is the un-prefixed name (the router strips the server prefix).
func (c *Client) CallTool(ctx context.Context, toolName string, _ map[string]any, _ map[string]string) (*mcp.ToolCallResult, error) {
	t, ok := c.toolIndex[toolName]
	if !ok {
		return nil, &mcp.RPCError{
			Code:    mcp.CodeToolNotFound,
			Message: fmt.Sprintf("local tool %q not found in server %q", toolName, c.name),
		}
	}

	timeout := c.defaultTimeout
	if t.Timeout != "" {
		if d, err := time.ParseDuration(t.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	tCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if t.Command != "" {
		return callExec(tCtx, t)
	}
	return callHTTP(tCtx, t)
}

// Ready always returns true — local tools need no connection.
func (c *Client) Ready() bool { return true }

// TerminateSession is a no-op for local tools.
func (c *Client) TerminateSession(_ context.Context, _ map[string]string) error { return nil }

// emptyObjectSchema returns the minimal JSON Schema for a tool that accepts
// no arguments: {"type":"object","properties":{}}.
func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
