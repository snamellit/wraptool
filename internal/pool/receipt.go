package pool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReceiptSchemaVersion is the schema_version this package writes and
// understands for receipt.json.
const ReceiptSchemaVersion = 1

// ReceiptFile is one entry of the digested file manifest (§11.5, §6.4:
// content is the identity — never mtimes, uids, or host paths).
type ReceiptFile struct {
	Path       string `json:"path"`       // generation-relative, forward slashes (path.ToSlash), sorted
	Executable bool   `json:"executable"` // any of the owner/group/other exec bits (0o111) set; the flag, never the raw mode
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"` // lowercase hex, no "sha256:" prefix
}

// ReceiptBackend records which provisioning backend produced a generation
// (§11.5). This is environment identity, not content — it sits outside
// ManifestDigest so that a lock + recipe realized on different hosts/backends
// still yields an identical file-manifest digest (§17 cross-backend test).
type ReceiptBackend struct {
	Name        string `json:"name"`                   // "guix" | "oci"
	ImageDigest string `json:"image_digest,omitempty"` // oci image digest
	GuixCommit  string `json:"guix_commit,omitempty"`  // best-effort guix channel commit
}

// Receipt is the recorded shape of one generation's realization (§11.5):
// what produced it (harness, version, platform, recipe, lock entry,
// backend) and what it contains (the digested file manifest). It is written
// as runtime/generations/<generation-id>/receipt.json.
type Receipt struct {
	SchemaVersion  int            `json:"schema_version"` // ReceiptSchemaVersion = 1
	Harness        string         `json:"harness"`
	Version        string         `json:"version"`
	Platform       PlatformKey    `json:"platform"`
	RecipeRevision string         `json:"recipe_revision"` // "sha256:<hex>"
	LockEntryHash  string         `json:"lock_entry_hash"` // "sha256:<hex>"
	Backend        ReceiptBackend `json:"backend"`         // environment identity — OUTSIDE the digest (§11.5)
	Files          []ReceiptFile  `json:"files"`           // sorted by Path
	ManifestDigest string         `json:"manifest_digest"` // "sha256:<hex>" over Files only
}

// sha256DigestPrefix is prepended to every "sha256:<hex>" digest string this
// package produces (recipe revisions, lock-entry hashes, manifest digests) —
// distinct from ReceiptFile.SHA256, which is bare hex (§11.5's field comment).
const sha256DigestPrefix = "sha256:"

