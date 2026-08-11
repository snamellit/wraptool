package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/paths"

	"gopkg.in/yaml.v3"
)

type Config struct {
	MCP        MCPConfig                  `yaml:"mcp"`
	Admin      AdminConfig                `yaml:"admin"`
	Tools      map[string]ToolConfig      `yaml:"tools"`
	MCPServers map[string]MCPServerConfig `yaml:"mcp_servers"`
}

// MCPConfig configures the agent-facing MCP listener and request audit log.
type MCPConfig struct {
	Transport     string `yaml:"transport"`
	Listen        string `yaml:"listen"`
	AuthTokenFile string `yaml:"auth_token_file"`
	LogLevel      string `yaml:"log_level"`
	RequestLog    string `yaml:"request_log"`
	// AllowedCWDRoots, when non-empty, constrains a client-supplied working
	// directory to these roots (symlinks resolved); a cwd outside them is
	// refused. Empty = no containment (warn-only). See security audit F5.
	AllowedCWDRoots []string `yaml:"allowed_cwd_roots"`
}

// AdminConfig configures the operator-facing request-review UI. It runs in the
// serve process but is a separate listener and authentication boundary from MCP.
// Leave Listen empty to disable the administration UI.
type AdminConfig struct {
	Listen        string `yaml:"listen"`          // e.g. 127.0.0.1:8718 (bind localhost only)
	AuthTokenFile string `yaml:"auth_token_file"` // required when Listen is set; Bearer token for UI access
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
	Required bool           `yaml:"required"`
	Type     ConstraintType `yaml:"type"`
	Max      *int           `yaml:"max"`
	Min      *int           `yaml:"min"`
	Pattern  string         `yaml:"pattern"`
	Deny     bool           `yaml:"deny"`
}

// ConstraintType is the value-typing of a flag constraint. It is a closed enum:
// UnmarshalYAML rejects any value outside this set at decode time, so an illegal
// type cannot exist in a loaded config — the single source of truth for the
// legal set lives here (design/policy/flag-constraint-type-single-source.md).
type ConstraintType string

const (
	ConstraintUntyped ConstraintType = ""        // no value typing; pattern-only
	ConstraintString  ConstraintType = "string"  // any value; pattern still applies
	ConstraintInteger ConstraintType = "integer" // parsed + range-checked
	ConstraintBoolean ConstraintType = "boolean" // valueless; rendered as a bare flag
)

// UnmarshalYAML rejects any constraint type outside the closed set, so a
// misspelled or unsupported type (e.g. `type: bolean`) fails when the config is
// parsed rather than silently at call time. An absent `type:` key never reaches
// here and stays ConstraintUntyped.
func (t *ConstraintType) UnmarshalYAML(n *yaml.Node) error {
	// Reject non-scalars first: a sequence or mapping (e.g. `type: [boolean]`)
	// has an empty n.Value, which would otherwise match ConstraintUntyped and be
	// silently accepted.
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("flag constraint type must be a scalar string, got a non-scalar YAML value at line %d", n.Line)
	}
	switch ConstraintType(n.Value) {
	case ConstraintUntyped, ConstraintString, ConstraintInteger, ConstraintBoolean:
		*t = ConstraintType(n.Value)
		return nil
	default:
		return fmt.Errorf("unknown flag constraint type %q (want string, integer, or boolean)", n.Value)
	}
}

// ConstraintKind is the derived semantics of a ConstraintType. It keeps the two
// axes separate: JSONSchema is presentation (read only by mcptools), while
// Valueless and Numeric are enforcement (read only by whitelist). Both layers
// derive from this single definition, so they cannot drift — the failure mode
// that let a boolean constraint render one way and be enforced another — and no
// consumer ever infers one axis from the other's field.
type ConstraintKind struct {
	JSONSchema string // presentation: MCP tool-schema type ("string"|"integer"|"boolean")
	Valueless  bool   // enforcement: bare switch, no value to validate
	Numeric    bool   // enforcement: value is parsed as an integer and range-checked
}

// Kind returns the semantics of the constraint type. The switch is exhaustive
// over the ConstraintType enum, so adding a member without giving it a Kind
// fails the exhaustive linter — that is the single-source-of-truth guarantee,
// enforced at build time with no shared mutable table. ok is false only for a
// value outside the enum, which UnmarshalYAML already prevents in a loaded
// config.
func (t ConstraintType) Kind() (ConstraintKind, bool) {
	switch t {
	case ConstraintInteger:
		return ConstraintKind{JSONSchema: "integer", Numeric: true}, true
	case ConstraintBoolean:
		return ConstraintKind{JSONSchema: "boolean", Valueless: true}, true
	case ConstraintString, ConstraintUntyped:
		return ConstraintKind{JSONSchema: "string"}, true
	default:
		return ConstraintKind{}, false
	}
}

type ArgConstraints struct {
	PositionalMax int `yaml:"positional_max"`
}

type DenyRule struct {
	Subcommand []string `yaml:"subcommand"`
}

