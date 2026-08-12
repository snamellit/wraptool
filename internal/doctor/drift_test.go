package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/introspect"
)

// ---------------------------------------------------------------- fixtures/helpers

// driftIsolateCacheDir points XDG_CACHE_HOME at a fresh t.TempDir() for the
// duration of the test, so introspect.SaveCache/LoadCacheStale — which this
// package's drift checks call directly, per drift.go's own header comment —
// never touch this HOST's real introspection cache. Mirrors
// internal/introspect/cache_test.go's own isolateCacheDir; duplicated rather
// than exported, since introspect has no reason to grow a test-only export
// for one caller outside the package.
func driftIsolateCacheDir(t *testing.T) {
	t.Helper()
	old, had := os.LookupEnv("XDG_CACHE_HOME")
	if err := os.Setenv("XDG_CACHE_HOME", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	xdg.Reload()
	t.Cleanup(func() {
		if had {
			os.Setenv("XDG_CACHE_HOME", old)
		} else {
			os.Unsetenv("XDG_CACHE_HOME")
		}
		xdg.Reload()
	})
}

// driftFakeShellScript writes an executable POSIX shell script at dir/name
// that prints stdout verbatim for ANY invocation (any argv, exit 0) — enough
// to drive both a --version probe (ProbeVersion takes the first line) and a
// catalog probe (ProbeCatalog runs the tool's own catalog argv, which this
// script ignores and answers identically). Mirrors
// internal/introspect/catalog_test.go's own fakeShellScript; duplicated for
// the same reason as driftIsolateCacheDir. Skips (never fails) on a platform
// with no POSIX shell.
func driftFakeShellScript(t *testing.T, dir, name, stdout string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script probing needs a POSIX shell")
	}
	bin := filepath.Join(dir, name)
	script := "#!/bin/sh\ncat <<'HELP'\n" + stdout + "HELP\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// driftGitCatalog is a git-shaped subcommand catalog listing, parseable by
// introspect.BestParseSubcommands into a non-empty catalog — enough for
// driftCatalogReason to accept a probe as trustworthy (Exhaustive == true).
const driftGitCatalog = `usage: git <command> [<args>]

These are common Git commands used in various situations:

   pull       Fetch from and integrate with another repository
   push       Update remote refs along with associated objects
   status     Show the working tree status
`

// driftInput builds a *Input suitable for driving runDriftToolVersionChanged/
// runDriftUnverifiable directly (plan §1 rule 8: tested by calling the
// RunFunc, never through Registered()). Env is a bare MapEnv — neither check
// in this file ever calls it (drift.go's own header comment: probing and
// cache reads go straight through introspect, not Env) — set only so Input
// satisfies doctor.Run's nil-Env guard, should a future caller route through
// it.
func driftInput(tools map[string]config.ToolConfig) *Input {
	return &Input{
		Env:    NewMapEnv(),
		GOOS:   "linux",
		Config: &config.Config{Tools: tools},
		Probe:  true,
	}
}

// driftSeedCache is introspect.SaveCache with drift_test.go's own defaults
// filled in (IntrospectedAt, an empty Subcommands map when the caller has no
// use for one) so each test case only states what it cares about.
func driftSeedCache(t *testing.T, name, versionOutput string, subs map[string]introspect.Subcommand) {
	t.Helper()
	if subs == nil {
		subs = map[string]introspect.Subcommand{}
	}
	if err := introspect.SaveCache(name, &introspect.Schema{
		VersionOutput:  versionOutput,
		IntrospectedAt: time.Now(),
		Subcommands:    subs,
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- Meta

// TestDriftCheckMeta pins both checks' ID/Needs against the SAME Meta
// literals init() registers, never through Registered() (plan §1 rule 8):
// once every other Phase-B/C/D/F file has merged, Registered() carries every
// one of THEIR checks too, so an assertion against it would not be testing
// this file's own behaviour in isolation.
func TestDriftCheckMeta(t *testing.T) {
	cases := []struct {
		meta      Meta
		wantID    CheckID
		wantNeeds Requirement
	}{
		{driftToolVersionChangedMeta, driftToolVersionChanged, NeedsConfig | NeedsProbe},
		{driftUnverifiableMeta, driftUnverifiable, NeedsConfig | NeedsProbe},
	}
	for _, tc := range cases {
		t.Run(string(tc.wantID), func(t *testing.T) {
			if tc.meta.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", tc.meta.ID, tc.wantID)
			}
			if tc.meta.Needs != tc.wantNeeds {
				t.Errorf("Needs = %v, want %v", tc.meta.Needs, tc.wantNeeds)
			}
			if strings.TrimSpace(tc.meta.Summary) == "" {
				t.Error("empty Meta.Summary")
			}
		})
	}
}

// ---------------------------------------------------------------- drift.tool-version-changed

func TestRunDriftToolVersionChanged(t *testing.T) {
	t.Run("no cached schema at all: no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "tool", "v2.0.0\n")
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftToolVersionChanged(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (nothing cached to compare against)", res.Outcome())
		}
	})

	t.Run("cached version has no VersionOutput recorded: no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "tool", "git version 2.44.0\n")
		driftSeedCache(t, "git", "", nil)
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftToolVersionChanged(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (no cached version to compare)", res.Outcome())
		}
	})

	t.Run("cached version matches installed: no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "tool", "git version 2.44.0\nextra line ignored\n")
		driftSeedCache(t, "git", "git version 2.44.0", nil)
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftToolVersionChanged(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (versions agree)", res.Outcome())
		}
	})

	t.Run("cached version differs from installed: info finding naming both, located at binary", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "tool", "git version 2.45.0\n")
		driftSeedCache(t, "git", "git version 2.44.0", nil)
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftToolVersionChanged(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		f := res.Findings[0]
		if f.Severity != SeverityInfo {
			t.Errorf("Severity = %q, want info", f.Severity)
		}
		if !strings.Contains(f.Detail, "2.44.0") || !strings.Contains(f.Detail, "2.45.0") {
			t.Errorf("Detail %q does not name both the cached and installed versions", f.Detail)
		}
		if got, want := f.Path.String(), At("tools", "git", "binary").String(); got != want {
			t.Errorf("Path = %q, want %q", got, want)
		}
		if f.Location != "" {
			t.Errorf("Location = %q, want empty — a check must set Path, never Location, for a config subject", f.Location)
		}
	})

	t.Run("probe fails (binary missing): no finding even with a cached version", func(t *testing.T) {
		driftIsolateCacheDir(t)
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		driftSeedCache(t, "git", "git version 2.44.0", nil)
		tools := map[string]config.ToolConfig{
			"git": {Binary: missing, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftToolVersionChanged(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (the probe itself cannot run)", res.Outcome())
		}
	})

	t.Run("multiple tools: sorted, deterministic, one finding per drifted tool", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		binA := driftFakeShellScript(t, dir, "toola", "version A2\n")
		binB := driftFakeShellScript(t, dir, "toolb", "version B2\n")
		driftSeedCache(t, "alpha", "version A1", nil)
		driftSeedCache(t, "zeta", "version B1", nil)
		tools := map[string]config.ToolConfig{
			"alpha": {Binary: binA, Allow: []config.AllowRule{{Subcommand: []string{"go"}}}},
			"zeta":  {Binary: binB, Allow: []config.AllowRule{{Subcommand: []string{"go"}}}},
		}
		in := driftInput(tools)
		first := runDriftToolVersionChanged(context.Background(), in)
		if len(first.Findings) != 2 {
			t.Fatalf("Findings = %d, want 2", len(first.Findings))
		}
		if first.Findings[0].Detail >= first.Findings[1].Detail && !strings.Contains(first.Findings[0].Detail, "alpha") {
			t.Errorf("expected alpha before zeta (sorted tool names): %+v", first.Findings)
		}
		for i := 0; i < 5; i++ {
			again := runDriftToolVersionChanged(context.Background(), in)
			if len(again.Findings) != len(first.Findings) {
				t.Fatalf("non-deterministic finding count: run 1 = %d, run %d = %d", len(first.Findings), i+2, len(again.Findings))
			}
		}
	})
}

