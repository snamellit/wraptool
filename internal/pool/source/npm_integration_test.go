package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// npmWithStubRealize is a temporary compile-time shim: impl/pool-realize-orchestration
// extended pool.Source with a Realize method (design/harness/unified-harness-pool.md
// §11.3/§11.4), but *NPM's own Realize is impl/npm-realize's — a sibling,
// parallel work package not yet merged onto this branch. It wraps a real
// *NPM so this pre-existing test keeps satisfying pool.Source, and builds a
// small but real generation tree so the realize step this work package wires
// into every UpdateHarness call has something genuine to verify (the same
// "fake but real receipt helpers" pattern internal/pool's own unit tests
// use) rather than merely stubbing success. Delete this wrapper (using *NPM
// directly again) once impl/npm-realize lands *NPM.Realize for real.
type npmWithStubRealize struct{ *NPM }

func (npmWithStubRealize) Realize(_ context.Context, _ backend.Backend, harnessName, binName string, h *pool.LockHarness, key pool.PlatformKey, _, stagingGenDir string) (*pool.Receipt, error) {
	name := binName
	if name == "" {
		name = harnessName
	}
	binDir := filepath.Join(stagingGenDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexec true\n"), 0o755); err != nil {
		return nil, err
	}
	files, err := pool.ScanGenerationFiles(stagingGenDir)
	if err != nil {
		return nil, err
	}
	digest, err := pool.ManifestDigest(files)
	if err != nil {
		return nil, err
	}
	lockHash, err := h.Hash()
	if err != nil {
		return nil, err
	}
	return &pool.Receipt{
		SchemaVersion:  pool.ReceiptSchemaVersion,
		Harness:        harnessName,
		Version:        h.Version,
		Platform:       key,
		RecipeRevision: h.RecipeRevision,
		LockEntryHash:  lockHash,
		Backend:        pool.ReceiptBackend{Name: "fake"},
		Files:          files,
		ManifestDigest: digest,
	}, nil
}

