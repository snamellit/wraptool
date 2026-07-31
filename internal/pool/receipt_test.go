package pool

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTreeFile creates path (creating parent directories) with content and
// perm, relative to dir.
func writeTreeFile(t *testing.T, dir, rel, content string, perm os.FileMode) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), perm); err != nil {
		t.Fatalf("WriteFile(%s): %v", full, err)
	}
	if err := os.Chmod(full, perm); err != nil {
		t.Fatalf("Chmod(%s): %v", full, err)
	}
}

func TestScanGenerationFilesDigestIgnoresMtimes(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	for _, dir := range []string{dirA, dirB} {
		writeTreeFile(t, dir, "bin/claude", "#!/bin/sh\nexec node\n", 0o755)
		writeTreeFile(t, dir, "lib/data.json", `{"a":1}`, 0o644)
	}

	// Give the two trees deliberately different mtimes.
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dirA, "bin", "claude"), past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dirB, "bin", "claude"), future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	filesA, err := ScanGenerationFiles(dirA)
	if err != nil {
		t.Fatalf("ScanGenerationFiles(dirA): %v", err)
	}
	filesB, err := ScanGenerationFiles(dirB)
	if err != nil {
		t.Fatalf("ScanGenerationFiles(dirB): %v", err)
	}

	digestA, err := ManifestDigest(filesA)
	if err != nil {
		t.Fatalf("ManifestDigest(A): %v", err)
	}
	digestB, err := ManifestDigest(filesB)
	if err != nil {
		t.Fatalf("ManifestDigest(B): %v", err)
	}
	if digestA != digestB {
		t.Fatalf("ManifestDigest differs across identical content with different mtimes: %q vs %q", digestA, digestB)
	}

	idA, err := GenerationID("claude", "1.0.0", PlatformLinuxAMD64Glibc, digestA)
	if err != nil {
		t.Fatalf("GenerationID(A): %v", err)
	}
	idB, err := GenerationID("claude", "1.0.0", PlatformLinuxAMD64Glibc, digestB)
	if err != nil {
		t.Fatalf("GenerationID(B): %v", err)
	}
	if idA != idB {
		t.Fatalf("GenerationID differs across identical content with different mtimes: %q vs %q", idA, idB)
	}
}

func TestScanGenerationFilesExecutableFlagIdentity(t *testing.T) {
	digestFor := func(perm os.FileMode) string {
		t.Helper()
		dir := t.TempDir()
		writeTreeFile(t, dir, "bin/tool", "same content", perm)
		files, err := ScanGenerationFiles(dir)
		if err != nil {
			t.Fatalf("ScanGenerationFiles: %v", err)
		}
		digest, err := ManifestDigest(files)
		if err != nil {
			t.Fatalf("ManifestDigest: %v", err)
		}
		return digest
	}

	d744 := digestFor(0o744)
	d755 := digestFor(0o755)
	if d744 != d755 {
		t.Errorf("0o744 vs 0o755 (both owner-exec) produced different digests: %q vs %q", d744, d755)
	}

	d644 := digestFor(0o644)
	if d644 == d755 {
		t.Errorf("0o644 (no exec) vs 0o755 (exec) produced the same digest: %q", d644)
	}
}

func TestScanGenerationFilesCaseCollision(t *testing.T) {
	dir := t.TempDir()
	writeTreeFile(t, dir, "Bin/x", "one", 0o644)
	writeTreeFile(t, dir, "bin/X", "two", 0o644)

	_, err := ScanGenerationFiles(dir)
	if err == nil {
		t.Fatal("expected an error for case-colliding paths, got nil")
	}
	if !strings.Contains(err.Error(), "Bin/x") || !strings.Contains(err.Error(), "bin/X") {
		t.Errorf("error does not name both colliding paths: %v", err)
	}
}

