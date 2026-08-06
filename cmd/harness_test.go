package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
	"github.com/pti/wraptool/internal/pool/source"
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
	// failRealize makes Realize always fail — wave1-integration-fixes finding
	// 2's cmd-level regression: a Realize failure must be non-fatal for
	// install/update (RealizeErr, not a returned error), so the legacy
	// install still runs.
	failRealize bool
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
		// Runtime records artifacts for the glibc platforms only, mirroring
		// the real npm Source (source/npm.go's nodeGlibcPlatforms, §5.1's
		// musl deferral) — Realize below consults this map, so a test that
		// requests a musl platform hits the same "no runtime artifact
		// recorded" failure a real pool would (wave1-integration-fixes
		// finding 5).
		Runtime: &pool.LockRuntime{
			Name: "node", Version: "22.11.0",
			Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: "https://nodejs.org/dist/v22.11.0/node-v22.11.0-linux-x64.tar.xz", SHA256: "fakeruntimesha"},
				pool.PlatformLinuxARM64Glibc: {URL: "https://nodejs.org/dist/v22.11.0/node-v22.11.0-linux-arm64.tar.xz", SHA256: "fakeruntimesha"},
			},
		},
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

// Realize writes a small but REAL generation tree and builds its receipt
// with the real pool receipt helpers (ScanGenerationFiles/ManifestDigest) —
// internal/pool/orchestrator_test.go's fakeSource.Realize does the same, for
// the same reason (exercise host-side verification for real).
func (f *fakeHarnessSource) Realize(_ context.Context, _ backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, _, stagingGenDir string) (*pool.Receipt, error) {
	if f.onCall != nil {
		f.onCall("realize:" + harnessName)
	}
	if f.failRealize {
		return nil, fmt.Errorf("fake realize failure for %s", harnessName)
	}
	// Mirror the real npm Source (internal/pool/source/realize.go): a
	// platform with no recorded runtime artifact — the two musl keys, §5.1's
	// deferral — must fail here too, so a cmd-level test can't silently
	// reach a platform production Realize would reject
	// (wave1-integration-fixes finding 5).
	if h.Runtime == nil {
		return nil, fmt.Errorf("realize: %s: lock has no runtime requirement", harnessName)
	}
	if _, ok := h.Runtime.Artifacts[key]; !ok {
		return nil, fmt.Errorf("realize: %s: no runtime artifact recorded for platform %q", harnessName, key)
	}
	name := binName
	if name == "" {
		name = harnessName
	}
	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		return nil, err
	}
	files, err := pool.ScanGenerationFiles(stagingGenDir)
	if err != nil {
		return nil, err
	}
	digest, err := pool.ManifestDigest(files)
	if err != nil {
		return nil, err
	}
	lockHash, err := h.Hash()
	if err != nil {
		return nil, err
	}
	return &pool.Receipt{
		SchemaVersion:  pool.ReceiptSchemaVersion,
		Harness:        harnessName,
		Version:        h.Version,
		Platform:       key,
		RecipeRevision: h.RecipeRevision,
		LockEntryHash:  lockHash,
		Backend:        pool.ReceiptBackend{Name: "fake"},
		Files:          files,
		ManifestDigest: digest,
	}, nil
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
		Manifest:            filepath.Join(dir, "manifest.yaml"),
		Lock:                filepath.Join(dir, "harness-lock.json"),
		Observations:        filepath.Join(dir, "observations.json"),
		Audit:               filepath.Join(dir, "audit.jsonl"),
		AdvisoryLock:        filepath.Join(dir, "lock"),
		Cache:               filepath.Join(dir, "cache", "sha256"), // mirrors pool.CacheSHA256Dir()
		Staging:             filepath.Join(dir, "staging"),
		Runtime:             filepath.Join(dir, "runtime"),
		LegacyRemovedMarker: filepath.Join(dir, "legacy-removed"),
	}
}

// testHarnessDeps builds a harnessDeps for tests: poolPaths returns paths,
// poolBinDir points at a fresh subdirectory of binDir, newSource returns
// src, and resolveBackend always succeeds with a no-op fake backend. This
// is constructor injection, not global mutation — a test builds its own
// harnessDeps value and passes it directly to newHarnessCmdWithDeps or a
// helper function, so concurrent tests (t.Parallel()) using their own
// testHarnessDeps never contend with each other. Tests that swap a
// package-level seam (swapHasGuix/swapOCIAvailable/swapHostArch/
// swapProvisioningImage/swapRunInstallArgv) must stay serial; each helper
// enforces that mechanically via t.Setenv, which panics under t.Parallel.
// It does NOT cover hasGuix
// or runInstallArgv, which stay package vars (out of the MR59 review's
// scope — see runInstallArgv's doc comment) — a test that reaches the
// legacy npm-install recipe still needs swapHasGuix/swapRunInstallArgv
// alongside this.
func testHarnessDeps(paths pool.PoolPaths, binDir string, src pool.Source) harnessDeps {
	return harnessDeps{
		poolPaths:      func() pool.PoolPaths { return paths },
		poolBinDir:     func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:      func(harness.Descriptor) (pool.Source, error) { return src, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) { return fakeHarnessBackend{}, nil },
	}
}

func swapHasGuix(t *testing.T, v bool) {
	// t.Setenv panics inside a t.Parallel test — a mechanical guard that a
	// test swapping this package-level seam can never run concurrently with
	// the rest of the suite (the swap would race every reader).
	t.Setenv("WRAPTOOL_TEST_SEAM_SWAP", "1")
	t.Helper()
	orig := hasGuix
	hasGuix = func() bool { return v }
	t.Cleanup(func() { hasGuix = orig })
}

// swapOCIAvailable stubs ociAvailable (defaultResolveBackend's docker/podman
// probe) without requiring a real container engine on the machine running
// the tests — the cmd-package analogue of internal/pool/backend's own
// lookPath seam, one layer up.
func swapOCIAvailable(t *testing.T, v bool) {
	// t.Setenv panics inside a t.Parallel test — a mechanical guard that a
	// test swapping this package-level seam can never run concurrently with
	// the rest of the suite (the swap would race every reader).
	t.Setenv("WRAPTOOL_TEST_SEAM_SWAP", "1")
	t.Helper()
	orig := ociAvailable
	ociAvailable = func() bool { return v }
	t.Cleanup(func() { ociAvailable = orig })
}

// swapHostArch stubs hostArch (defaultResolveBackend's own arch() comparison
// input) so cross-arch-vs-host-arch backend routing can be tested
// deterministically regardless of which architecture actually runs the test
// binary.
func swapHostArch(t *testing.T, v string) {
	// t.Setenv panics inside a t.Parallel test — a mechanical guard that a
	// test swapping this package-level seam can never run concurrently with
	// the rest of the suite (the swap would race every reader).
	t.Setenv("WRAPTOOL_TEST_SEAM_SWAP", "1")
	t.Helper()
	orig := hostArch
	hostArch = func() string { return v }
	t.Cleanup(func() { hostArch = orig })
}

// swapProvisioningImage stubs the provisioningImage package var (the
// production default lives in the immutable defaultProvisioningImage
// const — see that const's doc comment) so a test can prove the image
// reference actually reaches recipe_revision without a real second image to
// install against.
func swapProvisioningImage(t *testing.T, v string) {
	// t.Setenv panics inside a t.Parallel test — a mechanical guard that a
	// test swapping this package-level seam can never run concurrently with
	// the rest of the suite (the swap would race every reader).
	t.Setenv("WRAPTOOL_TEST_SEAM_SWAP", "1")
	t.Helper()
	orig := provisioningImage
	provisioningImage = v
	t.Cleanup(func() { provisioningImage = orig })
}

