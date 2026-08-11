package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------- yaml.v3 attachment (verified, not assumed)

// TestSuppressYAMLAttachesSequenceItemCommentToItemMappingNode pins the
// FIRST of §6.2's two placements against real yaml.v3 behaviour: a comment
// written before a sequence item attaches to the item's OWN MAPPING node —
// not to the first key inside it ("subcommand"). An earlier design draft
// guessed this landed somewhere else; this test is the guard against that
// mistake recurring.
func TestSuppressYAMLAttachesSequenceItemCommentToItemMappingNode(t *testing.T) {
	const doc = `
tools:
  git:
    allow:
      # doctor:ignore policy.dangerous-flag -- reviewed
      - subcommand: [pull]
        flags: ["-c"]
`
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	root := locateRoot(&n)
	_, toolsVal, ok := locateMapChild(root, "tools")
	if !ok {
		t.Fatal("tools not found")
	}
	_, gitVal, ok := locateMapChild(toolsVal, "git")
	if !ok {
		t.Fatal("git not found")
	}
	_, allow, ok := locateMapChild(gitVal, "allow")
	if !ok {
		t.Fatal("allow not found")
	}
	if allow.Kind != yaml.SequenceNode || len(allow.Content) != 1 {
		t.Fatalf("allow = %+v, want a one-item sequence", allow)
	}
	item := allow.Content[0]
	if item.Kind != yaml.MappingNode {
		t.Fatalf("item.Kind = %v, want MappingNode", item.Kind)
	}
	if !strings.Contains(item.HeadComment, "doctor:ignore policy.dangerous-flag") {
		t.Errorf("item.HeadComment = %q, want it to carry the ignore comment", item.HeadComment)
	}
	// The comment must NOT have attached to the "subcommand" key instead.
	subKey := item.Content[0]
	if subKey.Value != "subcommand" {
		t.Fatalf("item.Content[0] = %+v, want the subcommand key", subKey)
	}
	if subKey.HeadComment != "" {
		t.Errorf("subcommand key HeadComment = %q, want empty — the comment belongs to the item, not its first key", subKey.HeadComment)
	}
}

// TestSuppressYAMLAttachesMappingKeyCommentToKeyScalarNotValue pins the
// SECOND placement: a comment written before a mapping key attaches to the
// KEY SCALAR, never to that key's value node — the exact claim the earlier
// design draft got backwards.
func TestSuppressYAMLAttachesMappingKeyCommentToKeyScalarNotValue(t *testing.T) {
	const doc = `
tools:
  # doctor:ignore policy.dangerous-flag -- reviewed
  git:
    allow: []
`
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	root := locateRoot(&n)
	_, toolsVal, ok := locateMapChild(root, "tools")
	if !ok {
		t.Fatal("tools not found")
	}
	gitKey, gitVal, ok := locateMapChild(toolsVal, "git")
	if !ok {
		t.Fatal("git not found")
	}
	if !strings.Contains(gitKey.HeadComment, "doctor:ignore policy.dangerous-flag") {
		t.Errorf("git KEY HeadComment = %q, want it to carry the ignore comment", gitKey.HeadComment)
	}
	if gitVal.HeadComment != "" {
		t.Errorf("git VALUE (mapping node) HeadComment = %q, want empty — the comment belongs to the key scalar, not the value", gitVal.HeadComment)
	}
}

// ---------------------------------------------------------------- suppressParseComment

