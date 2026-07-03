package mcpproxy

import (
	"context"
	"fmt"
	"time"

	"github.com/pti/wraptool/internal/executor"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/mcp"
)

// handler services calls to one wrapped upstream MCP tool. It is deliberately
// separate from mcptools.Handler (which is CLI subcommand/flag shaped): MCP
// calls are policy-gated at tool-name granularity, forwarded verbatim, and
// scrubbed on the way back.
type handler struct {
	client     *Client
	serverName string
	toolName   string // upstream (un-prefixed) tool name
	policy     *whitelist.MCPPolicy
	store      *requestlog.Store
}

// Handle evaluates the MCP policy, forwards an allowed call to the upstream
// server, scrubs the result, and records an audit event. It never returns a
// non-nil error to the MCP layer: denials and upstream failures come back as
// tool-result errors so the AI sees a message rather than a transport fault.
func (h *handler) Handle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start := time.Now()
	args := req.GetArguments()

	if d := h.policy.Evaluate(h.serverName, h.toolName); !d.Allowed {
		h.audit(ctx, args, false, d.Reason, time.Since(start))
		return mcp.NewToolResultError(fmt.Sprintf(
			"DENIED: %s\n\nTo request this capability, call the 'wraptool_request_capability' tool.",
			d.Reason)), nil
	}

	res, err := h.client.CallTool(ctx, h.toolName, args)
	h.audit(ctx, args, true, "", time.Since(start)) // allowed either way; result differs
	if err != nil {
		// Scrub the error too: a JSON-RPC/transport error message can echo
		// upstream data, so give it the same secret-hiding as a normal result.
		return mcp.NewToolResultError(executor.Scrub(fmt.Sprintf("upstream error: %s", err), h.client.resolvedEnv)), nil
	}

	scrubContent(res.Content, h.client.resolvedEnv)
	return res, nil
}

// audit records the call in the append-only event log as a Source="mcp" event.
// It is best-effort: a logging failure never blocks the tool call.
func (h *handler) audit(ctx context.Context, args map[string]any, allowed bool, denyReason string, dur time.Duration) {
	if h.store == nil {
		return
	}
	sessionID, clientName, clientVer := mcptools.SessionInfo(ctx)
	if err := h.store.AppendMCPToolCall(h.serverName, h.toolName, args, allowed, denyReason, dur, sessionID, clientName, clientVer); err != nil {
		_ = err // non-fatal
	}
}
