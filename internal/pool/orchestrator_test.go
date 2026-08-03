package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool/backend"
)

// --- test fakes ---

// fakeSource is a Source test double. Its Resolve/Fetch record call counts
// so tests can assert the §10 idempotence and §9 phase-separation contracts
// (e.g. "sync never resolves") without any container runtime.
type fakeSource struct {
	candidates map[string][]Candidate // pkg -> candidates Refresh returns
	refreshErr error

	resolveErr   error
	resolveCalls int
	lastResolve  struct{ pkg, version, workDir, nodeVersion, recipeRevision string }

	fetchErr        error
	fetchCalls      int
	fetchedVersions []string

	realizeErr   error
	realizeCalls int
	lastRealize  struct {
		harnessName, binName string
		key                  PlatformKey
	}
	// realizeEmpty makes Realize write no files at all, producing an
	// empty-manifest receipt — RealizeGeneration must refuse to publish it
	// (§6.4/§11.4).
	realizeEmpty bool
	// realizeMismatch makes Realize write an extra, undeclared file into
	// stagingGenDir AFTER computing the receipt, so the receipt it returns no
	// longer matches the tree on disk — RealizeGeneration's host-side
	// VerifyGenerationTree must catch it.
	realizeMismatch bool
	// realizeBadSchema makes Realize return a receipt with SchemaVersion 0 —
	// RealizeGeneration must reject it via receipt.Validate() BEFORE writing
	// receipt.json or renaming, since publishing a receipt that later fails
	// to parse would permanently wedge the idempotent fast path on every
	// subsequent call.
	realizeBadSchema bool
	// realizeWrongVersion makes Realize return a receipt whose Version
	// disagrees with h.Version — RealizeGeneration must reject the mismatch
	// before publishing (the receipt must describe exactly what was asked
	// for, closing the two-different-GenerationID-sources hole between this
	// function's publish path and the idempotent fast path above it).
	realizeWrongVersion bool
}

func (f *fakeSource) Refresh(_ context.Context, pkg string) ([]Candidate, error) {
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.candidates[pkg], nil
}

func (f *fakeSource) Resolve(_ context.Context, _ backend.Backend, pkg, version, workDir, nodeVersion, recipeRevision string) (LockHarness, error) {
	f.resolveCalls++
	f.lastResolve = struct{ pkg, version, workDir, nodeVersion, recipeRevision string }{pkg, version, workDir, nodeVersion, recipeRevision}
	if f.resolveErr != nil {
		return LockHarness{}, f.resolveErr
	}
	return LockHarness{
		Source:         npmSourceName,
		Package:        pkg,
		Version:        version,
		PublishedAt:    FormatTimestamp(fixedNow),
		RecipeRevision: recipeRevision,
	}, nil
}

func (f *fakeSource) Fetch(_ context.Context, h *LockHarness, _ string) error {
	f.fetchCalls++
	f.fetchedVersions = append(f.fetchedVersions, h.Version)
	if f.fetchErr != nil {
		return f.fetchErr
	}
	// Mimic the real Source.Fetch: it MUTATES h in place, filling each npm
	// package entry's pool-local sha256 cache-address field (and, for the
	// allow-young integrity-reporting tests, the upstream integrity value)
	// as objects are admitted to cache. UpdateHarness must persist the lock
	// only after this mutation, never before.
	h.NPMLock = &NPMLock{LockfileVersion: 3, Packages: map[string]NPMLockPackage{
		h.Package: {Version: h.Version, Resolved: "https://registry.npmjs.org/x.tgz", Integrity: "sha512-fakeintegrity", SHA256: "deadbeef"},
	}}
	return nil
}

// Realize writes a small but REAL generation tree into stagingGenDir and
// builds its receipt with the real pool receipt helpers
// (ScanGenerationFiles/ManifestDigest) rather than fabricating one — so
// RealizeGeneration's host-side verification (VerifyGenerationTree) is
// exercised for real, not vacuously against a receipt that trivially agrees
// with itself.
func (f *fakeSource) Realize(_ context.Context, _ backend.Backend, harnessName, binName string, h *LockHarness, key PlatformKey, _, stagingGenDir string) (*Receipt, error) {
	f.realizeCalls++
	f.lastRealize = struct {
		harnessName, binName string
		key                  PlatformKey
	}{harnessName, binName, key}
	if f.realizeErr != nil {
		return nil, f.realizeErr
	}

	if !f.realizeEmpty {
		binDir := filepath.Join(stagingGenDir, "bin")
		if err := os.MkdirAll(binDir, poolDirPerm); err != nil {
			return nil, err
		}
		name := binName
		if name == "" {
			name = harnessName
		}
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
			return nil, err
		}
	}

	files, err := ScanGenerationFiles(stagingGenDir)
	if err != nil {
		return nil, err
	}
	digest, err := ManifestDigest(files)
	if err != nil {
		return nil, err
	}

	if f.realizeMismatch {
		// Written AFTER the receipt above is computed, so the receipt
		// Realize returns no longer describes the tree on disk —
		// RealizeGeneration's VerifyGenerationTree must catch this.
		if err := os.WriteFile(filepath.Join(stagingGenDir, "undeclared"), []byte("surprise"), 0o644); err != nil {
			return nil, err
		}
	}

	lockHash, err := h.Hash()
	if err != nil {
		return nil, err
	}

	schemaVersion := ReceiptSchemaVersion
	if f.realizeBadSchema {
		schemaVersion = 0
	}
	version := h.Version
	if f.realizeWrongVersion {
		version = h.Version + "-mismatched"
	}

	return &Receipt{
		SchemaVersion:  schemaVersion,
		Harness:        harnessName,
		Version:        version,
		Platform:       key,
		RecipeRevision: h.RecipeRevision,
		LockEntryHash:  lockHash,
		Backend:        ReceiptBackend{Name: "fake"},
		Files:          files,
		ManifestDigest: digest,
	}, nil
}

// fakeBackend is a no-op backend.Backend: orchestrator tests never actually
// run a container, they only need a value satisfying the interface to thread
// through to fakeSource.Resolve.
type fakeBackend struct{}

func (fakeBackend) Name() string                               { return "fake" }
func (fakeBackend) Available() bool                            { return true }
func (fakeBackend) Run(context.Context, backend.RunSpec) error { return nil }

var fixedNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

