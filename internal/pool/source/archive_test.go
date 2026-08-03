package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// --- shared fixtures / helpers ------------------------------------------

// unroutableManifestBaseURL is passed to NewArchive by every test in this
// file that exercises only Fetch/Realize (neither of which ever reads
// manifestBaseURL) — review round 1, item 9: such a test must never
// construct an Archive via NewArchive(""), since an empty base URL
// defaults to the REAL production Antigravity CLI endpoint. Using an
// address reserved by RFC 6761 (.localhost) on a port nothing listens on
// means any accidental manifest fetch in a test that shouldn't be making
// one fails fast and locally, never by actually reaching the network.
const unroutableManifestBaseURL = "http://invalid.localhost:1"

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

// TestResolve_ArchiveRejectsNonHTTPSArtifactURL is review round 1 item 3's
// regression test: a manifest that names a plain-http artifact URL must
// make the WHOLE Resolve call fail, naming the scheme problem, and — the
// more important half of this test — must never let that URL reach Fetch
// at all. The artifact URL here points at a REAL local httptest.Server
// (not just a string literal) specifically so this test can assert that
// server saw ZERO requests: if Resolve's https check were ever weakened to
// merely log-and-continue instead of hard-failing, a subsequent Fetch
// against the surviving (non-https) LockArchiveArtifact would eventually
// hit this server, and this assertion would catch that regression.
func TestResolve_ArchiveRejectsNonHTTPSArtifactURL(t *testing.T) {
	var artifactRequests int32
	artifactSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&artifactRequests, 1)
	}))
	defer artifactSrv.Close()

	manifestSrv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64": {Version: "1.1.10", URL: artifactSrv.URL + "/agy.tar.gz", SHA512: strings.Repeat("a", 128)},
	})
	defer manifestSrv.Close()

	a := NewArchive(manifestSrv.URL)
	fb := &fatalBackend{t: t}
	lh, err := a.Resolve(context.Background(), fb, "agy", "1.1.10", t.TempDir(), "", "sha256:recipe-rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want rejection of a non-https artifact URL")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error = %q, want it to mention https", err.Error())
	}
	if lh.Archive != nil {
		t.Errorf("Archive = %+v, want nil on the error return", lh.Archive)
	}

	// The artifact server is only ever contacted by Fetch (never by
	// Resolve itself, which only reads the manifest's URL text) — zero
	// requests here IS the "Fetch is never reached in that flow" proof.
	if got := atomic.LoadInt32(&artifactRequests); got != 0 {
		t.Errorf("artifact server received %d requests, want 0 (Fetch must never be reached when Resolve rejects the URL scheme)", got)
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

	a := NewArchive(unroutableManifestBaseURL)
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

	a := NewArchive(unroutableManifestBaseURL)
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

	a := NewArchive(unroutableManifestBaseURL)
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

	a := NewArchive(unroutableManifestBaseURL)
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

	a := NewArchive(unroutableManifestBaseURL)
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

	a := NewArchive(unroutableManifestBaseURL)
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

	a := NewArchive(unroutableManifestBaseURL)
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

	a := NewArchive(unroutableManifestBaseURL)
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

// realizeHostileArchiveIn is realizeHostileArchive's more general sibling:
// it lets a caller pick (and later inspect) the exact stagingGenDir a
// hostile Realize call targets — needed by
// TestRealize_ArchiveRejectsPathEscapeEntryMatchingBinName, which must
// assert that nothing was written OUTSIDE that specific directory.
func realizeHostileArchiveIn(t *testing.T, tarGZ []byte, stagingGenDir string) error {
	t.Helper()
	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)

	a := NewArchive(unroutableManifestBaseURL)
	fb := &fatalBackend{t: t}
	_, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	return err
}