// readLockRecipeRevision reads harness-lock.json at path and returns the
// named harness's persisted recipe_revision — a plain JSON read rather than
// going through internal/pool's own unexported lock loader, since cmd tests
// have no access to it (and don't need the rest of pool.Lock's validation
// machinery just to read one field back).
func readLockRecipeRevision(t *testing.T, path, harnessName string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc struct {
		Harnesses map[string]struct {
			RecipeRevision string `json:"recipe_revision"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	h, ok := doc.Harnesses[harnessName]
	if !ok {
		t.Fatalf("%s: no lock entry for %q", path, harnessName)
	}
	return h.RecipeRevision
}

func swapRunInstallArgv(t *testing.T, fn func([]string) error) {
	// t.Setenv panics inside a t.Parallel test — a mechanical guard that a
	// test swapping this package-level seam can never run concurrently with
	// the rest of the suite (the swap would race every reader).
	t.Setenv("WRAPTOOL_TEST_SEAM_SWAP", "1")
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
		{"refresh: legacy (non-lockable) harness", []string{"refresh", "gemini"}, "not pool-lockable"},
		{"update: unknown harness", []string{"update", "bogus"}, "unknown harness"},
		{"update: legacy (non-lockable) harness", []string{"update", "gemini"}, "not pool-lockable"},
		{"update: --allow-young without --reason", []string{"update", "claude", "--allow-young"}, "--reason"},
		{"sync: unknown harness", []string{"sync", "bogus"}, "unknown harness"},
		{"sync: legacy (non-lockable) harness", []string{"sync", "roo"}, "not pool-lockable"},
		{"sync: unknown --platform lists the closed enum", []string{"sync", "--platform", "bogus-platform"},
			"linux-amd64-glibc, linux-arm64-glibc, linux-amd64-musl, linux-arm64-musl"},
		{"rollback: unknown harness", []string{"rollback", "bogus"}, "unknown harness"},
		{"rollback: legacy (non-lockable) harness", []string{"rollback", "roo"}, "not pool-lockable"},
		{"rollback: unknown --platform lists the closed enum", []string{"rollback", "claude", "--platform", "bogus-platform"},
			"linux-amd64-glibc, linux-arm64-glibc, linux-amd64-musl, linux-arm64-musl"},
		{"rollback: requires exactly one harness", []string{"rollback"}, "accepts 1 arg"},
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
		pool.FormatTimestamp(now.Add(-200*time.Hour)), pool.FormatTimestamp(now.Add(-200*time.Hour)), false, false, false); err != nil {
		t.Fatal(err)
	}
	// Published exactly 165h ago: 168h - 165h = 3h remaining... use days instead.
	quarantinedPublished := now.Add(-(168*time.Hour - 3*24*time.Hour))
	if err := obs.Observe(pool.ObservationKey("npm", pkg, "2.1.224"),
		pool.FormatTimestamp(quarantinedPublished), pool.FormatTimestamp(quarantinedPublished), false, false, false); err != nil {
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
				Generations: map[pool.PlatformKey]string{
					pool.PlatformLinuxAMD64Glibc: "sha256:" + strings.Repeat("a", 64),
					pool.PlatformLinuxARM64Glibc: "sha256:" + strings.Repeat("b", 64),
				},
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

	if !strings.Contains(out, "HARNESS") || !strings.Contains(out, "CONSTRAINT") || !strings.Contains(out, "QUARANTINED") || !strings.Contains(out, "REALIZED") {
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
	if !strings.Contains(out, "linux-amd64-glibc,linux-arm64-glibc") {
		t.Errorf("expected claude's REALIZED cell to comma-join its two realized platforms, got:\n%s", out)
	}
	// A legacy (not pool-lockable) harness's REALIZED cell is "-", same as
	// every other column that doesn't apply to it. Locate the actual REALIZED
	// column position from the header (tabwriter aligns every row's column
	// starts identically) rather than trusting the line's trailing "-" —
	// QUARANTINED's own value ("legacy (not lockable)") contains no
	// whitespace-delimited "-", but a naive HasSuffix check would pass even
	// if REALIZED's cell silently went missing and some earlier column's
	// padding happened to end in "-".
	lines := strings.Split(out, "\n")
	var header string
	for _, line := range lines {
		if strings.HasPrefix(line, "HARNESS") {
			header = line
			break
		}
	}
	if header == "" {
		t.Fatalf("no header line found in output:\n%s", out)
	}
	col := strings.Index(header, "REALIZED")
	if col < 0 {
		t.Fatalf("REALIZED not found in header %q", header)
	}
	sawLegacyRow := false
	for _, line := range lines {
		if !strings.Contains(line, "legacy (not lockable)") {
			continue
		}
		sawLegacyRow = true
		if col >= len(line) {
			t.Fatalf("legacy row %q is shorter than the REALIZED column offset %d", line, col)
		}
		cell := strings.TrimRight(line[col:], " ")
		if cell != "-" {
			t.Errorf("expected a legacy harness's REALIZED cell to be exactly %q, got %q (row: %q)", "-", cell, line)
		}
	}
	if !sawLegacyRow {
		t.Fatal("no legacy (not lockable) row found to check")
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
		pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false, false); err != nil {
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
			pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false, false); err != nil {
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
			newSource: func(harness.Descriptor) (pool.Source, error) { return nil, errors.New("source unavailable") },
			resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
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
			newSource:      func(harness.Descriptor) (pool.Source, error) { return &fakeHarnessSource{}, nil },
			resolveBackend: func(pool.PlatformKey) (backend.Backend, error) { return nil, errors.New("guix not on PATH") },
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
		pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}

	// A plain `install` re-run must NOT advance to 2.1.224.
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, "", false, ""); err != nil {
		t.Fatalf("re-install: %v", err)
	}
	status, err := pool.StatusHarness(paths, "claude", pool.SourceNPM, claudePkg, fixedTestNow)
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
	status2, err := pool.StatusHarness(paths, "claude", pool.SourceNPM, claudePkg, fixedTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if status2.Locked != "2.1.224" {
		t.Errorf("Locked after update = %q, want advanced to 2.1.224", status2.Locked)
	}
}

// TestHarnessInstallRepairPrintsRealizedNotJustUpToDate is the CLI-
// truthfulness regression for review finding 5: a repair (§15/§17 — the
// lock still records a generation digest for this platform, but its
// directory has vanished) reaches DecisionRealize, which has Changed=false
// and no Note. Before this fix, printUpdateResult's default case reported
// that as a plain "already locked ... (up to date)", indistinguishable from
// a true no-op — even though RealizeGeneration just rebuilt a whole
// generation. It must instead name the generation it (re)built.
func TestHarnessInstallRepairPrintsRealizedNotJustUpToDate(t *testing.T) {
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

	key, err := pool.HostPlatformKey()
	if err != nil {
		t.Fatal(err)
	}
	status, err := pool.StatusHarness(paths, "claude", pool.SourceNPM, claudePkg, fixedTestNow)
	if err != nil {
		t.Fatal(err)
	}
	realized := false
	for _, k := range status.Realized {
		if k == key {
			realized = true
		}
	}
	if !realized {
		t.Fatalf("expected claude to be realized for %s after install, got %v", key, status.Realized)
	}

	// Delete the generation directory but leave the lock's recorded digest
	// alone — the repair scenario.
	if err := os.RemoveAll(filepath.Join(paths.Runtime, "generations")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := bareHarnessCmd()
	cmd.SetOut(&buf)
	if err := runHarnessInstall(context.Background(), cmd, deps, "claude", d, "", false, ""); err != nil {
		t.Fatalf("repair install: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "realized") {
		t.Errorf("expected the repair install's output to mention the rebuilt generation, got:\n%s", out)
	}
	if strings.Contains(out, "(up to date)") {
		t.Errorf("expected the repair install's output NOT to just report plain up-to-date (review finding 5), got:\n%s", out)
	}
}

// TestHarnessInstallSurvivesRealizeFailure is wave1-integration-fixes
// finding 2's cmd-level regression: a Realize failure must not abort
// `install` — dispatcher/activation/mounting are a later wave, so the
// legacy npm install (the only thing this wave actually executes) must
// still run, and the failure must be surfaced as a clearly-labeled warning,
// not swallowed or turned into a hard error.
func TestHarnessInstallSurvivesRealizeFailure(t *testing.T) {
	swapHasGuix(t, true)
	installCalls := 0
	swapRunInstallArgv(t, func([]string) error { installCalls++; return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{
		candidates: map[string][]pool.Candidate{
			claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
		},
		failRealize: true,
	}
	deps := testHarnessDeps(paths, t.TempDir(), src)
	d := harness.Registry["claude"]

	err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, ">=2 <3", false, "")
	if err != nil {
		t.Fatalf("install: %v, want nil — a Realize failure must not abort install (legacy install must still run)", err)
	}
	if installCalls != 1 {
		t.Errorf("legacy installer calls = %d, want 1 — install must still run the legacy install despite the Realize failure", installCalls)
	}
}

// TestHarnessInstallPrintsRealizeWarning proves the OTHER half of finding 2:
// the Realize failure must be reported as a clearly-labeled warning on
// stderr, not silently swallowed.
func TestHarnessInstallPrintsRealizeWarning(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{
		candidates: map[string][]pool.Candidate{
			claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
		},
		failRealize: true,
	}
	deps := testHarnessDeps(paths, t.TempDir(), src)
	d := harness.Registry["claude"]

	var buf bytes.Buffer
	cmd := bareHarnessCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := runHarnessInstall(context.Background(), cmd, deps, "claude", d, ">=2 <3", false, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "generation realization failed") {
		t.Errorf("expected a clearly-labeled realization-failure warning, got:\n%s", out)
	}
	if !strings.Contains(out, "fake realize failure for claude") {
		t.Errorf("expected the warning to include the underlying error, got:\n%s", out)
	}
}

// TestHarnessSyncRealizesRequestedPlatformAndSurfacesInList: `sync
// --platform <key>` (§16 Phase 2) realizes exactly the requested platform —
// not necessarily the host's own — and `list`'s REALIZED column then shows
// it, comma-joined were there more than one.
func TestHarnessSyncRealizesRequestedPlatformAndSurfacesInList(t *testing.T) {
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
		t.Fatalf("install: %v", err)
	}

	// install already realized the host's own platform (linux-amd64-glibc in
	// this test's runtime.GOARCH-independent case only if amd64 — instead of
	// depending on the test host's arch, explicitly sync a SECOND, arbitrary
	// platform key and check both end up recorded. linux-arm64-glibc, not a
	// musl key (wave1-integration-fixes finding 5): realization only supports
	// glibc platforms (§5.1's musl deferral), a path production can't reach —
	// this test must exercise a platform sync can actually realize.
	out, err := runHarnessCmdWithDeps(t, deps, "sync", "claude", "--platform", "linux-arm64-glibc")
	if err != nil {
		t.Fatalf("sync --platform: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "linux-arm64-glibc") {
		t.Errorf("expected sync's output to name the realized platform, got:\n%s", out)
	}

	status, err := pool.StatusHarness(paths, "claude", pool.SourceNPM, claudePkg, fixedTestNow)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range status.Realized {
		if k == pool.PlatformLinuxARM64Glibc {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Realized to include linux-arm64-glibc after sync --platform, got %v", status.Realized)
	}

	var buf bytes.Buffer
	if err := runHarnessList(paths, filepath.Join(t.TempDir(), "bin"), fixedTestNow, &buf); err != nil {
		t.Fatalf("runHarnessList: %v", err)
	}
	if !strings.Contains(buf.String(), "linux-arm64-glibc") {
		t.Errorf("expected list's REALIZED column to include linux-arm64-glibc, got:\n%s", buf.String())
	}
}

// --- MINOR: yamledit.MapSet preserves an edited scalar's inline comment ---

func TestMapSetPreservesInlineCommentOnEditedField(t *testing.T) {
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

// --- rollback / gc / ACTIVE column (pool-activation work package) ---

// tableCell locates rowPrefix's row in a runHarnessList-rendered table and
// returns the cell under columnHeader, trimmed. Column boundaries are found
// from the HEADER line (tabwriter aligns every row's column starts
// identically) rather than assumed from a fixed offset, and a cell is
// considered to end at the next run of two-or-more spaces (tabwriter's own
// padding) or end of line — mirroring
// TestRunHarnessListRendersColumnsAndQuarantineCountdown's existing
// REALIZED-column lookup, generalized to any column/row.
func tableCell(t *testing.T, out, columnHeader, rowPrefix string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	var header string
	for _, line := range lines {
		if strings.HasPrefix(line, "HARNESS") {
			header = line
			break
		}
	}
	if header == "" {
		t.Fatalf("no header line found in output:\n%s", out)
	}
	col := strings.Index(header, columnHeader)
	if col < 0 {
		t.Fatalf("%s not found in header %q", columnHeader, header)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, rowPrefix) {
			continue
		}
		if col >= len(line) {
			t.Fatalf("row %q is shorter than the %s column offset %d", line, columnHeader, col)
		}
		rest := line[col:]
		end := len(rest)
		for i := 0; i < len(rest)-1; i++ {
			if rest[i] == ' ' && rest[i+1] == ' ' {
				end = i
				break
			}
		}
		return strings.TrimRight(rest[:end], " ")
	}
	t.Fatalf("no row with prefix %q found in output:\n%s", rowPrefix, out)
	return ""
}

// seedOrphanGeneration builds a small, real, receipt-valid generation
// directly under paths.Runtime/generations/<id>/ using only pool's exported
// API (ScanGenerationFiles/ManifestDigest/GenerationID/Receipt/
// WriteFileAtomic) — cmd-package tests have no access to internal/pool's
// unexported test helpers. It is deliberately NOT referenced by any lock or
// pointer, so GC must remove it.
func seedOrphanGeneration(t *testing.T, paths pool.PoolPaths, harnessName, version string, key pool.PlatformKey, content string) (genID string) {
	t.Helper()
	scratch := t.TempDir()
	binDir := filepath.Join(scratch, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, harnessName), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := pool.ScanGenerationFiles(scratch)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := pool.ManifestDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	genID, err = pool.GenerationID(harnessName, version, key, digest)
	if err != nil {
		t.Fatal(err)
	}
	r := &pool.Receipt{
		SchemaVersion:  pool.ReceiptSchemaVersion,
		Harness:        harnessName,
		Version:        version,
		Platform:       key,
		RecipeRevision: "sha256:" + strings.Repeat("c", 64),
		LockEntryHash:  "sha256:" + strings.Repeat("d", 64),
		Backend:        pool.ReceiptBackend{Name: "fake"},
		Files:          files,
		ManifestDigest: digest,
	}
	data, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteFileAtomic(filepath.Join(scratch, "receipt.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(paths.Runtime, "generations", genID)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(scratch, dest); err != nil {
		t.Fatal(err)
	}
	return genID
}

// TestHarnessListActiveColumnShowsHostPlatformActiveVersion: after an
// `install`, list's ACTIVE column shows the VERSION of the generation
// currently activated for the HOST's own platform, not merely "yes"/"no" —
// the pre-Phase-2-wave-2 legacy-bin-based rendering stays only for
// non-pool-lockable harnesses (proven separately by
// TestRunHarnessListRendersColumnsAndQuarantineCountdown's "legacy
// (not lockable)" row).
func TestHarnessListActiveColumnShowsHostPlatformActiveVersion(t *testing.T) {
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
		t.Fatalf("install: %v", err)
	}

	var buf bytes.Buffer
	if err := runHarnessList(paths, filepath.Join(t.TempDir(), "bin"), fixedTestNow, &buf); err != nil {
		t.Fatalf("runHarnessList: %v", err)
	}
	if got := tableCell(t, buf.String(), "ACTIVE", "claude"); got != "2.1.220" {
		t.Errorf("ACTIVE cell = %q, want the installed version 2.1.220", got)
	}
}

// TestHarnessRollbackSwitchesActiveGenerationAndListReflectsIt: `update`
// advances claude to a newer version (activating it); `rollback --to` the
// original version switches the active pointer back, and list's ACTIVE
// column shows the rolled-back version even though LOCKED still shows the
// newer one (rollback never touches the lock, §10).
func TestHarnessRollbackSwitchesActiveGenerationAndListReflectsIt(t *testing.T) {
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
		t.Fatalf("install: %v", err)
	}

	obs, err := pool.LoadObservations(paths.Observations)
	if err != nil {
		t.Fatal(err)
	}
	if err := obs.Observe(pool.ObservationKey("npm", claudePkg, "2.1.224"),
		pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}
	if _, err := runHarnessCmdWithDeps(t, deps, "update", "claude"); err != nil {
		t.Fatalf("update: %v", err)
	}

	var beforeBuf bytes.Buffer
	if err := runHarnessList(paths, filepath.Join(t.TempDir(), "bin"), fixedTestNow, &beforeBuf); err != nil {
		t.Fatalf("runHarnessList (before rollback): %v", err)
	}
	if got := tableCell(t, beforeBuf.String(), "ACTIVE", "claude"); got != "2.1.224" {
		t.Fatalf("ACTIVE cell before rollback = %q, want 2.1.224 (update must have activated it)", got)
	}

	out, err := runHarnessCmdWithDeps(t, deps, "rollback", "claude", "--to", "2.1.220")
	if err != nil {
		t.Fatalf("rollback: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "2.1.220") {
		t.Errorf("expected rollback's output to name the generation rolled back to, got:\n%s", out)
	}

	var afterBuf bytes.Buffer
	if err := runHarnessList(paths, filepath.Join(t.TempDir(), "bin"), fixedTestNow, &afterBuf); err != nil {
		t.Fatalf("runHarnessList (after rollback): %v", err)
	}
	if got := tableCell(t, afterBuf.String(), "ACTIVE", "claude"); got != "2.1.220" {
		t.Errorf("ACTIVE cell after rollback = %q, want 2.1.220", got)
	}
	if got := tableCell(t, afterBuf.String(), "LOCKED", "claude"); got != "2.1.224" {
		t.Errorf("LOCKED cell after rollback = %q, want unchanged 2.1.224 (rollback never rewrites the lock)", got)
	}
}

// TestHarnessRollbackToUnknownVersionListsAvailable pins the cmd-level
// surface of RollbackHarnessLocked's error: a --to version that was never
// realized names what IS available.
func TestHarnessRollbackToUnknownVersionListsAvailable(t *testing.T) {
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
		t.Fatalf("install: %v", err)
	}

	_, err := runHarnessCmdWithDeps(t, deps, "rollback", "claude", "--to", "9.9.9")
	if err == nil {
		t.Fatal("expected an error for a --to version that was never realized")
	}
	if !strings.Contains(err.Error(), "2.1.220") {
		t.Errorf("error = %q, want it to list the available version 2.1.220", err.Error())
	}
}

// TestHarnessRollbackPrintsSharedHomeWarning pins §13's cheap honor: a
// successful rollback always prints a static warning about shared harness
// home/ state possibly having been written by the newer version being
// rolled back from — no snapshot machinery, just the warning.
func TestHarnessRollbackPrintsSharedHomeWarning(t *testing.T) {
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
		t.Fatalf("install: %v", err)
	}

	// Roll back to the exact same, only-ever-realized version: still a
	// valid, successful rollback (RollbackHarnessLocked doesn't require the
	// target to differ from the current one when --to is given explicitly),
	// so the warning must still print.
	out, err := runHarnessCmdWithDeps(t, deps, "rollback", "claude", "--to", "2.1.220")
	if err != nil {
		t.Fatalf("rollback: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "warning: shared harness home state") {
		t.Errorf("expected the shared-home-state warning on a successful rollback, got:\n%s", out)
	}
}

// --- gc ---

// seedOrphanGenerationSpares seeds n orphan generations for (harnessName,
// key) — none referenced by any pointer or lock — with strictly increasing
// directory mtimes (ids[0] oldest .. ids[n-1] newest), via os.Chtimes: a
// single orphan is trivially retained by gc's own "2 most recently created
// spares" rule (§15), so a test that wants an ACTUAL removal needs 3+.
func seedOrphanGenerationSpares(t *testing.T, paths pool.PoolPaths, harnessName string, key pool.PlatformKey, n int) (ids []string) {
	t.Helper()
	ids = make([]string, n)
	for i := 0; i < n; i++ {
		id := seedOrphanGeneration(t, paths, harnessName, fmt.Sprintf("%d.0.0", i+1), key, fmt.Sprintf("spare-%d", i))
		mtime := fixedTestNow.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(filepath.Join(paths.Runtime, "generations", id), mtime, mtime); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

func TestHarnessGCRemovesOrphanGenerationAndReportsCounts(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	deps := harnessDeps{poolPaths: func() pool.PoolPaths { return paths }}

	ids := seedOrphanGenerationSpares(t, paths, "claude", pool.PlatformLinuxAMD64Glibc, 3)

	out, err := runHarnessCmdWithDeps(t, deps, "gc")
	if err != nil {
		t.Fatalf("gc: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "removed 1 generation") {
		t.Errorf("expected gc's output to report 1 removed generation, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(paths.Runtime, "generations", ids[0])); !os.IsNotExist(err) {
		t.Errorf("expected the OLDEST orphan generation to be removed, stat err = %v", err)
	}
	for _, id := range ids[1:] {
		if _, err := os.Stat(filepath.Join(paths.Runtime, "generations", id)); err != nil {
			t.Errorf("expected the newer orphan generation %s to survive gc (top-2 retention): %v", id, err)
		}
	}
}

func TestHarnessGCDryRunRemovesNothing(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	deps := harnessDeps{poolPaths: func() pool.PoolPaths { return paths }}

	ids := seedOrphanGenerationSpares(t, paths, "claude", pool.PlatformLinuxAMD64Glibc, 3)

	out, err := runHarnessCmdWithDeps(t, deps, "gc", "--dry-run")
	if err != nil {
		t.Fatalf("gc --dry-run: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "would remove 1 generation") {
		t.Errorf("expected dry-run gc's output to say 'would remove', got:\n%s", out)
	}
	if !strings.Contains(out, "would free") {
		t.Errorf("expected dry-run gc's output to say 'would free' (not 'freed'), got:\n%s", out)
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(paths.Runtime, "generations", id)); err != nil {
			t.Errorf("dry run must not remove any generation (including %s, which a real run WOULD remove): %v", id, err)
		}
	}
}

// gcTestLegacyBinLib builds real, populated legacy bin/+lib/ directories
// under a fresh t.TempDir()-rooted pool root (the shape ensureHarnessPool
// produces), for `gc --legacy` command-level tests.
func gcTestLegacyBinLib(t *testing.T) (poolDir, poolBin, poolLib string) {
	t.Helper()
	poolDir = t.TempDir()
	poolBin = filepath.Join(poolDir, "bin")
	poolLib = filepath.Join(poolDir, "lib")
	if err := os.MkdirAll(poolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(poolLib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolBin, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return poolDir, poolBin, poolLib
}

// gcTestLegacyHarnessDeps builds a harnessDeps for `gc --legacy` cmd-level
// tests: poolPaths returns paths (LegacyRemovedMarker included, via
// testHarnessPaths), and poolLegacyDirs — NOT poolBinDir, review round 2's
// minor 5 finding — returns poolBin/poolLib as plain, non-creating path
// accessors, exactly mirroring newHarnessGCCmd's own production wiring
// (defaultHarnessDeps.poolLegacyDirs). poolBinDir is intentionally left
// unset here: a test asserting gc's cmd wiring never touches the
// ensureHarnessPool-backed accessor would otherwise pass vacuously if that
// field were still faked out identically.
func gcTestLegacyHarnessDeps(paths pool.PoolPaths, poolBin, poolLib string) harnessDeps {
	return harnessDeps{
		poolPaths:      func() pool.PoolPaths { return paths },
		poolLegacyDirs: func() (string, string) { return poolBin, poolLib },
	}
}

// TestHarnessGCLegacyRemovesLegacyDirs wires `gc --legacy` end to end
// through the real cobra command: a manifest-declared, pool-lockable
// harness with an active generation for the host's own platform passes the
// refusal check, so the legacy bin/+lib/ trees are removed and the report
// says so (§16 Phase 4 item 5).
func TestHarnessGCLegacyRemovesLegacyDirs(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Skipf("HostPlatformKey unsupported on this test host: %v", err)
	}
	_, poolBin, poolLib := gcTestLegacyBinLib(t)

	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  claude: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	genID := seedOrphanGeneration(t, paths, "claude", "2.1.220", hostKey, "claude-bin")
	if err := pool.ActivateGeneration(paths, "claude", hostKey, genID); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}

	deps := gcTestLegacyHarnessDeps(paths, poolBin, poolLib)

	out, err := runHarnessCmdWithDeps(t, deps, "gc", "--legacy")
	if err != nil {
		t.Fatalf("gc --legacy: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "removed the legacy bin/+lib/ trees") {
		t.Errorf("expected the report to mention legacy removal, got:\n%s", out)
	}
	for _, dir := range []string{poolBin, poolLib} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, stat err = %v", dir, err)
		}
	}
	if _, err := os.Stat(paths.LegacyRemovedMarker); err != nil {
		t.Errorf("expected the legacy-removed marker to be written: %v", err)
	}
}

// TestHarnessGCLegacyDryRunReportsWithoutRemoving mirrors the non-legacy
// dry-run test above for --legacy specifically: "would remove", nothing
// actually gone.
func TestHarnessGCLegacyDryRunReportsWithoutRemoving(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Skipf("HostPlatformKey unsupported on this test host: %v", err)
	}
	_, poolBin, poolLib := gcTestLegacyBinLib(t)

	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  claude: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	genID := seedOrphanGeneration(t, paths, "claude", "2.1.220", hostKey, "claude-bin")
	if err := pool.ActivateGeneration(paths, "claude", hostKey, genID); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}

	deps := gcTestLegacyHarnessDeps(paths, poolBin, poolLib)

	out, err := runHarnessCmdWithDeps(t, deps, "gc", "--legacy", "--dry-run")
	if err != nil {
		t.Fatalf("gc --legacy --dry-run: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "would remove the legacy bin/+lib/ trees") {
		t.Errorf("expected the report to say 'would remove' the legacy trees, got:\n%s", out)
	}
	for _, dir := range []string{poolBin, poolLib} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("dry run must not remove %s: %v", dir, err)
		}
	}
}

// TestHarnessGCLegacyRefusedNamesUnrealizedHarness pins the RULING: a
// manifest-declared, pool-lockable harness with NO active generation for
// this host's platform refuses the whole `gc --legacy` call (an error, not
// a warning), naming the harness and the fix — and removes nothing at all,
// including the legacy trees the command was asked to remove.
func TestHarnessGCLegacyRefusedNamesUnrealizedHarness(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	if _, err := pool.HostPlatformKey(); err != nil {
		t.Skipf("HostPlatformKey unsupported on this test host: %v", err)
	}
	_, poolBin, poolLib := gcTestLegacyBinLib(t)

	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  claude: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// claude is declared but never activated for any platform.

	deps := gcTestLegacyHarnessDeps(paths, poolBin, poolLib)

	out, err := runHarnessCmdWithDeps(t, deps, "gc", "--legacy")
	if err == nil {
		t.Fatalf("expected gc --legacy to be refused, output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error = %q, want it to name the unrealized harness", err)
	}
	if !strings.Contains(err.Error(), "wraptool harness install claude") {
		t.Errorf("error = %q, want it to name the fix command", err)
	}
	for _, dir := range []string{poolBin, poolLib} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("a refused --legacy must not remove %s: %v", dir, err)
		}
	}
}

// TestHarnessGCLegacyRefusedRunLeavesAlreadyGCdPoolUntouched is review round
// 2's minor 5: gc's cmd wiring (newHarnessGCCmd) must never resurrect an
// already-gc'd pool merely by reading its own legacy paths, REGARDLESS of
// whether this particular --legacy attempt is refused. Simulates a pool
// already gc'd (marker present, bin/+lib/ absent — as gcLegacy itself would
// leave it) with a SEPARATE, unrelated harness now unrealized (so this
// --legacy attempt is refused); the marker and the dirs' absence must both
// survive the refused attempt exactly as they were.
func TestHarnessGCLegacyRefusedRunLeavesAlreadyGCdPoolUntouched(t *testing.T) {
	t.Parallel()
	paths := testHarnessPaths(t)
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Skipf("HostPlatformKey unsupported on this test host: %v", err)
	}
	root := t.TempDir()
	poolBin, poolLib := filepath.Join(root, "bin"), filepath.Join(root, "lib") // never created — already gc'd
	if err := os.WriteFile(paths.LegacyRemovedMarker, []byte("2026-01-01T00:00:00Z\nremoved by `wraptool harness gc --legacy`\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  claude: {}\n  opencode: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// claude is realized/active; opencode is not — this run must be refused.
	genID := seedOrphanGeneration(t, paths, "claude", "2.1.220", hostKey, "claude-bin")
	if err := pool.ActivateGeneration(paths, "claude", hostKey, genID); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}

	deps := gcTestLegacyHarnessDeps(paths, poolBin, poolLib)

	out, err := runHarnessCmdWithDeps(t, deps, "gc", "--legacy")
	if err == nil {
		t.Fatalf("expected gc --legacy to be refused (opencode unrealized), output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "opencode") {
		t.Errorf("error = %q, want it to name opencode", err)
	}
	for _, dir := range []string{poolBin, poolLib} {
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Errorf("a refused --legacy run must not have created %s (pool was already gc'd): %v", dir, statErr)
		}
	}
	if _, statErr := os.Stat(paths.LegacyRemovedMarker); statErr != nil {
		t.Errorf("legacy-removed marker must survive a refused run: %v", statErr)
	}
}

// --- pool-source-wiring: source selection per descriptor (§11.3) ---

// TestDefaultHarnessDepsSelectsSourcePerDescriptor pins defaultHarnessDeps'
// newSource dispatch: an archive-sourced descriptor (agy, PoolArchive=true)
// must get the archive Source, everything else the npm one.
func TestDefaultHarnessDepsSelectsSourcePerDescriptor(t *testing.T) {
	deps := defaultHarnessDeps()

	npmSrc, err := deps.newSource(harness.Registry["claude"])
	if err != nil {
		t.Fatalf("newSource(claude): %v", err)
	}
	if _, ok := npmSrc.(*source.NPM); !ok {
		t.Errorf("newSource(claude) = %T, want *source.NPM", npmSrc)
	}

	archiveSrc, err := deps.newSource(harness.Registry["agy"])
	if err != nil {
		t.Fatalf("newSource(agy): %v", err)
	}
	if _, ok := archiveSrc.(*source.Archive); !ok {
		t.Errorf("newSource(agy) = %T, want *source.Archive", archiveSrc)
	}
}

// TestPoolSourceKind pins poolSourceKind's dispatch, the cmd-level
// counterpart of newSource's own per-descriptor selection.
func TestPoolSourceKind(t *testing.T) {
	if got := poolSourceKind(harness.Registry["claude"]); got != pool.SourceNPM {
		t.Errorf("poolSourceKind(claude) = %q, want %q", got, pool.SourceNPM)
	}
	if got := poolSourceKind(harness.Registry["opencode"]); got != pool.SourceNPM {
		t.Errorf("poolSourceKind(opencode) = %q, want %q", got, pool.SourceNPM)
	}
	if got := poolSourceKind(harness.Registry["agy"]); got != pool.SourceArchive {
		t.Errorf("poolSourceKind(agy) = %q, want %q", got, pool.SourceArchive)
	}
}

// TestHarnessBackendSkipsResolveBackendForArchiveSource pins the §6.2
// gate directly: harnessBackend must never call deps.resolveBackend for an
// archive-sourced descriptor (agy), and must always call it — returning
// whatever it returns — for every other (npm-sourced) descriptor.
func TestHarnessBackendSkipsResolveBackendForArchiveSource(t *testing.T) {
	called := false
	deps := harnessDeps{
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			called = true
			return fakeHarnessBackend{}, nil
		},
	}

	b, err := harnessBackend(deps, harness.Registry["agy"], hostPlatformTarget)
	if err != nil {
		t.Fatalf("harnessBackend(agy): %v", err)
	}
	if b != nil {
		t.Errorf("harnessBackend(agy) = %v, want nil (archive sources need no backend)", b)
	}
	if called {
		t.Error("deps.resolveBackend must not be called for an archive-sourced harness (agy)")
	}

	called = false
	if _, err := harnessBackend(deps, harness.Registry["claude"], hostPlatformTarget); err != nil {
		t.Fatalf("harnessBackend(claude): %v", err)
	}
	if !called {
		t.Error("deps.resolveBackend must be called for an npm-sourced harness (claude)")
	}
}