func testPaths(t *testing.T) PoolPaths {
	t.Helper()
	dir := t.TempDir()
	return PoolPaths{
		Manifest:     filepath.Join(dir, "manifest.yaml"),
		Lock:         filepath.Join(dir, "harness-lock.json"),
		Observations: filepath.Join(dir, "observations.json"),
		Audit:        filepath.Join(dir, "audit.jsonl"),
		AdvisoryLock: filepath.Join(dir, "lock"),
		Cache:        filepath.Join(dir, "cache", "sha256"), // mirrors pool.CacheSHA256Dir()
		Staging:      filepath.Join(dir, "staging"),
		Runtime:      filepath.Join(dir, "runtime"),
	}
}

func writeManifest(t *testing.T, paths PoolPaths, yamlContent string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(paths.Manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Manifest, []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func writeLock(t *testing.T, paths PoolPaths, l *Lock) {
	t.Helper()
	data, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- RefreshHarness ---

func TestRefreshHarnessStampsFirstSeenOnce(t *testing.T) {
	paths := testPaths(t)
	pkg := "@anthropic-ai/claude-code"
	published := fixedNow.Add(-30 * 24 * time.Hour)
	src := &fakeSource{candidates: map[string][]Candidate{
		pkg: {{Version: "2.1.220", PublishedAt: published}},
	}}

	first, err := RefreshHarness(ctxT(t), src, paths, pkg, fixedNow)
	if err != nil {
		t.Fatalf("first RefreshHarness: %v", err)
	}
	if len(first) != 1 || first[0].FirstSeenAt.IsZero() {
		t.Fatalf("first refresh: want one candidate with a stamped FirstSeenAt, got %+v", first)
	}
	if !first[0].FirstSeenAt.Equal(fixedNow) {
		t.Errorf("first refresh FirstSeenAt = %v, want %v (the refresh's own now)", first[0].FirstSeenAt, fixedNow)
	}

	later := fixedNow.Add(48 * time.Hour)
	second, err := RefreshHarness(ctxT(t), src, paths, pkg, later)
	if err != nil {
		t.Fatalf("second RefreshHarness: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second refresh: want one candidate, got %+v", second)
	}
	if !second[0].FirstSeenAt.Equal(fixedNow) {
		t.Errorf("second refresh FirstSeenAt = %v, want unchanged %v (must not move on a later refresh)", second[0].FirstSeenAt, fixedNow)
	}

	// Refresh is metadata-only (§9 phase 1): it must never call
	// Source.Resolve or Source.Fetch.
	if src.resolveCalls != 0 || src.fetchCalls != 0 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want none — refresh is metadata-only", src.resolveCalls, src.fetchCalls)
	}
}

func TestRefreshHarnessPropagatesSourceError(t *testing.T) {
	paths := testPaths(t)
	src := &fakeSource{refreshErr: errors.New("registry unreachable")}
	if _, err := RefreshHarness(ctxT(t), src, paths, "pkg", fixedNow); err == nil {
		t.Fatal("expected an error from a failing Source.Refresh")
	}
}

// --- UpdateHarness selection ---

const claudeManifest = `
schema_version: 1
policy:
  minimum_release_age: 168h
harnesses:
  claude:
    constraint: ">=2 <3"
`

func seedObservations(t *testing.T, paths PoolPaths, pkg string, candidates []Candidate) {
	t.Helper()
	obs := make(Observations, len(candidates))
	for _, c := range candidates {
		key := ObservationKey(npmSourceName, pkg, c.Version)
		if err := obs.Observe(key, FormatTimestamp(c.FirstSeenAt), FormatTimestamp(c.PublishedAt), c.Prerelease, c.Yanked); err != nil {
			t.Fatalf("seeding observation %s: %v", key, err)
		}
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatalf("saving observations: %v", err)
	}
}

func TestUpdateHarnessPicksNewestEligibleUnderConstraint(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		// Eligible: published/observed well outside the 168h window.
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
		// Newest, but quarantined: published only 1h ago.
		{Version: "2.1.224", PublishedAt: fixedNow.Add(-1 * time.Hour), FirstSeenAt: fixedNow.Add(-1 * time.Hour)},
		// Outside the manifest's constraint (">=2 <3") entirely.
		{Version: "3.0.0", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected changed=true for a first resolve")
	}
	if res.Harness.Version != "2.1.220" {
		t.Errorf("selected version = %q, want the newest ELIGIBLE 2.1.220 (2.1.224 is quarantined, 3.0.0 fails the constraint)", res.Harness.Version)
	}
	if src.resolveCalls != 1 || src.fetchCalls != 1 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want exactly one of each", src.resolveCalls, src.fetchCalls)
	}
}

func TestUpdateHarnessNothingEligibleErrors(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	// No usable lock exists at all, and the only observed candidate is
	// quarantined: this is the "hard-error only when there is no usable
	// lock and nothing eligible" case.
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.224", PublishedAt: fixedNow.Add(-1 * time.Hour), FirstSeenAt: fixedNow.Add(-1 * time.Hour)},
	})

	src := &fakeSource{}
	_, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err == nil {
		t.Fatal("expected an error when nothing is eligible and there is no usable lock")
	}
	if !strings.Contains(err.Error(), "2.1.224") {
		t.Errorf("error should name the quarantined candidate 2.1.224, got: %v", err)
	}
	if !strings.Contains(err.Error(), "remaining") {
		t.Errorf("error should report remaining quarantine time, got: %v", err)
	}
	if src.resolveCalls != 0 || src.fetchCalls != 0 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want none when nothing is eligible", src.resolveCalls, src.fetchCalls)
	}
}

func TestUpdateHarnessIdempotentWhenLockAlreadyAgrees(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	// Pre-seed a lock that already agrees with what selection would produce
	// — including the recipe revision, which strict idempotence also
	// requires to match (§8/§10).
	recipeRevision, err := npmRecipeRevision(pkg)
	if err != nil {
		t.Fatal(err)
	}
	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   FormatTimestamp(fixedNow.Add(-time.Hour)),
		PolicyHash:    "sha256:whatever",
		Harnesses: map[string]LockHarness{
			"claude": {Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				RecipeRevision: recipeRevision},
		},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if res.Changed {
		t.Error("expected changed=false when manifest and lock already agree")
	}
	if res.Note != "" {
		t.Errorf("Note = %q, want empty for the plain strict-idempotence case", res.Note)
	}
	if res.Harness.Version != "2.1.220" {
		t.Errorf("Version = %q, want the already-locked 2.1.220", res.Harness.Version)
	}
	if src.resolveCalls != 0 || src.fetchCalls != 0 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want none on an idempotent install/update", src.resolveCalls, src.fetchCalls)
	}
}