func realizeHostileArchive(t *testing.T, tarGZ []byte) error {
	t.Helper()
	return realizeHostileArchiveIn(t, tarGZ, filepath.Join(t.TempDir(), "gen1"))
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

// pathEscapeGuardMessage is the exact substring
// validateArchiveEntryName's ".." rejection produces — asserted verbatim
// (review round 1, item 2) rather than the looser Contains(err, "..") the
// first version of this test used, which was vacuous: binName ("agy")
// never matches this entry's basename ("passwd"), so
// locateBinNameOnDisk's OWN fallback error ("no entry named %q; contents:
// %s") also happens to embed the literal string ".." (it lists the
// archive's — here, absent, since extraction never got that far — or
// would-be entry names), meaning a test that only checks Contains(err,
// "..") passes whether or not the escape guard itself ever fires.
// Asserting this exact phrase instead ties the test to the SPECIFIC guard.
const pathEscapeGuardMessage = `contains a ".." path segment`

func TestRealize_ArchiveRejectsPathEscapeEntry(t *testing.T) {
	content := []byte("evil")
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{{Name: "../../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}},
		[][]byte{content})
	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a path-escaping entry")
	}
	if !strings.Contains(err.Error(), pathEscapeGuardMessage) {
		t.Errorf("error = %q, want it to contain the specific guard message %q", err.Error(), pathEscapeGuardMessage)
	}
}

// TestRealize_ArchiveRejectsPathEscapeEntryMatchingBinName is
// TestRealize_ArchiveRejectsPathEscapeEntry's sibling for the shape that
// actually matters from a security standpoint (review round 1, item 2): the
// escaping entry's BASENAME equals binName ("agy"). If
// validateArchiveEntryName's ".." guard were ever regressed away, THIS
// entry — unlike the previous test's "passwd" — would be exactly the one
// locateBinNameOnDisk finds and hardlinkOrCopy places into bin/agy, so a
// regressed guard here doesn't just risk a differently-worded error; it
// risks the archive's escaping write actually landing outside
// stagingGenDir and the pipeline completing "successfully" around it. The
// assertion below directly checks that no file was created at the
// escaping entry's resolved (outside-stagingGenDir) location — this test
// was verified to FAIL when the guard was temporarily neutered (see the
// work package's fix-round report for the exact verification transcript).
func TestRealize_ArchiveRejectsPathEscapeEntryMatchingBinName(t *testing.T) {
	genRoot := t.TempDir()
	stagingGenDir := filepath.Join(genRoot, "gen1")

	content := []byte("evil")
	// A single ".." hop from a raw name with no leading directory: if the
	// guard didn't stop it, filepath.Join(stagingGenDir,
	// "../escaped-agy-marker/agy") cleans to
	// genRoot/escaped-agy-marker/agy — outside stagingGenDir, but still
	// inside this test's own temp tree, so the assertion below can name
	// the exact path that must never come to exist.
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{{Name: "../escaped-agy-marker/agy", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}},
		[][]byte{content})

	err := realizeHostileArchiveIn(t, tarGZ, stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a path-escaping entry whose basename matches binName")
	}
	if !strings.Contains(err.Error(), pathEscapeGuardMessage) {
		t.Errorf("error = %q, want it to contain the specific guard message %q", err.Error(), pathEscapeGuardMessage)
	}

	escapedPath := filepath.Join(genRoot, "escaped-agy-marker", "agy")
	if _, statErr := os.Stat(escapedPath); statErr == nil {
		t.Fatalf("the escaping entry was written OUTSIDE stagingGenDir at %s — the path-escape guard did not hold", escapedPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("stat %s: %v", escapedPath, statErr)
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
	// actual content written: streamExtractArchive must reject this from
	// the header's declared Size BEFORE ever attempting to read that much
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

// TestRealize_ArchiveRejectsWhenTotalDeclaredSizeExceedsBudget is review
// round 1 item 1's regression test: the per-entry cap alone does not bound
// TOTAL memory/work across several individually-small entries (the
// reviewer's proof: GNU-sparse-PAX entries let a physically tiny artifact
// declare many just-under-the-per-entry-cap logical sizes that sum far
// past any reasonable whole-artifact budget). This test shrinks BOTH
// package-level size guards for its own duration (see their doc comments)
// so the scenario is reachable with byte-scale fixture content, without
// needing gigabyte-scale or genuinely GNU-sparse-encoded archives (Go's
// tar.Writer cannot produce the latter via its public API at all).
func TestRealize_ArchiveRejectsWhenTotalDeclaredSizeExceedsBudget(t *testing.T) {
	oldEntry, oldTotal := maxArchiveEntrySize, maxArchiveTotalBytes
	maxArchiveEntrySize = 100
	maxArchiveTotalBytes = 250
	t.Cleanup(func() {
		maxArchiveEntrySize = oldEntry
		maxArchiveTotalBytes = oldTotal
	})

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name    string
		content []byte
	}{
		{"one", bytes.Repeat([]byte("a"), 90)},
		{"two", bytes.Repeat([]byte("b"), 90)},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.content))}); err != nil {
			t.Fatalf("writing header for %s: %v", e.name, err)
		}
		if _, err := tw.Write(e.content); err != nil {
			t.Fatalf("writing content for %s: %v", e.name, err)
		}
	}
	// The third entry alone brings the running total to 270 bytes, over
	// the shrunk 250-byte whole-artifact budget — but it has NO backing
	// content in this fixture at all (unlike "one"/"two" above), proving
	// the budget check rejects it purely from its header's declared Size,
	// before ever attempting to read or write its (nonexistent) content.
	if err := tw.WriteHeader(&tar.Header{Name: "three", Typeflag: tar.TypeReg, Mode: 0o644, Size: 90}); err != nil {
		t.Fatalf("writing header for three: %v", err)
	}
	// Deliberately no tw.Write/tw.Close for "three" (tar.Writer.Close
	// would error: every declared byte must actually be written first) —
	// flush the gzip writer directly, the same technique
	// TestRealize_ArchiveRejectsOversizedEntry uses.
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	err := realizeHostileArchive(t, buf.Bytes())
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection when cumulative declared size exceeds the whole-artifact budget")
	}
	if !strings.Contains(err.Error(), "whole-artifact limit") {
		t.Errorf("error = %q, want it to mention the whole-artifact limit", err.Error())
	}
	if !strings.Contains(err.Error(), `"three"`) {
		t.Errorf("error = %q, want it to name the entry that tripped the budget (%q)", err.Error(), "three")
	}
}

