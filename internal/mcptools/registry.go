package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/server"
)

// RegisterAll registers the MCP tools produced by every source and returns the
// list of registered tool names, for later removal on reload.
func RegisterAll(s *server.MCPServer, sources []Source) ([]string, error) {
	ctx := context.Background()

	var registered []string
	for _, src := range sources {
		tools, err := src.Tools(ctx)
		if err != nil {
			return registered, err
		}
		for _, rt := range tools {
			s.AddTool(rt.Tool, rt.Handler)
			registered = append(registered, rt.Name)
		}
	}
	return registered, nil
}