// TestUpdateHarnessInstallErrorsOnCorruptObservationsEvenWithAnAgreeingLock
// pins a deliberate, accepted deviation from the pre-decision-core code: the
// decision core's DecisionInput always carries a fully-loaded candidate list
// (decision.go's doc comment / Decide's contract), so the shell now loads
// observations.json unconditionally before calling Decide — including on
// install's strict short-circuit path, which used to skip that read
// entirely when the lock already agreed. A corrupt observations.json next to
// an otherwise fully-agreeing lock therefore now surfaces LoadObservations'
// parse error instead of install silently short-circuiting to success. This
// is intentional (see UpdateHarnessLocked's DecisionInput-assembly comment)
// and this test exists so a future change can't silently re-introduce the
// old skip-on-agreement behavior without a reviewer noticing the diff here.
func TestUpdateHarnessInstallErrorsOnCorruptObservationsEvenWithAnAgreeingLock(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	recipeRevision, err := npmRecipeRevision(pkg)
	if err != nil {
		t.Fatal(err)
	}
	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   FormatTimestamp(fixedNow.Add(-time.Hour)),
		PolicyHash:    "sha256:whatever",
		Harnesses: map[string]LockHarness{
			"claude": {Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				RecipeRevision: recipeRevision},
		},
	})

	// A lock that, on its own, satisfies install's strict short-circuit
	// (§10) — except observations.json next to it is corrupt.
	if err := os.MkdirAll(filepath.Dir(paths.Observations), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Observations, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{}
	_, err = UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, // Advance: false (install)
	})
	if err == nil {
		t.Fatal("expected an error from corrupt observations.json, even though the lock alone would satisfy install's short-circuit")
	}
	if !strings.Contains(err.Error(), paths.Observations) {
		t.Errorf("error = %q, want it to name the corrupt file %q", err.Error(), paths.Observations)
	}
	if src.resolveCalls != 0 || src.fetchCalls != 0 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want none — the failure happens before any Source call", src.resolveCalls, src.fetchCalls)
	}
}

// TestUpdateHarnessKeepsExistingLockWhenNothingEligible covers MAJOR 1's
// second idempotence step: the existing lock's recipe is stale (so strict
// step-1 idempotence does NOT short-circuit), but the only newly observed
// candidate is quarantined. UpdateHarnessLocked must keep the existing lock
// and succeed with an informative note, not error.
func TestUpdateHarnessKeepsExistingLockWhenNothingEligible(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest) // minimum_release_age: 168h
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.230", PublishedAt: fixedNow.Add(-1 * time.Hour), FirstSeenAt: fixedNow.Add(-1 * time.Hour)},
	})

	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {Source: npmSourceName, Package: pkg, Version: "2.1.224",
				PublishedAt: FormatTimestamp(fixedNow.Add(-400 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-400 * time.Hour)),
				RecipeRevision: "sha256:stale"},
		},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("expected a successful no-op, got error: %v", err)
	}
	if res.Changed {
		t.Error("expected changed=false when nothing new is eligible")
	}
	if res.Harness.Version != "2.1.224" {
		t.Errorf("Harness.Version = %q, want the kept 2.1.224", res.Harness.Version)
	}
	if !strings.Contains(res.Note, "2.1.224") || !strings.Contains(res.Note, "2.1.230") || !strings.Contains(res.Note, "quarantined") {
		t.Errorf("Note = %q, want it to name both the kept version and the quarantined candidate", res.Note)
	}
	if src.resolveCalls != 0 || src.fetchCalls != 0 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want none", src.resolveCalls, src.fetchCalls)
	}
}

// TestUpdateHarnessRecipeRevisionMismatchForcesReResolve: even at the SAME
// version, a stale recipe_revision must force a fresh resolve — recipe
// changes cannot be silently reused (§8).
// TestUpdateHarnessRecipeRevisionMismatchForcesReResolve: even at the SAME
// version, a stale recipe_revision must force a fresh resolve — in BOTH
// install (Advance=false) and update (Advance=true) modes.
func TestUpdateHarnessRecipeRevisionMismatchForcesReResolve(t *testing.T) {
	for _, advance := range []bool{false, true} {
		mode := "install"
		if advance {
			mode = "update"
		}
		t.Run(mode, func(t *testing.T) {
			paths := testPaths(t)
			writeManifest(t, paths, claudeManifest)
			pkg := "@anthropic-ai/claude-code"
			seedObservations(t, paths, pkg, []Candidate{
				{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
			})
			writeLock(t, paths, &Lock{
				SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
				Harnesses: map[string]LockHarness{
					"claude": {Source: npmSourceName, Package: pkg, Version: "2.1.220",
						PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
						RecipeRevision: "sha256:stale"},
				},
			})

			src := &fakeSource{}
			res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
				Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, Advance: advance,
			})
			if err != nil {
				t.Fatalf("UpdateHarness: %v", err)
			}
			if !res.Changed {
				t.Error("expected changed=true: a recipe_revision mismatch must force a re-resolve even at the same version")
			}
			if res.Harness.Version != "2.1.220" {
				t.Errorf("Version = %q, want still 2.1.220 (same version, refreshed recipe)", res.Harness.Version)
			}
			if src.resolveCalls != 1 || src.fetchCalls != 1 {
				t.Errorf("resolveCalls=%d fetchCalls=%d, want exactly one of each", src.resolveCalls, src.fetchCalls)
			}
		})
	}
}

