package introspect

import "strings"

// ParseOutcome records how a subcommand's help was obtained and classified
// during introspection, closing the gap design/doctor/wraptool-doctor.md
// §4.2 item 5 names: an empty Subcommand.Flags map alone cannot tell three
// very different situations apart —
//
//  1. help succeeded and the subcommand genuinely has no flags,
//  2. help failed outright (no output was obtained at all), and
//  3. help "succeeded" with garbage that is not a help page — e.g.
//     `git bogus -h` prints "git: 'bogus' is not a git command. See 'git
//     --help'." (53 bytes, one line), which clears getHelp's 20-byte success
//     threshold and would otherwise be parsed as if it were real help.
//
// Absence (drift.unknown-flag) may only ever be asserted from case 1 — never
// from 2 or 3, which look identical to it in Flags alone. ParseOutcome is
// the persisted, positive signal that tells them apart.
type ParseOutcome string

const (
	// ParseOutcomeUnknown is the zero value: no probe has recorded an
	// outcome for this subcommand entry. A caller must treat it exactly
	// like ParseOutcomeHelpFailed — assume nothing. Introspect itself never
	// leaves a Subcommand it wrote at this zero value; the only way a caller
	// legitimately observes it is a hand-built Subcommand (e.g. an override
	// merged for a subcommand with no prior probe, see mergeOverrides) or a
	// cache entry from before ParseOutcome existed — though schemaFormatVersion
	// (bumped alongside this type) means the latter is never returned by
	// LoadCache/LoadCacheStale in the first place, so it can't masquerade as
	// a genuine "unknown" result from a current-format cache.
	ParseOutcomeUnknown ParseOutcome = ""
	// ParseOutcomeHelpFailed means getHelp returned an error for every form
	// it tried (-h, --help, "help <subcommand>") — no output was obtained at
	// all. Flags is empty because nothing ran, not because the subcommand
	// has none.
	ParseOutcomeHelpFailed ParseOutcome = "help_failed"
	// ParseOutcomeGarbage means getHelp returned output that cleared its
	// byte-length success threshold but does not have the SHAPE of a help
	// page — see looksLikeHelp. Whatever Flags/Coverage this entry carries
	// must not be trusted; the input they were derived from was not help
	// text.
	ParseOutcomeGarbage ParseOutcome = "garbage"
	// ParseOutcomeParsed means help output was obtained and has the shape of
	// a real help page. Flags/Coverage reflect a genuine parse attempt
	// against real help text — Coverage still gates whether the ABSENCE of
	// any one flag may be asserted; ParseOutcomeParsed alone means the input
	// was trustworthy, not that the parse found everything in it.
	ParseOutcomeParsed ParseOutcome = "parsed"
)

// looksLikeHelp reports whether text has the structural SHAPE of a real help
// page: more than one non-blank line. This is a bound, not a per-tool or
// per-vocabulary heuristic — a genuine --help/-h page always carries at
// least a usage line plus something else (a description, an options
// listing), while a CLI error message is exactly one line: `git bogus -h`
// prints "git: 'bogus' is not a git command. See 'git --help'." — one line,
// 53 bytes, well past getHelp's 20-byte success threshold. looksLikeHelp
// does not try to recognize every possible garbage output; it draws the one
// line beneath which nothing could possibly be a flag-and-description
// catalog, which is exactly the property drift.unknown-flag needs never to
// get wrong.
func looksLikeHelp(text string) bool {
	nonBlank := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		nonBlank++
		if nonBlank > 1 {
			return true
		}
	}
	return false
}

// flagCoverage answers "how much of this help did the winning parse actually
// understand": the fraction of long-flag-shaped tokens appearing ANYWHERE in
// helpText that ended up as a key in flags. It is deliberately not
// confidence — see Subcommand.Coverage's own doc comment for why confidence
// is the wrong signal — but a bound derived from the text itself.
//
// fallbackFlagRe (parser_fallback.go) already defines the maximal-recall
// scan "anything that looks like a flag, anywhere", which FallbackParser
// itself uses as introspection's own last resort; reusing it here as the
// coverage denominator needs no new vocabulary and no per-tool
// special-casing — the same regex plays "what's the universe of flags in
// this text" for both purposes.
//
// A text with zero flag-shaped tokens returns 1.0 (nothing to miss).
// Combined with ParseOutcomeParsed, that is exactly the "help succeeded, the
// subcommand genuinely has no flags" state item 5 asks to make
// distinguishable from a failed or garbage parse — a coverage of 1.0 with no
// tokens found is trustworthy precisely because looksLikeHelp already
// confirmed the input had the shape of real help text.
func flagCoverage(helpText string, flags map[string]Flag) float64 {
	matches := fallbackFlagRe.FindAllStringSubmatch(helpText, -1)
	if len(matches) == 0 {
		return 1.0
	}

	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		seen[m[1]] = true
	}

	found := 0
	for token := range seen {
		if _, ok := flags[token]; ok {
			found++
		}
	}
	return float64(found) / float64(len(seen))
}
