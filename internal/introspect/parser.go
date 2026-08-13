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
// design/policy/flag-type-inference.md).
//
// Applied by BestParse to the WINNING result, not by the individual parsers —
// which parser wins is data-dependent (GitParser also handles cobra-style help
// and wins for glab), so demoting anywhere but the final result silently misses
// whichever parser happened to win.
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

// BestParseSubcommands runs the parsers against a tool's top-level help and
// returns the first non-empty subcommand catalog in parser priority order.
// There is no confidence score to compare (ParseSubcommands returns a bare
// map), and the parsers are already ordered most- to least-specific, so first
// non-empty is the analogue of BestParse's highest-confidence. Most tools yield
// nothing here — only GitParser recognises a subcommand listing, and a flags-
// only help (curl, GNU utilities) correctly produces no catalog at all.
func BestParseSubcommands(helpText string) map[string]string {
	for _, p := range AllParsers() {
		if subs := p.ParseSubcommands(helpText); len(subs) > 0 {
			return subs
		}
	}
	return nil
}

// BestParseCatalog is BestParseSubcommands for a per-tool CATALOG argv -- the
// command a knowledge-table entry names precisely because it lists everything
// the tool dispatches (git help -a). It additionally admits description-less
// entries, which is how git prints a PATH-installed external command.
//
// Kept apart from BestParseSubcommands on purpose. That one parses an
// arbitrary tool's top-level -h into Schema.Available, which wraptool_discover
// reports to the AI, so a relaxation there can put a non-subcommand in front of
// the agent -- a cobra help whose body is uniformly indented and carries an
// "Environment:" section would contribute env var names. Here the argv was
// chosen and vetted per tool, so the bare-name shape is expected rather than
// tolerated.
func BestParseCatalog(helpText string) map[string]string {
	for _, p := range AllParsers() {
		if g, ok := p.(*GitParser); ok {
			g.AllowNameOnly = true
			subs := g.ParseSubcommands(helpText)
			g.AllowNameOnly = false
			if len(subs) > 0 {
				return subs
			}
			continue
		}
		if subs := p.ParseSubcommands(helpText); len(subs) > 0 {
			return subs
		}
	}
	return nil
}

// BestParse runs all parsers against the help text and returns the highest-
// confidence result, then relaxes its placeholder-less booleans (see
// demoteBooleansIfNoPlaceholders) — on the winner, so the choice of parser
// doesn't change the flag typing.
func BestParse(helpText string) ParseResult {
	var best ParseResult
	for _, p := range AllParsers() {
		result := p.ParseFlags(helpText)
		if result.Confidence > best.Confidence {
			best = result
		}
	}
	demoteBooleansIfNoPlaceholders(best.Subcommand.Flags)
	return best
}
