package introspect

import (
	"regexp"
	"strings"
)

// GitParser handles Git-style help output (also used by tools built with
// Git's parse-options: git -h produces structured flag listings).
// This also handles Cobra-style help which is similar enough.
//
// Patterns matched:
//   - Subcommands: "   name      description" (indented, 2+ spaces between)
//   - Flags from -h: "    -s, --[no-]long <value>  description"
//   - Flags from -h: "    --[no-]flag              description"
type GitParser struct{}

func (p *GitParser) Name() string { return "git" }

// subcommandRe matches git-style subcommand listings:
//
//	"   clone      Clone a repository..."
var subcommandRe = regexp.MustCompile(`^\s{2,}(\w[\w-]*)\s{2,}(.+)$`)

// catalogNameOnlyRe matches a bare catalog entry with NO description column
// at all — the exact shape `git help -a` gives a PATH-installed external
// command (design/doctor/wraptool-doctor.md §7.6, measured on git 2.54.0):
//
//	External commands
//	   foo
//
// ("   foo", three spaces, the name, nothing else — cat -A confirmed no
// trailing content). subcommandRe requires a SECOND 2+-space run followed by
// a description, so this line alone never matched; catalogNameOnlyRe is
// deliberately never applied on its own — see ParseSubcommands' second pass
// for the bound that gates it.
var catalogNameOnlyRe = regexp.MustCompile(`^(\s{2,})(\w[\w-]*)\s*$`)

// ParseSubcommands finds git/cobra-style catalog entries in two passes.
//
// Pass one is subcommandRe, unchanged: every "name<2+ spaces>description"
// line becomes an entry, and it also records, per line, the entry's own
// indentation width (establishedWidths) and — separately, over EVERY
// non-blank indented line in the text regardless of shape — every
// indentation width the whole document uses at all (allIndentWidths).
//
// Pass two extends the catalog with bare, description-less lines
// (catalogNameOnlyRe) — but ONLY when doing so is safe. THE BOUND: a
// description-less line is trusted as a catalog entry IFF this help text's
// ENTIRE 2-or-more-space-indented content sits at a single, uniform
// indentation width, and that width already carries at least one genuine
// two-column entry from pass one.
//
// Why this bound and not a looser one (e.g. "matches any indentation width
// already used by a two-column entry anywhere in the text"): a bare indented
// word is exactly as often prose as it is a listing entry, and the two
// shapes are not reliably distinguishable by section-header text alone —
// measured directly, against every fixture in testdata/ and this host's own
// installed tools (git, node, npm, pi, quarto, claude, and others), the same
// AMBIGUOUS shape — an indented bare token sharing a document's established
// two-column indentation width — appears as genuine prose in real help text:
// a subcommand-specific help page's own flag-value enumeration ("no",
// "normal", "all", one per line under a flag's description), a wrapped
// description continuation, and — the closest false-positive call — a
// literal usage example ("pi" alone, under an "Examples:" heading, at the
// SAME 2-space indentation pi's own document uses for its real subcommand
// list). What distinguishes git's genuine "External commands" block from
// all of those, measured: `git help -a` is the ONLY captured document, among
// every one tested, whose entire indented body sits at ONE indentation width
// with no exceptions (every section, every category, all 175 indented lines,
// same 3-space column) — every document containing an ambiguous bare word
// instead mixes several different indentation widths for its other content
// (options, continuations, examples, ASCII art), which is exactly the signal
// that a bare word there is not part of a flat, one-column-wide listing.
// Requiring GLOBAL purity, not just a locally-matching width, is what keeps
// this narrow: a document that mixes widths gets NONE of this treatment —
// its two-column entries are still found (pass one is unconditional), only
// the bare-word extension is withheld — so a misjudged document degrades to
// the pre-existing behavior, never to a worse one.
func (p *GitParser) ParseSubcommands(helpText string) map[string]string {
	result := make(map[string]string)
	lines := strings.Split(helpText, "\n")

	establishedWidths := map[int]bool{}
	allIndentWidths := map[int]bool{}

	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			continue
		}
		if indent := len(line) - len(trimmed); indent >= 2 {
			allIndentWidths[indent] = true
		}

		m := subcommandRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		desc := strings.TrimSpace(m[2])
		// Skip section headers (they don't have enough indentation
		// or have trailing colons).
		if strings.HasSuffix(desc, ":") {
			continue
		}
		result[name] = desc
		establishedWidths[len(line)-len(trimmed)] = true
	}

	// Exactly one indentation width used anywhere in the text, AND that
	// width already carries a genuine two-column entry: see the doc
	// comment above for why both conditions are required.
	if len(allIndentWidths) == 1 {
		var width int
		for w := range allIndentWidths {
			width = w
		}
		if establishedWidths[width] {
			for _, line := range lines {
				m := catalogNameOnlyRe.FindStringSubmatch(line)
				if m == nil || len(m[1]) != width {
					continue
				}
				name := m[2]
				if _, exists := result[name]; !exists {
					result[name] = ""
				}
			}
		}
	}

	return result
}