// TestHarnessUpdateDedupsBackendProbeAcrossNPMHarnesses pins backendCache's
// memoization (review round 2, nit): a bare `update` touching two
// npm-sourced harnesses in one run must call deps.resolveBackend exactly
// once, not once per harness — the guix-absent error message would
// otherwise print (or, here, the probe itself run) once per harness.
func TestHarnessUpdateDedupsBackendProbeAcrossNPMHarnesses(t *testing.T) {
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
			pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}

	src := &fakeHarnessSource{}
	backendCalls := 0
	binDir := t.TempDir()
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return src, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			backendCalls++
			return fakeHarnessBackend{}, nil
		},
	}

	out, err := runHarnessCmdWithDeps(t, deps, "update")
	if err != nil {
		t.Fatalf("update: %v\noutput:\n%s", err, out)
	}
	if backendCalls != 1 {
		t.Errorf("resolveBackend called %d times across 2 npm-sourced harnesses, want exactly 1 (memoized)", backendCalls)
	}
}

// TestDefaultResolveBackendSelectionMatrix pins §11.2's auto backend policy
// end to end through the real defaultResolveBackend: guix present/absent x
// docker-or-podman present/absent x host-arch/cross-arch target -> which
// backend gets selected. The oci case additionally confirms the pinned
// provisioningImage constant is what actually gets wired in (via
// backend.Select's own requireOCIImage guard: an empty image would error,
// so a nil error here already proves a non-empty image reached Select).
// errKindAbsent/errKindCrossArch are TestDefaultResolveBackendSelectionMatrix's
// labels for defaultResolveBackend's two distinct "no backend at all" error
// shapes: guix outright absent (or present but irrelevant because no
// candidate backend exists at all) vs. guix PRESENT but unable to
// cross-build for the requested target. Attributing the wrong one would
// blame guix's cross-arch limitation on it being "missing", or vice versa.
const (
	errKindNone      = "" // success — a backend was selected
	errKindAbsent    = "absent"
	errKindCrossArch = "crossarch"
)

