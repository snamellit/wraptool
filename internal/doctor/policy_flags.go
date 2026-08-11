// policy_flags.go implements the two flag-level policy lints of design
// §7.7's policy.* content-lint group: policy.unpatterned-value-flag and
// policy.dangerous-flag. Both range over every tools.*.allow rule's Flags —
// the LOADED, DEFAULTED list (config.SeedHelpFlags has already appended
// -h/--help and dropped anything the rule's own deny_flags names), never
// the raw file — so a flag this file ever considers is one the policy
// engine (internal/whitelist) would actually accept from an MCP call today.
//
// WHAT COUNTS AS "DANGEROUS" comes from toolknowledge.go (D0): a tool's own
// ToolKnowledge.DangerousFlags (exact spelling) plus the tool-independent
// IsGenericDangerousFlag shape ("--*-file"/"--*-cmd"). A tool with no
// ToolKnowledge entry still gets the generic check — see
// policyFlagsDangerousReason.
//
// WHAT COUNTS AS "VALUE-TAKING" is NOT knowable from config alone
// (internal/whitelist has no such notion: validateValue only branches on
// whether the constraint TYPE is valueless, never on the flag's real CLI
// shape). The only source available here — introspection is not, until
// Phase F's F0a lands an exported cache-validity constructor — is
// mcptools.IsBooleanFlag, which is itself exactly config.FlagConstraint's
// own single source of truth (ConstraintType.Kind().Valueless) for an
// EXPLICITLY typed flag, falling back to a small well-known-switch-name/
// prefix heuristic for a flag with no explicit type at all
// (design/policy/flag-type-inference.md's "placeholder-aware default":
// unknown resolves to string, i.e. value-taking, the recoverable direction).
// A flag IsBooleanFlag confirms boolean (an explicit boolean type, or a
// well-known switch shape) is exempt from policy.unpatterned-value-flag —
// every other flag is treated as this file's only confident signal permits:
// value-taking. This is a documented, accepted gap (design §7.6's own
// "second, compounding false positive through the string default" note) —
// Phase F's F4 later exempts a flag drift could not confirm; D2 ships
// without that exemption, exactly as plan §3 sequences it.
package doctor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
)

const (
	policyFlagsUnpatternedValue CheckID = "policy.unpatterned-value-flag"
	policyFlagsDangerous        CheckID = "policy.dangerous-flag"
)

// policyFlagsUnpatternedValueMeta and policyFlagsDangerousMeta are named so
// policy_flags_test.go's meta assertions read the SAME literal init()
// registers — never through Registered() (plan §1 rule 8).
var (
	policyFlagsUnpatternedValueMeta = Meta{
		ID:    policyFlagsUnpatternedValue,
		Needs: NeedsConfig,
		Summary: "warns when an allowed value-taking flag has neither a pattern nor a non-string type, " +
			"so its value passes to the tool unconstrained",
	}
	policyFlagsDangerousMeta = Meta{
		ID:    policyFlagsDangerous,
		Needs: NeedsConfig,
		Summary: "warns when an allowed flag is on its tool's execute-or-redirect list (or matches the generic " +
			"--*-file/--*-cmd shape) with no pattern",
	}
)

func init() {
	register(policyFlagsUnpatternedValueMeta, runPolicyFlagsUnpatternedValue)
	register(policyFlagsDangerousMeta, runPolicyFlagsDangerous)
}

// ---------------------------------------------------------------- shared helpers

