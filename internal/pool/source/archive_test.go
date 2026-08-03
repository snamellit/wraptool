package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// --- shared fixtures / helpers ------------------------------------------

// sha512Hex returns content's SHA-512 digest, hex-encoded — the shape this
// channel's manifest publishes (unlike npm's base64 SRI, see npm_test.go's
// sriFor).
func sha512Hex(content []byte) string {
	sum := sha512.Sum512(content)
	return hex.EncodeToString(sum[:])
}

// newManifestServer serves manifests[platform] as JSON at
// /manifests/<platform>.json for every key present; any platform NOT a key
// of manifests 404s (httptest.NewServer's default ServeMux behavior for an
// unregistered pattern) — modeling a mid-rollout or as-yet-unpublished
// platform.
func newManifestServer(t *testing.T, manifests map[string]archiveManifest) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for platform, m := range manifests {
		m := m
		mux.HandleFunc("/manifests/"+platform+".json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(m); err != nil {
				t.Errorf("encoding fixture manifest: %v", err)
			}
		})
	}
	return httptest.NewServer(mux)
}

// fatalBackend is a backend.Backend test double that fails the test the
// instant ANY of its methods is called — the regression fixture for §6.2's
// "archive sources resolve in pure Go, no backend" and §11.4's realize
// counterpart: Archive.Resolve/Realize must never invoke b at all.
type fatalBackend struct{ t *testing.T }

func (f *fatalBackend) Name() string {
	f.t.Helper()
	f.t.Fatal("fatalBackend.Name called: archive Resolve/Realize must never call into the backend")
	return ""
}

func (f *fatalBackend) Available() bool {
	f.t.Helper()
	f.t.Fatal("fatalBackend.Available called: archive Resolve/Realize must never call into the backend")
	return false
}

func (f *fatalBackend) Run(_ context.Context, _ backend.RunSpec) error {
	f.t.Helper()
	f.t.Fatal("fatalBackend.Run called: archive Resolve/Realize must never call into the backend")
	return nil
}

var _ backend.Backend = (*fatalBackend)(nil)

// --- Refresh -------------------------------------------------------------

func TestRefresh_ArchiveMapsPlatformsAndDedups(t *testing.T) {
	srv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64":      {Version: "1.1.10", URL: "https://example.com/amd64.tar.gz", SHA512: strings.Repeat("a", 128)},
		"linux_arm64":      {Version: "1.1.10", URL: "https://example.com/arm64.tar.gz", SHA512: strings.Repeat("b", 128)},
		"linux_amd64_musl": {Version: "1.1.9", URL: "https://example.com/amd64-musl.tar.gz", SHA512: strings.Repeat("c", 128)},
		"linux_arm64_musl": {Version: "1.1.10", URL: "https://example.com/arm64-musl.tar.gz", SHA512: strings.Repeat("d", 128)},
	})
	defer srv.Close()

	a := NewArchive(srv.URL)
	candidates, err := a.Refresh(context.Background(), "agy")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2 distinct versions: %+v", len(candidates), candidates)
	}

	versions := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		versions[c.Version] = true
		if !c.PublishedAt.IsZero() {
			t.Errorf("candidate %s: PublishedAt = %v, want zero (this channel has no publish history — must not invent one)", c.Version, c.PublishedAt)
		}
		if !c.FirstSeenAt.IsZero() {
			t.Errorf("candidate %s: FirstSeenAt = %v, want zero (orchestrator fills it)", c.Version, c.FirstSeenAt)
		}
		if c.Yanked {
			t.Errorf("candidate %s: Yanked = true, want false (this channel has no yank concept)", c.Version)
		}
	}
	if !versions["1.1.10"] || !versions["1.1.9"] {
		t.Errorf("candidate versions = %v, want {1.1.10, 1.1.9}", versions)
	}
}

func TestRefresh_ArchiveToleratesOneUnreachablePlatform(t *testing.T) {
	// Only two of the four platforms have a manifest; the other two 404 —
	// modeling a channel that (today) has not published musl artifacts.
	srv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64": {Version: "1.1.10", URL: "https://example.com/amd64.tar.gz", SHA512: strings.Repeat("a", 128)},
		"linux_arm64": {Version: "1.1.10", URL: "https://example.com/arm64.tar.gz", SHA512: strings.Repeat("b", 128)},
	})
	defer srv.Close()

	a := NewArchive(srv.URL)
	candidates, err := a.Refresh(context.Background(), "agy")
	if err != nil {
		t.Fatalf("Refresh: %v (a platform 404ing must not fail the whole call)", err)
	}
	if len(candidates) != 1 || candidates[0].Version != "1.1.10" {
		t.Fatalf("candidates = %+v, want exactly one, version 1.1.10", candidates)
	}
}

