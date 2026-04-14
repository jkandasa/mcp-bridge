package local

import (
	"bytes"
	"context"
	"os/exec"
	"strings"

	"mcp-bridge/internal/config"
	"mcp-bridge/internal/mcp"
)

// callExec runs the tool's command with its configured args, captures stdout
// and stderr separately, and returns them as distinct content items.
//
// Non-zero exit code or execution error → IsError: true. The output is still
// returned so the caller can see what went wrong.
func callExec(ctx context.Context, t *config.LocalTool) (*mcp.ToolCallResult, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, t.Command, t.Args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var content []mcp.ContentItem

	if out := strings.TrimRight(stdout.String(), "\n"); out != "" {
		content = append(content, mcp.ContentItem{
			Type: "text",
			Text: "stdout:\n" + out,
		})
	}

	if errOut := strings.TrimRight(stderr.String(), "\n"); errOut != "" {
		content = append(content, mcp.ContentItem{
			Type: "text",
			Text: "stderr:\n" + errOut,
		})
	}

	// If both streams were empty, return a placeholder so content is never nil.
	if len(content) == 0 {
		content = []mcp.ContentItem{{Type: "text", Text: "(no output)"}}
	}

	isError := runErr != nil
	return &mcp.ToolCallResult{Content: content, IsError: isError}, nil
}
