package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/introspect"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// toolDiscovery is one wrapped CLI tool as reported by wraptool_discover:
// what policy currently permits, and what the tool itself advertises.
type toolDiscovery struct {
	// Allowed lists the subcommands with an allow rule — these are already
	// callable as MCP tools, under the names in Tools.
	Allowed []string `json:"allowed"`
	Tools   []string `json:"tools,omitempty"`
	// Available maps each advertised subcommand that policy does not permit to
	// its one-line description.
	Available map[string]string `json:"available,omitempty"`
	Note      string            `json:"note,omitempty"`
}

// RegisterDiscoverTool registers the wraptool_discover MCP tool: the catalog of
// wrapped CLI tools, each with the subcommands policy currently allows and the
// subcommands the tool advertises. It exists to break a bootstrapping deadlock —
// a tool with no allow rules registers no MCP tools at all, so without this the
// only way to name a subcommand in a wraptool_request_capability call is to
// guess it.
//
// This reports policy; it does not change or bypass it. Every name here still
// has to go through the capability-request flow and an operator's approval
// before it can be called. The subcommand catalog comes from cache-backed
// introspection. A missing or stale cache may run the wrapped binary with a
// fixed `-h` / `--help` / `help` argv; no argument from the caller ever reaches
// that argv.
//
// liveConfig yields the current config, so a reload that adds a tool or an
// allow rule is reflected without re-registering this tool.
func RegisterDiscoverTool(s *server.MCPServer, liveConfig func() *config.Config) {
	tool := mcp.NewTool("wraptool_discover",
		mcp.WithDescription("List the wrapped CLI tools, the subcommands you are allowed to call today, "+
			"and the subcommands each tool advertises but policy does not yet permit. Use this before "+
			"wraptool_request_capability so the request names a subcommand the tool actually has. "+
			"Listing a subcommand here does NOT mean you may call it — unpermitted ones still require "+
			"an operator to approve a capability request."),
	)

	handler := func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cfg := liveConfig()
		if cfg == nil {
			return mcp.NewToolResultError("no configuration loaded"), nil
		}

		tools := make(map[string]toolDiscovery, len(cfg.Tools))
		for name := range cfg.Tools {
			tc := cfg.Tools[name] // fresh copy; Introspect takes a pointer
			tools[name] = discoverTool(ctx, name, &tc)
		}

		b, err := json.MarshalIndent(map[string]any{"tools": tools}, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("encoding discovery: %s", err)), nil
		}
		return mcp.NewToolResultText(string(b)), nil
	}

	s.AddTool(tool, handler)
}

// discoverTool builds one tool's discovery entry. Introspect is cache-backed, so
// the common path re-reads the cached schema rather than re-running --help.
func discoverTool(ctx context.Context, name string, tc *config.ToolConfig) toolDiscovery {
	d := toolDiscovery{Allowed: []string{}}
	allowed := make(map[string]struct{}, len(tc.Allow))
	for _, rule := range tc.Allow {
		subcommand := strings.Join(rule.Subcommand, " ")
		d.Allowed = append(d.Allowed, subcommand)
		d.Tools = append(d.Tools, ToolName(name, rule.Subcommand))
		allowed[subcommand] = struct{}{}
	}
	sort.Strings(d.Allowed)
	sort.Strings(d.Tools)

	schema, err := introspect.Introspect(ctx, name, tc)
	if err != nil {
		d.Note = fmt.Sprintf("introspection failed: %s", err)
		return d
	}
	if len(schema.Available) > 0 {
		d.Available = make(map[string]string, len(schema.Available))
		for subcommand, description := range schema.Available {
			if _, ok := allowed[subcommand]; !ok {
				d.Available[subcommand] = description
			}
		}
		if len(d.Available) == 0 {
			d.Available = nil
		}
	}

	switch {
	case len(d.Allowed) == 0 && len(d.Available) == 0:
		d.Note = "no allow rules, and this tool's help advertises no subcommands — " +
			"request a capability by name if you know the tool supports it"
	case len(d.Allowed) == 0:
		d.Note = "no allow rules: nothing is callable yet — request one of the available subcommands"
	}
	return d
}
