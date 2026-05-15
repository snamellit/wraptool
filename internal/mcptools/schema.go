package mcptools

import (
	"strings"

	"github.com/pti/wraptool/internal/config"
)

// ToolName generates the MCP tool name from a tool name and subcommand.
// e.g. ("git", ["commit"]) -> "git_commit"
// e.g. ("gcloud", ["compute", "instances", "list"]) -> "gcloud_compute_instances_list"
func ToolName(toolName string, subcommand []string) string {
	parts := make([]string, 0, 1+len(subcommand))
	parts = append(parts, toolName)
	parts = append(parts, subcommand...)
	return strings.Join(parts, "_")
}

// FlagToParam converts a CLI flag name to a JSON Schema parameter name.
// e.g. "--set-upstream" -> "set_upstream"
// e.g. "-m" -> "m"
func FlagToParam(flag string) string {
	name := strings.TrimLeft(flag, "-")
	return strings.ReplaceAll(name, "-", "_")
}

// ParamToFlag converts a JSON Schema parameter name back to a CLI flag.
// e.g. "set_upstream" -> "--set-upstream"
// e.g. "m" -> "-m"
func ParamToFlag(param string) string {
	name := strings.ReplaceAll(param, "_", "-")
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// FlagSchemaType maps a config flag constraint type to JSON Schema type.
func FlagSchemaType(c *config.FlagConstraint) string {
	if c == nil {
		return "string"
	}
	switch c.Type {
	case "integer":
		return "integer"
	case "boolean":
		return "boolean"
	default:
		return "string"
	}
}

// IsBooleanFlag checks if a flag is boolean (no value argument).
func IsBooleanFlag(flag string, constraints map[string]config.FlagConstraint) bool {
	if c, ok := constraints[flag]; ok {
		return c.Type == "boolean"
	}
	// Heuristic: common boolean flag patterns.
	boolPrefixes := []string{"--no-", "--enable-", "--disable-"}
	for _, prefix := range boolPrefixes {
		if strings.HasPrefix(flag, prefix) {
			return true
		}
	}
	return false
}
