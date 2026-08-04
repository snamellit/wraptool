package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const gcTestHarness = "claude"

var gcTestKey = PlatformLinuxAMD64Glibc

// gcWriteLock writes lockHarnesses as harness-lock.json at paths.Lock, via
// the same Marshal/WriteFileAtomic path production code uses.
func gcWriteLock(t *testing.T, paths PoolPaths, harnesses map[string]LockHarness) {
	t.Helper()
	l := &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   FormatTimestamp(fixedNow),
		PolicyHash:    "sha256:" + strings.Repeat("0", 64),
		Harnesses:     harnesses,
	}
	writeLock(t, paths, l)
}

// TestGCRetainsGenerationReferencedByLockButNotActive pins rule (b) — the
// lock-match retention rule. This test used to be VACUOUS: with only one
// generation in its (harness, platform) group, rule (c)'s "2 most
// recently created" trivially retains it even with rule (b) deleted
// entirely (verified: deleting the rule (b) block from
// retainedGenerationIDs still passed this test unmodified). Fixed by
// giving the group 4 generations total — the lock-recorded one made the
// OLDEST by ModTime (so rule (c)'s top-2 cannot cover it), plus 3 newer,
// unrecorded, unpointed spares (so rule (c) has real work to do in the
// same run: it retains only the 2 newest of THOSE, proving gc is actually
// removing something here, not just trivially retaining everything).
func TestGCRetainsGenerationReferencedByLockButNotActive(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)

	genID := seedActivateGeneration(t, paths, gcTestHarness, "2.1.220", gcTestKey, "in-lock-not-active")
	if err := os.Chtimes(runtimeGenerationDir(paths.Runtime, genID), fixedNow.Add(-time.Hour), fixedNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	digest := digestFromGenerationID(t, paths, genID)
	gcWriteLock(t, paths, map[string]LockHarness{
		gcTestHarness: {
			Source: npmSourceName, Package: "@example/claude", Version: "2.1.220",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Generations:    map[PlatformKey]string{gcTestKey: digest},
		},
	})
	// Not activated (no pointer file at all): retention must come entirely
	// from rule (b).

	// 3 newer, unrecorded, unpointed spares in the SAME (harness, platform)
	// group — rule (c) retains only its 2 most recent, so the oldest of
	// these three is removed in the same run.
	spareIDs := seedGCSpareGenerations(t, paths, gcTestHarness, gcTestKey, 3)

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedGenerations != 1 || report.RetainedGenerations != 3 {
		t.Errorf("report = %+v, want 1 removed (the oldest spare) / 3 retained (the lock-recorded generation + the 2 newest spares)", report)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, genID)); err != nil {
		t.Errorf("expected the lock-referenced (oldest, un-pointed) generation to survive GC via rule (b): %v", err)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, spareIDs[0])); !os.IsNotExist(err) {
		t.Errorf("expected the oldest spare %s to be removed by rule (c), stat err = %v", spareIDs[0], err)
	}
	for _, id := range spareIDs[1:] {
		if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, id)); err != nil {
			t.Errorf("expected spare %s to survive via rule (c)'s top-2: %v", id, err)
		}
	}
}

func TestGCRetainsActiveGenerationNotInLock(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genID := seedActivateGeneration(t, paths, gcTestHarness, "2.1.220", gcTestKey, "active-not-in-lock")
	if err := ActivateGeneration(paths, gcTestHarness, gcTestKey, genID); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}
	// No lock at all: the lock doesn't even mention this harness/generation
	// (e.g. after a manual lock edit) — the pointer alone must retain it.

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedGenerations != 0 || report.RetainedGenerations != 1 {
		t.Errorf("report = %+v, want 0 removed / 1 retained generation", report)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, genID)); err != nil {
		t.Errorf("expected the active generation to survive GC: %v", err)
	}
}

