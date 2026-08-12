package introspect

import (
	"regexp"
	"strings"
)

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
	// byte-length success threshold, yielded NO flags to any parser, and does
	// not have the shape of a help page — see looksLikeHelp and
	// classifySubcommandHelp for why all three conditions are required. Flags
	// is always empty here and coverage is always zero: the classifier stores
	// nothing derived from text it just declared was not help.
	//
	// The distinction this carries is the one drift depends on: an absent flag
	// under "garbage" means "could not tell", never "confirmed absent".
	ParseOutcomeGarbage ParseOutcome = "garbage"
	// ParseOutcomeParsed means help output was obtained and has the shape of
	// a real help page. Flags/CoverageTokens/CoverageFound reflect a genuine
	// parse attempt against real help text — coverage still gates whether
	// the ABSENCE of any one flag may be asserted; ParseOutcomeParsed alone
	// means the input was trustworthy, not that the parse found everything
	// in it.
	ParseOutcomeParsed ParseOutcome = "parsed"
)

// twoColumnLineRe matches a line with the shape both a flag-description line
// ("  -v, --verbose  explain") and a subcommand-catalog line
// ("  clone      Clone a repository...") share: 2+ leading spaces, a
// non-space token, then a further 2+-space gap, then more text. This is the
// same "indented, 2+ spaces between columns" convention subcommandRe,
// gitFlagWithDescRe, and gnuFlagRe already parse against — reused here only
// as a coarse SHAPE check, not a field extractor.
var twoColumnLineRe = regexp.MustCompile(`^\s{2,}\S.*\s{2,}\S`)

// usageLineRe matches a line beginning (after leading whitespace) with
// "usage" case-insensitively followed by ':' or a space — go's
// `usage: go build ...`, git's `usage: git log ...`, GNU's
// `Usage: widget [OPTION]...`.
var usageLineRe = regexp.MustCompile(`(?i)^\s*usage[:\s]`)

// looksLikeHelp reports whether text has POSITIVE help-page structure, not
// merely "more than one line" — a two-line CLI error page and a genuinely
// flagless help page both clear a bare line-count bound identically, which
// is the exact failure design/doctor/wraptool-doctor.md's own review found:
// "handle this one more case" only relocates the defect that already lived
// in the line-count version of this function. text counts as help-shaped
// when it has ANY of:
//
//  1. a flag DEFINITION line: a line whose first non-whitespace character is
//     '-' (hasFlagDefinitionLine) — deliberately narrower than
//     flagShapeTokens' anywhere-in-text scan, and on purpose: `git bogus
//     -h`'s error text is "git: 'bogus' is not a git command. See 'git
//     --help'." — it contains the substring "--help" in PROSE, which an
//     anywhere-in-text scan would misread as a flag listing and defeat the
//     entire point of ParseOutcomeGarbage. A prose mention is never the
//     first character on its line; a real flag-description line always is.
//  2. an indented two-column line (twoColumnLineRe) — the shape of a flag
//     listing OR a subcommand catalog listing, so a subcommand whose help
//     lists only further subcommands (no flags of its own) still counts.
//  3. a usage line (usageLineRe) followed by a FURTHER non-blank section —
//     at least one more non-blank line after the blank line that
//     conventionally separates a usage synopsis from the rest of the page.
//     This is the shape a genuinely flagless command's help still has:
//     `go build -h` prints "usage: go build [-o output] [build flags]
//     [packages]", a blank line, then "Use 'go help build' for more
//     information." — no flag definition line, no two-column block, but
//     still a real (if terse) help page, not an error dressed as one.
//
// What this deliberately CANNOT decide: a single-line usage synopsis with
// nothing after it ("usage: foo"), or help text that violates every listed
// convention (no leading "usage", no indented columns, no dash-led lines) —
// both read as "not help" here. That is the conservative direction to be
// wrong in: a real help page failing this check costs one entry marked
// ParseOutcomeGarbage (Coverage/Flags simply not trusted, degrading to
// drift.unverifiable); a garbage page passing it would let a false
// drift.unknown-flag or drift.unknown-subcommand finding through, which is
// the failure mode this whole function exists to prevent.
func looksLikeHelp(text string) bool {
	if hasFlagDefinitionLine(text) {
		return true
	}

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if twoColumnLineRe.MatchString(line) {
			return true
		}
	}

	// A usage line plus at least one further non-blank line. Deliberately NOT
	// "usage, then a BLANK line, then more": `go build -h` on go 1.26.5 prints
	// its two lines with nothing between them, so requiring the blank rejected
	// a genuine help page — the earlier fixture that appeared to prove
	// otherwise had invented the blank line.
	sawUsage := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if usageLineRe.MatchString(line) {
			sawUsage = true
			continue
		}
		if sawUsage && trimmed != "" {
			return true
		}
	}
	return false
}

// hasFlagDefinitionLine reports whether any line of text, once its leading
// whitespace is trimmed, begins with '-' — the shape of a real flag
// definition line ("-v, --verbose ...", "--[no-]amend ..."), as opposed to a
// flag NAME merely mentioned somewhere inside a sentence. See looksLikeHelp's
// own doc comment for why this must be narrower than flagShapeTokens.
func hasFlagDefinitionLine(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-") {
			return true
		}
	}
	return false
}

