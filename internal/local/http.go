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
// When the tool has Params set, {{name}} placeholders in the URL, header
// values, and body are expanded using the provided arguments map.
// When Params is empty, no arguments are accepted (returns an error if any
// are passed).
//
// Non-2xx status → IsError: true. The response body is still returned so the
// caller can inspect the server-side error detail.
func callHTTP(ctx context.Context, t *config.LocalTool, arguments map[string]any) (*mcp.ToolCallResult, error) {
	method := strings.ToUpper(t.Method)
	if method == "" {
		method = http.MethodGet
	}

	urlStr := t.URL
	body := t.Body
	headers := make(map[string]string, len(t.Headers))

	if len(t.Params) > 0 {
		// Named params mode: expand {{name}} placeholders.
		urlStr = expandString(t.URL, arguments)
		body = expandString(t.Body, arguments)
		for k, v := range t.Headers {
			headers[k] = expandString(v, arguments)
		}
	} else {
		// No-arg mode: reject any supplied arguments.
		if len(arguments) > 0 {
			return nil, &mcp.RPCError{
				Code:    mcp.CodeInvalidParams,
				Message: fmt.Sprintf("local tool %q does not accept arguments", t.Tool),
			}
		}
		for k, v := range t.Headers {
			headers[k] = v
		}
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to build request: %v", err)), nil
	}

	for k, v := range headers {
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to read response body: %v", err)), nil
	}

	text := strings.TrimRight(string(respBody), "\n")
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
