package pool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	activateTestHarness = "claude"
	activateTestBin     = "claude"
)

var activateTestKey = PlatformLinuxAMD64Glibc

// seedActivateGeneration builds a small, real, receipt-valid generation tree
// directly at RuntimeGenerationDir(genID) for (harnessName, version, key) —
// the same shape seedExistingGeneration (realize_test.go) builds — and
// returns its generation id. Content varies the generation id (and so lets
// a test tell two generations apart) via the content string written into
// bin/<harnessName>.
func seedActivateGeneration(t *testing.T, paths PoolPaths, harnessName, version string, key PlatformKey, content string) (genID string) {
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
	digest, err := ManifestDigest(files)
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
	return genID
}

func TestActivateGenerationWritesPointerAndDispatcher(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genID := seedActivateGeneration(t, paths, activateTestHarness, "2.1.220", activateTestKey, "v1")

	if err := ActivateGeneration(paths, activateTestHarness, activateTestKey, genID); err != nil {
		t.Fatalf("ActivateGeneration: %v", err)
	}

	// §5.2: pointer content is exactly one line, LF-terminated.
	ptrPath := pointerFile(paths.Runtime, activateTestKey, activateTestHarness)
	data, err := os.ReadFile(ptrPath)
	if err != nil {
		t.Fatalf("reading pointer: %v", err)
	}
	if string(data) != genID+"\n" {
		t.Errorf("pointer content = %q, want %q", data, genID+"\n")
	}

	got, ok := ActiveGeneration(paths, activateTestHarness, activateTestKey)
	if !ok || got != genID {
		t.Errorf("ActiveGeneration = (%q, %v), want (%q, true)", got, ok, genID)
	}

	// The dispatcher is regenerated on every activation (§5.3).
	dispatcherPath := runtimeBinFile(paths.Runtime, activateTestHarness)
	info, err := os.Stat(dispatcherPath)
	if err != nil {
		t.Fatalf("stat dispatcher: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("dispatcher mode = %o, want 0o755", info.Mode().Perm())
	}
	dispatcherData, err := os.ReadFile(dispatcherPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(dispatcherData) != string(GenerateDispatcher(activateTestHarness)) {
		t.Errorf("dispatcher content does not match GenerateDispatcher's output")
	}

	events := auditEvents(t, paths)
	found := false
	for _, e := range events {
		if e.Type == "generation_activated" && e.Fields["harness"] == activateTestHarness &&
			e.Fields["platform"] == string(activateTestKey) && e.Fields["generation_id"] == genID {
			found = true
		}
	}
	if !found {
		t.Errorf("no matching 'generation_activated' audit event, got: %+v", events)
	}
}

func TestActivateGenerationRejectsUnknownGeneration(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	if err := ActivateGeneration(paths, activateTestHarness, activateTestKey, "claude-2.1.220-linux-amd64-glibc-deadbeef"); err == nil {
		t.Fatal("expected an error activating a generation id with no receipt on disk")
	}
}

func TestActivateGenerationRejectsReceiptMismatch(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genID := seedActivateGeneration(t, paths, activateTestHarness, "2.1.220", activateTestKey, "v1")

	// The receipt names activateTestHarness/activateTestKey; asking to
	// activate it for a DIFFERENT harness must fail rather than silently
	// wiring one harness's dispatcher/pointer to another's generation.
	if err := ActivateGeneration(paths, "opencode", activateTestKey, genID); err == nil {
		t.Fatal("expected an error activating a generation whose receipt names a different harness")
	}
	if err := ActivateGeneration(paths, activateTestHarness, PlatformLinuxARM64Glibc, genID); err == nil {
		t.Fatal("expected an error activating a generation whose receipt names a different platform")
	}
}

// TestActivateGenerationTransitionsWithNoPartialRead is the §17 pointer-file
// activation invariant beyond WritePointer's own unit test: a concurrent
// reader looping ReadPointer against the SAME pointer file while
// ActivateGeneration repeatedly switches it between two known generation ids
// must never observe anything other than one of those two exact ids — never
// empty, never partial content (rename atomicity, §5.2).
func TestActivateGenerationTransitionsWithNoPartialRead(t *testing.T) {
	paths := testPaths(t)
	genA := seedActivateGeneration(t, paths, activateTestHarness, "1.0.0", activateTestKey, "generation-a")
	genB := seedActivateGeneration(t, paths, activateTestHarness, "2.0.0", activateTestKey, "generation-b")

	if err := ActivateGeneration(paths, activateTestHarness, activateTestKey, genA); err != nil {
		t.Fatalf("initial activation: %v", err)
	}

	ptrPath := pointerFile(paths.Runtime, activateTestKey, activateTestHarness)
	valid := map[string]bool{genA: true, genB: true}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var badReads []string

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(ptrPath)
			if err != nil {
				// A transient not-exist can only happen before the very
				// first activation above, which already completed — any
				// read error here is unexpected.
				mu.Lock()
				badReads = append(badReads, "read error: "+err.Error())
				mu.Unlock()
				continue
			}
			s := string(data)
			if !valid[strings.TrimSuffix(s, "\n")] {
				mu.Lock()
				badReads = append(badReads, "invalid content: "+s)
				mu.Unlock()
			}
		}
	}()

	current := genA
	for i := 0; i < 200; i++ {
		next := genB
		if current == genB {
			next = genA
		}
		if err := ActivateGeneration(paths, activateTestHarness, activateTestKey, next); err != nil {
			t.Fatalf("activation %d: %v", i, err)
		}
		current = next
	}
	close(stop)
	wg.Wait()

	if len(badReads) != 0 {
		t.Errorf("concurrent reader observed %d invalid pointer read(s), e.g. %q", len(badReads), badReads[0])
	}
}

func TestActivateIfNeededSkipsWhenAlreadyActive(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genID := seedActivateGeneration(t, paths, activateTestHarness, "2.1.220", activateTestKey, "v1")

	if err := activateIfNeeded(paths, activateTestHarness, activateTestKey, genID); err != nil {
		t.Fatalf("first activateIfNeeded: %v", err)
	}
	firstEvents := len(auditEvents(t, paths))

	if err := activateIfNeeded(paths, activateTestHarness, activateTestKey, genID); err != nil {
		t.Fatalf("second activateIfNeeded: %v", err)
	}
	secondEvents := len(auditEvents(t, paths))

	if secondEvents != firstEvents {
		t.Errorf("activateIfNeeded re-activated an already-active generation: audit events grew from %d to %d", firstEvents, secondEvents)
	}
}

func TestActivateIfNeededActivatesWhenPointerNamesDifferentGeneration(t *testing.T) {
	t.Parallel()
	paths := testPaths(t)
	genA := seedActivateGeneration(t, paths, activateTestHarness, "1.0.0", activateTestKey, "generation-a")
	genB := seedActivateGeneration(t, paths, activateTestHarness, "2.0.0", activateTestKey, "generation-b")

	if err := activateIfNeeded(paths, activateTestHarness, activateTestKey, genA); err != nil {
		t.Fatalf("activating genA: %v", err)
	}
	if err := activateIfNeeded(paths, activateTestHarness, activateTestKey, genB); err != nil {
		t.Fatalf("activating genB: %v", err)
	}

	got, ok := ActiveGeneration(paths, activateTestHarness, activateTestKey)
	if !ok || got != genB {
		t.Errorf("ActiveGeneration = (%q, %v), want (%q, true)", got, ok, genB)
	}
}
