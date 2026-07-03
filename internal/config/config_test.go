package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Create a dummy binary for validation.
	binPath := filepath.Join(dir, "git")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfgContent := `
server:
  transport: stdio
  log_level: debug
tools:
  git:
    binary: ` + binPath + `
    timeout: 60s
    allow:
      - subcommand: [status]
        flags: ["--short"]
      - subcommand: [commit]
        flags: ["--message"]
        flag_constraints:
          "--message":
            required: true
    deny:
      - subcommand: [push]
`

	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.Transport != "stdio" {
		t.Errorf("transport = %q, want stdio", cfg.Server.Transport)
	}

	git, ok := cfg.Tools["git"]
	if !ok {
		t.Fatal("git tool not found")
	}

	if git.Binary != binPath {
		t.Errorf("binary = %q, want %q", git.Binary, binPath)
	}

	if len(git.Allow) != 2 {
		t.Errorf("len(allow) = %d, want 2", len(git.Allow))
	}

	if len(git.Deny) != 1 {
		t.Errorf("len(deny) = %d, want 1", len(git.Deny))
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error: %v", err)
	}
}

func TestValidate_InvalidTransport(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Transport: "grpc"},
		Tools: map[string]ToolConfig{
			"git": {Binary: "/usr/bin/git", Allow: []AllowRule{{Subcommand: []string{"status"}}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid transport")
	}
}

func TestValidate_SSERequiresListen(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Transport: "sse"},
		Tools: map[string]ToolConfig{
			"git": {Binary: "/usr/bin/git", Allow: []AllowRule{{Subcommand: []string{"status"}}}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for SSE without listen")
	}
}

func TestValidate_RelativeBinaryPath(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {Binary: "git", Allow: []AllowRule{{Subcommand: []string{"status"}}}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for relative binary path")
	}
}

func TestValidate_NoAllowRules(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "git")
	os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755)

	cfg := &Config{
		Server: ServerConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {Binary: binPath, Allow: []AllowRule{}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for no allow rules")
	}
}

func TestValidate_EmptySubcommand(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "git")
	os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755)

	cfg := &Config{
		Server: ServerConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {Binary: binPath, Allow: []AllowRule{{Subcommand: []string{}}}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for empty subcommand")
	}
}

func TestResolvedEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "test.env")
	os.WriteFile(envFile, []byte("KEY1=value1\nKEY2=value2\n# comment\n\nKEY3=value3"), 0600)

	tc := &ToolConfig{
		EnvFiles: []string{envFile},
		Env: map[string]string{
			"KEY2": "override", // should override env file
		},
	}

	env, err := tc.ResolvedEnv()
	if err != nil {
		t.Fatalf("ResolvedEnv() error: %v", err)
	}

	if env["KEY1"] != "value1" {
		t.Errorf("KEY1 = %q, want value1", env["KEY1"])
	}
	if env["KEY2"] != "override" {
		t.Errorf("KEY2 = %q, want override", env["KEY2"])
	}
	if env["KEY3"] != "value3" {
		t.Errorf("KEY3 = %q, want value3", env["KEY3"])
	}
}

