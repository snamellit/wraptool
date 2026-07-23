package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	switch c.MCP.Transport {
	case "stdio", "sse", "unix":
		// valid
	default:
		return fmt.Errorf("mcp.transport must be stdio, sse, or unix, got %q", c.MCP.Transport)
	}

	if c.MCP.Transport == "sse" || c.MCP.Transport == "unix" {
		if c.MCP.Listen == "" {
			return fmt.Errorf("mcp.listen is required for %s transport", c.MCP.Transport)
		}
	}

	if c.MCP.AuthTokenFile != "" {
		if _, err := os.Stat(c.MCP.AuthTokenFile); err != nil {
			return fmt.Errorf("mcp.auth_token_file %q: %w", c.MCP.AuthTokenFile, err)
		}
	}
	if c.Admin.Listen != "" {
		if c.Admin.AuthTokenFile == "" {
			return fmt.Errorf("admin.auth_token_file is required when admin.listen is set")
		}
		adminInfo, err := os.Stat(c.Admin.AuthTokenFile)
		if err != nil {
			return fmt.Errorf("admin.auth_token_file %q: %w", c.Admin.AuthTokenFile, err)
		}
		if c.MCP.AuthTokenFile != "" {
			mcpInfo, err := os.Stat(c.MCP.AuthTokenFile)
			if err != nil {
				return fmt.Errorf("mcp.auth_token_file %q: %w", c.MCP.AuthTokenFile, err)
			}
			if os.SameFile(mcpInfo, adminInfo) {
				return fmt.Errorf("admin.auth_token_file must differ from mcp.auth_token_file")
			}
		}
	}

	for _, root := range c.MCP.AllowedCWDRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("mcp.allowed_cwd_roots: %q must be an absolute path", root)
		}
	}

	if len(c.Tools) == 0 && len(c.MCPServers) == 0 {
		return fmt.Errorf("no tools configured")
	}

	for name, tool := range c.Tools {
		if err := validateTool(&tool); err != nil {
			return fmt.Errorf("tool %q: %w", name, err)
		}
	}

	if err := validateMCPServers(c); err != nil {
		return err
	}
	return nil
}

// validateMCPServers checks each mcp_servers entry has a command, non-empty
// allow/deny tool names, and that effective name prefixes are unique across all
// mcp_servers and collision-free with CLI tool names. Upstream tool names are
// not known at config-load time, so only prefix uniqueness is enforced here;
// full final-name uniqueness against discovered tools is checked at spawn time.
func validateMCPServers(c *Config) error {
	// Seed the prefix set with CLI tool names so an MCP prefix cannot collide
	// with a CLI tool's namespace.
	prefixOwner := make(map[string]string, len(c.Tools)+len(c.MCPServers))
	for name := range c.Tools {
		prefixOwner[name] = fmt.Sprintf("tools.%s", name)
	}

	// Deterministic iteration for stable error messages.
	names := make([]string, 0, len(c.MCPServers))
	for name := range c.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	// Track final registered names (prefix_tool) of allow-listed tools to catch
	// the one underscore clash vector: one server's prefix equalling another's
	// prefix_<leading-tool-segment> (e.g. github+create_issue vs
	// github_create+issue, both → github_create_issue).
	finalOwner := make(map[string]string)

	for _, name := range names {
		srv := c.MCPServers[name]
		if srv.Command == "" {
			return fmt.Errorf("mcp_servers.%s: command is required", name)
		}

		for i, rule := range srv.Deny {
			if strings.TrimSpace(rule.Tool) == "" {
				return fmt.Errorf("mcp_servers.%s: deny rule %d: tool name is required", name, i)
			}
		}

		// applyDefaults sets NamePrefix to the map key when empty, but guard
		// against callers that construct Config directly without defaults.
		prefix := srv.NamePrefix
		if prefix == "" {
			prefix = name
		}
		if owner, ok := prefixOwner[prefix]; ok {
			return fmt.Errorf("mcp_servers.%s: name prefix %q collides with %s", name, prefix, owner)
		}
		prefixOwner[prefix] = fmt.Sprintf("mcp_servers.%s", name)

		for i, rule := range srv.Allow {
			if strings.TrimSpace(rule.Tool) == "" {
				return fmt.Errorf("mcp_servers.%s: allow rule %d: tool name is required", name, i)
			}
			final := prefix + "_" + rule.Tool
			if owner, ok := finalOwner[final]; ok {
				return fmt.Errorf("mcp_servers.%s: tool %q resolves to registered name %q, which collides with %s", name, rule.Tool, final, owner)
			}
			finalOwner[final] = fmt.Sprintf("mcp_servers.%s", name)
		}
	}

	return nil
}

func validateTool(tc *ToolConfig) error {
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

	// A tool with no allow rules is permitted: it is inert (registers no MCP
	// tools and default-denies every call), so it is a safe intermediate state
	// while bootstrapping a tool and adding its rules incrementally.
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
