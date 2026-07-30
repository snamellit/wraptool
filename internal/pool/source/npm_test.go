package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
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

// --- Refresh -----------------------------------------------------------

// newPackumentServer serves a canned npm registry packument (the full
// document, not abbreviated) at every path, so the test package name is
// irrelevant to routing.
func newPackumentServer(t *testing.T, timeMap map[string]string, versions []string) *httptest.Server {
	t.Helper()
	versionsMap := make(map[string]json.RawMessage, len(versions))
	for _, v := range versions {
		versionsMap[v] = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(map[string]interface{}{
		"time":     timeMap,
		"versions": versionsMap,
	})
	if err != nil {
		t.Fatalf("marshaling fixture packument: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
}

func candidateByVersion(t *testing.T, cs []pool.Candidate, version string) pool.Candidate {
	t.Helper()
	for _, c := range cs {
		if c.Version == version {
			return c
		}
	}
	t.Fatalf("no candidate for version %q among %d candidates", version, len(cs))
	return pool.Candidate{}
}

func TestRefresh(t *testing.T) {
	srv := newPackumentServer(t, map[string]string{
		"created":      "2019-01-01T00:00:00.000Z",
		"modified":     "2024-01-01T00:00:00.000Z",
		"1.0.0":        "2020-01-01T00:00:00.000Z",
		"1.1.0":        "2020-06-01T00:00:00.000Z",
		"2.0.0-beta.1": "2021-01-01T00:00:00.000Z",
		"1.2.0":        "2022-01-01T00:00:00.000Z", // unpublished: absent from "versions" below
	}, []string{"1.0.0", "1.1.0", "2.0.0-beta.1", "1.3.0"}) // 1.3.0: no time entry
	defer srv.Close()

	n := NewNPM(srv.URL)
	candidates, err := n.Refresh(context.Background(), "example-pkg")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(candidates) != 5 {
		t.Fatalf("got %d candidates, want 5: %+v", len(candidates), candidates)
	}

	for _, c := range candidates {
		if !c.FirstSeenAt.IsZero() {
			t.Errorf("candidate %s: FirstSeenAt = %v, want zero (orchestrator fills it)", c.Version, c.FirstSeenAt)
		}
	}

	t.Run("normal version", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "1.0.0")
		want := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if !c.PublishedAt.Equal(want) {
			t.Errorf("PublishedAt = %v, want %v", c.PublishedAt, want)
		}
		if c.Prerelease {
			t.Error("Prerelease = true, want false")
		}
		if c.Yanked {
			t.Error("Yanked = true, want false")
		}
	})

	t.Run("prerelease", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "2.0.0-beta.1")
		if !c.Prerelease {
			t.Error("Prerelease = false, want true")
		}
		if c.Yanked {
			t.Error("Yanked = true, want false")
		}
	})

	t.Run("unpublished (yanked)", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "1.2.0")
		if !c.Yanked {
			t.Error("Yanked = false, want true (present in time, absent from versions)")
		}
		want := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
		if !c.PublishedAt.Equal(want) {
			t.Errorf("PublishedAt = %v, want %v (yanked candidates still carry their publish time)", c.PublishedAt, want)
		}
	})

	t.Run("missing time entry", func(t *testing.T) {
		c := candidateByVersion(t, candidates, "1.3.0")
		if !c.PublishedAt.IsZero() {
			t.Errorf("PublishedAt = %v, want zero (no time entry — must not invent one)", c.PublishedAt)
		}
		if c.Yanked {
			t.Error("Yanked = true, want false (present in versions, just missing a time entry)")
		}
	})
}

// --- Resolve -------------------------------------------------------------

// fakeBackend is a backend.Backend test double: it records every RunSpec it
// receives and, if lockJSON is set, writes it as package-lock.json into the
// first mount's host directory — standing in for what a real `npm install
// --package-lock-only` would produce.
type fakeBackend struct {
	lockJSON []byte
	runErr   error
	calls    []backend.RunSpec
}

func (f *fakeBackend) Name() string    { return "fake" }
func (f *fakeBackend) Available() bool { return true }