// seedGCSpareGenerations builds n generations for (harnessName, key), none
// referenced by any pointer or lock ("spares" — §15's third retention
// rule), with strictly increasing directory ModTimes (ids[0] oldest,
// ids[n-1] newest) so a test can assert exactly which ones GC's "2
// most-recently-created" rule keeps.
func seedGCSpareGenerations(t *testing.T, paths PoolPaths, harnessName string, key PlatformKey, n int) (ids []string) {
	t.Helper()
	ids = make([]string, n)
	for i := 0; i < n; i++ {
		id := seedActivateGeneration(t, paths, harnessName, fmt.Sprintf("%d.0.0", i+1), key, fmt.Sprintf("spare-%d", i))
		mtime := fixedNow.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(runtimeGenerationDir(paths.Runtime, id), mtime, mtime); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

// TestGCRetainsTwoMostRecentSparesRemovesOlder is §15's third retention
// rule: among generations retained by NEITHER a pointer NOR the current
// lock ("spares"), GC keeps the 2 most recently created per
// (harness, platform) and removes the rest — a single spare (the pre-fix
// behavior this test used to cover) is retained trivially, so this needs
// 3+ spares to actually exercise the boundary.
func TestGCRetainsTwoMostRecentSparesRemovesOlder(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	ids := seedGCSpareGenerations(t, paths, gcTestHarness, gcTestKey, 3) // ids[0] oldest .. ids[2] newest
	// None activated, none recorded in any lock.

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedGenerations != 1 || report.RetainedGenerations != 2 {
		t.Errorf("report = %+v, want 1 removed / 2 retained (the 2 most recently created spares stay)", report)
	}
	if report.FreedBytes <= 0 {
		t.Errorf("FreedBytes = %d, want > 0", report.FreedBytes)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, ids[0])); !os.IsNotExist(err) {
		t.Errorf("expected the OLDEST spare %s to be removed, stat err = %v", ids[0], err)
	}
	for _, id := range ids[1:] {
		if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, id)); err != nil {
			t.Errorf("expected the newer spare %s to survive GC (top-2 retention): %v", id, err)
		}
	}
}

// TestGCRetainsSupersededVersionAsSpareEnablingRollback is the REQUIRED
// regression proving §15's third retention rule fixes review finding 1:
// install (realize v1, activate) -> update (realize v2, activate; the fresh
// resolve REPLACES the whole lock entry, so harness-lock.json now records
// only v2's digest, exactly as UpdateHarnessLocked's DecisionResolveVersion
// path does, orchestrator.go) -> gc -> v1 must still be present (as a
// retained spare, not because the lock remembers it — it doesn't anymore)
// -> `rollback --to v1` must still succeed, with no network access.
func TestGCRetainsSupersededVersionAsSpareEnablingRollback(t *testing.T) {
	paths := testPaths(t)
	src := &fakeSource{}

	h1 := LockHarness{
		Source: npmSourceName, Package: "@example/claude", Version: "1.0.0",
		PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
		RecipeRevision: "sha256:" + strings.Repeat("1", 64),
	}
	seedRealizeLock(t, paths, h1)
	receipt1, err := RealizeGeneration(ctxT(t), src, fakeBackend{}, paths, gcTestHarness, gcTestHarness, &h1, gcTestKey)
	if err != nil {
		t.Fatalf("realizing v1: %v", err)
	}
	gen1 := mustGenerationID(t, gcTestHarness, "1.0.0", gcTestKey, receipt1.ManifestDigest)

	// Simulate `update`: a fresh resolve replaces the WHOLE lock entry with
	// no Generations recorded yet — v1's digest is gone from the lock the
	// instant this write lands, exactly like a real update's fresh
	// LockHarness from Source.Resolve.
	h2 := LockHarness{
		Source: npmSourceName, Package: "@example/claude", Version: "2.0.0",
		PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
		RecipeRevision: "sha256:" + strings.Repeat("1", 64),
	}
	gcWriteLock(t, paths, map[string]LockHarness{gcTestHarness: h2})
	receipt2, err := RealizeGeneration(ctxT(t), src, fakeBackend{}, paths, gcTestHarness, gcTestHarness, &h2, gcTestKey)
	if err != nil {
		t.Fatalf("realizing v2: %v", err)
	}
	gen2 := mustGenerationID(t, gcTestHarness, "2.0.0", gcTestKey, receipt2.ManifestDigest)

	lockAfterUpdate := readRealizeLock(t, paths)
	if lockAfterUpdate.Generations[gcTestKey] != receipt2.ManifestDigest {
		t.Fatalf("lock Generations[%s] = %+v, want only v2's digest recorded", gcTestKey, lockAfterUpdate.Generations)
	}
	active, ok := ActiveGeneration(paths, gcTestHarness, gcTestKey)
	if !ok || active != gen2 {
		t.Fatalf("active generation = (%q, %v), want v2 (%q) active after update", active, ok, gen2)
	}

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedGenerations != 0 {
		t.Errorf("RemovedGenerations = %d, want 0 (v1 must survive gc as a retained spare)", report.RemovedGenerations)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, gen1)); err != nil {
		t.Fatalf("v1's generation %s was removed by gc despite the lock no longer recording it: %v", gen1, err)
	}

	got, err := RollbackHarnessLocked(paths, gcTestHarness, gcTestKey, "1.0.0")
	if err != nil {
		t.Fatalf("RollbackHarnessLocked to v1 after gc: %v", err)
	}
	if got != gen1 {
		t.Errorf("rolled back to %q, want v1's generation %q", got, gen1)
	}
}

