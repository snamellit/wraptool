package source

import (
	"context"
	"crypto/sha512"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

// countingServer serves content and counts requests.
func countingServer(t *testing.T, content []byte) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// freshNPMLock is a lock entry as a fresh resolve produces it: integrity
// recorded, sha256 cache address still empty.
func freshNPMLock(url string, content []byte) *pool.LockHarness {
	return &pool.LockHarness{NPMLock: &pool.NPMLock{Packages: map[string]pool.NPMLockPackage{
		"node_modules/pkg": {Resolved: url, Integrity: sriFor(content)},
	}}}
}

// TestFetch_IndexFindsObjectWithoutRecordedSHA256 is the interrupted-update
// case: a second fetch from a fresh resolve (no sha256 in the lock) must
// find the object the first fetch cached, not download it again.
func TestFetch_IndexFindsObjectWithoutRecordedSHA256(t *testing.T) {
	content := []byte("tarball bytes")
	srv, requests := countingServer(t, content)
	cacheDir := t.TempDir()

	if err := NewNPM("").Fetch(context.Background(), freshNPMLock(srv.URL+"/pkg.tgz", content), cacheDir); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	lh := freshNPMLock(srv.URL+"/pkg.tgz", content)
	if err := NewNPM("").Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("requests = %d, want 1 (second fetch must hit the index)", got)
	}
	if got, want := lh.NPMLock.Packages["node_modules/pkg"].SHA256, sha256Hex(content); got != want {
		t.Errorf("SHA256 = %q, want %q (filled in from the index hit)", got, want)
	}
}

// TestFetch_IndexBackfillsPreexistingObjects covers a cache filled before
// the index existed: the object is found by hashing, with no download.
func TestFetch_IndexBackfillsPreexistingObjects(t *testing.T) {
	content := []byte("cached before the index existed")
	srv, requests := countingServer(t, content)
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, sha256Hex(content)), content, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := NewNPM("").Fetch(context.Background(), freshNPMLock(srv.URL+"/pkg.tgz", content), cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Errorf("requests = %d, want 0 (pre-existing object must be backfilled into the index)", got)
	}
}

// TestFetch_IndexEntryIsReverified: an index entry pointing at an object
// whose content does not match the wanted sha512 is never trusted. The
// fetch downloads the real artifact and the entry is corrected.
func TestFetch_IndexEntryIsReverified(t *testing.T) {
	content := []byte("the real tarball")
	other := []byte("some other cached object")
	srv, requests := countingServer(t, content)
	cacheDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(cacheDir, sha256Hex(other)), other, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(content)
	entry := pool.CacheIndexEntryPath(cacheDir, sum[:])
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte(sha256Hex(other)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lh := freshNPMLock(srv.URL+"/pkg.tgz", content)
	if err := NewNPM("").Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("requests = %d, want 1 (a mismatched index entry must not be trusted)", got)
	}
	if got, want := lh.NPMLock.Packages["node_modules/pkg"].SHA256, sha256Hex(content); got != want {
		t.Errorf("SHA256 = %q, want %q", got, want)
	}
	data, err := os.ReadFile(entry)
	if err != nil || strings.TrimSpace(string(data)) != sha256Hex(content) {
		t.Errorf("index entry = %q (err %v), want it rewritten to %s", data, err, sha256Hex(content))
	}
}

func TestArchiveFetch_IndexFindsObjectWithoutRecordedSHA256(t *testing.T) {
	content := []byte("archive bytes")
	srv, requests := countingServer(t, content)
	cacheDir := t.TempDir()
	newLock := func() *pool.LockHarness {
		return &pool.LockHarness{Package: "agy", Archive: &pool.LockArchive{Artifacts: map[pool.PlatformKey]pool.LockArchiveArtifact{
			pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/agy.tar.gz", SHA512: sha512Hex(content)},
		}}}
	}

	for i := range 2 {
		if err := NewArchive("").Fetch(context.Background(), newLock(), cacheDir); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("requests = %d, want 1 (second fetch must hit the index)", got)
	}
}