// TestUpdateVsInstallAdvanceSemantics is the reviewer-named untested
// quadrant: with a valid, fully-agreeing lock AND a newer eligible
// candidate available, update (Advance=true) must advance to it while
// install (Advance=false) must no-op — §10's "update is the only normal
// command that advances a floating constraint".
func TestUpdateVsInstallAdvanceSemantics(t *testing.T) {
	setup := func(t *testing.T) (PoolPaths, string) {
		paths := testPaths(t)
		writeManifest(t, paths, claudeManifest) // constraint ">=2 <3"
		pkg := "@anthropic-ai/claude-code"
		seedObservations(t, paths, pkg, []Candidate{
			{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
			{Version: "2.1.224", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)}, // also eligible, newer
		})
		recipeRevision, err := npmRecipeRevision(pkg)
		if err != nil {
			t.Fatal(err)
		}
		writeLock(t, paths, &Lock{
			SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
			Harnesses: map[string]LockHarness{
				"claude": {Source: npmSourceName, Package: pkg, Version: "2.1.220",
					PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
					RecipeRevision: recipeRevision},
			},
		})
		return paths, pkg
	}

	t.Run("install does not advance to the newer eligible candidate", func(t *testing.T) {
		paths, pkg := setup(t)
		src := &fakeSource{}
		res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
			Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, Advance: false,
		})
		if err != nil {
			t.Fatalf("install (Advance=false): %v", err)
		}
		if res.Changed {
			t.Error("expected changed=false: install must not advance past an already-agreeing lock")
		}
		if res.Harness.Version != "2.1.220" {
			t.Errorf("Version = %q, want unchanged 2.1.220", res.Harness.Version)
		}
		if src.resolveCalls != 0 || src.fetchCalls != 0 {
			t.Errorf("resolveCalls=%d fetchCalls=%d, want none — install must not even scan", src.resolveCalls, src.fetchCalls)
		}
	})

	t.Run("update advances to the newer eligible candidate", func(t *testing.T) {
		paths, pkg := setup(t)
		src := &fakeSource{}
		res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
			Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, Advance: true,
		})
		if err != nil {
			t.Fatalf("update (Advance=true): %v", err)
		}
		if !res.Changed {
			t.Error("expected changed=true: update must advance to the newer eligible candidate")
		}
		if res.Harness.Version != "2.1.224" {
			t.Errorf("Version = %q, want advanced to 2.1.224", res.Harness.Version)
		}
		if src.resolveCalls != 1 || src.fetchCalls != 1 {
			t.Errorf("resolveCalls=%d fetchCalls=%d, want exactly one of each", src.resolveCalls, src.fetchCalls)
		}
	})
}

// TestUpdateVsInstallAfterConstraintWidened is the reviewer's second named
// scenario: widening the manifest constraint to admit a new major, with that
// major eligible, must let update advance to it while install keeps the
// still-constraint-satisfying locked version.
func TestUpdateVsInstallAfterConstraintWidened(t *testing.T) {
	setup := func(t *testing.T) (PoolPaths, string) {
		paths := testPaths(t)
		// Constraint widened to admit 3.x too.
		writeManifest(t, paths, "schema_version: 1\npolicy:\n  minimum_release_age: 168h\nharnesses:\n  claude:\n    constraint: \">=2 <4\"\n")
		pkg := "@anthropic-ai/claude-code"
		seedObservations(t, paths, pkg, []Candidate{
			{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
			{Version: "3.0.0", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)}, // eligible under the widened constraint
		})
		recipeRevision, err := npmRecipeRevision(pkg)
		if err != nil {
			t.Fatal(err)
		}
		writeLock(t, paths, &Lock{
			SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
			Harnesses: map[string]LockHarness{
				"claude": {Source: npmSourceName, Package: pkg, Version: "2.1.220",
					PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
					RecipeRevision: recipeRevision},
			},
		})
		return paths, pkg
	}

	t.Run("install keeps the still-satisfying 2.1.220", func(t *testing.T) {
		paths, pkg := setup(t)
		src := &fakeSource{}
		res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
			Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, Advance: false,
		})
		if err != nil {
			t.Fatalf("install (Advance=false): %v", err)
		}
		if res.Changed {
			t.Error("expected changed=false: install keeps 2.1.220 (it still satisfies the widened constraint)")
		}
		if res.Harness.Version != "2.1.220" {
			t.Errorf("Version = %q, want unchanged 2.1.220", res.Harness.Version)
		}
		if src.resolveCalls != 0 {
			t.Errorf("resolveCalls = %d, want 0", src.resolveCalls)
		}
	})

	t.Run("update advances to 3.0.0 under the widened constraint", func(t *testing.T) {
		paths, pkg := setup(t)
		src := &fakeSource{}
		res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
			Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, Advance: true,
		})
		if err != nil {
			t.Fatalf("update (Advance=true): %v", err)
		}
		if !res.Changed {
			t.Error("expected changed=true: update must advance to 3.0.0 under the widened constraint")
		}
		if res.Harness.Version != "3.0.0" {
			t.Errorf("Version = %q, want advanced to 3.0.0", res.Harness.Version)
		}
	})
}

// TestUpdateHarnessAfterAllowYoungPlainRerunIsIdempotent: once --allow-young
// locks a young candidate, a later PLAIN re-run (same observations, same
// manifest) must recognize the existing lock via strict step-1 idempotence
// and no-op — not attempt a fresh eligibility scan that would find the
// locked version still quarantined and either error or downgrade it.
func TestUpdateHarnessAfterAllowYoungPlainRerunIsIdempotent(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.224", PublishedAt: fixedNow.Add(-1 * time.Hour), FirstSeenAt: fixedNow.Add(-1 * time.Hour)},
	})

	src := &fakeSource{}
	res1, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, AllowYoung: true, Reason: "test",
	})
	if err != nil {
		t.Fatalf("first (allow-young) UpdateHarness: %v", err)
	}
	if !res1.Changed || res1.Harness.Version != "2.1.224" {
		t.Fatalf("first update: got changed=%v version=%q, want changed=true version=2.1.224", res1.Changed, res1.Harness.Version)
	}
	if src.resolveCalls != 1 {
		t.Fatalf("resolveCalls after first update = %d, want 1", src.resolveCalls)
	}

	res2, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("second (plain) UpdateHarness: %v", err)
	}
	if res2.Changed {
		t.Error("expected changed=false on the plain re-run after an allow-young install")
	}
	if res2.Harness.Version != "2.1.224" {
		t.Errorf("Version after plain re-run = %q, want unchanged 2.1.224", res2.Harness.Version)
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls after plain re-run = %d, want still 1 (no re-resolve)", src.resolveCalls)
	}
}

// TestUpdateHarnessIdempotentAcrossPolicyTightening: tightening pool policy
// alone (without touching the manifest constraint) must not force a
// re-resolve of an already-locked, still-constraint-satisfying version.
func TestUpdateHarnessIdempotentAcrossPolicyTightening(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, "schema_version: 1\npolicy:\n  minimum_release_age: 0h\nharnesses:\n  claude:\n    constraint: \">=2 <3\"\n")
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-1 * time.Hour), FirstSeenAt: fixedNow.Add(-1 * time.Hour)},
	})

	src := &fakeSource{}
	res1, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow})
	if err != nil {
		t.Fatalf("first update (loose policy): %v", err)
	}
	if !res1.Changed {
		t.Fatal("expected the first update to resolve")
	}

	// Tighten policy to 168h. The already-locked 2.1.220 is far too young
	// to be freshly eligible, but it must still be recognized as
	// already-locked (strict idempotence doesn't consult eligibility).
	writeManifest(t, paths, claudeManifest)
	res2, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow.Add(time.Minute)})
	if err != nil {
		t.Fatalf("update after tightening policy: %v", err)
	}
	if res2.Changed {
		t.Error("expected changed=false after tightening policy alone")
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls = %d, want still 1", src.resolveCalls)
	}
}

