// Package mcp defines the JSON-RPC 2.0 message types and MCP-specific
// protocol structures used for communication between the bridge and both
// its parent client (Claude Code) and its child/network MCP servers.
package mcp

import (
	"encoding/json"
	"fmt"
	"io"
)

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 base types
// ---------------------------------------------------------------------------

// JSONRPC is the fixed JSON-RPC version string required by the spec.
const JSONRPC = "2.0"

// ID represents a JSON-RPC request/response identifier, which may be a
// number, a string, or null (for notifications).
type ID struct {
	num    int64
	str    string
	isStr  bool
	isNull bool
}

// NumberID returns an ID backed by an integer.
func NumberID(n int64) ID { return ID{num: n} }

// StringID returns an ID backed by a string.
func StringID(s string) ID { return ID{str: s, isStr: true} }

// NullID returns a null ID (used for notifications).
func NullID() ID { return ID{isNull: true} }

func (id ID) MarshalJSON() ([]byte, error) {
	if id.isNull {
		return []byte("null"), nil
	}
	if id.isStr {
		return json.Marshal(id.str)
	}
	return json.Marshal(id.num)
}

func (id *ID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		id.isNull = true
		return nil
	}
	// Try number first.
	var n int64
	if err := json.Unmarshal(data, &n); err == nil {
		id.num = n
		return nil
	}
	// Fall back to string.
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	id.str = s
	id.isStr = true
	return nil
}

func (id ID) String() string {
	if id.isNull {
		return "null"
	}
	if id.isStr {
		return id.str
	}
	return fmt.Sprintf("%d", id.num)
}

// Request is a JSON-RPC 2.0 request or notification.
// Notifications have no ID field.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *ID             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// Standard JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// MCP application-level error codes (in the -32000 to -32099 range).
	CodeChildUnavailable = -32001
	CodeToolNotFound     = -32002
)

// NewErrorResponse builds a JSON-RPC error response for the given request ID.
func NewErrorResponse(id *ID, code int, msg string) *Response {
	return &Response{
		JSONRPC: JSONRPC,
		ID:      id,
		Error:   &RPCError{Code: code, Message: msg},
	}
}

// NewResultResponse builds a successful JSON-RPC response.
func NewResultResponse(id *ID, result any) (*Response, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &Response{JSONRPC: JSONRPC, ID: id, Result: raw}, nil
}

// ---------------------------------------------------------------------------
// MCP protocol structures
// ---------------------------------------------------------------------------

// ProtocolVersion is the MCP protocol version this bridge implements.
const ProtocolVersion = "2024-11-05"

// MCP-defined HTTP header names used in the Streamable HTTP transport.
const (
	// HeaderSessionID is set by the server on the initialize response and must
	// be echoed by the client on all subsequent requests within the session.
	HeaderSessionID = "Mcp-Session-Id"

	// HeaderProtocolVersion is sent by the client on all requests after
	// initialization to indicate the negotiated protocol version.
	HeaderProtocolVersion = "MCP-Protocol-Version"

	// HeaderLastEventID is sent by the client on a GET reconnection to indicate
	// the last SSE event ID it received, enabling stream resumption.
	HeaderLastEventID = "Last-Event-Id"
)

// ClientInfo identifies this wrapper to child MCP servers.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerInfo is returned by child servers in their initialize response.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeParams are sent in the initialize request to each child.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
}

// InitializeResult is what a child server returns for initialize.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
}

// Tool represents a single MCP tool as returned by tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// ToolsListResult is the response body for tools/list.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// ToolCallParams are the parameters for tools/call.
type ToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ContentItem is one element in a tool call result.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolCallResult is the response body for tools/call.
// When Stream is non-nil the result is being delivered as a server-sent event
// stream. The caller must copy Stream directly to the HTTP response as
// text/event-stream and close it when done. Content and IsError are unused
// in that case — they live inside the SSE events themselves.
type ToolCallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`

	// Stream is set (non-nil) only when the upstream server replied with
	// text/event-stream. The reader delivers raw SSE bytes that must be
	// forwarded verbatim to the parent client. Callers must close it.
	Stream io.ReadCloser `json:"-"`
}

// WrapperInfo identifies this bridge server to clients.
var WrapperInfo = ServerInfo{
	Name:    "go-mcp-bridge",
	Version: "1.0.0",
}
