package mcptools

import (
	"context"
	"fmt"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/requestlog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// validFlagType reports whether t is a concrete flag-constraint value type the
// requester may attach to a flag. The untyped "" is excluded — a supplied type
// should be definite.
func validFlagType(t string) bool {
	switch config.ConstraintType(t) {
	case config.ConstraintString, config.ConstraintInteger, config.ConstraintBoolean:
		return true
	case config.ConstraintUntyped:
		return false
	default:
		return false
	}
}

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
		mcp.WithObject("flag_types",
			mcp.Description("Optional value type per flag, as {flag: type} where type is "+
				"'string', 'integer', or 'boolean' (e.g. {'--reviewer': 'string'}). Supply this "+
				"when a flag takes a value the tool schema currently types wrong — read the tool's "+
				"--help to decide. On approval it becomes a flag_constraints entry that overrides "+
				"the introspected type."),
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
		if arr, ok := args["subcommand"].([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					subcommand = append(subcommand, s)
				}
			}
		}

		var flags []string
		if arr, ok := args["flags"].([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					flags = append(flags, s)
				}
			}
		}

		// flag_types: keep only entries with a valid type; a bad type is ignored
		// here (and would be rejected again by config validation on --apply).
		var flagTypes map[string]string
		if obj, ok := args["flag_types"].(map[string]any); ok {
			for flag, v := range obj {
				t, ok := v.(string)
				if !ok || !validFlagType(t) {
					continue
				}
				if flagTypes == nil {
					flagTypes = make(map[string]string)
				}
				flagTypes[flag] = t
			}
		}

		sessionID, _, _ := SessionInfo(ctx)
		req := &requestlog.CapabilityRequest{
			Tool:       toolName,
			Subcommand: subcommand,
			Flags:      flags,
			FlagTypes:  flagTypes,
			Rationale:  rationale,
			Enables:    enables,
			SessionID:  sessionID,
		}

		if err := store.Append(req); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to log request: %s", err)), nil
		}

		typeLine := ""
		if len(flagTypes) > 0 {
			typeLine = fmt.Sprintf("\nFlag types: %v", flagTypes)
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"Capability request recorded (ID: %s).\n\nTool: %s\nSubcommand: %v\nFlags: %v%s\nRationale: %s\nEnables: %s\n\nAn administrator will review this request.",
			req.ID, toolName, subcommand, flags, typeLine, rationale, enables,
		)), nil
	}

	s.AddTool(tool, handler)
}