// ---------------------------------------------------------------- drift.unverifiable

func TestRunDriftUnverifiable(t *testing.T) {
	t.Run("inert tool (no allow rules): no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		tools := map[string]config.ToolConfig{
			"git": {Binary: "/usr/bin/git"},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (tools.inert's own subject, nothing here to verify)", res.Outcome())
		}
	})

	t.Run("unknown tool: no knowledge-table entry AND no cached schema, both named", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "mytool", "irrelevant\n")
		tools := map[string]config.ToolConfig{
			"custom": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"run"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		f := res.Findings[0]
		if f.Severity != SeverityInfo {
			t.Errorf("Severity = %q, want info", f.Severity)
		}
		if !strings.Contains(f.Detail, "no per-tool knowledge-table entry") {
			t.Errorf("Detail %q missing the no-knowledge-table reason", f.Detail)
		}
		if !strings.Contains(f.Detail, "never been introspected") {
			t.Errorf("Detail %q missing the no-cached-schema reason", f.Detail)
		}
		if got, want := f.Path.String(), At("tools", "custom", "allow").String(); got != want {
			t.Errorf("Path = %q, want %q", got, want)
		}
	})

	t.Run("known tool, binary missing: catalog probe untrusted, plus no-cached-schema", func(t *testing.T) {
		driftIsolateCacheDir(t)
		// Named "git" (LookupToolKnowledge's key, via ToolBasename) but never
		// written to disk — a known tool whose CatalogArgv is known but whose
		// live probe cannot run at all.
		missing := filepath.Join(t.TempDir(), "git")
		tools := map[string]config.ToolConfig{
			"git": {Binary: missing, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		f := res.Findings[0]
		if !strings.Contains(f.Detail, "did not produce a listing that could be trusted as exhaustive") {
			t.Errorf("Detail %q missing the catalog-untrusted reason", f.Detail)
		}
		if !strings.Contains(f.Detail, "never been introspected") {
			t.Errorf("Detail %q missing the no-cached-schema reason", f.Detail)
		}
	})

	t.Run("fully verifiable rule: exhaustive catalog, parsed, full coverage, single segment — no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", "git version 2.44.0", map[string]introspect.Subcommand{
			"pull": {
				Flags:          map[string]introspect.Flag{"--depth": {Type: "integer", TakesValue: true}},
				ParseOutcome:   introspect.ParseOutcomeParsed,
				CoverageTokens: 1,
				CoverageFound:  1,
			},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass: %+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("multi-segment subcommand fires regardless of an otherwise-clean tool", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGitCatalog)
		driftSeedCache(t, "gcloud", "", map[string]introspect.Subcommand{
			"compute instances list": {
				ParseOutcome:   introspect.ParseOutcomeParsed,
				CoverageTokens: 1,
				CoverageFound:  1,
			},
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"compute", "instances", "list"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		f := res.Findings[0]
		if !strings.Contains(f.Detail, "more than one subcommand segment") {
			t.Errorf("Detail %q missing the multi-segment reason", f.Detail)
		}
		if strings.Contains(f.Detail, "subcommand catalog:") {
			t.Errorf("Detail %q should not carry a catalog-dimension reason — the catalog probe was exhaustive", f.Detail)
		}
	})

	t.Run("not-in-cache: allow rule absent from an otherwise-present cached schema", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", "", map[string]introspect.Subcommand{
			"status": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 1, CoverageFound: 1},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		if !strings.Contains(res.Findings[0].Detail, "missing from the last cached introspection") {
			t.Errorf("Detail %q missing the not-in-cache reason", res.Findings[0].Detail)
		}
	})

	t.Run("not-parsed: cached ParseOutcome is not ParseOutcomeParsed", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeGarbage},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		if !strings.Contains(res.Findings[0].Detail, "not confirmed as a genuine, parsed help page") {
			t.Errorf("Detail %q missing the not-parsed reason", res.Findings[0].Detail)
		}
	})

	t.Run("coverage unmeasurable: ParseOutcomeParsed but zero option-shaped tokens found in the help text", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 0, CoverageFound: 0},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		if !strings.Contains(res.Findings[0].Detail, "no flag-shaped tokens to measure the parse against") {
			t.Errorf("Detail %q missing the coverage-unmeasurable reason", res.Findings[0].Detail)
		}
	})

	t.Run("low coverage: the parse captured fewer tokens than the help text contained", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 5, CoverageFound: 2},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		if !strings.Contains(res.Findings[0].Detail, "did not fully account for") {
			t.Errorf("Detail %q missing the low-coverage reason", res.Findings[0].Detail)
		}
	})

	t.Run("a rule may be named under more than one reason bucket at once", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGitCatalog)
		// No cache seeded at all: the multi-segment rule is BOTH multi-segment
		// AND has no cached schema — two independently true facts about the
		// same rule, not a double-count of one problem.
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"compute", "instances", "list"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		d := res.Findings[0].Detail
		if !strings.Contains(d, "more than one subcommand segment") {
			t.Errorf("Detail %q missing the multi-segment reason", d)
		}
		if !strings.Contains(d, "never been introspected") {
			t.Errorf("Detail %q missing the no-cached-schema reason", d)
		}
	})

	t.Run("list is capped at driftMaxNamedItems with an 'and N more' remainder", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)

		var allow []config.AllowRule
		for i := 0; i < 7; i++ {
			allow = append(allow, config.AllowRule{Subcommand: []string{fmt.Sprintf("sub%d", i)}})
		}
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: allow},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		d := res.Findings[0].Detail
		if !strings.Contains(d, "and 2 more") {
			t.Errorf("Detail %q does not fold the remainder into \"and 2 more\": %s", d, d)
		}
		if !strings.Contains(d, "7 allow rule(s)") {
			t.Errorf("Detail %q does not report the full count (7)", d)
		}
	})

	t.Run("multiple tools: sorted, deterministic", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		binGit := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		binOther := driftFakeShellScript(t, dir, "othertool", "irrelevant\n")
		tools := map[string]config.ToolConfig{
			"git":   {Binary: binGit, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
			"other": {Binary: binOther, Allow: []config.AllowRule{{Subcommand: []string{"run"}}}},
		}
		in := driftInput(tools)
		first := runDriftUnverifiable(context.Background(), in)
		if len(first.Findings) != 2 {
			t.Fatalf("Findings = %d, want 2: %+v", len(first.Findings), first.Findings)
		}
		if first.Findings[0].Title == first.Findings[1].Title {
			t.Fatalf("expected two distinct tool findings, got identical titles: %+v", first.Findings)
		}
		for i := 0; i < 5; i++ {
			again := runDriftUnverifiable(context.Background(), in)
			if len(again.Findings) != len(first.Findings) ||
				again.Findings[0].Detail != first.Findings[0].Detail ||
				again.Findings[1].Detail != first.Findings[1].Detail {
				t.Fatalf("non-deterministic result: run 1 = %+v, run %d = %+v", first.Findings, i+2, again.Findings)
			}
		}
	})
}

