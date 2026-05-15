package mcptools

import (
	"context"
	"fmt"

	"github.com/pti/wraptool/internal/requestlog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// RegisterRequestCapabilityTool registers the wraptool_request_capability MCP tool.
func RegisterRequestCapabilityTool(s *server.MCPServer, store *requestlog.Store) {
	tool := mcp.NewTool("wraptool_request_capability",
		mcp.WithDescription("Request access to a denied CLI capability. An admin will review the request."),
		mcp.WithString("tool",
			mcp.Description("The CLI tool name (e.g. 'git')"),
			mcp.Required(),
		),
		mcp.WithArray("subcommand",
			mcp.Description("Subcommand tokens (e.g. ['push'])"),
		),
		mcp.WithArray("flags",
			mcp.Description("Flags requested (e.g. ['--force'])"),
		),
		mcp.WithString("rationale",
			mcp.Description("Why this capability is needed"),
			mcp.Required(),
		),
		mcp.WithString("enables",
			mcp.Description("What workflow or task this would unblock"),
		),
	)

	handler := func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.GetArguments()

		toolName, _ := args["tool"].(string)
		rationale, _ := args["rationale"].(string)
		enables, _ := args["enables"].(string)

		var subcommand []string
		if arr, ok := args["subcommand"].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					subcommand = append(subcommand, s)
				}
			}
		}

		var flags []string
		if arr, ok := args["flags"].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					flags = append(flags, s)
				}
			}
		}

		req := &requestlog.CapabilityRequest{
			Tool:       toolName,
			Subcommand: subcommand,
			Flags:      flags,
			Rationale:  rationale,
			Enables:    enables,
		}

		if err := store.Append(req); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to log request: %s", err)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf(
			"Capability request recorded (ID: %s).\n\nTool: %s\nSubcommand: %v\nFlags: %v\nRationale: %s\nEnables: %s\n\nAn administrator will review this request.",
			req.ID, toolName, subcommand, flags, rationale, enables,
		)), nil
	}

	s.AddTool(tool, handler)
}
