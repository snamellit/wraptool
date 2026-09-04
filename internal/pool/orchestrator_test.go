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
	// resolveZeroPublishedAt makes Resolve return a zero-formatted
	// PublishedAt, mirroring a real archive Source's Resolve (that
	// channel's manifest carries no publish-history timestamp at all,
	// source/archive.go's own doc comment) — used to exercise
	// UpdateHarnessLocked's PublishedAt-synthesis-before-lock-persist path
	// without depending on the real Archive implementation.
	resolveZeroPublishedAt bool

	// resolveNPMLockfile, when non-nil, is embedded as NPMLock.Lockfile on
	// the LockHarness Resolve returns — the npm-raw-lockfile-backfill
	// self-heal tests use this to simulate a (fixed) Source.Resolve
	// producing a freshly patched, RawComplete-true raw lockfile, so the
	// test can assert the SELF-HEAL round trip (existing lock incomplete ->
	// Decide forces a fresh Resolve -> persisted lock complete) without
	// depending on the real npm Source's HTTP/backend mechanics.
	resolveNPMLockfile json.RawMessage
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
	publishedAt := FormatTimestamp(fixedNow)
	if f.resolveZeroPublishedAt {
		publishedAt = FormatTimestamp(time.Time{})
	}
	lh := LockHarness{
		Source:         npmSourceName,
		Package:        pkg,
		Version:        version,
		PublishedAt:    publishedAt,
		RecipeRevision: recipeRevision,
	}
	if f.resolveNPMLockfile != nil {
		lh.NPMLock = &NPMLock{LockfileVersion: 3, Lockfile: f.resolveNPMLockfile}
	}
	return lh, nil
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
	// only after this mutation, never before. A Lockfile Resolve already set
	// (resolveNPMLockfile above) is PRESERVED — the real Source.Fetch never
	// touches NPMLock.Lockfile, only the Packages summary map (§8: Lockfile
	// is realization's materialization source, Packages is fetch/cache's).
	var lockfile json.RawMessage
	if h.NPMLock != nil {
		lockfile = h.NPMLock.Lockfile
	}
	h.NPMLock = &NPMLock{LockfileVersion: 3, Lockfile: lockfile, Packages: map[string]NPMLockPackage{
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

	first, err := RefreshHarness(ctxT(t), src, paths, SourceNPM, pkg, fixedNow)
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
	second, err := RefreshHarness(ctxT(t), src, paths, SourceNPM, pkg, later)
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
	if _, err := RefreshHarness(ctxT(t), src, paths, SourceNPM, "pkg", fixedNow); err == nil {
		t.Fatal("expected an error from a failing Source.Refresh")
	}
}

// --- PublishedAt synthesis (pool-source-wiring, §9/§6.6) ---

// TestRefreshHarnessLockedSynthesizesPublishedAtBeforeObserve is a
// regression this work package exists to close (source/archive.go's review
// finding): a candidate with no real publish-history timestamp — the shape
// every candidate an archive Source returns — must never reach
// observations.json with a zero published_at. Note what IS and is NOT
// write-once here (§6.6): first_seen_at is write-once (Observe never moves
// it once set); published_at is NOT (Observe overwrites it on every merge,
// by design, so an upstream republish/reclassify is reflected) — so a zero
// published_at persisted here would not be "permanent" in the write-once
// sense, it would simply keep getting recomputed from whatever is fed in at
// merge time. The synthesis must therefore be deterministic (derived from
// the write-once first_seen_at), not merely "non-zero once" — see
// TestRefreshHarnessLockedPublishedAtSynthesisIsStableAcrossRefreshes for
// the regression that actually matters operationally: a synthesis fed the
// wrong input drifts published_at forward on every refresh, permanently
// quarantining the version instead of merely being zero once. This test
// asserts the synthesis happens BEFORE Observe (not merely somewhere before
// this function returns) by reading observations.json's raw bytes off disk
// and checking literally no zero timestamp appears anywhere in it.
func TestRefreshHarnessLockedSynthesizesPublishedAtBeforeObserve(t *testing.T) {
	paths := testPaths(t)
	src := &fakeSource{candidates: map[string][]Candidate{
		"agy": {{Version: "1.1.10", PublishedAt: time.Time{}}}, // zero PublishedAt, like a real archive candidate
	}}

	out, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", fixedNow)
	if err != nil {
		t.Fatalf("RefreshHarnessLocked: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("candidates = %+v, want exactly one", out)
	}
	if out[0].PublishedAt.IsZero() {
		t.Errorf("returned candidate's PublishedAt is still zero; want it synthesized from FirstSeenAt")
	}
	if !out[0].PublishedAt.Equal(out[0].FirstSeenAt) {
		t.Errorf("PublishedAt = %v, want it to equal the synthesized FirstSeenAt %v", out[0].PublishedAt, out[0].FirstSeenAt)
	}
	if !out[0].PublishedAtSynthesized {
		t.Error("PublishedAtSynthesized = false, want true (wave2-integration-fixes item 1: Eligible must skip minimum_release_age for this candidate)")
	}

	// Regression proof: the persisted observations.json file must contain no
	// zero ("0001-01-01...") timestamp anywhere.
	raw, err := os.ReadFile(paths.Observations)
	if err != nil {
		t.Fatalf("reading observations.json: %v", err)
	}
	if strings.Contains(string(raw), "0001-01-01") {
		t.Errorf("observations.json contains a zero timestamp — pool.Eligible unconditionally treats a zero published_at as ineligible:\n%s", raw)
	}

	// Same assertion via the typed API: every stored observation's
	// published_at must parse as non-zero.
	stored, err := LoadObservations(paths.Observations)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	obs, ok := stored[ObservationKey(string(SourceArchive), "agy", "1.1.10")]
	if !ok {
		t.Fatal("expected an observation for archive/agy/1.1.10")
	}
	publishedAt, err := ParseTimestamp(obs.PublishedAt)
	if err != nil {
		t.Fatalf("parsing stored published_at: %v", err)
	}
	if publishedAt.IsZero() {
		t.Error("stored published_at is zero — the synthesis did not reach observations.json")
	}
}

// TestRefreshHarnessLockedPreservesNonZeroPublishedAt proves the synthesis
// is additive, not destructive: a candidate that already carries a real
// PublishedAt (the ordinary npm case) must pass through completely
// untouched, both in the returned Candidate and in what gets persisted.
func TestRefreshHarnessLockedPreservesNonZeroPublishedAt(t *testing.T) {
	paths := testPaths(t)
	published := fixedNow.Add(-30 * 24 * time.Hour)
	src := &fakeSource{candidates: map[string][]Candidate{
		"@anthropic-ai/claude-code": {{Version: "2.1.220", PublishedAt: published}},
	}}

	out, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceNPM, "@anthropic-ai/claude-code", fixedNow)
	if err != nil {
		t.Fatalf("RefreshHarnessLocked: %v", err)
	}
	if len(out) != 1 || !out[0].PublishedAt.Equal(published) {
		t.Fatalf("candidates = %+v, want PublishedAt unchanged at %v", out, published)
	}
	// wave2-integration-fixes item 1(c): a real (non-synthesized) npm
	// PublishedAt must not be misidentified as synthesized — release-age
	// stays enforced for it.
	if out[0].PublishedAtSynthesized {
		t.Error("PublishedAtSynthesized = true for a real npm PublishedAt, want false")
	}

	stored, err := LoadObservations(paths.Observations)
	if err != nil {
		t.Fatalf("LoadObservations: %v", err)
	}
	obs := stored[ObservationKey(npmSourceName, "@anthropic-ai/claude-code", "2.1.220")]
	if obs.PublishedAt != FormatTimestamp(published) {
		t.Errorf("stored published_at = %q, want %q (unchanged)", obs.PublishedAt, FormatTimestamp(published))
	}
}

// TestRefreshHarnessLockedPublishedAtSynthesisIsStableAcrossRefreshes is
// the BLOCKER regression this fix round exists for: the synthesis must
// read each candidate's own EXISTING first_seen_at, not this call's own
// `now`, or a synthesized published_at silently restamps to "now" on every
// refresh — since published_at (unlike first_seen_at) is NOT write-once,
// nothing else would ever catch this, and `now - published_at` would never
// grow past ~0, permanently quarantining the version under any nonzero
// minimum_release_age. Two refreshes of the SAME still-zero-PublishedAt
// candidate, spaced >= the default 168h minimum_release_age apart, must
// record byte-identical published_at.
func TestRefreshHarnessLockedPublishedAtSynthesisIsStableAcrossRefreshes(t *testing.T) {
	paths := testPaths(t)
	src := &fakeSource{candidates: map[string][]Candidate{
		"agy": {{Version: "1.1.10", PublishedAt: time.Time{}}}, // zero, like a real archive candidate
	}}
	key := ObservationKey(string(SourceArchive), "agy", "1.1.10")

	if _, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", fixedNow); err != nil {
		t.Fatalf("first refresh (T0): %v", err)
	}
	first, err := LoadObservations(paths.Observations)
	if err != nil {
		t.Fatalf("LoadObservations after first refresh: %v", err)
	}
	firstPublished := first[key].PublishedAt
	if firstPublished == "" {
		t.Fatal("no observation recorded after the first refresh")
	}

	// 200h > the default 168h minimum_release_age — exactly the gap the
	// drift bug would have re-zeroed the effective age for.
	later := fixedNow.Add(200 * time.Hour)
	if _, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", later); err != nil {
		t.Fatalf("second refresh (T0+200h): %v", err)
	}
	second, err := LoadObservations(paths.Observations)
	if err != nil {
		t.Fatalf("LoadObservations after second refresh: %v", err)
	}
	secondPublished := second[key].PublishedAt

	if secondPublished != firstPublished {
		t.Errorf("published_at drifted across refreshes: %q (T0) vs %q (T0+200h) — want byte-identical", firstPublished, secondPublished)
	}
}

