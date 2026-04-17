package local

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"mcp-bridge/internal/config"
	"mcp-bridge/internal/mcp"
)

// shellMetachars are characters that require a shell to interpret correctly.
// Metacharacter detection is applied only to scalar-form commands (t.Command.Raw != "").
// List-form commands are always executed directly — the caller controls the tokens.
const shellMetachars = "|&;<>()$`\\\"'*?[]#~="

// callExec runs the tool's command, captures stdout and stderr separately,
// and returns them as distinct content items.
//
// Command forms:
//   - Scalar ("ls -alh {{path}}"): tokens split on whitespace; shell metacharacters
//     cause the whole string to run via sh -c (with {{name}} expanded first).
//   - List (["sh", "-c", "find {{p}} | wc -l"]): tokens used as-is; always
//     direct exec — no metacharacter detection.
//
// When Params is set, {{name}} placeholders are expanded before execution.
// A standalone "{{name}}" token whose value is an array expands to N args.
// Tools without Params accept no arguments; any supplied arguments are rejected.
// Non-zero exit code or execution error → IsError: true.
func callExec(ctx context.Context, t *config.LocalTool, arguments map[string]any) (*mcp.ToolCallResult, error) {
	if t.Command.IsEmpty() {
		return errorResult("command is empty"), nil
	}

	if len(t.Params) == 0 && len(arguments) > 0 {
		return nil, &mcp.RPCError{
			Code:    mcp.CodeInvalidParams,
			Message: fmt.Sprintf("local tool %q does not accept arguments", t.Tool),
		}
	}

	var cmd *exec.Cmd

	if t.Command.Raw != "" && strings.ContainsAny(t.Command.Raw, shellMetachars) {
		// Scalar form with shell metacharacters — expand then run via sh -c.
		expanded := expandString(t.Command.Raw, arguments)
		cmd = exec.CommandContext(ctx, "sh", "-c", expanded)
	} else {
		// Direct exec: expand each token (array values expand to N args).
		expanded := expandTokens(t.Command.Tokens, arguments)
		if len(expanded) == 0 {
			return errorResult("command expanded to empty token list"), nil
		}
		cmd = exec.CommandContext(ctx, expanded[0], expanded[1:]...)
	}

	var stdout, stderr bytes.Buffer
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

	if len(content) == 0 {
		content = []mcp.ContentItem{{Type: "text", Text: "(no output)"}}
	}

	isError := runErr != nil
	return &mcp.ToolCallResult{Content: content, IsError: isError}, nil
}