func TestRefresh_ArchiveFailsWhenAllPlatformsUnreachable(t *testing.T) {
	srv := newManifestServer(t, map[string]archiveManifest{})
	defer srv.Close()

	a := NewArchive(srv.URL)
	if _, err := a.Refresh(context.Background(), "agy"); err == nil {
		t.Fatal("Refresh: got nil error, want failure when not one platform manifest is reachable")
	}
}

// --- Resolve ---------------------------------------------------------------

func TestResolve_ArchiveLocksOnlyVersionMatchingPlatforms(t *testing.T) {
	srv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64": {Version: "1.1.10", URL: "https://example.com/amd64.tar.gz", SHA512: strings.Repeat("a", 128)},
		// mid-rollout: arm64 still reports the OLD version.
		"linux_arm64": {Version: "1.1.9", URL: "https://example.com/arm64.tar.gz", SHA512: strings.Repeat("b", 128)},
		// musl platforms have no manifest at all (404) — absent entirely.
	})
	defer srv.Close()

	a := NewArchive(srv.URL)
	fb := &fatalBackend{t: t}
	lh, err := a.Resolve(context.Background(), fb, "agy", "1.1.10", t.TempDir(), "", "sha256:recipe-rev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if lh.Source != "archive" {
		t.Errorf("Source = %q, want %q", lh.Source, "archive")
	}
	if lh.Package != "agy" {
		t.Errorf("Package = %q, want %q", lh.Package, "agy")
	}
	if lh.Version != "1.1.10" {
		t.Errorf("Version = %q, want %q", lh.Version, "1.1.10")
	}
	if lh.RecipeRevision != "sha256:recipe-rev" {
		t.Errorf("RecipeRevision = %q, want %q", lh.RecipeRevision, "sha256:recipe-rev")
	}
	zero := pool.FormatTimestamp(time.Time{})
	if lh.PublishedAt != zero {
		t.Errorf("PublishedAt = %q, want the zero-formatted timestamp %q (this channel has no publish history)", lh.PublishedAt, zero)
	}
	if lh.FirstSeenAt != zero {
		t.Errorf("FirstSeenAt = %q, want the zero-formatted timestamp %q (orchestrator fills it in)", lh.FirstSeenAt, zero)
	}

	if lh.Archive == nil {
		t.Fatal("Archive is nil")
	}
	if len(lh.Archive.Artifacts) != 1 {
		t.Fatalf("got %d artifacts, want 1 (only linux-amd64-glibc's manifest reports 1.1.10): %+v", len(lh.Archive.Artifacts), lh.Archive.Artifacts)
	}
	art, ok := lh.Archive.Artifacts[pool.PlatformLinuxAMD64Glibc]
	if !ok {
		t.Fatal("missing linux-amd64-glibc artifact")
	}
	if art.URL != "https://example.com/amd64.tar.gz" {
		t.Errorf("URL = %q, want %q", art.URL, "https://example.com/amd64.tar.gz")
	}
	if art.SHA512 != strings.Repeat("a", 128) {
		t.Errorf("SHA512 = %q, want %q", art.SHA512, strings.Repeat("a", 128))
	}
	if art.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty (not yet Fetched)", art.SHA256)
	}
	if _, ok := lh.Archive.Artifacts[pool.PlatformLinuxARM64Glibc]; ok {
		t.Error("linux-arm64-glibc artifact present, want absent (its manifest reports a different version)")
	}
}

func TestResolve_ArchiveErrorsWhenNoPlatformReportsTheVersion(t *testing.T) {
	srv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64": {Version: "1.1.9", URL: "https://example.com/amd64.tar.gz", SHA512: strings.Repeat("a", 128)},
	})
	defer srv.Close()

	a := NewArchive(srv.URL)
	fb := &fatalBackend{t: t}
	_, err := a.Resolve(context.Background(), fb, "agy", "1.1.10", t.TempDir(), "", "sha256:recipe-rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection when no platform reports the requested version")
	}
}

