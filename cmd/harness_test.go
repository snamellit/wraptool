package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
	"github.com/pti/wraptool/internal/yamledit"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// fixedTestNow is this file's fixed clock, mirroring
// internal/pool/orchestrator_test.go's fixedNow so ages/quarantine windows
// are reproducible across test runs.
var fixedTestNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// fakeHarnessSource is a pool.Source test double for cmd-level tests — the
// cmd-package analogue of internal/pool/orchestrator_test.go's fakeSource,
// duplicated here (rather than exported from internal/pool) because a
// production Source implementation should never be reachable from cmd.
type fakeHarnessSource struct {
	onCall       func(step string)
	candidates   map[string][]pool.Candidate
	failPkgs     map[string]bool
	resolvedPkgs []string
}

func (f *fakeHarnessSource) Refresh(_ context.Context, pkg string) ([]pool.Candidate, error) {
	if f.onCall != nil {
		f.onCall("refresh:" + pkg)
	}
	if f.failPkgs[pkg] {
		return nil, fmt.Errorf("fake refresh failure for %s", pkg)
	}
	return f.candidates[pkg], nil
}

func (f *fakeHarnessSource) Resolve(_ context.Context, _ backend.Backend, pkg, version, _, _, recipeRevision string) (pool.LockHarness, error) {
	f.resolvedPkgs = append(f.resolvedPkgs, pkg)
	if f.onCall != nil {
		f.onCall("resolve:" + pkg)
	}
	if f.failPkgs[pkg] {
		return pool.LockHarness{}, fmt.Errorf("fake resolve failure for %s", pkg)
	}
	return pool.LockHarness{
		Source: "npm", Package: pkg, Version: version,
		PublishedAt: pool.FormatTimestamp(fixedTestNow), RecipeRevision: recipeRevision,
	}, nil
}

func (f *fakeHarnessSource) Fetch(_ context.Context, h *pool.LockHarness, _ string) error {
	if f.onCall != nil {
		f.onCall("fetch:" + h.Package)
	}
	h.NPMLock = &pool.NPMLock{LockfileVersion: 3, Packages: map[string]pool.NPMLockPackage{
		h.Package: {Version: h.Version, Resolved: "https://registry.npmjs.org/x.tgz", Integrity: "sha512-fake", SHA256: "deadbeef"},
	}}
	return nil
}

// fakeHarnessBackend is a no-op backend.Backend: cmd-level tests never
// actually run a container (fakeHarnessSource.Resolve ignores its backend
// argument entirely), they only need a value satisfying the interface to
// hand to deps.resolveBackend.
type fakeHarnessBackend struct{}

func (fakeHarnessBackend) Name() string                               { return "fake" }
func (fakeHarnessBackend) Available() bool                            { return true }
func (fakeHarnessBackend) Run(context.Context, backend.RunSpec) error { return nil }

// --- test helpers ---

// testHarnessPaths builds a pool.PoolPaths rooted at a fresh t.TempDir(),
// with Cache already shaped like pool.CacheSHA256Dir() (cache/sha256, not
// cache/ — see harnessDeps' poolPaths doc comment).
func testHarnessPaths(t *testing.T) pool.PoolPaths {
	t.Helper()
	dir := t.TempDir()
	return pool.PoolPaths{
		Manifest:     filepath.Join(dir, "manifest.yaml"),
		Lock:         filepath.Join(dir, "harness-lock.json"),
		Observations: filepath.Join(dir, "observations.json"),
		Audit:        filepath.Join(dir, "audit.jsonl"),
		AdvisoryLock: filepath.Join(dir, "lock"),
		Cache:        filepath.Join(dir, "cache", "sha256"), // mirrors pool.CacheSHA256Dir()
		Staging:      filepath.Join(dir, "staging"),
	}
}

// testHarnessDeps builds a harnessDeps for tests: poolPaths returns paths,
// poolBinDir points at a fresh subdirectory of binDir, newSource returns
// src, and resolveBackend always succeeds with a no-op fake backend. This
// is constructor injection, not global mutation — a test builds its own
// harnessDeps value and passes it directly to newHarnessCmdWithDeps or a
// helper function, so concurrent tests (t.Parallel()) using their own
// testHarnessDeps never contend with each other. It does NOT cover hasGuix
// or runInstallArgv, which stay package vars (out of the MR59 review's
// scope — see runInstallArgv's doc comment) — a test that reaches the
// legacy npm-install recipe still needs swapHasGuix/swapRunInstallArgv
// alongside this.
func testHarnessDeps(paths pool.PoolPaths, binDir string, src pool.Source) harnessDeps {
	return harnessDeps{
		poolPaths:      func() pool.PoolPaths { return paths },
		poolBinDir:     func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:      func() (pool.Source, error) { return src, nil },
		resolveBackend: func() (backend.Backend, error) { return fakeHarnessBackend{}, nil },
	}
}

