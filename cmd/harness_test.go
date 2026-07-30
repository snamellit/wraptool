package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"

	"github.com/gofrs/flock"
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

// --- test seams ---

func swapHarnessPoolPaths(t *testing.T, paths pool.PoolPaths) {
	t.Helper()
	orig := harnessPoolPaths
	harnessPoolPaths = func() pool.PoolPaths { return paths }
	t.Cleanup(func() { harnessPoolPaths = orig })
}

func swapHasGuix(t *testing.T, v bool) {
	t.Helper()
	orig := hasGuix
	hasGuix = func() bool { return v }
	t.Cleanup(func() { hasGuix = orig })
}

func swapHarnessSource(t *testing.T, src pool.Source) {
	t.Helper()
	orig := newHarnessSource
	newHarnessSource = func() (pool.Source, error) { return src, nil }
	t.Cleanup(func() { newHarnessSource = orig })
}

func swapRunInstallArgv(t *testing.T, fn func([]string) error) {
	t.Helper()
	orig := runInstallArgv
	runInstallArgv = fn
	t.Cleanup(func() { runInstallArgv = orig })
}

func swapPoolBinDir(t *testing.T, dir string) {
	t.Helper()
	orig := poolBinDir
	poolBinDir = func() (string, string, error) { return dir, filepath.Join(dir, "bin"), nil }
	t.Cleanup(func() { poolBinDir = orig })
}

// runHarnessCmd executes `harness <args...>` through the root command,
// returning combined output and the command error. Mirrors runValidate's
// pattern in cmd/validate_test.go.
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

// TestHarnessCommandArgValidation table-tests every subcommand's argument
// validation. Every case here must fail before touching the filesystem (no
// pool paths, no Source, no backend) — each one returns from its RunE before
// reaching newHarnessSource()/resolveBackend()/setHarnessConstraint, so none
// of them can write into the real developer pool ($XDG_DATA_HOME).
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

// TestHarnessInstallTooManyArgs and TestHarnessUpdateTooManyArgs pin cobra's
// own arity enforcement (ExactArgs(1) for install, MaximumNArgs(1) for the
// optional-harness commands) so a future edit can't accidentally loosen it.
func TestHarnessInstallRequiresExactlyOneArg(t *testing.T) {
	if _, err := runHarnessCmd(t, "install"); err == nil {
		t.Error("expected an error for `install` with no harness name")
	}
	if _, err := runHarnessCmd(t, "install", "claude", "extra"); err == nil {
		t.Error("expected an error for `install` with more than one argument")
	}
}

func TestHarnessUpdateAcceptsAtMostOneArg(t *testing.T) {
	if _, err := runHarnessCmd(t, "update", "claude", "extra"); err == nil {
		t.Error("expected an error for `update` with more than one argument")
	}
}

// --- list rendering (§10 column shapes) ---

func testHarnessPaths(t *testing.T) pool.PoolPaths {
	t.Helper()
	dir := t.TempDir()
	return pool.PoolPaths{
		Manifest:     filepath.Join(dir, "manifest.yaml"),
		Lock:         filepath.Join(dir, "harness-lock.json"),
		Observations: filepath.Join(dir, "observations.json"),
		Audit:        filepath.Join(dir, "audit.jsonl"),
		AdvisoryLock: filepath.Join(dir, "lock"),
		Cache:        filepath.Join(dir, "cache"),
		Staging:      filepath.Join(dir, "staging"),
	}
}

func TestRunHarnessListRendersColumnsAndQuarantineCountdown(t *testing.T) {
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
		target := ensureMapChild(doc, "policy")
		mapSetNode(target, "minimum_observation_age", scalarNode((24 * time.Hour).String()))
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
	argv := []string{"guix", "shell", "node", "--", "npm", "install", "-g", "some-other-pkg"}
	if _, err := pinInstallVersion(argv, "@anthropic-ai/claude-code", "2.1.220"); err == nil {
		t.Fatal("expected an error when the package is not present in argv")
	}
}

// --- MAJOR 2: bare refresh/update/sync iterate manifest/lock-listed
// harnesses only, and per-harness failures are collected, not fatal ---

func TestHarnessUpdateBareOnlyTouchesManifestListedHarnesses(t *testing.T) {
	paths := testHarnessPaths(t)
	swapHarnessPoolPaths(t, paths)
	swapHasGuix(t, true)
	swapPoolBinDir(t, t.TempDir())
	swapRunInstallArgv(t, func([]string) error { return nil })

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
	swapHarnessSource(t, src)

	out, err := runHarnessCmd(t, "update")
	if err != nil {
		t.Fatalf("bare update: %v\noutput:\n%s", err, out)
	}
	if len(src.resolvedPkgs) != 1 || src.resolvedPkgs[0] != claudePkg {
		t.Errorf("resolvedPkgs = %v, want exactly [%s] — bare update must touch only manifest-listed harnesses", src.resolvedPkgs, claudePkg)
	}
}

