// toolknowledge.go is the per-tool knowledge table design §7.8 describes:
// one in-tree table, keyed by tool BASENAME, holding what §7.7's policy.*
// content-lint checks (D1-D4) and the later drift group (Phase F) need to
// reason about a specific CLI tool by name — dangerous flags, shell-
// equivalent subcommands, mutating subcommands, and a catalog argv. This
// file is DATA ONLY (plan §2/§3): no check, no init(), no register() call.
// It is one of invariants_test.go's invariantsFoundationFiles, so
// TestGroupFileIdentifiersArePrefixedWithFileGroup does not scan it (or its
// own _test.go) for the group-prefix naming rule (plan §1 rule 7) — names
// below are chosen for readability, not for a "toolknowledge" prefix.
//
// THE BOUNDING RULE (architect brief): a check expressed as a list of known
// cases relocates a defect rather than closing it, unless the list itself IS
// the genuinely per-tool knowledge (which this table is, by design). Two
// consequences follow directly:
//
//   - A tool with no entry here gets NO finding from any check that consults
//     this table — never a guess built from another tool's data, and never a
//     generic fallback that pretends to know a shape it does not. See
//     LookupToolKnowledge's own doc comment.
//   - Where a per-tool SHAPE would otherwise require enumerating every
//     concrete case (gcloud's mutating verbs can trail ANY resource path —
//     "gcloud compute instances delete", "gcloud iam service-accounts
//     delete", the security audit's own "gcloud … delete" ellipsis), the
//     table stores the INVARIANT (the verb token) rather than the
//     unenumerable list of paths it can appear at the end of. See
//     ToolKnowledge.MutatingVerbs' own doc comment for why that makes its
//     match rule deliberately different from ShellEquivalentSubcommands'.
package doctor

import (
	"path/filepath"
	"strings"
)

// ToolKnowledge is one tool's entry in the per-tool knowledge table (design
// §7.8). Every field here is data a Phase-D check (D1-D3) or the later
// drift group (Phase F) reads; none of it is interpreted by this file.
type ToolKnowledge struct {
	// DangerousFlags are this tool's own execute-or-redirect flags (§7.7's
	// policy.dangerous-flag row), matched by EXACT spelling — e.g. git's
	// "-c" and "--config", never a substring or prefix match. The
	// tool-INDEPENDENT "--*-file"/"--*-cmd" shapes are deliberately NOT
	// folded in here; see IsGenericDangerousFlag.
	DangerousFlags map[string]bool

	// ShellEquivalentSubcommands are exact subcommand PATHS (§7.7's
	// policy.shell-equivalent-subcommand row) — general-purpose execution or
	// data channels through this tool: kubectl's are each ONE segment
	// (exec, cp, port-forward, proxy); gcloud's is TWO (compute, ssh).
	// Matched as a PREFIX of an allow rule's Subcommand slice, from segment
	// 0 — see ShellEquivalentMatch. Variable segment count is the whole
	// reason the entries are [][]string rather than []string: a lookup that
	// only handled one segment would silently miss "compute ssh".
	ShellEquivalentSubcommands [][]string

	// MutatingVerbs are bare verb TOKENS (§7.7's policy.mutating-subcommand
	// row) that write or delete remote state — kubectl's
	// delete/apply/edit/patch/replace/scale/drain, gcloud's
	// delete/set-iam-policy/add-iam-policy-binding, terraform's
	// apply/destroy/import.
	//
	// Matched against the LAST segment of an allow rule's Subcommand slice,
	// at ANY depth — NOT a prefix match, and this is a deliberate difference
	// from ShellEquivalentSubcommands, not an inconsistency: gcloud's verbs
	// trail an arbitrary, effectively unenumerable resource path
	// ("gcloud compute instances delete", "gcloud iam service-accounts
	// delete", "gcloud projects delete" are all the SAME verb at different
	// depths — the security audit's own "gcloud … delete" notation). A
	// fixed-depth PREFIX entry could only ever name one resource path out of
	// the hundreds gcloud has, which is exactly the "list of known cases"
	// shape the architect brief's bounding rule warns relocates the defect
	// rather than closing it. kubectl's and terraform's verbs are top-level
	// (one segment each), so last-segment matching gives the identical
	// result a prefix match would for them — only gcloud actually needs the
	// wider net, but all three tools share ONE matching rule rather than
	// three special cases.
	MutatingVerbs map[string]bool

	// CatalogArgv is the argv (excluding the binary itself) that lists this
	// tool's full subcommand catalog (design §5.3: "a tool with no catalog
	// argv in the table never produces a subcommand-drift finding — it
	// reports drift.unverifiable"): `git help -a`, `kubectl --help`,
	// `gcloud help`, `terraform --help`. NOT used by any check yet — it
	// ships with its data now so the later drift group (Phase F) does not
	// need a second per-tool table.
	CatalogArgv []string
}

