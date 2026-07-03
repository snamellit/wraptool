package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/paths"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig               `yaml:"server"`
	Tools      map[string]ToolConfig      `yaml:"tools"`
	MCPServers map[string]MCPServerConfig `yaml:"mcp_servers"`
}

type ServerConfig struct {
	Transport     string    `yaml:"transport"`
	Listen        string    `yaml:"listen"`
	AuthTokenFile string    `yaml:"auth_token_file"`
	LogLevel      string    `yaml:"log_level"`
	RequestLog    string    `yaml:"request_log"`
	Web           WebConfig `yaml:"web"`
}

// WebConfig configures the operator-facing request-review Web UI. It runs
// in the serve process on its own listener, independent of the AI-facing
// MCP transport. Leave Listen empty to disable the Web UI.
type WebConfig struct {
	Listen        string `yaml:"listen"`          // e.g. 127.0.0.1:8718 (bind localhost only)
	AuthTokenFile string `yaml:"auth_token_file"` // optional; Basic-auth password or Bearer token
}

type ToolConfig struct {
	Binary        string            `yaml:"binary"`
	WorkingDir    string            `yaml:"working_dir"`
	Env           map[string]string `yaml:"env"`
	EnvFiles      []string          `yaml:"env_files"`
	Timeout       time.Duration     `yaml:"timeout"`
	Introspection IntrospectConfig  `yaml:"introspection"`
	Allow         []AllowRule       `yaml:"allow"`
	Deny          []DenyRule        `yaml:"deny"`
}

// MCPServerConfig describes a local stdio MCP server that wraptool spawns and
// polices, deliberately parallel to ToolConfig. command/args/working_dir/env/
// env_files/timeout mirror ToolConfig so the shared env builder and mental
// model carry over. Policy is expressed as allow/deny lists of upstream tool
// names (default-deny, deny wins), not subcommands/flags.
type MCPServerConfig struct {
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	WorkingDir  string            `yaml:"working_dir"`
	Env         map[string]string `yaml:"env"`
	EnvFiles    []string          `yaml:"env_files"`
	Timeout     time.Duration     `yaml:"timeout"`      // per-call timeout
	InitTimeout time.Duration     `yaml:"init_timeout"` // handshake/startup budget
	NamePrefix  string            `yaml:"name_prefix"`  // optional; default = server key
	Allow       []MCPAllowRule    `yaml:"allow"`
	Deny        []MCPDenyRule     `yaml:"deny"`
}

// MCPAllowRule allow-lists a single upstream MCP tool by name. It is a distinct
// type from AllowRule (which is subcommand/flag shaped for CLI tools); the
// policy engine, config editors, and rule CRUD do not operate on it until
// extended in a later phase.
type MCPAllowRule struct {
	Tool string `yaml:"tool"`
}

// MCPDenyRule deny-lists a single upstream MCP tool by name. Deny takes
// precedence over allow. See MCPAllowRule for the type-distinction note.
type MCPDenyRule struct {
	Tool string `yaml:"tool"`
}

type IntrospectConfig struct {
	CacheTTL      time.Duration `yaml:"cache_ttl"`
	OverridesFile string        `yaml:"overrides_file"`
}

type AllowRule struct {
	Subcommand      []string                  `yaml:"subcommand"`
	Flags           []string                  `yaml:"flags"`
	DenyFlags       []string                  `yaml:"deny_flags"`
	FlagConstraints map[string]FlagConstraint `yaml:"flag_constraints"`
	ArgConstraints  *ArgConstraints           `yaml:"arg_constraints"`
}

type FlagConstraint struct {
	Required bool   `yaml:"required"`
	Type     string `yaml:"type"`
	Max      *int   `yaml:"max"`
	Min      *int   `yaml:"min"`
	Pattern  string `yaml:"pattern"`
	Deny     bool   `yaml:"deny"`
}

type ArgConstraints struct {
	PositionalMax int `yaml:"positional_max"`
}

type DenyRule struct {
	Subcommand []string `yaml:"subcommand"`
}

func Load(path string) (*Config, error) {
	path = expandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyDefaults()

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Transport == "" {
		c.Server.Transport = "stdio"
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Server.RequestLog == "" {
		c.Server.RequestLog = paths.DefaultRequestLog()
	} else {
		c.Server.RequestLog = expandPath(c.Server.RequestLog)
	}
	if c.Server.AuthTokenFile != "" {
		c.Server.AuthTokenFile = expandPath(c.Server.AuthTokenFile)
	}
	if c.Server.Web.AuthTokenFile != "" {
		c.Server.Web.AuthTokenFile = expandPath(c.Server.Web.AuthTokenFile)
	}

	for name, tool := range c.Tools {
		if tool.Timeout == 0 {
			tool.Timeout = 30 * time.Second
		}
		if tool.Introspection.CacheTTL == 0 {
			tool.Introspection.CacheTTL = 24 * time.Hour
		}
		if tool.Introspection.OverridesFile != "" {
			tool.Introspection.OverridesFile = expandPath(tool.Introspection.OverridesFile)
		}
		c.Tools[name] = tool
	}

	for name, srv := range c.MCPServers {
		if srv.Timeout == 0 {
			srv.Timeout = 30 * time.Second
		}
		if srv.InitTimeout == 0 {
			srv.InitTimeout = 10 * time.Second
		}
		if srv.NamePrefix == "" {
			srv.NamePrefix = name
		}
		for i, f := range srv.EnvFiles {
			srv.EnvFiles[i] = expandPath(f)
		}
		c.MCPServers[name] = srv
	}
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	return path
}

// ResolvedEnv returns the combined environment for a tool,
// loading env_files and merging with inline env vars.
// Inline env takes precedence over env_files.
func (tc *ToolConfig) ResolvedEnv() (map[string]string, error) {
	env := make(map[string]string)

	for _, envFile := range tc.EnvFiles {
		envFile = expandPath(envFile)
		data, err := os.ReadFile(envFile)
		if err != nil {
			return nil, fmt.Errorf("reading env file %s: %w", envFile, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if ok {
				env[k] = v
			}
		}
	}

	for k, v := range tc.Env {
		env[k] = v
	}

	return env, nil
}

// ResolvedEnv returns the combined environment for an MCP server, loading
// env_files and merging with inline env vars. Inline env takes precedence over
// env_files. Mirrors ToolConfig.ResolvedEnv.
func (mc *MCPServerConfig) ResolvedEnv() (map[string]string, error) {
	env := make(map[string]string)

	for _, envFile := range mc.EnvFiles {
		envFile = expandPath(envFile)
		data, err := os.ReadFile(envFile)
		if err != nil {
			return nil, fmt.Errorf("reading env file %s: %w", envFile, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if ok {
				env[k] = v
			}
		}
	}

	for k, v := range mc.Env {
		env[k] = v
	}

	return env, nil
}
