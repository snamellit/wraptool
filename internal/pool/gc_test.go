package pool

import (
	"context"
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

func TestGCRetainsGenerationReferencedByLockButNotActive(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genID := seedActivateGeneration(t, paths, gcTestHarness, "2.1.220", gcTestKey, "in-lock-not-active")

	// Recorded in the lock's Generations map, but NEVER activated (no
	// pointer file at all) — must still be retained.
	digest := digestFromGenerationID(t, paths, genID)
	gcWriteLock(t, paths, map[string]LockHarness{
		gcTestHarness: {
			Source: npmSourceName, Package: "@example/claude", Version: "2.1.220",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Generations:    map[PlatformKey]string{gcTestKey: digest},
		},
	})

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedGenerations != 0 || report.RetainedGenerations != 1 {
		t.Errorf("report = %+v, want 0 removed / 1 retained generation", report)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, genID)); err != nil {
		t.Errorf("expected the lock-referenced generation to survive GC: %v", err)
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

func TestGCRemovesGenerationNeitherActiveNorInLock(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genID := seedActivateGeneration(t, paths, gcTestHarness, "1.0.0", gcTestKey, "orphan")
	// Neither activated nor recorded in any lock.

	report, err := GCPoolLocked(paths, false)
	if err != nil {
		t.Fatalf("GCPoolLocked: %v", err)
	}
	if report.RemovedGenerations != 1 || report.RetainedGenerations != 0 {
		t.Errorf("report = %+v, want 1 removed / 0 retained generation", report)
	}
	if report.FreedBytes <= 0 {
		t.Errorf("FreedBytes = %d, want > 0", report.FreedBytes)
	}
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, genID)); !os.IsNotExist(err) {
		t.Errorf("expected the orphaned generation to be removed, stat err = %v", err)
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
	genID := seedActivateGeneration(t, paths, gcTestHarness, "1.0.0", gcTestKey, "orphan")
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
	if _, err := os.Stat(runtimeGenerationDir(paths.Runtime, genID)); err != nil {
		t.Errorf("dry run must not remove the generation: %v", err)
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
