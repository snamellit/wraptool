package cmd

import (
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
	"github.com/pti/wraptool/internal/pool/source"
)

// fakeReleaseSource is the release-source (omp) analogue of
// fakeArchiveSource: network- and backend-free, with the lock shape
// (Source "release", Release populated) the real source returns.
type fakeReleaseSource struct {
	failRealize bool
}

func (f *fakeReleaseSource) Refresh(_ context.Context, _ string) ([]pool.Candidate, error) {
	return []pool.Candidate{{Version: "18.6.1"}}, nil
}

func (f *fakeReleaseSource) Resolve(_ context.Context, b backend.Backend, pkg, version, _, _, recipeRevision string) (pool.LockHarness, error) {
	if b != nil {
		return pool.LockHarness{}, errors.New("release source must receive a nil backend")
	}
	art := pool.LockReleaseArtifact{URL: "https://example.invalid/omp", SHA256: strings.Repeat("c", 64)}
	return pool.LockHarness{
		Source: "release", Package: pkg, Version: version,
		PublishedAt:    pool.FormatTimestamp(time.Time{}),
		FirstSeenAt:    pool.FormatTimestamp(time.Time{}),
		RecipeRevision: recipeRevision,
		Release: &pool.LockRelease{Repo: pkg, Tag: "v" + version, Artifacts: map[pool.PlatformKey]pool.LockReleaseArtifact{
			pool.PlatformLinuxAMD64Glibc: art,
			pool.PlatformLinuxARM64Glibc: art,
		}},
	}, nil
}

func (f *fakeReleaseSource) Fetch(context.Context, *pool.LockHarness, string, []pool.PlatformKey) error {
	return nil
}

func (f *fakeReleaseSource) Realize(ctx context.Context, b backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, nodeVersion, stagingGenDir string) (*pool.Receipt, error) {
	if f.failRealize {
		return nil, fmt.Errorf("fake release realize failure for %s", harnessName)
	}
	// The generation tree and receipt are identical in shape to the archive
	// fake's; reuse it rather than duplicating the receipt plumbing.
	return (&fakeArchiveSource{}).Realize(ctx, b, harnessName, binName, h, key, nodeVersion, stagingGenDir)
}

// ompDeps wires deps for omp: a resolveBackend that fails the test if ever
// probed (the release source needs no backend) and a poolBinDir pointing at
// an empty legacy tree.
func ompDeps(t *testing.T, paths pool.PoolPaths, src pool.Source) harnessDeps {
	t.Helper()
	binDir := t.TempDir()
	return harnessDeps{
		poolPaths:  func() pool.PoolPaths { return paths },
		poolBinDir: func() (string, string, error) { return binDir, filepath.Join(binDir, "bin"), nil },
		newSource:  func(harness.Descriptor) (pool.Source, error) { return src, nil },
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			t.Error("resolveBackend must not be called for omp (release-sourced, needs no backend)")
			return nil, errors.New("no backend")
		},
	}
}

func TestPoolSourceKindRelease(t *testing.T) {
	if got := poolSourceKind(harness.Registry["omp"]); got != pool.SourceRelease {
		t.Errorf("poolSourceKind(omp) = %q, want %q", got, pool.SourceRelease)
	}
	if got := poolSourceKind(harness.Registry["agy"]); got != pool.SourceArchive {
		t.Errorf("poolSourceKind(agy) = %q, want archive", got)
	}
	if got := poolSourceKind(harness.Registry["claude"]); got != pool.SourceNPM {
		t.Errorf("poolSourceKind(claude) = %q, want npm", got)
	}
}

func TestDefaultNewSourceReleaseIsReleaseSource(t *testing.T) {
	src, err := defaultHarnessDeps().newSource(harness.Registry["omp"])
	if err != nil {
		t.Fatalf("newSource(omp): %v", err)
	}
	if _, ok := src.(*source.Release); !ok {
		t.Errorf("newSource(omp) = %T, want *source.Release", src)
	}
}

func TestNewSourceRejectsUnparsableReleaseAssetKey(t *testing.T) {
	d := harness.Registry["omp"]
	spec := *d.PoolRelease
	spec.Assets = map[string]string{"linux-amd64-glibc": "omp-linux-x64", "plan9-mips": "omp-plan9"}
	d.PoolRelease = &spec

	if _, err := newReleaseSource(d, "", ""); err == nil || !strings.Contains(err.Error(), "plan9-mips") {
		t.Errorf("newReleaseSource with a bad asset key: err = %v, want one naming the key", err)
	}
	if _, err := releaseRecipeFor(d); err == nil {
		t.Error("releaseRecipeFor with a bad asset key: want an error, got nil")
	}
}

