package introspect

import "time"

// Schema represents the introspected structure of a CLI tool.
type Schema struct {
	Binary        string `json:"binary"`
	VersionOutput string `json:"version_output,omitempty"`
	// ConfigHash is a stable hash of the introspection-relevant config
	// (binary + allowed subcommands + overrides file) that produced this schema.
	// The cache is reused only when it still matches — so editing the config
	// regenerates the schema. See design/introspection-cache-invalidation.md.
	ConfigHash     string                `json:"config_hash,omitempty"`
	IntrospectedAt time.Time             `json:"introspected_at"`
	Subcommands    map[string]Subcommand `json:"subcommands"`
	// Available is the tool's advertised subcommand catalog (name →
	// description), parsed from its top-level help. Unlike Subcommands it is
	// independent of the allow rules: it is what the tool *offers*, not what
	// policy permits, and it is what wraptool_discover serves so an operator's
	// AI can name a real subcommand in a capability request instead of guessing.
	// Empty for tools whose help advertises no subcommands (most GNU tools).
	Available map[string]string `json:"available,omitempty"`
}

// Subcommand describes a single subcommand of a CLI tool.
type Subcommand struct {
	Description string          `json:"description"`
	Flags       map[string]Flag `json:"flags"`
}

// Flag describes a single flag of a CLI subcommand.
type Flag struct {
	Short       string `json:"short,omitempty"`
	Type        string `json:"type"` // "string", "boolean", "integer"
	Description string `json:"description"`
	TakesValue  bool   `json:"takes_value"`
}

// ParseResult is returned by a parser with a confidence score.
type ParseResult struct {
	Subcommand Subcommand
	Confidence float64 // 0.0 to 1.0
}
