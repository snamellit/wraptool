package pool

import (
	"context"
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
	lastResolve  struct{ pkg, version, nodeVersion, recipeRevision string }

	fetchErr        error
	fetchCalls      int
	fetchedVersions []string
}

func (f *fakeSource) Refresh(_ context.Context, pkg string) ([]Candidate, error) {
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.candidates[pkg], nil
}

func (f *fakeSource) Resolve(_ context.Context, _ backend.Backend, pkg, version, _, nodeVersion, recipeRevision string) (LockHarness, error) {
	f.resolveCalls++
	f.lastResolve = struct{ pkg, version, nodeVersion, recipeRevision string }{pkg, version, nodeVersion, recipeRevision}
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
	// package entry's pool-local sha256 cache-address field as objects are
	// admitted to cache. UpdateHarness must persist the lock only after this
	// mutation, never before.
	h.NPMLock = &NPMLock{LockfileVersion: 3, Packages: map[string]NPMLockPackage{
		h.Package: {Version: h.Version, Resolved: "https://registry.npmjs.org/x.tgz", Integrity: "sha512-x", SHA256: "deadbeef"},
	}}
	return nil
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
		Cache:        filepath.Join(dir, "cache"),
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
	lh, changed, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true for a first resolve")
	}
	if lh.Version != "2.1.220" {
		t.Errorf("selected version = %q, want the newest ELIGIBLE 2.1.220 (2.1.224 is quarantined, 3.0.0 fails the constraint)", lh.Version)
	}
	if src.resolveCalls != 1 || src.fetchCalls != 1 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want exactly one of each", src.resolveCalls, src.fetchCalls)
	}
}

func TestUpdateHarnessNothingEligibleErrors(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.224", PublishedAt: fixedNow.Add(-1 * time.Hour), FirstSeenAt: fixedNow.Add(-1 * time.Hour)},
	})

	src := &fakeSource{}
	_, _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: pkg, Now: fixedNow,
	})
	if err == nil {
		t.Fatal("expected an error when nothing is eligible")
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
	// — including the recipe revision, which idempotence also requires to
	// match (§8/§10).
	recipeRevision, err := npmRecipeRevision(pkg)
	if err != nil {
		t.Fatal(err)
	}
	existing := &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   FormatTimestamp(fixedNow.Add(-time.Hour)),
		PolicyHash:    "sha256:whatever",
		Harnesses: map[string]LockHarness{
			"claude": {Source: npmSourceName, Package: pkg, Version: "2.1.220", PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), RecipeRevision: recipeRevision},
		},
	}
	data, err := existing.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{}
	lh, changed, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: pkg, Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if changed {
		t.Error("expected changed=false when manifest and lock already agree")
	}
	if lh.Version != "2.1.220" {
		t.Errorf("Version = %q, want the already-locked 2.1.220", lh.Version)
	}
	if src.resolveCalls != 0 || src.fetchCalls != 0 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want none on an idempotent install/update", src.resolveCalls, src.fetchCalls)
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
	if _, _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: pkg, Now: fixedNow,
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

	// Pre-seed a lock for a DIFFERENT version, so selection below does not
	// take the idempotent shortcut and actually calls Resolve/Fetch.
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
	_, _, err = UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: pkg, Now: fixedNow,
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

func TestUpdateHarnessAllowYoungRequiresReason(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	src := &fakeSource{}
	_, _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: "pkg", Now: fixedNow, AllowYoung: true, Reason: "",
	})
	if err == nil {
		t.Fatal("expected an error for --allow-young with an empty reason")
	}
	if !strings.Contains(err.Error(), "reason") {
		t.Errorf("error should mention the missing reason, got: %v", err)
	}
}

