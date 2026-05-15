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
