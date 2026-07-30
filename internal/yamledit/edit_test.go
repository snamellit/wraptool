package yamledit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func noValidate([]byte) error { return nil }

// TestEdit_RoundTripPreservesCommentsAndOrder: a no-op edit must not lose a
// head comment, an inline comment, or reorder keys — the baseline contract
// both internal/config and cmd/harness.go's manifest editor depend on.
func TestEdit_RoundTripPreservesCommentsAndOrder(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "doc.yaml", "# head comment\nzeta: 1\nalpha: 2 # inline\nmiddle:\n  nested: 3\n")

	err := Edit(path, Options{Kind: "document", FileMode: 0o600, Validate: noValidate}, func(*yaml.Node) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)

	if !strings.Contains(gotStr, "# head comment") {
		t.Errorf("head comment lost:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "alpha: 2 # inline") {
		t.Errorf("inline comment lost or reformatted:\n%s", gotStr)
	}
	// Key order (zeta before alpha before middle) must survive — yaml.Node
	// preserves declaration order, unlike a plain map[string]any decode.
	zetaIdx := strings.Index(gotStr, "zeta:")
	alphaIdx := strings.Index(gotStr, "alpha:")
	middleIdx := strings.Index(gotStr, "middle:")
	if zetaIdx >= alphaIdx || alphaIdx >= middleIdx {
		t.Errorf("key order not preserved, got:\n%s", gotStr)
	}
}

