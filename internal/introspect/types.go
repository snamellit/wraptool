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
