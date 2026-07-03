package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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
type RegisteredTool struct {
	Name    string                 // final (namespaced) MCP tool name
	Tool    mcp.Tool               // built schema (flag options, or raw schema)
	Handler server.ToolHandlerFunc // per-tool invocation handler
}