func TestResolve_ArchiveIgnoresBAndNodeVersionArguments(t *testing.T) {
	// b MUST NEVER be called (fatalBackend enforces this); nodeVersion is
	// accepted only for Source-interface parity and must not affect the
	// result at all.
	srv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64": {Version: "1.1.10", URL: "https://example.com/amd64.tar.gz", SHA512: strings.Repeat("a", 128)},
	})
	defer srv.Close()

	a := NewArchive(srv.URL)
	fb := &fatalBackend{t: t}
	lh, err := a.Resolve(context.Background(), fb, "agy", "1.1.10", t.TempDir(), "99.99.99", "sha256:recipe-rev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lh.Runtime != nil {
		t.Errorf("Runtime = %+v, want nil (archive harnesses bundle no separate runtime)", lh.Runtime)
	}
}

// --- Fetch -------------------------------------------------------------

func TestFetch_ArchiveVerifiesSHA512AndFillsSHA256(t *testing.T) {
	content := []byte("fake agy tar.gz bytes")
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	lh := &pool.LockHarness{
		Package: "agy",
		Archive: &pool.LockArchive{
			Artifacts: map[pool.PlatformKey]pool.LockArchiveArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/agy.tar.gz", SHA512: sha512Hex(content)},
			},
		},
	}

	a := NewArchive("")
	if err := a.Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}

	wantDigest := sha256Hex(content)
	got := lh.Archive.Artifacts[pool.PlatformLinuxAMD64Glibc]
	if got.SHA256 != wantDigest {
		t.Errorf("SHA256 = %q, want %q (filled in from empty)", got.SHA256, wantDigest)
	}
	cached, err := os.ReadFile(filepath.Join(cacheDir, wantDigest))
	if err != nil {
		t.Fatalf("reading cached object: %v", err)
	}
	if !bytes.Equal(cached, content) {
		t.Error("cached object content does not match downloaded content")
	}
}

func TestFetch_ArchiveSHA512MismatchAdmitsNothing(t *testing.T) {
	content := []byte("actual bytes served")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	lh := &pool.LockHarness{
		Package: "agy",
		Archive: &pool.LockArchive{
			Artifacts: map[pool.PlatformKey]pool.LockArchiveArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/agy.tar.gz", SHA512: sha512Hex([]byte("different bytes entirely"))},
			},
		},
	}

	a := NewArchive("")
	err := a.Fetch(context.Background(), lh, cacheDir)
	if err == nil {
		t.Fatal("Fetch: got nil error, want a sha512 mismatch rejection")
	}
	if !strings.Contains(err.Error(), string(pool.PlatformLinuxAMD64Glibc)) {
		t.Errorf("error = %q, want it to name the platform key", err.Error())
	}

	entries, rerr := os.ReadDir(cacheDir)
	if rerr != nil {
		t.Fatalf("reading cache dir: %v", rerr)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".wraptool-pool-fetch-") {
			t.Errorf("cache dir contains %q after a mismatch — nothing should be admitted", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(cacheDir, sha256Hex(content))); err == nil {
		t.Error("mismatched object's digest exists in cache, want absent")
	}
}

func TestFetch_ArchiveAlreadyCachedSkipsDownload(t *testing.T) {
	content := []byte("already-cached bytes")
	digest := sha256Hex(content)
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, digest), content, 0o644); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Write(content)
	}))
	defer srv.Close()

	lh := &pool.LockHarness{
		Archive: &pool.LockArchive{
			Artifacts: map[pool.PlatformKey]pool.LockArchiveArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/agy.tar.gz", SHA512: sha512Hex(content), SHA256: digest},
			},
		},
	}

	a := NewArchive("")
	if err := a.Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 (already-cached object must skip the download)", requests)
	}
}

// --- Realize -----------------------------------------------------------