func TestDefaultResolveBackendSelectionMatrix(t *testing.T) {
	cases := []struct {
		name     string
		guix     bool
		oci      bool
		hostArch string
		target   pool.PlatformKey
		want     string // backend.Backend.Name(), or "" (errKindNone) when errKind is set
		errKind  string // errKindAbsent/errKindCrossArch when want == ""
	}{
		{
			name: "guix available, host-arch target -> guix",
			guix: true, oci: true, hostArch: "amd64", target: pool.PlatformLinuxAMD64Glibc,
			want: "guix",
		},
		{
			name: "guix available, host-arch target, oci absent -> guix (guix doesn't need oci)",
			guix: true, oci: false, hostArch: "amd64", target: pool.PlatformLinuxAMD64Glibc,
			want: "guix",
		},
		{
			name: "guix available but wrong arch, oci available -> oci (cross-arch)",
			guix: true, oci: true, hostArch: "amd64", target: pool.PlatformLinuxARM64Glibc,
			want: "oci",
		},
		{
			name: "guix absent, oci available -> oci",
			guix: false, oci: true, hostArch: "amd64", target: pool.PlatformLinuxAMD64Glibc,
			want: "oci",
		},
		{
			name: "guix absent, oci absent -> no backend at all (absent-guix wording)",
			guix: false, oci: false, hostArch: "amd64", target: pool.PlatformLinuxAMD64Glibc,
			errKind: errKindAbsent,
		},
		{
			name: "guix available but wrong arch, oci absent -> no backend at all (cross-arch wording)",
			guix: true, oci: false, hostArch: "amd64", target: pool.PlatformLinuxARM64Glibc,
			errKind: errKindCrossArch,
		},
		{
			name: "musl target routes the same as its glibc arch sibling (deferral happens later, at realize)",
			guix: true, oci: true, hostArch: "arm64", target: pool.PlatformLinuxARM64Musl,
			want: "guix",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapHasGuix(t, tc.guix)
			swapOCIAvailable(t, tc.oci)
			swapHostArch(t, tc.hostArch)

			b, err := defaultResolveBackend(tc.target)
			switch tc.errKind {
			case errKindAbsent:
				if err == nil {
					t.Fatalf("expected an error, got backend %q", b.Name())
				}
				for _, want := range []string{"install guix", "install docker or podman"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("no-backend error = %q, want it to contain %q", err.Error(), want)
					}
				}
				if strings.Contains(err.Error(), "cross-build") {
					t.Errorf("guix-absent error unexpectedly used the cross-arch wording: %q", err.Error())
				}
				return
			case errKindCrossArch:
				if err == nil {
					t.Fatalf("expected an error, got backend %q", b.Name())
				}
				for _, want := range []string{"cannot cross-build", string(tc.target), tc.hostArch, "install docker or podman"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("cross-arch error = %q, want it to contain %q", err.Error(), want)
					}
				}
				if strings.Contains(err.Error(), "install guix") {
					t.Errorf("cross-arch error wrongly attributes the failure to guix being absent (guix IS present): %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("defaultResolveBackend: %v", err)
			}
			if got := b.Name(); got != tc.want {
				t.Errorf("backend = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultResolveBackendNoBackendErrorNamesAllThreeOptions pins the exact
// §16 item 5 wording this work package retires the old guix-absent-only
// error for: the error must fire ONLY when no backend exists at all (the
// matrix test above already covers the "guix present but wrong arch, oci
// absent" case, which is also a no-backend-at-all outcome), name all three
// installable options, retain the devcontainer-feature fallback guidance,
// and drop the old "Phase 3 pending" phrasing — this work package IS Phase 3.
func TestDefaultResolveBackendNoBackendErrorNamesAllThreeOptions(t *testing.T) {
	swapHasGuix(t, false)
	swapOCIAvailable(t, false)

	_, err := defaultResolveBackend(pool.PlatformLinuxAMD64Glibc)
	if err == nil {
		t.Fatal("expected an error when neither guix nor an oci engine is available")
	}
	msg := err.Error()
	for _, want := range []string{"install guix", "install docker or podman", "npm-sourced harnesses need a backend"} {
		if !strings.Contains(msg, want) {
			t.Errorf("no-backend error = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "Phase 3") {
		t.Errorf("no-backend error still carries the retired Phase-3-pending phrasing: %q", msg)
	}
	if !strings.Contains(msg, `"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": "<name>"}`) {
		t.Errorf("no-backend error dropped the devcontainer-feature fallback guidance: %q", msg)
	}
}

// TestResolveBackendForFlag exercises install/update/sync's shared --backend
// flag resolution: "auto" (default) defers to the resolveBackend it's given
// unchanged; an explicit "guix" or "oci" bypasses that policy and either
// succeeds or fails outright.
func TestResolveBackendForFlag(t *testing.T) {
	t.Run("auto defers to the given resolveBackend", func(t *testing.T) {
		called := false
		fake := func(pool.PlatformKey) (backend.Backend, error) {
			called = true
			return fakeHarnessBackend{}, nil
		}
		if _, err := resolveBackendForFlag("auto", pool.PlatformLinuxAMD64Glibc, fake); err != nil {
			t.Fatalf("resolveBackendForFlag: %v", err)
		}
		if !called {
			t.Error("\"auto\" must call the given resolveBackend")
		}
	})

	t.Run("empty preference behaves as auto", func(t *testing.T) {
		called := false
		fake := func(pool.PlatformKey) (backend.Backend, error) {
			called = true
			return fakeHarnessBackend{}, nil
		}
		if _, err := resolveBackendForFlag("", pool.PlatformLinuxAMD64Glibc, fake); err != nil {
			t.Fatalf("resolveBackendForFlag: %v", err)
		}
		if !called {
			t.Error("empty preference must call the given resolveBackend, same as \"auto\"")
		}
	})

	t.Run("explicit guix with guix absent errors immediately", func(t *testing.T) {
		swapHostArch(t, "amd64")
		swapHasGuix(t, false)
		_, err := resolveBackendForFlag("guix", pool.PlatformLinuxAMD64Glibc, nil)
		if err == nil {
			t.Fatal("resolveBackendForFlag: expected an error with guix absent")
		}
		if !strings.Contains(err.Error(), "not found on PATH") || !strings.Contains(err.Error(), "guix") {
			t.Errorf("error = %q, want it to name guix and PATH", err)
		}
	})

	t.Run("explicit guix on a matching-arch target succeeds and never consults resolveBackend", func(t *testing.T) {
		swapHostArch(t, "amd64")
		swapHasGuix(t, true)
		called := false
		fake := func(pool.PlatformKey) (backend.Backend, error) {
			called = true
			return nil, errors.New("must not be called")
		}
		b, err := resolveBackendForFlag("guix", pool.PlatformLinuxAMD64Glibc, fake)
		if err != nil {
			t.Fatalf("resolveBackendForFlag: %v", err)
		}
		if got := b.Name(); got != "guix" {
			t.Errorf("backend = %q, want %q", got, "guix")
		}
		if called {
			t.Error("an explicit --backend must not consult the auto-policy resolveBackend")
		}
	})

	t.Run("explicit guix on a cross-arch target errors with the cross-build message", func(t *testing.T) {
		swapHostArch(t, "amd64")
		fake := func(pool.PlatformKey) (backend.Backend, error) { return fakeHarnessBackend{}, nil }
		_, err := resolveBackendForFlag("guix", pool.PlatformLinuxARM64Glibc, fake)
		if err == nil {
			t.Fatal("expected an error: guix cannot cross-build")
		}
		for _, want := range []string{"cannot cross-build", "linux-arm64-glibc", "amd64", "install docker or podman"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), want)
			}
		}
	})

	t.Run("explicit oci with an engine available succeeds", func(t *testing.T) {
		swapOCIAvailable(t, true)
		fake := func(pool.PlatformKey) (backend.Backend, error) { return nil, errors.New("must not be called") }
		b, err := resolveBackendForFlag("oci", pool.PlatformLinuxAMD64Glibc, fake)
		if err != nil {
			t.Fatalf("resolveBackendForFlag: %v", err)
		}
		if got := b.Name(); got != "oci" {
			t.Errorf("backend = %q, want %q", got, "oci")
		}
	})

	t.Run("explicit oci with no engine errors naming docker and podman", func(t *testing.T) {
		swapOCIAvailable(t, false)
		fake := func(pool.PlatformKey) (backend.Backend, error) { return fakeHarnessBackend{}, nil }
		_, err := resolveBackendForFlag("oci", pool.PlatformLinuxAMD64Glibc, fake)
		if err == nil {
			t.Fatal("expected an error: no oci engine available")
		}
		for _, want := range []string{"docker", "podman"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), want)
			}
		}
	})

	t.Run("unknown preference errors", func(t *testing.T) {
		fake := func(pool.PlatformKey) (backend.Backend, error) { return fakeHarnessBackend{}, nil }
		if _, err := resolveBackendForFlag("bogus", pool.PlatformLinuxAMD64Glibc, fake); err == nil {
			t.Error("expected an error for an unrecognized --backend value")
		}
	})
}

// TestHarnessInstallBackendFlagReachesResolveBackendForFlag pins
// withBackendFlag's wiring into `install` specifically — that the --backend
// CLI flag actually reaches resolveBackendForFlag, not just that
// resolveBackendForFlag behaves correctly in isolation (TestResolveBackendForFlag
// above). hostArch is stubbed to a value that can never equal a real
// PlatformKey's arch ("amd64"/"arm64"), so install's target (always the
// real host's own platform, via pool.HostPlatformKey — independent of the
// hostArch stub) can never "match" under --backend=guix, deterministically
// regardless of the test runner's actual architecture: this only produces
// the cross-build error if the CLI flag genuinely reached
// resolveBackendForFlag's guix branch.
func TestHarnessInstallBackendFlagReachesResolveBackendForFlag(t *testing.T) {
	swapHasGuix(t, true)
	swapHostArch(t, "unobtainium")

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	deps := testHarnessDeps(paths, t.TempDir(), src) // resolveBackend fake must never be reached

	out, err := runHarnessCmdWithDeps(t, deps, "install", "claude", "--constraint", ">=2 <3", "--backend", "guix")
	if err == nil {
		t.Fatalf("expected an error (guix cannot cross-build under the stubbed host arch), got success:\n%s", out)
	}
	if !strings.Contains(err.Error(), "cannot cross-build") {
		t.Errorf("error = %q, want it to contain %q (proving --backend=guix reached resolveBackendForFlag)", err.Error(), "cannot cross-build")
	}
}

// TestHarnessUpdateAndSyncBackendFlagReachResolveBackendForFlag is
// TestHarnessInstallBackendFlagReachesResolveBackendForFlag's `update` and
// `sync` counterpart — same stubbed-arch technique, proving withBackendFlag
// is wired into all three commands, not just install.
func TestHarnessUpdateAndSyncBackendFlagReachResolveBackendForFlag(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	deps := testHarnessDeps(paths, t.TempDir(), src)

	// A normal install first (hostArch unstubbed, so this succeeds
	// normally), giving update/sync a lock entry to operate on.
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", harness.Registry["claude"], ">=2 <3", false, ""); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	t.Run("update", func(t *testing.T) {
		swapHostArch(t, "unobtainium")
		out, err := runHarnessCmdWithDeps(t, deps, "update", "claude", "--backend", "guix")
		if err == nil {
			t.Fatalf("expected an error (guix cannot cross-build under the stubbed host arch), got success:\n%s", out)
		}
		if !strings.Contains(out, "cannot cross-build") {
			t.Errorf("output = %q, want it to contain %q", out, "cannot cross-build")
		}
	})

	t.Run("sync", func(t *testing.T) {
		swapHostArch(t, "unobtainium")
		hostKey, err := pool.HostPlatformKey()
		if err != nil {
			t.Fatalf("HostPlatformKey: %v", err)
		}
		out, err := runHarnessCmdWithDeps(t, deps, "sync", "claude", "--platform", string(hostKey), "--backend", "guix")
		if err == nil {
			t.Fatalf("expected an error (guix cannot cross-build under the stubbed host arch), got success:\n%s", out)
		}
		if !strings.Contains(out, "cannot cross-build") {
			t.Errorf("output = %q, want it to contain %q", out, "cannot cross-build")
		}
	})
}