func TestSuppressParseComment(t *testing.T) {
	cases := []struct {
		name    string
		comment string
		want    []suppressIgnoreText
	}{
		{"with reason", "# doctor:ignore policy.dangerous-flag -- reviewed 2026-08",
			[]suppressIgnoreText{{ID: "policy.dangerous-flag", Reason: "reviewed 2026-08"}}},
		{"no reason", "# doctor:ignore policy.dangerous-flag",
			[]suppressIgnoreText{{ID: "policy.dangerous-flag", Reason: ""}}},
		{"extra whitespace", "#   doctor:ignore   policy.dangerous-flag   --   reviewed  ",
			[]suppressIgnoreText{{ID: "policy.dangerous-flag", Reason: "reviewed"}}},
		{"ordinary comment ignored", "# just a note about this rule", nil},
		{"prose mentioning the phrase is not a directive", "# see doctor:ignore usage docs", nil},
		{"word-boundary rejects a run-on token", "# doctor:ignoreme policy.x", nil},
		{"empty comment", "", nil},
		{"bare directive with no id", "# doctor:ignore", nil},
		{"two stacked lines", "# doctor:ignore policy.a -- first\n# doctor:ignore policy.b -- second",
			[]suppressIgnoreText{{ID: "policy.a", Reason: "first"}, {ID: "policy.b", Reason: "second"}}},
		{"reason containing its own dashes", "# doctor:ignore policy.dangerous-flag -- fine -- really",
			[]suppressIgnoreText{{ID: "policy.dangerous-flag", Reason: "fine -- really"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := suppressParseComment(tc.comment)
			if len(got) != len(tc.want) {
				t.Fatalf("suppressParseComment(%q) = %+v, want %+v", tc.comment, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("suppressParseComment(%q)[%d] = %+v, want %+v", tc.comment, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------- suppressWalk / line ranges

// suppressTestDoc exercises both placements plus a THIRD rule that no
// directive covers at all — the negative control every scope test below
// checks against, so a bug that suppresses everything indiscriminately
// cannot pass silently.
const suppressTestDoc = `
tools:
  git:
    binary: /usr/bin/git
    allow:
      # doctor:ignore policy.dangerous-flag -- reviewed 2026-08, value is fixed by the wrapper
      - subcommand: [pull]
        flags: ["-c"]
      - subcommand: [push]
        flags: ["--force"]
  # doctor:ignore policy.dangerous-flag -- reviewed for kubectl as a whole
  kubectl:
    binary: /usr/bin/kubectl
    allow:
      - subcommand: [exec]
        flags: ["--token"]
  gcloud:
    binary: /usr/bin/gcloud
    allow:
      - subcommand: [compute]
        flags: ["--impersonate-service-account"]
`

func TestSuppressWalkComputesLineRanges(t *testing.T) {
	ds := suppressDirectivesIn([]byte(suppressTestDoc))
	if len(ds) != 2 {
		t.Fatalf("suppressDirectivesIn = %+v, want exactly 2 directives", ds)
	}
	// git's "pull" rule (sequence-item placement): scoped to lines 7-8 only,
	// NOT the sibling "push" rule at lines 9-10.
	item := ds[0]
	if item.ID != "policy.dangerous-flag" || item.StartLine != 7 || item.EndLine != 8 {
		t.Errorf("item directive = %+v, want ID policy.dangerous-flag, range [7,8]", item)
	}
	// kubectl (mapping-key placement): scoped to the WHOLE kubectl subtree,
	// lines 12-16 -- both binary: and every rule under allow:.
	key := ds[1]
	if key.ID != "policy.dangerous-flag" || key.StartLine != 12 || key.EndLine != 16 {
		t.Errorf("key directive = %+v, want ID policy.dangerous-flag, range [12,16]", key)
	}
}

// TestSuppressWalkNilNodeReturnsZero exercises the guard directly: a nil
// *yaml.Node (never produced by yaml.Unmarshal itself, but reachable if a
// future caller passes one) must not panic.
func TestSuppressWalkNilNodeReturnsZero(t *testing.T) {
	var out []suppressDirective
	if got := suppressWalk(nil, &out); got != 0 {
		t.Errorf("suppressWalk(nil) = %d, want 0", got)
	}
	if out != nil {
		t.Errorf("out = %+v, want nil", out)
	}
}

// TestSuppressWalkDocumentNodeRecursesIntoItsSoleChild covers the
// DocumentNode case directly: suppressParseRoot always unwraps to the
// mapping root before calling suppressWalk (so this case is normally
// unreachable through the package's own entry point, mirroring
// locate.go's locateFieldValue's own DocumentNode/AliasNode arm), but the
// switch names it for the exhaustive linter and it must still behave
// correctly if ever invoked directly.
func TestSuppressWalkDocumentNodeRecursesIntoItsSoleChild(t *testing.T) {
	var doc yaml.Node
	raw := "tools:\n  git:\n    # doctor:ignore policy.dangerous-flag -- reviewed\n    binary: /usr/bin/git\n"
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if doc.Kind != yaml.DocumentNode {
		t.Fatalf("doc.Kind = %v, want DocumentNode", doc.Kind)
	}
	var out []suppressDirective
	got := suppressWalk(&doc, &out)
	if got != 4 {
		t.Errorf("suppressWalk(document) = %d, want 4 (deepest line, the binary: value)", got)
	}
	if len(out) != 1 || out[0].ID != "policy.dangerous-flag" {
		t.Errorf("out = %+v, want the one directive found through the document root", out)
	}
}

// ---------------------------------------------------------------- suppressLocationLine

func TestSuppressLocationLine(t *testing.T) {
	cases := []struct {
		location string
		wantLine int
		wantOK   bool
	}{
		{"/x/config.yaml:37", 37, true},
		{"C:\\Users\\x\\config.yaml:12", 12, true}, // a Windows path's own colon must not confuse the split
		{"no-colon-at-all", 0, false},
		{"/x/config.yaml:not-a-number", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		line, ok := suppressLocationLine(tc.location)
		if ok != tc.wantOK || (ok && line != tc.wantLine) {
			t.Errorf("suppressLocationLine(%q) = (%d, %v), want (%d, %v)", tc.location, line, ok, tc.wantLine, tc.wantOK)
		}
	}
}

// ---------------------------------------------------------------- ApplySuppression

// suppressWarnFinding builds a warn finding identified by a UNIQUE detail
// label (never reused across a test's other findings) rather than by index:
// Run() sorts findings into canonical order (severity, ID, Location, Detail),
// so a test that needs to look one back up after ApplySuppression must match
// on something the sort/suppression pass cannot have changed the IDENTITY
// of. Finding itself cannot be used as a map key (Path embeds a slice), so
// suppressFindByDetail below does a linear scan instead.
//
// It deliberately leaves Finding.ID empty: Run's own validateResult rejects
// a check-set ID that is not registered (finding.go's own doc comment — "the
// engine then verifies that ID is registered"), and none of §7's policy.*
// IDs are registered yet on this branch (Phase D lands after C2). Leaving ID
// empty lets the engine stamp it from the synthetic check's own Meta.ID
// instead (suppressReportAt), exactly like every real Phase-B/D check does.
func suppressWarnFinding(detail string, path ConfigPath) Finding {
	return Finding{Severity: SeverityWarn, Title: "t", Detail: detail, Remedy: "fix it", Path: path}
}

func suppressFindByDetail(t *testing.T, r Report, detail string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Detail == detail {
			return f
		}
	}
	t.Fatalf("no finding with Detail %q in %+v", detail, r.Findings)
	return Finding{}
}

// suppressReportAt runs Run() with a single synthetic check, registered
// under id (see suppressWarnFinding's doc comment for why), emitting fs
// against a Locator built from doc — so every finding's Location is
// resolved exactly the way cmd/doctor.go's real pipeline resolves it,
// before handing the Report to ApplySuppression, never hand-built.
func suppressReportAt(t *testing.T, id CheckID, doc string, fs ...Finding) Report {
	t.Helper()
	check := doctorCheck(id, NeedsNothing, func(context.Context, *Input) Result {
		return Found(fs...)
	})
	in := &Input{Env: NewMapEnv(), GOOS: "linux", ConfigPath: "/x/config.yaml", ConfigState: ConfigLoaded}
	return Run(context.Background(), in, RunOptions{
		Checks:  []Check{check},
		Locator: NewLocator("/x/config.yaml", []byte(doc)),
	})
}

// TestApplySuppressionSequenceItemScopeIsOneRuleOnly is design §6.2's own
// worked example: the ignore comment before git's "pull" rule suppresses a
// finding on that rule's own flags, but NOT the same check ID on the
// sibling "push" rule, and NOT on gcloud's rule (no directive covers it at
// all).
func TestApplySuppressionSequenceItemScopeIsOneRuleOnly(t *testing.T) {
	pullFlag := suppressWarnFinding("pull -c",
		At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-c"))
	pushFlag := suppressWarnFinding("push --force",
		At("tools", "git", "allow").Item("subcommand", "push").Key("flags").Elem("--force"))
	gcloudFlag := suppressWarnFinding("gcloud impersonate",
		At("tools", "gcloud", "allow").Item("subcommand", "compute").Key("flags").Elem("--impersonate-service-account"))

	r := suppressReportAt(t, "policy.dangerous-flag", suppressTestDoc, pullFlag, pushFlag, gcloudFlag)
	r = ApplySuppression(r, []byte(suppressTestDoc))

	if f := suppressFindByDetail(t, r, "pull -c"); !f.Suppressed || f.SuppressionReason != "reviewed 2026-08, value is fixed by the wrapper" {
		t.Errorf("pull's flag = %+v, want suppressed with the item-level reason", f)
	}
	if f := suppressFindByDetail(t, r, "push --force"); f.Suppressed {
		t.Errorf("push's flag = %+v, want NOT suppressed (sibling rule, out of the item directive's scope)", f)
	}
	if f := suppressFindByDetail(t, r, "gcloud impersonate"); f.Suppressed {
		t.Errorf("gcloud's flag = %+v, want NOT suppressed (no directive covers it at all)", f)
	}
	if r.Summary.Suppressed != 1 {
		t.Errorf("Summary.Suppressed = %d, want 1", r.Summary.Suppressed)
	}
}

// TestApplySuppressionMappingKeyScopeCoversWholeSubtree is §6.2's other
// placement: the ignore comment before the "kubectl:" key suppresses a
// matching finding ANYWHERE under kubectl (its binary AND every rule under
// allow), unlike the narrower sequence-item scope above.
func TestApplySuppressionMappingKeyScopeCoversWholeSubtree(t *testing.T) {
	kubectlBinary := suppressWarnFinding("kubectl binary",
		At("tools", "kubectl", "binary"))
	kubectlFlag := suppressWarnFinding("kubectl exec --token",
		At("tools", "kubectl", "allow").Item("subcommand", "exec").Key("flags").Elem("--token"))

	r := suppressReportAt(t, "policy.dangerous-flag", suppressTestDoc, kubectlBinary, kubectlFlag)
	r = ApplySuppression(r, []byte(suppressTestDoc))

	if f := suppressFindByDetail(t, r, "kubectl binary"); !f.Suppressed {
		t.Errorf("kubectl binary = %+v, want suppressed (mapping-key scope covers the whole subtree)", f)
	}
	if f := suppressFindByDetail(t, r, "kubectl exec --token"); !f.Suppressed || f.SuppressionReason != "reviewed for kubectl as a whole" {
		t.Errorf("kubectl exec flag = %+v, want suppressed with the key-level reason", f)
	}
}

// TestApplySuppressionOnlyWarnIsSuppressible pins §6.2: an ignore comment
// covering a FAIL-severity finding's location is inert. Every fail in the
// catalog was re-checked against "never fires on a working configuration",
// so this is not a judgement call doctor makes silently.
func TestApplySuppressionOnlyWarnIsSuppressible(t *testing.T) {
	failFinding := Finding{
		Severity: SeverityFail, Title: "t", Detail: "d", Remedy: "fix it",
		Path: At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-c"),
	}
	r := suppressReportAt(t, "policy.dangerous-flag", suppressTestDoc, failFinding)
	r = ApplySuppression(r, []byte(suppressTestDoc))
	if r.Findings[0].Suppressed {
		t.Errorf("fail finding = %+v, want never suppressed regardless of a covering comment", r.Findings[0])
	}
	if r.Summary.Suppressed != 0 {
		t.Errorf("Summary.Suppressed = %d, want 0", r.Summary.Suppressed)
	}
}

// TestApplySuppressionUnresolvedLocationNeverSuppressed covers a finding
// whose Path never resolved (e.g. a stale rule the operator already
// deleted): it has no Location, and therefore no line to anchor a comment
// lookup to — never suppressed, even though its ID matches a directive
// elsewhere in the file.
func TestApplySuppressionUnresolvedLocationNeverSuppressed(t *testing.T) {
	stale := suppressWarnFinding("stale",
		At("tools", "git", "allow").Item("subcommand", "does-not-exist").Key("flags").Elem("-c"))
	r := suppressReportAt(t, "policy.dangerous-flag", suppressTestDoc, stale)
	if r.Findings[0].Location != "" {
		t.Fatalf("precondition failed: expected an unresolved Location, got %q", r.Findings[0].Location)
	}
	r = ApplySuppression(r, []byte(suppressTestDoc))
	if r.Findings[0].Suppressed {
		t.Errorf("stale finding = %+v, want never suppressed with no resolved Location", r.Findings[0])
	}
}

// TestApplySuppressionInnermostDirectiveWins: a directive on the enclosing
// rule (item placement) and a MORE specific directive on a key nested
// inside it both name the same check ID; the finding must report the
// innermost directive's reason.
func TestApplySuppressionInnermostDirectiveWins(t *testing.T) {
	const doc = `
tools:
  git:
    allow:
      # doctor:ignore policy.dangerous-flag -- outer
      - subcommand: [pull]
        # doctor:ignore policy.dangerous-flag -- inner
        flags: ["-c"]
`
	f := suppressWarnFinding("d",
		At("tools", "git", "allow").Item("subcommand", "pull").Key("flags"))
	r := suppressReportAt(t, "policy.dangerous-flag", doc, f)
	r = ApplySuppression(r, []byte(doc))
	if !r.Findings[0].Suppressed || r.Findings[0].SuppressionReason != "inner" {
		t.Errorf("finding = %+v, want suppressed with the innermost (\"inner\") reason", r.Findings[0])
	}
}

// TestApplySuppressionDoesNotMutateInputReport guards the value-semantics
// contract ApplySuppression's doc comment promises: calling it must not
// retroactively mutate a Report a caller is still holding a reference to
// (e.g. two callers racing on the same Run() output).
func TestApplySuppressionDoesNotMutateInputReport(t *testing.T) {
	f := suppressWarnFinding("d",
		At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-c"))
	before := suppressReportAt(t, "policy.dangerous-flag", suppressTestDoc, f)
	_ = ApplySuppression(before, []byte(suppressTestDoc))
	if before.Findings[0].Suppressed {
		t.Errorf("the ORIGINAL Report's finding was mutated in place, want it untouched")
	}
}

func TestApplySuppressionEmptyRawIsNoop(t *testing.T) {
	f := suppressWarnFinding("d",
		At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-c"))
	r := suppressReportAt(t, "policy.dangerous-flag", suppressTestDoc, f)
	r = ApplySuppression(r, nil)
	if r.Findings[0].Suppressed {
		t.Errorf("finding = %+v, want not suppressed against nil raw", r.Findings[0])
	}
}

func TestApplySuppressionMalformedRawIsNoop(t *testing.T) {
	f := suppressWarnFinding("d",
		At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-c"))
	r := suppressReportAt(t, "policy.dangerous-flag", suppressTestDoc, f)
	r = ApplySuppression(r, []byte("tools: [this is not\n  a valid: document"))
	if r.Findings[0].Suppressed {
		t.Errorf("finding = %+v, want not suppressed against malformed raw", r.Findings[0])
	}
}

func TestApplySuppressionNoFindingsIsNoop(t *testing.T) {
	r := ApplySuppression(Report{}, []byte(suppressTestDoc))
	if r.Summary.Suppressed != 0 || len(r.Findings) != 0 {
		t.Errorf("r = %+v, want an untouched empty report", r)
	}
}

// TestApplySuppressionMalformedLocationNeverSuppressed covers the
// suppressLocationLine failure branch directly: ApplySuppression takes any
// Report value, not just one built by Run()/C1's Locator (which always
// produces a well-formed "file:line" or leaves Location empty), so a
// Location that fails to parse must fail closed rather than panic or match
// spuriously.
func TestApplySuppressionMalformedLocationNeverSuppressed(t *testing.T) {
	r := Report{Findings: []Finding{
		{ID: "policy.dangerous-flag", Severity: SeverityWarn, Title: "t", Detail: "d", Remedy: "fix it", Location: "not-a-valid-location"},
	}}
	r = ApplySuppression(r, []byte(suppressTestDoc))
	if r.Findings[0].Suppressed {
		t.Errorf("finding = %+v, want not suppressed against a malformed Location", r.Findings[0])
	}
}

// ---------------------------------------------------------------- config.unknown-suppression

func TestSuppressUnknownIDMeta(t *testing.T) {
	found := false
	for _, c := range Registered() {
		if c.Meta().ID == suppressUnknownID {
			found = true
			if c.Meta().Needs != NeedsConfig {
				t.Errorf("Needs = %v, want NeedsConfig", c.Meta().Needs)
			}
		}
	}
	if !found {
		t.Fatalf("%q is not registered", suppressUnknownID)
	}
}

func TestSuppressUnknownIDFlagsAnUnrecognizedID(t *testing.T) {
	const doc = `
tools:
  git:
    allow:
      # doctor:ignore policy.dangeruos-flag -- typo'd id
      - subcommand: [pull]
`
	in := &Input{Env: NewMapEnv(), ConfigPath: "/x/config.yaml", ConfigState: ConfigLoaded,
		Config: &config.Config{}, ConfigRaw: []byte(doc)}
	res := runSuppressUnknownID(context.Background(), in)
	if len(res.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly 1", res.Findings)
	}
	f := res.Findings[0]
	if f.Severity != SeverityWarn {
		t.Errorf("Severity = %v, want warn", f.Severity)
	}
	if !strings.Contains(f.Detail, "/x/config.yaml:6") || !strings.Contains(f.Detail, "policy.dangeruos-flag") {
		t.Errorf("Detail = %q, want it to name the line and the misspelled ID", f.Detail)
	}
	if !f.Path.Empty() {
		t.Errorf("Path = %v, want empty — a comment has no config VALUE to anchor a ConfigPath to", f.Path)
	}
	if f.Location != "" {
		t.Errorf("Location = %q, want empty (never hand-rolled, plan §1 rule 9)", f.Location)
	}
}

func TestSuppressUnknownIDIgnoresARecognizedID(t *testing.T) {
	const doc = `
tools:
  git:
    allow:
      # doctor:ignore config.parse -- a real, if odd, id to ignore here
      - subcommand: [pull]
`
	in := &Input{Env: NewMapEnv(), ConfigPath: "/x/config.yaml", ConfigState: ConfigLoaded,
		Config: &config.Config{}, ConfigRaw: []byte(doc)}
	res := runSuppressUnknownID(context.Background(), in)
	if len(res.Findings) != 0 {
		t.Errorf("Findings = %+v, want none: config.parse is a real registered check ID", res.Findings)
	}
}

func TestSuppressUnknownIDNoDirectivesPasses(t *testing.T) {
	in := &Input{Env: NewMapEnv(), ConfigPath: "/x/config.yaml", ConfigState: ConfigLoaded,
		Config: &config.Config{}, ConfigRaw: []byte("tools:\n  git:\n    binary: /usr/bin/git\n")}
	res := runSuppressUnknownID(context.Background(), in)
	if res.Outcome() != OutcomePass {
		t.Errorf("Outcome = %v, want pass — a clean config with zero doctor:ignore comments is the common case, not a skip", res.Outcome())
	}
}

func TestSuppressUnknownIDNoConfigRawSkips(t *testing.T) {
	in := &Input{Env: NewMapEnv(), ConfigPath: "/x/config.yaml", ConfigState: ConfigLoaded, Config: &config.Config{}}
	res := runSuppressUnknownID(context.Background(), in)
	if res.Outcome() != OutcomeSkipped {
		t.Errorf("Outcome = %v, want skipped: no ConfigRaw to scan", res.Outcome())
	}
}

func TestSuppressUnknownIDMalformedRawSkips(t *testing.T) {
	in := &Input{Env: NewMapEnv(), ConfigPath: "/x/config.yaml", ConfigState: ConfigLoaded,
		Config: &config.Config{}, ConfigRaw: []byte("tools: [this is not\n  a valid: document")}
	res := runSuppressUnknownID(context.Background(), in)
	if res.Outcome() != OutcomeSkipped {
		t.Errorf("Outcome = %v, want skipped: config could not be parsed", res.Outcome())
	}
}

func TestSuppressUnknownIDMultipleOccurrencesEachReported(t *testing.T) {
	const doc = `
tools:
  git:
    allow:
      # doctor:ignore policy.dangeruos-flag -- typo one
      - subcommand: [pull]
      # doctor:ignore policy.dangeruos-flag -- typo two
      - subcommand: [push]
`
	in := &Input{Env: NewMapEnv(), ConfigPath: "/x/config.yaml", ConfigState: ConfigLoaded,
		Config: &config.Config{}, ConfigRaw: []byte(doc)}
	res := runSuppressUnknownID(context.Background(), in)
	if len(res.Findings) != 2 {
		t.Fatalf("Findings = %+v, want exactly 2 (one per occurrence, not deduplicated by ID)", res.Findings)
	}
}
