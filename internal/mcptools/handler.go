package mcptools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Handler processes MCP tool calls for a specific CLI tool + subcommand.
type Handler struct {
	ToolName   string
	Subcommand []string
	ToolConfig *config.ToolConfig
	AllowRule  *config.AllowRule
	Policy     *whitelist.Policy
	Store      *requestlog.Store
}

// Handle processes an MCP tool call.
func (h *Handler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start := time.Now()

	sessionID, clientName, clientVer := sessionInfo(ctx)

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
		h.logCall(ctx, sessionID, clientName, clientVer, flags, positionalArgs, false, decision.Reason, 0, false, time.Since(start))
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
		h.logCall(ctx, sessionID, clientName, clientVer, flags, positionalArgs, true, "", -1, false, time.Since(start))
		return mcp.NewToolResultError(fmt.Sprintf("execution error: %s", err)), nil
	}

	h.logCall(ctx, sessionID, clientName, clientVer, flags, positionalArgs, true, "", result.ExitCode, result.Truncated, time.Since(start))

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

func (h *Handler) logCall(ctx context.Context, sessionID, clientName, clientVer string, flags map[string]string, positionalArgs []string, allowed bool, denyReason string, exitCode int, truncated bool, dur time.Duration) {
	if h.Store == nil {
		return
	}
	evt := &requestlog.ToolCallEvent{
		SessionID:      sessionID,
		ClientName:     clientName,
		ClientVersion:  clientVer,
		Tool:           h.ToolName,
		Subcommand:     h.Subcommand,
		Allowed:        allowed,
		DenyReason:     denyReason,
		Flags:          flags,
		PositionalArgs: positionalArgs,
		ExitCode:       exitCode,
		Truncated:      truncated,
		Duration:       dur.Round(time.Millisecond).String(),
	}
	if err := h.Store.AppendToolCall(evt); err != nil {
		// Non-fatal; don't block the tool call for a logging failure.
		_ = err
	}
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

func sessionInfo(ctx context.Context) (sessionID, clientName, clientVersion string) {
	session := server.ClientSessionFromContext(ctx)
	if session == nil {
		return "", "", ""
	}
	sessionID = session.SessionID()
	if ci, ok := session.(server.SessionWithClientInfo); ok {
		info := ci.GetClientInfo()
		clientName = info.Name
		clientVersion = info.Version
	}
	return
}
