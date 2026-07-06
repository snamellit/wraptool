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
)

// cliSource exposes the configured CLI tools (git, gcloud, ...) as MCP tools.
// It reproduces wraptool's original registration behaviour: one MCP tool per
// allow-rule, enriched with --help introspection.
type cliSource struct {
	cfg    *config.Config
	policy *whitelist.Policy
	store  *requestlog.Store
}

// NewCLISource builds the CLI tool source from the loaded config, policy, and
// audit store.
func NewCLISource(cfg *config.Config, policy *whitelist.Policy, store *requestlog.Store) Source {
	return &cliSource{cfg: cfg, policy: policy, store: store}
}

func (c *cliSource) Tools(ctx context.Context) ([]RegisteredTool, error) {
	var out []RegisteredTool
	for toolName, toolCfg := range c.cfg.Tools {
		tc := toolCfg // capture; handlers hold &tc

		// Introspect the tool to get flag descriptions and types.
		schema, err := introspect.Introspect(ctx, toolName, &tc)
		if err != nil {
			log.Printf("introspection failed for %s, using config-only metadata: %v", toolName, err)
		}

		tools, err := buildCLITools(toolName, &tc, c.policy, schema, c.store)
		if err != nil {
			return out, fmt.Errorf("registering tool %q: %w", toolName, err)
		}
		out = append(out, tools...)
	}
	return out, nil
}

func (c *cliSource) Close() error { return nil }

// buildCLITools builds one RegisteredTool per allow-rule for a single CLI tool.
func buildCLITools(toolName string, tc *config.ToolConfig, policy *whitelist.Policy, schema *introspect.Schema, store *requestlog.Store) ([]RegisteredTool, error) {
	var out []RegisteredTool
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

			// Determine boolean-ness with precedence: explicit constraint >
			// --help introspection > name heuristic. Introspection, when present,
			// is authoritative for whether a flag takes a value, so a well-known
			// boolean name (e.g. --verbose) that a given tool actually uses with a
			// value (--verbose <level>) is not forced boolean against its help.
			var isBool bool
			switch {
			case hasConstraint && constraint.Type != config.ConstraintUntyped:
				k, _ := constraint.Type.Kind()
				isBool = k.Valueless
			case introFlag != nil && introFlag.Type != "":
				isBool = introFlag.Type == "boolean"
			default:
				// Typed constraints are handled by the first case above, so only
				// the name/prefix heuristic is wanted here — pass nil rather than
				// re-consulting the constraints map.
				isBool = IsBooleanFlag(flag, nil)
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

		out = append(out, RegisteredTool{
			Name:    mcpName,
			Tool:    tool,
			Handler: handler.Handle,
		})
	}

	return out, nil
}