// spyBackend is a minimal backend.Backend test double that records every
// RunSpec it's asked to Run, for a cmd-level test to inspect (e.g.
// RunSpec.Platform) without a real container engine.
type spyBackend struct {
	name  string
	specs []backend.RunSpec
}

func (s *spyBackend) Name() string    { return s.name }
func (s *spyBackend) Available() bool { return true }
func (s *spyBackend) Run(_ context.Context, spec backend.RunSpec) error {
	s.specs = append(s.specs, spec)
	return nil
}

// TestPrintRealizeWarningNamesImageForOCIBackendFailure pins
// realizeErrorHint: a realize failure that ran against the oci backend must
// have its warning name the pinned provisioning image and mention
// architecture support, so an arch-mismatch container-engine error (which
// otherwise surfaces as an opaque message like "exec format error") is
// diagnosable without a network preflight check. The same failure against a
// non-oci backend must NOT carry that hint — it would be actively
// misleading for a backend where an image was never involved.
func TestPrintRealizeWarningNamesImageForOCIBackendFailure(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	runInstall := func(t *testing.T, b backend.Backend) string {
		t.Helper()
		paths := testHarnessPaths(t)
		binDir := t.TempDir()
		claudePkg := harness.Registry["claude"].PoolPackage
		src := &fakeHarnessSource{
			candidates: map[string][]pool.Candidate{
				claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
			},
			failRealize: true,
		}
		deps := harnessDeps{
			poolPaths:      func() pool.PoolPaths { return paths },
			poolBinDir:     func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
			newSource:      func(harness.Descriptor) (pool.Source, error) { return src, nil },
			resolveBackend: func(pool.PlatformKey) (backend.Backend, error) { return b, nil },
		}

		var buf bytes.Buffer
		cmd := bareHarnessCmd()
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		if err := runHarnessInstall(context.Background(), cmd, deps, "claude", harness.Registry["claude"], ">=2 <3", false, ""); err != nil {
			t.Fatalf("runHarnessInstall: %v", err)
		}
		return buf.String()
	}

	t.Run("oci backend names the pinned image", func(t *testing.T) {
		out := runInstall(t, &spyBackend{name: "oci"})
		if !strings.Contains(out, "generation realization failed") {
			t.Fatalf("expected the realize-failure warning, got:\n%s", out)
		}
		if !strings.Contains(out, provisioningImage) {
			t.Errorf("expected the warning to name the pinned image %q, got:\n%s", provisioningImage, out)
		}
		if !strings.Contains(out, "architecture") {
			t.Errorf("expected the warning to mention architecture support, got:\n%s", out)
		}
	})

	t.Run("guix backend carries no image hint", func(t *testing.T) {
		out := runInstall(t, &spyBackend{name: "guix"})
		if !strings.Contains(out, "generation realization failed") {
			t.Fatalf("expected the realize-failure warning, got:\n%s", out)
		}
		if strings.Contains(out, provisioningImage) {
			t.Errorf("guix-backend warning unexpectedly named the oci provisioning image, got:\n%s", out)
		}
	})
}

