package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// paramNamePattern is the property-key pattern the Anthropic API enforces on
// tool input schemas. A flag whose derived parameter name violates it would
// be rejected with a 400 mid-session, so reject it at config-load time.
const paramNamePattern = `^[a-zA-Z0-9_.-]{1,64}$`

var paramNameRe = regexp.MustCompile(paramNamePattern)

// flagParamName mirrors mcptools.FlagToParam — the schema property key derived
// from a flag. Duplicated here rather than imported to avoid a
// config → mcptools import cycle.
func flagParamName(flag string) string {
	return strings.ReplaceAll(strings.TrimLeft(flag, "-"), "-", "_")
}

func (c *Config) Validate() error {
	switch c.Server.Transport {
	case "stdio", "sse", "unix":
		// valid
	default:
		return fmt.Errorf("server.transport must be stdio, sse, or unix, got %q", c.Server.Transport)
	}

	if c.Server.Transport == "sse" || c.Server.Transport == "unix" {
		if c.Server.Listen == "" {
			return fmt.Errorf("server.listen is required for %s transport", c.Server.Transport)
		}
	}

	if c.Server.AuthTokenFile != "" {
		if _, err := os.Stat(c.Server.AuthTokenFile); err != nil {
			return fmt.Errorf("server.auth_token_file %q: %w", c.Server.AuthTokenFile, err)
		}
	}

	if len(c.Tools) == 0 {
		return fmt.Errorf("no tools configured")
	}

	for name, tool := range c.Tools {
		if err := validateTool(name, &tool); err != nil {
			return fmt.Errorf("tool %q: %w", name, err)
		}
	}
	return nil
}

func validateTool(name string, tc *ToolConfig) error {
	if tc.Binary == "" {
		return fmt.Errorf("binary path is required")
	}

	if !filepath.IsAbs(tc.Binary) {
		return fmt.Errorf("binary path must be absolute, got %q", tc.Binary)
	}

	// Resolve symlinks and validate the final target.
	resolved, err := filepath.EvalSymlinks(tc.Binary)
	if err != nil {
		return fmt.Errorf("binary %q: %w", tc.Binary, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("binary %q (resolved to %q): %w", tc.Binary, resolved, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("binary %q (resolved to %q) is not a regular file", tc.Binary, resolved)
	}

	if len(tc.Allow) == 0 {
		return fmt.Errorf("at least one allow rule is required")
	}

	for i, rule := range tc.Allow {
		if len(rule.Subcommand) == 0 {
			return fmt.Errorf("allow rule %d: subcommand is required", i)
		}
		for _, flag := range rule.Flags {
			if p := flagParamName(flag); !paramNameRe.MatchString(p) {
				return fmt.Errorf("allow rule %d: flag %q yields invalid schema property %q (must match %s) — %q is not a flag; drop it (positional args go through `args`)", i, flag, p, paramNamePattern, flag)
			}
		}
	}

	for i, rule := range tc.Deny {
		if len(rule.Subcommand) == 0 {
			return fmt.Errorf("deny rule %d: subcommand is required", i)
		}
	}

	return nil
}
