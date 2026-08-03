package pool

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const rollbackTestHarness = "claude"

var rollbackTestKey = PlatformLinuxAMD64Glibc

// seedRollbackGeneration is seedActivateGeneration under a name that reads
// naturally alongside this file's own tests; genDelay lets a test control
// mtime ordering deterministically (os.Chtimes) instead of relying on real
// wall-clock gaps between seed calls.
func seedRollbackGeneration(t *testing.T, paths PoolPaths, version string, content string, genDelay time.Duration) (genID string) {
	t.Helper()
	genID = seedActivateGeneration(t, paths, rollbackTestHarness, version, rollbackTestKey, content)
	mtime := fixedNow.Add(genDelay)
	if err := os.Chtimes(runtimeGenerationDir(paths.Runtime, genID), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return genID
}

func TestRollbackToExplicitVersion(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genOld := seedRollbackGeneration(t, paths, "1.0.0", "old", 0)
	genNew := seedRollbackGeneration(t, paths, "2.0.0", "new", time.Hour)
	if err := ActivateGeneration(paths, rollbackTestHarness, rollbackTestKey, genNew); err != nil {
		t.Fatalf("activating current: %v", err)
	}

	got, err := RollbackHarnessLocked(paths, rollbackTestHarness, rollbackTestKey, "1.0.0")
	if err != nil {
		t.Fatalf("RollbackHarnessLocked: %v", err)
	}
	if got != genOld {
		t.Errorf("rolled back to %q, want %q", got, genOld)
	}
	active, ok := ActiveGeneration(paths, rollbackTestHarness, rollbackTestKey)
	if !ok || active != genOld {
		t.Errorf("ActiveGeneration after rollback = (%q, %v), want (%q, true)", active, ok, genOld)
	}
}

func TestRollbackToExplicitVersionErrorListsAvailableVersions(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	seedRollbackGeneration(t, paths, "1.0.0", "old", 0)
	seedRollbackGeneration(t, paths, "1.5.0", "mid", time.Hour)

	_, err := RollbackHarnessLocked(paths, rollbackTestHarness, rollbackTestKey, "9.9.9")
	if err == nil {
		t.Fatal("expected an error for a --to version that was never realized")
	}
	if !strings.Contains(err.Error(), "1.0.0") || !strings.Contains(err.Error(), "1.5.0") {
		t.Errorf("error = %q, want it to list the available versions 1.0.0 and 1.5.0", err.Error())
	}
}

func TestRollbackDefaultsToMostRecentlyCreatedOtherGeneration(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genOldest := seedRollbackGeneration(t, paths, "1.0.0", "oldest", 0)
	genMiddle := seedRollbackGeneration(t, paths, "1.5.0", "middle", time.Hour)
	genNewest := seedRollbackGeneration(t, paths, "2.0.0", "newest", 2*time.Hour)
	if err := ActivateGeneration(paths, rollbackTestHarness, rollbackTestKey, genNewest); err != nil {
		t.Fatalf("activating current: %v", err)
	}

	got, err := RollbackHarnessLocked(paths, rollbackTestHarness, rollbackTestKey, "")
	if err != nil {
		t.Fatalf("RollbackHarnessLocked: %v", err)
	}
	// genMiddle is the most recently created generation OTHER than the
	// currently active genNewest.
	if got != genMiddle {
		t.Errorf("rolled back to %q, want the most recently created other generation %q (not %q)", got, genMiddle, genOldest)
	}
}

func TestRollbackErrorsWhenOnlyActiveGenerationIsRetained(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genOnly := seedRollbackGeneration(t, paths, "1.0.0", "only", 0)
	if err := ActivateGeneration(paths, rollbackTestHarness, rollbackTestKey, genOnly); err != nil {
		t.Fatalf("activating: %v", err)
	}

	if _, err := RollbackHarnessLocked(paths, rollbackTestHarness, rollbackTestKey, ""); err == nil {
		t.Fatal("expected an error: only the currently active generation is retained, nothing to roll back to")
	}
}

func TestRollbackErrorsWhenNothingRealized(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	if _, err := RollbackHarnessLocked(paths, rollbackTestHarness, rollbackTestKey, ""); err == nil {
		t.Fatal("expected an error when no generation has ever been realized for this harness/platform")
	}
}

// TestRollbackRewritesPointerOnly is the §17 invariant: rollback must not
// touch harness-lock.json or runtime/generations/ at all — the ONLY
// mutation is the pointer file (via ActivateGeneration).
func TestRollbackRewritesPointerOnly(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genOld := seedRollbackGeneration(t, paths, "1.0.0", "old", 0)
	genNew := seedRollbackGeneration(t, paths, "2.0.0", "new", time.Hour)
	if err := ActivateGeneration(paths, rollbackTestHarness, rollbackTestKey, genNew); err != nil {
		t.Fatalf("activating current: %v", err)
	}

	digest := digestFromGenerationID(t, paths, genNew)
	gcWriteLock(t, paths, map[string]LockHarness{
		rollbackTestHarness: {
			Source: npmSourceName, Package: "@example/claude", Version: "2.0.0",
			PublishedAt: FormatTimestamp(fixedNow), FirstSeenAt: FormatTimestamp(fixedNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Generations:    map[PlatformKey]string{rollbackTestKey: digest},
		},
	})
	lockBefore, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatal(err)
	}
	generationsDir := runtimeGenerationsDir(paths.Runtime)
	entriesBefore, err := os.ReadDir(generationsDir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RollbackHarnessLocked(paths, rollbackTestHarness, rollbackTestKey, "1.0.0"); err != nil {
		t.Fatalf("RollbackHarnessLocked: %v", err)
	}

	lockAfter, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatal(err)
	}
	if string(lockBefore) != string(lockAfter) {
		t.Errorf("harness-lock.json changed after rollback; want byte-identical\nbefore:\n%s\nafter:\n%s", lockBefore, lockAfter)
	}
	entriesAfter, err := os.ReadDir(generationsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesBefore) != len(entriesAfter) {
		t.Errorf("runtime/generations/ entry count changed after rollback: before %d, after %d", len(entriesBefore), len(entriesAfter))
	}

	active, ok := ActiveGeneration(paths, rollbackTestHarness, rollbackTestKey)
	if !ok || active != genOld {
		t.Errorf("ActiveGeneration after rollback = (%q, %v), want (%q, true)", active, ok, genOld)
	}
}

func TestRollbackAppendsRollbackAuditEventNamingBothGenerations(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genOld := seedRollbackGeneration(t, paths, "1.0.0", "old", 0)
	genNew := seedRollbackGeneration(t, paths, "2.0.0", "new", time.Hour)
	if err := ActivateGeneration(paths, rollbackTestHarness, rollbackTestKey, genNew); err != nil {
		t.Fatalf("activating current: %v", err)
	}

	if _, err := RollbackHarnessLocked(paths, rollbackTestHarness, rollbackTestKey, "1.0.0"); err != nil {
		t.Fatalf("RollbackHarnessLocked: %v", err)
	}

	events := auditEvents(t, paths)
	var found *AuditEvent
	for i := range events {
		if events[i].Type == "generation_rollback" {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no rollback audit event, got: %+v", events)
	}
	if found.Fields["harness"] != rollbackTestHarness || found.Fields["platform"] != string(rollbackTestKey) ||
		found.Fields["from_generation"] != genNew || found.Fields["to_generation"] != genOld {
		t.Errorf("rollback audit event fields = %+v, want from_generation=%q to_generation=%q", found.Fields, genNew, genOld)
	}
}

func TestRollbackHarnessAcquiresLock(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	seedRollbackGeneration(t, paths, "1.0.0", "old", 0)
	seedRollbackGeneration(t, paths, "2.0.0", "new", time.Hour)

	lock := NewPoolLock(paths.AdvisoryLock)
	if err := lock.Acquire(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := RollbackHarness(ctx, paths, rollbackTestHarness, rollbackTestKey, ""); err == nil {
		t.Fatal("expected RollbackHarness to fail acquiring an already-held pool lock")
	}
}
