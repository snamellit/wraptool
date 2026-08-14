package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/introspect"
	"github.com/pti/wraptool/internal/paths"
)

// ---------------------------------------------------------------- fixtures/helpers

// driftIsolateCacheDir points XDG_CACHE_HOME at a fresh t.TempDir() for the
// duration of the test, so introspect.SaveCache — the only remaining WRITE
// path, which this package's drift checks never call themselves (design
// §5.2) but which driftSeedCache uses to seed fixtures — never touches this
// HOST's real introspection cache. Mirrors internal/introspect/cache_test.go's
// own isolateCacheDir; duplicated rather than exported, since introspect has
// no reason to grow a test-only export for one caller outside the package.
//
// The READ side no longer needs this isolation at all (design §7.6 RULING 1:
// drift.go now reads through introspect.LoadCacheStaleAt(in.Paths.CacheDir,
// ...), never introspect.LoadCacheStale) — driftInput sets Paths.CacheDir to
// paths.CacheDir() explicitly, which only agrees with where driftSeedCache
// wrote because THIS function has already redirected XDG_CACHE_HOME and
// reloaded by the time driftInput reads it. Still required before every
// driftSeedCache call for that reason.
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
// driftCatalogAxis to accept a probe as trustworthy (Exhaustive == true).
const driftGitCatalog = `usage: git <command> [<args>]

These are common Git commands used in various situations:

   pull       Fetch from and integrate with another repository
   push       Update remote refs along with associated objects
   status     Show the working tree status
`

// driftMakeEligible temporarily adds basename to driftEligibleCatalogTools
// for the duration of the test, restoring the map afterward. This is this
// file's ONLY way to exercise the ELIGIBLE path of runDriftUnknownSubcommand
// / driftCatalogAxis: design §7.6 F2 RULING 1's real allowlist is expected
// to be empty (no real tool has been vetted yet), so a test that wants to
// prove the ALLOWLIST-PRESENT mechanism itself works — as opposed to proving
// it stays silent, which the other subtests already do against the real,
// unmodified table — needs a synthetic, test-only entry. This is NOT a
// production claim about the named tool's real catalog completeness (that
// rule binds driftEligibleCatalogTools' PERMANENT contents, never a
// t.Cleanup-scoped test fixture that never leaves this file).
func driftMakeEligible(t *testing.T, basename string, entry driftCatalogEligible) {
	t.Helper()
	old, had := driftEligibleCatalogTools[basename]
	driftEligibleCatalogTools[basename] = entry
	t.Cleanup(func() {
		if had {
			driftEligibleCatalogTools[basename] = old
		} else {
			delete(driftEligibleCatalogTools, basename)
		}
	})
}

// driftGcloudLikeSentinel is a subcommand present in both driftGitCatalog and
// driftGcloudLikeCatalog (which reuses driftGitCatalog's text verbatim) —
// used as the Sentinel for driftMakeEligible's synthetic "gcloud" entries so
// a real, exhaustive probe of that fixture text satisfies design §7.6
// RULING 2 unless a subtest deliberately wants it not to.
const driftGcloudLikeSentinel = "status"

// driftInput builds a *Input suitable for driving runDriftToolVersionChanged/
// runDriftUnverifiable directly (plan §1 rule 8: tested by calling the
// RunFunc, never through Registered()). Env is a bare MapEnv — most cases in
// this file never populate it (driftCacheTrustReason's Env.Stat call only
// fires once a cache entry exists AND names the currently configured binary,
// see the "cache older than binary" case below) — set only so Input
// satisfies doctor.Run's nil-Env guard and so a case that DOES need Env can
// populate it via `in.Env.(*MapEnv)`.
//
// Paths.CacheDir is set to paths.CacheDir() — the value driftIsolateCacheDir
// has already redirected via XDG_CACHE_HOME by the time a test calls this —
// so introspect.LoadCacheStaleAt(in.Paths.CacheDir, ...), the ONLY cache read
// drift.go performs (design §7.6 RULING 1), resolves to the same isolated
// directory driftSeedCache wrote into, hermetically, with no reliance on
// LoadCacheStale's real-host default.
func driftInput(tools map[string]config.ToolConfig) *Input {
	return &Input{
		Env:    NewMapEnv(),
		GOOS:   "linux",
		Paths:  HostPaths{CacheDir: paths.CacheDir()},
		Config: &config.Config{Tools: tools},
		Probe:  true,
	}
}

