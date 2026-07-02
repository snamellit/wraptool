package mcptools

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/introspect"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// RegisterAll registers MCP tools for all configured CLI tools.
// It runs introspection to enrich tool descriptions and flag metadata.
// Returns the list of MCP tool names registered, for later removal on reload.
func RegisterAll(s *server.MCPServer, cfg *config.Config, policy *whitelist.Policy, store *requestlog.Store) ([]string, error) {
	ctx := context.Background()

	var registered []string
	for toolName, toolCfg := range cfg.Tools {
		tc := toolCfg // capture

		// Introspect the tool to get flag descriptions and types.
		schema, err := introspect.Introspect(ctx, toolName, &tc)
		if err != nil {
			log.Printf("introspection failed for %s, using config-only metadata: %v", toolName, err)
		}

		names, err := registerTool(s, toolName, &tc, policy, schema, store)
		if err != nil {
			return registered, fmt.Errorf("registering tool %q: %w", toolName, err)
		}
		registered = append(registered, names...)
	}
	return registered, nil
}

func registerTool(s *server.MCPServer, toolName string, tc *config.ToolConfig, policy *whitelist.Policy, schema *introspect.Schema, store *requestlog.Store) ([]string, error) {
	var registered []string
	for i := range tc.Allow {
		rule := &tc.Allow[i]
		mcpName := ToolName(toolName, rule.Subcommand)
		subcmdKey := strings.Join(rule.Subcommand, " ")

		// Build description from introspection or fallback.
		description := fmt.Sprintf("%s %s", toolName, subcmdKey)
		var introFlags map[string]introspect.Flag
		if schema != nil {
			if sub, ok := schema.Subcommands[subcmdKey]; ok {
				if sub.Description != "" {
					description = fmt.Sprintf("%s %s: %s", toolName, subcmdKey, sub.Description)
				}
				introFlags = sub.Flags
			}
		}

		opts := []mcp.ToolOption{
			mcp.WithDescription(description),
		}

		for _, flag := range rule.Flags {
			paramName := FlagToParam(flag)
			constraint, hasConstraint := rule.FlagConstraints[flag]

			// Get introspected metadata for this flag.
			var introFlag *introspect.Flag
			if introFlags != nil {
				if f, ok := introFlags[flag]; ok {
					introFlag = &f
				}
			}

			// Determine flag description.
			flagDesc := fmt.Sprintf("Flag: %s", flag)
			if introFlag != nil && introFlag.Description != "" {
				flagDesc = fmt.Sprintf("%s (%s)", introFlag.Description, flag)
			}

			// Determine type: constraint > introspection > heuristic.
			isBool := IsBooleanFlag(flag, rule.FlagConstraints)
			if !isBool && introFlag != nil && introFlag.Type == "boolean" {
				isBool = true
			}

			if isBool {
				opts = append(opts, mcp.WithBoolean(paramName,
					mcp.Description(flagDesc),
				))
			} else {
				schemaType := "string"
				if hasConstraint {
					schemaType = FlagSchemaType(&constraint)
				} else if introFlag != nil {
					schemaType = introFlag.Type
				}

				required := hasConstraint && constraint.Required

				switch schemaType {
				case "integer":
					numberOpts := []mcp.PropertyOption{mcp.Description(flagDesc)}
					if required {
						numberOpts = append(numberOpts, mcp.Required())
					}
					opts = append(opts, mcp.WithNumber(paramName, numberOpts...))
				default:
					stringOpts := []mcp.PropertyOption{mcp.Description(flagDesc)}
					if required {
						stringOpts = append(stringOpts, mcp.Required())
					}
					opts = append(opts, mcp.WithString(paramName, stringOpts...))
				}
			}
		}

		// Add positional args parameter.
		opts = append(opts, mcp.WithArray(
			"args",
			mcp.Description("Positional arguments"),
		))

		tool := mcp.NewTool(mcpName, opts...)

		handler := &Handler{
			ToolName:   toolName,
			Subcommand: rule.Subcommand,
			ToolConfig: tc,
			AllowRule:  rule,
			Policy:     policy,
			Store:      store,
		}

		s.AddTool(tool, handler.Handle)
		registered = append(registered, mcpName)
	}

	return registered, nil
}
