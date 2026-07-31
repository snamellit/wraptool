package pool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realizeTestHarness/realizeTestVersion/realizeTestBin/realizeTestKey are
// this file's fixed fixtures, mirroring decision_test.go's decisionTest*
// constants.
const (
	realizeTestHarness = "claude"
	realizeTestVersion = "2.1.220"
	realizeTestBin     = "claude"
)

var realizeTestKey = PlatformLinuxAMD64Glibc

// seedRealizeLock writes an initial harness-lock.json with one harness entry
// and returns a copy of h — callers pass &h (or the returned copy's
// address) to RealizeGeneration so in-memory mutation and the on-disk copy
// start out identical.
func seedRealizeLock(t *testing.T, paths PoolPaths, h LockHarness) {
	t.Helper()
	l := &Lock{
		SchemaVersion: LockSchemaVersion,
		GeneratedAt:   FormatTimestamp(fixedNow),
		PolicyHash:    "sha256:" + strings.Repeat("0", 64),
		Harnesses:     map[string]LockHarness{realizeTestHarness: h},
	}
	writeLock(t, paths, l)
}

// readRealizeLock re-reads harness-lock.json and returns realizeTestHarness's
// entry, failing the test if it's missing.
func readRealizeLock(t *testing.T, paths PoolPaths) LockHarness {
	t.Helper()
	data, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("reading lock: %v", err)
	}
	l, err := UnmarshalLock(data)
	if err != nil {
		t.Fatalf("parsing lock: %v", err)
	}
	lh, ok := l.Harnesses[realizeTestHarness]
	if !ok {
		t.Fatalf("lock has no entry for %q", realizeTestHarness)
	}
	return lh
}

// stagingEntries lists staging/txn-* directories still present, so a test
// can assert the scratch dir was cleaned up on both success and failure.
func stagingEntries(t *testing.T, paths PoolPaths) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(paths.Staging, "txn-*"))
	if err != nil {
		t.Fatalf("globbing staging dir: %v", err)
	}
	return matches
}

// auditEvents parses every line of audit.jsonl, returning nil (not an
// error) when the file doesn't exist yet.
func auditEvents(t *testing.T, paths PoolPaths) []AuditEvent {
	t.Helper()
	data, err := os.ReadFile(paths.Audit)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading audit log: %v", err)
	}
	var events []AuditEvent
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var evt AuditEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("parsing audit event %q: %v", line, err)
		}
		events = append(events, evt)
	}
	return events
}