// TestRefreshThenUpdateSelectsArchiveVersionAfterDefaultQuarantineWindow is
// this fix round's end-to-end proof, under the DEFAULT (unmodified) 168h
// minimum_release_age policy: refresh at T0, refresh again at T0+200h (the
// exact "refresh, wait, refresh again" pattern a developer running a timer
// would produce), then update at T0+200h — the version must be SELECTED,
// not quarantined. Before this fix round, the second refresh would have
// restamped published_at to T0+200h itself, so update's own `now -
// published_at` would read ~0h < 168h and the version would stay
// quarantined forever.
func TestRefreshThenUpdateSelectsArchiveVersionAfterDefaultQuarantineWindow(t *testing.T) {
	paths := testPaths(t)
	// No policy override — the default 168h minimum_release_age applies.
	writeManifest(t, paths, `
schema_version: 1
harnesses:
  agy:
    constraint: ">=1 <2"
`)
	src := &fakeSource{
		candidates:             map[string][]Candidate{"agy": {{Version: "1.1.10", PublishedAt: time.Time{}}}},
		resolveZeroPublishedAt: true,
	}

	if _, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", fixedNow); err != nil {
		t.Fatalf("refresh at T0: %v", err)
	}
	later := fixedNow.Add(200 * time.Hour)
	if _, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", later); err != nil {
		t.Fatalf("refresh at T0+200h: %v", err)
	}

	res, err := UpdateHarnessLocked(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "agy", Package: "agy", Source: SourceArchive, BinName: "agy", Now: later,
	})
	if err != nil {
		t.Fatalf("UpdateHarnessLocked at T0+200h: %v", err)
	}
	if !res.Changed || res.Harness == nil || res.Harness.Version != "1.1.10" {
		t.Fatalf("UpdateResult = %+v, want Changed=true Version=1.1.10 (selected, not quarantined)", res)
	}
}