func TestUpdateHarnessAllowYoungRequiresReason(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	src := &fakeSource{}
	_, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: "pkg", Now: fixedNow, AllowYoung: true, Reason: "",
	})
	if err == nil {
		t.Fatal("expected an error for --allow-young with an empty reason")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("error should mention the missing reason, got: %v", err)
	}
}

// TestUpdateHarnessAllowYoungAppliedBypassesQuarantineAndAudits: an
// effective bypass (the selected candidate would not otherwise be eligible)
// is reported via AllowYoungApplied/CandidateAge/CandidateIntegrity and
// appends exactly one audit.jsonl event, written after the lock is written.
func TestUpdateHarnessAllowYoungAppliedBypassesQuarantineAndAudits(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	published := fixedNow.Add(-1 * time.Hour)
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.224", PublishedAt: published, FirstSeenAt: published},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
		AllowYoung: true, Reason: "CVE-2026-00000",
	})
	if err != nil {
		t.Fatalf("UpdateHarness with --allow-young: %v", err)
	}
	if !res.Changed || res.Harness.Version != "2.1.224" {
		t.Fatalf("got changed=%v version=%q, want changed=true version=2.1.224", res.Changed, res.Harness.Version)
	}
	if !res.AllowYoungApplied {
		t.Error("expected AllowYoungApplied=true: 2.1.224 is not eligible under the real policy")
	}
	if res.CandidateAge != fixedNow.Sub(published) {
		t.Errorf("CandidateAge = %v, want %v", res.CandidateAge, fixedNow.Sub(published))
	}
	if res.CandidateIntegrity != "sha512-fakeintegrity" {
		t.Errorf("CandidateIntegrity = %q, want the fake Source's integrity value", res.CandidateIntegrity)
	}
	if src.resolveCalls != 1 || src.fetchCalls != 1 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want exactly one of each", src.resolveCalls, src.fetchCalls)
	}

	evt := findAuditEvent(t, paths.Audit, "allow_young")
	if evt.Fields["reason"] != "CVE-2026-00000" {
		t.Errorf("audit event reason = %v, want CVE-2026-00000", evt.Fields["reason"])
	}
	if evt.Fields["age"] == nil || evt.Fields["age"] == "" {
		t.Errorf("audit event should carry the candidate's age, got fields: %+v", evt.Fields)
	}
	if evt.Fields["integrity"] != "sha512-fakeintegrity" {
		t.Errorf("audit event integrity = %v, want sha512-fakeintegrity", evt.Fields["integrity"])
	}
	// A fresh resolve also realizes (§16 Phase 2) — its own audit event
	// coexists with allow_young's, in either order; both are asserted
	// independently rather than assuming a fixed line position.
	_ = findAuditEvent(t, paths.Audit, "generation_realized")
}

// findAuditEvent parses every line of the audit log at path and returns the
// first event of the given type, failing the test if none is found — used
// instead of assuming a fixed line number, since more than one command (e.g.
// a resolve's own "allow_young" event and RealizeGeneration's
// "generation_realized" event) can append to the same audit log within one
// call.
func findAuditEvent(t *testing.T, path, eventType string) AuditEvent {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var evt AuditEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("parsing audit event %q: %v", line, err)
		}
		if evt.Type == eventType {
			return evt
		}
	}
	t.Fatalf("no audit event of type %q found in %s", eventType, path)
	return AuditEvent{}
}

// TestUpdateHarnessAllowYoungAuditSurvivesRealizeFailure is the fix-round
// regression for review finding 1: the allow_young audit event must be
// written as soon as the lock write succeeds, BEFORE RealizeGeneration runs
// — not after it. If it ran after and Realize failed, the lock would already
// be advanced (durably persisted) but carry no audit record of the
// quarantine bypass that produced it, and a caller's retry would find the
// lock already matching (DecisionUpToDate/DecisionRealize, not
// DecisionResolveVersion) — the audit event could never be emitted on any
// later call either. This test injects a Realize failure on an
// otherwise-successful allow-young update and asserts BOTH halves survive
// it: the lock is advanced to the bypassed version, AND the allow_young
// audit line exists.
//
// Updated for wave1-integration-fixes finding 2: a Realize failure is now
// NON-FATAL (UpdateResult.RealizeErr, not a returned error) — see
// UpdateHarnessLocked's doc comment — so this test now asserts
// err == nil and RealizeErr carrying the injected failure, instead of the
// pre-wave1 contract of UpdateHarness itself returning the error.
func TestUpdateHarnessAllowYoungAuditSurvivesRealizeFailure(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	published := fixedNow.Add(-1 * time.Hour)
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.224", PublishedAt: published, FirstSeenAt: published},
	})

	src := &fakeSource{realizeErr: errRealizeBoom}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
		AllowYoung: true, Reason: "CVE-2026-00000",
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v, want nil — a Realize failure is non-fatal (RealizeErr)", err)
	}
	if !errors.Is(res.RealizeErr, errRealizeBoom) {
		t.Errorf("RealizeErr = %v, want the injected %v", res.RealizeErr, errRealizeBoom)
	}
	if res.RealizedGenerationID != "" {
		t.Errorf("RealizedGenerationID = %q, want empty alongside a non-nil RealizeErr", res.RealizedGenerationID)
	}

	// The lock must still have advanced: Realize failing must not roll back
	// an already-persisted, already-audited resolve+fetch+lock.
	lockDoc, lerr := loadLockOrEmpty(paths.Lock)
	if lerr != nil {
		t.Fatalf("loading lock: %v", lerr)
	}
	lh, ok := lockDoc.Harnesses["claude"]
	if !ok || lh.Version != "2.1.224" {
		t.Fatalf("lock harness = %+v (ok=%v), want version 2.1.224 advanced despite the Realize failure", lh, ok)
	}

	// And the allow_young audit event must exist — this is the crux of the
	// fix: it must not depend on Realize succeeding.
	evt := findAuditEvent(t, paths.Audit, "allow_young")
	if evt.Fields["reason"] != "CVE-2026-00000" {
		t.Errorf("audit event reason = %v, want CVE-2026-00000", evt.Fields["reason"])
	}
}