// TestRealize_ArchiveRejectsWhenEntryCountExceedsCap is review round 2 item
// 1's regression test: the byte-size budgets are blind to many genuinely
// ZERO-size regular entries (each costs nothing against
// maxArchiveEntrySize/maxArchiveTotalBytes) — an inode-exhaustion shape a
// separate entry-count cap is needed to catch. Shrinks maxArchiveEntryCount
// for its own duration so the scenario is reachable with a handful of
// entries rather than 65536+1 real ones.
func TestRealize_ArchiveRejectsWhenEntryCountExceedsCap(t *testing.T) {
	oldCount := maxArchiveEntryCount
	maxArchiveEntryCount = 5
	t.Cleanup(func() { maxArchiveEntryCount = oldCount })

	files := make(map[string]string, maxArchiveEntryCount+1)
	for i := 0; i <= maxArchiveEntryCount; i++ { // cap+1 entries, one over
		files[fmt.Sprintf("f%d", i)] = "" // zero-size: the free-inode attack shape
	}
	tarGZ := buildAgyTarGZ(t, "", files)

	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection when entry count exceeds the cap")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", maxArchiveEntryCount)) {
		t.Errorf("error = %q, want it to name the cap (%d)", err.Error(), maxArchiveEntryCount)
	}
}