// TestRefreshThenUpdateSelectsArchiveVersionAtDayZeroUnderDefaultPolicy is
// item 1(a)'s exact regression (wave2-integration-fixes, §9 RULING): a
// FRESH pool, under the DEFAULT (unmodified) policy — 168h
// minimum_release_age, 0h minimum_observation_age — refreshing and then
// IMMEDIATELY updating a real-shaped archive candidate (zero PublishedAt
// from Source, like the real archive Source) must select the version at
// DAY 0, with no wait at all. Before this fix, PublishedAt synthesized as
// FirstSeenAt == "now" made `now - published_at` == 0 < 168h, an
// unavoidable wall no amount of waiting-before-refreshing could ever cross
// (`wraptool harness install agy` failed for 7 days on a fresh pool, a
// regression vs main). TestRefreshThenUpdateSelectsArchiveVersionAfter
// DefaultQuarantineWindow (above) does not catch this: it waits 200h
// before updating, which is >= 168h even under the OLD (unfixed) logic.
func TestRefreshThenUpdateSelectsArchiveVersionAtDayZeroUnderDefaultPolicy(t *testing.T) {
	paths := testPaths(t)
	// No policy override at all — the real default (168h/0h) applies.
	writeManifest(t, paths, `
schema_version: 1
harnesses:
  agy:
    constraint: ">=1 <2"
`)
	src := &fakeSource{
		candidates:             map[string][]Candidate{"agy": {{Version: "1.1.10", PublishedAt: time.Time{}}}},
		resolveZeroPublishedAt: true,
	}

	if _, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", fixedNow); err != nil {
		t.Fatalf("refresh at T0: %v", err)
	}

	res, err := UpdateHarnessLocked(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "agy", Package: "agy", Source: SourceArchive, BinName: "agy", Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarnessLocked at T0 (day 0): %v", err)
	}
	if !res.Changed || res.Harness == nil || res.Harness.Version != "1.1.10" {
		t.Fatalf("UpdateResult = %+v, want Changed=true Version=1.1.10 (selected at day 0, not quarantined)", res)
	}
}

