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

func (p *GitParser) ParseSubcommands(helpText string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(helpText, "\n") {
		m := subcommandRe.FindStringSubmatch(line)
		if m != nil {
			name := m[1]
			desc := strings.TrimSpace(m[2])
			// Skip section headers (they don't have enough indentation
			// or have trailing colons).
			if strings.HasSuffix(desc, ":") {
				continue
			}
			result[name] = desc
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