// TestEdit_BlankLinesBetweenKeysAreNormalized pins yaml.v3's own re-encoding
// behavior (not something yamledit adds or removes): a blank line separating
// two mapping entries does not survive a decode/encode round-trip, while a
// blank line directly under a head comment does. This is "current behavior"
// to preserve, not a guarantee either editor advertises to its users.
func TestEdit_BlankLinesBetweenKeysAreNormalized(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "doc.yaml", "# head comment\n\na: 1\n\nb: 2\n")

	if err := Edit(path, Options{Kind: "document", FileMode: 0o600, Validate: noValidate}, func(*yaml.Node) error {
		return nil
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# head comment\n\na: 1\nb: 2\n"
	if string(got) != want {
		t.Errorf("blank-line normalization changed; got:\n%q\nwant:\n%q", got, want)
	}
}

// TestEdit_InPlaceScalarCommentPreserved exercises the full Edit driver
// (not just MapSet in isolation): editing an existing scalar field via
// MapSet inside the mutate func must keep that field's inline comment.
func TestEdit_InPlaceScalarCommentPreserved(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "doc.yaml", "value: 168h # seven days\n")

	err := Edit(path, Options{Kind: "document", FileMode: 0o600, Validate: noValidate}, func(doc *yaml.Node) error {
		MapSet(doc, "value", ScalarNode("72h"))
		return nil
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "value: 72h # seven days\n"
	if string(got) != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// TestEdit_StructuralReplacementFallback: replacing a scalar with a mapping
// is a structural change MapSet cannot preserve a comment across (there is
// no equivalent field on a mapping node to carry it) — Edit must still
// succeed, just without the old comment.
func TestEdit_StructuralReplacementFallback(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "doc.yaml", "value: leaf # will be dropped\n")

	err := Edit(path, Options{Kind: "document", FileMode: 0o600, Validate: noValidate}, func(doc *yaml.Node) error {
		child := &yaml.Node{Kind: yaml.MappingNode}
		MapSet(child, "nested", ScalarNode("x"))
		MapSet(doc, "value", child)
		return nil
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if strings.Contains(gotStr, "will be dropped") {
		t.Errorf("expected the inline comment to be dropped on a structural change, got:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, "nested: x") {
		t.Errorf("expected the new mapping content, got:\n%s", gotStr)
	}
}

// TestEdit_ValidatorRejectionLeavesFileUntouched: when Validate rejects the
// re-encoded bytes, Edit must return the error and the file on disk must be
// byte-for-byte unchanged — no partial write, no temp-file leftover applied.
func TestEdit_ValidatorRejectionLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	original := "a: 1\n"
	path := writeFile(t, dir, "doc.yaml", original)

	err := Edit(path, Options{
		Kind:     "document",
		FileMode: 0o600,
		Validate: func([]byte) error { return errRejected },
	}, func(doc *yaml.Node) error {
		MapSet(doc, "a", ScalarNode("2"))
		return nil
	})
	if err == nil {
		t.Fatal("expected Edit to fail validation")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error should wrap the validator's error, got: %v", err)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Errorf("file modified despite validation failure; got:\n%q\nwant:\n%q", got, original)
	}
}

// TestEdit_MutateErrorLeavesFileUntouched: the same guarantee, but for a
// mutate func that itself errors before Edit ever re-encodes.
func TestEdit_MutateErrorLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	original := "a: 1\n"
	path := writeFile(t, dir, "doc.yaml", original)

	err := Edit(path, Options{Kind: "document", FileMode: 0o600, Validate: noValidate}, func(doc *yaml.Node) error {
		return errRejected
	})
	if err == nil {
		t.Fatal("expected Edit to propagate the mutate error")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Errorf("file modified despite mutate failure; got:\n%q\nwant:\n%q", got, original)
	}
}

var errRejected = &rejectError{}

type rejectError struct{}

func (*rejectError) Error() string { return "rejected" }

// TestEdit_RejectsZeroFileMode: a zero-value FileMode would write a
// mode-0000 file (unreadable, effectively bricking the config/manifest it
// replaced), so Edit refuses it outright rather than silently writing an
// unusable file. The check must run before touching path, so a caller's
// mistake never even reads or overwrites anything.
func TestEdit_RejectsZeroFileMode(t *testing.T) {
	dir := t.TempDir()
	original := "a: 1\n"
	path := writeFile(t, dir, "doc.yaml", original)

	err := Edit(path, Options{Kind: "document", Validate: noValidate}, func(doc *yaml.Node) error {
		MapSet(doc, "a", ScalarNode("2"))
		return nil
	})
	if err == nil {
		t.Fatal("expected Edit to reject a zero FileMode")
	}
	if !strings.Contains(err.Error(), "FileMode") {
		t.Errorf("error should name the missing field, got: %v", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Errorf("file modified despite the FileMode rejection; got:\n%q\nwant:\n%q", got, original)
	}
}

// TestEdit_RejectsNilValidate: a nil Validate would write an edit with no
// semantic check at all onto a security-policy file (config.yaml,
// manifest.yaml) — Edit refuses it outright, before touching path, rather
// than silently skipping validation.
func TestEdit_RejectsNilValidate(t *testing.T) {
	dir := t.TempDir()
	original := "a: 1\n"
	path := writeFile(t, dir, "doc.yaml", original)

	err := Edit(path, Options{Kind: "document", FileMode: 0o600}, func(doc *yaml.Node) error {
		MapSet(doc, "a", ScalarNode("2"))
		return nil
	})
	if err == nil {
		t.Fatal("expected Edit to reject a nil Validate")
	}
	if !strings.Contains(err.Error(), "Validate") {
		t.Errorf("error should name the missing field, got: %v", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Errorf("file modified despite the Validate rejection; got:\n%q\nwant:\n%q", got, original)
	}
}

// TestEdit_SeedIfMissing: a missing file with SeedIfMissing seeds an empty
// mapping document instead of erroring, mirroring cmd/harness.go's
// manifest.yaml bootstrap (`policy set`/`install` are the manifest's own
// init step).
func TestEdit_SeedIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.yaml")

	err := Edit(path, Options{Kind: "document", FileMode: 0o600, SeedIfMissing: true, Validate: noValidate}, func(doc *yaml.Node) error {
		MapSet(doc, "a", ScalarNode("1"))
		return nil
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a: 1\n" {
		t.Errorf("got %q, want %q", got, "a: 1\n")
	}
}

// TestEdit_MissingFileErrorsWithoutSeedIfMissing: the default (SeedIfMissing
// false, matching internal/config's editConfig) is that a missing file is an
// error, never silently creates one.
func TestEdit_MissingFileErrorsWithoutSeedIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.yaml")

	err := Edit(path, Options{Kind: "config", FileMode: 0o600, Validate: noValidate}, func(*yaml.Node) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("Edit must not create the file when SeedIfMissing is false")
	}
}

// TestAtomicWrite_TempFileInSameDirectory: AtomicWrite must leave exactly
// the target file behind — no leftover temp file — and the write must not
// cross a directory boundary (os.CreateTemp is given path's own directory),
// which is what makes the final os.Rename atomic and same-filesystem. It
// also must not create a missing destination directory: mirroring
// internal/config's original writeAtomic, callers own directory creation.
func TestAtomicWrite_TempFileInSameDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")

	if err := AtomicWrite(path, []byte("a: 1\n"), 0o640); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.yaml" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contents = %v, want exactly [out.yaml] (no leftover temp file)", names)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a: 1\n" {
		t.Errorf("content = %q, want %q", got, "a: 1\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
}

// TestAtomicWrite_MissingParentDirErrors documents that AtomicWrite does not
// create the destination directory — a caller (e.g. cmd/harness.go's
// editManifest, which runs inside pool.LockPool and so already has its
// directory made by the lock acquisition) is responsible for that.
func TestAtomicWrite_MissingParentDirErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-subdir", "out.yaml")

	if err := AtomicWrite(path, []byte("a: 1\n"), 0o600); err == nil {
		t.Error("expected an error when the parent directory does not exist")
	}
}