// TestArchiveCandidateQuarantinedByMinimumObservationAge is item 1(b): with
// minimum_observation_age set to 24h (minimum_release_age left at its
// default, which item 1(a) proves is skipped for a synthesized
// PublishedAt), a freshly refreshed archive candidate must be quarantined
// at day 0 — with a countdown consistent with the observation age, not the
// release age — and become eligible once 24h of observation has elapsed.
func TestArchiveCandidateQuarantinedByMinimumObservationAge(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, `
schema_version: 1
policy:
  minimum_observation_age: 24h
harnesses:
  agy:
    constraint: ">=1 <2"
`)
	src := &fakeSource{
		candidates:             map[string][]Candidate{"agy": {{Version: "1.1.10", PublishedAt: time.Time{}}}},
		resolveZeroPublishedAt: true,
	}

	if _, err := RefreshHarnessLocked(ctxT(t), src, paths, SourceArchive, "agy", fixedNow); err != nil {
		t.Fatalf("refresh at T0: %v", err)
	}

	// At T0: quarantined. No prior lock exists, so this is a hard error
	// (Decide's DecisionFail path) naming the remaining wait.
	_, err := UpdateHarnessLocked(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "agy", Package: "agy", Source: SourceArchive, BinName: "agy", Now: fixedNow,
	})
	if err == nil {
		t.Fatal("expected UpdateHarnessLocked at T0 to fail: the candidate is quarantined by minimum_observation_age")
	}
	if !strings.Contains(err.Error(), "quarantined") {
		t.Errorf("error = %v, want it to name the candidate as quarantined", err)
	}

	// StatusHarness's QuarantineRemaining must reflect the OBSERVATION age
	// countdown (24h), not the release age (which is skipped entirely for a
	// synthesized PublishedAt) — this is the "watch out" the brief calls
	// out explicitly: list/StatusHarness must not render a release-age
	// countdown for a synthesized candidate.
	status, err := StatusHarness(paths, "agy", SourceArchive, "agy", fixedNow)
	if err != nil {
		t.Fatalf("StatusHarness at T0: %v", err)
	}
	if !status.QuarantineOK || status.QuarantinedVersion != "1.1.10" {
		t.Fatalf("StatusHarness at T0 = %+v, want QuarantineOK=true QuarantinedVersion=1.1.10", status)
	}
	if status.QuarantineRemaining != 24*time.Hour {
		t.Errorf("QuarantineRemaining at T0 = %v, want exactly 24h (the observation-age remainder)", status.QuarantineRemaining)
	}

	// At T0+24h: eligible.
	later := fixedNow.Add(24 * time.Hour)
	res, err := UpdateHarnessLocked(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "agy", Package: "agy", Source: SourceArchive, BinName: "agy", Now: later,
	})
	if err != nil {
		t.Fatalf("UpdateHarnessLocked at T0+24h: %v", err)
	}
	if !res.Changed || res.Harness == nil || res.Harness.Version != "1.1.10" {
		t.Fatalf("UpdateResult at T0+24h = %+v, want Changed=true Version=1.1.10 (eligible once observation age elapses)", res)
	}
}

