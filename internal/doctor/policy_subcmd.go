// policy_subcmd.go implements the SUBCOMMAND-level slice of the policy.*
// content lint (design §7.7, step D3): policy.shell-equivalent-subcommand,
// policy.mutating-subcommand, and policy.force-push-allowed. All three are
// warn (§7.7's header: "Every finding is warn except the one that is not a
// judgement call" — that one is policy.uncompilable-pattern, D1's, not any
// of these).
//
// The first two range over Input.Config.Tools' Allow rules and consult D0's
// toolknowledge.go table (LookupToolKnowledge) — a tool with no entry, or an
// allow rule whose Subcommand matches none of that tool's known shapes,
// produces NOTHING (the bounding rule: no guess, ever). The table's own
// ShellEquivalentMatch/MutatingVerbMatch methods already implement the
// "match a rule's subcommand by segment prefix" rule this step was given —
// see toolknowledge.go's doc comments for why they use different match
// depths (prefix vs last-segment).
//
// policy.force-push-allowed is git-specific data the table does not (and
// should not) carry: it is one hardcoded fact about one tool's one
// subcommand, not a per-tool SHAPE the way DangerousFlags/MutatingVerbs are,
// so it lives here rather than growing toolKnowledgeTable a field only one
// entry would ever use.
//
// ---------------------------------------------------------------- "without capability gating"
//
// Design §7.7 says policy.mutating-subcommand fires when a subcommand
// "mutates remote state without capability gating". Decided operationally,
// against the actual schema (internal/config/config.go's AllowRule): there
// is NO schema construct that expresses "this allow rule is gated" as
// distinct from "this allow rule exists". wraptool's one human-in-the-loop
// mechanism — wraptool_request_capability, reviewed and applied via
// config.ApplyRequest — operates BEFORE a rule is written into config.yaml;
// once a rule is in the static file doctor audits, it is an unconditional,
// standing grant with no further per-call approval, no matter what other
// fields the rule carries. FlagConstraints/ArgConstraints bound VALUES and
// COUNTS (and are D2's/D4's own concern), not whether the subcommand may be
// invoked at all — a `positional_max: 1` on `kubectl delete` still lets
// exactly one resource name through, so treating it as "gating" would hide
// the exact case this check exists to surface. So: "without capability
// gating" reduces to "is present in Allow at all", and this check fires
// unconditionally on a match, exactly like its sibling
// policy.shell-equivalent-subcommand (§7.7's own row for that one carries no
// gating clause at all). This is stated here once rather than re-derived at
// each finding site; each Finding.Detail below also says it inline, per the
// architect brief's instruction to say so in the Detail.
//
// A short, deliberate absence: neither check here cross-references Deny
// rules. DenyRule (config.go) carries a Subcommand only, no flags — an
// EXACT-length deny of the same subcommand a matching allow rule names would
// make that allow rule dead (deny wins, internal/whitelist's subcommandMatches
// is exact-length), but authoring both is not a pattern any merged doctor
// check treats as load-bearing (policy.deny-prefix-gap, D4, covers the
// PREFIX-gap direction only), and reconstructing "is this allow rule
// actually reachable" here would duplicate that check's own job under a
// different ID. Left to D4/the operator, not invented here.
package doctor

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/config"
)

const (
	policySubcmdShellEquivalent CheckID = "policy.shell-equivalent-subcommand"
	policySubcmdMutating        CheckID = "policy.mutating-subcommand"
	policySubcmdForcePush       CheckID = "policy.force-push-allowed"
)

// policySubcmdShellEquivalentMeta, policySubcmdMutatingMeta, and
// policySubcmdForcePushMeta are named so policy_subcmd_test.go's meta
// assertions read the SAME literal init() registers — never through
// Registered() (plan §1 rule 8; pool.go/mcpservers.go model this).
var (
	policySubcmdShellEquivalentMeta = Meta{
		ID:    policySubcmdShellEquivalent,
		Needs: NeedsConfig,
		Summary: "warns when an allowed subcommand is a general-purpose execution or data channel " +
			"for its tool (kubectl exec/cp/port-forward/proxy, gcloud compute ssh)",
	}
	policySubcmdMutatingMeta = Meta{
		ID:    policySubcmdMutating,
		Needs: NeedsConfig,
		Summary: "warns when an allowed subcommand mutates remote state (kubectl delete/apply/edit/" +
			"patch/replace/scale/drain, gcloud ... delete/set-iam-policy/add-iam-policy-binding, " +
			"terraform apply/destroy/import)",
	}
	policySubcmdForcePushMeta = Meta{
		ID:    policySubcmdForcePush,
		Needs: NeedsConfig,
		Summary: "warns when `git push` is allowed with no deny_flags/flag_constraints covering " +
			"--force/--force-with-lease/-f",
	}
)