// ---------------------------------------------------------------- well-formedness spot check

// TestDriftFindingsAreWellFormed is a focused version of
// invariants_test.go's package-wide TestFindingsAreWellFormed, scoped to a
// realistic multi-tool config that exercises every reason bucket at once —
// invariants_test.go's own fixtures use binaries that do not exist
// (/usr/bin/kubectl, /usr/bin/gcloud on most hosts), so they never reach the
// "exhaustive catalog, seeded cache" branches this test does.
func TestDriftFindingsAreWellFormed(t *testing.T) {
	driftIsolateCacheDir(t)
	dir := t.TempDir()
	bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
	driftSeedCache(t, "git", "git version 2.44.0", map[string]introspect.Subcommand{
		"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 3, CoverageFound: 1},
	})
	tools := map[string]config.ToolConfig{
		"git": {Binary: bin, Allow: []config.AllowRule{
			{Subcommand: []string{"pull"}},
			{Subcommand: []string{"push"}},
		}},
	}
	in := driftInput(tools)

	for _, run := range []RunFunc{runDriftToolVersionChanged, runDriftUnverifiable} {
		res := run(context.Background(), in)
		for _, f := range res.Findings {
			if !f.Severity.Valid() {
				t.Errorf("invalid severity %q", f.Severity)
			}
			if strings.TrimSpace(f.Title) == "" {
				t.Error("empty Title")
			}
			if strings.TrimSpace(f.Detail) == "" {
				t.Error("empty Detail")
			}
			if !f.Path.Empty() && f.Location != "" {
				t.Errorf("finding sets both Path and Location: %+v", f)
			}
		}
	}
}