func (f *fakeBackend) Run(_ context.Context, spec backend.RunSpec) error {
	f.calls = append(f.calls, spec)
	if f.runErr != nil {
		return f.runErr
	}
	if f.lockJSON == nil {
		return nil
	}
	if len(spec.Mounts) == 0 {
		return fmt.Errorf("fakeBackend: RunSpec has no mounts")
	}
	return os.WriteFile(filepath.Join(spec.Mounts[0].Host, "package-lock.json"), f.lockJSON, 0o644)
}

func newNodeDistServer(t *testing.T, shasums string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHASUMS256.txt") {
			w.Write([]byte(shasums))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

const canonicalPackageLock = `{
  "name": "resolve-scratch",
  "version": "0.0.0",
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch", "version": "0.0.0"},
    "node_modules/@anthropic-ai/claude-code": {
      "version": "2.1.220",
      "resolved": "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.220.tgz",
      "integrity": "sha512-AAAA=="
    },
    "node_modules/leftpad": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/leftpad/-/leftpad-1.0.0.tgz",
      "integrity": "sha512-BBBB=="
    }
  }
}`

const shasums = "abc111  node-v24.18.1-linux-x64.tar.xz\ndef222  node-v24.18.1-linux-arm64.tar.xz\n"

func TestResolve(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{
		"2.1.220": "2026-07-29T12:00:00.000Z",
	}, []string{"2.1.220"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL

	fb := &fakeBackend{lockJSON: []byte(canonicalPackageLock)}
	workDir := t.TempDir()

	lh, err := n.Resolve(context.Background(), fb, "@anthropic-ai/claude-code", "2.1.220", workDir, "24.18.1", "sha256:recipe-rev")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	t.Run("RunSpec", func(t *testing.T) {
		if len(fb.calls) != 1 {
			t.Fatalf("got %d backend.Run calls, want 1", len(fb.calls))
		}
		spec := fb.calls[0]
		wantArgv := []string{"npm", "install", "--package-lock-only", "--ignore-scripts"}
		if len(spec.Argv) != len(wantArgv) {
			t.Fatalf("Argv = %v, want %v", spec.Argv, wantArgv)
		}
		for i := range wantArgv {
			if spec.Argv[i] != wantArgv[i] {
				t.Fatalf("Argv = %v, want %v", spec.Argv, wantArgv)
			}
		}
		if !spec.Network {
			t.Error("Network = false, want true (resolve needs registry access, §6.2)")
		}
		if len(spec.Mounts) != 1 {
			t.Fatalf("got %d mounts, want 1", len(spec.Mounts))
		}
		if spec.Mounts[0].ReadOnly {
			t.Error("scratch mount is read-only, want read-write")
		}
		if spec.Mounts[0].Host == "" || spec.Mounts[0].Container == "" {
			t.Errorf("mount has empty Host/Container: %+v", spec.Mounts[0])
		}
	})

	t.Run("LockHarness fields", func(t *testing.T) {
		if lh.Source != "npm" {
			t.Errorf("Source = %q, want npm", lh.Source)
		}
		if lh.Package != "@anthropic-ai/claude-code" {
			t.Errorf("Package = %q", lh.Package)
		}
		if lh.Version != "2.1.220" {
			t.Errorf("Version = %q", lh.Version)
		}
		wantPublished := pool.FormatTimestamp(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
		if lh.PublishedAt != wantPublished {
			t.Errorf("PublishedAt = %q, want %q", lh.PublishedAt, wantPublished)
		}
		wantZero := pool.FormatTimestamp(time.Time{})
		if lh.FirstSeenAt != wantZero {
			t.Errorf("FirstSeenAt = %q, want zero-formatted %q (orchestrator fills it)", lh.FirstSeenAt, wantZero)
		}
		if lh.RecipeRevision != "sha256:recipe-rev" {
			t.Errorf("RecipeRevision = %q", lh.RecipeRevision)
		}
	})

	t.Run("Runtime artifacts", func(t *testing.T) {
		if lh.Runtime == nil {
			t.Fatal("Runtime is nil")
		}
		if lh.Runtime.Name != "node" || lh.Runtime.Version != "24.18.1" {
			t.Errorf("Runtime = %+v", lh.Runtime)
		}
		if len(lh.Runtime.Artifacts) != 2 {
			t.Fatalf("got %d artifacts, want 2 (glibc only, musl deferred): %+v", len(lh.Runtime.Artifacts), lh.Runtime.Artifacts)
		}
		amd64, ok := lh.Runtime.Artifacts[pool.PlatformLinuxAMD64Glibc]
		if !ok {
			t.Fatal("missing linux-amd64-glibc artifact")
		}
		if amd64.SHA256 != "abc111" {
			t.Errorf("amd64 sha256 = %q, want abc111", amd64.SHA256)
		}
		if !strings.Contains(amd64.URL, "node-v24.18.1-linux-x64.tar.xz") {
			t.Errorf("amd64 URL = %q", amd64.URL)
		}
		arm64, ok := lh.Runtime.Artifacts[pool.PlatformLinuxARM64Glibc]
		if !ok {
			t.Fatal("missing linux-arm64-glibc artifact")
		}
		if arm64.SHA256 != "def222" {
			t.Errorf("arm64 sha256 = %q, want def222", arm64.SHA256)
		}
		if _, ok := lh.Runtime.Artifacts[pool.PlatformLinuxAMD64Musl]; ok {
			t.Error("linux-amd64-musl artifact present, want absent (musl deferred, §5.1)")
		}
	})

	t.Run("NPMLock conversion", func(t *testing.T) {
		if lh.NPMLock == nil {
			t.Fatal("NPMLock is nil")
		}
		if lh.NPMLock.LockfileVersion != 3 {
			t.Errorf("LockfileVersion = %d, want 3", lh.NPMLock.LockfileVersion)
		}
		if _, ok := lh.NPMLock.Packages[""]; ok {
			t.Error(`Packages contains "" (the synthesized root entry), want it excluded`)
		}
		if len(lh.NPMLock.Packages) != 2 {
			t.Fatalf("got %d packages, want 2: %+v", len(lh.NPMLock.Packages), lh.NPMLock.Packages)
		}
		claude, ok := lh.NPMLock.Packages["node_modules/@anthropic-ai/claude-code"]
		if !ok {
			t.Fatal("missing claude-code package entry")
		}
		if claude.Resolved != "https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.220.tgz" {
			t.Errorf("Resolved = %q", claude.Resolved)
		}
		if claude.Integrity != "sha512-AAAA==" {
			t.Errorf("Integrity = %q", claude.Integrity)
		}
		if claude.SHA256 != "" {
			t.Errorf("SHA256 = %q, want empty (Fetch fills it later)", claude.SHA256)
		}
	})
}

func TestResolve_RejectsUnlockableDependency(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := `{
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch"},
    "node_modules/git-dep": {"version": "1.0.0"}
  }
}`
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want one naming the unlockable entry")
	}
	if !strings.Contains(err.Error(), "node_modules/git-dep") {
		t.Errorf("error = %q, want it to name node_modules/git-dep", err.Error())
	}
}

func TestResolve_RejectsWrongLockfileVersion(t *testing.T) {
	registrySrv := newPackumentServer(t, map[string]string{"1.0.0": "2026-01-01T00:00:00.000Z"}, []string{"1.0.0"})
	defer registrySrv.Close()
	nodeSrv := newNodeDistServer(t, shasums)
	defer nodeSrv.Close()

	lockJSON := `{"lockfileVersion": 2, "packages": {}}`
	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	_, err := n.Resolve(context.Background(), fb, "example", "1.0.0", t.TempDir(), "24.18.1", "sha256:rev")
	if err == nil {
		t.Fatal("Resolve: got nil error, want one rejecting lockfileVersion 2")
	}
	if !strings.Contains(err.Error(), "lockfileVersion") {
		t.Errorf("error = %q, want it to mention lockfileVersion", err.Error())
	}
}

// --- Fetch -----------------------------------------------------------------

func sriFor(content []byte) string {
	sum := sha512.Sum512(content)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestFetch_NPMPackage(t *testing.T) {
	content := []byte("tarball bytes for the npm package")
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			LockfileVersion: 3,
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {
					Version:   "1.0.0",
					Resolved:  srv.URL + "/pkg.tgz",
					Integrity: sriFor(content),
				},
			},
		},
	}

	n := NewNPM("")
	if err := n.Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}

	wantDigest := sha256Hex(content)
	got := lh.NPMLock.Packages["node_modules/pkg"]
	if got.SHA256 != wantDigest {
		t.Errorf("Sha256 = %q, want %q (filled in from empty)", got.SHA256, wantDigest)
	}

	cached, err := os.ReadFile(filepath.Join(cacheDir, wantDigest))
	if err != nil {
		t.Fatalf("reading cached object: %v", err)
	}
	if !bytes.Equal(cached, content) {
		t.Error("cached object content does not match downloaded content")
	}
}

