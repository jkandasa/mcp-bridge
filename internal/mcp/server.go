// Package mcp implements the bridge's own MCP server endpoint, exposed over
// HTTP using the MCP Streamable HTTP transport.
//
// The parent client (e.g. Claude Code) connects via HTTP or HTTPS:
//
//	{ "mcpServers": { "bridge": { "url": "http://localhost:7575/mcp" } } }
//
// Supported HTTP methods on the MCP endpoint:
//
//	POST   — send any JSON-RPC request or notification.
//	         Responses are application/json for most methods.
//	         For tools/call the response may be text/event-stream when the
//	         upstream remote server returns a streaming result.
//	GET    — server-sent events stream for server-initiated notifications.
//	         The connection stays open; events are sent as they arrive.
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
	"sync"

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

	// pushMu guards pushSubs.
	pushMu   sync.RWMutex
	pushSubs map[chan []byte]struct{} // active GET SSE subscribers
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
		pushSubs:  make(map[chan []byte]struct{}),
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
//	POST   — JSON-RPC dispatch; response may be SSE for tools/call
//	GET    — server-sent events stream for server-initiated notifications
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
		s.handleGetSSE(w, r)
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

	// tools/call may return a streaming result — handle separately.
	if req.Method == "tools/call" {
		s.handleToolsCallHTTP(w, r, &req, mcpHeaders)
		return
	}

	resp, respHeaders := s.dispatch(r.Context(), &req, mcpHeaders)

	respBytes, _ := json.Marshal(resp)
	log.Debug("mcp response", zap.String("body", string(respBytes)))

	writeJSONResponse(w, http.StatusOK, respHeaders, resp)
}

// handleGetSSE opens a long-lived server-sent events stream to the parent
// client. The connection stays open until the client disconnects or the
// server shuts down. Server-initiated notifications (e.g.
// notifications/tools/list_changed) are flushed through this channel.
func (s *Server) handleGetSSE(w http.ResponseWriter, r *http.Request) {
	log := logger.L()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Send an initial comment to confirm the stream is open.
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Register this subscriber.
	ch := make(chan []byte, 16)
	s.pushMu.Lock()
	s.pushSubs[ch] = struct{}{}
	s.pushMu.Unlock()

	log.Debug("SSE subscriber connected", zap.String("remote", r.RemoteAddr))

	defer func() {
		s.pushMu.Lock()
		delete(s.pushSubs, ch)
		s.pushMu.Unlock()
		log.Debug("SSE subscriber disconnected", zap.String("remote", r.RemoteAddr))
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write(event)
			flusher.Flush()
		}
	}
}

// broadcastSSE sends a server-initiated SSE event to all active GET subscribers.
// msg must be a complete SSE event including the trailing double newline.
func (s *Server) broadcastSSE(msg []byte) {
	s.pushMu.RLock()
	defer s.pushMu.RUnlock()
	for ch := range s.pushSubs {
		select {
		case ch <- msg:
		default:
			// Subscriber is full; skip rather than block.
		}
	}
}

// handleDelete forwards a session termination to all registered clients.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, mcpHeaders map[string]string) {
	_, _ = io.Copy(io.Discard, r.Body)
	s.router.TerminateAll(r.Context(), mcpHeaders)
	w.WriteHeader(http.StatusOK)
}

// dispatch routes a JSON-RPC request to the appropriate handler.
// It returns the response and any MCP-layer headers to include in the HTTP
// response (e.g. Mcp-Session-Id returned by a remote server).
// Note: tools/call is NOT dispatched here — it is handled directly in
// handlePost via handleToolsCallHTTP to support streaming responses.
func (s *Server) dispatch(ctx context.Context, req *Request, mcpHeaders map[string]string) (*Response, map[string]string) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req), nil
	case "tools/list":
		return s.handleToolsList(req), nil
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
// Returns a *Response for the non-streaming (application/json) path only.
// Callers that need streaming must use handleToolsCallHTTP instead.
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

	// Streaming result — this path should not be reached via dispatch();
	// streaming calls go through handleToolsCallHTTP directly.
	if result.Stream != nil {
		result.Stream.Close()
		return NewErrorResponse(req.ID, CodeInternalError, "unexpected streaming result in non-streaming path")
	}

	resp, err := NewResultResponse(req.ID, result)
	if err != nil {
		return NewErrorResponse(req.ID, CodeInternalError, err.Error())
	}
	return resp
}

// handleToolsCallHTTP is the HTTP-aware tools/call handler. It detects whether
// the upstream returned a streaming (text/event-stream) result and, if so,
// proxies the SSE bytes directly back to the parent. For non-streaming results
// it falls back to the normal JSON response path.
func (s *Server) handleToolsCallHTTP(w http.ResponseWriter, r *http.Request, req *Request, mcpHeaders map[string]string) {
	log := logger.L()

	var params ToolCallParams
	if req.Params != nil {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeJSONResponse(w, http.StatusBadRequest, nil,
				NewErrorResponse(req.ID, CodeInvalidParams, "invalid params: "+err.Error()))
			return
		}
	}
	if params.Name == "" {
		writeJSONResponse(w, http.StatusBadRequest, nil,
			NewErrorResponse(req.ID, CodeInvalidParams, "tools/call: name is required"))
		return
	}

	result, err := s.router.Call(r.Context(), params.Name, params.Arguments, mcpHeaders)
	if err != nil {
		var resp *Response
		if rpcErr, ok := err.(*RPCError); ok {
			resp = NewErrorResponse(req.ID, rpcErr.Code, rpcErr.Message)
		} else {
			resp = NewErrorResponse(req.ID, CodeChildUnavailable,
				fmt.Sprintf("tool call failed: %v", err))
		}
		writeJSONResponse(w, http.StatusOK, nil, resp)
		return
	}

	// --- Streaming path ---
	if result.Stream != nil {
		defer result.Stream.Close()

		flusher, ok := w.(http.Flusher)
		if !ok {
			// ResponseWriter doesn't support flushing — buffer and return JSON.
			log.Warn("ResponseWriter does not support flushing; buffering SSE stream")
			writeJSONResponse(w, http.StatusInternalServerError, nil,
				NewErrorResponse(req.ID, CodeInternalError, "streaming not supported by transport"))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		log.Debug("proxying SSE stream to parent", zap.String("tool", params.Name))
		_, copyErr := io.Copy(w, result.Stream)
		flusher.Flush()
		if copyErr != nil && r.Context().Err() == nil {
			log.Warn("SSE proxy copy error", zap.String("tool", params.Name), zap.Error(copyErr))
		}
		return
	}

	// --- Non-streaming path ---
	resp, err := NewResultResponse(req.ID, result)
	if err != nil {
		writeJSONResponse(w, http.StatusOK, nil,
			NewErrorResponse(req.ID, CodeInternalError, err.Error()))
		return
	}

	respBytes, _ := json.Marshal(resp)
	log.Debug("mcp response", zap.String("body", string(respBytes)))
	writeJSONResponse(w, http.StatusOK, nil, resp)
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