// stringSet builds a membership set from a literal list — used for both
// DangerousFlags and MutatingVerbs below, since both are pure membership
// tests with no ordering requirement (unlike ShellEquivalentSubcommands,
// which must stay an ordered slice — see ShellEquivalentMatch).
func stringSet(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

// toolKnowledgeTable is keyed by lowercase tool basename. See
// LookupToolKnowledge for how a caller resolves a configured binary path
// into this table's key, and its doc comment for what a tool NOT listed
// here gets (nothing — never a guess).
//
// Data sourced verbatim from design/doctor/wraptool-doctor.md §7.7's own
// catalog rows for policy.dangerous-flag / shell-equivalent-subcommand /
// mutating-subcommand, cross-checked against
// design/policy/security-audit-policy-enforcement.md's matching checklist
// item. glab is deliberately NOT an entry here despite being named
// elsewhere as an example tool basename (CLAUDE.md's introspect section):
// neither design doc gives it dangerous-flag/subcommand data, and inventing
// some would be exactly the guess LookupToolKnowledge's doc comment forbids.
// DangerousFlags may only contain flags that are dangerous in the position
// wraptool can actually send them. executor.buildArgs always appends flags
// AFTER the subcommand, so a flag that is dangerous only in a tool's
// PRE-subcommand position cannot be sent through wraptool at all, and listing
// it produces a finding about a call the policy engine could never make.
//
// git is the tool where this bites: `git -c core.sshCommand=... pull` really
// does execute an arbitrary command, but wraptool would emit `git pull -c ...`,
// which git rejects. Meanwhile `-c` AFTER a subcommand is that subcommand's own
// flag and usually harmless -- `git switch -c <branch>` names a new branch --
// so listing `-c` warned operators about branch creation. kubectl and gcloud
// accept their global flags in either position (`kubectl get pods
// --kubeconfig=X`), so theirs stay.
var toolKnowledgeTable = map[string]ToolKnowledge{
	"git": {
		// Subcommand flags only, and every one of these runs a program:
		// --upload-pack/--receive-pack name the program git executes on the
		// other end of a fetch/push, and --exec is push's alias for the latter.
		DangerousFlags: stringSet("--upload-pack", "--receive-pack", "--exec"),
		CatalogArgv:    []string{"help", "-a"},
	},
	"kubectl": {
		DangerousFlags:             stringSet("--kubeconfig", "--token", "--server", "--as"),
		ShellEquivalentSubcommands: [][]string{{"exec"}, {"cp"}, {"port-forward"}, {"proxy"}},
		MutatingVerbs:              stringSet("delete", "apply", "edit", "patch", "replace", "scale", "drain"),
		CatalogArgv:                []string{"--help"},
	},
	"gcloud": {
		DangerousFlags:             stringSet("--impersonate-service-account", "--flags-file", "--configuration"),
		ShellEquivalentSubcommands: [][]string{{"compute", "ssh"}},
		MutatingVerbs:              stringSet("delete", "set-iam-policy", "add-iam-policy-binding"),
		CatalogArgv:                []string{"help"},
	},
	"terraform": {
		MutatingVerbs: stringSet("apply", "destroy", "import"),
		CatalogArgv:   []string{"--help"},
	},
}

// LookupToolKnowledge returns basename's entry in the per-tool knowledge
// table, keyed by exact lowercase tool basename ("git", "kubectl", "gcloud",
// "terraform"). ok is false for any tool not in the table — including a
// real, common tool like glab that simply has no entry yet (§7.8: "An
// unknown tool gets the generic checks only, plus info: no per-tool
// ruleset"). Callers (D1-D3) MUST treat !ok as "this check emits nothing for
// this tool", never approximate with another tool's data, a zero-value
// ToolKnowledge (which would silently claim "no dangerous flags, no
// mutating verbs" — a false negative, not an honest "unknown"), or a guess.
//
// basename is matched case-insensitively (ToolBasename already lowercases,
// but a caller passing a raw name directly is not punished for skipping
// it).
func LookupToolKnowledge(basename string) (ToolKnowledge, bool) {
	tk, ok := toolKnowledgeTable[strings.ToLower(basename)]
	return tk, ok
}

// ToolBasename reduces a configured tool binary path (a ToolConfig.Binary
// value) to the lowercase basename toolKnowledgeTable is keyed by: path
// separators and well-known Windows executable suffixes stripped, so a
// Phase-D check can pass tc.Binary straight through rather than
// re-deriving this itself.
//
// Cuts on BOTH '/' and '\\' explicitly (filepath.Base alone is not enough):
// doctor may run on Linux auditing a config authored on Windows, where
// filepath.Base never splits a backslash path — mirroring mcpservers.go's
// mcpRunnerBase, which has the identical requirement for the same reason.
// Not shared with that helper: it is scoped to the npx/uvx runner
// vocabulary, a different (and smaller) suffix set.
func ToolBasename(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if i := strings.LastIndexByte(base, '\\'); i >= 0 {
		base = base[i+1:]
	}
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if trimmed, cut := strings.CutSuffix(base, ext); cut {
			return trimmed
		}
	}
	return base
}