// driftSeedCache is introspect.SaveCache with drift_test.go's own defaults
// filled in (IntrospectedAt, an empty Subcommands map when the caller has no
// use for one) so each test case only states what it cares about. binary is
// stamped as Schema.Binary — the field driftCacheTrustReason (design §7.6
// RULING 3) compares against the tool's CURRENTLY configured Binary, so a
// case exercising the "cache describes a different binary" reason passes a
// binary here that deliberately differs from config.ToolConfig.Binary.
func driftSeedCache(t *testing.T, name, binary, versionOutput string, subs map[string]introspect.Subcommand) {
	t.Helper()
	if subs == nil {
		subs = map[string]introspect.Subcommand{}
	}
	if err := introspect.SaveCache(name, &introspect.Schema{
		Binary:         binary,
		VersionOutput:  versionOutput,
		IntrospectedAt: time.Now(),
		Subcommands:    subs,
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- Meta

// TestDriftCheckMeta pins all three checks' ID/Needs against the SAME Meta
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
		{driftUnknownSubcommandMeta, driftUnknownSubcommand, NeedsConfig | NeedsProbe},
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
		driftSeedCache(t, "git", bin, "", nil)
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
		driftSeedCache(t, "git", bin, "git version 2.44.0", nil)
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
		driftSeedCache(t, "git", bin, "git version 2.44.0", nil)
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
		driftSeedCache(t, "git", missing, "git version 2.44.0", nil)
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
		driftSeedCache(t, "alpha", binA, "version A1", nil)
		driftSeedCache(t, "zeta", binB, "version B1", nil)
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
		if got, want := f.Path.String(), At("tools", "custom").String(); got != want {
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

	t.Run("full coverage, single segment, trusted cache: still unverifiable — no exhaustiveness assertion exists yet", func(t *testing.T) {
		// Design §7.6 RULING 4: coverage alone is not the whole flag-drift gate
		// — a per-tool "help is exhaustive" assertion is required too, and no
		// tool carries one yet, so even a rule this clean cannot support a
		// flag-level absence claim today.
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", bin, "git version 2.44.0", map[string]introspect.Subcommand{
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
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		if !strings.Contains(res.Findings[0].Detail, "no tool yet carries the design §7.6") {
			t.Errorf("Detail %q missing the no-exhaustiveness-assertion reason", res.Findings[0].Detail)
		}
		if strings.Contains(res.Findings[0].Detail, "subcommand catalog:") {
			t.Errorf("Detail %q should not carry a catalog-dimension reason — the catalog probe was exhaustive", res.Findings[0].Detail)
		}
	})

	t.Run("multi-segment subcommand fires regardless of an otherwise-clean tool", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGitCatalog)
		driftSeedCache(t, "gcloud", bin, "", map[string]introspect.Subcommand{
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
		driftSeedCache(t, "git", bin, "", map[string]introspect.Subcommand{
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
		driftSeedCache(t, "git", bin, "", map[string]introspect.Subcommand{
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
		driftSeedCache(t, "git", bin, "", map[string]introspect.Subcommand{
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
		driftSeedCache(t, "git", bin, "", map[string]introspect.Subcommand{
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

	t.Run("binary changed: cached schema names a different binary path — the exact gap a reviewer measured", func(t *testing.T) {
		// Design §7.6 RULING 3: a config whose binary points at a different
		// file reporting the same version must not read as verifiable just
		// because the cache "looks" complete — the cache describes a DIFFERENT
		// program.
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		otherBin := driftFakeShellScript(t, dir, "not-really-git", driftGitCatalog)
		driftSeedCache(t, "git", otherBin, "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 1, CoverageFound: 1},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		d := res.Findings[0].Detail
		if !strings.Contains(d, "different binary path") {
			t.Errorf("Detail %q missing the binary-changed reason", d)
		}
		// The stale per-subcommand fields (not-in-cache/not-parsed/coverage) must
		// NOT also fire once the cache itself is untrusted — one honest reason,
		// not a pile of derivative ones built on data that cannot be trusted.
		if strings.Contains(d, "missing from the last cached introspection") ||
			strings.Contains(d, "did not fully account for") {
			t.Errorf("Detail %q should not ALSO report per-subcommand reasons once the binary itself changed: %s", d, d)
		}
	})

	t.Run("cache older than binary: same path, rebuilt in place since the cache was written", func(t *testing.T) {
		// Design §7.6 RULING 3, second half: a binary can be upgraded in place
		// (same path), which a path-only comparison would miss entirely —
		// Env.Stat's mtime is the only remaining signal.
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", bin, "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 1, CoverageFound: 1},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		in := driftInput(tools)
		// The cache's IntrospectedAt is "now" (driftSeedCache); make the binary
		// appear rebuilt an hour after that.
		env := in.Env.(*MapEnv)
		env.Files[bin] = MapFile{ModTime: time.Now().Add(time.Hour)}
		res := runDriftUnverifiable(context.Background(), in)
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		if !strings.Contains(res.Findings[0].Detail, "older than the currently configured binary's own modification time") {
			t.Errorf("Detail %q missing the cache-older-than-binary reason", res.Findings[0].Detail)
		}
	})

	t.Run("cache newer than binary: no staleness reason", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", bin, "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 1, CoverageFound: 1},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		in := driftInput(tools)
		env := in.Env.(*MapEnv)
		env.Files[bin] = MapFile{ModTime: time.Now().Add(-time.Hour)}
		res := runDriftUnverifiable(context.Background(), in)
		// Full coverage, trusted cache: only driftReasonNoExhaustivenessAssertion
		// fires (ruling 4), never the staleness reason.
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		if strings.Contains(res.Findings[0].Detail, "older than the currently configured binary") {
			t.Errorf("Detail %q should not report staleness — the binary is OLDER than the cache: %s", res.Findings[0].Detail, res.Findings[0].Detail)
		}
	})

	t.Run("deny rules: multi-segment counted and labeled deny, distinct from allow", func(t *testing.T) {
		// Design §7.6 RULING 5: a tool with deny rules but no allow rules must
		// still be examined, and the text must say "deny", not "allow".
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGitCatalog)
		tools := map[string]config.ToolConfig{
			"gcloud": {
				Binary: bin,
				Deny:   []config.DenyRule{{Subcommand: []string{"compute", "instances", "delete"}}},
			},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		d := res.Findings[0].Detail
		if !strings.Contains(d, "1 deny rule(s) have more than one subcommand segment") {
			t.Errorf("Detail %q missing the deny-labeled multi-segment reason: %s", d, d)
		}
		if strings.Contains(d, "allow rule(s) have more than one") {
			t.Errorf("Detail %q wrongly labels the deny rule as allow: %s", d, d)
		}
		if !strings.Contains(d, "0 allow rule(s) and 1 deny rule(s)") {
			t.Errorf("Detail %q missing the allow/deny rule counts: %s", d, d)
		}
	})

	t.Run("deny rule with a single segment and no cache concerns: nothing to report", func(t *testing.T) {
		// A deny rule carries no flags (config.DenyRule has only Subcommand) and
		// is never introspected into the cache, so — unlike an allow rule — a
		// single-segment deny rule with an otherwise-trustworthy catalog has
		// NOTHING left to report: driftReasonNoCachedSchema and the flag-cache
		// buckets are allow-only by design.
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGitCatalog)
		tools := map[string]config.ToolConfig{
			"gcloud": {
				Binary: bin,
				Deny:   []config.DenyRule{{Subcommand: []string{"compute"}}},
			},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass: %+v", res.Outcome(), res.Findings)
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

	t.Run("catalog exhaustive but not on the allowlist: driftReasonCatalogNotVetted names it (design §7.6 F2 RULING 1)", func(t *testing.T) {
		// git is a REAL toolKnowledgeTable entry (driftCatalogExcludedTools,
		// not driftEligibleCatalogTools) — no driftMakeEligible here. This is
		// the exact fallthrough RULING 1 describes: "a tool absent from the
		// list emits no absence claim, ever; it falls through to
		// drift.unverifiable, with a reason saying its catalog has not been
		// vetted for completeness."
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", bin, "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 1, CoverageFound: 1},
		})
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		d := res.Findings[0].Detail
		if !strings.Contains(d, "had been vetted as listing every subcommand it dispatches") {
			t.Errorf("Detail %q missing the catalog-not-vetted reason: %s", d, d)
		}
		if !strings.Contains(d, `1 allow rule(s)`) {
			t.Errorf("Detail %q should count the affected rule: %s", d, d)
		}
		// The tool-WIDE "subcommand catalog:" sentence must NOT also fire —
		// git's parse itself is genuine (F1's bar); only the NEWER,
		// absence-claim-specific bar is unmet.
		if strings.Contains(d, "subcommand catalog:") {
			t.Errorf("Detail %q should not carry the tool-wide catalog-dimension sentence — the parse itself was genuine: %s", d, d)
		}
	})

	t.Run("catalog exhaustive, eligible, sentinel present: no driftReasonCatalogNotVetted", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		driftSeedCache(t, "gcloud", bin, "", map[string]introspect.Subcommand{
			"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 1, CoverageFound: 1},
		})
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}
		res := runDriftUnverifiable(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		d := res.Findings[0].Detail
		if strings.Contains(d, "could have supported a subcommand-absence claim") {
			t.Errorf("Detail %q should not carry the catalog-not-vetted reason once the tool is eligible: %s", d, d)
		}
	})
}

// ---------------------------------------------------------------- drift.unknown-subcommand

// driftGcloudLikeCatalog reuses driftGitCatalog's text (pull/push/status)
// under a "gcloud" binary name — ProbeCatalog does not care which tool ran,
// only what came back. gcloud is used throughout this file's ELIGIBLE-path
// subtests specifically because it is a REAL toolKnowledgeTable entry
// (driftCatalogExcludedTools, not driftEligibleCatalogTools — see that
// map's own doc comment), so driftMakeEligible's synthetic override is
// visibly test-only rather than accidentally shadowing a real claim about a
// tool this file also uses elsewhere.
const driftGcloudLikeCatalog = driftGitCatalog

func TestRunDriftUnknownSubcommand(t *testing.T) {
	t.Run("no allow rules (deny only): no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Deny: []config.DenyRule{{Subcommand: []string{"compute"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (no allow rules to check)", res.Outcome())
		}
	})

	t.Run("unknown tool (no knowledge-table entry): no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "mytool", driftGcloudLikeCatalog)
		tools := map[string]config.ToolConfig{
			"custom": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"run"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (gate 1: no catalog argv known for this tool)", res.Outcome())
		}
	})

	t.Run("git: real allowlist is empty, so even a genuine, exhaustively-probed typo produces nothing", func(t *testing.T) {
		// Design §7.6 F2 RULING 1's own point, against the REAL, unmodified
		// driftEligibleCatalogTools (no driftMakeEligible here): an otherwise
		// textbook-clean case (exhaustive probe, trusted cache, a subcommand
		// that plainly is not in the catalog) must still produce NOTHING for
		// git — git is not on the allowlist (driftCatalogExcludedTools names
		// why), so no absence claim may be built from its catalog at all.
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
		driftSeedCache(t, "git", bin, "", nil)
		tools := map[string]config.ToolConfig{
			"git": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"frobnicate"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (gate 3: git is not in driftEligibleCatalogTools): %+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("kubectl: real allowlist is empty too", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "kubectl", driftGcloudLikeCatalog)
		driftSeedCache(t, "kubectl", bin, "", nil)
		tools := map[string]config.ToolConfig{
			"kubectl": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"frobnicate"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (gate 3: kubectl is not in driftEligibleCatalogTools): %+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("gcloud (not plugin-risky): absent first segment fires a warn finding naming version and argv", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		driftSeedCache(t, "gcloud", bin, "", nil)
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"frobnicate"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if len(res.Findings) != 1 {
			t.Fatalf("Findings = %d, want 1: %+v", len(res.Findings), res.Findings)
		}
		f := res.Findings[0]
		if f.Severity != SeverityWarn {
			t.Errorf("Severity = %q, want warn", f.Severity)
		}
		if !strings.Contains(f.Detail, `"frobnicate"`) {
			t.Errorf("Detail %q missing the offending subcommand", f.Detail)
		}
		if !strings.Contains(f.Detail, "help") { // gcloud's CatalogArgv
			t.Errorf("Detail %q missing the catalog argv used", f.Detail)
		}
		if f.Remedy == "" {
			t.Error("empty Remedy")
		}
		if strings.Contains(strings.ToLower(f.Remedy), "delete this rule") {
			t.Errorf("Remedy %q must never say to delete the rule", f.Remedy)
		}
		// driftFakeShellScript answers ANY argv (including --version) with the
		// same fixture text, so the "version probed" clause is that text's
		// first line — proving the finding actually threads ProbeVersion's
		// result through, not that it reports "unknown" (see the dedicated
		// TestDriftUnknownSubcommandVersionDisplay for the empty-probe case).
		const wantVersion = "usage: git <command> [<args>]"
		if !strings.Contains(f.Detail, wantVersion) {
			t.Errorf("Detail %q does not name the version probed (%q)", f.Detail, wantVersion)
		}
		if !strings.Contains(f.Remedy, wantVersion) {
			t.Errorf("Remedy %q does not name the version probed (%q)", f.Remedy, wantVersion)
		}
		if got, want := f.Path.String(), At("tools", "gcloud", "allow").Item("subcommand", "frobnicate").String(); got != want {
			t.Errorf("Path = %q, want %q", got, want)
		}
		if f.Location != "" {
			t.Errorf("Location = %q, want empty — a check must set Path, never Location", f.Location)
		}
	})

	t.Run("gcloud: first segment present (even for a multi-segment rule): no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		driftSeedCache(t, "gcloud", bin, "", nil)
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			// "pull" is in driftGitCatalog/driftGcloudLikeCatalog; deeper segments
			// are never checked by this check (design §5.3: first-segment only —
			// drift.unverifiable's driftReasonMultiSegment covers the rest).
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"pull", "some", "deeper", "thing"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (first segment %q is in the catalog): %+v", res.Outcome(), "pull", res.Findings)
		}
	})

	t.Run("catalog probe not exhaustive (binary missing): no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		missing := filepath.Join(t.TempDir(), "gcloud")
		driftSeedCache(t, "gcloud", missing, "", nil)
		// Eligible, even — the probe fails before eligibility is ever consulted.
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: missing, Allow: []config.AllowRule{{Subcommand: []string{"frobnicate"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (gate 2: probe cannot run at all): %+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("no cached schema: no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"frobnicate"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (gate 4: never introspected): %+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("cache untrusted (binary changed): no finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		otherBin := driftFakeShellScript(t, dir, "not-really-gcloud", driftGcloudLikeCatalog)
		driftSeedCache(t, "gcloud", otherBin, "", nil)
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"frobnicate"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (gate 4: cached schema names a different binary): %+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("multiple offending rules: one finding per rule, sorted by tool, deterministic", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		driftSeedCache(t, "gcloud", bin, "", nil)
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{
				{Subcommand: []string{"pull"}},        // in the catalog: no finding
				{Subcommand: []string{"frobnicate"}},  // absent: finding
				{Subcommand: []string{"nonexistent"}}, // absent: finding
			}},
		}
		in := driftInput(tools)
		first := runDriftUnknownSubcommand(context.Background(), in)
		if len(first.Findings) != 2 {
			t.Fatalf("Findings = %d, want 2: %+v", len(first.Findings), first.Findings)
		}
		for i := 0; i < 5; i++ {
			again := runDriftUnknownSubcommand(context.Background(), in)
			if !reflect.DeepEqual(first, again) {
				t.Fatalf("non-deterministic result: run 1 = %+v, run %d = %+v", first.Findings, i+2, again.Findings)
			}
		}
	})

	t.Run("empty rule.Subcommand: skipped defensively, never a finding", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		driftSeedCache(t, "gcloud", bin, "", nil)
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        driftGcloudLikeSentinel,
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: nil}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass: %+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("eligible but sentinel missing from probe: falls through, no finding (RULING 2)", func(t *testing.T) {
		driftIsolateCacheDir(t)
		dir := t.TempDir()
		bin := driftFakeShellScript(t, dir, "gcloud", driftGcloudLikeCatalog)
		driftSeedCache(t, "gcloud", bin, "", nil)
		driftMakeEligible(t, "gcloud", driftCatalogEligible{
			VersionMeasured: "synthetic test fixture",
			Evidence:        "synthetic test fixture — see driftMakeEligible",
			Sentinel:        "this-subcommand-is-not-in-the-fixture-catalog",
		})
		tools := map[string]config.ToolConfig{
			"gcloud": {Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"frobnicate"}}}},
		}
		res := runDriftUnknownSubcommand(context.Background(), driftInput(tools))
		if res.Outcome() != OutcomePass {
			t.Fatalf("Outcome = %v, want pass (gate 3: probe missing the vetted sentinel): %+v", res.Outcome(), res.Findings)
		}
	})
}

// TestDriftUnknownSubcommandVersionDisplay covers both branches directly —
// design §7.6's "include the version probed, so a stale claim is
// self-dating" applies even when the live probe itself produced nothing.
func TestDriftUnknownSubcommandVersionDisplay(t *testing.T) {
	if got := driftUnknownSubcommandVersionDisplay(""); got != "unknown (a live --version probe produced no output)" {
		t.Errorf("empty probe: got %q", got)
	}
	if got := driftUnknownSubcommandVersionDisplay("git version 2.44.0"); got != "git version 2.44.0" {
		t.Errorf("non-empty probe: got %q, want passthrough", got)
	}
}

// TestDriftEligibleCatalogToolsHaveEvidence guards against a
// driftEligibleCatalogTools entry with an empty VersionMeasured, Evidence,
// or Sentinel sneaking in — the map exists to be read by a human (this
// file's own report, and any later maintainer) as much as by
// driftCatalogEligibilityReason. driftEligibleCatalogTools is expected to be
// empty today (design §7.6 F2 RULING 1), so this loop is a no-op now; it
// stays as a regression guard for whenever a real entry is added.
func TestDriftEligibleCatalogToolsHaveEvidence(t *testing.T) {
	for basename, entry := range driftEligibleCatalogTools {
		if strings.TrimSpace(entry.VersionMeasured) == "" {
			t.Errorf("driftEligibleCatalogTools[%q].VersionMeasured is empty", basename)
		}
		if strings.TrimSpace(entry.Evidence) == "" {
			t.Errorf("driftEligibleCatalogTools[%q].Evidence is empty", basename)
		}
		if strings.TrimSpace(entry.Sentinel) == "" {
			t.Errorf("driftEligibleCatalogTools[%q].Sentinel is empty", basename)
		}
	}
}

// TestDriftCatalogEligibilityIsExplicit is design §7.6 F2 RULING 3's
// structural guard: every toolKnowledgeTable entry with a non-empty
// CatalogArgv must be accounted for in EXACTLY ONE of
// driftEligibleCatalogTools (with evidence) or driftCatalogExcludedTools
// (with a reason) — so a later CatalogArgv addition can never silently
// grant drift.unknown-subcommand absence-claim authority by omission; the
// reviewed decision is visible in this test, not implied by what a map
// happens to leave out.
func TestDriftCatalogEligibilityIsExplicit(t *testing.T) {
	for basename, tk := range toolKnowledgeTable {
		if len(tk.CatalogArgv) == 0 {
			continue
		}
		_, eligible := driftEligibleCatalogTools[basename]
		reason, excluded := driftCatalogExcludedTools[basename]
		switch {
		case eligible && excluded:
			t.Errorf("%q is in BOTH driftEligibleCatalogTools and driftCatalogExcludedTools — pick one", basename)
		case !eligible && !excluded:
			t.Errorf("%q has a CatalogArgv but is in NEITHER driftEligibleCatalogTools nor "+
				"driftCatalogExcludedTools — add it to one, with evidence or a reason, so the decision is "+
				"reviewed rather than left to an omission", basename)
		case excluded && strings.TrimSpace(reason) == "":
			t.Errorf("driftCatalogExcludedTools[%q] has an empty reason", basename)
		}
	}

	// The reverse direction: every entry in either map must correspond to a
	// REAL toolKnowledgeTable entry with a CatalogArgv — a stale entry left
	// behind by a later toolKnowledgeTable edit is exactly the kind of
	// silent drift this test exists to catch.
	for basename := range driftEligibleCatalogTools {
		if tk, ok := LookupToolKnowledge(basename); !ok || len(tk.CatalogArgv) == 0 {
			t.Errorf("driftEligibleCatalogTools has a stale entry %q with no matching toolKnowledgeTable CatalogArgv", basename)
		}
	}
	for basename := range driftCatalogExcludedTools {
		if tk, ok := LookupToolKnowledge(basename); !ok || len(tk.CatalogArgv) == 0 {
			t.Errorf("driftCatalogExcludedTools has a stale entry %q with no matching toolKnowledgeTable CatalogArgv", basename)
		}
	}
}

// ---------------------------------------------------------------- reason-table completeness (RULING 6)

// TestDriftReasonTableIsComplete is the structural guard design §7.6 RULING 6
// asks for: every reason driftAllReasons() enumerates (i.e. every declared
// driftUnverifiableReason member, up to the driftReasonCount sentinel)
// appears EXACTLY ONCE in driftReasonOrder, and has a non-empty entry in
// driftReasonText. A reviewer produced a causeless finding — "tools.git has 1
// allow rule(s). ." — by deleting one entry from driftReasonOrder; this test
// fails the build the moment that happens again, for either table.
func TestDriftReasonTableIsComplete(t *testing.T) {
	all := driftAllReasons()

	seenInOrder := map[driftUnverifiableReason]int{}
	for _, r := range driftReasonOrder {
		seenInOrder[r]++
	}
	for _, r := range all {
		switch n := seenInOrder[r]; {
		case n == 0:
			t.Errorf("driftReasonOrder is missing reason %d — a rule flagged with it would never be rendered", r)
		case n > 1:
			t.Errorf("driftReasonOrder names reason %d %d times, want exactly once", r, n)
		}
	}
	if len(driftReasonOrder) != len(all) {
		t.Errorf("driftReasonOrder has %d entries, want %d (driftAllReasons()) — an entry not in the declared set snuck in, or a duplicate is padding the count", len(driftReasonOrder), len(all))
	}

	for _, r := range all {
		text, ok := driftReasonText[r]
		if !ok {
			t.Errorf("driftReasonText is missing an entry for reason %d", r)
			continue
		}
		if strings.TrimSpace(text) == "" {
			t.Errorf("driftReasonText[%d] is empty", r)
		}
	}
	if len(driftReasonText) != len(all) {
		t.Errorf("driftReasonText has %d entries, want %d (driftAllReasons()) — a stale entry for a removed reason, or an entry for an undeclared one", len(driftReasonText), len(all))
	}
}

// TestDriftBuildUnverifiableFindingRefusesACauselessResult is the runtime
// backstop TestDriftReasonTableIsComplete's structural guard exists to make
// unreachable: driftBuildUnverifiableFinding itself must refuse to emit a
// finding whose sentence list came out empty (design §7.6 RULING 6), rather
// than trusting the table is always complete. Simulated directly by handing
// it an item whose reason has no driftReasonText entry — the exact shape a
// reviewer produced by deleting one.
func TestDriftBuildUnverifiableFindingRefusesACauselessResult(t *testing.T) {
	const driftReasonUndeclared driftUnverifiableReason = 999 // not in driftReasonOrder or driftReasonText

	items := []driftItem{{reason: driftReasonUndeclared, kind: driftKindAllow, subject: `"git pull"`}}
	if _, ok := driftBuildUnverifiableFinding("git", 1, 0, "", items); ok {
		t.Error("driftBuildUnverifiableFinding should refuse to emit a finding for a reason with no rendering — items alone are not enough, a sentence must actually have been built")
	}

	// The catalog-dimension reason alone still produces a real, non-causeless
	// finding, proving the refusal is specific to an empty SENTENCE list, not
	// to "items was non-nil".
	f, ok := driftBuildUnverifiableFinding("git", 1, 0, "an unrelated but real reason", items)
	if !ok {
		t.Fatal("driftBuildUnverifiableFinding should still emit a finding when the catalog-dimension reason alone is non-empty")
	}
	if !strings.Contains(f.Detail, "an unrelated but real reason") {
		t.Errorf("Detail %q missing the catalog-dimension reason", f.Detail)
	}
}

// ---------------------------------------------------------------- well-formedness spot check

// TestDriftFindingsAreWellFormed is a focused version of
// invariants_test.go's package-wide TestFindingsAreWellFormed, scoped to a
// realistic multi-tool config that exercises every reason bucket at once —
// invariants_test.go's own fixtures use binaries that do not exist
// (/usr/bin/kubectl, /usr/bin/gcloud on most hosts), so they never reach the
// "exhaustive catalog, seeded cache" branches this test does. gcloud is
// included alongside git specifically so runDriftUnknownSubcommand is
// exercised too: git is not in driftEligibleCatalogTools (see
// driftCatalogExcludedTools' own doc comment), so a git-only fixture would
// leave this new check permanently silent here — gcloud is made eligible via
// driftMakeEligible (a synthetic, test-only override; see that helper's own
// doc comment) so a REAL finding from runDriftUnknownSubcommand is actually
// validated below, not just an empty pass.
func TestDriftFindingsAreWellFormed(t *testing.T) {
	driftIsolateCacheDir(t)
	dir := t.TempDir()
	bin := driftFakeShellScript(t, dir, "git", driftGitCatalog)
	driftSeedCache(t, "git", bin, "git version 2.44.0", map[string]introspect.Subcommand{
		"pull": {ParseOutcome: introspect.ParseOutcomeParsed, CoverageTokens: 3, CoverageFound: 1},
	})
	gcloudBin := driftFakeShellScript(t, dir, "gcloud", driftGitCatalog)
	driftSeedCache(t, "gcloud", gcloudBin, "", nil)
	driftMakeEligible(t, "gcloud", driftCatalogEligible{
		VersionMeasured: "synthetic test fixture",
		Evidence:        "synthetic test fixture — see driftMakeEligible",
		Sentinel:        driftGcloudLikeSentinel,
	})
	tools := map[string]config.ToolConfig{
		"git": {
			Binary: bin,
			Allow: []config.AllowRule{
				{Subcommand: []string{"pull"}},
				{Subcommand: []string{"push"}},
			},
			// Exercises the deny-rule bucket alongside allow (design §7.6 RULING 5):
			// a multi-segment deny rule.
			Deny: []config.DenyRule{{Subcommand: []string{"push", "--force"}}},
		},
		"gcloud": {
			Binary: gcloudBin,
			Allow:  []config.AllowRule{{Subcommand: []string{"frobnicate"}}},
		},
	}
	in := driftInput(tools)

	for _, run := range []RunFunc{runDriftToolVersionChanged, runDriftUnverifiable, runDriftUnknownSubcommand} {
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
			if (f.Severity == SeverityFail || f.Severity == SeverityWarn) && f.Remedy == "" {
				t.Errorf("severity %s requires a Remedy: %+v", f.Severity, f)
			}
			if !f.Path.Empty() && f.Location != "" {
				t.Errorf("finding sets both Path and Location: %+v", f)
			}
		}
	}
}

// TestDriftSentinelMismatchIsItsOwnReason pins RULING 2's requirement that a
// sentinel mismatch is reported AS that reason. The two facts are different
// for the operator: "never vetted" is a standing limitation of wraptool, while
// a missing sentinel says this tool's catalog output changed shape or was
// truncated on THIS run, which is worth investigating. A single "A or B"
// sentence would hide the second behind the first.
func TestDriftSentinelMismatchIsItsOwnReason(t *testing.T) {
	const basename = "driftsentineltool"
	driftMakeEligible(t, basename, driftCatalogEligible{
		VersionMeasured: "test",
		Evidence:        "synthetic entry for this test only",
		Sentinel:        "init",
	})

	withSentinel := introspect.CatalogProbe{
		Catalog:    map[string]string{"init": "start", "plan": "plan"},
		Exhaustive: true,
	}
	if _, failed := driftCatalogEligibilityFailure(basename, withSentinel); failed {
		t.Errorf("a catalog containing the sentinel must be eligible")
	}

	truncated := introspect.CatalogProbe{
		Catalog:    map[string]string{"plan": "plan"},
		Exhaustive: true,
	}
	reason, failed := driftCatalogEligibilityFailure(basename, truncated)
	if !failed {
		t.Fatal("a catalog missing the sentinel must not be eligible")
	}
	if reason != driftReasonCatalogSentinelMissing {
		t.Errorf("reason = %v, want driftReasonCatalogSentinelMissing", reason)
	}

	unvetted, failed := driftCatalogEligibilityFailure("nevervettedtool", withSentinel)
	if !failed || unvetted != driftReasonCatalogNotVetted {
		t.Errorf("an unvetted tool must report driftReasonCatalogNotVetted, got %v (failed=%v)", unvetted, failed)
	}
}