// policyFlagsSortedToolNames returns cfg.Tools' keys, sorted — both checks
// below range over a Go map, and TestChecksAreDeterministic runs each check
// 8 times against a >=2-entry fixture, which an unsorted `range` fails at
// better than even odds (invariants_test.go's invariantsDeterminismRuns).
func policyFlagsSortedToolNames(tools map[string]config.ToolConfig) []string {
	out := make([]string, 0, len(tools))
	for name := range tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// policyFlagsRuleSubcommand renders rule.Subcommand the way every ConfigPath
// Item step and every existing locate_test.go fixture already spells a
// multi-segment subcommand (RULING R6, finding.go): space-joined.
func policyFlagsRuleSubcommand(rule config.AllowRule) string {
	return strings.Join(rule.Subcommand, " ")
}

// policyFlagsConstraint returns flag's constraint entry under rule, and
// whether one exists at all — a flag can be in rule.Flags with no
// flag_constraints entry, the common "bare, fully unconstrained" case this
// file's whole reason to exist is auditing.
func policyFlagsConstraint(rule config.AllowRule, flag string) (config.FlagConstraint, bool) {
	c, ok := rule.FlagConstraints[flag]
	return c, ok
}

// policyFlagsNeutralized reports whether flag is already neutralized by an
// explicit flag_constraints[flag].deny: true — the SECOND of the two
// mandatory exemptions design §7.7 names (the first, deny_flags, needs no
// code here at all: config.SeedHelpFlags already drops a rule's own
// deny_flags entries from the LOADED rule.Flags this file iterates, so a
// flag denied that way is simply never seen). A flag_constraints entry
// carrying deny: true makes the flag unusable even though it nominally
// survived into rule.Flags (checkConstraints checks constraint.Deny before
// ever reaching validateValue/pattern matching, internal/whitelist/
// constraints.go) — a hardened policy that explicitly denies e.g. git -c
// must not be warned at for doing so.
func policyFlagsNeutralized(rule config.AllowRule, flag string) bool {
	c, ok := policyFlagsConstraint(rule, flag)
	return ok && c.Deny
}

// ---------------------------------------------------------------- policy.unpatterned-value-flag

// policyFlagsValueTaking reports whether flag is confidently value-taking —
// i.e. NOT confirmed boolean by mcptools.IsBooleanFlag, this file's only
// available oracle (see the file doc comment for why introspection is not
// one yet). IsBooleanFlag itself already resolves an explicitly typed
// flag from config.FlagConstraint.Type.Kind() (the single source of truth,
// design/policy/flag-constraint-type-single-source.md) before falling back
// to a well-known switch name/prefix heuristic for an untyped one — so a
// flag this function calls value-taking is never one flag_constraints or a
// well-known switch shape already said was boolean.
func policyFlagsValueTaking(flag string, constraints map[string]config.FlagConstraint) bool {
	return !mcptools.IsBooleanFlag(flag, constraints)
}

// policyFlagsHasPatternOrNonString reports whether flag's constraint (if
// any) already carries a `pattern` or a non-string `type` — either one
// closes design §7.7's unpatterned-value-flag gap on its own. Integer is the
// only non-string, non-boolean ConstraintType (Boolean flags never reach
// here at all: policyFlagsValueTaking already excludes them via
// IsBooleanFlag).
func policyFlagsHasPatternOrNonString(c config.FlagConstraint, hasConstraint bool) bool {
	if !hasConstraint {
		return false
	}
	if c.Pattern != "" {
		return true
	}
	return c.Type == config.ConstraintInteger
}

// runPolicyFlagsUnpatternedValue implements policy.unpatterned-value-flag
// (design §7.7): an allowed, confidently value-taking flag with neither a
// pattern nor a non-string type — the residual gap security audit F2 left
// open (an allowlisted value flag with no pattern is a free-form value; F2
// itself closed only the shell-metachar denylist at the executor, not
// per-flag content constraints).
func runPolicyFlagsUnpatternedValue(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range policyFlagsSortedToolNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		for _, rule := range tc.Allow {
			subcommand := policyFlagsRuleSubcommand(rule)
			for _, flag := range rule.Flags {
				if policyFlagsNeutralized(rule, flag) {
					continue
				}
				if !policyFlagsValueTaking(flag, rule.FlagConstraints) {
					continue
				}
				c, hasConstraint := policyFlagsConstraint(rule, flag)
				if policyFlagsHasPatternOrNonString(c, hasConstraint) {
					continue
				}
				findings = append(findings, Finding{
					Severity: SeverityWarn,
					Title:    "Allowed value-taking flag has no pattern",
					Detail: fmt.Sprintf(
						"tools.%s's allow rule for %q permits flag %q, which takes a value, with neither a "+
							"`pattern` nor a non-string `type` — its value passes to the wrapped tool unconstrained "+
							"(security audit F2's residue: an allowlisted value flag with no pattern is a free-form "+
							"value)",
						name, subcommand, flag),
					Path: At("tools", name, "allow").Item("subcommand", subcommand).Key("flags").Elem(flag),
					Remedy: fmt.Sprintf(
						"add a `pattern` (or a non-string `type`, e.g. integer with min/max) to "+
							"flag_constraints[%q] under tools.%s's %q allow rule, or move %q to deny_flags if it "+
							"should never be permitted",
						flag, name, subcommand, flag),
					DocRef: "docs/configuration.qmd",
				})
			}
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- policy.dangerous-flag

// policyFlagsDangerousReason returns why flag is on tk's tool-specific
// execute-or-redirect list, OR why it matches the tool-independent generic
// shape, and whether either matched at all. Tool-specific wins when both
// would (e.g. gcloud's own --flags-file also matches the generic --*-file
// shape) so the finding names the more specific reason, never both.
// hasTK false (an unknown tool — no toolknowledge.go entry at all) still
// reaches the generic check: IsGenericDangerousFlag is deliberately
// tool-independent (toolknowledge.go's own doc comment).
func policyFlagsDangerousReason(tk ToolKnowledge, hasTK bool, flag string) (reason string, dangerous bool) {
	if hasTK && tk.IsDangerousFlag(flag) {
		return "which is on this tool's own execute-or-redirect flag list", true
	}
	if IsGenericDangerousFlag(flag) {
		return "whose name matches the generic --*-file/--*-cmd shape (its value names a file or command the tool acts on)", true
	}
	return "", false
}

// runPolicyFlagsDangerous implements policy.dangerous-flag (design §7.7): an
// allowed flag on its tool's execute-or-redirect list (D0's per-tool table)
// or the generic --*-file/--*-cmd shape, with no pattern to constrain its
// value. Unlike unpatterned-value-flag, no value-taking determination is
// needed here at all — every flag on either list is inherently value-taking
// by its own documented shape (git's -c takes "key=value", --exec takes a
// command, a generic --*-file flag takes a path), so this check fires on
// ANY constraint type, not just string/untyped — design §7.7's row for this
// check names only "lacks a pattern", never a type exemption.
func runPolicyFlagsDangerous(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range policyFlagsSortedToolNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		tk, hasTK := LookupToolKnowledge(ToolBasename(tc.Binary))
		for _, rule := range tc.Allow {
			subcommand := policyFlagsRuleSubcommand(rule)
			for _, flag := range rule.Flags {
				reason, dangerous := policyFlagsDangerousReason(tk, hasTK, flag)
				if !dangerous {
					continue
				}
				if policyFlagsNeutralized(rule, flag) {
					continue
				}
				c, hasConstraint := policyFlagsConstraint(rule, flag)
				if hasConstraint && c.Pattern != "" {
					continue
				}
				findings = append(findings, Finding{
					Severity: SeverityWarn,
					Title:    "Allowed flag is on the dangerous-flag list with no pattern",
					Detail: fmt.Sprintf(
						"tools.%s's allow rule for %q permits flag %q, %s, with no pattern to constrain its value",
						name, subcommand, flag, reason),
					Path: At("tools", name, "allow").Item("subcommand", subcommand).Key("flags").Elem(flag),
					Remedy: fmt.Sprintf(
						"add a `pattern` to flag_constraints[%q] under tools.%s's %q allow rule, or move %q to "+
							"deny_flags if it should never be permitted",
						flag, name, subcommand, flag),
					DocRef: "docs/configuration.qmd",
				})
			}
		}
	}
	return Found(findings...)
}