// TestGCLockRetentionMatchesByReceiptNotReconstructedID is review finding
// 2's arm64 scenario: a harness locked at a NEWER top-level Version for one
// platform, whose OTHER platform is still only realized at an OLDER version
// — GC's lock-retention rule must match by reading each generation's OWN
// receipt (harness/platform/digest), never by reconstructing a generation
// id from (harnessName, the lock's current top-level Version, digest),
// which would embed the wrong version for the older platform's generation
// and never match its real directory name.
// TestGCLockRetentionMatchesByReceiptNotReconstructedID is review finding
// 2's arm64 scenario: a harness locked at a NEWER top-level Version for one
// platform, whose OTHER platform is still only realized at an OLDER version
// — GC's lock-retention rule must match by reading each generation's OWN
// receipt (harness/platform/digest), never by reconstructing a generation
// id from (harnessName, the lock's current top-level Version, digest),
// which would embed the wrong version for the older platform's generation
// and never match its real directory name.
//
// Like TestGCRetainsGenerationReferencedByLockButNotActive, this test used
// to be VACUOUS: with only one generation per (harness, platform) group,
// rule (c) trivially retained both even with rule (b) deleted entirely
// (verified the same way). Fixed the same way: each lock-recorded
// generation is made the OLDEST in its own group, with 3 newer, unrecorded,
// unpointed spares alongside it, so rule (c) cannot cover the lock-recorded
// one and has real removal work to do in both groups in the same run.
func TestGCLockRetentionMatchesByReceiptNotReconstructedID(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)

	// arm64 was realized (and is still locked) at the OLDER version 1.0.0.
	genArm64 := seedActivateGeneration(t, paths, gcTestHarness, "1.0.0", PlatformLinuxARM64Glibc, "arm64-old-version")
	if err := os.Chtimes(runtimeGenerationDir(paths.Runtime, genArm64), fixedNow.Add(-time.Hour), fixedNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	digestArm64 := digestFromGenerationID(t, paths, genArm64)
	arm64Spares := seedGCSpareGenerations(t, paths, gcTestHarness, PlatformLinuxARM64Glibc, 3)

	// amd64 was realized at the NEWER version 2.0.0 (this is what makes
	// h.Version, the lock's TOP-LEVEL field, "2.0.0" overall).
	genAmd64 := seedActivateGeneration(t, paths, gcTestHarness, "2.0.0", PlatformLinuxAMD64Glibc, "amd64-new-version")
	if err := os.Chtimes(runtimeGenerationDir(paths.Runtime, genAmd64), fixedNow.Add(-time.Hour), fixedNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	digestAmd64 := digestFromGenerationID(t, paths, genAmd64)
	amd64Spares := seedGCSpareGenerations(t, paths, gcTestHarness, PlatformLinuxAMD64Glibc, 3)

	gcWriteLock(t, paths, map[string]LockHarness{
		gcTestHarness: {
			Source: npmSourceName, Package: "@example/claude", Version: "2.0.0",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Generations: map[PlatformKey]string{
				PlatformLinuxARM64Glibc: digestArm64,
				PlatformLinuxAMD64Glibc: digestAmd64,
			},
		},
	})
	// Neither lock-recorded generation is pointed to by any active pointer
	// — their retention must come entirely from rule (b): each is the
	// OLDEST in its own group, so rule (c)'s top-2 cannot cover it.

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	// Per group: the lock-recorded generation (rule b) + the 2 newest
	// spares (rule c) retained, the oldest spare removed; two groups.
	if report.RemovedGenerations != 2 || report.RetainedGenerations != 6 {
		t.Errorf("report = %+v, want 2 removed (the oldest spare per group) / 6 retained (2 lock-recorded + 2x2 newest spares)", report)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, genArm64)); err != nil {
		t.Errorf("arm64's OLDER-version, lock-recorded generation %s was incorrectly removed (review finding 2): %v", genArm64, err)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, genAmd64)); err != nil {
		t.Errorf("amd64's lock-recorded generation %s was incorrectly removed: %v", genAmd64, err)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, arm64Spares[0])); !os.IsNotExist(err) {
		t.Errorf("expected arm64's oldest spare %s to be removed by rule (c), stat err = %v", arm64Spares[0], err)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, amd64Spares[0])); !os.IsNotExist(err) {
		t.Errorf("expected amd64's oldest spare %s to be removed by rule (c), stat err = %v", amd64Spares[0], err)
	}
}