// TestRealize_ArchiveRejectsTruncatedEntryContent is review round 2 item
// 2's regression test: a regular entry whose header declares MORE bytes
// than the tar stream actually backs it with must fail via the io.CopyN
// READ path (streamExtractRegularEntry) — as opposed to every OTHER
// hostile-entry test in this file, which reject purely from header
// metadata before any content read is attempted. Also asserts the
// partially-written destination file is removed on that failure.
func TestRealize_ArchiveRejectsTruncatedEntryContent(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "agy", Typeflag: tar.TypeReg, Mode: 0o755, Size: 100}); err != nil {
		t.Fatalf("writing header: %v", err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("x"), 10)); err != nil {
		t.Fatalf("writing partial content: %v", err)
	}
	// Deliberately no tw.Close() (archive/tar would error: not all 100
	// declared bytes were written) — flush the gzip writer directly, the
	// same technique TestRealize_ArchiveRejectsOversizedEntry and
	// TestRealize_ArchiveRejectsWhenTotalDeclaredSizeExceedsBudget use.
	// Here, unlike those two, 10 REAL bytes precede the truncation, so the
	// reader actually enters the content-copy loop before failing.
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	genRoot := t.TempDir()
	stagingGenDir := filepath.Join(genRoot, "gen1")
	err := realizeHostileArchiveIn(t, buf.Bytes(), stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of truncated entry content")
	}
	if !strings.Contains(err.Error(), "agy") {
		t.Errorf("error = %q, want it to mention the entry (\"agy\")", err.Error())
	}

	if _, statErr := os.Stat(filepath.Join(stagingGenDir, "agy")); statErr == nil {
		t.Error("partially-written destination file still exists, want it removed after the failed copy")
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("stat: %v", statErr)
	}
}

// TestRealize_ArchiveAllowsPAXGlobalHeader is review round 1 item 4's
// regression test: `git archive` prepends a pax_global_header entry
// (tar.TypeXGlobalHeader). Go's tar.Reader does NOT auto-absorb this type
// the way it does local PAX/GNU-longname headers (confirmed empirically —
// Next() returns it to the caller like any other entry), so without an
// explicit case for it a legitimate git-archive-produced tarball would be
// rejected outright as an "unsupported type".
func TestRealize_ArchiveAllowsPAXGlobalHeader(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": "irrelevant"},
	}); err != nil {
		t.Fatalf("writing pax global header: %v", err)
	}
	content := []byte("binary content")
	if err := tw.WriteHeader(&tar.Header{Name: "agy", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatalf("writing header for agy: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("writing content for agy: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}

	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, buf.Bytes())
	a := NewArchive(unroutableManifestBaseURL)
	fb := &fatalBackend{t: t}
	stagingGenDir := filepath.Join(t.TempDir(), "gen1")
	receipt, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err != nil {
		t.Fatalf("Realize: %v (a PAX global header must not cause rejection)", err)
	}
	if err := pool.VerifyGenerationTree(stagingGenDir, receipt); err != nil {
		t.Errorf("VerifyGenerationTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stagingGenDir, "pax_global_header")); err == nil {
		t.Error("pax_global_header entry was extracted as a file, want it skipped (it is metadata, not content)")
	}
}

// TestRealize_ArchiveNormalizesEntryModes is review round 1 item 5's
// regression test: an extracted entry's on-disk mode is derived ONLY from
// whether the archive's recorded mode carries any exec bit — 0o755 if so,
// else 0o644 — never the archive's raw mode verbatim. This is
// umask-independent (unlike relying on WriteFile's own mode-subject-to-
// umask behavior), defeats a hostile group/world-writable or setuid
// archive mode, and keeps the receipt's digested "executable" flag
// (boolean, not the raw mode — receipt.go's ReceiptFile) identical
// regardless of which host/archiver produced the .tar.gz.
func TestRealize_ArchiveNormalizesEntryModes(t *testing.T) {
	binContent := []byte("binary content")
	sidecarContent := []byte("plain sidecar content")
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{
			// setuid (04000) + group/other exec: exec bit present -> 0755,
			// and the setuid bit must not survive at all.
			{Name: "agy", Typeflag: tar.TypeReg, Mode: 0o4770, Size: int64(len(binContent))},
			// world-writable, no exec bit anywhere -> 0644.
			{Name: "sidecar", Typeflag: tar.TypeReg, Mode: 0o666, Size: int64(len(sidecarContent))},
		},
		[][]byte{binContent, sidecarContent})

	cacheDir := t.TempDir()
	h := seedArchiveCache(t, cacheDir, pool.PlatformLinuxAMD64Glibc, tarGZ)
	a := NewArchive(unroutableManifestBaseURL)
	fb := &fatalBackend{t: t}
	stagingGenDir := filepath.Join(t.TempDir(), "gen1")
	if _, err := a.Realize(context.Background(), fb, "agy", "agy", h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir); err != nil {
		t.Fatalf("Realize: %v", err)
	}

	sidecarInfo, err := os.Stat(filepath.Join(stagingGenDir, "sidecar"))
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	if sidecarInfo.Mode().Perm() != 0o644 {
		t.Errorf("sidecar (archive mode 0666, no exec bit) on-disk mode = %v, want 0644", sidecarInfo.Mode().Perm())
	}

	binInfo, err := os.Stat(filepath.Join(stagingGenDir, "bin", "agy"))
	if err != nil {
		t.Fatalf("stat bin/agy: %v", err)
	}
	if binInfo.Mode().Perm() != 0o755 {
		t.Errorf("bin/agy (archive mode 04770, has exec bit + setuid) on-disk mode = %v, want 0755 (setuid must be stripped)", binInfo.Mode().Perm())
	}
}

