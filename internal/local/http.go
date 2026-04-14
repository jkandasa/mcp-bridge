package local

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"mcp-bridge/internal/config"
	"mcp-bridge/internal/mcp"
)

// callHTTP fires a configured HTTP request and returns the response body as
// the tool result.
//
// Non-2xx status → IsError: true. The response body is still returned so the
// caller can see the error detail from the server.
func callHTTP(ctx context.Context, t *config.LocalTool) (*mcp.ToolCallResult, error) {
	method := strings.ToUpper(t.Method)
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader io.Reader
	if t.Body != "" {
		bodyReader = strings.NewReader(t.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, t.URL, bodyReader)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build request: %v", err)), nil
	}

	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}

	// The http.Client timeout is handled by the context (set in CallTool),
	// so we use a client with no additional timeout here.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return errorResult(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to read response body: %v", err)), nil
	}

	text := strings.TrimRight(string(body), "\n")
	if text == "" {
		text = "(empty response body)"
	}

	isError := resp.StatusCode < 200 || resp.StatusCode >= 300
	content := []mcp.ContentItem{
		{Type: "text", Text: fmt.Sprintf("status: %d\n\n%s", resp.StatusCode, text)},
	}
	return &mcp.ToolCallResult{Content: content, IsError: isError}, nil
}

// errorResult wraps a message string into an error ToolCallResult.
func errorResult(msg string) *mcp.ToolCallResult {
	return &mcp.ToolCallResult{
		Content: []mcp.ContentItem{{Type: "text", Text: msg}},
		IsError: true,
	}
}