func Load(path string) (*Config, error) {
	return load(path, false)
}

// LoadStrict loads and defaults the config like Load, but rejects unknown or
// misspelled keys (yaml KnownFields) instead of silently ignoring them. It backs
// the security-critical paths — `serve`, config reload, and `wraptool config
// validate` — so a typo'd key (e.g. flag_constraint vs flag_constraints) fails
// loudly instead of silently dropping a rule and weakening the policy. The
// lenient Load remains for read-only/forward-compatible consumers (up, requests,
// server status, web UI) that don't gate execution.
func LoadStrict(path string) (*Config, error) {
	return load(path, true)
}

func load(path string, strict bool) (*Config, error) {
	path = expandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(strict)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF means an empty document — treat as a zero config, matching the
		// lenient yaml.Unmarshal behaviour Load previously had.
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyDefaults()

	return &cfg, nil
}

// HelpFlags are the universal help flags. wraptool seeds them into every allow
// rule's Flags at load, so `-h`/`--help` are always callable and introspectable
// on any approved subcommand — the sole exception being an operator who lists a
// help flag under deny_flags to revoke it (deny wins). This is the single source
// of that behaviour: the policy engine and the MCP schema builder just read the
// resulting rule.Flags with no help-specific logic of their own.
var HelpFlags = []string{"-h", "--help"}

// SeedHelpFlags returns flags with HelpFlags folded in and denyFlags removed —
// order-stable and de-duplicated. Configured flags keep their order and come
// first; the help flags are appended; anything listed in denyFlags (a help flag
// included) is dropped, so deny takes precedence over allow. Idempotent, so it is
// safe to apply more than once.
func SeedHelpFlags(flags, denyFlags []string) []string {
	denied := make(map[string]bool, len(denyFlags))
	for _, d := range denyFlags {
		denied[d] = true
	}
	seen := make(map[string]bool, len(flags)+len(HelpFlags))
	out := make([]string, 0, len(flags)+len(HelpFlags))
	add := func(f string) {
		if seen[f] || denied[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	for _, f := range flags {
		add(f)
	}
	for _, f := range HelpFlags {
		add(f)
	}
	return out
}

func (c *Config) applyDefaults() {
	if c.MCP.Transport == "" {
		// sse on loopback is the user-facing default. stdio is a TEST-ONLY
		// transport and is never selected implicitly — see CLAUDE.md.
		c.MCP.Transport = "sse"
		if c.MCP.Listen == "" {
			c.MCP.Listen = "127.0.0.1:8717"
		}
	}
	if c.MCP.LogLevel == "" {
		c.MCP.LogLevel = "info"
	}
	if c.MCP.RequestLog == "" {
		c.MCP.RequestLog = paths.DefaultRequestLog()
	} else {
		c.MCP.RequestLog = expandPath(c.MCP.RequestLog)
	}
	if c.MCP.AuthTokenFile != "" {
		c.MCP.AuthTokenFile = expandPath(c.MCP.AuthTokenFile)
	}
	if c.Admin.AuthTokenFile != "" {
		c.Admin.AuthTokenFile = expandPath(c.Admin.AuthTokenFile)
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
		// Seed the universal help flags into every allow rule so -h/--help are
		// always permitted and introspectable on any approved subcommand, unless
		// explicitly revoked via deny_flags. Single source: the policy engine and
		// the MCP schema builder both read the resulting rule.Flags, so no
		// help-specific logic lives anywhere downstream.
		for i := range tool.Allow {
			tool.Allow[i].Flags = SeedHelpFlags(tool.Allow[i].Flags, tool.Allow[i].DenyFlags)
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
		// EnvFiles paths are expanded in resolveEnv at read time (same as CLI
		// ToolConfig), so no pre-expansion here.
		c.MCPServers[name] = srv
	}
}

// expandPath expands a leading ~ to the user's home directory. Handles both
// bare "~" and "~/...", matching cmd.expandTilde so a token file authored as
// `~` resolves the same way at config load and at `up` runtime.
func expandPath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
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
	return resolveEnv(tc.EnvFiles, tc.Env)
}

// ResolvedEnv returns the combined environment for an MCP server, loading
// env_files and merging with inline env vars. Inline env takes precedence over
// env_files.
func (mc *MCPServerConfig) ResolvedEnv() (map[string]string, error) {
	return resolveEnv(mc.EnvFiles, mc.Env)
}

// resolveEnv loads each env file (KEY=VALUE lines; blanks and # comments
// skipped) then overlays the inline env, which takes precedence. Shared by the
// CLI-tool and MCP-server config so both resolve credentials identically.
func resolveEnv(envFiles []string, inline map[string]string) (map[string]string, error) {
	env := make(map[string]string)

	for _, envFile := range envFiles {
		envFile = expandPath(envFile)
		data, err := os.ReadFile(envFile)
		if err != nil {
			return nil, fmt.Errorf("reading env file %s: %w", envFile, err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
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

	maps.Copy(env, inline)

	return env, nil
}
