package whitelist

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/pti/wraptool/internal/config"
)

// Decision represents the result of a policy evaluation.
type Decision struct {
	Allowed      bool
	Reason       string
	AllowedFlags []string // populated on denial to show what is permitted
}

// Policy evaluates commands against the whitelist configuration.
type Policy struct {
	mu    sync.RWMutex
	tools map[string]*config.ToolConfig
}

func NewPolicy(tools map[string]*config.ToolConfig) *Policy {
	return &Policy{tools: tools}
}

// Reload atomically swaps the policy configuration.
func (p *Policy) Reload(tools map[string]*config.ToolConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools = tools
}

// Evaluate checks whether a command is allowed.
func (p *Policy) Evaluate(toolName string, subcommand []string, flags map[string]string, positionalArgs []string) Decision {
	p.mu.RLock()
	defer p.mu.RUnlock()

	tc, ok := p.tools[toolName]
	if !ok {
		return Decision{
			Allowed: false,
			Reason:  fmt.Sprintf("tool %q is not configured", toolName),
		}
	}

	// Check deny rules first — they take absolute precedence.
	for _, deny := range tc.Deny {
		if subcommandMatches(subcommand, deny.Subcommand) {
			return Decision{
				Allowed: false,
				Reason:  fmt.Sprintf("subcommand %q is explicitly denied", strings.Join(subcommand, " ")),
			}
		}
	}

	// Find a matching allow rule.
	for _, allow := range tc.Allow {
		if !subcommandMatches(subcommand, allow.Subcommand) {
			continue
		}

		// Check deny_flags.
		for flag := range flags {
			for _, denied := range allow.DenyFlags {
				if flag == denied {
					return Decision{
						Allowed:      false,
						Reason:       fmt.Sprintf("flag %q is explicitly denied for %q", flag, strings.Join(subcommand, " ")),
						AllowedFlags: allow.Flags,
					}
				}
			}
		}

		// -h/--help need no special-casing here: config load seeds them into every
		// allow rule's Flags (config.SeedHelpFlags), so they pass the allowlist
		// check below like any other permitted flag — and an operator's deny_flags
		// entry removes them at the same source, handled by the deny_flags check
		// above. Help is thus an ordinary allowed flag, not a policy exception.

		// Check that all provided flags are in the allowlist.
		for flag := range flags {
			if !containsFlag(allow.Flags, flag) {
				return Decision{
					Allowed:      false,
					Reason:       fmt.Sprintf("flag %q is not in the allowlist for %q", flag, strings.Join(subcommand, " ")),
					AllowedFlags: allow.Flags,
				}
			}
		}

		// Check flag constraints.
		if err := checkConstraints(flags, allow.FlagConstraints); err != nil {
			return Decision{
				Allowed:      false,
				Reason:       err.Error(),
				AllowedFlags: allow.Flags,
			}
		}

		// Check positional arg constraints.
		if allow.ArgConstraints != nil && len(positionalArgs) > allow.ArgConstraints.PositionalMax {
			return Decision{
				Allowed: false,
				Reason: fmt.Sprintf("too many positional arguments: got %d, max %d",
					len(positionalArgs), allow.ArgConstraints.PositionalMax),
				AllowedFlags: allow.Flags,
			}
		}

		return Decision{Allowed: true}
	}

	// No matching allow rule found.
	return Decision{
		Allowed: false,
		Reason:  fmt.Sprintf("subcommand %q is not in the allowlist", strings.Join(subcommand, " ")),
	}
}

// subcommandMatches checks if the given subcommand matches the rule.
// The rule tokens must equal the subcommand tokens exactly, token for
// token — this is NOT a prefix match. A deny rule for [auth] therefore
// does not cover [auth, print-access-token]; it needs its own entry. Used
// for both allow and deny rules, so the same exact-length semantics apply
// to each (see docs/configuration.qmd's "Deny rules" section and
// doctor's policy.deny-prefix-gap check, which flags this gap).
func subcommandMatches(actual, rule []string) bool {
	return slices.Equal(actual, rule)
}

func containsFlag(allowed []string, flag string) bool {
	for _, f := range allowed {
		if f == flag {
			return true
		}
	}
	return false
}
