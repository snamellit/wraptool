// policy_generic.go implements the design §7.7 tool-INDEPENDENT policy-content
// lints (plan step D1): the four checks that need no per-tool knowledge table
// (internal/doctor/toolknowledge.go, D0) because they only reason about the
// SHAPE of a rule's own values — a flag_constraints pattern, an
// arg_constraints block, a deny/allow subcommand pair — never a
// tool-specific dangerous-flag/shell-equivalent/mutating-verb list (that is
// D2/D3's job).
//
// policy.uncompilable-pattern is the one FAIL in the §7.7 group, and the one
// the design calls "not a judgement call": internal/whitelist's validateValue
// (constraints.go:74-78) auto-anchors every pattern as ^(?:...)$ before
// compiling it at call time (audit F6), so this check compiles the SAME
// anchored form. An uncompilable pattern there makes that rule permanently
// uncallable whenever the constrained flag is present, and nothing else
// reports it. policy.overbroad-pattern, .unbounded-positional-count, and
// .deny-prefix-gap are warn, per §7.7.
//
// Every check below that reasons about a flag_constraints entry's Pattern
// first asks policygenericConstraintReachable whether whitelist's
// checkConstraints (internal/whitelist/constraints.go) can ever actually
// reach that Pattern for a real call. Three of that function's four
// conditions are STRICT PRECONDITIONS for the checks' own claims to be
// true — not a borrowed heuristic from §7.7's D2-scoped "mandatory
// exemptions" note, but a straight reading of internal/whitelist/policy.go's
// evaluation order: a flag in DenyFlags, or a flag_constraints entry with
// Deny: true, denies the WHOLE call before checkConstraints's per-flag
// pattern loop ever runs (policy.go:64-95); a flag absent from the rule's own
// Flags list is refused as "not in the allowlist" before checkConstraints
// runs at all (policy.go:83-92, and is config.constraint-orphan's domain, not
// this file's); and a boolean-typed constraint's own validateValue returns
// before it ever reaches Pattern (constraints.go:50-53). Skipping these is
// required for policy.uncompilable-pattern's claim ("this pattern makes
// validateValue fail closed at call time") to be true of every finding it
// emits, and for policy.overbroad-pattern's ("the flag is effectively
// unconstrained") not to be actively misleading about a flag a call cannot
// carry at all.
package doctor

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

const (
	policygenericUncompilablePattern      CheckID = "policy.uncompilable-pattern"
	policygenericOverbroadPattern         CheckID = "policy.overbroad-pattern"
	policygenericUnboundedPositionalCount CheckID = "policy.unbounded-positional-count"
	policygenericDenyPrefixGap            CheckID = "policy.deny-prefix-gap"
)

// policygenericUncompilablePatternMeta, ... are the checks' Meta literals,
// named so policy_generic_test.go's meta assertions read the SAME literal
// init() registers — never through Registered() (plan §1 rule 8: by the time
// this package's Phase-B/C/D siblings have merged, Registered() also carries
// every one of THEIR checks, which a test asserting one check's own ID/Needs
// must not depend on; pool.go/mcpservers.go model this).
var (
	policygenericUncompilablePatternMeta = Meta{
		ID:    policygenericUncompilablePattern,
		Needs: NeedsConfig,
		Summary: "checks that every reachable flag_constraints pattern compiles in the same " +
			"auto-anchored form whitelist runs at call time",
	}
	policygenericOverbroadPatternMeta = Meta{
		ID:    policygenericOverbroadPattern,
		Needs: NeedsConfig,
		Summary: "warns when a reachable flag_constraints pattern constrains nothing " +
			"(.*, .+, .*?, (?s).*, or empty)",
	}
	policygenericUnboundedPositionalCountMeta = Meta{
		ID:    policygenericUnboundedPositionalCount,
		Needs: NeedsConfig,
		Summary: "warns when an allow rule sets no arg_constraints.positional_max, leaving the " +
			"positional argument COUNT unbounded",
	}
	policygenericDenyPrefixGapMeta = Meta{
		ID:    policygenericDenyPrefixGap,
		Needs: NeedsConfig,
		Summary: "warns when a deny rule's subcommand is a strict prefix of a longer allowed " +
			"subcommand, which its exact-length match never actually covers",
	}
)

func init() {
	register(policygenericUncompilablePatternMeta, runPolicygenericUncompilablePattern)
	register(policygenericOverbroadPatternMeta, runPolicygenericOverbroadPattern)
	register(policygenericUnboundedPositionalCountMeta, runPolicygenericUnboundedPositionalCount)
	register(policygenericDenyPrefixGapMeta, runPolicygenericDenyPrefixGap)
}

// ---------------------------------------------------------------- shared helpers

