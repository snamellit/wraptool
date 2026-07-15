package introspect

import (
	"regexp"
	"strings"
)

// FallbackParser is a last-resort parser that extracts anything
// looking like a flag (--word-word) from the help text.
// Low confidence, but better than nothing.
type FallbackParser struct{}

func (p *FallbackParser) Name() string { return "fallback" }

func (p *FallbackParser) ParseSubcommands(helpText string) map[string]string {
	return nil
}

var fallbackFlagRe = regexp.MustCompile(`(--[\w][\w-]*)(?:[=\s]<([\w-]+)>)?`)

func (p *FallbackParser) ParseFlags(helpText string) ParseResult {
	flags := make(map[string]Flag)
	seen := make(map[string]bool)

	for _, line := range strings.Split(helpText, "\n") {
		matches := fallbackFlagRe.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			flag := m[1]
			valueName := m[2]

			if seen[flag] {
				continue
			}
			seen[flag] = true

			// Skip common meta-flags that are not useful.
			if flag == "--help" || flag == "--version" {
				continue
			}

			takesValue := valueName != ""
			flagType := "boolean"
			if takesValue {
				flagType = guessType(valueName)
			}

			flags[flag] = Flag{
				Type:       flagType,
				TakesValue: takesValue,
			}
		}
	}

	demoteBooleansIfNoPlaceholders(flags)

	confidence := 0.0
	if len(flags) > 0 {
		confidence = 0.3
	}

	return ParseResult{
		Subcommand: Subcommand{Flags: flags},
		Confidence: confidence,
	}
}