// TestUpdateHarnessLockedSynthesizesPublishedAtBeforeLockPersist is
// RefreshHarnessLocked's synthesis test's counterpart at the resolve/lock
// stage (source/archive.go's Resolve doc comment: "until that wiring lands,
// a LockHarness this method returns cannot be persisted at all —
// (*pool.Lock).Validate rejects a zero published_at/first_seen_at
// outright"). fakeSource.resolveZeroPublishedAt simulates a real archive
// Source's Resolve, which always returns a zero-formatted PublishedAt.
func TestUpdateHarnessLockedSynthesizesPublishedAtBeforeLockPersist(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, `
schema_version: 1
policy:
  minimum_release_age: 0h
harnesses:
  agy:
    constraint: ">=1 <2"
`)
	pkg := "agy"
	seedObservationsForSource(t, paths, SourceArchive, pkg, []Candidate{
		{Version: "1.1.10", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})
	src := &fakeSource{resolveZeroPublishedAt: true}

	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "agy", Package: pkg, Source: SourceArchive, BinName: "agy", Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected a fresh resolve to change the lock")
	}
	if res.Harness.PublishedAt == "" || res.Harness.PublishedAt == FormatTimestamp(time.Time{}) {
		t.Errorf("UpdateResult.Harness.PublishedAt = %q, want it synthesized (non-zero)", res.Harness.PublishedAt)
	}
	if res.Harness.PublishedAt != res.Harness.FirstSeenAt {
		t.Errorf("PublishedAt = %q, FirstSeenAt = %q, want them equal (the synthesis sets published_at = first_seen_at)",
			res.Harness.PublishedAt, res.Harness.FirstSeenAt)
	}

	// The persisted lock must independently validate — Lock.Validate rejects
	// a zero published_at outright (validateLockTimestamp), so a successful
	// UpdateHarness call here is itself proof the synthesis ran before the
	// lock was written, not merely before this function returned.
	lockData, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("reading harness-lock.json: %v", err)
	}
	if _, err := UnmarshalLock(lockData); err != nil {
		t.Fatalf("harness-lock.json failed to validate: %v", err)
	}
}

// seedObservationsForSource is seedObservations' source-aware counterpart
// (this file's existing seedObservations hardcodes npmSourceName) — used by
// tests exercising an archive-sourced harness.
func seedObservationsForSource(t *testing.T, paths PoolPaths, kind SourceKind, pkg string, candidates []Candidate) {
	t.Helper()
	obs := make(Observations, len(candidates))
	for _, c := range candidates {
		key := ObservationKey(string(kind), pkg, c.Version)
		if err := obs.Observe(key, FormatTimestamp(c.FirstSeenAt), FormatTimestamp(c.PublishedAt), c.Prerelease, c.Yanked, c.PublishedAtSynthesized); err != nil {
			t.Fatalf("seeding observation %s: %v", key, err)
		}
	}
	if err := MergeObservations(paths.Observations, obs); err != nil {
		t.Fatal(err)
	}
}

// --- recipe_revision: archive vs npm (pool-source-wiring, §8) ---

