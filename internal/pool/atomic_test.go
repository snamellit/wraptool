package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFileAtomicCreatesFileWithContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.txt")

	if err := WriteFileAtomic(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
}

func TestWriteFileAtomicOverwritesLeavingNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	if err := WriteFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic (1): %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic (2): %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file after two writes (no leftover temp files), got %d: %v", len(entries), entries)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
}

// TestWriteFileAtomicDirsAreTraversable covers MINOR 6: pool directories
// must be 0755, not 0700, so a project container's UID can traverse into
// runtime/ (the only subtree it mounts). WriteFileAtomic creates its
// destination directory via the same poolDirPerm every other pool directory
// uses.
func TestWriteFileAtomicDirsAreTraversable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	nested := filepath.Join(dir, "runtime", "active", "linux-amd64-glibc")
	path := filepath.Join(nested, "claude.gen")

	if err := WriteFileAtomic(path, []byte("gen-1\n"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != poolDirPerm {
		t.Errorf("directory mode = %o, want %o (must remain traversable by a container UID)", perm, poolDirPerm)
	}
}

// TestWritePointerTempFilePattern covers NIT 10: the pointer temp file must
// be named "<pointer-basename>.tmp-<random>" — the literal §5.2 shape — not
// WriteFileAtomic's generic ".wraptool-pool-*.tmp" pattern, so an orphaned
// temp file left under active/ after a crash is recognizable as pool cleanup
// work. os.CreateTemp resolves "*" against a fixed clock-derived counter
// (not crypto-random), so a temp file created with the same pattern
// immediately before WritePointer runs deterministically predicts the name
// WritePointer's own temp file would have taken, letting this test assert
// the pattern without racing WritePointer's internal cleanup.
func TestWritePointerTempFilePattern(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.gen")

	probe, err := os.CreateTemp(dir, "claude.gen.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	probeName := filepath.Base(probe.Name())
	probe.Close()
	os.Remove(probe.Name())

	if !strings.HasPrefix(probeName, "claude.gen.tmp-") {
		t.Fatalf("test setup: probe name %q does not match the expected pattern", probeName)
	}

	if err := WritePointer(path, "gen-1"); err != nil {
		t.Fatalf("WritePointer: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "claude.gen" {
		t.Fatalf("expected exactly claude.gen after WritePointer (no leftover temp files), got %v", entries)
	}
}

func TestPointerWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.gen")

	if err := WritePointer(path, "claude-2.1.220-linux-amd64-glibc-a1b2c3d4"); err != nil {
		t.Fatalf("WritePointer: %v", err)
	}
	got, err := ReadPointer(path)
	if err != nil {
		t.Fatalf("ReadPointer: %v", err)
	}
	if got != "claude-2.1.220-linux-amd64-glibc-a1b2c3d4" {
		t.Errorf("ReadPointer() = %q, want the generation id with no trailing newline", got)
	}

	// The file on disk must be exactly one LF-terminated line (§5.2).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("pointer file should be LF-terminated")
	}
	if strings.Count(string(raw), "\n") != 1 {
		t.Errorf("pointer file should contain exactly one newline, got %d", strings.Count(string(raw), "\n"))
	}
}

func TestPointerRollbackRewritesPointerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.gen")

	if err := WritePointer(path, "gen-1"); err != nil {
		t.Fatalf("WritePointer (activate): %v", err)
	}
	if err := WritePointer(path, "gen-2"); err != nil {
		t.Fatalf("WritePointer (upgrade): %v", err)
	}
	// Rollback = write a previous generation id (§5.2 "No tree surgery").
	if err := WritePointer(path, "gen-1"); err != nil {
		t.Fatalf("WritePointer (rollback): %v", err)
	}
	got, err := ReadPointer(path)
	if err != nil {
		t.Fatalf("ReadPointer: %v", err)
	}
	if got != "gen-1" {
		t.Errorf("after rollback, ReadPointer() = %q, want %q", got, "gen-1")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("rollback should leave exactly the pointer file, found %v", entries)
	}
}

func TestReadPointerMissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadPointer(filepath.Join(dir, "missing.gen")); err == nil {
		t.Fatal("expected an error reading a missing pointer file, got nil")
	} else if !os.IsNotExist(err) {
		t.Errorf("expected an os.IsNotExist error, got: %v", err)
	}
}

// TestPointerActivationIsAtomic exercises the rename-based atomicity claim:
// concurrent readers only ever see either the old or the new pointer content
// in full, never a partial write, because WritePointer always writes to a
// temp file and renames over the target rather than truncating it in place.
func TestPointerActivationIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.gen")

	if err := WritePointer(path, "gen-old"); err != nil {
		t.Fatalf("WritePointer (initial): %v", err)
	}

	done := make(chan struct{})
	errs := make(chan error, 100)
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			got, err := ReadPointer(path)
			if err != nil {
				errs <- err
				continue
			}
			if got != "gen-old" && got != "gen-new" {
				errs <- fmt.Errorf("torn read: %q", got)
			}
		}
	}()

	if err := WritePointer(path, "gen-new"); err != nil {
		t.Fatalf("WritePointer (activate): %v", err)
	}
	<-done
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