// ScanGenerationFiles walks root (a generation tree, or a staging tree about
// to become one) and returns its digested file manifest, sorted by Path
// (plain byte sort). It uses WalkDir, whose directory entries never resolve
// symlinks — the equivalent of an explicit Lstat scan (§6.4) — so a symlink
// anywhere in the tree is seen as a symlink, never silently followed into
// its target.
//
// It FAILS the whole scan (never skips) on:
//   - any symlink, naming the offending root-relative path;
//   - any irregular file (device, fifo, socket — anything that is neither a
//     regular file nor a directory), naming the path and its mode;
//   - any two paths whose full root-relative path is equal after case
//     folding, naming both paths (APFS/NTFS would merge them, §6.4).
//
// The top-level receipt.json is excluded (a receipt cannot contain itself);
// a receipt.json nested under a subdirectory is an ordinary file and is
// included. Empty directories contribute no entry — content is the identity,
// not the tree shape. Executable records only whether any of the host file's
// 0o111 (owner/group/other execute) bits are set, never the raw mode
// (§6.4: "The receipt records an executable: true flag per entry (not the
// raw mode), so receipts hash identically across hosts").
func ScanGenerationFiles(root string) ([]ReceiptFile, error) {
	// Lstat root itself before walking: WalkDir's callback never sees root's
	// own entry, so a symlinked root (the reported failure mode: a
	// symlink-to-directory root silently walked into its target, producing
	// an empty manifest for an unverified tree) or a missing/non-directory
	// root would otherwise pass through unnoticed instead of failing loudly.
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("stat generation root %s: %w", root, err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("generation tree contains a symlink at %s: generations must be symlink-free (§6.4)", root)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("generation root %s is not a directory", root)
	}

	var files []ReceiptFile
	seen := make(map[string]string) // case-folded relative path -> first-seen original relative path

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking %s: %w", p, err)
		}
		if p == root {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", p, err)
		}
		relSlash := filepath.ToSlash(rel)

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relSlash, err)
		}
		mode := info.Mode()

		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("generation tree contains a symlink at %s: generations must be symlink-free (§6.4)", relSlash)
		}
		if !mode.IsRegular() {
			return fmt.Errorf("generation tree contains an irregular file at %s (mode %s): only regular files and directories are allowed", relSlash, mode)
		}

		if relSlash == "receipt.json" {
			return nil
		}

		folded := strings.ToLower(relSlash)
		if prior, ok := seen[folded]; ok {
			return fmt.Errorf("generation tree has case-colliding paths %q and %q, which would merge on a case-insensitive filesystem (§6.4)", prior, relSlash)
		}
		seen[folded] = relSlash

		sum, err := hashFile(p)
		if err != nil {
			return fmt.Errorf("hashing %s: %w", relSlash, err)
		}

		files = append(files, ReceiptFile{
			Path:       relSlash,
			Executable: mode.Perm()&0o111 != 0,
			Size:       info.Size(),
			SHA256:     sum,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// hashFile returns the lowercase hex SHA256 of the file at path's content.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ManifestDigest returns "sha256:<hex>" over the deterministic JSON
// serialization of files. encoding/json's struct field order is fixed by
// ReceiptFile's type definition (the same argument Lock.Marshal's doc
// comment makes), so this is stable across calls given an equal, equally
// ordered slice. Callers pass the already-sorted result of
// ScanGenerationFiles (or a Receipt's own Files, which Validate additionally
// checks are sorted) — ManifestDigest does not itself sort or deduplicate.
// A nil files is normalized to an empty slice before marshaling: encoding/json
// renders a nil slice as "null" and an empty (non-nil) slice as "[]", so
// without this a manifest-less generation would hash differently depending
// on which of those two equivalent Go values happened to be in hand.
func ManifestDigest(files []ReceiptFile) (string, error) {
	if files == nil {
		files = []ReceiptFile{}
	}
	data, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("marshaling files for manifest digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return sha256DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// GenerationID returns the generation id
// "<harness>-<version>-<platform-key>-<digest8>" (§5), where digest8 is the
// first 8 hex characters of manifestDigest. harness and version must be
// non-empty, single path segments (no "/" or "\", and not "." or ".."), key
// must be one of the closed PlatformKey values, and manifestDigest must be a
// well-formed "sha256:" + 64 lowercase hex digest.
func GenerationID(harness, version string, key PlatformKey, manifestDigest string) (string, error) {
	if err := validatePathSegment("harness", harness); err != nil {
		return "", err
	}
	if err := validatePathSegment("version", version); err != nil {
		return "", err
	}
	if _, err := ParsePlatformKey(string(key)); err != nil {
		return "", fmt.Errorf("platform key: %w", err)
	}
	digestHex, err := parsePrefixedSHA256(manifestDigest)
	if err != nil {
		return "", fmt.Errorf("manifest digest: %w", err)
	}
	return fmt.Sprintf("%s-%s-%s-%s", harness, version, key, digestHex[:8]), nil
}

// validatePathSegment rejects a value that cannot be a single path segment
// on any host filesystem: empty, containing a path separator, or one of the
// dot-segments "." / "..". Used for GenerationID's harness/version inputs,
// which become part of a generation directory name (§5).
func validatePathSegment(label, s string) error {
	if s == "" {
		return fmt.Errorf("%s must not be empty", label)
	}
	if strings.ContainsAny(s, "/\\") {
		return fmt.Errorf("%s %q must not contain a path separator", label, s)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("%s must not be %q", label, s)
	}
	return nil
}

// validateReceiptFilePath enforces receipt path discipline on one
// ReceiptFile.Path (§6.3 "nothing inside runtime/ may contain an absolute
// path ... receipts' file lists"; §6.4): non-empty, never filepath.IsAbs
// (host-rooted paths have no meaning once the pool is mounted at a
// different path on a different runtime), never containing a backslash
// (receipt paths are ToSlash'd forward-slash paths, never host-native on
// Windows), and no "." or ".." path segment (either could let a consumer
// that joins Path onto a root escape the generation tree).
func validateReceiptFilePath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("path %q must not be absolute", p)
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("path %q must not contain a backslash", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("path %q must not contain a %q segment", p, seg)
		}
	}
	return nil
}

// parsePrefixedSHA256 validates that digest has the form "sha256:" followed
// by exactly 64 lowercase hex characters, and returns the hex part.
func parsePrefixedSHA256(digest string) (string, error) {
	hexPart, ok := strings.CutPrefix(digest, sha256DigestPrefix)
	if !ok {
		return "", fmt.Errorf("digest %q must have the %q prefix", digest, sha256DigestPrefix)
	}
	if !isLowerHex64(hexPart) {
		return "", fmt.Errorf("digest %q must be %q followed by 64 lowercase hex characters", digest, sha256DigestPrefix)
	}
	return hexPart, nil
}

// isLowerHex64 reports whether s is exactly 64 lowercase hex characters —
// the shape of a bare (unprefixed) SHA256 digest, as recorded in
// ReceiptFile.SHA256.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Marshal serializes the receipt deterministically, following the same
// reasoning as Lock.Marshal: encoding/json's struct field order is fixed by
// the type, and SetEscapeHTML(false) keeps characters like "<" (which can
// appear in version constraints elsewhere in the pool) from being rewritten
// to <.
func (r *Receipt) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("marshaling receipt: %w", err)
	}
	// json.Encoder.Encode appends a trailing newline; strip it so Marshal's
	// output shape matches a plain json.MarshalIndent caller would expect.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// UnmarshalReceipt parses receipt JSON produced by Marshal. Decoding is
// strict (DisallowUnknownFields, mirroring the KnownFields discipline
// LoadManifest applies to manifest.yaml): an unrecognized field fails to
// load rather than being silently dropped. The parsed receipt is then
// validated via Validate, so a structurally malformed or internally
// inconsistent receipt is rejected outright rather than accepted and
// mishandled later.
func UnmarshalReceipt(data []byte) (*Receipt, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var r Receipt
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parsing receipt: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks that r is internally well-formed: schema version, that
// harness/version/recipe revision/lock-entry hash are non-empty, that
// Platform is one of the closed PlatformKey values (via ParsePlatformKey,
// reusing platform.go's existing validity check rather than duplicating its
// exhaustive switch), that every file's Path satisfies receipt path
// discipline (§6.3/§6.4 — non-empty, relative, forward-slash-only, no "."/".."
// segment) with no two paths colliding after case folding, that Files is
// sorted by Path with no duplicates and every SHA256 is well-formed, and
// that ManifestDigest matches recomputing ManifestDigest(r.Files). Exported
// so a receipt assembled in memory by a Realize implementation can be
// validated before it is written, not only after being read back via
// UnmarshalReceipt (which also calls Validate).
func (r *Receipt) Validate() error {
	if r.SchemaVersion != ReceiptSchemaVersion {
		return fmt.Errorf("unsupported receipt schema_version %d (want %d)", r.SchemaVersion, ReceiptSchemaVersion)
	}
	if r.Harness == "" {
		return fmt.Errorf("harness must not be empty")
	}
	if r.Version == "" {
		return fmt.Errorf("version must not be empty")
	}
	if r.RecipeRevision == "" {
		return fmt.Errorf("recipe_revision must not be empty")
	}
	if r.LockEntryHash == "" {
		return fmt.Errorf("lock_entry_hash must not be empty")
	}
	if _, err := ParsePlatformKey(string(r.Platform)); err != nil {
		return fmt.Errorf("platform: %w", err)
	}

	// case-folded path -> first-seen original path, so a collision names
	// both offending entries even when they are far apart in sort order
	// (e.g. "Bin/x" and "bin/X" sort on opposite sides of "a.txt") — the
	// same approach ScanGenerationFiles uses.
	folded := make(map[string]string, len(r.Files))
	for i, f := range r.Files {
		if err := validateReceiptFilePath(f.Path); err != nil {
			return fmt.Errorf("files: %w", err)
		}
		if !isLowerHex64(f.SHA256) {
			return fmt.Errorf("files: %q has a malformed sha256 %q (want 64 lowercase hex characters)", f.Path, f.SHA256)
		}
		fold := strings.ToLower(f.Path)
		if prior, ok := folded[fold]; ok {
			return fmt.Errorf("files: case-colliding paths %q and %q would merge on a case-insensitive filesystem", prior, f.Path)
		}
		folded[fold] = f.Path
		if i == 0 {
			continue
		}
		prev := r.Files[i-1]
		switch {
		case f.Path == prev.Path:
			return fmt.Errorf("files: duplicate path %q", f.Path)
		case f.Path < prev.Path:
			return fmt.Errorf("files: not sorted (%q must come after %q)", f.Path, prev.Path)
		}
	}

	if _, err := parsePrefixedSHA256(r.ManifestDigest); err != nil {
		return fmt.Errorf("manifest_digest: %w", err)
	}
	digest, err := ManifestDigest(r.Files)
	if err != nil {
		return fmt.Errorf("recomputing manifest digest: %w", err)
	}
	if digest != r.ManifestDigest {
		return fmt.Errorf("manifest_digest %q does not match the digest recomputed from files (%q)", r.ManifestDigest, digest)
	}
	return nil
}

// VerifyGenerationTree re-scans root via ScanGenerationFiles, recomputes its
// manifest digest, and compares both the digest and the per-file list
// against r — the host-side verification step after realization (§11.4 step
// 6). On any mismatch the error names the first offending path: a file
// present on disk but not in the receipt, a file in the receipt but missing
// from disk, or a file present in both whose executable flag, size, or
// content (sha256) differs.
//
// r.Files is assumed Validate-clean, in particular sorted by Path: a
// receipt that has round-tripped through UnmarshalReceipt already satisfies
// this (Validate is called there), but an in-memory Receipt built by hand
// with unsorted Files will produce spurious mismatches from this function's
// merge comparison rather than a meaningful diff. Call r.Validate() first if
// that isn't already guaranteed.
func VerifyGenerationTree(root string, r *Receipt) error {
	actual, err := ScanGenerationFiles(root)
	if err != nil {
		return err
	}

	ai, ri := 0, 0
	for ai < len(actual) && ri < len(r.Files) {
		a, want := actual[ai], r.Files[ri]
		switch {
		case a.Path < want.Path:
			return fmt.Errorf("generation tree has file %q which is not recorded in the receipt", a.Path)
		case a.Path > want.Path:
			return fmt.Errorf("generation tree is missing file %q, which is recorded in the receipt", want.Path)
		default:
			if a.Executable != want.Executable {
				return fmt.Errorf("file %q: executable flag differs (tree %v, receipt %v)", a.Path, a.Executable, want.Executable)
			}
			if a.Size != want.Size || a.SHA256 != want.SHA256 {
				return fmt.Errorf("file %q: content differs from the receipt", a.Path)
			}
			ai++
			ri++
		}
	}
	if ai < len(actual) {
		return fmt.Errorf("generation tree has file %q which is not recorded in the receipt", actual[ai].Path)
	}
	if ri < len(r.Files) {
		return fmt.Errorf("generation tree is missing file %q, which is recorded in the receipt", r.Files[ri].Path)
	}

	digest, err := ManifestDigest(actual)
	if err != nil {
		return fmt.Errorf("computing manifest digest: %w", err)
	}
	if digest != r.ManifestDigest {
		return fmt.Errorf("generation tree manifest digest %q does not match receipt manifest_digest %q", digest, r.ManifestDigest)
	}
	return nil
}