// TestArchiveRecipeRevisionDistinctFromNPMAndStable proves recipe_revision
// namespacing between sources (§8: "a recipe change to one source must not
// invalidate the other's generations"): archiveRecipeRevision and
// npmRecipeRevision must never collide for the same package identity, and
// archiveRecipeRevision itself must be a pure, stable function of its input
// (repeated calls with the same package produce byte-identical output).
func TestArchiveRecipeRevisionDistinctFromNPMAndStable(t *testing.T) {
	const pkg = "agy"

	npmRev, err := npmRecipeRevision(pkg, "")
	if err != nil {
		t.Fatalf("npmRecipeRevision: %v", err)
	}
	archRev1, err := archiveRecipeRevision(pkg)
	if err != nil {
		t.Fatalf("archiveRecipeRevision: %v", err)
	}
	archRev2, err := archiveRecipeRevision(pkg)
	if err != nil {
		t.Fatalf("archiveRecipeRevision (2nd call): %v", err)
	}

	if archRev1 != archRev2 {
		t.Errorf("archiveRecipeRevision(%q) is not stable: %s vs %s", pkg, archRev1, archRev2)
	}
	if archRev1 == npmRev {
		t.Errorf("archiveRecipeRevision(%q) == npmRecipeRevision(%q) = %s; the two sources must never share a recipe_revision", pkg, pkg, archRev1)
	}

	// recipeRevisionForSource must dispatch to exactly the same functions.
	if got, err := recipeRevisionForSource(SourceNPM, pkg, ""); err != nil || got != npmRev {
		t.Errorf("recipeRevisionForSource(SourceNPM, %q) = %q, %v, want %q, nil", pkg, got, err, npmRev)
	}
	if got, err := recipeRevisionForSource(SourceArchive, pkg, ""); err != nil || got != archRev1 {
		t.Errorf("recipeRevisionForSource(SourceArchive, %q) = %q, %v, want %q, nil", pkg, got, err, archRev1)
	}
	if _, err := recipeRevisionForSource(SourceKind("bogus"), pkg, ""); err == nil {
		t.Error("recipeRevisionForSource with an unknown SourceKind should fail closed, got nil error")
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

// testNodeVersion/testNodeFirstSeen are seedObservations'/seedNodeObservation's
// default node-runtime candidate: comfortably eligible under every policy
// this file exercises (published/first-seen well over a year before
// fixedNow, clearing the 168h maximum minimum_release_age any test here
// sets — node-runtime-observe-resolve WP1).
const testNodeVersion = "22.99.0"

var testNodeFirstSeen = fixedNow.Add(-24 * 400 * time.Hour)

func seedObservations(t *testing.T, paths PoolPaths, pkg string, candidates []Candidate) {
	t.Helper()
	obs := make(Observations, len(candidates)+1)
	for _, c := range candidates {
		key := ObservationKey(npmSourceName, pkg, c.Version)
		if err := obs.Observe(key, FormatTimestamp(c.FirstSeenAt), FormatTimestamp(c.PublishedAt), c.Prerelease, c.Yanked, c.PublishedAtSynthesized); err != nil {
			t.Fatalf("seeding observation %s: %v", key, err)
		}
	}
	// node-runtime-observe-resolve (WP1): UpdateHarnessLocked now needs an
	// ELIGIBLE observed node-line candidate for ANY SourceNPM fresh resolve
	// (selectNodeVersion, orchestrator.go) — every caller of this helper
	// gets one for free, exactly like they already get one for the package
	// without having to think about observations.json's own mechanics.
	nodeKey := nodeObservationKey(defaultNodeLine, testNodeVersion)
	if err := obs.Observe(nodeKey, FormatTimestamp(testNodeFirstSeen), FormatTimestamp(testNodeFirstSeen), false, false, false); err != nil {
		t.Fatalf("seeding node observation %s: %v", nodeKey, err)
	}
	if err := obs.Save(paths.Observations); err != nil {
		t.Fatalf("saving observations: %v", err)
	}
}

// seedNodeObservation records a single, comfortably-eligible node candidate
// directly into observations.json via MergeObservations (not Save) — the
// node-runtime counterpart of seedObservations for a test whose package
// observations come from a REAL RefreshHarnessLocked call rather than
// seedObservations itself (a Save would clobber those).
func seedNodeObservation(t *testing.T, paths PoolPaths) {
	t.Helper()
	key := nodeObservationKey(defaultNodeLine, testNodeVersion)
	obs := Observations{}
	if err := obs.Observe(key, FormatTimestamp(testNodeFirstSeen), FormatTimestamp(testNodeFirstSeen), false, false, false); err != nil {
		t.Fatalf("seeding node observation %s: %v", key, err)
	}
	if err := MergeObservations(paths.Observations, obs); err != nil {
		t.Fatal(err)
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
	recipeRevision, err := npmRecipeRevision(pkg, "")
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

	recipeRevision, err := npmRecipeRevision(pkg, "")
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
		recipeRevision, err := npmRecipeRevision(pkg, "")
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
		recipeRevision, err := npmRecipeRevision(pkg, "")
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
	wantRecipe, err := npmRecipeRevision(pkg, "")
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

// TestUpdateHarnessSelfHealsPhase2RecipeMismatch pins this work package's own
// recipeFormatVersion bump (2 -> 3, §8/§11.2): a wave-2-era lock's
// recipe_revision was computed over {package, recipe_format_version} only —
// recipeRevisionForFormat(t, pkg, 2) reproduces that exact old payload shape,
// since no provisioning_image field existed before this bump. Without the
// format bump, that stale recipe_revision would keep matching forever
// regardless of which provisioning image a fresh resolve now folds in
// (lockMatchesRecipe would never see a difference), so a generation realized
// before the OCI backend's image identity became a locked input could never
// be invalidated by an image upgrade. The v2->v3 bump forces the same
// self-heal path as 1->2 above: the freshly computed (image-aware)
// recipe_revision differs from the persisted v2 one, Decide returns
// DecisionResolveVersion, and the ensuing Resolve+Fetch+Realize re-locks
// under the new, image-aware recipe_revision.
func TestUpdateHarnessSelfHealsPhase2RecipeMismatch(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	const testProvisioningImage = "forge.example/pool-provisioning@sha256:aaaa"
	phase2Recipe := recipeRevisionForFormat(t, pkg, 2)
	wantRecipe, err := npmRecipeRevision(pkg, testProvisioningImage)
	if err != nil {
		t.Fatal(err)
	}
	if wantRecipe == phase2Recipe {
		t.Fatal("test setup bug: the current recipeFormatVersion's hash must differ from format 2's — did the bump get reverted?")
	}

	writeLock(t, paths, &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow.Add(-time.Hour)), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				RecipeRevision: phase2Recipe,
				// Wave-2 shape: recipe_revision predates the provisioning
				// image becoming a hashed input, but is otherwise a
				// perfectly ordinary current-schema lock entry (unlike the
				// Phase-1 test above, this one DOES carry Lockfile) — the
				// mismatch under test here is purely the recipe_revision
				// format bump, not any other schema gap.
				NPMLock: &NPMLock{
					LockfileVersion: 3,
					Lockfile:        json.RawMessage(sampleLockNPMLockfile),
					Packages: map[string]NPMLockPackage{
						"node_modules/bar": {Version: "2.0.0", Resolved: "https://registry.npmjs.org/bar.tgz", Integrity: "sha512-abc", SHA256: "deadbeef"},
					},
				},
			},
		},
	})

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow, // install (Advance: false)
		ProvisioningImage: testProvisioningImage,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true: the format bump must invalidate the wave-2 recipe and force a re-resolve")
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

// TestNPMRecipeRevisionFoldsProvisioningImage pins §8/§11.2's requirement
// that recipe_revision cover the provisioning image identity: two calls
// differing only in provisioningImage must produce different revisions (a
// mutation of the "constant" changes the revision — exactly the property
// that makes an image upgrade force a fresh generation instead of silently
// reusing one built under different assumptions), the empty string is a
// valid (if degenerate) input rather than an error, and the function is
// otherwise pure/stable.
func TestNPMRecipeRevisionFoldsProvisioningImage(t *testing.T) {
	pkg := "@anthropic-ai/claude-code"

	revA1, err := npmRecipeRevision(pkg, "forge.example/pool-provisioning@sha256:aaaa")
	if err != nil {
		t.Fatalf("npmRecipeRevision: %v", err)
	}
	revA2, err := npmRecipeRevision(pkg, "forge.example/pool-provisioning@sha256:aaaa")
	if err != nil {
		t.Fatalf("npmRecipeRevision (2nd call): %v", err)
	}
	if revA1 != revA2 {
		t.Errorf("npmRecipeRevision(%q, image) is not stable: %s vs %s", pkg, revA1, revA2)
	}

	revB, err := npmRecipeRevision(pkg, "forge.example/pool-provisioning@sha256:bbbb")
	if err != nil {
		t.Fatalf("npmRecipeRevision (different image): %v", err)
	}
	if revA1 == revB {
		t.Errorf("npmRecipeRevision(%q, ...) did not change when the provisioning image changed: %s", pkg, revA1)
	}

	revEmpty, err := npmRecipeRevision(pkg, "")
	if err != nil {
		t.Fatalf("npmRecipeRevision (empty image): %v", err)
	}
	if revEmpty == revA1 {
		t.Errorf("npmRecipeRevision(%q, \"\") unexpectedly matched a non-empty image's revision", pkg)
	}
}

// TestNPMRecipeRevisionGoldenValue pins npmRecipeRevision's exact output
// (fix-round RULING, item 6): a hard-coded sha256 literal, not just the
// mutation-sensitivity properties TestNPMRecipeRevisionFoldsProvisioningImage
// above already checks. This is what actually catches a silent change to the
// hashed struct's field names, field order, or JSON tags (encoding/json's
// output is order-preserving over struct field declaration order, so
// reordering the Package/ProvisioningImage/Format fields — or renaming any
// one JSON tag — changes the hash even though every other property
// (stability, mutation-sensitivity) would still hold) — the 2->3 bump this
// work package made was exactly this kind of change, and it shipped with no
// golden-value test pinning the new format at all.
func TestNPMRecipeRevisionGoldenValue(t *testing.T) {
	// Format 4 (issue #38: node runtime bump must invalidate existing locks).
	const wantGolden = "sha256:63b70ceacb0184d3bc7689d221ed800e44114aa664385e6ef53cbe1bbb92f7e5"
	got, err := npmRecipeRevision("x", "img")
	if err != nil {
		t.Fatalf("npmRecipeRevision: %v", err)
	}
	if got != wantGolden {
		t.Errorf(`npmRecipeRevision("x", "img") = %q, want golden value %q — if this change is`+
			` intentional (a deliberate recipe_format_version/field-shape bump), update wantGolden`+
			` here AND bump recipeFormatVersion with a doc-comment entry explaining why`, got, wantGolden)
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

	status, err := StatusHarness(paths, "claude", SourceNPM, pkg, fixedNow)
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
	status, err := StatusHarness(paths, "claude", SourceNPM, "@anthropic-ai/claude-code", fixedNow)
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
	seedNodeObservation(t, paths)

	done := make(chan error, 1)
	go func() {
		ctx := ctxT(t)
		if _, err := RefreshHarness(ctx, src, paths, SourceNPM, pkg, fixedNow); err != nil {
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
	seedNodeObservation(t, paths)

	done := make(chan error, 1)
	go func() {
		ctx := ctxT(t)
		release, err := LockPool(ctx, paths)
		if err != nil {
			done <- err
			return
		}
		defer release()
		if _, err := RefreshHarnessLocked(ctx, src, paths, SourceNPM, pkg, fixedNow); err != nil {
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

// TestUpdateHarnessUpToDateRepairsMissingActivation reproduces the
// wave-1-to-wave-2 upgrade pool found on a real developer machine: the
// generation exists on disk and the lock records its digest (wave 1 realized
// it), so Decide correctly returns DecisionUpToDate — but activation did not
// exist when the generation was built, so there is no pointer and no
// dispatcher, and before this repair nothing on the up-to-date path would
// ever write them. §10 includes the ACTIVE receipt in install's idempotence
// contract, so the up-to-date path must repair activation in place, without
// calling Source.Resolve or Source.Realize.
func TestUpdateHarnessUpToDateRepairsMissingActivation(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	key, err := HostPlatformKey()
	if err != nil {
		t.Fatal(err)
	}

	// Wave-1-shaped pool: realized generation + lock digest, no pointer.
	genID, digest := seedExistingGeneration(t, paths, "claude", "2.1.220", key, "#!/bin/sh\nexec true\n")
	rr, err := npmRecipeRevision(pkg, "")
	if err != nil {
		t.Fatal(err)
	}
	h := baseRealizeLockHarness()
	h.Package = pkg
	h.Version = "2.1.220"
	h.RecipeRevision = rr
	h.Generations = map[PlatformKey]string{key: digest}
	seedRealizeLock(t, paths, h)

	src := &fakeSource{}
	res, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if res.RealizeErr != nil {
		t.Fatalf("RealizeErr = %v, want nil", res.RealizeErr)
	}
	if want := "activated " + genID; res.Note != want {
		t.Errorf("Note = %q, want %q", res.Note, want)
	}
	if src.resolveCalls != 0 || src.realizeCalls != 0 {
		t.Errorf("source calls = resolve:%d realize:%d, want 0/0 — repair must not resolve or rebuild", src.resolveCalls, src.realizeCalls)
	}
	if active, ok := ActiveGeneration(paths, "claude", key); !ok || active != genID {
		t.Errorf("ActiveGeneration = %q ok=%v, want %q", active, ok, genID)
	}
	if fi, statErr := os.Stat(filepath.Join(paths.Runtime, "bin", "claude")); statErr != nil {
		t.Errorf("dispatcher missing after repair: %v", statErr)
	} else if fi.Mode().Perm() != 0o755 {
		t.Errorf("dispatcher mode = %v, want 0755", fi.Mode().Perm())
	}

	// Second run: pointer already correct — silent no-op, no duplicate note.
	res2, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", BinName: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("second UpdateHarness: %v", err)
	}
	if res2.Note != "" {
		t.Errorf("second run Note = %q, want empty (no re-repair)", res2.Note)
	}
}
