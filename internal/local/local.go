// Package local implements an MCP client that serves tools defined directly
// in the config file, without requiring an external process or network server.
//
// Each tool is either an exec command (runs a local binary) or an HTTP request
// (calls a remote URL). Named parameters ({{name}} placeholders) are declared
// in config; tools without params accept no runtime arguments.
//
// The Client satisfies the router.ChildClient interface and is always Ready.
package local

import (
	"context"
	"fmt"
	"strings"
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
			InputSchema: inputSchemaForTool(t),
		})
	}
	if c.ToolsRefreshed != nil {
		c.ToolsRefreshed(c.name, mcpTools)
	}
	return nil
}

// CallTool executes the named tool and returns its result.
// toolName is the un-prefixed name (the router strips the server prefix).
func (c *Client) CallTool(ctx context.Context, toolName string, arguments map[string]any, _ map[string]string) (*mcp.ToolCallResult, error) {
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

	if !t.Command.IsEmpty() {
		return callExec(tCtx, t, arguments)
	}
	return callHTTP(tCtx, t, arguments)
}

// Ready always returns true — local tools need no connection.
func (c *Client) Ready() bool { return true }

// TerminateSession is a no-op for local tools.
func (c *Client) TerminateSession(_ context.Context, _ map[string]string) error { return nil }

// inputSchemaForTool returns the MCP input schema for a local tool.
// Tools with params get a typed schema; all others accept no arguments.
func inputSchemaForTool(t config.LocalTool) map[string]any {
	if len(t.Params) > 0 {
		return localParamSchema(t.Params)
	}
	return emptyObjectSchema()
}

// emptyObjectSchema returns the minimal JSON Schema for a tool that accepts
// no arguments: {"type":"object","properties":{}}.
func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

// localParamSchema builds a JSON Schema object from a list of LocalParams.
func localParamSchema(params []config.LocalParam) map[string]any {
	properties := make(map[string]any, len(params))
	required := make([]string, 0)

	for _, p := range params {
		prop := map[string]any{}
		switch p.Type {
		case "array":
			prop["type"] = "array"
			prop["items"] = map[string]any{"type": "string"}
		case "integer":
			prop["type"] = "integer"
		case "number":
			prop["type"] = "number"
		case "boolean":
			prop["type"] = "boolean"
		default: // "string" or empty
			prop["type"] = "string"
		}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		properties[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// expandString replaces all {{name}} placeholders in s with their string
// representations from values. Arrays are joined with commas.
func expandString(s string, values map[string]any) string {
	for name, val := range values {
		placeholder := "{{" + name + "}}"
		if !strings.Contains(s, placeholder) {
			continue
		}
		var replacement string
		switch v := val.(type) {
		case []any:
			parts := make([]string, 0, len(v))
			for _, item := range v {
				parts = append(parts, fmt.Sprintf("%v", item))
			}
			replacement = strings.Join(parts, ",")
		case []string:
			replacement = strings.Join(v, ",")
		default:
			replacement = fmt.Sprintf("%v", val)
		}
		s = strings.ReplaceAll(s, placeholder, replacement)
	}
	return s
}

// expandTokens expands a slice of command tokens against the arguments map.
// A token that is exactly "{{name}}" (standalone placeholder) and whose value
// is an array expands into multiple tokens — one per element.
// All other tokens undergo plain string replacement via expandString.
func expandTokens(tokens []string, values map[string]any) []string {
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if name, ok := standaloneParam(tok); ok {
			if val, found := values[name]; found {
				switch v := val.(type) {
				case []any:
					for _, item := range v {
						out = append(out, fmt.Sprintf("%v", item))
					}
					continue
				case []string:
					out = append(out, v...)
					continue
				}
			}
		}
		out = append(out, expandString(tok, values))
	}
	return out
}

// standaloneParam reports whether tok is exactly "{{name}}" and returns the name.
func standaloneParam(tok string) (string, bool) {
	if len(tok) < 5 || !strings.HasPrefix(tok, "{{") || !strings.HasSuffix(tok, "}}") {
		return "", false
	}
	inner := tok[2 : len(tok)-2]
	if strings.ContainsAny(inner, "{}") {
		return "", false
	}
	return inner, true
}