func TestHarnessBackendReleaseNeedsNone(t *testing.T) {
	deps := harnessDeps{
		resolveBackend: func(pool.PlatformKey) (backend.Backend, error) {
			t.Error("resolveBackend must not be called for omp")
			return fakeHarnessBackend{}, nil
		},
	}
	targetCalled := false
	target := func() (pool.PlatformKey, error) { targetCalled = true; return pool.PlatformLinuxAMD64Glibc, nil }

	b, err := harnessBackend(deps, harness.Registry["omp"], target)
	if err != nil || b != nil {
		t.Errorf("harnessBackend(omp) = (%v, %v), want (nil, nil)", b, err)
	}
	var bc backendCache
	b, err = bc.get(deps, harness.Registry["omp"], target)
	if err != nil || b != nil {
		t.Errorf("backendCache.get(omp) = (%v, %v), want (nil, nil)", b, err)
	}
	if targetCalled {
		t.Error("target platform was resolved for a harness that needs no backend")
	}
}

func TestReleaseRecipeForOmp(t *testing.T) {
	d := harness.Registry["omp"]
	r, err := releaseRecipeFor(d)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("releaseRecipeFor(omp) = nil")
	}
	if r.VersionFeed != d.PoolRelease.VersionFeed || r.Checksums != d.PoolRelease.Checksums {
		t.Errorf("recipe feed/checksums = %q/%q, want %q/%q", r.VersionFeed, r.Checksums, d.PoolRelease.VersionFeed, d.PoolRelease.Checksums)
	}
	if len(r.Assets) != len(d.PoolRelease.Assets) || len(r.Assets) == 0 {
		t.Fatalf("recipe assets = %v, want %d entries", r.Assets, len(d.PoolRelease.Assets))
	}
	for k, name := range d.PoolRelease.Assets {
		if got := r.Assets[pool.PlatformKey(k)]; got != name {
			t.Errorf("asset %s = %q, want %q", k, got, name)
		}
	}
	if d.PoolPackageID() != d.PoolRelease.Repo {
		t.Errorf("PoolPackageID = %q, want repo %q", d.PoolPackageID(), d.PoolRelease.Repo)
	}
	if nr, err := releaseRecipeFor(harness.Registry["agy"]); err != nil || nr != nil {
		t.Errorf("releaseRecipeFor(agy) = (%v, %v), want (nil, nil)", nr, err)
	}
}

// TestHarnessInstallOmpIsPoolOnlyAndSilent: a realized+activated pool-only
// harness completes with no legacy install attempt and no warning.
func TestHarnessInstallOmpIsPoolOnlyAndSilent(t *testing.T) {
	swapHasGuix(t, false)
	swapRunInstallArgv(t, func(argv []string) error {
		t.Errorf("legacy installer must not run for omp, got %v", argv)
		return nil
	})
	paths := testHarnessPaths(t)
	deps := ompDeps(t, paths, &fakeReleaseSource{})

	out, err := runHarnessCmdWithDeps(t, deps, "install", "omp")
	if err != nil {
		t.Fatalf("install omp: %v\noutput:\n%s", err, out)
	}
	for _, bad := range []string{"warning", "no automated installer", "legacy", "installing"} {
		if strings.Contains(out, bad) {
			t.Errorf("output mentions %q for a pool-only install:\n%s", bad, out)
		}
	}
	hostKey, err := pool.HostPlatformKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.ActiveGeneration(paths, "omp", hostKey); !ok {
		t.Error("expected an active generation for omp")
	}

	// Idempotent re-run (nothing changed) stays silent and succeeds.
	out, err = runHarnessCmdWithDeps(t, deps, "install", "omp")
	if err != nil || strings.Contains(out, "warning") {
		t.Errorf("second install omp: err=%v output:\n%s", err, out)
	}
}