// TestUpdateHarnessRealizeFailureIsNonFatal is wave1-integration-fixes
// finding 2's dedicated orchestrator-level regression, isolated from the
// allow-young scenario above: a plain (no --allow-young) first-time resolve
// whose Realize fails must still report err == nil, with the resolve+
// fetch+lock effects fully applied (Changed=true, the new version durably
// persisted) and the failure surfaced only via UpdateResult.RealizeErr —
// dispatcher/activation/mounting are a later wave, so legacy install (the
// only thing this wave executes) must still be reachable by the caller.
func TestUpdateHarnessRealizeFailureIsNonFatal(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{realizeErr: errRealizeBoom}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v, want nil — a Realize failure must be non-fatal", err)
	}
	if !res.Changed {
		t.Error("expected Changed=true: the resolve+fetch+lock effects must still be reported, despite the Realize failure")
	}
	if res.Harness.Version != "2.1.220" {
		t.Errorf("Harness.Version = %q, want the freshly resolved 2.1.220", res.Harness.Version)
	}
	if !errors.Is(res.RealizeErr, errRealizeBoom) {
		t.Errorf("RealizeErr = %v, want the injected %v", res.RealizeErr, errRealizeBoom)
	}
	if res.RealizedGenerationID != "" {
		t.Errorf("RealizedGenerationID = %q, want empty alongside a non-nil RealizeErr", res.RealizedGenerationID)
	}

	lockDoc, lerr := loadLockOrEmpty(paths.Lock)
	if lerr != nil {
		t.Fatalf("loading lock: %v", lerr)
	}
	lh, ok := lockDoc.Harnesses["claude"]
	if !ok || lh.Version != "2.1.220" {
		t.Fatalf("lock harness = %+v (ok=%v), want version 2.1.220 persisted despite the Realize failure", lh, ok)
	}
	if len(lh.Generations) != 0 {
		t.Errorf("lock Generations = %v, want empty — Realize never succeeded, so nothing should be recorded", lh.Generations)
	}
}

// TestUpdateHarnessSelfHealsPhase1RecipeMismatch pins wave1-integration-fixes
// finding 1: bumping recipeFormatVersion (1 -> 2, §8) must make a Phase-1
// pool self-heal instead of permanently wedging. A Phase-1 lock has no
// npm_lock.lockfile field (internal/pool/source's NPM.Realize requires one,
// added by this wave) and its recipe_revision was computed under the OLD
// recipeFormatVersion (1). Without the bump: install/update would loop
// forever — DecisionRealize (the stale Phase-1 recipe_revision still
// "matches" under the old format, so the resolve axis looks fully agreed) ->
// NPM.Realize errors "lock predates the embedded-lockfile schema" -> a later
// update short-circuits right back to DecisionRealize (nothing about
// RecipeRevision changed) -> the same error again, with Resolve never
// running and the lockfile field never backfilled. Bumping the format
// changes the freshly computed recipe_revision, so lockMatchesRecipe
// (decision.go, §8) is false, Decide instead returns DecisionResolveVersion,
// and the ensuing Resolve+Fetch+lock-write self-heals the pool on its very
// next install.
func TestUpdateHarnessSelfHealsPhase1RecipeMismatch(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	phase1Recipe := recipeRevisionForFormat(t, pkg, 1)
	wantRecipe, err := npmRecipeRevision(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if wantRecipe == phase1Recipe {
		t.Fatal("test setup bug: the current recipeFormatVersion's hash must differ from format 1's — did the bump get reverted?")
	}

	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				RecipeRevision: phase1Recipe,
				// Phase-1 shape: a Packages summary, but no Lockfile field —
				// internal/pool/source's NPM.Realize refuses to realize this
				// (its own doc comment), which is exactly the wedge this
				// format bump exists to break.
				NPMLock: &NPMLock{LockfileVersion: 3, Packages: map[string]NPMLockPackage{
					pkg: {Version: "2.1.220", Resolved: "https://registry.npmjs.org/x.tgz", Integrity: "sha512-old", SHA256: "deadbeef"},
				}},
			},
		},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, // install (Advance: false)
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true: the format bump must invalidate the Phase-1 recipe and force a re-resolve")
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls = %d, want 1 — the decision path must call Resolve, not just Realize", src.resolveCalls)
	}
	if src.realizeCalls != 1 {
		t.Errorf("realizeCalls = %d, want 1 — the fresh resolve must still realize", src.realizeCalls)
	}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		t.Fatalf("loading persisted lock: %v", err)
	}
	lh, ok := lockDoc.Harnesses["claude"]
	if !ok {
		t.Fatal("no persisted lock entry for claude")
	}
	if lh.RecipeRevision != wantRecipe {
		t.Errorf("persisted RecipeRevision = %q, want the new format's %q", lh.RecipeRevision, wantRecipe)
	}
}

// recipeRevisionForFormat mirrors npmRecipeRevision's hashing exactly but
// with an explicit format number, so TestUpdateHarnessSelfHealsPhase1RecipeMismatch
// can construct a recipe_revision as it would have been computed under a
// PRIOR recipeFormatVersion (here, Phase 1's format 1) without depending on
// that old constant value still existing anywhere in the source.
func recipeRevisionForFormat(t *testing.T, pkg string, format int) string {
	t.Helper()
	data, err := json.Marshal(struct {
		Package string `json:"package"`
		Format  int    `json:"recipe_format_version"`
	}{Package: pkg, Format: format})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestUpdateHarnessAllowYoungNotAppliedWhenAlreadyEligible: requesting
// --allow-young when the normally-selected candidate is eligible anyway is
// a no-op bypass — no audit event, AllowYoungApplied stays false.
func TestUpdateHarnessAllowYoungNotAppliedWhenAlreadyEligible(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, AllowYoung: true, Reason: "just in case",
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if res.AllowYoungApplied {
		t.Error("expected AllowYoungApplied=false: 2.1.220 is eligible under the real policy already")
	}
	// The realize step (§16 Phase 2) still appends its own
	// "generation_realized" event for the fresh resolve — only "allow_young"
	// must be absent for a no-op bypass.
	data, err := os.ReadFile(paths.Audit)
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	if strings.Contains(string(data), `"allow_young"`) {
		t.Errorf("expected no allow_young audit event for a no-op bypass, got: %s", data)
	}
}

func TestUpdateHarnessErrorsWithoutManifestEntry(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, "schema_version: 1\n")
	src := &fakeSource{}
	_, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: "pkg", Now: fixedNow,
	})
	if err == nil {
		t.Fatal("expected an error when the harness has no manifest entry")
	}
}