// crossArchRecordingSource wraps fakeHarnessSource's ordinary behavior but
// makes Realize additionally invoke b.Run with a RunSpec.Platform derived
// from key via targetArch — mirroring what the real npm Source
// (internal/pool/source/realize.go's platformArch) actually does — so a
// cmd-level test can observe, through the injected backend, which arch the
// realize phase would have targeted.
type crossArchRecordingSource struct {
	fakeHarnessSource
}

func (s *crossArchRecordingSource) Realize(ctx context.Context, b backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, cacheDir, stagingGenDir string) (*pool.Receipt, error) {
	arch, err := targetArch(key)
	if err != nil {
		return nil, err
	}
	if err := b.Run(ctx, backend.RunSpec{Argv: []string{"true"}, Platform: arch}); err != nil {
		return nil, err
	}
	return s.fakeHarnessSource.Realize(ctx, b, harnessName, binName, h, key, cacheDir, stagingGenDir)
}

// TestHarnessSyncCrossArchThreadsTargetPlatformIntoRealize pins the wiring
// §11.2's cross-arch case depends on: `sync --platform linux-arm64-glibc`
// must ask deps.resolveBackend for exactly that target (never the host's
// own platform, even though install/update always do) and the backend it
// gets back must be the one Source.Realize actually runs its container step
// against, with RunSpec.Platform carrying "arm64" — the same threading
// defaultResolveBackend's cross-arch routing (tested directly above) relies
// on to reach the oci backend in the first place.
func TestHarnessSyncCrossArchThreadsTargetPlatformIntoRealize(t *testing.T) {
	swapHasGuix(t, true) // claude's legacy install recipe starts with "guix"
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	binDir := t.TempDir()

	// install first (ordinary fake backend, whatever the host's own
	// platform is) so a lock entry exists for sync to realize against.
	installDeps := testHarnessDeps(paths, binDir, src)
	d := harness.Registry["claude"]
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), installDeps, "claude", d, ">=2 <3", false, ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	var gotTargets []pool.PlatformKey
	spy := &spyBackend{name: "oci"}
	crossSrc := &crossArchRecordingSource{fakeHarnessSource: *src}
	crossDeps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return crossSrc, nil },
		resolveBackend: func(target pool.PlatformKey) (backend.Backend, error) {
			gotTargets = append(gotTargets, target)
			return spy, nil
		},
	}

	out, err := runHarnessCmdWithDeps(t, crossDeps, "sync", "claude", "--platform", "linux-arm64-glibc")
	if err != nil {
		t.Fatalf("sync --platform: %v\noutput:\n%s", err, out)
	}

	if len(gotTargets) != 1 || gotTargets[0] != pool.PlatformLinuxARM64Glibc {
		t.Errorf("resolveBackend targets = %v, want exactly [linux-arm64-glibc] — sync must thread its "+
			"--platform value through, never the host's own platform", gotTargets)
	}
	if len(spy.specs) == 0 {
		t.Fatal("expected the injected backend's Run to have been called by Realize")
	}
	if got := spy.specs[0].Platform; got != "arm64" {
		t.Errorf("RunSpec.Platform = %q, want %q", got, "arm64")
	}
}

// crossArchTargetKey returns a pool.PlatformKey guaranteed to differ from
// this test host's own HostPlatformKey() — the opposite glibc arch — so a
// test can request a genuinely cross-platform `sync --platform` regardless
// of which architecture actually runs the test binary.
func crossArchTargetKey(t *testing.T) pool.PlatformKey {
	t.Helper()
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Fatalf("HostPlatformKey: %v", err)
	}
	if hostKey == pool.PlatformLinuxARM64Glibc {
		return pool.PlatformLinuxAMD64Glibc
	}
	return pool.PlatformLinuxARM64Glibc
}

