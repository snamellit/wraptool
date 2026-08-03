package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

// TestIntegration_ArchiveFullChain drives the REAL Archive source
// (this package's Archive: Refresh -> Resolve -> Fetch -> Realize) end to
// end against local httptest fixtures — a per-platform manifest server and
// an artifact server — with a real tar.gz built in-test containing a
// genuine #!/bin/sh script. No real network request ever leaves this test
// (both the manifest and artifact URLs point at local httptest servers);
// no container runtime is involved at all (§6.2: archive resolve/realize
// are pure Go, no backend — this test's fatalBackend, archive_test.go,
// asserts that with a t.Fatal-on-any-call fake, same as the unit tests).
//
// This mirrors realize_integration_test.go's "local fixtures only, drive
// the real Source" pattern one level up: instead of a real npm/tar
// subprocess, every step here is this package's own pure-Go code path.
func TestIntegration_ArchiveFullChain(t *testing.T) {
	const version = "1.1.10"
	scriptContent := "#!/bin/sh\necho hello from fake agy\n"
	tarGZ := buildAgyTarGZ(t, "agy-"+version+"-linux-x64", map[string]string{"agy": scriptContent})

	// httptest.NewTLSServer, not NewServer: Resolve (review round 1, item 3)
	// rejects a non-https manifest-supplied artifact URL before it is ever
	// locked, so the server Fetch actually downloads from must genuinely
	// speak https for this end-to-end chain to reach Fetch/Realize at all.
	// The manifest server itself stays plain http — only the URL a manifest
	// NAMES as the artifact location is scheme-checked, not the transport
	// used to fetch the manifest document itself (see validateManifest's
	// doc comment on that split).
	artifactSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarGZ)
	}))
	defer artifactSrv.Close()

	artifactSHA512 := sha512Hex(tarGZ)
	manifestSrv := newManifestServer(t, map[string]archiveManifest{
		"linux_amd64": {Version: version, URL: artifactSrv.URL + "/agy-linux-amd64.tar.gz", SHA512: artifactSHA512},
		"linux_arm64": {Version: version, URL: artifactSrv.URL + "/agy-linux-arm64.tar.gz", SHA512: artifactSHA512},
	})
	defer manifestSrv.Close()

	a := NewArchive(manifestSrv.URL)
	// Trust the TLS test server's self-signed certificate for Fetch's
	// downloads; production code never overrides this client.
	a.downloadClient = artifactSrv.Client()
	fb := &fatalBackend{t: t}
	ctx := context.Background()

	// --- Refresh ---
	candidates, err := a.Refresh(ctx, "agy")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Version != version {
		t.Fatalf("candidates = %+v, want exactly one, version %s", candidates, version)
	}

	// --- Resolve ---
	lh, err := a.Resolve(ctx, fb, "agy", candidates[0].Version, t.TempDir(), "", "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lh.Archive == nil || len(lh.Archive.Artifacts) != 2 {
		t.Fatalf("Resolve: Archive = %+v, want 2 locked platforms", lh.Archive)
	}

	// --- Fetch ---
	cacheDir := t.TempDir()
	if err := a.Fetch(ctx, &lh, cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for key, artifact := range lh.Archive.Artifacts {
		if artifact.SHA256 == "" {
			t.Fatalf("platform %s: SHA256 not filled in after Fetch", key)
		}
		if _, err := os.Stat(filepath.Join(cacheDir, artifact.SHA256)); err != nil {
			t.Fatalf("platform %s: cache object missing after Fetch: %v", key, err)
		}
	}

	// --- Realize (linux-amd64-glibc) ---
	staging1 := filepath.Join(t.TempDir(), "gen1")
	receipt1, err := a.Realize(ctx, fb, "agy", "agy", &lh, pool.PlatformLinuxAMD64Glibc, cacheDir, staging1)
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}
	if err := pool.VerifyGenerationTree(staging1, receipt1); err != nil {
		t.Fatalf("VerifyGenerationTree: %v", err)
	}

	// The unpacked binary must actually execute.
	wrapperPath := filepath.Join(staging1, "bin", "agy")
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH; cannot execute the fixture script")
	}
	out, err := exec.Command(wrapperPath).CombinedOutput()
	if err != nil {
		t.Fatalf("executing bin/agy: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "hello from fake agy" {
		t.Errorf("bin/agy output = %q, want %q", strings.TrimSpace(string(out)), "hello from fake agy")
	}

	// --- Determinism: a second, independent Realize of the same lock ---
	staging2 := filepath.Join(t.TempDir(), "gen2")
	receipt2, err := a.Realize(ctx, fb, "agy", "agy", &lh, pool.PlatformLinuxAMD64Glibc, cacheDir, staging2)
	if err != nil {
		t.Fatalf("Realize (2nd run): %v", err)
	}
	if receipt1.ManifestDigest != receipt2.ManifestDigest {
		t.Errorf("ManifestDigest differs across two Realize runs of the same lock: %s vs %s", receipt1.ManifestDigest, receipt2.ManifestDigest)
	}

	// --- Realize for the OTHER locked platform, from the SAME lock/cache ---
	staging3 := filepath.Join(t.TempDir(), "gen3")
	receipt3, err := a.Realize(ctx, fb, "agy", "agy", &lh, pool.PlatformLinuxARM64Glibc, cacheDir, staging3)
	if err != nil {
		t.Fatalf("Realize (linux-arm64-glibc): %v", err)
	}
	if err := pool.VerifyGenerationTree(staging3, receipt3); err != nil {
		t.Fatalf("VerifyGenerationTree (linux-arm64-glibc): %v", err)
	}
}

// buildRealTarGZAssertion is a compile-time sanity check that this file's
// tarball-building helper (buildAgyTarGZ, archive_test.go) really does
// produce a gzip-compressed tar stream a standard reader can parse — a
// belt-and-braces check independent of this package's own
// streamExtractArchive, since a bug shared by both the fixture builder and
// the reader would otherwise never surface as a test failure.
func TestBuildAgyTarGZProducesAValidGzipTarStream(t *testing.T) {
	data := buildAgyTarGZ(t, "top", map[string]string{"a": "1", "b": "2"})

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	if len(names) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(names), names)
	}
}
