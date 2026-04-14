// Package mcp implements the bridge's own MCP server endpoint, exposed over
// HTTP using the MCP Streamable HTTP transport.
//
// The parent client (e.g. Claude Code) connects via HTTP or HTTPS:
//
//	{ "mcpServers": { "bridge": { "url": "http://localhost:7575/mcp" } } }
//
// Supported HTTP methods on the MCP endpoint:
//
//	POST   — send any JSON-RPC request or notification; response is
//	          application/json (single object).
//	GET    — returns 405; server-push SSE to the parent is not implemented
//	          (Level 1: mcp-bridge consumes remote push internally only).
//	DELETE — terminate an MCP session; forwarded to all registered clients.
//
// MCP-layer HTTP headers are proxied between the parent and remote servers:
//
//	Mcp-Session-Id        — forwarded on requests; returned on responses
//	MCP-Protocol-Version  — forwarded on requests
//	Last-Event-Id         — forwarded on requests (GET reconnection)
//
// The JSON-RPC dispatcher handles:
//
//	initialize              MCP handshake
//	notifications/*         Accepted silently (202 No Content)
//	tools/list              Returns aggregated tool list
//	tools/call              Routes to the appropriate child/network server
//	ping                    Health check
package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"

	"mcp-bridge/internal/logger"
)

// maxRequestBodyBytes is the maximum number of bytes read from an incoming
// POST body. Requests exceeding this limit are rejected with 400.
const maxRequestBodyBytes = 4 << 20 // 4 MiB

// ToolRouter is the interface the Server uses to list and dispatch tools.
// *router.Router satisfies this interface.
type ToolRouter interface {
	Tools() []Tool
	// Call routes a tools/call request. headers contains MCP-layer HTTP
	// headers from the parent request to forward to the child/remote server.
	Call(ctx context.Context, unifiedName string, arguments map[string]any, headers map[string]string) (*ToolCallResult, error)
	// TerminateAll signals all registered clients to terminate their sessions.
	// headers contains the MCP-layer headers from the parent DELETE request.
	TerminateAll(ctx context.Context, headers map[string]string)
}

// Server is the bridge's HTTP MCP server.
type Server struct {
	router    ToolRouter
	authToken string
	tlsCfg    *tls.Config // nil = plain HTTP
	mux       *http.ServeMux
	http      *http.Server
}

// NewServer creates a Server that listens on addr and serves the MCP endpoint
// at path using the given router. authToken may be empty to disable auth.
// tlsCfg may be nil for plain HTTP; when set the server uses HTTPS.
func NewServer(router ToolRouter, addr, path, authToken string, tlsCfg *tls.Config) *Server {
	s := &Server{
		router:    router,
		authToken: authToken,
		tlsCfg:    tlsCfg,
		mux:       http.NewServeMux(),
	}
	s.mux.HandleFunc(path, s.handleHTTP)
	s.http = &http.Server{
		Addr:      addr,
		Handler:   s.mux,
		TLSConfig: tlsCfg,
	}
	return s
}

// Start begins listening. It blocks until the context is cancelled, then
// performs a graceful HTTP shutdown.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if s.tlsCfg != nil {
			logger.L().Info("listening (TLS)", zap.String("addr", s.http.Addr))
			if err := s.http.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		} else {
			logger.L().Info("listening", zap.String("addr", s.http.Addr))
			if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return s.http.Shutdown(context.Background())
	}
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// handleHTTP is the single HTTP handler for the MCP endpoint.
// It dispatches based on HTTP method:
//
//	POST   — JSON-RPC dispatch
//	GET    — 405 (server-push SSE not implemented at this level)
//	DELETE — session termination forwarded to all clients
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	log := logger.L()

	// Bearer token auth — only enforced when auth_token is configured.
	if s.authToken != "" {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		token := ""
		if len(header) > len(prefix) && header[:len(prefix)] == prefix {
			token = header[len(prefix):]
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) != 1 {
			log.Warn("unauthorized request", zap.String("remote", r.RemoteAddr))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Extract MCP-layer headers from the parent request to forward downstream.
	mcpHeaders := extractMCPHeaders(r)

	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r, mcpHeaders)
	case http.MethodGet:
		// Level 1: we consume server-push internally; we do not expose it to
		// the parent. Return 405 so the parent knows not to attempt GET.
		http.Error(w, "server-sent events not supported on this bridge", http.StatusMethodNotAllowed)
	case http.MethodDelete:
		s.handleDelete(w, r, mcpHeaders)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePost processes a JSON-RPC POST request.
func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, mcpHeaders map[string]string) {
	log := logger.L()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		writeJSONResponse(w, http.StatusBadRequest, nil,
			NewErrorResponse(nil, CodeParseError, "read body: "+err.Error()))
		return
	}
	log.Debug("mcp request", zap.String("body", string(body)))

	var req Request
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		writeJSONResponse(w, http.StatusBadRequest, nil,
			NewErrorResponse(nil, CodeParseError, "parse error: "+err.Error()))
		return
	}

	// Notifications (no id) require no response body — acknowledge with 202.
	if req.ID == nil {
		s.handleNotification(&req)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp, respHeaders := s.dispatch(r.Context(), &req, mcpHeaders)

	respBytes, _ := json.Marshal(resp)
	log.Debug("mcp response", zap.String("body", string(respBytes)))

	writeJSONResponse(w, http.StatusOK, respHeaders, resp)
}