// TestHarnessSyncSkipsLegacyInstallForCrossPlatformTarget pins that a
// cross-platform `sync --platform` (one that names something other than
// this host's own platform) must perform ZERO runInstallArgv calls — the
// realized generation is a foreign-platform artifact, and mutating the
// host's own legacy bin/ with it is out of scope — while an ordinary
// host-platform sync keeps installing exactly as before (regression guard,
// same subtest so both behaviors are pinned side by side).
func TestHarnessSyncSkipsLegacyInstallForCrossPlatformTarget(t *testing.T) {
	swapHasGuix(t, true) // claude's legacy install recipe starts with "guix"
	installCalls := 0
	swapRunInstallArgv(t, func([]string) error { installCalls++; return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	deps := testHarnessDeps(paths, t.TempDir(), src)

	d := harness.Registry["claude"]
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, ">=2 <3", false, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	installCalls = 0 // reset: only interested in sync's own install calls below

	t.Run("cross-platform sync installs nothing", func(t *testing.T) {
		crossKey := crossArchTargetKey(t)
		out, err := runHarnessCmdWithDeps(t, deps, "sync", "claude", "--platform", string(crossKey))
		if err != nil {
			t.Fatalf("sync --platform %s: %v\noutput:\n%s", crossKey, err, out)
		}
		if installCalls != 0 {
			t.Errorf("runInstallArgv called %d times for a cross-platform sync, want 0", installCalls)
		}
		if !strings.Contains(out, "skipping host-side legacy install") {
			t.Errorf("expected sync's output to explain the skip, got:\n%s", out)
		}
	})

	t.Run("host-platform sync still installs", func(t *testing.T) {
		installCalls = 0
		hostKey, err := pool.HostPlatformKey()
		if err != nil {
			t.Fatalf("HostPlatformKey: %v", err)
		}
		out, err := runHarnessCmdWithDeps(t, deps, "sync", "claude", "--platform", string(hostKey))
		if err != nil {
			t.Fatalf("sync --platform %s: %v\noutput:\n%s", hostKey, err, out)
		}
		if installCalls != 1 {
			t.Errorf("runInstallArgv called %d times for a host-platform sync, want 1", installCalls)
		}
	})
}

// TestHarnessInstallFoldsProvisioningImageIntoRecipeRevision pins that the
// provisioningImage var must actually reach the persisted
// harness-lock.json recipe_revision, not just the in-memory UpdateOptions
// value some other test already checks. Two installs of the
// SAME harness, differing only in provisioningImage between them, must
// persist two DIFFERENT recipe_revision values — install's own
// Advance=false short-circuit does not suppress this: a recipe_revision
// mismatch against the persisted lock defeats the short-circuit's
// lockMatchesRecipe precondition (decision.go), so the second install still
// re-resolves under the new image. This is deliberately an end-to-end
// (cmd -> orchestrator -> lock file) test, not just a unit test of
// npmRecipeRevision's hashing (that lives in internal/pool/orchestrator_test.go)
// — it is what actually catches a wiring regression like the second
// install's `ProvisioningImage: provisioningImage` field going missing from
// the UpdateOptions literal.
func TestHarnessInstallFoldsProvisioningImageIntoRecipeRevision(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	deps := testHarnessDeps(paths, t.TempDir(), src)
	d := harness.Registry["claude"]

	swapProvisioningImage(t, "forge.example/pool-provisioning@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, ">=2 <3", false, ""); err != nil {
		t.Fatalf("first install: %v", err)
	}
	revA := readLockRecipeRevision(t, paths.Lock, "claude")
	if revA == "" {
		t.Fatal("first install persisted an empty recipe_revision")
	}

	swapProvisioningImage(t, "forge.example/pool-provisioning@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, "", false, ""); err != nil {
		t.Fatalf("second install (after provisioningImage changed): %v", err)
	}
	revB := readLockRecipeRevision(t, paths.Lock, "claude")

	if revA == revB {
		t.Fatalf("recipe_revision unchanged (%q) after provisioningImage changed between installs — "+
			"the image reference is not reaching harness-lock.json's recipe_revision", revA)
	}
}

// TestHarnessUpdateFoldsProvisioningImageIntoRecipeRevision pins update's own
// UpdateOptions.ProvisioningImage wiring (newHarnessUpdateCmd), separately
// from install's (the sibling test above): an `update` of an already-locked
// harness, with provisioningImage UNCHANGED since install, must leave
// recipe_revision unchanged too — decision.go's lockMatchesRecipe short-
// circuit depends on update recomputing the SAME recipe_revision install
// persisted. If update's UpdateOptions literal ever drops
// ProvisioningImage, the freshly computed recipe_revision reverts to the
// empty-image hash, no longer matches the persisted one, and update
// force-resolves every single time it runs — alternating install/update
// would then ping-pong recipe_revision between the two values on every
// call, never settling.
func TestHarnessUpdateFoldsProvisioningImageIntoRecipeRevision(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })

	paths := testHarnessPaths(t)
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	deps := testHarnessDeps(paths, t.TempDir(), src)
	d := harness.Registry["claude"]

	swapProvisioningImage(t, "forge.example/pool-provisioning@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", d, ">=2 <3", false, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	revInstall := readLockRecipeRevision(t, paths.Lock, "claude")
	if revInstall == "" {
		t.Fatal("install persisted an empty recipe_revision")
	}

	// No swapProvisioningImage call here: the var is left exactly as install
	// used it.
	if out, err := runHarnessCmdWithDeps(t, deps, "update", "claude"); err != nil {
		t.Fatalf("update: %v\noutput:\n%s", err, out)
	}
	revUpdate := readLockRecipeRevision(t, paths.Lock, "claude")

	if revUpdate != revInstall {
		t.Fatalf("recipe_revision changed on update (%q -> %q) with an unchanged provisioningImage — "+
			"update is not folding the same image install used", revInstall, revUpdate)
	}
}

// TestProvisioningImageIsDigestPinned asserts the reference has the
// name@sha256:<64 hex> shape §11.2 requires (a digest, not a floating tag)
// and is registry-qualified (podman has no unqualified-search default, so a
// bare "node@sha256:..." would fail there).
func TestProvisioningImageIsDigestPinned(t *testing.T) {
	if !regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}/[^@]+@sha256:[0-9a-f]{64}$`).MatchString(defaultProvisioningImage) {
		t.Errorf("defaultProvisioningImage = %q, want <registry>/<name>@sha256:<64 lowercase hex digest>", defaultProvisioningImage)
	}
}

// fakeArchiveSource is a minimal pool.Source test double simulating an
// archive-sourced harness (agy) — the cmd-package analogue of
// fakeHarnessSource, but without npm's Runtime/NPMLock plumbing (an
// archive-sourced LockHarness carries neither — internal/pool/source/
// archive.go's own doc comment). Its Resolve deliberately mirrors the real
// Archive's own zero-formatted PublishedAt/FirstSeenAt (see that method's
// doc comment) so this fake also exercises the PublishedAt-synthesis path
// end to end through cmd, not just through internal/pool's own unit tests.
// Real HTTP fixtures for archive.go's actual Refresh/Resolve/Fetch are
// covered by internal/pool/source's own integration test
// (TestIntegration_RealArchiveSourceThroughRealOrchestrator); this fake
// keeps the cmd-level proof network/backend-free, "follow[ing] the
// fakeHarnessSource pattern" per this work package's brief.
type fakeArchiveSource struct {
	// failRealize makes Realize always fail — the negative-case fixture for
	// maybeInstallLocked's archive/off-guix downgrade (review round 2
	// major): a Realize failure must NOT be downgraded to a warning, since
	// the downgrade's whole premise (the pool generation genuinely exists
	// and is active) doesn't hold.
	failRealize bool
}

// Refresh returns a ZERO PublishedAt, mirroring the real archive Source's
// own Refresh (internal/pool/source/archive.go's RULING doc comment: "this
// channel's manifest has no publish-history field ... this method
// deliberately does NOT invent one"). wave2-integration-fixes item 1(d):
// this fake previously carried a non-zero, already-168h+-old PublishedAt,
// which papered over the eligibility regression this whole work package
// exists to fix — with a real zero PublishedAt, RefreshHarnessLocked's own
// synthesis (PublishedAt := FirstSeenAt, marked PublishedAtSynthesized)
// is what makes these tests' fresh-pool day-0 installs eligible, exactly
// as it must for the real Source.
func (f *fakeArchiveSource) Refresh(_ context.Context, pkg string) ([]pool.Candidate, error) {
	return []pool.Candidate{{Version: "1.1.10"}}, nil
}

func (f *fakeArchiveSource) Resolve(_ context.Context, _ backend.Backend, pkg, version, _, _, recipeRevision string) (pool.LockHarness, error) {
	return pool.LockHarness{
		Source: "archive", Package: pkg, Version: version,
		PublishedAt:    pool.FormatTimestamp(time.Time{}),
		FirstSeenAt:    pool.FormatTimestamp(time.Time{}),
		RecipeRevision: recipeRevision,
		Archive: &pool.LockArchive{Artifacts: map[pool.PlatformKey]pool.LockArchiveArtifact{
			pool.PlatformLinuxAMD64Glibc: {URL: "https://example.invalid/agy.tar.gz", SHA512: strings.Repeat("a", 128)},
			pool.PlatformLinuxARM64Glibc: {URL: "https://example.invalid/agy.tar.gz", SHA512: strings.Repeat("a", 128)},
		}},
	}, nil
}

func (f *fakeArchiveSource) Fetch(_ context.Context, h *pool.LockHarness, _ string) error {
	for key, artifact := range h.Archive.Artifacts {
		artifact.SHA256 = strings.Repeat("b", 64)
		h.Archive.Artifacts[key] = artifact
	}
	return nil
}

// Realize writes a small but REAL generation tree and builds its receipt
// with the real pool receipt helpers, mirroring fakeHarnessSource.Realize —
// but with no Runtime-artifact check, since an archive-sourced lock has no
// Runtime at all.
func (f *fakeArchiveSource) Realize(_ context.Context, _ backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, _, stagingGenDir string) (*pool.Receipt, error) {
	if f.failRealize {
		return nil, fmt.Errorf("fake archive realize failure for %s", harnessName)
	}
	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(binDir, binName), []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		return nil, err
	}
	files, err := pool.ScanGenerationFiles(stagingGenDir)
	if err != nil {
		return nil, err
	}
	digest, err := pool.ManifestDigest(files)
	if err != nil {
		return nil, err
	}
	lockHash, err := h.Hash()
	if err != nil {
		return nil, err
	}
	return &pool.Receipt{
		SchemaVersion:  pool.ReceiptSchemaVersion,
		Harness:        harnessName,
		Version:        h.Version,
		Platform:       key,
		RecipeRevision: h.RecipeRevision,
		LockEntryHash:  lockHash,
		Backend:        pool.ReceiptBackend{Name: "none"},
		Files:          files,
		ManifestDigest: digest,
	}, nil
}

// TestHarnessInstallClaudeOnDockerOnlyHostDowngradesGuixAbsentToWarning pins
// that the legacy-install guix-absent downgrade applies to an npm-sourced
// harness (claude), not just PoolArchive ones — a docker-only host: no guix
// on PATH, so the host-side legacy npm installer, which literally shells
// `guix shell node -- npm install ...` (internal/harness's npmInstall
// recipe), is impossible PERMANENTLY and BY DESIGN there. deps.resolveBackend
// simulates a working oci backend (the thing that actually makes this
// host's install succeed) — the backend-selection POLICY itself
// (guix-vs-oci-vs-error) is exercised separately by
// TestDefaultResolveBackendSelectionMatrix; this test is about what happens
// AFTER resolve/realize/activate succeed and only the legacy step is
// guix-absent-blocked. Exit 0, a clearly labeled warning, and full pool
// state (lock, generation, active pointer) — the same guarantee the
// archive-source downgrade already provides, extended to npm.
func TestHarnessInstallClaudeOnDockerOnlyHostDowngradesGuixAbsentToWarning(t *testing.T) {
	swapHasGuix(t, false)

	paths := testHarnessPaths(t)
	binDir := t.TempDir()
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	backendCalled := false
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return src, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			backendCalled = true
			return fakeHarnessBackend{}, nil // simulates a working oci backend
		},
	}

	var buf bytes.Buffer
	cmd := bareHarnessCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := runHarnessInstall(context.Background(), cmd, deps, "claude", harness.Registry["claude"], ">=2 <3", false, "")
	if err != nil {
		t.Fatalf("expected nil error (guix-absent legacy-install failure downgraded to a warning on a docker-only host): %v\noutput:\n%s", err, buf.String())
	}
	if !backendCalled {
		t.Fatal("resolveBackend should have been called for npm-sourced claude (simulating a docker-only host's oci backend)")
	}
	out := buf.String()
	if !strings.Contains(out, "pool generation built and activated") {
		t.Errorf("expected the downgraded warning text to name what succeeded, got:\n%s", out)
	}
	if !strings.Contains(out, "host-side legacy install was skipped") {
		t.Errorf("expected the downgraded warning to say the host-side legacy binary was not installed, got:\n%s", out)
	}
	if !strings.Contains(out, `"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": "claude"}`) {
		t.Errorf("expected the devcontainer-feature guidance to be retained, got:\n%s", out)
	}

	// Pool state must be fully populated: refresh/resolve/fetch/realize/
	// activate all completed for real, with no guix at all.
	lockData, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("harness-lock.json missing: %v", err)
	}
	lockDoc, err := pool.UnmarshalLock(lockData)
	if err != nil {
		t.Fatalf("harness-lock.json failed to validate: %v", err)
	}
	lh, ok := lockDoc.Harnesses["claude"]
	if !ok || lh.Source != "npm" || lh.Version != "2.1.220" {
		t.Fatalf("lock entry = %+v, ok=%v, want source=npm version=2.1.220", lh, ok)
	}

	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Fatalf("HostPlatformKey: %v", err)
	}
	genID, ok := pool.ActiveGeneration(paths, "claude", hostKey)
	if !ok {
		t.Fatal("expected an active generation pointer for claude")
	}
	if _, err := os.Stat(filepath.Join(paths.Runtime, "generations", genID, "receipt.json")); err != nil {
		t.Errorf("expected a receipt.json in the published generation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Runtime, "bin", "claude")); err != nil {
		t.Errorf("expected a generated dispatcher at runtime/bin/claude: %v", err)
	}
}

// TestHarnessInstallClaudeGuixHostFailingLegacyInstallerStillFails pins that
// on a guix host, a legacy-install failure that is NOT the guix-absent
// cause (e.g. a broken npm registry, a failing postinstall) must NOT be
// downgraded — it is a real installer problem, and the pool's own realize
// succeeding doesn't excuse it. This is what distinguishes npm's narrow
// downgrade rule from archive's wide one.
func TestHarnessInstallClaudeGuixHostFailingLegacyInstallerStillFails(t *testing.T) {
	swapHasGuix(t, true) // guix IS present, so the guix-absent path never fires
	swapRunInstallArgv(t, func([]string) error {
		return errors.New("npm ERR! registry unreachable")
	})

	paths := testHarnessPaths(t)
	binDir := t.TempDir()
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return src, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			return fakeHarnessBackend{}, nil // simulates the guix backend
		},
	}

	err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", harness.Registry["claude"], ">=2 <3", false, "")
	if err == nil {
		t.Fatal("expected an error: a non-guix-absent legacy-install failure on a guix host must stay fatal for an npm-sourced harness")
	}
	if !strings.Contains(err.Error(), "registry unreachable") {
		t.Errorf("error = %q, want it to name the underlying legacy-install failure", err.Error())
	}

	// The realized generation must still exist — realize itself succeeded;
	// only the legacy step (correctly) failed the command.
	hostKey, hkErr := pool.HostPlatformKey()
	if hkErr != nil {
		t.Fatalf("HostPlatformKey: %v", hkErr)
	}
	if _, ok := pool.ActiveGeneration(paths, "claude", hostKey); !ok {
		t.Error("expected an active generation for claude despite the legacy-install failure (realize itself succeeded)")
	}
}

// TestHarnessInstallClaudeStaysFailedWhenRealizeFailsOnDockerOnlyHost is the
// negative case for warnInsteadOfFailForRealizedGeneration's RealizeErr
// guard, npm-sourced: when Realize itself fails, the pool generation was
// NOT genuinely built, so a guix-absent legacy-install failure on top of
// that must still fail the command — the downgrade never applies without a
// real generation behind it.
// TestHarnessInstallClaudeStaysFailedWhenRealizeFailsOnDockerOnlyHost is
// built to isolate the RealizeErr guard specifically, not just the
// ActiveGeneration lookup that follows it: a naive version of this test
// (Realize failing on a harness's very first install, so no pointer file
// exists yet either) would pass even with the RealizeErr guard deleted,
// since ActiveGeneration's own "no pointer" check would independently
// return false. So instead: a first install succeeds normally (writing a
// real active pointer), its generation directory is then deleted out from
// under the STILL-PRESENT stale pointer (simulating disk loss/corruption —
// generationRealized will see dirPresent=false), and a second call is made
// with guix absent and Realize now failing. On that second call,
// pool.ActiveGeneration(...) still reports ok=true (it only reads the
// pointer file, never checks the generation directory) — the RealizeErr
// guard is the ONLY thing that can still prevent an incorrect downgrade
// here.
func TestHarnessInstallClaudeStaysFailedWhenRealizeFailsOnDockerOnlyHost(t *testing.T) {
	paths := testHarnessPaths(t)
	binDir := t.TempDir()
	claudePkg := harness.Registry["claude"].PoolPackage
	src := &fakeHarnessSource{candidates: map[string][]pool.Candidate{
		claudePkg: {{Version: "2.1.220", PublishedAt: fixedTestNow.Add(-200 * time.Hour)}},
	}}
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return src, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			return fakeHarnessBackend{}, nil
		},
	}

	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return nil })
	if err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", harness.Registry["claude"], ">=2 <3", false, ""); err != nil {
		t.Fatalf("initial install: %v", err)
	}

	hostKey, hkErr := pool.HostPlatformKey()
	if hkErr != nil {
		t.Fatalf("HostPlatformKey: %v", hkErr)
	}
	genID, ok := pool.ActiveGeneration(paths, "claude", hostKey)
	if !ok {
		t.Fatal("expected an active generation pointer after the initial install")
	}
	if err := os.RemoveAll(filepath.Join(paths.Runtime, "generations", genID)); err != nil {
		t.Fatal(err)
	}

	swapHasGuix(t, false)
	src.failRealize = true

	err := runHarnessInstall(context.Background(), bareHarnessCmd(), deps, "claude", harness.Registry["claude"], "", false, "")
	if err == nil {
		t.Fatal("expected an error: Realize failed on this call, so the stale active pointer must not cause a downgrade")
	}
	if !errors.Is(err, errGuixAbsent) {
		t.Errorf("error = %v, want it to still be (wrap) the guix-absent legacy-install failure", err)
	}
}

// TestHarnessInstallAgyReachesRealizeWithoutBackendOnNoGuixHost is this
// work package's headline capability, end to end through cmd: on a
// simulated no-guix host, `wraptool harness install agy` must reach
// refresh -> resolve -> fetch -> realize -> activate with NO backend probe
// at all (deps.resolveBackend is wired to t.Fatal if ever called). The
// legacy curl|bash install step is explicitly NOT skipped for agy (this
// work package's RULING, §16 Phase 2 posture) and DOES still require guix
// — but review round 2's major amendment downgrades THAT SPECIFIC failure
// (guix absent, AND the pool generation was genuinely built+activated) to
// a warning rather than a command failure: the developer's actual goal
// (`install agy`) substantively succeeded — a locked, realized, active
// generation exists — even though the LEGACY bin/ copy (which nothing
// currently mounts runtime/ into a container in place of, §16 Phase 2
// posture) did not run. The command therefore exits 0, with a clearly
// labeled warning on stderr retaining the devcontainer-feature guidance.
// Pool state (lock, generation, pointer, dispatcher) is asserted exactly as
// before. This is the exact UX transcript reported alongside this work
// package's fix round.
func TestHarnessInstallAgyDowngradesGuixAbsentLegacyInstallToWarning(t *testing.T) {
	swapHasGuix(t, false)

	paths := testHarnessPaths(t)
	binDir := t.TempDir()
	backendCalled := false
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return &fakeArchiveSource{}, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			backendCalled = true
			t.Error("resolveBackend must not be called for agy (archive-sourced, needs no backend)")
			return nil, errors.New("guix not on PATH")
		},
	}

	var buf bytes.Buffer
	cmd := bareHarnessCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := runHarnessInstall(context.Background(), cmd, deps, "agy", harness.Registry["agy"], "", false, "")
	if err != nil {
		t.Fatalf("expected nil error (guix-absent legacy-install failure downgraded to a warning): %v\noutput:\n%s", err, buf.String())
	}
	if backendCalled {
		t.Fatal("resolveBackend was called — the archive-source backend gate did not hold")
	}
	out := buf.String()
	if !strings.Contains(out, "pool generation built and locked, but the pool binary was NOT installed") {
		t.Errorf("expected the downgraded warning text, got:\n%s", out)
	}
	if !strings.Contains(out, `"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": "agy"}`) {
		t.Errorf("expected the devcontainer-feature guidance to be retained, got:\n%s", out)
	}

	// Pool state must be fully populated: refresh/resolve/fetch/realize/
	// activate all completed for real, with no backend at all.
	lockData, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("harness-lock.json missing: %v", err)
	}
	lockDoc, err := pool.UnmarshalLock(lockData)
	if err != nil {
		t.Fatalf("harness-lock.json failed to validate: %v", err)
	}
	lh, ok := lockDoc.Harnesses["agy"]
	if !ok || lh.Source != "archive" || lh.Version != "1.1.10" {
		t.Fatalf("lock entry = %+v, ok=%v, want source=archive version=1.1.10", lh, ok)
	}

	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Fatalf("HostPlatformKey: %v", err)
	}
	genID, ok := pool.ActiveGeneration(paths, "agy", hostKey)
	if !ok {
		t.Fatal("expected an active generation pointer for agy")
	}
	if _, err := os.Stat(filepath.Join(paths.Runtime, "generations", genID, "receipt.json")); err != nil {
		t.Errorf("expected a receipt.json in the published generation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Runtime, "bin", "agy")); err != nil {
		t.Errorf("expected a generated dispatcher at runtime/bin/agy: %v", err)
	}
	if v, err := pool.GenerationVersion(paths, genID); err != nil || v != "1.1.10" {
		t.Errorf("GenerationVersion(%s) = %q, %v, want 1.1.10, nil", genID, v, err)
	}

	// list/status: agy must render like a pool-lockable (npm) harness, not
	// "legacy (not lockable)" — the REALIZED/ACTIVE columns must be live.
	var listBuf bytes.Buffer
	if err := runHarnessList(paths, filepath.Join(binDir, "bin"), fixedTestNow, &listBuf); err != nil {
		t.Fatalf("runHarnessList: %v", err)
	}
	var agyLine string
	for _, line := range strings.Split(listBuf.String(), "\n") {
		if strings.HasPrefix(line, "agy ") || strings.HasPrefix(line, "agy\t") {
			agyLine = line
			break
		}
	}
	if agyLine == "" {
		t.Fatalf("no agy row found in `list` output:\n%s", listBuf.String())
	}
	if strings.Contains(agyLine, "legacy (not lockable)") {
		t.Errorf("agy still renders as a legacy/non-lockable harness in `list`: %q", agyLine)
	}
	fields := strings.Fields(agyLine)
	// HARNESS CONSTRAINT LOCKED ACTIVE LATEST ELIGIBLE QUARANTINED REALIZED
	if len(fields) < 4 || fields[3] != "1.1.10" {
		t.Errorf("agy row = %q, want the ACTIVE column (4th field) to be 1.1.10", agyLine)
	}
}

// TestHarnessUpdateAgyDowngradesGuixAbsentLegacyInstallToWarning is
// TestHarnessInstallAgyDowngradesGuixAbsentLegacyInstallToWarning's `update`
// counterpart — maybeInstallLocked is shared by both commands (review round
// 2's placement direction), so `update agy` on a no-guix host must exit 0
// with the same downgraded warning, driven through the full cobra command
// tree (not a direct function call) to also prove the wiring inside
// newHarnessUpdateCmd's RunE.
func TestHarnessUpdateAgyDowngradesGuixAbsentLegacyInstallToWarning(t *testing.T) {
	swapHasGuix(t, false)

	paths := testHarnessPaths(t)
	if err := os.WriteFile(paths.Manifest, []byte("schema_version: 1\nharnesses:\n  agy: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// `update` never refreshes itself — it reads already-observed candidates
	// (mirrors this file's other update tests, e.g.
	// TestHarnessUpdateBareOnlyTouchesManifestListedHarnesses).
	obs := pool.Observations{}
	if err := obs.Observe(pool.ObservationKey("archive", "agy", "1.1.10"),
		pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), pool.FormatTimestamp(fixedTestNow.Add(-200*time.Hour)), false, false, false); err != nil {
		t.Fatal(err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return &fakeArchiveSource{}, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			t.Error("resolveBackend must not be called for agy (archive-sourced, needs no backend)")
			return nil, errors.New("guix not on PATH")
		},
	}

	out, err := runHarnessCmdWithDeps(t, deps, "update", "agy")
	if err != nil {
		t.Fatalf("update agy: expected nil error (downgraded to a warning): %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "pool generation built and locked, but the pool binary was NOT installed") {
		t.Errorf("expected the downgraded warning text, got:\n%s", out)
	}
	if !strings.Contains(out, `"forge.snamellit.com/pti/wraptool/wraptool:0": {"harness": "agy"}`) {
		t.Errorf("expected the devcontainer-feature guidance to be retained, got:\n%s", out)
	}

	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Fatalf("HostPlatformKey: %v", err)
	}
	if _, ok := pool.ActiveGeneration(paths, "agy", hostKey); !ok {
		t.Error("expected an active generation pointer for agy after update")
	}
}

// TestHarnessInstallAgyStaysFailedWhenRealizeFailsOnNoGuixHost is the
// negative case: when Realize itself fails (result.RealizeErr set), the
// pool generation was NOT genuinely built — the downgrade's premise doesn't
// hold — so a guix-absent legacy-install failure on top of that must still
// fail the command (review round 2 major, explicit negative case).
func TestHarnessInstallAgyStaysFailedWhenRealizeFailsOnNoGuixHost(t *testing.T) {
	swapHasGuix(t, false)

	paths := testHarnessPaths(t)
	binDir := t.TempDir()
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return &fakeArchiveSource{failRealize: true}, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			t.Error("resolveBackend must not be called for agy (archive-sourced, needs no backend)")
			return nil, errors.New("guix not on PATH")
		},
	}

	var buf bytes.Buffer
	cmd := bareHarnessCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := runHarnessInstall(context.Background(), cmd, deps, "agy", harness.Registry["agy"], "", false, "")
	if err == nil {
		t.Fatal("expected a non-nil error: Realize failed, so the guix-absent legacy-install failure must NOT be downgraded")
	}
	if !errors.Is(err, errGuixAbsent) {
		t.Errorf("error = %v, want it to still be (wrap) the guix-absent legacy-install failure", err)
	}

	hostKey, err2 := pool.HostPlatformKey()
	if err2 != nil {
		t.Fatalf("HostPlatformKey: %v", err2)
	}
	if _, ok := pool.ActiveGeneration(paths, "agy", hostKey); ok {
		t.Error("no generation should have been activated when Realize failed")
	}
}

// TestHarnessInstallAgyDowngradesLegacyInstallFailureForOtherReasonsToWarning
// is the WIDENED downgrade (wave2-integration-fixes item 4): with guix
// PRESENT (so legacyInstallVersionLocked never even reaches its own
// guix-absent check) and runInstallArgv itself failing with a genuine
// upstream error (e.g. curl couldn't resolve the host) — NOT the
// guix-absent one — maybeInstallLocked's archive-source downgrade now
// still applies (item 3's guix runtime mount makes runtime/ reachable
// regardless of why the legacy install failed), so the command exits 0
// with a warning that names the real underlying error. Renamed from (and
// inverting) the pre-wave2 "StaysFailed..." test, which pinned the OLD
// narrow guix-absent-only gate this ruling replaces.
func TestHarnessInstallAgyDowngradesLegacyInstallFailureForOtherReasonsToWarning(t *testing.T) {
	swapHasGuix(t, true)
	swapRunInstallArgv(t, func([]string) error { return errors.New("curl: could not resolve host") })

	paths := testHarnessPaths(t)
	binDir := t.TempDir()
	deps := harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return &fakeArchiveSource{}, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			t.Error("resolveBackend must not be called for agy (archive-sourced, needs no backend)")
			return nil, errors.New("guix not on PATH")
		},
	}

	var buf bytes.Buffer
	cmd := bareHarnessCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := runHarnessInstall(context.Background(), cmd, deps, "agy", harness.Registry["agy"], "", false, "")
	if err != nil {
		t.Fatalf("expected nil error (any legacy-install failure is downgraded to a warning when the pool generation is genuinely built+active): %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "pool generation built and locked, but the pool binary was NOT installed") {
		t.Errorf("expected the downgraded warning text, got:\n%s", out)
	}
	if !strings.Contains(out, "could not resolve host") {
		t.Errorf("expected the warning to name the real underlying legacy-install error, got:\n%s", out)
	}

	// The pool's own work still succeeded (realize/activate both ran fine
	// with guix present) — only the unrelated upstream curl failure did
	// not.
	hostKey, hkErr := pool.HostPlatformKey()
	if hkErr != nil {
		t.Fatalf("HostPlatformKey: %v", hkErr)
	}
	if _, ok := pool.ActiveGeneration(paths, "agy", hostKey); !ok {
		t.Error("expected an active generation for agy even though the legacy install failed")
	}
}

// The guix-absent legacy-install downgrade applies to npm-sourced harnesses
// too, scoped to the guix-absent cause only (warnInsteadOfFailForRealizedGeneration's
// doc comment): a docker-only host installing claude is the ordinary,
// exit-0 case (TestHarnessInstallClaudeOnDockerOnlyHostDowngradesGuixAbsentToWarning),
// and only a non-guix-absent legacy-install failure on a guix host still
// fails the command (TestHarnessInstallClaudeGuixHostFailingLegacyInstallerStillFails).