func TestHarnessUpdatePerHarnessFailureContinuesToNext(t *testing.T) {
	paths := testHarnessPaths(t)
	swapHarnessPoolPaths(t, paths)
	swapHasGuix(t, true)
	swapPoolBinDir(t, t.TempDir())
	swapRunInstallArgv(t, func([]string) error { return nil })

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
	swapHarnessSource(t, src)

	out, err := runHarnessCmd(t, "update")
	if err == nil {
		t.Fatal("expected a non-nil error since claude's update failed")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the failed harness claude, got: %v", err)
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

// --- MAJOR 3: one pool-lock acquisition for the whole `install` span ---

// TestHarnessInstallHoldsPoolLockForWholeSpan proves the pool lock is held
// continuously across constraint-write→refresh→update→install: the fake
// Source and the fake installer each probe (via a second, independent
// flock.Flock on the same lock file) that they can NOT acquire it — if they
// could, the command-level lock must have been released and re-acquired
// somewhere in between, which is exactly the bug MAJOR 3 fixes.
func TestHarnessInstallHoldsPoolLockForWholeSpan(t *testing.T) {
	paths := testHarnessPaths(t)
	swapHarnessPoolPaths(t, paths)
	swapHasGuix(t, true)
	swapPoolBinDir(t, t.TempDir())

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
	swapHarnessSource(t, src)

	installCalls := 0
	swapRunInstallArgv(t, func([]string) error {
		installCalls++
		probeHeld("install")
		return nil
	})

	if err := runHarnessInstall(context.Background(), "claude", harness.Registry["claude"], "", false, ""); err != nil {
		t.Fatalf("runHarnessInstall: %v", err)
	}
	if installCalls != 1 {
		t.Errorf("installCalls = %d, want 1", installCalls)
	}
}

// --- MINOR: install's idempotent path skips the npm run ---

func TestHarnessInstallIdempotentSkipsInstallWhenBinaryPresent(t *testing.T) {
	paths := testHarnessPaths(t)
	swapHarnessPoolPaths(t, paths)
	swapHasGuix(t, true)
	binDir := t.TempDir()
	swapPoolBinDir(t, binDir)

	src := &fakeHarnessSource{
		candidates: map[string][]pool.Candidate{
			"@anthropic-ai/claude-code": {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
		},
	}
	swapHarnessSource(t, src)
	installCalls := 0
	swapRunInstallArgv(t, func([]string) error { installCalls++; return nil })

	d := harness.Registry["claude"]
	if err := runHarnessInstall(context.Background(), "claude", d, "", false, ""); err != nil {
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
	if err := runHarnessInstall(context.Background(), "claude", d, "", false, ""); err != nil {
		t.Fatalf("second (idempotent) install: %v", err)
	}
	if installCalls != 1 {
		t.Errorf("installCalls after second (idempotent) install = %d, want still 1 (npm run must be skipped)", installCalls)
	}
}

// TestHarnessInstallVsUpdateAdvanceWiring is the cmd-level end-to-end check
// that `install` passes UpdateOptions.Advance=false and `update` passes
// Advance=true (§10: "update is the only normal command that advances a
// floating constraint"): once a newer, also-eligible candidate is observed,
// re-running `install` must not move the lock, but `update` must.
func TestHarnessInstallVsUpdateAdvanceWiring(t *testing.T) {
	paths := testHarnessPaths(t)
	swapHarnessPoolPaths(t, paths)
	swapHasGuix(t, true)
	swapPoolBinDir(t, t.TempDir())
	swapRunInstallArgv(t, func([]string) error { return nil })

	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{
		candidates: map[string][]pool.Candidate{
			claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
		},
	}
	swapHarnessSource(t, src)

	d := harness.Registry["claude"]
	if err := runHarnessInstall(context.Background(), "claude", d, ">=2 <3", false, ""); err != nil {
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
	if err := runHarnessInstall(context.Background(), "claude", d, "", false, ""); err != nil {
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
	if _, err := runHarnessCmd(t, "update", "claude"); err != nil {
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

// --- MINOR: mapSetNode preserves an edited scalar's inline comment ---

func TestMapSetNodePreservesInlineCommentOnEditedField(t *testing.T) {
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
		target := ensureMapChild(doc, "policy")
		mapSetNode(target, "minimum_release_age", scalarNode((72 * time.Hour).String()))
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
// field's line, comment included, byte-for-byte.
func TestHarnessPolicySetLeavesUntouchedFieldByteIdentical(t *testing.T) {
	paths := testHarnessPaths(t)
	swapHarnessPoolPaths(t, paths)

	untouchedLine := "  minimum_release_age: 168h # seven days"
	original := "schema_version: 1\npolicy:\n" + untouchedLine + "\n"
	if err := os.WriteFile(paths.Manifest, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runHarnessCmd(t, "policy", "set", "--minimum-observation-age", "24h"); err != nil {
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
