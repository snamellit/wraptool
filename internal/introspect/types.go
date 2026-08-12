package introspect

import "time"

// Schema represents the introspected structure of a CLI tool.
type Schema struct {
	Binary        string `json:"binary"`
	VersionOutput string `json:"version_output,omitempty"`
	// ConfigHash is a stable hash of the introspection-relevant config
	// (binary + allowed subcommands + overrides file) that produced this schema.
	// The cache is reused only when it still matches — so editing the config
	// regenerates the schema. See design/introspect/introspection-cache-invalidation.md.
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
	// FormatVersion is the on-disk shape of this Schema, stamped by SaveCache
	// on every write (a caller-supplied value is always overwritten — there is
	// exactly one writer of truth). Bumped whenever Schema or Subcommand grows
	// a field a pre-bump cache entry could be silently MISREAD under: a missing
	// JSON field decodes to its Go zero value rather than erroring, which for
	// Subcommand.CoverageTokens/CoverageFound/ParseOutcome (added at format 2,
	// reshaped from a single float Coverage field to this pair at format 3 —
	// see their own doc comments) is indistinguishable from a genuine "probed
	// and found nothing" result. LoadCache and LoadCacheStale both reject any
	// Schema whose FormatVersion doesn't match schemaFormatVersion, forcing a
	// full re-introspection instead of serving a schema shaped like an older
	// format. See design/doctor/wraptool-doctor.md §4.2 items 1 and 4.
	FormatVersion int `json:"format_version"`
}

// Subcommand describes a single subcommand of a CLI tool.
type Subcommand struct {
	Description string          `json:"description"`
	Flags       map[string]Flag `json:"flags"`
	// CoverageTokens is how many DISTINCT option-shaped tokens
	// (flagShapeTokens: --x, --[no-]x, --x[=...], short -x) were found
	// ANYWHERE in the subcommand's help text — the coverage denominator.
	// CoverageFound is how many of those ended up as a key in Flags (or
	// matched some Flag's Short) — the numerator. "How much of this help did
	// I actually understand", not which parser ran. See flagCoverage's doc
	// comment for why this pair — raw counts, not a precomputed fraction —
	// replaces ParseResult.Confidence as the persisted quality signal
	// (design/doctor/wraptool-doctor.md §4.2 item 4): Confidence is a
	// four-value step function of parser IDENTITY (0.3 fallback, 0.6/0.7
	// GNU, 0.8/0.9 git, gradated only by matchCount > 5) — `git log -h`
	// parses 7 flags at the maximum 0.90 while missing
	// --oneline/--graph/--pretty/--since/--author entirely, so thresholding
	// on it produces maximum-confidence false positives. Confidence still
	// decides which PARSER wins (BestParse); it is simply never persisted,
	// because nothing downstream can act on "which parser" as a trust
	// signal.
	//
	// CoverageTokens == 0 means UNMEASURABLE — flagShapeTokens found nothing
	// in the text to check the parse against, which is NOT the same as "the
	// parse captured everything". A caller must check that before treating
	// CoverageFound == CoverageTokens as "verified, no gaps": a fraction
	// computed naively (found/total with total defaulting to 1) would make
	// an unmeasurable subcommand indistinguishable from a genuinely
	// zero-flag one at full confidence — exactly the false-confidence bug
	// this pair of fields replaces. Both fields are meaningless (0, 0)
	// unless ParseOutcome == ParseOutcomeParsed — a caller MUST check
	// ParseOutcome first.
	CoverageTokens int `json:"coverage_tokens"`
	CoverageFound  int `json:"coverage_found"`
	// ParseOutcome records how this subcommand's help was obtained and
	// classified, so a caller can tell "verified: no flags" from "could not
	// tell" — an empty Flags map alone is ambiguous between them. See
	// ParseOutcome's own doc comment (design/doctor/wraptool-doctor.md §4.2
	// item 5).
	ParseOutcome ParseOutcome `json:"parse_outcome,omitempty"`
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