// TestUpdateHarnessWritesLockOnlyAfterFetchFills proves the fetch-before-lock
// ordering: Source.Fetch mutates its LockHarness argument in place (filling
// the pool-local sha256 cache-address field), so the persisted
// harness-lock.json must contain that mutation, not a value-less entry
// written before Fetch ran.
func TestUpdateHarnessWritesLockOnlyAfterFetchFills(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{}
	if _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	}); err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}

	lockDoc, err := loadLockOrEmpty(paths.Lock)
	if err != nil {
		t.Fatalf("loading written lock: %v", err)
	}
	lh, ok := lockDoc.Harnesses["claude"]
	if !ok {
		t.Fatal("no lock entry for claude was written")
	}
	if lh.NPMLock == nil || lh.NPMLock.Packages[pkg].SHA256 != "deadbeef" {
		t.Errorf("persisted lock is missing the sha256 Fetch fills in, got: %+v", lh.NPMLock)
	}
}

// TestUpdateHarnessFetchFailureLeavesPreviousLockUntouched proves a Fetch
// error (including a partial one) never persists a half-fetched resolution:
// since the lock is written only after Fetch succeeds, an error there must
// leave whatever was on disk before the call completely unchanged.
func TestUpdateHarnessFetchFailureLeavesPreviousLockUntouched(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	// Pre-seed a lock for a DIFFERENT version (no matching RecipeRevision
	// either), so selection below does not take the strict idempotent
	// shortcut and actually calls Resolve/Fetch.
	previous := &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:prev",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source: npmSourceName, Package: pkg, Version: "2.1.100",
				PublishedAt: FormatTimestamp(fixedNow.Add(-400 * time.Hour)),
				FirstSeenAt: FormatTimestamp(fixedNow.Add(-400 * time.Hour)),
			},
		},
	}
	prevData, err := previous.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(paths.Lock, prevData, 0o644); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{fetchErr: errors.New("network reset mid-download")}
	_, err = UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err == nil {
		t.Fatal("expected an error from a failing Fetch")
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls = %d, want 1 (Resolve must still run before the failing Fetch)", src.resolveCalls)
	}

	afterData, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("reading lock after failed update: %v", err)
	}
	if string(afterData) != string(prevData) {
		t.Errorf("lock file changed after a failed Fetch; before:\n%s\nafter:\n%s", prevData, afterData)
	}
}

// TestUpdateHarnessCreatesAndRemovesScratchDirWhenWorkDirEmpty covers
// MAJOR 4: an empty UpdateOptions.WorkDir must never reach Source.Resolve —
// UpdateHarnessLocked creates one under paths.Staging and always removes it.
func TestUpdateHarnessCreatesAndRemovesScratchDirWhenWorkDirEmpty(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{}
	if _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, // WorkDir left empty
	}); err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}

	gotWorkDir := src.lastResolve.workDir
	if gotWorkDir == "" {
		t.Fatal("Source.Resolve was called with an empty workDir")
	}
	if !strings.HasPrefix(gotWorkDir, paths.Staging) {
		t.Errorf("workDir = %q, want it rooted under paths.Staging = %q", gotWorkDir, paths.Staging)
	}
	if _, err := os.Stat(gotWorkDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir %q should have been removed after the call, stat err = %v", gotWorkDir, err)
	}
}

// TestUpdateHarnessRemovesScratchDirOnResolveFailure: the scratch dir is
// cleaned up even when Source.Resolve itself fails.
func TestUpdateHarnessRemovesScratchDirOnResolveFailure(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{resolveErr: errors.New("resolve blew up")}
	if _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	}); err == nil {
		t.Fatal("expected an error from a failing Resolve")
	}
	gotWorkDir := src.lastResolve.workDir
	if gotWorkDir == "" {
		t.Fatal("Source.Resolve was called with an empty workDir")
	}
	if _, err := os.Stat(gotWorkDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir %q should have been removed even after a Resolve failure, stat err = %v", gotWorkDir, err)
	}
}

// TestUpdateHarnessRespectsExplicitWorkDir: a caller-supplied WorkDir is
// passed through unchanged and is not touched by UpdateHarnessLocked's own
// cleanup (it isn't the one that created it).
func TestUpdateHarnessRespectsExplicitWorkDir(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	explicit := filepath.Join(t.TempDir(), "my-scratch")
	if err := os.MkdirAll(explicit, 0o755); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{}
	if _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, WorkDir: explicit,
	}); err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if src.lastResolve.workDir != explicit {
		t.Errorf("workDir = %q, want the caller-supplied %q unchanged", src.lastResolve.workDir, explicit)
	}
	if _, err := os.Stat(explicit); err != nil {
		t.Errorf("caller-supplied WorkDir should survive the call, stat err = %v", err)
	}
}

// --- SyncHarness ---

func TestSyncHarnessFetchesButNeverResolves(t *testing.T) {
	paths := testPaths(t)
	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   FormatTimestamp(fixedNow),
		PolicyHash:    "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {Source: npmSourceName, Package: "@anthropic-ai/claude-code", Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), RecipeRevision: "sha256:y"},
		},
	})

	src := &fakeSource{}
	lh, err := SyncHarness(ctxT(t), src, fakeBackend{}, paths, "claude", "claude", PlatformLinuxAMD64Glibc)
	if err != nil {
		t.Fatalf("SyncHarness: %v", err)
	}
	if lh.Version != "2.1.220" {
		t.Errorf("Version = %q, want the locked 2.1.220", lh.Version)
	}
	if src.resolveCalls != 0 {
		t.Errorf("resolveCalls = %d, want 0 — sync must never resolve", src.resolveCalls)
	}
	if src.fetchCalls != 1 {
		t.Errorf("fetchCalls = %d, want 1", src.fetchCalls)
	}
	if src.realizeCalls != 1 {
		t.Errorf("realizeCalls = %d, want 1 — sync must realize the platform it's given", src.realizeCalls)
	}
}

func TestSyncHarnessErrorsWithoutLockEntry(t *testing.T) {
	paths := testPaths(t)
	src := &fakeSource{}
	if _, err := SyncHarness(ctxT(t), src, fakeBackend{}, paths, "claude", "claude", PlatformLinuxAMD64Glibc); err == nil {
		t.Fatal("expected an error when there is no lock entry to sync from")
	}
}

// --- StatusHarness / list ---

