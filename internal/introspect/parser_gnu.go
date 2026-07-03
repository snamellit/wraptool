package introspect

import (
	"regexp"
	"strings"
)

// GNUParser handles GNU getopt-style help output, as used by curl, wget,
// coreutils, and many other tools.
//
// Pattern: " -f, --flag[=VALUE]  description"
//
//	" -f, --flag <value>  description"
//	"     --long-only     description"
type GNUParser struct{}

func (p *GNUParser) Name() string { return "gnu" }

func (p *GNUParser) ParseSubcommands(helpText string) map[string]string {
	// GNU tools typically don't have subcommands.
	return nil
}

// gnuFlagRe matches GNU-style flag lines:
//
//	" -d, --data <data>          HTTP POST data"
//	" -f, --fail                 Fail fast with no output..."
//	"     --compressed           Request compressed response"
//	" -o, --output <file>        Write to file instead of stdout"
//	" -H, --header <header/@file> Extra header..."
var gnuFlagRe = regexp.MustCompile(
	`^\s+` + // leading whitespace
		`(?:(-\w),\s+)?` + // optional short flag: -d,
		`(--[\w][\w-]*)` + // long flag: --data
		`(?:[=\s]\s*<?(\w[^>\s]*)>?)?` + // optional value: <data>, =VALUE
		`\s{2,}(.+)$`, // description
)

func (p *GNUParser) ParseFlags(helpText string) ParseResult {
	flags := make(map[string]Flag)
	matchCount := 0

	for _, line := range strings.Split(helpText, "\n") {
		m := gnuFlagRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		short := m[1]
		long := m[2]
		valueName := m[3]
		desc := strings.TrimSpace(m[4])

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
	}

	confidence := 0.0
	if matchCount > 0 {
		confidence = 0.6
		if matchCount > 5 {
			confidence = 0.7
		}
	}

	return ParseResult{
		Subcommand: Subcommand{Flags: flags},
		Confidence: confidence,
	}
}
