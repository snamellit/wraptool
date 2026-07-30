package pool

import (
	"fmt"
	"os"
	"path/filepath"
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