// policygenericSortedFlagNames returns m's keys sorted — every check below
// that ranges over an allow rule's FlagConstraints must not do so on the raw
// map, or TestChecksAreDeterministic (invariants_test.go, 8 runs against a
// multi-entry fixture) fails on the merge commit.
func policygenericSortedFlagNames(m map[string]config.FlagConstraint) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// policygenericConstraintReachable reports whether whitelist's
// checkConstraints can ever actually evaluate flagName's own Pattern for
// rule — see this file's own doc comment for why each of these three
// conditions is a strict precondition, not a judgement call:
//
//   - flagName must be genuinely callable: present in rule.Flags AND absent
//     from rule.DenyFlags. Either failing denies the whole call before
//     checkConstraints's per-flag pattern loop runs at all.
//   - the constraint itself must not already short-circuit the call: Deny:
//     true denies the call outright before validateValue is reached for ANY
//     flag in that same call.
//   - the constraint's Type must not be boolean (Kind().Valueless):
//     validateValue returns before it ever reaches Pattern for a valueless
//     flag.
func policygenericConstraintReachable(rule config.AllowRule, flagName string, c config.FlagConstraint) bool {
	if c.Deny {
		return false
	}
	if slices.Contains(rule.DenyFlags, flagName) {
		return false
	}
	if !slices.Contains(rule.Flags, flagName) {
		return false
	}
	kind, ok := c.Type.Kind()
	return ok && !kind.Valueless
}

// policygenericAnchoredPattern mirrors internal/whitelist/constraints.go's
// validateValue exactly ("^(?:" + c.Pattern + ")$") — the form whitelist
// actually compiles at call time, not the operator's raw pattern string.
func policygenericAnchoredPattern(pattern string) string {
	return "^(?:" + pattern + ")$"
}

// ---------------------------------------------------------------- policy.uncompilable-pattern