// TestGCErrorsLoudlyOnUnreadablePointer is the ruling for corrupt/unreadable
// pointer files: GC holds the exclusive pool lock, so an existing pointer
// that fails to read is corruption, not a benign race, and gc must fail
// loudly (naming the pointer path) rather than silently continuing —
// continuing risks deleting the very generation that pointer references.
// Nothing is removed on this failure.
// TestGCErrorsLoudlyOnUnreadablePointer seeds 3 spares in the (harness,
// platform) group the corrupted pointer names — deliberately enough that,
// absent the pointer-read guard, a real gc run WOULD remove the oldest of
// the three (rule (c) retains only the 2 newest). "Nothing removed" below
// therefore proves the loud failure actually PREVENTED a removal that
// would otherwise have happened, not merely that there was nothing to
// remove in the first place (a single-generation fixture couldn't
// distinguish the two).
func TestGCErrorsLoudlyOnUnreadablePointer(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	ids := seedGCSpareGenerations(t, paths, gcTestHarness, gcTestKey, 3)

	// A directory in place of the pointer FILE reliably makes ReadPointer's
	// os.ReadFile fail, portably, without permission tricks.
	ptrPath := pointerFile(paths.Runtime, gcTestKey, gcTestHarness)
	if err := os.MkdirAll(ptrPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := GCPoolLocked(paths, false); err == nil {
		t.Fatal("expected gc to fail loudly on an unreadable pointer file")
	} else if !strings.Contains(err.Error(), ptrPath) {
		t.Errorf("error = %q, want it to name the pointer path %q", err.Error(), ptrPath)
	}

	for _, id := range ids {
		if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, id)); err != nil {
			t.Errorf("expected generation %s to survive a failed gc — including %s, which a real (non-erroring) run WOULD have removed: %v", id, ids[0], err)
		}
	}
}

