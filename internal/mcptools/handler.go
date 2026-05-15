package mcptools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/mcp"
)

// Handler processes MCP tool calls for a specific CLI tool + subcommand.
type Handler struct {
	ToolName   string
	Subcommand []string
	ToolConfig *config.ToolConfig
	AllowRule  *config.AllowRule
	Policy     *whitelist.Policy
}

// Handle processes an MCP tool call.
func (h *Handler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Extract flags and positional args from the MCP request arguments.
	flags := make(map[string]string)
	var positionalArgs []string

	for param, value := range request.GetArguments() {
		if param == "args" {
			// Positional arguments array.
			if arr, ok := value.([]interface{}); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok {
						positionalArgs = append(positionalArgs, s)
					}
				}
			}
			continue
		}

		// Convert parameter name back to flag.
		flag := ParamToFlag(param)
		switch v := value.(type) {
		case bool:
			if v {
				flags[flag] = ""
			}
		case string:
			flags[flag] = v
		case float64:
			flags[flag] = fmt.Sprintf("%v", v)
		default:
			flags[flag] = fmt.Sprintf("%v", v)
		}
	}

	// Evaluate against the whitelist policy.
	decision := h.Policy.Evaluate(h.ToolName, h.Subcommand, flags, positionalArgs)
	if !decision.Allowed {
		return denialResult(h.ToolName, h.Subcommand, flags, positionalArgs, decision), nil
	}

	// Execute the command with a timeout.
	timeout := h.ToolConfig.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := executor.Run(execCtx, h.ToolConfig, h.Subcommand, flags, positionalArgs)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("execution error: %s", err)), nil
	}

	// Build response.
	output := result.Stdout
	if result.ExitCode != 0 {
		output = fmt.Sprintf("Exit code: %d\n\nStdout:\n%s\n\nStderr:\n%s",
			result.ExitCode, result.Stdout, result.Stderr)
	}
	if result.Truncated {
		output += "\n\n[WARNING: output was truncated at 1MB limit]"
	}

	return mcp.NewToolResultText(output), nil
}

func denialResult(toolName string, subcommand []string, flags map[string]string, positionalArgs []string, decision whitelist.Decision) *mcp.CallToolResult {
	// Reconstruct the attempted command for the denial message.
	cmdParts := []string{toolName}
	cmdParts = append(cmdParts, subcommand...)
	for flag, value := range flags {
		cmdParts = append(cmdParts, flag)
		if value != "" {
			cmdParts = append(cmdParts, value)
		}
	}
	cmdParts = append(cmdParts, positionalArgs...)

	var sb strings.Builder
	fmt.Fprintf(&sb, "DENIED: '%s' is not permitted.\n\n", strings.Join(cmdParts, " "))
	fmt.Fprintf(&sb, "Policy: %s\n", decision.Reason)

	if len(decision.AllowedFlags) > 0 {
		fmt.Fprintf(&sb, "Allowed flags for '%s %s': %s\n",
			toolName, strings.Join(subcommand, " "),
			strings.Join(decision.AllowedFlags, ", "))
	}

	sb.WriteString("\nTo request this capability, call the 'wraptool_request_capability' tool.")

	return mcp.NewToolResultText(sb.String())
}