// seedExistingGeneration builds a small but real generation tree directly at
// RuntimeGenerationDir(genID) for (harnessName, version, key) — mimicking a
// PRIOR, already-published RealizeGeneration call, without going through
// Source.Realize — and returns its generation id and manifest digest. Used
// by the "target already exists" tests (both the idempotent-match and the
// immutability-violation-mismatch cases).
func seedExistingGeneration(t *testing.T, paths PoolPaths, harnessName, version string, key PlatformKey, content string) (genID, digest string) {
	t.Helper()
	scratch := t.TempDir()
	binDir := filepath.Join(scratch, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, harnessName), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := ScanGenerationFiles(scratch)
	if err != nil {
		t.Fatal(err)
	}
	digest, err = ManifestDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	genID, err = GenerationID(harnessName, version, key, digest)
	if err != nil {
		t.Fatal(err)
	}

	r := &Receipt{
		SchemaVersion:  ReceiptSchemaVersion,
		Harness:        harnessName,
		Version:        version,
		Platform:       key,
		RecipeRevision: "sha256:" + strings.Repeat("c", 64),
		LockEntryHash:  "sha256:" + strings.Repeat("d", 64),
		Backend:        ReceiptBackend{Name: "fake"},
		Files:          files,
		ManifestDigest: digest,
	}
	data, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filepath.Join(scratch, "receipt.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	dest := runtimeGenerationDir(paths.Runtime, genID)
	if err := os.MkdirAll(filepath.Dir(dest), poolDirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(scratch, dest); err != nil {
		t.Fatal(err)
	}
	return genID, digest
}

// baseRealizeLockHarness returns a plausible, Validate-clean LockHarness for
// realizeTestHarness/realizeTestVersion with no Generations recorded yet.
func baseRealizeLockHarness() LockHarness {
	return LockHarness{
		Source:         npmSourceName,
		Package:        "@example/claude",
		Version:        realizeTestVersion,
		PublishedAt:    FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
		FirstSeenAt:    FormatTimestamp(fixedNow.Add(-200 * time.Hour)),
		RecipeRevision: "sha256:" + strings.Repeat("1", 64),
	}
}

func mustGenerationID(t *testing.T, harnessName, version string, key PlatformKey, digest string) string {
	t.Helper()
	id, err := GenerationID(harnessName, version, key, digest)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRealizeGenerationPublishesNewGeneration is the happy path: a fresh
// realize creates a txn dir, calls Source.Realize, verifies, writes
// receipt.json, renames into runtime/generations/<generation-id>/, updates
// the lock (both in memory and on disk), and appends the audit event.
func TestRealizeGenerationPublishesNewGeneration(t *testing.T) {
	paths := testPaths(t)
	h := baseRealizeLockHarness()
	seedRealizeLock(t, paths, h)

	src := &fakeSource{}
	receipt, err := RealizeGeneration(ctxT(t), src, fakeBackend{}, paths, realizeTestHarness, realizeTestBin, &h, realizeTestKey)
	if err != nil {
		t.Fatalf("RealizeGeneration: %v", err)
	}
	if src.realizeCalls != 1 {
		t.Errorf("realizeCalls = %d, want 1", src.realizeCalls)
	}
	if src.lastRealize.harnessName != realizeTestHarness || src.lastRealize.binName != realizeTestBin || src.lastRealize.key != realizeTestKey {
		t.Errorf("lastRealize = %+v, want harness=%q bin=%q key=%q", src.lastRealize, realizeTestHarness, realizeTestBin, realizeTestKey)
	}

	// Staging is empty again: the txn dir was renamed away, not merely left
	// behind.
	if entries := stagingEntries(t, paths); len(entries) != 0 {
		t.Errorf("staging entries after success = %v, want none", entries)
	}

	genID := mustGenerationID(t, realizeTestHarness, realizeTestVersion, realizeTestKey, receipt.ManifestDigest)
	genDir := runtimeGenerationDir(paths.Runtime, genID)
	if _, err := os.Stat(genDir); err != nil {
		t.Errorf("expected generation dir %s to exist: %v", genDir, err)
	}

	receiptData, err := os.ReadFile(filepath.Join(genDir, "receipt.json"))
	if err != nil {
		t.Fatalf("reading published receipt.json: %v", err)
	}
	onDisk, err := UnmarshalReceipt(receiptData) // UnmarshalReceipt calls Validate internally
	if err != nil {
		t.Fatalf("UnmarshalReceipt/Validate on published receipt: %v", err)
	}
	if onDisk.ManifestDigest != receipt.ManifestDigest {
		t.Errorf("published receipt digest = %q, want %q", onDisk.ManifestDigest, receipt.ManifestDigest)
	}

	if h.Generations[realizeTestKey] != receipt.ManifestDigest {
		t.Errorf("in-memory h.Generations[%s] = %q, want %q", realizeTestKey, h.Generations[realizeTestKey], receipt.ManifestDigest)
	}

	lh := readRealizeLock(t, paths)
	if lh.Generations[realizeTestKey] != receipt.ManifestDigest {
		t.Errorf("persisted lock Generations[%s] = %q, want %q", realizeTestKey, lh.Generations[realizeTestKey], receipt.ManifestDigest)
	}

	events := auditEvents(t, paths)
	var found *AuditEvent
	for i := range events {
		if events[i].Type == "generation_realized" {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no generation_realized audit event, got: %+v", events)
	}
	if found.Fields["harness"] != realizeTestHarness || found.Fields["version"] != realizeTestVersion ||
		found.Fields["platform"] != string(realizeTestKey) || found.Fields["generation_id"] != genID {
		t.Errorf("generation_realized audit event fields = %+v, want harness/version/platform/generation_id matching %s/%s/%s/%s",
			found.Fields, realizeTestHarness, realizeTestVersion, realizeTestKey, genID)
	}
}

// TestRealizeGenerationExistingTargetRightDigestIsIdempotent covers the
// "existing target with RIGHT digest" case: the lock already records the
// exact digest of an already-published generation — success, with
// Source.Realize never called.
func TestRealizeGenerationExistingTargetRightDigestIsIdempotent(t *testing.T) {
	paths := testPaths(t)
	_, digest := seedExistingGeneration(t, paths, realizeTestHarness, realizeTestVersion, realizeTestKey, "already published")

	h := baseRealizeLockHarness()
	h.Generations = map[PlatformKey]string{realizeTestKey: digest}
	seedRealizeLock(t, paths, h)

	src := &fakeSource{}
	receipt, err := RealizeGeneration(ctxT(t), src, fakeBackend{}, paths, realizeTestHarness, realizeTestBin, &h, realizeTestKey)
	if err != nil {
		t.Fatalf("RealizeGeneration: %v", err)
	}
	if src.realizeCalls != 0 {
		t.Errorf("realizeCalls = %d, want 0 (idempotent match must not call Source.Realize)", src.realizeCalls)
	}
	if receipt.ManifestDigest != digest {
		t.Errorf("returned receipt digest = %q, want the already-published %q", receipt.ManifestDigest, digest)
	}
	if entries := stagingEntries(t, paths); len(entries) != 0 {
		t.Errorf("staging entries = %v, want none (no txn dir should ever be created)", entries)
	}
}

// TestRealizeGenerationExistingTargetWrongDigestFailsLoudly covers the
// "existing target with WRONG digest" case: the lock's recorded digest
// resolves (via its first-8-hex-chars generation id) to an already-published
// directory whose OWN, internally self-consistent receipt names a DIFFERENT
// full digest — an immutability violation, never silently repaired, never
// overwritten. Source.Realize is never called: the mismatch is detected
// purely from what's already locked and already on disk, with no rebuild
// attempted.
func TestRealizeGenerationExistingTargetWrongDigestFailsLoudly(t *testing.T) {
	paths := testPaths(t)
	_, actualDigest := seedExistingGeneration(t, paths, realizeTestHarness, realizeTestVersion, realizeTestKey, "already published")

	// wrongDigest shares actualDigest's generation-id-relevant prefix (the
	// first 8 hex chars after "sha256:") so RealizeGeneration's short-circuit
	// resolves to the SAME already-published directory, but differs in its
	// last character — a "recorded a different content" scenario, distinct
	// from the not-recorded-at-all "repair" case above.
	last := actualDigest[len(actualDigest)-1]
	flipped := byte('0')
	if last == '0' {
		flipped = '1'
	}
	wrongDigest := actualDigest[:len(actualDigest)-1] + string(flipped)

	h := baseRealizeLockHarness()
	h.Generations = map[PlatformKey]string{realizeTestKey: wrongDigest}
	seedRealizeLock(t, paths, h)

	src := &fakeSource{}
	before := readRealizeLock(t, paths)

	_, err := RealizeGeneration(ctxT(t), src, fakeBackend{}, paths, realizeTestHarness, realizeTestBin, &h, realizeTestKey)
	if err == nil {
		t.Fatal("expected an immutability-violation error")
	}
	if !strings.Contains(err.Error(), "immutability violation") {
		t.Errorf("error = %q, want it to mention an immutability violation", err.Error())
	}
	if !strings.Contains(err.Error(), wrongDigest) || !strings.Contains(err.Error(), actualDigest) {
		t.Errorf("error = %q, want it to name both the locked %q and the on-disk %q digests", err.Error(), wrongDigest, actualDigest)
	}
	if src.realizeCalls != 0 {
		t.Errorf("realizeCalls = %d, want 0", src.realizeCalls)
	}
	if entries := stagingEntries(t, paths); len(entries) != 0 {
		t.Errorf("staging entries = %v, want none", entries)
	}
	after := readRealizeLock(t, paths)
	if after.Generations[realizeTestKey] != before.Generations[realizeTestKey] {
		t.Errorf("lock's Generations[%s] changed from %q to %q, want untouched", realizeTestKey, before.Generations[realizeTestKey], after.Generations[realizeTestKey])
	}
}

// TestRealizeGenerationRepairsWhenDirectoryVanished is the "repair" case
// (§15/§17): the lock records a digest for this platform, but its
// generation directory is gone (lost cache, manual deletion, a copied lock
// file on a fresh workstation) — RealizeGeneration must realize again
// instead of silently reporting success.
func TestRealizeGenerationRepairsWhenDirectoryVanished(t *testing.T) {
	paths := testPaths(t)
	h := baseRealizeLockHarness()
	h.Generations = map[PlatformKey]string{realizeTestKey: "sha256:" + strings.Repeat("9", 64)} // recorded, but nothing on disk names it
	seedRealizeLock(t, paths, h)

	src := &fakeSource{}
	receipt, err := RealizeGeneration(ctxT(t), src, fakeBackend{}, paths, realizeTestHarness, realizeTestBin, &h, realizeTestKey)
	if err != nil {
		t.Fatalf("RealizeGeneration: %v", err)
	}
	if src.realizeCalls != 1 {
		t.Errorf("realizeCalls = %d, want 1 (a vanished directory must trigger a real rebuild)", src.realizeCalls)
	}
	if h.Generations[realizeTestKey] != receipt.ManifestDigest {
		t.Errorf("h.Generations[%s] = %q after repair, want the freshly computed %q", realizeTestKey, h.Generations[realizeTestKey], receipt.ManifestDigest)
	}
	genDir := runtimeGenerationDir(paths.Runtime, mustGenerationID(t, realizeTestHarness, realizeTestVersion, realizeTestKey, receipt.ManifestDigest))
	if _, err := os.Stat(genDir); err != nil {
		t.Errorf("expected the repaired generation dir to exist: %v", err)
	}
}

var errRealizeBoom = errors.New("boom")

// TestRealizeGenerationFailureInjection table-tests every failure point the
// brief calls out, asserting the §15/§17 invariant in each case: staging is
// left empty, the lock is untouched, and runtime/generations gains nothing.
func TestRealizeGenerationFailureInjection(t *testing.T) {
	tests := []struct {
		name        string
		src         *fakeSource
		errContains string
	}{
		{
			name:        "Source.Realize itself fails",
			src:         &fakeSource{realizeErr: errRealizeBoom},
			errContains: "boom",
		},
		{
			name:        "a zero-file receipt is refused before publish",
			src:         &fakeSource{realizeEmpty: true},
			errContains: "empty generation",
		},
		{
			name:        "a receipt that disagrees with the tree fails host-side verification",
			src:         &fakeSource{realizeMismatch: true},
			errContains: "verifying",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := testPaths(t)
			h := baseRealizeLockHarness()
			seedRealizeLock(t, paths, h)
			before := readRealizeLock(t, paths)

			_, err := RealizeGeneration(ctxT(t), tt.src, fakeBackend{}, paths, realizeTestHarness, realizeTestBin, &h, realizeTestKey)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.errContains)
			}
			if entries := stagingEntries(t, paths); len(entries) != 0 {
				t.Errorf("staging entries after failure = %v, want none (txn dir must be cleaned up)", entries)
			}
			after := readRealizeLock(t, paths)
			if len(after.Generations) != 0 {
				t.Errorf("lock Generations = %v, want untouched (empty)", after.Generations)
			}
			if before.Version != after.Version {
				t.Errorf("lock harness entry changed on failure: before %+v, after %+v", before, after)
			}
			generationsDir := runtimeGenerationsDir(paths.Runtime)
			if entries, err := os.ReadDir(generationsDir); err == nil && len(entries) != 0 {
				t.Errorf("runtime/generations = %v, want empty (nothing published on failure)", entries)
			}
		})
	}
}