// TestIntegration_RealNPMSourceThroughRealOrchestrator drives the REAL npm
// Source (this package's NPM) against local httptest fixtures — a packument
// server, an nodejs.org-dist-shaped server, and an npm-tarball server —
// through the REAL command-level orchestrator (pool.RefreshHarness,
// pool.UpdateHarness), on a t.TempDir pool. No real network request and no
// container runtime is involved (the resolve phase's backend.Backend is
// this package's own fakeBackend, reused from npm_test.go).
//
// This is the regression test that would have caught both harness-cli
// review MAJORs, neither of which produced a build or lint failure and
// both of which are silent at the unit level (a fake Source/Backend never
// exercises the real HTTP shapes):
//
//   - MAJOR-1: cmd/harness.go's PoolPaths.Cache must be
//     pool.CacheSHA256Dir() (cache/sha256/), not pool.CacheDir() (cache/) —
//     passing the wrong one doesn't error, it just makes every object a
//     silent, permanent cache miss. Asserted below by checking every
//     downloaded object lands at the exact cache/sha256/<hex> path Fetch's
//     contract promises, and that nothing is written directly under cache/.
//   - MAJOR-2: internal/pool's defaultNodeVersion must be an exact,
//     published Node version ("22.11.0"), not a bare major ("22") —
//     nodejs.org/dist has no v22/ directory, so a bare major 404s on every
//     real resolve. Asserted below by capturing the exact SHASUMS256.txt
//     path the orchestrator's own defaultNodeVersion constant drives NPM.Resolve
//     to request (this test never supplies a Node version itself).
func TestIntegration_RealNPMSourceThroughRealOrchestrator(t *testing.T) {
	const pkg = "@example/test-harness"
	const version = "1.2.3"
	// defaultNodeVersion (internal/pool/orchestrator.go) is unexported and
	// not passed in by this test — it must match what the orchestrator
	// itself is pinned to (§8's MAJOR-2 fix) for the SHASUMS assertion
	// below to mean anything.
	const wantNodeVersion = "22.11.0"

	// --- npm registry packument server ---
	packument := map[string]any{
		"time": map[string]string{
			"created":  "2020-01-01T00:00:00.000Z",
			"modified": "2026-07-01T00:00:00.000Z",
			version:    "2026-01-01T00:00:00.000Z",
		},
		"versions": map[string]json.RawMessage{version: json.RawMessage(`{}`)},
	}
	packumentBody, err := json.Marshal(packument)
	if err != nil {
		t.Fatalf("marshaling fixture packument: %v", err)
	}
	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(packumentBody)
	}))
	defer registrySrv.Close()

	// --- npm tarball server ---
	tarballContent := []byte("fake npm tarball bytes for the integration test")
	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarballContent)
	}))
	defer npmSrv.Close()

	// --- nodejs.org-dist-shaped server: SHASUMS256.txt + the two glibc
	// tarballs it references, plus a recorder for every SHASUMS256.txt path
	// requested (this is where MAJOR-2's exact-version assertion comes from).
	amd64Tarball := []byte("fake node linux-x64 tarball bytes")
	arm64Tarball := []byte("fake node linux-arm64 tarball bytes")
	amd64Name := fmt.Sprintf("node-v%s-linux-x64.tar.xz", wantNodeVersion)
	arm64Name := fmt.Sprintf("node-v%s-linux-arm64.tar.xz", wantNodeVersion)
	shasums := fmt.Sprintf("%s  %s\n%s  %s\n",
		sha256Hex(amd64Tarball), amd64Name,
		sha256Hex(arm64Tarball), arm64Name)

	var shasumsRequests []string
	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "SHASUMS256.txt"):
			shasumsRequests = append(shasumsRequests, r.URL.Path)
			w.Write([]byte(shasums))
		case strings.HasSuffix(r.URL.Path, amd64Name):
			w.Write(amd64Tarball)
		case strings.HasSuffix(r.URL.Path, arm64Name):
			w.Write(arm64Tarball)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer nodeSrv.Close()

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL // in-package field set — see package doc
	src := npmWithStubRealize{n}    // see its doc comment

	// The resolve-phase Backend is faked (no container runtime): it writes
	// a canned package-lock.json into the scratch mount, exactly like
	// TestResolve's fakeBackend above.
	lockJSON := fmt.Sprintf(`{
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch", "version": "0.0.0"},
    %q: {
      "version": %q,
      "resolved": %q,
      "integrity": %q
    }
  }
}`, "node_modules/"+pkg, version, npmSrv.URL+"/test-harness-1.2.3.tgz", sriFor(tarballContent))
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	// --- t.TempDir pool, with Cache wired to the flat, content-addressed
	// cache/sha256 shape Source.Fetch's contract requires (the MAJOR-1
	// fix: production wiring is cmd/harness.go's harnessPoolPaths()). ---
	root := t.TempDir()
	paths := pool.PoolPaths{
		Manifest:     filepath.Join(root, "manifest.yaml"),
		Lock:         filepath.Join(root, "harness-lock.json"),
		Observations: filepath.Join(root, "observations.json"),
		Audit:        filepath.Join(root, "audit.jsonl"),
		AdvisoryLock: filepath.Join(root, "lock"),
		Cache:        filepath.Join(root, "cache", "sha256"),
		Staging:      filepath.Join(root, "staging"),
		Runtime:      filepath.Join(root, "runtime"),
	}
	manifestYAML := "schema_version: 1\npolicy:\n  minimum_release_age: 0h\nharnesses:\n  claude: {}\n"
	if err := os.MkdirAll(filepath.Dir(paths.Manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Manifest, []byte(manifestYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	// --- Refresh (real Source.Refresh, one registry GET) ---
	if _, err := pool.RefreshHarness(ctx, src, paths, pkg, now); err != nil {
		t.Fatalf("RefreshHarness: %v", err)
	}

	// --- Update (real Source.Resolve via the fake Backend, real
	// Source.Fetch against the npm + node-dist httptest servers; realize via
	// npmWithStubRealize's small-but-real generation, pending impl/npm-realize) ---
	result, err := pool.UpdateHarness(ctx, src, fb, paths, pool.UpdateOptions{
		Harness: "claude", Package: pkg, BinName: "test-harness", Now: now, Advance: false,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: %v", err)
	}
	if !result.Changed || result.Harness.Version != version {
		t.Fatalf("UpdateResult = %+v, want Changed=true Version=%s", result, version)
	}

	// --- Assertion 1: the lock file exists and validates. ---
	lockBytes, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("reading harness-lock.json: %v", err)
	}
	if _, err := pool.UnmarshalLock(lockBytes); err != nil {
		t.Fatalf("harness-lock.json failed to validate: %v", err)
	}

	// --- Assertion 2 (MAJOR-1): every cache object landed under
	// <pool>/cache/sha256/, not <pool>/cache/. ---
	wantDigests := []string{sha256Hex(tarballContent), sha256Hex(amd64Tarball), sha256Hex(arm64Tarball)}
	for _, digest := range wantDigests {
		p := filepath.Join(paths.Cache, digest)
		if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
			t.Errorf("expected cache object at %s (cache/sha256/<hex>), stat error: %v", p, err)
		}
	}
	cacheParent := filepath.Dir(paths.Cache) // <root>/cache
	entries, err := os.ReadDir(cacheParent)
	if err != nil {
		t.Fatalf("reading cache parent dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "sha256" {
			t.Errorf("cache/ contains %q outside the sha256/ subdirectory — objects must land only under cache/sha256/", e.Name())
		}
	}

	// --- Assertion 3 (MAJOR-2): the SHASUMS256.txt path requested matches
	// v22.11.0 exactly — proving the orchestrator's own defaultNodeVersion,
	// which this test never supplies, is a real, resolvable version. ---
	wantPath := "/v" + wantNodeVersion + "/SHASUMS256.txt"
	if len(shasumsRequests) != 1 || shasumsRequests[0] != wantPath {
		t.Errorf("SHASUMS256.txt requests = %v, want exactly [%s]", shasumsRequests, wantPath)
	}
}