func TestHarnessInstallOmpRealizeFailureFails(t *testing.T) {
	paths := testHarnessPaths(t)
	deps := ompDeps(t, paths, &fakeReleaseSource{failRealize: true})

	out, err := runHarnessCmdWithDeps(t, deps, "install", "omp")
	if err == nil {
		t.Fatalf("install omp with failing realize: want error, got nil\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "fake release realize failure") {
		t.Errorf("error = %v, want it to carry the realize error", err)
	}
	if strings.Contains(err.Error()+out, "no automated installer") {
		t.Errorf("must not mention a missing automated installer:\n%v\n%s", err, out)
	}
}

func TestHarnessSyncAndUpdateOmpSkipLegacySilently(t *testing.T) {
	swapRunInstallArgv(t, func(argv []string) error {
		t.Errorf("legacy installer must not run for omp, got %v", argv)
		return nil
	})
	paths := testHarnessPaths(t)
	deps := ompDeps(t, paths, &fakeReleaseSource{})
	if out, err := runHarnessCmdWithDeps(t, deps, "install", "omp"); err != nil {
		t.Fatalf("install omp: %v\n%s", err, out)
	}

	for _, sub := range []string{"sync", "update"} {
		out, err := runHarnessCmdWithDeps(t, deps, sub, "omp")
		if err != nil {
			t.Fatalf("%s omp: %v\noutput:\n%s", sub, err, out)
		}
		for _, bad := range []string{"warning", "no automated installer", "installing ...", "skipping host-side legacy"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s omp output mentions %q:\n%s", sub, bad, out)
			}
		}
	}
}

// TestHarnessListOmpReportsPoolGenerationVersion: ACTIVE shows the active
// generation's version, not legacy bin/ "yes"/"no" state.
func TestHarnessListOmpReportsPoolGenerationVersion(t *testing.T) {
	paths := testHarnessPaths(t)
	deps := ompDeps(t, paths, &fakeReleaseSource{})
	if out, err := runHarnessCmdWithDeps(t, deps, "install", "omp"); err != nil {
		t.Fatalf("install omp: %v\n%s", err, out)
	}
	var sb strings.Builder
	if err := runHarnessList(paths, filepath.Join(t.TempDir(), "bin"), time.Now(), &sb); err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(line, "omp") {
			row = line
		}
	}
	if !strings.Contains(row, "18.6.1") || strings.Contains(row, "legacy") {
		t.Errorf("omp row = %q, want the active version 18.6.1 and no legacy marker", row)
	}
}

// TestUpOmpWithoutActiveGenerationNamesInstall: a pool-only harness has no
// legacy fallback, so the guix plan errors (even with legacy trees present)
// and names the fix; with an active pointer it behaves like any harness.
func TestUpOmpWithoutActiveGenerationNamesInstall(t *testing.T) {
	dir := t.TempDir()
	legacyBin, legacyLib := filepath.Join(dir, "bin"), filepath.Join(dir, "lib")
	for _, p := range []string{legacyBin, legacyLib} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	in := guixPoolPlanInput{
		poolPlanInput: poolPlanInput{
			Harness: "omp", PoolLockable: true, PoolOnly: true,
			PointerPath:           filepath.Join(dir, "active", "omp.gen"),
			RuntimeDir:            filepath.Join(dir, "runtime"),
			RuntimeBinDir:         filepath.Join(dir, "runtime", "bin"),
			RuntimeGenerationsDir: filepath.Join(dir, "runtime", "generations"),
		},
		LegacyBin: legacyBin, LegacyLib: legacyLib,
	}
	plan := resolveGuixPoolPlan(in)
	if plan.err == nil || !strings.Contains(plan.err.Error(), "wraptool harness install omp") {
		t.Fatalf("plan.err = %v, want an error naming `wraptool harness install omp`", plan.err)
	}

	dplan := resolveDevcontainerPoolPlan(devcontainerPoolPlanInput{poolPlanInput: in.poolPlanInput})
	if !strings.Contains(dplan.notice, "wraptool harness install omp") || strings.Contains(dplan.notice, "per-image") {
		t.Errorf("devcontainer notice = %q, want install guidance without per-image wording", dplan.notice)
	}

	// With a live generation the plan is the ordinary active runtime plan.
	genID := "gen-1"
	for _, f := range []string{
		filepath.Join(in.RuntimeBinDir, "omp"),
		filepath.Join(in.RuntimeGenerationsDir, genID, "bin", "omp"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(in.PointerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.PointerPath, []byte(genID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if plan := resolveGuixPoolPlan(in); plan.err != nil || !plan.active {
		t.Errorf("active plan = %+v, want active with no error", plan)
	}
	if dplan := resolveDevcontainerPoolPlan(devcontainerPoolPlanInput{poolPlanInput: in.poolPlanInput}); !dplan.mountRuntime {
		t.Errorf("devcontainer active plan = %+v, want mountRuntime", dplan)
	}
}