// buildAgyTarGZ builds an in-memory gzip-compressed tar: every entry in
// files, keyed by its path relative to the archive root, optionally
// prefixed by topDir (a single wrapping directory, or "" for a flat
// archive with no wrapping directory at all).
func buildAgyTarGZ(t *testing.T, topDir string, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		content := files[name]
		full := name
		if topDir != "" {
			full = topDir + "/" + name
		}
		hdr := &tar.Header{Name: full, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", full, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("writing tar content for %s: %v", full, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	return buf.Bytes()
}

// seedArchiveCache writes content into cacheDir under its own sha256 hex
// name — the shape Fetch would have left behind — and returns a LockHarness
// referencing it for platform key.
func seedArchiveCache(t *testing.T, cacheDir string, key pool.PlatformKey, content []byte) *pool.LockHarness {
	t.Helper()
	digest := sha256Hex(content)
	if err := os.WriteFile(filepath.Join(cacheDir, digest), content, 0o644); err != nil {
		t.Fatalf("seeding cache object: %v", err)
	}
	return &pool.LockHarness{
		Package:        "agy",
		Version:        "1.1.10",
		RecipeRevision: "sha256:" + strings.Repeat("a", 64),
		Archive: &pool.LockArchive{
			Artifacts: map[pool.PlatformKey]pool.LockArchiveArtifact{
				key: {URL: "https://example.com/agy.tar.gz", SHA512: sha512Hex(content), SHA256: digest},
			},
		},
	}
}

func TestRealize_ArchiveUnpacksAndStripsWrappingDirectory(t *testing.T) {
	tarGZ := buildAgyTarGZ(t, "agy-1.1.10-linux-x64", map[string]string{
		"agy":     "fake ELF binary content",
		"LICENSE": "license text",
	})

	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)

	a := NewArchive("")
	fb := &fatalBackend{t: t}
	stagingGenDir := filepath.Join(t.TempDir(), "gen1")
	receipt, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}

	wrapperPath := filepath.Join(stagingGenDir, "bin", "agy")
	info, err := os.Stat(wrapperPath)
	if err != nil {
		t.Fatalf("stat bin/agy: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("bin/agy mode = %v, want 0755", info.Mode().Perm())
	}
	data, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("reading bin/agy: %v", err)
	}
	if string(data) != "fake ELF binary content" {
		t.Errorf("bin/agy content = %q, want %q", data, "fake ELF binary content")
	}

	// The wrapping directory itself must NOT survive into the generation.
	if _, err := os.Stat(filepath.Join(stagingGenDir, "agy-1.1.10-linux-x64")); err == nil {
		t.Error("the archive's wrapping top directory was not stripped")
	}
	if _, err := os.Stat(filepath.Join(stagingGenDir, "LICENSE")); err != nil {
		t.Errorf("LICENSE was not unpacked: %v", err)
	}

	if err := pool.VerifyGenerationTree(stagingGenDir, receipt); err != nil {
		t.Errorf("VerifyGenerationTree: %v", err)
	}
	if receipt.Backend.Name != "none" {
		t.Errorf("Backend.Name = %q, want %q (no backend built this generation)", receipt.Backend.Name, "none")
	}
}

func TestRealize_ArchiveHandlesFlatArchiveWithNoWrappingDirectory(t *testing.T) {
	// Two top-level entries with no shared leading directory at all —
	// stripCommonLeadingDir must leave names untouched, not error.
	tarGZ := buildAgyTarGZ(t, "", map[string]string{
		"agy":     "flat binary content",
		"LICENSE": "license text",
	})

	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)

	a := NewArchive("")
	fb := &fatalBackend{t: t}
	stagingGenDir := filepath.Join(t.TempDir(), "gen1")
	if _, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir); err != nil {
		t.Fatalf("Realize: %v", err)
	}

	wrapperPath := filepath.Join(stagingGenDir, "bin", "agy")
	data, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("reading bin/agy: %v", err)
	}
	if string(data) != "flat binary content" {
		t.Errorf("bin/agy content = %q, want %q", data, "flat binary content")
	}
}

func TestRealize_ArchiveDeterministicAcrossRuns(t *testing.T) {
	tarGZ := buildAgyTarGZ(t, "agy-1.1.10-linux-x64", map[string]string{"agy": "deterministic content"})
	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)

	a := NewArchive("")
	fb := &fatalBackend{t: t}

	r1, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, filepath.Join(t.TempDir(), "gen1"))
	if err != nil {
		t.Fatalf("Realize (1st run): %v", err)
	}
	r2, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, filepath.Join(t.TempDir(), "gen2"))
	if err != nil {
		t.Fatalf("Realize (2nd run): %v", err)
	}
	if r1.ManifestDigest != r2.ManifestDigest {
		t.Errorf("ManifestDigest differs across two Realize runs of the same lock: %s vs %s", r1.ManifestDigest, r2.ManifestDigest)
	}
}

func TestRealize_ArchiveErrorsWhenBinNameAbsent(t *testing.T) {
	tarGZ := buildAgyTarGZ(t, "agy-1.1.10-linux-x64", map[string]string{
		"not-the-binary": "content",
		"README":         "content",
	})
	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)

	a := NewArchive("")
	fb := &fatalBackend{t: t}
	_, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, filepath.Join(t.TempDir(), "gen1"))
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection when binName has no matching entry")
	}
	if !strings.Contains(err.Error(), "not-the-binary") || !strings.Contains(err.Error(), "README") {
		t.Errorf("error = %q, want it to list the archive's actual contents", err.Error())
	}
}