// flagShapeLongRe matches a long-flag-SHAPED token: --word, --[no-]word, or
// the flag-name prefix of --word[=...]/--word=VALUE (the bracket/value
// suffix, if any, is not part of the match — flagShapeTokens normalizes the
// [no-] form the same way cleanNoPrefix does, so a token compares equal to
// the parsers' own Flags map keys, which are already [no-]-stripped).
var flagShapeLongRe = regexp.MustCompile(`--(?:\[no-\])?[A-Za-z][\w-]*`)

// flagShapeTokens returns every option-shaped token found ANYWHERE in text:
// long flags via flagShapeLongRe, plus short flags (a lone -x) found by
// shortFlagTokens. This is the coverage DENOMINATOR's vocabulary
// (flagCoverage) — deliberately broader-recall than looksLikeHelp's
// hasFlagDefinitionLine, which only asks "is there a flag DEFINITION line at
// all" and stops there; flagShapeTokens instead needs to find every
// individual flag mention so coverage can measure how many of them the
// winning parse actually captured, including shapes the previous
// (long-only, no-[no-]-support) denominator was blind to: --[no-]x, --x[=...],
// and short -x. A denominator built by scanning position-blind is safe here
// specifically because flagCoverage only ever runs on text looksLikeHelp has
// already confirmed is a genuine help page — an isolated "--help" mention in
// PROSE cannot reach this function the way it could reach a naive
// looksLikeHelp.
func flagShapeTokens(text string) []string {
	var tokens []string
	for _, line := range strings.Split(text, "\n") {
		for _, m := range flagShapeLongRe.FindAllString(line, -1) {
			tokens = append(tokens, cleanNoPrefix(m))
		}
		tokens = append(tokens, shortFlagTokens(line)...)
	}
	return tokens
}

// shortFlagTokens scans one line for standalone short-flag tokens: a '-'
// immediately followed by a single ASCII letter, where that '-' is not
// itself preceded by another flag-word byte (which would make it the SECOND
// dash of a long flag, as in "--fail", or a hyphen inside a compound word,
// as in "decorate-refs") and the letter is not itself followed by another
// flag-word byte (which would make it the start of a longer word, not a
// lone short flag).
func shortFlagTokens(line string) []string {
	var tokens []string
	for i := 0; i+1 < len(line); i++ {
		if line[i] != '-' {
			continue
		}
		if i > 0 && isFlagWordByte(line[i-1]) {
			continue
		}
		c := line[i+1]
		if !isASCIILetter(c) {
			continue
		}
		if i+2 < len(line) && isFlagWordByte(line[i+2]) {
			continue
		}
		tokens = append(tokens, "-"+string(c))
	}
	return tokens
}

func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// isFlagWordByte reports whether b could plausibly continue a longer
// flag/word token (letter, digit, '-', or '_'), used by shortFlagTokens to
// find a lone short flag's boundaries.
func isFlagWordByte(b byte) bool {
	return isASCIILetter(b) || (b >= '0' && b <= '9') || b == '-' || b == '_'
}

// flagCoverage answers "how much of this help did the winning parse actually
// understand": found is how many of the DISTINCT option-shaped tokens
// (flagShapeTokens) in helpText ended up as a key in flags (a long token) or
// matched some Flag's Short (a short token); total is how many distinct
// tokens flagShapeTokens found at all — the coverage denominator. It is
// deliberately not confidence — see Subcommand's own doc comment for why
// confidence is the wrong signal — but a bound derived from the text
// itself.
//
// total == 0 means UNMEASURABLE: flagShapeTokens found nothing to check the
// parse against, which is NOT the same as "the parse captured everything".
// Reporting that case as a plain 1.0 fraction (the previous shape of this
// function) is exactly the false confidence design/doctor/wraptool-doctor.md
// §4.2 item 4 exists to rule out — `go build -h`'s two-line, largely
// flagless help would otherwise be indistinguishable from a genuinely
// verified "help succeeded, zero flags, high confidence" result. Returning
// raw counts instead of a precomputed fraction lets a caller (F4) tell
// "verified: N of N flags found" from "nothing here to verify against" for
// itself, rather than trusting a single float to already have made that
// call.
func flagCoverage(helpText string, flags map[string]Flag) (found, total int) {
	seen := make(map[string]bool)
	for _, tok := range flagShapeTokens(helpText) {
		seen[tok] = true
	}
	total = len(seen)
	for tok := range seen {
		if flagTokenFound(tok, flags) {
			found++
		}
	}
	return found, total
}

// flagTokenFound reports whether token — a long flag name (already
// [no-]-normalized by flagShapeTokens) or a short flag ("-x") — is present
// in flags: as a map key for a long token, or as some entry's Short for a
// short one. Flags is keyed by long name only, so a short-only flag (git
// log -h's "-L <range:file>", which GitParser's own regexes never match at
// all, long-form being mandatory in all three of them) is correctly reported
// as NOT found — flagCoverage must not silently credit the parse with a flag
// it never captured under any key.
func flagTokenFound(token string, flags map[string]Flag) bool {
	if strings.HasPrefix(token, "--") {
		_, ok := flags[token]
		return ok
	}
	for _, f := range flags {
		if f.Short == token {
			return true
		}
	}
	return false
}