// handleDelete forwards a session termination to all registered clients.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, mcpHeaders map[string]string) {
	_, _ = io.Copy(io.Discard, r.Body)
	s.router.TerminateAll(r.Context(), mcpHeaders)
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Request dispatcher
// ---------------------------------------------------------------------------

// dispatch routes a JSON-RPC request to the appropriate handler.
// It returns the response and any MCP-layer headers to include in the HTTP
// response (e.g. Mcp-Session-Id returned by a remote server).
func (s *Server) dispatch(ctx context.Context, req *Request, mcpHeaders map[string]string) (*Response, map[string]string) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req), nil
	case "tools/list":
		return s.handleToolsList(req), nil
	case "tools/call":
		return s.handleToolsCall(ctx, req, mcpHeaders), nil
	case "ping":
		return s.handlePing(req), nil
	default:
		logger.L().Warn("unknown method", zap.String("method", req.Method))
		return NewErrorResponse(req.ID, CodeMethodNotFound,
			fmt.Sprintf("method not found: %q", req.Method)), nil
	}
}

func (s *Server) handleNotification(req *Request) {
	logger.L().Debug("notification received", zap.String("method", req.Method))
}

// handleInitialize responds to the MCP initialize handshake.
func (s *Server) handleInitialize(req *Request) *Response {
	result := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": WrapperInfo,
	}
	resp, err := NewResultResponse(req.ID, result)
	if err != nil {
		return NewErrorResponse(req.ID, CodeInternalError, err.Error())
	}
	logger.L().Info("initialized by parent client")
	return resp
}

func (s *Server) handlePing(req *Request) *Response {
	resp, err := NewResultResponse(req.ID, map[string]any{})
	if err != nil {
		return NewErrorResponse(req.ID, CodeInternalError, err.Error())
	}
	return resp
}

// handleToolsList returns the merged tool list from all child/network servers.
func (s *Server) handleToolsList(req *Request) *Response {
	result := ToolsListResult{Tools: s.router.Tools()}
	resp, err := NewResultResponse(req.ID, result)
	if err != nil {
		return NewErrorResponse(req.ID, CodeInternalError, err.Error())
	}
	return resp
}

// handleToolsCall routes a tools/call to the correct child/network server.
// mcpHeaders contains MCP-layer headers from the parent to forward downstream.
func (s *Server) handleToolsCall(ctx context.Context, req *Request, mcpHeaders map[string]string) *Response {
	var params ToolCallParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return NewErrorResponse(req.ID, CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if params.Name == "" {
		return NewErrorResponse(req.ID, CodeInvalidParams, "tools/call: name is required")
	}

	result, err := s.router.Call(ctx, params.Name, params.Arguments, mcpHeaders)
	if err != nil {
		if rpcErr, ok := err.(*RPCError); ok {
			return NewErrorResponse(req.ID, rpcErr.Code, rpcErr.Message)
		}
		return NewErrorResponse(req.ID, CodeChildUnavailable,
			fmt.Sprintf("tool call failed: %v", err))
	}

	resp, err := NewResultResponse(req.ID, result)
	if err != nil {
		return NewErrorResponse(req.ID, CodeInternalError, err.Error())
	}
	return resp
}

// ---------------------------------------------------------------------------
// Header helpers
// ---------------------------------------------------------------------------

// extractMCPHeaders pulls the MCP-layer headers out of an HTTP request.
// Only the headers that have semantic meaning in the MCP protocol are
// extracted; transport-level headers (Content-Type, etc.) are not included.
func extractMCPHeaders(r *http.Request) map[string]string {
	out := make(map[string]string, 3)
	for _, name := range []string{HeaderSessionID, HeaderProtocolVersion, HeaderLastEventID} {
		if v := r.Header.Get(name); v != "" {
			out[name] = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Output helper
// ---------------------------------------------------------------------------

// writeJSONResponse writes a JSON-RPC response. extraHeaders contains
// MCP-layer headers to include in the HTTP response (may be nil).
func writeJSONResponse(w http.ResponseWriter, status int, extraHeaders map[string]string, v any) {
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