func init() {
	register(policySubcmdShellEquivalentMeta, runPolicySubcmdShellEquivalent)
	register(policySubcmdMutatingMeta, runPolicySubcmdMutating)
	register(policySubcmdForcePushMeta, runPolicySubcmdForcePush)
}

// policySubcmdSortedToolNames returns Input.Config.Tools' keys, sorted — the
// helper every check below ranges through, so no two of the three checks in
// this file can independently forget to sort (mirrors mcpservers.go's
// mcpSortedServerNames / pool.go's poolSortedHarnessNames; not shared with
// tools.go's own generic toolSortedNames, matching the established
// per-file-helper convention those two set).
func policySubcmdSortedToolNames(m map[string]config.ToolConfig) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- policy.shell-equivalent-subcommand

func runPolicySubcmdShellEquivalent(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range policySubcmdSortedToolNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		tk, ok := LookupToolKnowledge(ToolBasename(tc.Binary))
		if !ok {
			continue // no per-tool knowledge at all — never a guess (D0's bounding rule)
		}
		for _, rule := range tc.Allow {
			matched, ok := tk.ShellEquivalentMatch(rule.Subcommand)
			if !ok {
				continue
			}
			sub := strings.Join(rule.Subcommand, " ")
			findings = append(findings, Finding{
				Severity: SeverityWarn,
				Title:    "Allowed subcommand is shell-equivalent",
				Detail: fmt.Sprintf(
					"tools.%s.allow permits subcommand %q, which matches %s's shell-equivalent shape %q — its "+
						"positional payloads are not content-constrained (audit F3), so this is a general-purpose "+
						"execution or data channel through %s, not a scoped operation",
					name, sub, name, strings.Join(matched, " "), name),
				Path: At("tools", name, "allow").Item("subcommand", sub),
				Remedy: fmt.Sprintf(
					"confirm tools.%s.allow's %q entry is genuinely required; deny it if not truly needed",
					name, sub),
				DocRef: "design/doctor/wraptool-doctor.md",
			})
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- policy.mutating-subcommand

func runPolicySubcmdMutating(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range policySubcmdSortedToolNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		tk, ok := LookupToolKnowledge(ToolBasename(tc.Binary))
		if !ok {
			continue // no per-tool knowledge at all — never a guess (D0's bounding rule)
		}
		for _, rule := range tc.Allow {
			verb, ok := tk.MutatingVerbMatch(rule.Subcommand)
			if !ok {
				continue
			}
			sub := strings.Join(rule.Subcommand, " ")
			findings = append(findings, Finding{
				Severity: SeverityWarn,
				Title:    "Allowed subcommand mutates remote state",
				Detail: fmt.Sprintf(
					"tools.%s.allow permits subcommand %q, whose last segment %q mutates remote state for %s. "+
						"The config schema has no construct for \"gated\" vs \"ungated\" allow — "+
						"wraptool_request_capability's human review happens before a rule reaches config.yaml, "+
						"so once here it is a standing, unconditional grant; flag_constraints/arg_constraints "+
						"bound values and counts, not whether this subcommand may run at all",
					name, sub, verb, name),
				Path: At("tools", name, "allow").Item("subcommand", sub),
				Remedy: fmt.Sprintf(
					"prefer denying tools.%s.allow's %q entry, or scope it as tightly as possible "+
						"(flag_constraints/deny_flags) if it must stay allowed", name, sub),
				DocRef: "design/policy/security-audit-policy-enforcement.md",
			})
		}
	}
	return Found(findings...)
}

// ---------------------------------------------------------------- policy.force-push-allowed

// policySubcmdForcePushFlags are the three spellings design §7.7's row names
// verbatim — git's three ways to force-push: the long flag, its safer sibling
// (still a force push), and the short alias. ALL three must be neutralized
// for the finding to stay silent: an operator who denies only "--force" while
// "-f" (its exact alias) is still callable has not actually closed anything,
// so partial coverage still fires, naming exactly which spelling(s) remain
// open.
var policySubcmdForcePushFlags = []string{"--force", "--force-with-lease", "-f"}

// policySubcmdFlagNeutralized reports whether flag is already blocked on
// rule — listed in DenyFlags, or carrying flag_constraints[flag].Deny==true.
// Mirrors §7.7's own "Exemptions, mandatory" clause (deny_flags checked
// before the allowlist; flag_constraints[f].deny: true) — that clause is
// textually scoped to unpatterned-value-flag/dangerous-flag (D2's checks),
// but the underlying question ("is this flag already blocked, by either
// mechanism the schema offers") is identical here, and honoring both is what
// keeps a config that neutralizes --force via flag_constraints instead of
// deny_flags from producing a false warn (the standing severity rule: prove
// the most plausible legitimate config stays silent).
func policySubcmdFlagNeutralized(rule config.AllowRule, flag string) bool {
	for _, d := range rule.DenyFlags {
		if d == flag {
			return true
		}
	}
	if fc, ok := rule.FlagConstraints[flag]; ok && fc.Deny {
		return true
	}
	return false
}

// policySubcmdReachable reports whether flag can actually be sent through rule
// at all. whitelist denies any flag absent from the rule's own `flags`
// allowlist (policy.go), which is the PRIMARY gate — deny_flags only subtracts
// from it. Without this bound the check fired on wraptool's own documented git
// push policy (`flags: ["--set-upstream"]`, `deny_flags: ["--force",
// "--force-with-lease"]`, printed verbatim in four docs pages), telling the
// operator that -f "still force-pushes" when the policy engine provably
// rejects it.
//
// rule.Flags is authoritative here: doctor's lenient load has already run
// applyDefaults, and SeedHelpFlags itself subtracts deny_flags, so what remains
// is exactly what a caller may send.
func policySubcmdReachable(rule config.AllowRule, flag string) bool {
	return slices.Contains(rule.Flags, flag)
}

// policySubcmdIsPush reports whether subcommand names `git push` — its first
// segment is "push". git push takes no further subcommand of its own, but
// this is a prefix check (not an exact-length one) for the same reason
// toolknowledge.go's own match rules are: a longer Subcommand slice starting
// with "push" is still `git push`'s own policy surface, not a different one.
func policySubcmdIsPush(subcommand []string) bool {
	return len(subcommand) > 0 && subcommand[0] == "push"
}

func runPolicySubcmdForcePush(_ context.Context, in *Input) Result {
	var findings []Finding
	for _, name := range policySubcmdSortedToolNames(in.Config.Tools) {
		tc := in.Config.Tools[name]
		if ToolBasename(tc.Binary) != "git" {
			continue
		}
		for _, rule := range tc.Allow {
			if !policySubcmdIsPush(rule.Subcommand) {
				continue
			}
			var open []string
			for _, f := range policySubcmdForcePushFlags {
				if policySubcmdReachable(rule, f) && !policySubcmdFlagNeutralized(rule, f) {
					open = append(open, f)
				}
			}
			if len(open) == 0 {
				continue
			}
			sub := strings.Join(rule.Subcommand, " ")
			findings = append(findings, Finding{
				Severity: SeverityWarn,
				Title:    "git push is allowed without force-push protection",
				Detail: fmt.Sprintf(
					"tools.%s.allow permits subcommand %q with no deny_flags or flag_constraints[f].deny "+
						"covering: %s — any of these still force-pushes, silently overwriting remote history",
					name, sub, strings.Join(open, ", ")),
				Path: At("tools", name, "allow").Item("subcommand", sub),
				Remedy: fmt.Sprintf(
					"add %s to tools.%s.allow's %q deny_flags (or set flag_constraints[<flag>].deny: true "+
						"for each)", strings.Join(open, "/"), name, sub),
				DocRef: "design/policy/security-audit-policy-enforcement.md",
			})
		}
	}
	return Found(findings...)
}