func TestFetch_SRIMismatchAdmitsNothing(t *testing.T) {
	content := []byte("actual bytes served")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {
					Resolved:  srv.URL + "/pkg.tgz",
					Integrity: sriFor([]byte("different bytes entirely")),
				},
			},
		},
	}

	n := NewNPM("")
	err := n.Fetch(context.Background(), lh, cacheDir)
	if err == nil {
		t.Fatal("Fetch: got nil error, want SRI mismatch")
	}
	if !strings.Contains(err.Error(), "node_modules/pkg") {
		t.Errorf("error = %q, want it to name node_modules/pkg", err.Error())
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
	// The mismatched digest must specifically not exist in the cache.
	if _, err := os.Stat(filepath.Join(cacheDir, sha256Hex(content))); err == nil {
		t.Error("mismatched object's digest exists in cache, want absent")
	}
}

func TestFetch_SHA256VerifyAndMismatch(t *testing.T) {
	content := []byte("runtime artifact bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()
	digest := sha256Hex(content)

	t.Run("matches recorded sha256", func(t *testing.T) {
		cacheDir := t.TempDir()
		lh := &pool.LockHarness{
			Runtime: &pool.LockRuntime{
				Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
					pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node.tar.xz", SHA256: digest},
				},
			},
		}
		n := NewNPM("")
		if err := n.Fetch(context.Background(), lh, cacheDir); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	})

	t.Run("mismatches recorded sha256", func(t *testing.T) {
		cacheDir := t.TempDir()
		lh := &pool.LockHarness{
			Runtime: &pool.LockRuntime{
				Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
					pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node.tar.xz", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"},
				},
			},
		}
		n := NewNPM("")
		err := n.Fetch(context.Background(), lh, cacheDir)
		if err == nil {
			t.Fatal("Fetch: got nil error, want sha256 mismatch")
		}
		if !strings.Contains(err.Error(), string(pool.PlatformLinuxAMD64Glibc)) {
			t.Errorf("error = %q, want it to name the platform key", err.Error())
		}
	})
}