func TestScanGenerationFilesSymlinkFails(t *testing.T) {
	dir := t.TempDir()
	writeTreeFile(t, dir, "lib/real.txt", "content", 0o644)
	if err := os.Symlink("real.txt", filepath.Join(dir, "lib", "link.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := ScanGenerationFiles(dir)
	if err == nil {
		t.Fatal("expected an error for a symlink in the tree, got nil")
	}
	if !strings.Contains(err.Error(), filepath.ToSlash(filepath.Join("lib", "link.txt"))) {
		t.Errorf("error does not name the offending symlink path: %v", err)
	}
}

func TestScanGenerationFilesExcludesTopLevelReceiptJSON(t *testing.T) {
	dir := t.TempDir()
	writeTreeFile(t, dir, "receipt.json", `{"top":true}`, 0o644)
	writeTreeFile(t, dir, "sub/receipt.json", `{"nested":true}`, 0o644)

	files, err := ScanGenerationFiles(dir)
	if err != nil {
		t.Fatalf("ScanGenerationFiles: %v", err)
	}

	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if slicesContains(paths, "receipt.json") {
		t.Errorf("top-level receipt.json must be excluded, got paths %v", paths)
	}
	if !slicesContains(paths, "sub/receipt.json") {
		t.Errorf("nested sub/receipt.json must be included, got paths %v", paths)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestScanGenerationFilesSortedSlashSeparated(t *testing.T) {
	dir := t.TempDir()
	writeTreeFile(t, dir, "lib/node_modules/foo/index.js", "x", 0o644)
	writeTreeFile(t, dir, "bin/claude", "x", 0o755)
	writeTreeFile(t, dir, "a.txt", "x", 0o644)

	files, err := ScanGenerationFiles(dir)
	if err != nil {
		t.Fatalf("ScanGenerationFiles: %v", err)
	}

	want := []string{"a.txt", "bin/claude", "lib/node_modules/foo/index.js"}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %v", len(files), len(want), files)
	}
	for i, f := range files {
		if f.Path != want[i] {
			t.Errorf("files[%d].Path = %q, want %q", i, f.Path, want[i])
		}
		if strings.Contains(f.Path, "\\") {
			t.Errorf("files[%d].Path contains a backslash: %q", i, f.Path)
		}
	}
}

// sampleReceipt builds a Receipt whose Files/ManifestDigest are consistent
// (as ScanGenerationFiles + ManifestDigest would produce), for tests that
// need a valid starting point.
func sampleReceipt(t *testing.T) *Receipt {
	t.Helper()
	files := []ReceiptFile{
		{Path: "bin/claude", Executable: true, Size: 5, SHA256: strings.Repeat("a", 64)},
		{Path: "lib/data.json", Executable: false, Size: 7, SHA256: strings.Repeat("b", 64)},
	}
	digest, err := ManifestDigest(files)
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	return &Receipt{
		SchemaVersion:  ReceiptSchemaVersion,
		Harness:        "claude",
		Version:        "2.1.220",
		Platform:       PlatformLinuxAMD64Glibc,
		RecipeRevision: "sha256:" + strings.Repeat("c", 64),
		LockEntryHash:  "sha256:" + strings.Repeat("d", 64),
		Backend:        ReceiptBackend{Name: "guix", GuixCommit: "deadbeef"},
		Files:          files,
		ManifestDigest: digest,
	}
}

func TestReceiptMarshalUnmarshalRoundTrip(t *testing.T) {
	r := sampleReceipt(t)

	data, err := r.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	data2, err := r.Marshal()
	if err != nil {
		t.Fatalf("Marshal (2): %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("Marshal is not deterministic:\n--- a ---\n%s\n--- b ---\n%s", data, data2)
	}

	got, err := UnmarshalReceipt(data)
	if err != nil {
		t.Fatalf("UnmarshalReceipt: %v", err)
	}
	roundTripped, err := got.Marshal()
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(data, roundTripped) {
		t.Fatalf("round trip is not byte-stable:\n--- original ---\n%s\n--- re-marshaled ---\n%s", data, roundTripped)
	}
}

func TestUnmarshalReceiptRejectsUnknownField(t *testing.T) {
	r := sampleReceipt(t)
	data, err := r.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Inject an unknown top-level field.
	mutated := bytes.Replace(data, []byte(`"schema_version": 1,`), []byte(`"schema_version": 1, "bogus_field": true,`), 1)
	if bytes.Equal(mutated, data) {
		t.Fatal("test setup: replacement did not apply")
	}
	if _, err := UnmarshalReceipt(mutated); err == nil {
		t.Fatal("expected an error for an unknown field, got nil")
	}
}

func TestUnmarshalReceiptRejectsGarbage(t *testing.T) {
	if _, err := UnmarshalReceipt([]byte("not json")); err == nil {
		t.Fatal("expected an error for invalid JSON, got nil")
	}
}

func TestReceiptValidateAcceptsWellFormed(t *testing.T) {
	r := sampleReceipt(t)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() on a well-formed receipt: %v", err)
	}
}

func TestReceiptValidateRejectsBadPlatformKey(t *testing.T) {
	r := sampleReceipt(t)
	r.Platform = PlatformKey("darwin-arm64")
	if err := r.Validate(); err == nil {
		t.Fatal("expected an error for an unrecognized platform key, got nil")
	}
}

func TestReceiptValidateRejectsUnsortedFiles(t *testing.T) {
	r := sampleReceipt(t)
	r.Files[0], r.Files[1] = r.Files[1], r.Files[0]
	if err := r.Validate(); err == nil {
		t.Fatal("expected an error for unsorted files, got nil")
	}
}

func TestReceiptValidateRejectsDuplicatePath(t *testing.T) {
	r := sampleReceipt(t)
	dup := r.Files[0]
	r.Files = []ReceiptFile{dup, dup}
	digest, err := ManifestDigest(r.Files)
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	r.ManifestDigest = digest
	if err := r.Validate(); err == nil {
		t.Fatal("expected an error for a duplicate path, got nil")
	}
}

func TestReceiptValidateRejectsMalformedDigest(t *testing.T) {
	t.Run("file sha256", func(t *testing.T) {
		r := sampleReceipt(t)
		r.Files[0].SHA256 = "not-hex"
		if err := r.Validate(); err == nil {
			t.Fatal("expected an error for a malformed file sha256, got nil")
		}
	})
	t.Run("manifest_digest shape", func(t *testing.T) {
		r := sampleReceipt(t)
		r.ManifestDigest = "not-a-digest"
		if err := r.Validate(); err == nil {
			t.Fatal("expected an error for a malformed manifest_digest, got nil")
		}
	})
	t.Run("manifest_digest mismatch", func(t *testing.T) {
		r := sampleReceipt(t)
		r.ManifestDigest = "sha256:" + strings.Repeat("0", 64)
		if err := r.Validate(); err == nil {
			t.Fatal("expected an error for a manifest_digest that doesn't match the files, got nil")
		}
	})
}

func TestVerifyGenerationTree(t *testing.T) {
	buildValid := func(t *testing.T) (string, *Receipt) {
		t.Helper()
		dir := t.TempDir()
		writeTreeFile(t, dir, "bin/claude", "#!/bin/sh\nexec node\n", 0o755)
		writeTreeFile(t, dir, "lib/data.json", `{"a":1}`, 0o644)
		files, err := ScanGenerationFiles(dir)
		if err != nil {
			t.Fatalf("ScanGenerationFiles: %v", err)
		}
		digest, err := ManifestDigest(files)
		if err != nil {
			t.Fatalf("ManifestDigest: %v", err)
		}
		r := &Receipt{
			SchemaVersion:  ReceiptSchemaVersion,
			Harness:        "claude",
			Version:        "1.0.0",
			Platform:       PlatformLinuxAMD64Glibc,
			RecipeRevision: "sha256:" + strings.Repeat("c", 64),
			LockEntryHash:  "sha256:" + strings.Repeat("d", 64),
			Backend:        ReceiptBackend{Name: "guix"},
			Files:          files,
			ManifestDigest: digest,
		}
		return dir, r
	}

	t.Run("matching tree passes", func(t *testing.T) {
		dir, r := buildValid(t)
		if err := VerifyGenerationTree(dir, r); err != nil {
			t.Fatalf("VerifyGenerationTree: %v", err)
		}
	})

	t.Run("added file", func(t *testing.T) {
		dir, r := buildValid(t)
		writeTreeFile(t, dir, "bin/extra", "surprise", 0o644)
		err := VerifyGenerationTree(dir, r)
		if err == nil || !strings.Contains(err.Error(), "bin/extra") {
			t.Fatalf("VerifyGenerationTree = %v, want an error naming bin/extra", err)
		}
	})

	t.Run("removed file", func(t *testing.T) {
		dir, r := buildValid(t)
		if err := os.Remove(filepath.Join(dir, "lib", "data.json")); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		err := VerifyGenerationTree(dir, r)
		if err == nil || !strings.Contains(err.Error(), "lib/data.json") {
			t.Fatalf("VerifyGenerationTree = %v, want an error naming lib/data.json", err)
		}
	})

	t.Run("content change", func(t *testing.T) {
		dir, r := buildValid(t)
		if err := os.WriteFile(filepath.Join(dir, "lib", "data.json"), []byte(`{"a":2}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err := VerifyGenerationTree(dir, r)
		if err == nil || !strings.Contains(err.Error(), "lib/data.json") {
			t.Fatalf("VerifyGenerationTree = %v, want an error naming lib/data.json", err)
		}
	})

	t.Run("exec bit change", func(t *testing.T) {
		dir, r := buildValid(t)
		if err := os.Chmod(filepath.Join(dir, "bin", "claude"), 0o644); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		err := VerifyGenerationTree(dir, r)
		if err == nil || !strings.Contains(err.Error(), "bin/claude") {
			t.Fatalf("VerifyGenerationTree = %v, want an error naming bin/claude", err)
		}
	})
}

func sampleLockHarnessForHash() LockHarness {
	return LockHarness{
		Source:         "npm",
		Package:        "@anthropic-ai/claude-code",
		Version:        "2.1.220",
		PublishedAt:    "2026-07-29T12:00:00Z",
		FirstSeenAt:    "2026-07-29T12:10:00Z",
		RecipeRevision: "sha256:abc",
		Generations: map[PlatformKey]string{
			PlatformLinuxAMD64Glibc: "sha256:gen1",
		},
	}
}

func TestLockHarnessHashEqualValuesHashEqual(t *testing.T) {
	a := sampleLockHarnessForHash()
	b := sampleLockHarnessForHash()

	ha, err := a.Hash()
	if err != nil {
		t.Fatalf("Hash(a): %v", err)
	}
	hb, err := b.Hash()
	if err != nil {
		t.Fatalf("Hash(b): %v", err)
	}
	if ha != hb {
		t.Errorf("equal LockHarness values hashed differently: %q vs %q", ha, hb)
	}
	if !strings.HasPrefix(ha, "sha256:") {
		t.Errorf("Hash() = %q, want a sha256: prefix", ha)
	}
}

func TestLockHarnessHashChangesWithField(t *testing.T) {
	a := sampleLockHarnessForHash()
	b := sampleLockHarnessForHash()
	b.Version = "2.1.221"

	ha, err := a.Hash()
	if err != nil {
		t.Fatalf("Hash(a): %v", err)
	}
	hb, err := b.Hash()
	if err != nil {
		t.Fatalf("Hash(b): %v", err)
	}
	if ha == hb {
		t.Errorf("changing Version did not change the hash: %q", ha)
	}
}

func TestGenerationIDShape(t *testing.T) {
	digest := "sha256:a1b2c3d4" + strings.Repeat("e", 56)
	got, err := GenerationID("claude", "2.1.220", PlatformLinuxAMD64Glibc, digest)
	if err != nil {
		t.Fatalf("GenerationID: %v", err)
	}
	want := "claude-2.1.220-linux-amd64-glibc-a1b2c3d4"
	if got != want {
		t.Fatalf("GenerationID() = %q, want %q", got, want)
	}
}

func TestGenerationIDRejectsInvalidInputs(t *testing.T) {
	validDigest := "sha256:" + strings.Repeat("a", 64)

	cases := []struct {
		name    string
		harness string
		version string
		key     PlatformKey
		digest  string
	}{
		{"empty harness", "", "1.0.0", PlatformLinuxAMD64Glibc, validDigest},
		{"empty version", "claude", "", PlatformLinuxAMD64Glibc, validDigest},
		{"harness with slash", "cla/ude", "1.0.0", PlatformLinuxAMD64Glibc, validDigest},
		{"version with slash", "claude", "1.0/0", PlatformLinuxAMD64Glibc, validDigest},
		{"bad platform key", "claude", "1.0.0", PlatformKey("bogus"), validDigest},
		{"malformed digest, no prefix", "claude", "1.0.0", PlatformLinuxAMD64Glibc, strings.Repeat("a", 64)},
		{"malformed digest, short", "claude", "1.0.0", PlatformLinuxAMD64Glibc, "sha256:abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := GenerationID(c.harness, c.version, c.key, c.digest); err == nil {
				t.Fatalf("GenerationID(%q, %q, %q, %q): expected an error, got nil", c.harness, c.version, c.key, c.digest)
			}
		})
	}
}