func TestLoad_ValidMCPConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	binPath := filepath.Join(dir, "git")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "github.env")
	if err := os.WriteFile(envFile, []byte("GITHUB_TOKEN=fromfile\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cfgContent := `
server:
  transport: stdio
tools:
  git:
    binary: ` + binPath + `
    allow:
      - subcommand: [status]
mcp_servers:
  github:
    command: /usr/bin/github-mcp-server
    args: ["stdio"]
    working_dir: ` + dir + `
    env_files: ["` + envFile + `"]
    env:
      GITHUB_TOKEN: "inline"
    timeout: 45s
    init_timeout: 5s
    name_prefix: gh
    allow:
      - tool: create_issue
      - tool: get_file_contents
    deny:
      - tool: delete_repository
  local:
    command: /usr/bin/local-mcp
`

	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	gh, ok := cfg.MCPServers["github"]
	if !ok {
		t.Fatal("github mcp server not found")
	}
	if gh.Command != "/usr/bin/github-mcp-server" {
		t.Errorf("command = %q", gh.Command)
	}
	if len(gh.Args) != 1 || gh.Args[0] != "stdio" {
		t.Errorf("args = %v, want [stdio]", gh.Args)
	}
	if gh.WorkingDir != dir {
		t.Errorf("working_dir = %q, want %q", gh.WorkingDir, dir)
	}
	if gh.Timeout != 45*time.Second {
		t.Errorf("timeout = %v, want 45s", gh.Timeout)
	}
	if gh.InitTimeout != 5*time.Second {
		t.Errorf("init_timeout = %v, want 5s", gh.InitTimeout)
	}
	if gh.NamePrefix != "gh" {
		t.Errorf("name_prefix = %q, want gh", gh.NamePrefix)
	}
	if len(gh.Allow) != 2 || gh.Allow[0].Tool != "create_issue" {
		t.Errorf("allow = %v", gh.Allow)
	}
	if len(gh.Deny) != 1 || gh.Deny[0].Tool != "delete_repository" {
		t.Errorf("deny = %v", gh.Deny)
	}

	// Defaults applied to the server that omitted them.
	local := cfg.MCPServers["local"]
	if local.Timeout != 30*time.Second {
		t.Errorf("local timeout = %v, want default 30s", local.Timeout)
	}
	if local.InitTimeout != 10*time.Second {
		t.Errorf("local init_timeout = %v, want default 10s", local.InitTimeout)
	}
	if local.NamePrefix != "local" {
		t.Errorf("local name_prefix = %q, want default map key 'local'", local.NamePrefix)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestResolvedEnvMCP(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "test.env")
	os.WriteFile(envFile, []byte("KEY1=value1\nKEY2=value2\n# comment\n\nKEY3=value3"), 0600)

	mc := &MCPServerConfig{
		EnvFiles: []string{envFile},
		Env: map[string]string{
			"KEY2": "override", // should override env file
		},
	}

	env, err := mc.ResolvedEnv()
	if err != nil {
		t.Fatalf("ResolvedEnv() error: %v", err)
	}

	if env["KEY1"] != "value1" {
		t.Errorf("KEY1 = %q, want value1", env["KEY1"])
	}
	if env["KEY2"] != "override" {
		t.Errorf("KEY2 = %q, want override", env["KEY2"])
	}
	if env["KEY3"] != "value3" {
		t.Errorf("KEY3 = %q, want value3", env["KEY3"])
	}
}

// mcpBaseConfig returns a minimal valid config with one CLI tool and a
// customizable set of MCP servers.
func mcpBaseConfig(servers map[string]MCPServerConfig) *Config {
	return &Config{
		Server: ServerConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {
				Binary: os.Args[0],
				Allow:  []AllowRule{{Subcommand: []string{"add"}}},
			},
		},
		MCPServers: servers,
	}
}

func TestValidate_MCPServerMissingCommand(t *testing.T) {
	cfg := mcpBaseConfig(map[string]MCPServerConfig{
		"github": {NamePrefix: "gh"},
	})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("err = %v, want command is required", err)
	}
}

func TestValidate_MCPServerEmptyToolName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server MCPServerConfig
	}{
		{"empty allow", MCPServerConfig{Command: "/bin/x", NamePrefix: "gh", Allow: []MCPAllowRule{{Tool: ""}}}},
		{"whitespace allow", MCPServerConfig{Command: "/bin/x", NamePrefix: "gh", Allow: []MCPAllowRule{{Tool: "  "}}}},
		{"empty deny", MCPServerConfig{Command: "/bin/x", NamePrefix: "gh", Deny: []MCPDenyRule{{Tool: ""}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mcpBaseConfig(map[string]MCPServerConfig{"github": tc.server})
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "tool name is required") {
				t.Fatalf("err = %v, want tool name is required", err)
			}
		})
	}
}

func TestValidate_MCPServerPrefixCollision_SameServer(t *testing.T) {
	cfg := mcpBaseConfig(map[string]MCPServerConfig{
		"a": {Command: "/bin/a", NamePrefix: "dup"},
		"b": {Command: "/bin/b", NamePrefix: "dup"},
	})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("err = %v, want prefix collision", err)
	}
}

func TestValidate_MCPServerPrefixCollisionWithCLI(t *testing.T) {
	// CLI tool is named "git"; an MCP server prefix of "git" must be rejected.
	cfg := mcpBaseConfig(map[string]MCPServerConfig{
		"github": {Command: "/bin/x", NamePrefix: "git"},
	})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tools.git") {
		t.Fatalf("err = %v, want collision with tools.git", err)
	}
}

func TestValidate_MCPServerUnderscoreCollision(t *testing.T) {
	// github+create_issue and github_create+issue both resolve to the final
	// registered name github_create_issue.
	cfg := mcpBaseConfig(map[string]MCPServerConfig{
		"github":        {Command: "/bin/x", NamePrefix: "github", Allow: []MCPAllowRule{{Tool: "create_issue"}}},
		"github_create": {Command: "/bin/y", NamePrefix: "github_create", Allow: []MCPAllowRule{{Tool: "issue"}}},
	})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "github_create_issue") {
		t.Fatalf("err = %v, want final-name collision on github_create_issue", err)
	}
}

func TestValidate_MixedToolsAndMCPServers(t *testing.T) {
	cfg := mcpBaseConfig(map[string]MCPServerConfig{
		"github": {Command: "/bin/x", NamePrefix: "gh", Allow: []MCPAllowRule{{Tool: "create_issue"}}},
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestValidate_MCPServersOnly(t *testing.T) {
	// A config with no CLI tools but at least one MCP server is valid.
	cfg := &Config{
		Server: ServerConfig{Transport: "stdio"},
		MCPServers: map[string]MCPServerConfig{
			"github": {Command: "/bin/x", NamePrefix: "gh"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}