func runPolicygenericUncompilablePattern(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		for _, rule := range tc.Allow {
			subcommand := strings.Join(rule.Subcommand, " ")
			for _, flag := range policygenericSortedFlagNames(rule.FlagConstraints) {
				c := rule.FlagConstraints[flag]
				if c.Pattern == "" || !policygenericConstraintReachable(rule, flag, c) {
					continue
				}
				anchored := policygenericAnchoredPattern(c.Pattern)
				_, err := regexp.Compile(anchored)
				if err == nil {
					continue
				}
				findings = append(findings, Finding{
					Severity: SeverityFail,
					Title:    "flag_constraints pattern does not compile",
					Detail: fmt.Sprintf(
						"tools.%s allow[subcommand=%q].flag_constraints[%q].pattern %q does not compile once "+
							"auto-anchored as %q — whitelist's validateValue runs regexp.MatchString on exactly "+
							"this anchored form at call time, so this rule is permanently uncallable whenever "+
							"%q is present: %v",
						name, subcommand, flag, c.Pattern, anchored, flag, err),
					Path: At("tools", name, "allow").Item("subcommand", subcommand).
						Key("flag_constraints", flag, "pattern"),
					Remedy: fmt.Sprintf(
						"fix the regular expression in tools.%s's flag_constraints[%q].pattern, or remove it",
						name, flag),
					DocRef: "design/doctor/wraptool-doctor.md",
				})
			}
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- policy.overbroad-pattern

// policygenericOverbroadPatterns is §7.7's own literal list of patterns that
// constrain nothing once anchored: ".*"/".+"/"(?s).*" all admit essentially
// any value a caller could supply; ".*?" is written lazy, but an anchored
// ^(?:.*?)$ backtracks to match exactly the same everything a greedy .*
// does.
//
// The EMPTY pattern is deliberately NOT here. config.FlagConstraint.Pattern's
// zero value cannot distinguish an explicitly written `pattern: ""` from no
// pattern key at all, so including it made this check fire on every constraint
// that simply has no pattern — including docs/getting-started.qmd's own
// `"--depth": {type: integer, max: 100}`, where validateValue really does
// parse and range-check the value, so "effectively unconstrained" was a false
// statement and the remedy would have deleted a working constraint. This check
// speaks only about patterns the operator actually wrote; the no-pattern case
// belongs to policy.unpatterned-value-flag. Deliberately a closed, literal set, not a
// semantic "matches everything" analysis: a differently spelled but
// equally-overbroad pattern (e.g. "[\s\S]*") is outside what this heuristic
// catches, matching the same closed-list nature as every other §7.7 check.
var policygenericOverbroadPatterns = map[string]bool{
	".*":     true,
	".+":     true,
	".*?":    true,
	"(?s).*": true,
}

func runPolicygenericOverbroadPattern(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		for _, rule := range tc.Allow {
			subcommand := strings.Join(rule.Subcommand, " ")
			for _, flag := range policygenericSortedFlagNames(rule.FlagConstraints) {
				c := rule.FlagConstraints[flag]
				if !policygenericOverbroadPatterns[c.Pattern] || !policygenericConstraintReachable(rule, flag, c) {
					continue
				}
				shown := c.Pattern
				if shown == "" {
					shown = "(empty)"
				}
				findings = append(findings, Finding{
					Severity: SeverityWarn,
					Title:    "flag_constraints pattern constrains nothing",
					Detail: fmt.Sprintf(
						"tools.%s allow[subcommand=%q].flag_constraints[%q].pattern is %s, which matches — or, if "+
							"empty, checks nothing about — any value at all, leaving the flag effectively "+
							"unconstrained despite the flag_constraints entry",
						name, subcommand, flag, shown),
					Path: At("tools", name, "allow").Item("subcommand", subcommand).
						Key("flag_constraints", flag, "pattern"),
					Remedy: fmt.Sprintf(
						"write a real pattern for tools.%s's flag_constraints[%q], or remove the entry if the flag "+
							"genuinely needs no value constraint",
						name, flag),
					DocRef: "docs/configuration.qmd#flag-constraints",
				})
			}
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- policy.unbounded-positional-count

func runPolicygenericUnboundedPositionalCount(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		for _, rule := range tc.Allow {
			if rule.ArgConstraints != nil {
				continue
			}
			subcommand := strings.Join(rule.Subcommand, " ")
			findings = append(findings, Finding{
				Severity: SeverityWarn,
				Title:    "allow rule has no positional argument count limit",
				Detail: fmt.Sprintf(
					"tools.%s's allow rule for subcommand %q sets no arg_constraints.positional_max, so "+
						"whitelist places no limit at all on how many positional arguments a call may carry. "+
						"This bounds COUNT ONLY — ArgConstraints has no content field, so even a positional_max "+
						"would not constrain what those positional values ARE; audit F3 remains open upstream",
					name, subcommand),
				Path: At("tools", name, "allow").Item("subcommand", subcommand),
				Remedy: fmt.Sprintf(
					"add arg_constraints.positional_max to tools.%s's allow rule for %q if this subcommand should "+
						"cap (or forbid) positional arguments",
					name, subcommand),
				DocRef: "design/doctor/wraptool-doctor.md",
			})
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- policy.deny-prefix-gap

// policygenericIsStrictPrefix reports whether prefix is a non-empty, STRICTLY
// SHORTER token-wise prefix of full — the exact shape whitelist's
// subcommandMatches (internal/whitelist/policy.go) requires equal length to
// match at all, so a deny rule shaped this way never matches the longer
// subcommand it looks, to a human reader, like it should also cover.
func policygenericIsStrictPrefix(prefix, full []string) bool {
	if len(prefix) == 0 || len(prefix) >= len(full) {
		return false
	}
	for i, tok := range prefix {
		if full[i] != tok {
			return false
		}
	}
	return true
}

// policygenericDeniedExactly reports whether some deny rule names subcommand at
// exactly its own length, which is the length whitelist matches at.
func policygenericDeniedExactly(deny []config.DenyRule, subcommand []string) bool {
	for _, d := range deny {
		if slices.Equal(d.Subcommand, subcommand) {
			return true
		}
	}
	return false
}

func runPolicygenericDenyPrefixGap(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range toolSortedNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		for _, deny := range tc.Deny {
			denySub := strings.Join(deny.Subcommand, " ")
			for _, allow := range tc.Allow {
				if !policygenericIsStrictPrefix(deny.Subcommand, allow.Subcommand) {
					continue
				}
				// The gap is closed when the longer subcommand is itself denied at
				// exact length by another rule for the same tool: whitelist matches
				// that one and the allow rule is dead. Warning there would nag an
				// operator for having done exactly the thing the remedy asks for,
				// usually on the next line of the same file.
				if policygenericDeniedExactly(tc.Deny, allow.Subcommand) {
					continue
				}
				allowSub := strings.Join(allow.Subcommand, " ")
				findings = append(findings, Finding{
					Severity: SeverityWarn,
					Title:    "deny rule does not cover a longer allowed subcommand",
					Detail: fmt.Sprintf(
						"tools.%s denies subcommand %q, but also allows the longer subcommand %q that starts with "+
							"it — whitelist matches a deny rule at EXACT subcommand length (subcommandMatches), so "+
							"denying %q does NOT also deny %q, contrary to what the prefix relationship suggests",
						name, denySub, allowSub, denySub, allowSub),
					Path: At("tools", name, "deny").Item("subcommand", denySub),
					Remedy: fmt.Sprintf(
						"add an explicit deny rule for subcommand %q (or for every longer subcommand under %q you "+
							"mean to block)",
						allowSub, denySub),
					DocRef: "design/doctor/wraptool-doctor.md",
				})
			}
		}
	}
	return Found(findings...)
}