// IsDangerousFlag reports whether flag is on tk's OWN exact dangerous-flag
// list. It does not consult IsGenericDangerousFlag — a caller wanting both
// (the normal case for policy.dangerous-flag, D2) calls both explicitly, so
// neither predicate silently absorbs the other's job.
func (tk ToolKnowledge) IsDangerousFlag(flag string) bool {
	return tk.DangerousFlags[flag]
}

// genericDangerousFlagSuffixes are the tool-INDEPENDENT shapes design §7.7
// names as a separate clause alongside the per-tool lists: "plus generic
// --*-file/--*-cmd shapes". Kept apart from any ToolKnowledge.DangerousFlags
// set on purpose — see IsGenericDangerousFlag.
var genericDangerousFlagSuffixes = []string{"-file", "-cmd"}

// IsGenericDangerousFlag reports whether flag has the shape design §7.7
// calls out as a SEPARATE, tool-independent predicate — never buried inside
// a per-tool DangerousFlags list, and applicable even to a tool with NO
// ToolKnowledge entry at all: a long flag whose name ends "-file" or "-cmd"
// (e.g. "--env-file", "--exec-cmd") is dangerous by construction, because of
// what its VALUE does (names a file the tool reads/writes, or a command the
// tool runs), regardless of which tool or subcommand carries it.
//
// Only long ("--") flags are considered: a short flag ("-f") has no
// descriptive suffix for this shape to match, and treating e.g. "-f" (git's
// force flag on many subcommands) as matching "-cmd"/"-file" by coincidence
// of its trailing letters would be a false positive with no signal behind
// it at all.
func IsGenericDangerousFlag(flag string) bool {
	if !strings.HasPrefix(flag, "--") {
		return false
	}
	lower := strings.ToLower(flag)
	for _, suffix := range genericDangerousFlagSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// subcommandHasPrefix reports whether path is a prefix of subcommand,
// compared segment by segment, case-sensitively — mirroring
// internal/whitelist/policy.go's own subcommandMatches (exact-case,
// exact-token comparison). subcommand may be longer than path; it is never
// shorter for a match to hold.
func subcommandHasPrefix(subcommand, path []string) bool {
	if len(path) > len(subcommand) {
		return false
	}
	for i, seg := range path {
		if subcommand[i] != seg {
			return false
		}
	}
	return true
}

// ShellEquivalentMatch reports whether subcommand — an allow rule's own
// Subcommand slice — matches one of tk's shell-equivalent subcommand paths,
// and which path matched. A path matches when it is a PREFIX of subcommand
// (subcommandHasPrefix): kubectl's one-segment "exec" matches
// subcommand == ["exec"]; gcloud's two-segment "compute","ssh" matches
// subcommand == ["compute", "ssh"] (and would also match a longer
// subcommand starting with those two tokens, though today's config schema
// gives Subcommand no reason to be longer than the path it names).
//
// Table order decides which path is reported first when more than one could
// match — never happens with today's data (no tool's entries share a common
// first segment), but ShellEquivalentSubcommands is iterated as the fixed
// slice it is (never converted to a map first) precisely so this stays
// deterministic if that ever changes.
func (tk ToolKnowledge) ShellEquivalentMatch(subcommand []string) (matched []string, ok bool) {
	for _, path := range tk.ShellEquivalentSubcommands {
		if subcommandHasPrefix(subcommand, path) {
			return path, true
		}
	}
	return nil, false
}

// MutatingVerbMatch reports whether subcommand's LAST segment is one of
// tk's mutating verbs, and which one. See MutatingVerbs' own doc comment for
// why this is a last-segment match rather than the prefix match
// ShellEquivalentMatch uses. An empty subcommand slice never matches.
func (tk ToolKnowledge) MutatingVerbMatch(subcommand []string) (verb string, ok bool) {
	if len(subcommand) == 0 {
		return "", false
	}
	last := subcommand[len(subcommand)-1]
	if tk.MutatingVerbs[last] {
		return last, true
	}
	return "", false
}
