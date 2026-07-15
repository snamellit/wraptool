package introspect

// Parser attempts to extract flag information from CLI help text.
type Parser interface {
	// Name returns a human-readable name for this parser.
	Name() string

	// ParseSubcommands extracts top-level subcommand names and descriptions
	// from the help output of the root command.
	ParseSubcommands(helpText string) map[string]string

	// ParseFlags extracts flags from the help output of a specific subcommand.
	// Returns a ParseResult with extracted flags and a confidence score.
	ParseFlags(helpText string) ParseResult
}

// AllParsers returns the parser pipeline in priority order.
func AllParsers() []Parser {
	return []Parser{
		&GitParser{},
		&GNUParser{},
		&FallbackParser{},
	}
}

// demoteBooleansIfNoPlaceholders relaxes valueless flags to the UNKNOWN type
// ("") when the parsed help showed no value placeholder on ANY flag. Some CLIs
// (e.g. glab) render every flag without a placeholder, so a value-taking flag
// looks identical to a switch — there is no signal to tell them apart, and
// asserting "boolean" would make the value flags uncarriable. Emitting "" lets
// the schema builder default them to string (recoverable: a switch mistyped
// string still works as a bare flag) while its well-known-boolean allowlist
// still catches genuine switches by name. When the help DID use placeholders
// (curl, git), the absence of one is a reliable "boolean", so those are left
// untouched. Authoritative types still come from flag_constraints (see
// design/flag-type-inference.md).
func demoteBooleansIfNoPlaceholders(flags map[string]Flag) {
	for _, f := range flags {
		if f.TakesValue {
			return // the help uses placeholders — trust that valueless means boolean
		}
	}
	for name, f := range flags {
		if f.Type == "boolean" {
			f.Type = ""
			flags[name] = f
		}
	}
}

// BestParse runs all parsers against the help text and returns the
// result with the highest confidence.
func BestParse(helpText string) ParseResult {
	var best ParseResult
	for _, p := range AllParsers() {
		result := p.ParseFlags(helpText)
		if result.Confidence > best.Confidence {
			best = result
		}
	}
	return best
}
