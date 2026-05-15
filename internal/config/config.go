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
	Server ServerConfig          `yaml:"server"`
	Tools  map[string]ToolConfig `yaml:"tools"`
}

type ServerConfig struct {
	Transport     string `yaml:"transport"`
	Listen        string `yaml:"listen"`
	AuthTokenFile string `yaml:"auth_token_file"`
	LogLevel      string `yaml:"log_level"`
	RequestLog    string `yaml:"request_log"`
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

type IntrospectConfig struct {
	CacheTTL      time.Duration `yaml:"cache_ttl"`
	OverridesFile string        `yaml:"overrides_file"`
}

type AllowRule struct {
	Subcommand      []string                   `yaml:"subcommand"`
	Flags           []string                   `yaml:"flags"`
	DenyFlags       []string                   `yaml:"deny_flags"`
	FlagConstraints map[string]FlagConstraint  `yaml:"flag_constraints"`
	ArgConstraints  *ArgConstraints            `yaml:"arg_constraints"`
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
