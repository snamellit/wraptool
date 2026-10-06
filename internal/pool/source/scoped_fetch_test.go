package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

// allTargets is every pool platform key, for tests that exercise Fetch
// without caring about platform scoping.
var allTargets = []pool.PlatformKey{
	pool.PlatformLinuxAMD64Glibc, pool.PlatformLinuxARM64Glibc,
	pool.PlatformLinuxAMD64Musl, pool.PlatformLinuxARM64Musl,
}

// requestRecorder serves the given path->body map (404 otherwise) and
// records every request path, like TestFetch_OnlyRequestsListedURLs.
type requestRecorder struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newRequestRecorder(t *testing.T, bodies map[string][]byte) *requestRecorder {
	t.Helper()
	r := &requestRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.mu.Unlock()
		body, ok := bodies[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *requestRecorder) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.paths...)
	sort.Strings(out)
	return out
}

func TestArchiveFetch_ScopedToTargets(t *testing.T) {
	bodies := map[string][]byte{}
	for i, key := range allTargets {
		bodies["/agy-"+string(key)+".tar.gz"] = []byte("agy bytes " + string(rune('a'+i)))
	}
	srv := newRequestRecorder(t, bodies)
	artifacts := map[pool.PlatformKey]pool.LockArchiveArtifact{}
	for _, key := range allTargets {
		path := "/agy-" + string(key) + ".tar.gz"
		artifacts[key] = pool.LockArchiveArtifact{URL: srv.URL + path, SHA512: sha512Hex(bodies[path])}
	}
	lh := &pool.LockHarness{Package: "agy", Archive: &pool.LockArchive{Artifacts: artifacts}}

	target := pool.PlatformLinuxARM64Glibc
	if err := NewArchive(unroutableManifestBaseURL).Fetch(context.Background(), lh, t.TempDir(), []pool.PlatformKey{target}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := "/agy-" + string(target) + ".tar.gz"
	if got := srv.requested(); len(got) != 1 || got[0] != want {
		t.Fatalf("requested = %v, want exactly [%s]", got, want)
	}
	for _, key := range allTargets {
		sum := lh.Archive.Artifacts[key].SHA256
		if key == target && sum == "" {
			t.Errorf("target %s: SHA256 not filled", key)
		}
		if key != target && sum != "" {
			t.Errorf("non-target %s: SHA256 = %q, want empty", key, sum)
		}
	}
}

func TestNPMFetch_RuntimeScopedToTargetsPackagesStillAllFetched(t *testing.T) {
	bodies := map[string][]byte{
		"/a.tgz":       []byte("pkg a"),
		"/b.tgz":       []byte("pkg b"),
		"/node-amd.xz": []byte("node amd64"),
		"/node-arm.xz": []byte("node arm64"),
	}
	srv := newRequestRecorder(t, bodies)
	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{Packages: map[string]pool.NPMLockPackage{
			"node_modules/a": {Resolved: srv.URL + "/a.tgz", Integrity: sriFor(bodies["/a.tgz"])},
			"node_modules/b": {Resolved: srv.URL + "/b.tgz", Integrity: sriFor(bodies["/b.tgz"])},
		}},
		Runtime: &pool.LockRuntime{Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
			pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node-amd.xz", SHA256: sha256Hex(bodies["/node-amd.xz"])},
			pool.PlatformLinuxARM64Glibc: {URL: srv.URL + "/node-arm.xz", SHA256: sha256Hex(bodies["/node-arm.xz"])},
		}},
	}
	if err := NewNPM("").Fetch(context.Background(), lh, t.TempDir(), []pool.PlatformKey{pool.PlatformLinuxAMD64Glibc}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := srv.requested()
	want := []string{"/a.tgz", "/b.tgz", "/node-amd.xz"}
	if len(got) != len(want) {
		t.Fatalf("requested = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("requested = %v, want %v", got, want)
		}
	}
}

func TestFetch_RejectsEmptyTargets(t *testing.T) {
	npmLH := &pool.LockHarness{NPMLock: &pool.NPMLock{}}
	archLH := &pool.LockHarness{Package: "agy", Archive: &pool.LockArchive{}}
	for _, targets := range [][]pool.PlatformKey{nil, {}} {
		if err := NewNPM("").Fetch(context.Background(), npmLH, t.TempDir(), targets); err == nil {
			t.Errorf("NPM.Fetch(targets=%v): want error", targets)
		}
		if err := NewArchive(unroutableManifestBaseURL).Fetch(context.Background(), archLH, t.TempDir(), targets); err == nil {
			t.Errorf("Archive.Fetch(targets=%v): want error", targets)
		}
	}
}