func swapHasGuix(t *testing.T, v bool) {
	t.Helper()
	orig := hasGuix
	hasGuix = func() bool { return v }
	t.Cleanup(func() { hasGuix = orig })
}

func swapRunInstallArgv(t *testing.T, fn func([]string) error) {
	t.Helper()
	orig := runInstallArgv
	runInstallArgv = fn
	t.Cleanup(func() { runInstallArgv = orig })
}

// runHarnessCmd executes `harness <args...>` through the FULL root command
// (production harnessDeps, via root.go -> newHarnessCmd -> defaultHarnessDeps).
// Only for tests that must fail on argument validation alone, before ever
// touching a dependency — mirrors runValidate's pattern in
// cmd/validate_test.go.
func runHarnessCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"harness"}, args...))
	err := root.Execute()
	return buf.String(), err
}

// runHarnessCmdWithDeps executes the harness command tree built with deps
// directly (bypassing root.go, which always uses defaultHarnessDeps()),
// returning combined stdout+stderr and the command error. This is how tests
// exercise fake dependencies: constructor injection, not global var
// mutation.
func runHarnessCmdWithDeps(t *testing.T, deps harnessDeps, args ...string) (string, error) {
	t.Helper()
	cmd := newHarnessCmdWithDeps(deps)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// bareHarnessCmd is a minimal *cobra.Command for tests that call a helper
// function (runHarnessInstall, maybeInstallLocked, ...) directly rather than
// through cobra's own arg parsing — OutOrStdout()/ErrOrStderr() fall back to
// os.Stdout/os.Stderr when no writer has been set.
func bareHarnessCmd() *cobra.Command { return &cobra.Command{} }

// --- MAJOR: harnessDeps is production-correct ---

// TestDefaultHarnessDepsPoolPathsCacheMatchesSourceFetchContract pins
// defaultHarnessDeps()'s real (production) poolPaths wiring against
// Source.Fetch's documented contract (internal/pool.PoolPaths.Cache's doc
// comment): Cache MUST be pool.CacheSHA256Dir(), never pool.CacheDir() one
// level up. A regression here produces no build or lint error and no
// runtime error either — every fetched object just becomes a silent,
// permanent cache miss (harness-cli review MAJOR-1). Pure path-string
// comparison; touches no files.
func TestDefaultHarnessDepsPoolPathsCacheMatchesSourceFetchContract(t *testing.T) {
	t.Parallel()
	got := defaultHarnessDeps().poolPaths().Cache
	want := pool.CacheSHA256Dir()
	if got != want {
		t.Errorf("defaultHarnessDeps().poolPaths().Cache = %q, want pool.CacheSHA256Dir() = %q", got, want)
	}
}

// TestHarnessCommandArgValidation table-tests every subcommand's argument
// validation. Every case here must fail before touching any dependency (no
// pool paths, no Source, no backend) — each one returns from its RunE before
// reaching deps.newSource()/deps.resolveBackend()/setHarnessConstraint, so
// none of them can write into the real developer pool ($XDG_DATA_HOME); safe
// to run against the production root command, and safe to parallelize.
func TestHarnessCommandArgValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"install: unknown harness", []string{"install", "bogus"}, "unknown harness"},
		{"install: host IDE harness is not a container CLI", []string{"install", "cursor"}, "host-side IDE"},
		{"install: --allow-young without --reason", []string{"install", "claude", "--allow-young"}, "--reason"},
		{"refresh: unknown harness", []string{"refresh", "bogus"}, "unknown harness"},
		{"refresh: legacy (non-lockable) harness", []string{"refresh", "agy"}, "not pool-lockable"},
		{"update: unknown harness", []string{"update", "bogus"}, "unknown harness"},
		{"update: legacy (non-lockable) harness", []string{"update", "gemini"}, "not pool-lockable"},
		{"update: --allow-young without --reason", []string{"update", "claude", "--allow-young"}, "--reason"},
		{"sync: unknown harness", []string{"sync", "bogus"}, "unknown harness"},
		{"sync: legacy (non-lockable) harness", []string{"sync", "roo"}, "not pool-lockable"},
		{"policy set: no flags given", []string{"policy", "set"}, "specify at least one of"},
		{"policy set: unknown --harness", []string{"policy", "set", "--harness", "bogus", "--minimum-release-age", "1h"}, "unknown harness"},
		{"policy set: negative --minimum-release-age", []string{"policy", "set", "--minimum-release-age", "-1h"}, "must not be negative"},
		{"policy set: negative --minimum-observation-age", []string{"policy", "set", "--minimum-observation-age", "-1h"}, "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := runHarnessCmd(t, tc.args...)
			if err == nil {
				t.Fatalf("expected an error, got none; output:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestHarnessArgHelpersRejectMultipleArgs pins the helpers' OWN >1-arg guard,
// independent of cobra's MaximumNArgs(1) at the command layer: without it,
// two args would fall through to the bare-command branch and silently widen
// the target set to every manifest/lock-listed harness (MR !59 review,
// notes 1905161/1905162).
func TestHarnessArgHelpersRejectMultipleArgs(t *testing.T) {
	t.Parallel()
	paths := pool.PoolPaths{} // never touched: the guard fires before any filesystem access
	for _, fn := range []struct {
		name string
		call func() ([]string, error)
	}{
		{"manifestLockableHarnessArgs", func() ([]string, error) {
			return manifestLockableHarnessArgs(paths, []string{"claude", "opencode"})
		}},
		{"lockHarnessArgs", func() ([]string, error) {
			return lockHarnessArgs(paths, []string{"claude", "opencode"})
		}},
	} {
		names, err := fn.call()
		if err == nil {
			t.Errorf("%s: expected an error for two args, got names %v", fn.name, names)
		} else if !strings.Contains(err.Error(), "at most one harness") {
			t.Errorf("%s: error = %q, want it to name the at-most-one contract", fn.name, err)
		}
	}
}

// TestHarnessInstallRequiresExactlyOneArg and TestHarnessUpdateAcceptsAtMostOneArg
// pin cobra's own arity enforcement (ExactArgs(1) for install,
// MaximumNArgs(1) for the optional-harness commands) so a future edit can't
// accidentally loosen it. Cobra rejects the arg count before RunE runs, so
// these never touch a dependency either.
func TestHarnessInstallRequiresExactlyOneArg(t *testing.T) {
	t.Parallel()
	if _, err := runHarnessCmd(t, "install"); err == nil {
		t.Error("expected an error for `install` with no harness name")
	}
	if _, err := runHarnessCmd(t, "install", "claude", "extra"); err == nil {
		t.Error("expected an error for `install` with more than one argument")
	}
}

func TestHarnessUpdateAcceptsAtMostOneArg(t *testing.T) {
	t.Parallel()
	if _, err := runHarnessCmd(t, "update", "claude", "extra"); err == nil {
		t.Error("expected an error for `update` with more than one argument")
	}
}

// --- list rendering (§10 column shapes) ---

func TestRunHarnessListRendersColumnsAndQuarantineCountdown(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	pkg := "@anthropic-ai/claude-code"

	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\npolicy:\n  minimum_release_age: 168h\nharnesses:\n  claude:\n    constraint: \">=2 <3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	obs := pool.Observations{}
	if err := obs.Observe(pool.ObservationKey("npm", pkg, "2.1.220"),
		pool.FormatTimestamp(now.Add(-200*time.Hour)), pool.FormatTimestamp(now.Add(-200*time.Hour)), false, false); err != nil {
		t.Fatal(err)
	}
	// Published exactly 165h ago: 168h - 165h = 3h remaining... use days instead.
	quarantinedPublished := now.Add(-(168*time.Hour - 3*24*time.Hour))
	if err := obs.Observe(pool.ObservationKey("npm", pkg, "2.1.224"),
		pool.FormatTimestamp(quarantinedPublished), pool.FormatTimestamp(quarantinedPublished), false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}

	lock := &pool.Lock{
		SchemaVersion: pool.LockSchemaVersion, GeneratedAt: pool.FormatTimestamp(now), PolicyHash: "sha256:x",
		Harnesses: map[string]pool.LockHarness{
			"claude": {
				Source: "npm", Package: pkg, Version: "2.1.220",
				PublishedAt: pool.FormatTimestamp(now.Add(-200 * time.Hour)),
				FirstSeenAt: pool.FormatTimestamp(now.Add(-200 * time.Hour)),
			},
		},
	}
	data, err := lock.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		t.Fatal(err)
	}

	poolBin := t.TempDir() // no binaries present -> ACTIVE = "no" for CLI harnesses

	var buf bytes.Buffer
	if err := runHarnessList(paths, poolBin, now, &buf); err != nil {
		t.Fatalf("runHarnessList: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "HARNESS") || !strings.Contains(out, "CONSTRAINT") || !strings.Contains(out, "QUARANTINED") {
		t.Fatalf("missing expected header columns, got:\n%s", out)
	}
	if !strings.Contains(out, "2.1.224 (3d left)") {
		t.Errorf("expected the quarantined-with-remaining cell '2.1.224 (3d left)', got:\n%s", out)
	}
	if !strings.Contains(out, ">=2 <3") {
		t.Errorf("expected the claude row's constraint, got:\n%s", out)
	}
	if !strings.Contains(out, "legacy (not lockable)") {
		t.Errorf("expected a legacy-harness row (e.g. agy/gemini/cursor/roo), got:\n%s", out)
	}
}

// --- editManifest / setHarnessConstraint ---

func TestEditManifestPreservesComments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	original := "# developer intent — do not remove this comment\n" +
		"schema_version: 1\n" +
		"policy:\n" +
		"  minimum_release_age: 168h # seven days\n" +
		"harnesses:\n" +
		"  claude:\n" +
		"    constraint: \">=2 <3\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	err := editManifest(path, func(doc *yaml.Node) error { return nil })
	if err != nil {
		t.Fatalf("editManifest (no-op mutate): %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "developer intent — do not remove this comment") {
		t.Errorf("top-level comment was dropped; got:\n%s", got)
	}
	if !strings.Contains(string(got), "seven days") {
		t.Errorf("inline comment was dropped; got:\n%s", got)
	}
}

func TestSetHarnessConstraintCreatesAndUpdates(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)

	if err := setHarnessConstraint(paths, "claude", ">=2 <3"); err != nil {
		t.Fatalf("first setHarnessConstraint: %v", err)
	}
	m, err := pool.LoadManifest(paths.Manifest)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got := m.Harnesses["claude"].Constraint; got != ">=2 <3" {
		t.Errorf("Constraint = %q, want >=2 <3", got)
	}

	// An empty constraint on a later install must not clear the existing one.
	if err := setHarnessConstraint(paths, "claude", ""); err != nil {
		t.Fatalf("second setHarnessConstraint (empty): %v", err)
	}
	m2, err := pool.LoadManifest(paths.Manifest)
	if err != nil {
		t.Fatalf("LoadManifest after empty constraint: %v", err)
	}
	if got := m2.Harnesses["claude"].Constraint; got != ">=2 <3" {
		t.Errorf("Constraint after empty-constraint install = %q, want unchanged >=2 <3", got)
	}
}

// --- policy set round-trip ---

func TestHarnessPolicySetRoundTripsCommentedManifest(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	original := "# do not remove\nschema_version: 1\npolicy:\n  minimum_release_age: 168h\n"
	if err := os.WriteFile(paths.Manifest, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	lock := pool.NewPoolLock(paths.AdvisoryLock)
	ctx := t.Context()
	if err := lock.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	err := editManifest(paths.Manifest, func(doc *yaml.Node) error {
		target := yamledit.EnsureMapChild(doc, "policy")
		yamledit.MapSet(target, "minimum_observation_age", yamledit.ScalarNode((24 * time.Hour).String()))
		return nil
	})
	lock.Release()
	if err != nil {
		t.Fatalf("editManifest: %v", err)
	}

	got, err := os.ReadFile(paths.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "do not remove") {
		t.Errorf("comment was dropped; got:\n%s", got)
	}
	m, err := pool.LoadManifest(paths.Manifest)
	if err != nil {
		t.Fatalf("LoadManifest after policy set: %v", err)
	}
	if m.Policy.MinimumObservationAge == nil || *m.Policy.MinimumObservationAge != 24*time.Hour {
		t.Errorf("MinimumObservationAge = %v, want 24h", m.Policy.MinimumObservationAge)
	}
	if m.Policy.MinimumReleaseAge == nil || *m.Policy.MinimumReleaseAge != 168*time.Hour {
		t.Errorf("MinimumReleaseAge = %v, want the original 168h preserved", m.Policy.MinimumReleaseAge)
	}
}

// --- pinInstallVersion ---

func TestPinInstallVersion(t *testing.T) {
	t.Parallel()
	argv := []string{"guix", "shell", "--container", "--network", "--emulate-fhs",
		"--share={prefix}={prefix}", "node", "--", "npm", "install", "-g", "@anthropic-ai/claude-code", "--prefix", "{prefix}"}
	pinned, err := pinInstallVersion(argv, "@anthropic-ai/claude-code", "2.1.220")
	if err != nil {
		t.Fatalf("pinInstallVersion: %v", err)
	}
	found := false
	for _, a := range pinned {
		if a == "@anthropic-ai/claude-code@2.1.220" {
			found = true
		}
		if a == "@anthropic-ai/claude-code" {
			t.Errorf("floating package argument %q survived pinning", a)
		}
	}
	if !found {
		t.Errorf("pinned argv %v does not contain the pinned package@version", pinned)
	}
	// The original argv must not be mutated (pinInstallVersion copies).
	stillFloating := false
	for _, a := range argv {
		if a == "@anthropic-ai/claude-code" {
			stillFloating = true
		}
	}
	if !stillFloating {
		t.Errorf("pinInstallVersion mutated its input argv in place: %v", argv)
	}
}

func TestPinInstallVersionErrorsWhenPackageNotFound(t *testing.T) {
	t.Parallel()
	argv := []string{"guix", "shell", "node", "--", "npm", "install", "-g", "some-other-pkg"}
	if _, err := pinInstallVersion(argv, "@anthropic-ai/claude-code", "2.1.220"); err == nil {
		t.Fatal("expected an error when the package is not present in argv")
	}
}

// --- MAJOR 2: bare refresh/update/sync iterate manifest/lock-listed
// harnesses only, and per-harness failures are collected, not fatal ---

// TestHarnessUpdateBareOnlyTouchesManifestListedHarnesses and its sibling
// below still use swapHasGuix/swapRunInstallArgv (both package vars, out of
// the harnessDeps constructor-injection scope — see runInstallArgv's doc
// comment), so they are NOT marked t.Parallel().
func TestHarnessUpdateBareOnlyTouchesManifestListedHarnesses(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	// Only claude has recorded pool intent (as if `install claude` ran);
	// opencode and pi are also pool-lockable but were never installed.
	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  claude: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudePkg := harness.Registry["claude"].PoolPackage
	obs := pool.Observations{}
	if err := obs.Observe(pool.ObservationKey("npm", claudePkg, "2.1.220"),
		pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}

	src := &fakeHarnessSource{}
	deps := testHarnessDeps(paths, t.TempDir(), src)

	out, err := runHarnessCmdWithDeps(t, deps, "update")
	if err != nil {
		t.Fatalf("bare update: %v\noutput:\n%s", err, out)
	}
	if len(src.resolvedPkgs) != 1 || src.resolvedPkgs[0] != claudePkg {
		t.Errorf("resolvedPkgs = %v, want exactly [%s] — bare update must touch only manifest-listed harnesses", src.resolvedPkgs, claudePkg)
	}
}

func TestHarnessUpdatePerHarnessFailureContinuesToNext(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  claude: {}\n  opencode: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudePkg := harness.Registry["claude"].PoolPackage
	opencodePkg := harness.Registry["opencode"].PoolPackage
	obs := pool.Observations{}
	for _, pkg := range []string{claudePkg, opencodePkg} {
		if err := obs.Observe(pool.ObservationKey("npm", pkg, "1.0.0"),
			pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}

	src := &fakeHarnessSource{failPkgs: map[string]bool{claudePkg: true}}
	deps := testHarnessDeps(paths, t.TempDir(), src)

	out, err := runHarnessCmdWithDeps(t, deps, "update")
	if err == nil {
		t.Fatal("expected a non-nil error since claude's update failed")
	}
	// The failed harness's name is deliberately NOT in Error() (that's the
	// one-line summary cobra prints; the detail already went to stderr —
	// MR !59 note 1905174). It must still be reachable through the wrapped
	// join for programmatic callers.
	var agg *perHarnessFailure
	if !errors.As(err, &agg) {
		t.Fatalf("expected a *perHarnessFailure aggregate, got %T: %v", err, err)
	}
	if !strings.Contains(agg.joined.Error(), "claude") {
		t.Errorf("joined chain should name the failed harness claude, got: %v", agg.joined)
	}
	if !strings.Contains(err.Error(), "1 of 2 harnesses failed") {
		t.Errorf("summary should count failures, got: %v", err)
	}
	found := false
	for _, p := range src.resolvedPkgs {
		if p == opencodePkg {
			found = true
		}
	}
	if !found {
		t.Errorf("opencode was never resolved — a failing claude must not block the rest; output:\n%s", out)
	}
}

// --- MINOR 1 / MINOR 2: runPerHarness stderr routing and errors.Join ---

// TestRunPerHarnessJoinsErrorsAndWritesFailedLinesToStderr covers both
// runPerHarness minors directly, without going through a full harness
// command: FAILED lines land on cmd.ErrOrStderr(), a fn's own success line
// (simulating what refresh/update/sync's fn closures print) lands on
// cmd.OutOrStdout() and stays there, and the returned error is an
// errors.Join that errors.Is can still see through to the original
// sentinel cause — not a stringified summary that loses the chain. This
// test has no global state at all, so it's parallel-safe.
func TestRunPerHarnessJoinsErrorsAndWritesFailedLinesToStderr(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	cmd := &cobra.Command{}
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)

	err := runPerHarness(cmd, []string{"a", "b"}, func(name string) error {
		if name == "a" {
			return fmt.Errorf("wrapping: %w", sentinel)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: ok\n", name)
		return nil
	})

	if err == nil {
		t.Fatal("expected a non-nil error since harness 'a' failed")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is did not find the sentinel through the join: %v", err)
	}
	// The detail was already streamed to stderr; the returned error is the
	// one cobra prints, so it must be the one-line summary, not a repeat of
	// every joined message (MR !59 note 1905174: no double error reporting).
	if got := err.Error(); strings.Contains(got, "boom") || !strings.Contains(got, "1 of 2 harnesses failed") {
		t.Errorf("returned error should be the short summary, got %q", got)
	}
	if !strings.Contains(errBuf.String(), "a: FAILED:") {
		t.Errorf("expected a FAILED line on stderr, got stderr=%q", errBuf.String())
	}
	if strings.Contains(outBuf.String(), "FAILED") {
		t.Errorf("FAILED line leaked onto stdout: %q", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "b: ok") {
		t.Errorf("expected harness b's success line on stdout, got stdout=%q", outBuf.String())
	}
}

// TestRunPerHarnessAllSucceedReturnsNilNotEmptyJoin: errors.Join of zero
// errors must still be a plain nil error (not a non-nil error wrapping
// nothing), so a fully successful run reports success normally.
func TestRunPerHarnessAllSucceedReturnsNilNotEmptyJoin(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := runPerHarness(cmd, []string{"a", "b"}, func(string) error { return nil })
	if err != nil {
		t.Errorf("runPerHarness with no failures = %v, want nil", err)
	}
}

// TestRunHarnessInstallProbesBeforeAnyManifestMutation pins the ordering
// documented on runHarnessInstall ("Source/backend availability is probed
// BEFORE the lock is taken ... so an off-guix host or an unwired Source
// fails with zero side effects"): a failing deps.newSource or
// deps.resolveBackend must error out before setHarnessConstraintLocked ever
// touches manifest.yaml, and before anything creates observations.json.
// poolBinDir asserts it is never even called, since both probes happen
// before maybeInstallLocked's poolBinDir lookup too. Global-state-free
// (harnessDeps only, no package-var swaps), so parallel-safe.
func TestRunHarnessInstallProbesBeforeAnyManifestMutation(t *testing.T) {
	t.Parallel()

	t.Run("newSource fails, manifest already exists", func(t *testing.T) {
		t.Parallel()
		paths := testHarnessPaths(t)
		original := "schema_version: 1\nharnesses:\n  claude:\n    constraint: \">=1 <2\"\n"
		if err := os.WriteFile(paths.Manifest, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		deps := harnessDeps{
			poolPaths: func() pool.PoolPaths { return paths },
			poolBinDir: func() (string, string, error) {
				t.Fatal("poolBinDir must not be called before the newSource/resolveBackend probe")
				return "", "", nil
			},
			newSource: func() (pool.Source, error) { return nil, errors.New("source unavailable") },
			resolveBackend: func() (backend.Backend, error) {
				t.Fatal("resolveBackend must not be called once newSource fails")
				return nil, nil
			},
		}

		// A non-empty, DIFFERENT constraint than what's already on disk: if
		// setHarnessConstraintLocked ran despite the probe failing, the
		// constraint field would visibly change from ">=1 <2" to ">=2 <3",
		// so the byte-comparison below is a reliable detector rather than
		// one that could pass by coincidence on a no-op rewrite.
		if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", harness.Registry["claude"], ">=2 <3", false, ""); err == nil {
			t.Fatal("expected an error from a failing newSource probe")
		}

		got, err := os.ReadFile(paths.Manifest)
		if err != nil {
			t.Fatalf("manifest should still exist unmodified: %v", err)
		}
		if string(got) != original {
			t.Errorf("manifest.yaml was mutated despite the newSource probe failing;\nwant:\n%s\ngot:\n%s", original, got)
		}
		if _, err := os.Stat(paths.Observations); !os.IsNotExist(err) {
			t.Errorf("observations.json should not exist after a probe failure, stat err = %v", err)
		}
	})

	t.Run("resolveBackend fails, no manifest yet", func(t *testing.T) {
		t.Parallel()
		paths := testHarnessPaths(t)
		deps := harnessDeps{
			poolPaths: func() pool.PoolPaths { return paths },
			poolBinDir: func() (string, string, error) {
				t.Fatal("poolBinDir must not be called before the newSource/resolveBackend probe")
				return "", "", nil
			},
			newSource:      func() (pool.Source, error) { return &fakeHarnessSource{}, nil },
			resolveBackend: func() (backend.Backend, error) { return nil, errors.New("guix not on PATH") },
		}

		if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", harness.Registry["claude"], "", false, ""); err == nil {
			t.Fatal("expected an error from a failing resolveBackend probe")
		}

		if _, err := os.Stat(paths.Manifest); !os.IsNotExist(err) {
			t.Errorf("manifest.yaml should not have been created after a probe failure, stat err = %v", err)
		}
		if _, err := os.Stat(paths.Observations); !os.IsNotExist(err) {
			t.Errorf("observations.json should not exist after a probe failure, stat err = %v", err)
		}
	})
}

// --- MAJOR 3: one pool-lock acquisition for the whole `install` span ---

// TestHarnessInstallHoldsPoolLockForWholeSpan proves the pool lock is held
// continuously across constraint-write→refresh→update→install: the fake
// Source and the fake installer each probe (via a second, independent
// flock.Flock on the same lock file) that they can NOT acquire it — if they
// could, the command-level lock must have been released and re-acquired
// somewhere in between, which is exactly the bug MAJOR 3 fixes.
func TestHarnessInstallHoldsPoolLockForWholeSpan(t *testing.T) {
	swapHasGuix(t, true)

	paths := testHarnessPaths(t)

	probeHeld := func(step string) {
		fl := flock.New(paths.AdvisoryLock)
		locked, err := fl.TryLock()
		if err != nil {
			t.Errorf("probing pool lock during %s: %v", step, err)
			return
		}
		if locked {
			t.Errorf("pool lock was NOT held during %s — the command-level lock must span the whole install", step)
			_ = fl.Unlock()
		}
	}

	src := &fakeHarnessSource{
		onCall: probeHeld,
		candidates: map[string][]pool.Candidate{
			"@anthropic-ai/claude-code": {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
		},
	}
	deps := testHarnessDeps(paths, t.TempDir(), src)

	installCalls := 0
	swapRunInstallArgv(t, func([]string) error {
		installCalls++
		probeHeld("install")
		return nil
	})

	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", harness.Registry["claude"], "", false, ""); err != nil {
		t.Fatalf("runHarnessInstall: %v", err)
	}
	if installCalls != 1 {
		t.Errorf("installCalls = %d, want 1", installCalls)
	}
}

// --- MINOR: install's idempotent path skips the npm run ---

func TestHarnessInstallIdempotentSkipsInstallWhenBinaryPresent(t *testing.T) {
	swapHasGuix(t, true)

	paths := testHarnessPaths(t)
	binDir := t.TempDir()
	src := &fakeHarnessSource{
		candidates: map[string][]pool.Candidate{
			"@anthropic-ai/claude-code": {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
		},
	}
	deps := testHarnessDeps(paths, binDir, src)
	installCalls := 0
	swapRunInstallArgv(t, func([]string) error { installCalls++; return nil })

	d := harness.Registry["claude"]
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, "", false, ""); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("installCalls after first install = %d, want 1", installCalls)
	}

	// Simulate the (faked) installer having actually dropped a binary.
	if err := os.MkdirAll(filepath.Join(binDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "bin", d.Bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Second install call: manifest/lock/recipe all agree (idempotent), and
	// the binary is present — must skip the npm run entirely.
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, "", false, ""); err != nil {
		t.Fatalf("second (idempotent) install: %v", err)
	}
	if installCalls != 1 {
		t.Errorf("installCalls after second (idempotent) install = %d, want still 1 (npm run must be skipped)", installCalls)
	}
}

// TestMaybeInstallLockedWarnsOnlyWhenReplacingUnverifiedInstall covers the
// §16 Phase 1 item 4 minor: maybeInstallLocked prints the
// "provenance unverified" line only when it is about to overwrite an
// ALREADY-PRESENT pool binary with a freshly resolved (result.Changed)
// version — not on a fresh install with no prior binary, and not on the
// idempotent (!result.Changed) path, which skips the install entirely.
func TestMaybeInstallLockedWarnsOnlyWhenReplacingUnverifiedInstall(t *testing.T) {
	d := harness.Registry["claude"]

	cases := []struct {
		name        string
		changed     bool
		binPresent  bool
		wantWarning bool
	}{
		{"changed + binary already present: warns", true, true, true},
		{"changed + no prior binary: no warning", true, false, false},
		{"idempotent + binary already present: no warning (install skipped)", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapHasGuix(t, true)
			swapRunInstallArgv(t, func([]string) error { return nil })

			binDir := t.TempDir()
			deps := testHarnessDeps(pool.PoolPaths{}, binDir, &fakeHarnessSource{})

			if tc.binPresent {
				if err := os.MkdirAll(filepath.Join(binDir, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(binDir, "bin", d.Bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			result := &pool.UpdateResult{
				Harness: &pool.LockHarness{Version: "2.1.220"},
				Changed: tc.changed,
			}

			cmd := &cobra.Command{}
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			if err := maybeInstallLocked(cmd, deps, "claude", d, result); err != nil {
				t.Fatalf("maybeInstallLocked: %v", err)
			}

			gotWarning := strings.Contains(buf.String(), "provenance unverified")
			if gotWarning != tc.wantWarning {
				t.Errorf("warning present = %v, want %v; output:\n%s", gotWarning, tc.wantWarning, buf.String())
			}
		})
	}
}

// TestHarnessInstallVsUpdateAdvanceWiring is the cmd-level end-to-end check
// that `install` passes UpdateOptions.Advance=false and `update` passes
// Advance=true (§10: "update is the only normal command that advances a
// floating constraint"): once a newer, also-eligible candidate is observed,
// re-running `install` must not move the lock, but `update` must.
func TestHarnessInstallVsUpdateAdvanceWiring(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{
		candidates: map[string][]pool.Candidate{
			claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
		},
	}
	deps := testHarnessDeps(paths, t.TempDir(), src)

	d := harness.Registry["claude"]
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, ">=2 <3", false, ""); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	// A later refresh discovers a newer, also-eligible candidate.
	obs, err := pool.LoadObservations(paths.Observations)
	if err != nil {
		t.Fatal(err)
	}
	if err := obs.Observe(pool.ObservationKey("npm", claudePkg, "2.1.224"),
		pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}

	// A plain `install` re-run must NOT advance to 2.1.224.
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, "", false, ""); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	status, err := pool.StatusHarness(paths, "claude", claudePkg, fixedTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if status.Locked != "2.1.220" {
		t.Errorf("Locked after re-install = %q, want unchanged 2.1.220 (install must not advance)", status.Locked)
	}

	// `update` MUST advance to 2.1.224.
	if _, err := runHarnessCmdWithDeps(t, deps, "update", "claude"); err != nil {
		t.Fatalf("update: %v", err)
	}
	status2, err := pool.StatusHarness(paths, "claude", claudePkg, fixedTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if status2.Locked != "2.1.224" {
		t.Errorf("Locked after update = %q, want advanced to 2.1.224", status2.Locked)
	}
}

// --- MINOR: yamledit.MapSet preserves an edited scalar's inline comment ---

func TestMapSetNodePreservesInlineCommentOnEditedField(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	// A single space before the comment, matching yaml.v3's own canonical
	// re-encoding — the achievable "byte-level" guarantee is that editing
	// one field doesn't disturb another node's already-canonical line, not
	// that arbitrary hand-formatted spacing survives the encoder (yaml.v3
	// re-renders LineComment spacing on every Encode call regardless).
	original := "schema_version: 1\npolicy:\n  minimum_release_age: 168h # seven days\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	err := editManifest(path, func(doc *yaml.Node) error {
		target := yamledit.EnsureMapChild(doc, "policy")
		yamledit.MapSet(target, "minimum_release_age", yamledit.ScalarNode((72 * time.Hour).String()))
		return nil
	})
	if err != nil {
		t.Fatalf("editManifest: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantLine := "  minimum_release_age: " + (72 * time.Hour).String() + " # seven days"
	found := false
	for _, line := range strings.Split(string(got), "\n") {
		if line == wantLine {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the edited line to keep its inline comment exactly as %q, got:\n%s", wantLine, got)
	}
}

// TestHarnessPolicySetLeavesUntouchedFieldByteIdentical: editing one policy
// field via the real `policy set` command must not disturb an unrelated
// field's line, comment included, byte-for-byte. policy set never reaches
// poolBinDir/newSource/resolveBackend/hasGuix/runInstallArgv, so this test
// needs only a poolPaths fake and is safe to parallelize.
func TestHarnessPolicySetLeavesUntouchedFieldByteIdentical(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	deps := harnessDeps{poolPaths: func() pool.PoolPaths { return paths }}

	untouchedLine := "  minimum_release_age: 168h # seven days"
	original := "schema_version: 1\npolicy:\n" + untouchedLine + "\n"
	if err := os.WriteFile(paths.Manifest, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runHarnessCmdWithDeps(t, deps, "policy", "set", "--minimum-observation-age", "24h"); err != nil {
		t.Fatalf("policy set: %v", err)
	}

	got, err := os.ReadFile(paths.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(got), "\n") {
		if line == untouchedLine {
			found = true
		}
	}
	if !found {
		t.Errorf("untouched line %q did not survive byte-identical, got:\n%s", untouchedLine, got)
	}
}