// gitFlagWithDescRe matches flag lines with an inline description:
//
//	"    -v, --[no-]verbose    show diff in commit message template"
//	"    --[no-]amend          amend previous commit"
//	"    -S, --[no-]gpg-sign[=<key-id>] GPG sign commit"
var gitFlagWithDescRe = regexp.MustCompile(
	`^\s{2,}` + // leading whitespace
		`(?:(-\w),\s+)?` + // optional short flag: -m,
		`(--(?:\[no-\])?[\w][\w-]*)` + // long flag: --[no-]message or --amend
		`(?:\[?[=\s]<([^>]+)>\]?)?` + // optional value: <message>, [=<key-id>]
		`(?:\s<(\w+)>)?` + // optional standalone value: <n>
		`\s{2,}(.+)$`, // description (2+ spaces before it)
)

// gitFlagNoDescRe matches flag lines where the description is on the next line:
//
//	"    -m, --[no-]message <message>"
//	"    -F, --[no-]file <file>"
//	"    --[no-]fixup [(amend|reword):]commit"
var gitFlagNoDescRe = regexp.MustCompile(
	`^\s{2,}` + // leading whitespace
		`(?:(-\w),\s+)?` + // optional short flag
		`(--(?:\[no-\])?[\w][\w-]*)` + // long flag
		`(?:\[?[=\s]<?([^>\s]+)>?\]?)?` + // optional value
		`\s*$`, // no description, just trailing whitespace
)

// continuationRe matches a continuation line (deeply indented description).
//
//	"                          commit message"
var continuationRe = regexp.MustCompile(`^\s{20,}(\S.+)$`)

// gitFlagSimpleRe is a fallback for lines like:
//
//	"    --no-post-rewrite     bypass post-rewrite hook"
var gitFlagSimpleRe = regexp.MustCompile(
	`^\s{2,}(--[\w][\w-]*)\s{2,}(.+)$`,
)

func (p *GitParser) ParseFlags(helpText string) ParseResult {
	flags := make(map[string]Flag)
	matchCount := 0

	lines := strings.Split(helpText, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		// Try flag with inline description first.
		if m := gitFlagWithDescRe.FindStringSubmatch(line); m != nil {
			short := m[1]
			long := cleanNoPrefix(m[2])
			valueName := m[3]
			if valueName == "" {
				valueName = m[4]
			}
			desc := strings.TrimSpace(m[5])

			takesValue := valueName != ""
			flagType := "boolean"
			if takesValue {
				flagType = guessType(valueName)
			}

			flags[long] = Flag{
				Short:       short,
				Type:        flagType,
				Description: desc,
				TakesValue:  takesValue,
			}
			matchCount++
			continue
		}

		// Try flag without description (description on next line).
		if m := gitFlagNoDescRe.FindStringSubmatch(line); m != nil {
			short := m[1]
			long := cleanNoPrefix(m[2])
			valueName := m[3]

			// Only match if it looks like a real flag line (has -- prefix).
			if long == "" {
				continue
			}

			// Look ahead for a continuation line with the description.
			desc := ""
			if i+1 < len(lines) {
				if cm := continuationRe.FindStringSubmatch(lines[i+1]); cm != nil {
					desc = strings.TrimSpace(cm[1])
					i++ // consume the continuation line
				}
			}

			takesValue := valueName != ""
			flagType := "boolean"
			if takesValue {
				flagType = guessType(valueName)
			}

			flags[long] = Flag{
				Short:       short,
				Type:        flagType,
				Description: desc,
				TakesValue:  takesValue,
			}
			matchCount++
			continue
		}

		// Simple fallback for lines like "--no-post-rewrite  description".
		if m := gitFlagSimpleRe.FindStringSubmatch(line); m != nil {
			flagName := cleanNoPrefix(m[1])
			desc := strings.TrimSpace(m[2])

			// Skip if it looks like a section header.
			if strings.HasSuffix(desc, ":") || strings.HasSuffix(desc, "options") {
				continue
			}

			if _, exists := flags[flagName]; !exists {
				flags[flagName] = Flag{
					Type:        "boolean",
					Description: desc,
				}
				matchCount++
			}
		}
	}

	confidence := 0.0
	if matchCount > 0 {
		// Git -h style is very structured; high confidence if we find flags.
		confidence = 0.8
		if matchCount > 5 {
			confidence = 0.9
		}
	}

	return ParseResult{
		Subcommand: Subcommand{Flags: flags},
		Confidence: confidence,
	}
}

// cleanNoPrefix normalizes --[no-]flag to --flag.
func cleanNoPrefix(flag string) string {
	return strings.Replace(flag, "[no-]", "", 1)
}

// guessType infers a JSON Schema type from a value placeholder name.
func guessType(valueName string) string {
	valueName = strings.ToLower(valueName)
	switch valueName {
	case "n", "num", "number", "count", "depth", "lines":
		return "integer"
	default:
		return "string"
	}
}