func TestFetch_CacheHitSkipsDownload(t *testing.T) {
	content := []byte("already cached content")
	digest := sha256Hex(content)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Write(content)
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, digest), content, 0o644); err != nil {
		t.Fatalf("seeding cache: %v", err)
	}

	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {
					Resolved:  srv.URL + "/pkg.tgz",
					Integrity: sriFor(content),
					SHA256:    digest,
				},
			},
		},
	}

	n := NewNPM("")
	if err := n.Fetch(context.Background(), lh, cacheDir); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0 (cache hit must skip the download)", requests)
	}
}

// TestFetch_OnlyRequestsListedURLs asserts Fetch never issues a request for
// anything beyond the URLs already recorded on the LockHarness (§6.2: "A
// fetch of an unlisted URL is a bug, not a fallback").
func TestFetch_OnlyRequestsListedURLs(t *testing.T) {
	pkgContent := []byte("pkg tarball")
	runtimeContent := []byte("node runtime tarball")

	var requested []string
	mux := http.NewServeMux()
	mux.HandleFunc("/pkg.tgz", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		w.Write(pkgContent)
	})
	mux.HandleFunc("/node.tar.xz", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		w.Write(runtimeContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	lh := &pool.LockHarness{
		NPMLock: &pool.NPMLock{
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/pkg": {Resolved: srv.URL + "/pkg.tgz", Integrity: sriFor(pkgContent)},
			},
		},
		Runtime: &pool.LockRuntime{
			Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: srv.URL + "/node.tar.xz", SHA256: sha256Hex(runtimeContent)},
			},
		},
	}

	n := NewNPM("")
	if err := n.Fetch(context.Background(), lh, t.TempDir()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	sort.Strings(requested)
	want := []string{"/node.tar.xz", "/pkg.tgz"}
	if len(requested) != len(want) {
		t.Fatalf("requested paths = %v, want %v", requested, want)
	}
	for i := range want {
		if requested[i] != want[i] {
			t.Fatalf("requested paths = %v, want %v", requested, want)
		}
	}
}