func TestStatusHarnessRendersDesiredLockedLatestEligibleQuarantined(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
		{Version: "2.1.224", PublishedAt: fixedNow.Add(-3 * 24 * time.Hour), FirstSeenAt: fixedNow.Add(-3 * 24 * time.Hour)},
	})
	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
			},
		},
	})

	status, err := StatusHarness(paths, "claude", pkg, fixedNow)
	if err != nil {
		t.Fatalf("StatusHarness: %v", err)
	}
	if status.Constraint != ">=2 <3" {
		t.Errorf("Constraint = %q, want >=2 <3", status.Constraint)
	}
	if status.Locked != "2.1.220" {
		t.Errorf("Locked = %q, want 2.1.220", status.Locked)
	}
	if status.Latest != "2.1.224" {
		t.Errorf("Latest = %q, want 2.1.224", status.Latest)
	}
	if status.Eligible != "2.1.220" {
		t.Errorf("Eligible = %q, want 2.1.220", status.Eligible)
	}
	if status.QuarantinedVersion != "2.1.224" {
		t.Errorf("QuarantinedVersion = %q, want 2.1.224", status.QuarantinedVersion)
	}
	if !status.QuarantineOK {
		t.Fatal("QuarantineOK = false, want true (2.1.224 is quarantined by age alone)")
	}
	wantRemaining := 168*time.Hour - 3*24*time.Hour
	if status.QuarantineRemaining != wantRemaining {
		t.Errorf("QuarantineRemaining = %v, want %v", status.QuarantineRemaining, wantRemaining)
	}
}

func TestStatusHarnessToleratesMissingManifestAndLock(t *testing.T) {
	paths := testPaths(t)
	status, err := StatusHarness(paths, "claude", "@anthropic-ai/claude-code", fixedNow)
	if err != nil {
		t.Fatalf("StatusHarness on an empty pool: %v", err)
	}
	if status.Constraint != "" || status.Locked != "" || status.Latest != "" || status.Eligible != "" {
		t.Errorf("expected an all-empty status on a pool with nothing installed, got %+v", status)
	}
}

// --- ManifestHarnessNames / LockHarnessNames ---

func TestManifestHarnessNames(t *testing.T) {
	paths := testPaths(t)
	if names, err := ManifestHarnessNames(paths.Manifest); err != nil || len(names) != 0 {
		t.Fatalf("ManifestHarnessNames on a missing manifest = %v, %v, want empty, nil", names, err)
	}
	writeManifest(t, paths, claudeManifest)
	names, err := ManifestHarnessNames(paths.Manifest)
	if err != nil {
		t.Fatalf("ManifestHarnessNames: %v", err)
	}
	if len(names) != 1 || names[0] != "claude" {
		t.Errorf("ManifestHarnessNames = %v, want [claude]", names)
	}
}

func TestLockHarnessNames(t *testing.T) {
	paths := testPaths(t)
	if names, err := LockHarnessNames(paths.Lock); err != nil || len(names) != 0 {
		t.Fatalf("LockHarnessNames on a missing lock = %v, %v, want empty, nil", names, err)
	}
	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {Source: npmSourceName, Package: "pkg", Version: "1.0.0",
				PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow)},
		},
	})
	names, err := LockHarnessNames(paths.Lock)
	if err != nil {
		t.Fatalf("LockHarnessNames: %v", err)
	}
	if len(names) != 1 || names[0] != "claude" {
		t.Errorf("LockHarnessNames = %v, want [claude]", names)
	}
}

// --- lock ordering / no nested-lock deadlock ---

// TestSequentialRefreshThenUpdateDoesNotDeadlock exercises exactly the
// composition `install` uses in cmd (write manifest intent, then
// RefreshHarness, then UpdateHarness): each call acquires and fully releases
// its own PoolLock rather than nesting one inside another. Two *separate*
// PoolLock file locks on the same path from the same process would deadlock
// if nested (a second open-file-description flock from the same process
// blocks on the first), so this proves the composition pattern this
// package's exported functions are meant to be used with is safe.
func TestSequentialRefreshThenUpdateDoesNotDeadlock(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	src := &fakeSource{candidates: map[string][]Candidate{
		pkg: {{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour)}},
	}}

	done := make(chan error, 1)
	go func() {
		ctx := ctxT(t)
		if _, err := RefreshHarness(ctx, src, paths, pkg, fixedNow); err != nil {
			done <- err
			return
		}
		_, err := UpdateHarness(ctx, src, fakeBackend{}, paths, UpdateOptions{
			Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sequential refresh+update: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sequential refresh then update did not complete within 5s — suspected lock deadlock")
	}
}

// TestLockPoolComposesRefreshAndUpdateLockedWithoutDeadlock is the MAJOR-3
// composition pattern cmd commands use: acquire the pool lock exactly ONCE
// via LockPool, then call the *Locked variants directly. If either *Locked
// function mistakenly tried to acquire its own lock internally, this would
// deadlock against the lock already held here.
func TestLockPoolComposesRefreshAndUpdateLockedWithoutDeadlock(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	src := &fakeSource{candidates: map[string][]Candidate{
		pkg: {{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour)}},
	}}

	done := make(chan error, 1)
	go func() {
		ctx := ctxT(t)
		release, err := LockPool(ctx, paths)
		if err != nil {
			done <- err
			return
		}
		defer release()
		if _, err := RefreshHarnessLocked(ctx, src, paths, pkg, fixedNow); err != nil {
			done <- err
			return
		}
		_, err = UpdateHarnessLocked(ctx, src, fakeBackend{}, paths, UpdateOptions{
			Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("single-lock-span refresh+update: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("single-lock-span refresh then update did not complete within 5s — suspected lock deadlock")
	}
}

// TestUpdateHarnessSecondCallWithUnchangedInputsIsIdempotent: calling
// UpdateHarness twice in a row with nothing changed between calls (same
// manifest, same observations) must resolve exactly once.
func TestUpdateHarnessSecondCallWithUnchangedInputsIsIdempotent(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{}
	if _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow}); err != nil {
		t.Fatalf("first UpdateHarness: %v", err)
	}
	if src.resolveCalls != 1 {
		t.Fatalf("resolveCalls after first update = %d, want 1", src.resolveCalls)
	}

	res2, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow.Add(time.Hour)})
	if err != nil {
		t.Fatalf("second UpdateHarness: %v", err)
	}
	if res2.Changed {
		t.Error("second UpdateHarness with nothing changed should be idempotent")
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls after second (idempotent) update = %d, want still 1", src.resolveCalls)
	}
}
