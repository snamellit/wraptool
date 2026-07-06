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

// FlagSchemaType maps a config flag constraint type to JSON Schema type,
// derived from the single ConstraintType.Kind definition.
func FlagSchemaType(c *config.FlagConstraint) string {
	if c == nil {
		return "string"
	}
	if k, ok := c.Type.Kind(); ok {
		return k.JSONSchema
	}
	return "string"
}

// wellKnownBooleanFlags are flag names that are effectively always valueless
// switches across the CLIs wraptool wraps. Recognizing them by exact name saves
// a config from spelling out a `flag_constraints: {--yes: {type: boolean}}`
// entry for each — without it a bare switch like --yes would default to a
// string param and render as "--yes true", which most tools reject.
//
// Kept deliberately conservative — only names that are boolean across
// essentially every tool. Short -f is deliberately excluded: it is a value flag
// at least as often as a force switch (tar -f <file>, make -f <file>,
// awk -f <script>), so the unambiguous long form --force is listed instead.
//
// This is only a fallback: an explicit flag_constraints entry overrides it, and
// --help introspection (when available) takes precedence too (see cli_source.go
// buildCLITools), so a tool that genuinely takes a value for a listed name —
// e.g. --verbose <level> — is still typed correctly from its parsed help.
var wellKnownBooleanFlags = map[string]bool{
	"--yes": true, "-y": true,
	"--force":   true,
	"--dry-run": true,
	"--quiet":   true,
	"--verbose": true,
}

// IsBooleanFlag checks if a flag is boolean (no value argument). Precedence: an
// explicit flag_constraints entry with a type wins; otherwise a small set of
// well-known boolean flag names and prefixes is recognized so common switches
// need no per-flag config.
func IsBooleanFlag(flag string, constraints map[string]config.FlagConstraint) bool {
	// Only an explicit type decides boolean-ness. A constraint that sets other
	// fields (e.g. required or pattern) but leaves type empty must NOT short-
	// circuit to false — that would suppress the well-known-name and prefix
	// heuristics below and silently turn a bare switch like --yes back into a
	// value-taking flag.
	if c, ok := constraints[flag]; ok && c.Type != config.ConstraintUntyped {
		k, known := c.Type.Kind()
		return known && k.Valueless
	}
	if wellKnownBooleanFlags[flag] {
		return true
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