// TestRealize_ArchiveRejectsDuplicateEntryNames is review round 1 item 6's
// regression test: two tar entries that resolve to the identical
// destination path must be rejected, not silently last-wins-overwritten —
// the same "reject, don't guess" stance this file already takes for an
// ambiguous binName match.
func TestRealize_ArchiveRejectsDuplicateEntryNames(t *testing.T) {
	first := []byte("first")
	second := []byte("second, would silently win if overwriting were allowed")
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{
			{Name: "agy", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(first))},
			{Name: "agy", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(second))},
		},
		[][]byte{first, second})

	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a duplicate entry name")
	}
	if !strings.Contains(err.Error(), "duplicate entry") {
		t.Errorf("error = %q, want it to mention a duplicate entry", err.Error())
	}
}

// TestRealize_ArchiveDistinguishesDirectoryCollisionFromDuplicateEntry is
// review round 2 item 3's regression test: an entry named "foo" that
// collides with a DIRECTORY (created implicitly for a nested entry like
// "foo/bar") must get its own, more specific error — never the generic
// "duplicate entry" wording TestRealize_ArchiveRejectsDuplicateEntryNames
// above exercises for two entries with the identical name.
func TestRealize_ArchiveDistinguishesDirectoryCollisionFromDuplicateEntry(t *testing.T) {
	nested := []byte("nested content")
	colliding := []byte("collides with the directory created for foo/bar")
	tarGZ := buildRawTarGZ(t,
		[]tar.Header{
			{Name: "foo/bar", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(nested))},
			{Name: "foo", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(colliding))},
		},
		[][]byte{nested, colliding})

	err := realizeHostileArchive(t, tarGZ)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection when an entry collides with a directory created for nested entries")
	}
	if !strings.Contains(err.Error(), "collides with a directory created for nested entries") {
		t.Errorf("error = %q, want the specific directory-collision message", err.Error())
	}
	if strings.Contains(err.Error(), "duplicate entry") {
		t.Errorf("error = %q, incorrectly used the duplicate-entry wording for a directory collision", err.Error())
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