func TestUpdateHarnessAllowYoungBypassesQuarantineAndAudits(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"

	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.224", PublishedAt: fixedNow.Add(-1 * time.Hour), FirstSeenAt: fixedNow.Add(-1 * time.Hour)},
	})

	src := &fakeSource{}
	lh, changed, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: pkg, Now: fixedNow,
		AllowYoung: true, Reason: "CVE-2026-00000",
	})
	if err != nil {
		t.Fatalf("UpdateHarness with --allow-young: %v", err)
	}
	if !changed || lh.Version != "2.1.224" {
		t.Fatalf("got changed=%v version=%q, want changed=true version=2.1.224", changed, lh.Version)
	}
	if src.resolveCalls != 1 || src.fetchCalls != 1 {
		t.Errorf("resolveCalls=%d fetchCalls=%d, want exactly one of each", src.resolveCalls, src.fetchCalls)
	}

	auditData, err := os.ReadFile(paths.Audit)
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	var evt AuditEvent
	if err := json.Unmarshal(bytesFirstLine(auditData), &evt); err != nil {
		t.Fatalf("parsing audit event: %v", err)
	}
	if evt.Type != "allow_young" {
		t.Errorf("audit event type = %q, want allow_young", evt.Type)
	}
	if evt.Fields["reason"] != "CVE-2026-00000" {
		t.Errorf("audit event reason = %v, want CVE-2026-00000", evt.Fields["reason"])
	}
}

func bytesFirstLine(b []byte) []byte {
	if i := strings.IndexByte(string(b), '\n'); i >= 0 {
		return b[:i]
	}
	return b
}

func TestUpdateHarnessErrorsWithoutManifestEntry(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, "schema_version: 1\n")
	src := &fakeSource{}
	_, _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{
		Harness: "claude", Package: "pkg", Now: fixedNow,
	})
	if err == nil {
		t.Fatal("expected an error when the harness has no manifest entry")
	}
}

// --- SyncHarness ---

func TestSyncHarnessFetchesButNeverResolves(t *testing.T) {
	paths := testPaths(t)
	lock := &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   FormatTimestamp(fixedNow),
		PolicyHash:    "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {Source: npmSourceName, Package: "@anthropic-ai/claude-code", Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)), RecipeRevision: "sha256:y"},
		},
	}
	data, err := lock.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		t.Fatal(err)
	}

	src := &fakeSource{}
	lh, err := SyncHarness(ctxT(t), src, paths, "claude")
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
}

func TestSyncHarnessErrorsWithoutLockEntry(t *testing.T) {
	paths := testPaths(t)
	src := &fakeSource{}
	if _, err := SyncHarness(ctxT(t), src, paths, "claude"); err == nil {
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
	lock := &Lock{
		SchemaVersion: LockSchemaVersion, GeneratedAt: FormatTimestamp(fixedNow), PolicyHash: "sha256:x",
		Harnesses: map[string]LockHarness{
			"claude": {
				Source: npmSourceName, Package: pkg, Version: "2.1.220",
				PublishedAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
				FirstSeenAt: FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
			},
		},
	}
	data, err := lock.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(paths.Lock, data, 0o644); err != nil {
		t.Fatal(err)
	}

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
		_, _, err := UpdateHarness(ctx, src, fakeBackend{}, paths, UpdateOptions{
			Harness: "claude", Package: pkg, Now: fixedNow,
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

// TestPolicyHashChangesInvalidateIdempotence documents that idempotence is
// keyed on (package, version) only, per this package's UpdateHarness doc
// comment — a policy-only change does not, by itself, cause Resolve/Fetch to
// re-run for an already-selected version. This pins current behavior; it is
// not a normative requirement of §10, which is silent on this exact edge.
func TestPolicyHashChangesInvalidateIdempotence(t *testing.T) {
	paths := testPaths(t)
	writeManifest(t, paths, claudeManifest)
	pkg := "@anthropic-ai/claude-code"
	seedObservations(t, paths, pkg, []Candidate{
		{Version: "2.1.220", PublishedAt: fixedNow.Add(-200 * time.Hour), FirstSeenAt: fixedNow.Add(-200 * time.Hour)},
	})

	src := &fakeSource{}
	if _, _, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{Harness: "claude", Package: pkg, Now: fixedNow}); err != nil {
		t.Fatalf("first UpdateHarness: %v", err)
	}
	if src.resolveCalls != 1 {
		t.Fatalf("resolveCalls after first update = %d, want 1", src.resolveCalls)
	}

	// Same manifest, same observations, called again: still idempotent.
	if _, changed, err := UpdateHarness(ctxT(t), src, fakeBackend{}, paths, UpdateOptions{Harness: "claude", Package: pkg, Now: fixedNow.Add(time.Hour)}); err != nil {
		t.Fatalf("second UpdateHarness: %v", err)
	} else if changed {
		t.Error("second UpdateHarness with nothing changed should be idempotent")
	}
	if src.resolveCalls != 1 {
		t.Errorf("resolveCalls after second (idempotent) update = %d, want still 1", src.resolveCalls)
	}
}