func TestGCCacheObjectRetentionAndDryRun(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	if err := os.MkdirAll(paths.Cache, 0o755); err != nil {
		t.Fatal(err)
	}
	retainedDigest := strings.Repeat("a", 64)
	orphanDigest := strings.Repeat("b", 64)
	for _, d := range []string{retainedDigest, orphanDigest} {
		if err := os.WriteFile(filepath.Join(paths.Cache, d), []byte("payload-"+d), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	gcWriteLock(t, paths, map[string]LockHarness{
		gcTestHarness: {
			Source: npmSourceName, Package: "@example/claude", Version: "1.0.0",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			NPMLock: &NPMLock{
				LockfileVersion: 3,
				Packages: map[string]NPMLockPackage{
					"@example/claude": {Version: "1.0.0", Resolved: "https://registry.npmjs.org/x.tgz", Integrity: "sha512-x", SHA256: retainedDigest},
				},
			},
		},
	})

	// Dry run: nothing removed, but the report still reflects what WOULD go.
	dryReport, err := GCPoolLocked(paths, true)
	if err != nil {
		t.Fatalf("dry-run GCPoolLocked: %v", err)
	}
	if !dryReport.DryRun {
		t.Error("dryReport.DryRun = false, want true")
	}
	if dryReport.RemovedCacheObjects != 1 || dryReport.RetainedCacheObjects != 1 {
		t.Errorf("dry-run report = %+v, want 1 removed / 1 retained cache object", dryReport)
	}
	for _, d := range []string{retainedDigest, orphanDigest} {
		if _, err := os.Stat(filepath.Join(paths.Cache, d)); err != nil {
			t.Errorf("dry run must not remove %s: %v", d, err)
		}
	}

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedCacheObjects != 1 || report.RetainedCacheObjects != 1 {
		t.Errorf("report = %+v, want 1 removed / 1 retained cache object", report)
	}
	if _, err := os.Stat(filepath.Join(paths.Cache, retainedDigest)); err != nil {
		t.Errorf("expected the lock-referenced cache object to survive GC: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.Cache, orphanDigest)); !os.IsNotExist(err) {
		t.Errorf("expected the orphaned cache object to be removed, stat err = %v", err)
	}
}

func TestGCRuntimeArtifactCacheDigestIsRetained(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	if err := os.MkdirAll(paths.Cache, 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeDigest := strings.Repeat("c", 64)
	if err := os.WriteFile(filepath.Join(paths.Cache, runtimeDigest), []byte("node runtime tarball"), 0o644); err != nil {
		t.Fatal(err)
	}

	gcWriteLock(t, paths, map[string]LockHarness{
		gcTestHarness: {
			Source: npmSourceName, Package: "@example/claude", Version: "1.0.0",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Runtime: &LockRuntime{
				Name: "node", Version: "22.11.0",
				Artifacts: map[PlatformKey]LockRuntimeArtifact{
					gcTestKey: {URL: "https://nodejs.org/dist/x.tar.xz", SHA256: runtimeDigest},
				},
			},
		},
	})

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedCacheObjects != 0 || report.RetainedCacheObjects != 1 {
		t.Errorf("report = %+v, want 0 removed / 1 retained cache object", report)
	}
}

// TestGCArchiveArtifactCacheDigestIsRetained mirrors
// TestGCRuntimeArtifactCacheDigestIsRetained for LockHarness.Archive
// (wave2-integration-fixes item 2): retainedCacheDigests originally walked
// only NPMLock.Packages and Runtime.Artifacts, so GC deleted a still-locked
// archive-sourced harness's (e.g. agy) cache object.
func TestGCArchiveArtifactCacheDigestIsRetained(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	if err := os.MkdirAll(paths.Cache, 0o755); err != nil {
		t.Fatal(err)
	}
	archiveDigest := strings.Repeat("d", 64)
	if err := os.WriteFile(filepath.Join(paths.Cache, archiveDigest), []byte("agy archive artifact"), 0o644); err != nil {
		t.Fatal(err)
	}

	gcWriteLock(t, paths, map[string]LockHarness{
		gcTestHarness: {
			Source: archiveSourceName, Package: gcTestHarness, Version: "1.0.0",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Archive: &LockArchive{
				Artifacts: map[PlatformKey]LockArchiveArtifact{
					gcTestKey: {URL: "https://example.invalid/agy.tar.gz", SHA512: strings.Repeat("a", 128), SHA256: archiveDigest},
				},
			},
		},
	})

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedCacheObjects != 0 || report.RetainedCacheObjects != 1 {
		t.Errorf("report = %+v, want 0 removed / 1 retained cache object", report)
	}
}

func TestGCRemovesAllStagingTxnDirsUnderLock(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	for _, txn := range []string{"txn-abc", "txn-def"} {
		dir := filepath.Join(paths.Staging, txn)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("debris"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedStagingDirs != 2 {
		t.Errorf("RemovedStagingDirs = %d, want 2", report.RemovedStagingDirs)
	}
	entries, err := os.ReadDir(paths.Staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging entries after GC = %v, want none", entries)
	}
}

func TestGCDryRunTakesLockButRemovesNothing(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	// 3 spares so one is actually a removal CANDIDATE (a lone spare is
	// trivially retained by the top-2 rule, which would make this test
	// vacuous — see TestGCRetainsTwoMostRecentSparesRemovesOlder).
	ids := seedGCSpareGenerations(t, paths, gcTestHarness, gcTestKey, 3)
	stagingDir := filepath.Join(paths.Staging, "txn-x")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := GCPoolLocked(paths, true)
	if err != nil {
		t.Fatalf("GCPoolLocked (dry run): %v", err)
	}
	if !report.DryRun {
		t.Error("DryRun = false, want true")
	}
	if report.RemovedGenerations != 1 || report.RemovedStagingDirs != 1 {
		t.Errorf("dry-run report = %+v, want 1 removed generation and 1 removed staging dir reported", report)
	}
	for _, id := range ids {
		if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, id)); err != nil {
			t.Errorf("dry run must not remove any generation (including %s, which a real run WOULD remove): %v", id, err)
		}
	}
	if _, err := os.Stat(stagingDir); err != nil {
		t.Errorf("dry run must not remove the staging dir: %v", err)
	}

	events := auditEvents(t, paths)
	for _, e := range events {
		if e.Type == "gc" {
			t.Errorf("dry run must not append a gc audit event, got: %+v", e)
		}
	}
}

func TestGCAppendsAuditEventWithReportedCounts(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	seedActivateGeneration(t, paths, gcTestHarness, "1.0.0", gcTestKey, "orphan")

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}

	events := auditEvents(t, paths)
	var found *AuditEvent
	for i := range events {
		if events[i].Type == "gc" {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no gc audit event, got: %+v", events)
	}
	if int(found.Fields["removed_generations"].(float64)) != report.RemovedGenerations {
		t.Errorf("audit removed_generations = %v, want %d", found.Fields["removed_generations"], report.RemovedGenerations)
	}
	if int(found.Fields["removed_cache_objects"].(float64)) != report.RemovedCacheObjects {
		t.Errorf("audit removed_cache_objects = %v, want %d", found.Fields["removed_cache_objects"], report.RemovedCacheObjects)
	}
	if int64(found.Fields["freed_bytes"].(float64)) != report.FreedBytes {
		t.Errorf("audit freed_bytes = %v, want %d", found.Fields["freed_bytes"], report.FreedBytes)
	}
}

func TestGCPoolAcquiresLock(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	lock := NewPoolLock(paths.AdvisoryLock)
	if err := lock.Acquire(ctxT(t)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := GCPool(ctx, paths, false); err == nil {
		t.Fatal("expected GCPool to fail acquiring an already-held pool lock")
	}
	lock.Release()
}

// digestFromGenerationID reads a just-seeded generation's own receipt.json
// to recover its full ManifestDigest — genID's own trailing digest8 is only
// the first 8 hex chars (§5), not enough to populate a
// LockHarness.Generations entry, which records the complete "sha256:<hex>"
// digest.
func digestFromGenerationID(t *testing.T, paths PoolPaths, genID string) string {
	t.Helper()
	r, err := readGenerationReceipt(runtimeGenerationDir(paths.Runtime, genID))
	if err != nil {
		t.Fatalf("reading receipt for %s: %v", genID, err)
	}
	return r.ManifestDigest
}
