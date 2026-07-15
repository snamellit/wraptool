package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pti/wraptool/internal/buildinfo"
	"github.com/pti/wraptool/internal/introspect"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// RegisterInfoTool registers the wraptool_info MCP tool. It reports the running
// server's build identity (version + git revision + dirty flag) and, per
// configured CLI tool, when its --help schema was last introspected — so a
// client can confirm which binary is serving and whether a just-shipped
// introspection fix is actually live, in one call instead of fingerprinting
// tool schemas. toolNames is the set of wrapped CLI tools to report freshness
// for.
func RegisterInfoTool(s *server.MCPServer, toolNames []string) {
	names := append([]string(nil), toolNames...)
	sort.Strings(names)

	tool := mcp.NewTool("wraptool_info",
		mcp.WithDescription("Report the running wraptool server's build identity (version, git revision, "+
			"dirty flag, Go version, start time) and, per wrapped tool, when its --help schema was last "+
			"introspected. Use to confirm which wraptool binary is serving and whether an introspection "+
			"change is live."),
	)

	handler := func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type toolFreshness struct {
			IntrospectedAt any    `json:"introspected_at"` // RFC3339 time, or null if not cached
			ConfigHash     string `json:"config_hash,omitempty"`
			Note           string `json:"note,omitempty"`
		}
		introspection := make(map[string]toolFreshness, len(names))
		for _, name := range names {
			if at, hash, ok := introspect.CacheStamp(name); ok {
				introspection[name] = toolFreshness{IntrospectedAt: at, ConfigHash: hash}
			} else {
				introspection[name] = toolFreshness{Note: "no cache (introspected fresh this boot, or introspection failed)"}
			}
		}

		resp := map[string]any{
			"build":         buildinfo.Get(),
			"introspection": introspection,
		}
		b, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encoding info: %s", err)), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}

	s.AddTool(tool, handler)
}
