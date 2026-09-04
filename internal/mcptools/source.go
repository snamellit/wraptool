package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/pti/wraptool/internal/config"
)

// Source produces registrable MCP tools and their invocation handlers.
//
// It is the seam that lets wraptool expose tools from more than one kind of
// backend behind the same registration, hot-reload, audit, and scrubbing
// pipeline. The CLI backend is cliSource (this package); a future stdio
// MCP-server proxy is a second implementation.
type Source interface {
	// Tools returns the tools to register, already policy-filtered. It is
	// called at initial registration and on every config reload. The context
	// bounds any I/O the source performs to enumerate its tools.
	Tools(ctx context.Context) ([]RegisteredTool, error)

	// Close releases any resources the source holds (e.g. child processes).
	// It is a no-op for the CLI source.
	Close() error
}

// RegisteredTool is a fully-built MCP tool plus the handler that services its
// calls, ready to hand to server.AddTool.
//
// Build one with NewRegisteredTool rather than a struct literal: Name and
// Tool.Name are two copies of one string that the registrar relies on being
// equal — it registers under Tool.Name but deregisters by Name, so a divergent
// pair leaks a registration across every reload — and the constructor derives
// both from a single sanitized value.
type RegisteredTool struct {
	Name    string                 // final (namespaced) MCP tool name
	Tool    mcp.Tool               // built schema (flag options, or raw schema)
	Handler server.ToolHandlerFunc // per-tool invocation handler
}

// NewRegisteredTool sanitizes the built tool's name and derives the
// registration name from it, so Name and Tool.Name cannot disagree and no
// Source can hand the registrar a name a model API would reject.
//
// This is the choke point every Source funnels through: minting sites sanitize
// too (ToolName, config.ProxyToolName) because they need the final name for
// collision checks and logging, but SanitizeToolName is idempotent, so this
// pass costs nothing and holds for a Source that forgets.
func NewRegisteredTool(tool mcp.Tool, handler server.ToolHandlerFunc) RegisteredTool {
	tool.Name = config.SanitizeToolName(tool.Name)
	return RegisteredTool{Name: tool.Name, Tool: tool, Handler: handler}
}