func TestRealize_ArchiveErrorsWhenNoArtifactRecordedForPlatform(t *testing.T) {
	tarGZ := buildAgyTarGZ(t, "agy-1.1.10-linux-x64", map[string]string{"agy": "content"})
	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)

	a := NewArchive("")
	fb := &fatalBackend{t: t}
	// Ask for arm64, but only amd64 was seeded/locked.
	_, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxARM64Glibc, cacheDir, filepath.Join(t.TempDir(), "gen1"))
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection when no artifact is recorded for the requested platform")
	}
}

// --- Realize: hostile tar entries (§17 "craft hostile tars in-test") ------

// buildRawTarGZ builds a gzip-compressed tar stream from headers/contents
// pairs, giving tests full control over entry Typeflag/Linkname/Size —
// buildAgyTarGZ (regular files only) cannot express these.
func buildRawTarGZ(t *testing.T, entries []tar.Header, contents [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, hdr := range entries {
		h := hdr
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("writing tar header for %s: %v", h.Name, err)
		}
		if i < len(contents) && len(contents[i]) > 0 {
			if _, err := tw.Write(contents[i]); err != nil {
				t.Fatalf("writing tar content for %s: %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	return buf.Bytes()
}

func realizeHostileArchive(t *testing.T, tarGZ []byte) error {
	t.Helper()
	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)

	a := NewArchive("")
	fb := &fatalBackend{t: t}
	_, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, filepath.Join(t.TempDir(), "gen1"))
	return err
}

func TestRealize_ArchiveRejectsSymlinkEntry(t *testing.T) {
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{{Name: "agy", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o644}},
		nil)
	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a symlink entry")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error = %q, want it to mention \"symlink\"", err.Error())
	}
}

func TestRealize_ArchiveRejectsHardlinkEntry(t *testing.T) {
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{{Name: "agy", Typeflag: tar.TypeLink, Linkname: "somewhere-else", Mode: 0o644}},
		nil)
	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a hardlink entry")
	}
	if !strings.Contains(err.Error(), "hardlink") {
		t.Errorf("error = %q, want it to mention \"hardlink\"", err.Error())
	}
}

func TestRealize_ArchiveRejectsPathEscapeEntry(t *testing.T) {
	content := []byte("evil")
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{{Name: "../../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}},
		[][]byte{content})
	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a path-escaping entry")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("error = %q, want it to mention the \"..\" segment", err.Error())
	}
}

func TestRealize_ArchiveRejectsAbsolutePathEntry(t *testing.T) {
	content := []byte("evil")
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{{Name: "/etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}},
		[][]byte{content})
	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of an absolute-path entry")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error = %q, want it to mention \"absolute\"", err.Error())
	}
}

func TestRealize_ArchiveRejectsOversizedEntry(t *testing.T) {
	// A header alone, declaring a size far over the 1 GiB guard, with no
	// actual content written: readArchiveEntries must reject this from the
	// header's declared Size BEFORE ever attempting to read that much
	// content (so this test does not need to actually produce a
	// gigabyte-sized fixture).
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "agy", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(maxArchiveEntrySize) + 1,
	}); err != nil {
		t.Fatalf("writing oversized tar header: %v", err)
	}
	// Deliberately do not write the (huge, declared) content and do not
	// call tw.Close() (which would try to account for the missing bytes) —
	// flush the underlying gzip writer directly so the header block alone
	// reaches the compressed stream.
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	err := realizeHostileArchive(t, buf.Bytes())
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of an oversized entry")
	}
	if !strings.Contains(err.Error(), "zip-bomb") {
		t.Errorf("error = %q, want it to mention the zip-bomb guard", err.Error())
	}
}

func TestRealize_ArchiveRejectsAmbiguousBinNameMatch(t *testing.T) {
	tarGZ := buildAgyTarGZ(t, "agy-1.1.10-linux-x64", map[string]string{
		"agy":         "top-level binary",
		"extra/agy":   "a decoy with the same basename",
		"extra/other": "filler",
	})
	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of an ambiguous binName match")
	}
	if !strings.Contains(err.Error(), "more than one entry named") {
		t.Errorf("error = %q, want it to mention the ambiguity", err.Error())
	}
}
