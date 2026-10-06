package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
)

// TestIntegration_ResolveFetchRealizeWithBackfilledIntegrity is the live
// pi-coding-agent regression test (npm-raw-lockfile-backfill work package),
// end to end through the real Source pipeline: Resolve (backfilling a
// missing integrity from the registry, patching it into the raw document),
// Fetch (downloading and verifying every artifact against the SUMMARY map),
// and Realize (materializing the RAW, now-patched Lockfile verbatim and
// running real `npm cache add` / `npm ci --offline` / `tar` on the host, no
// container runtime — hostExecBackend, same pattern as
// realize_integration_test.go). Before the fix, step 3 failed with
// ENOTCACHED: Fetch/the summary map were already correct, but Realize reads
// NPMLock.Lockfile, which still lacked the field.
//
// Skips if npm is not on PATH (Realize needs a real npm/tar).
func TestIntegration_ResolveFetchRealizeWithBackfilledIntegrity(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}

	const (
		harnessPkg     = "tiny-harness-pi"
		harnessVersion = "1.0.0"
		siblingPkg     = "@example/pi-sibling"
		siblingVersion = "2.0.0"
		binName        = "tiny-harness-pi"
		nodeVersion    = "24.18.1"
	)
	siblingKey := "node_modules/" + harnessPkg + "/node_modules/" + siblingPkg

	// --- fixtures: two real, extractable npm tarballs ---
	harnessTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": fmt.Sprintf(`{"name":%q,"version":%q,"bin":{%q:"bin/cli.js"},"dependencies":{%q:%q}}`,
			harnessPkg, harnessVersion, binName, siblingPkg, siblingVersion),
		"bin/cli.js": "#!/usr/bin/env node\nconsole.log('hello from tiny-harness-pi');\n",
	})
	siblingTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": fmt.Sprintf(`{"name":%q,"version":%q}`, siblingPkg, siblingVersion),
		"index.js":     "module.exports = 1;\n",
	})

	// --- npm registry: packument for the harness (Resolve's
	// versionPublishedAt) and for the sibling (backfillIntegrity's
	// dist.integrity lookup), PLUS the sibling's own tarball — the sibling's
	// "resolved" URL must be on this same registry host, since
	// backfillIntegrity refuses to look up anything else (see npm.go's
	// registry-host guard, TestResolve_RejectsGitDependency's regression
	// test for it). ---
	packuments := map[string]string{
		harnessPkg: fmt.Sprintf(`{
			"time": {%q: "2026-01-01T00:00:00.000Z"},
			"versions": {%q: {}}
		}`, harnessVersion, harnessVersion),
		siblingPkg: fmt.Sprintf(`{
			"time": {%q: "2026-01-01T00:00:00.000Z"},
			"versions": {%q: {"dist": {"integrity": %q}}}
		}`, siblingVersion, siblingVersion, sriFor(siblingTarball)),
	}
	const siblingTarballPath = "/@example/pi-sibling/-/pi-sibling-2.0.0.tgz"
	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == siblingTarballPath {
			w.Write(siblingTarball)
			return
		}
		body, ok := packuments[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer registrySrv.Close()

	// --- npm tarball server: the harness's own tarball only (its entry
	// already carries integrity, so it never goes through the registry-host
	// backfill guard above). ---
	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(harnessTarball)
	}))
	defer npmSrv.Close()

	// --- nodejs.org-dist-shaped server (real, extractable .tar.xz) ---
	amd64Tarball, ok := buildFakeNodeTarXZ(t, fmt.Sprintf("node-v%s-linux-x64", nodeVersion))
	if !ok {
		t.Skip("host tar cannot produce .tar.xz output")
	}
	arm64Tarball, ok := buildFakeNodeTarXZ(t, fmt.Sprintf("node-v%s-linux-arm64", nodeVersion))
	if !ok {
		t.Skip("host tar cannot produce .tar.xz output")
	}
	amd64Name := fmt.Sprintf("node-v%s-linux-x64.tar.xz", nodeVersion)
	arm64Name := fmt.Sprintf("node-v%s-linux-arm64.tar.xz", nodeVersion)
	shasums := fmt.Sprintf("%s  %s\n%s  %s\n",
		sha256Hex(amd64Tarball), amd64Name,
		sha256Hex(arm64Tarball), arm64Name)
	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "SHASUMS256.txt"):
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
	n.nodeDistBaseURL = nodeSrv.URL

	// --- the resolve-phase `npm install --package-lock-only` is faked,
	// exactly like hybridIntegrationBackend/fakeBackend elsewhere in this
	// package: it writes a canned package-lock.json shaped like the live
	// pi-coding-agent bug — a nested entry (the sibling) with a registry
	// "resolved" URL but NO "integrity". ---
	lockJSON := fmt.Sprintf(`{
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch", "version": "0.0.0", "dependencies": {%q: %q}},
    %q: {
      "version": %q,
      "resolved": %q,
      "integrity": %q,
      "dependencies": {%q: %q}
    },
    %q: {
      "version": %q,
      "resolved": %q
    }
  }
}`,
		harnessPkg, harnessVersion,
		"node_modules/"+harnessPkg, harnessVersion, npmSrv.URL+"/tiny-harness-pi-1.0.0.tgz", sriFor(harnessTarball), siblingPkg, siblingVersion,
		siblingKey, siblingVersion, registrySrv.URL+siblingTarballPath)
	fb := &fakeBackend{lockJSON: []byte(lockJSON)}

	ctx := context.Background()

	// --- Resolve: backfills the sibling's integrity AND patches it into
	// the raw Lockfile (the fix under test). ---
	lh, err := n.Resolve(ctx, fb, harnessPkg, harnessVersion, t.TempDir(), nodeVersion, "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	sibling, ok := lh.NPMLock.Packages[siblingKey]
	if !ok {
		t.Fatal("summary map missing the sibling entry")
	}
	if sibling.Integrity != sriFor(siblingTarball) {
		t.Fatalf("summary Integrity = %q, want the sibling tarball's SRI", sibling.Integrity)
	}
	var rawCheck struct {
		Packages map[string]struct {
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lh.NPMLock.Lockfile, &rawCheck); err != nil {
		t.Fatalf("parsing returned Lockfile: %v", err)
	}
	if rawCheck.Packages[siblingKey].Integrity != sriFor(siblingTarball) {
		t.Fatalf("raw Lockfile sibling integrity = %q, want the sibling tarball's SRI (the ENOTCACHED bug)", rawCheck.Packages[siblingKey].Integrity)
	}

	// --- Fetch: downloads + verifies every artifact against the summary
	// map (already correct even before the fix — this is the "Fetch works"
	// half of the bug report). ---
	cacheDir := t.TempDir()
	if err := n.Fetch(ctx, &lh, cacheDir, allTargets); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// --- Realize: real npm/tar on the host (hostExecBackend), fully
	// offline. Before the fix this step is exactly where the live bug
	// failed (ENOTCACHED) despite Fetch having already succeeded. ---
	b := hostExecBackend{}
	stagingDir := filepath.Join(t.TempDir(), "gen")
	receipt, err := n.Realize(ctx, b, "pi", binName, &lh, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingDir)
	if err != nil {
		t.Fatalf("Realize: %v (this is the pi-coding-agent ENOTCACHED bug if it fails)", err)
	}

	if err := pool.VerifyGenerationTree(stagingDir, receipt); err != nil {
		t.Errorf("VerifyGenerationTree: %v", err)
	}
	siblingInstalled := filepath.Join(stagingDir, "lib", "node_modules", harnessPkg, "node_modules", siblingPkg, "package.json")
	if _, err := os.Stat(siblingInstalled); err != nil {
		t.Errorf("sibling package was not installed nested under the harness: %v", err)
	}
	wrapperPath := filepath.Join(stagingDir, "bin", binName)
	if _, err := os.Stat(wrapperPath); err != nil {
		t.Errorf("expected the generation's own bin/%s to exist: %v", binName, err)
	}

	// --- Mutation: revert the raw-document patch (simulating a lock
	// persisted by a PRE-FIX binary — the summary map and cache are exactly
	// as a real Fetch left them, only the raw Lockfile's sibling entry
	// integrity is stripped back out — exactly the shape `sync` can be
	// asked to realize straight from an existing lock, with no Resolve in
	// between) and confirm Realize now refuses it with the actionable,
	// non-circular "re-run wraptool harness update" message — NPM.Realize's
	// own backstop (item 4, npm-raw-lockfile-backfill rework) catching it
	// BEFORE ever invoking real npm, rather than real npm dying with an
	// opaque ENOTCACHED three subprocess calls deep. This is the regression
	// guard: without the fix (and without this backstop), this exact shape
	// is what shipped and broke `wraptool harness install pi`. ---
	t.Run("reverting the raw-document patch is caught by Realize's own backstop before npm ever runs", func(t *testing.T) {
		var doc map[string]any
		if err := json.Unmarshal(lh.NPMLock.Lockfile, &doc); err != nil {
			t.Fatalf("parsing Lockfile for mutation: %v", err)
		}
		packages, ok := doc["packages"].(map[string]any)
		if !ok {
			t.Fatal("Lockfile has no packages object")
		}
		entry, ok := packages[siblingKey].(map[string]any)
		if !ok {
			t.Fatalf("Lockfile has no %q entry", siblingKey)
		}
		delete(entry, "integrity") // the exact regression: raw entry missing integrity again
		reverted, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("re-marshaling reverted Lockfile: %v", err)
		}

		mutated := lh
		mutated.NPMLock = &pool.NPMLock{
			LockfileVersion: lh.NPMLock.LockfileVersion,
			Packages:        lh.NPMLock.Packages, // unchanged: Fetch already succeeded against this
			Lockfile:        reverted,
		}

		mutatedStaging := filepath.Join(t.TempDir(), "gen-mutated")
		_, err = n.Realize(ctx, b, "pi", binName, &mutated, pool.PlatformLinuxAMD64Glibc, cacheDir, mutatedStaging)
		if err == nil {
			t.Fatal("Realize: got nil error with a reverted (integrity-stripped) raw lockfile, want the RawComplete backstop to refuse it")
		}
		if !strings.Contains(err.Error(), "re-run wraptool harness update") {
			t.Errorf("Realize error = %v, want the actionable, non-circular fix-command message", err)
		}
	})
}

// TestIntegration_SyncRejectsIncompleteRawLockfileWithActionableError is item
// (c) of the npm-raw-lockfile-backfill rework: `sync` (pool.SyncHarness)
// realizes straight from an existing lock entry — it never calls
// Source.Resolve at all (see pool.SyncHarnessLocked's own doc comment), so
// UpdateHarnessLocked's self-heal (which routes a raw-incomplete entry
// through a fresh Resolve, see decision.go's LockNPMRawIncomplete) never
// runs on this path. This exercises the REAL pool.SyncHarness against the
// REAL NPM Source end to end, confirming the actionable "re-run wraptool
// harness update" error (NPM.Realize's own backstop, item 4) surfaces
// through the real orchestrator call a human would actually run — and here
// that advice is NOT circular, since `wraptool harness update` (unlike
// `sync`) DOES call Resolve and would genuinely heal it.
//
// No npm/network/backend calls happen in this test: the lock's NPMLock has
// no Packages and no Runtime, so Source.Fetch (which SyncHarnessLocked calls
// before Realize) is a pure no-op, and Realize's backstop check fires before
// it would otherwise need a real npm/tar backend at all.
func TestIntegration_SyncRejectsIncompleteRawLockfileWithActionableError(t *testing.T) {
	incompleteLockfile := []byte(`{
  "lockfileVersion": 3,
  "packages": {
    "": {"dependencies": {"@example/sync-test-harness": "1.0.0"}},
    "node_modules/@example/sync-test-harness": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/@example/sync-test-harness/-/sync-test-harness-1.0.0.tgz",
      "integrity": "sha512-AAAA=="
    },
    "node_modules/@example/sync-test-harness/node_modules/pi-sibling": {
      "version": "2.0.0",
      "resolved": "https://registry.npmjs.org/pi-sibling/-/pi-sibling-2.0.0.tgz"
    }
  }
}`)

	root := t.TempDir()
	paths := pool.PoolPaths{
		Lock:         filepath.Join(root, "harness-lock.json"),
		AdvisoryLock: filepath.Join(root, "lock"),
		Cache:        filepath.Join(root, "cache", "sha256"),
		Staging:      filepath.Join(root, "staging"),
		Runtime:      filepath.Join(root, "runtime"),
	}

	now := pool.FormatTimestamp(time.Now().UTC())
	lockDoc := &pool.Lock{
		SchemaVersion: pool.LockSchemaVersion,
		GeneratedAt:   now,
		PolicyHash:    "sha256:" + strings.Repeat("a", 64),
		Harnesses: map[string]pool.LockHarness{
			"pi": {
				Source:         "npm",
				Package:        "@example/sync-test-harness",
				Version:        "1.0.0",
				PublishedAt:    now,
				FirstSeenAt:    now,
				RecipeRevision: "sha256:" + strings.Repeat("b", 64),
				NPMLock: &pool.NPMLock{
					LockfileVersion: 3,
					Packages:        map[string]pool.NPMLockPackage{}, // deliberately empty: Fetch is a no-op
					Lockfile:        incompleteLockfile,
				},
			},
		},
	}
	if err := lockDoc.Validate(); err != nil {
		t.Fatalf("test fixture lock fails Validate (lock LOADING must tolerate this shape): %v", err)
	}
	data, err := lockDoc.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Lock, data, 0o644); err != nil {
		t.Fatal(err)
	}

	n := NewNPM("")
	_, err = pool.SyncHarness(context.Background(), n, hostExecBackend{}, paths, "pi", "pi", pool.PlatformLinuxAMD64Glibc)
	if err == nil {
		t.Fatal("SyncHarness: got nil error with an incomplete raw lockfile, want the actionable realize-preflight error")
	}
	if !strings.Contains(err.Error(), "re-run wraptool harness update") {
		t.Errorf("SyncHarness error = %v, want the actionable, non-circular fix-command message", err)
	}
}

// Note: this file used to also carry
// TestIntegration_SyncRejectsShrinkwrappedHarnessWithActionableError, proving
// that `sync` hit NPM.Realize's shrinkwrap preflight and refused outright.
// That preflight is gone (see realize.go's seedShrinkwrapCacheIndex): a
// shrinkwrapped lock entry now realizes like any other, seeding npm's
// URL-cache index first. See TestIntegration_ShrinkwrapSeeding
// (realize_shrinkwrap_integration_test.go) for the real, end-to-end
// replacement, and TestIntegration_InstallWarnsAndContinuesForShrinkwrappedHarness
// below for the "seeding doesn't help" (unrealizable fixture) case.

// TestIntegration_InstallWarnsAndContinuesForShrinkwrappedHarness is the
// "seeding doesn't help" counterpart of TestIntegration_ShrinkwrapSeeding:
// `install` (pool.UpdateHarness, Advance=false, first resolve — no existing
// lock) drives a REAL Resolve+Fetch+Realize against a shrinkwrapped fixture
// through the REAL orchestrator (pool.RefreshHarness/pool.UpdateHarness,
// hybridIntegrationBackend faking only the resolve-phase `npm install`,
// exactly like npm_integration_test.go's own end-to-end test). Unlike
// `sync`, a realize failure on install/update is NON-FATAL
// (UpdateResult.RealizeErr, orchestrator.go's own doc comment) — this
// confirms the whole call still reports success (err == nil, "exit 0"
// equivalent) with the underlying npm error surfaced through RealizeErr, so
// a caller (cmd/harness.go) prints the warning and falls through to the
// legacy install path instead of failing the command outright.
//
// A REAL npm/tar subprocess now runs in this test (realize's preflight
// rejection is gone — see realize.go's seedShrinkwrapCacheIndex): the npm
// package "tarball" and node runtime "tarballs" served below are
// deliberately still arbitrary, non-tarball bytes (this test cares about
// install's forgiving behavior on ANY realize failure, not about a
// specific one), so `npm cache add` itself fails immediately
// (TAR_BAD_ARCHIVE) — a realize failure exactly like any other, just no
// longer a shrinkwrap-specific message. Skips if npm is not on PATH.
func TestIntegration_InstallWarnsAndContinuesForShrinkwrappedHarness(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}

	const pkg = "@earendil-works/pi-coding-agent"
	const version = "1.0.0"
	// nodeVersion is the one release this test's index.json fixture (below)
	// reports on the pool's default line — pool.RefreshNodeLine +
	// selectNodeVersion (design/harness/node-runtime-tracking.md WP1) must
	// select exactly this version and hand it to NPM.Resolve.
	const nodeVersion = "22.23.2"

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

	tarballContent := []byte("arbitrary npm tarball bytes — never extracted in this test")
	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarballContent)
	}))
	defer npmSrv.Close()

	amd64Content := []byte("arbitrary amd64 node tarball bytes — never extracted")
	arm64Content := []byte("arbitrary arm64 node tarball bytes — never extracted")
	amd64Name := fmt.Sprintf("node-v%s-linux-x64.tar.xz", nodeVersion)
	arm64Name := fmt.Sprintf("node-v%s-linux-arm64.tar.xz", nodeVersion)
	shasums := fmt.Sprintf("%s  %s\n%s  %s\n",
		sha256Hex(amd64Content), amd64Name,
		sha256Hex(arm64Content), arm64Name)
	// index.json-shaped fixture (design/harness/node-runtime-tracking.md
	// WP1): reports the one release above on the pool's default line, so
	// pool.RefreshNodeLine below has something to observe.
	nodeIndexBody, err := json.Marshal([]map[string]string{
		{"version": "v" + nodeVersion, "date": "2026-07-01"},
	})
	if err != nil {
		t.Fatalf("marshaling fixture node index: %v", err)
	}
	nodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "index.json"):
			w.Header().Set("Content-Type", "application/json")
			w.Write(nodeIndexBody)
		case strings.HasSuffix(r.URL.Path, "SHASUMS256.txt"):
			w.Write([]byte(shasums))
		case strings.HasSuffix(r.URL.Path, amd64Name):
			w.Write(amd64Content)
		case strings.HasSuffix(r.URL.Path, arm64Name):
			w.Write(arm64Content)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer nodeSrv.Close()

	n := NewNPM(registrySrv.URL)
	n.nodeDistBaseURL = nodeSrv.URL

	lockJSON := fmt.Sprintf(`{
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "resolve-scratch", "version": "0.0.0", "dependencies": {%q: %q}},
    %q: {
      "version": %q,
      "resolved": %q,
      "integrity": %q,
      "hasShrinkwrap": true
    }
  }
}`, pkg, version, "node_modules/"+pkg, version, npmSrv.URL+"/pi-coding-agent-1.0.0.tgz", sriFor(tarballContent))
	hb := hybridIntegrationBackend{lockJSON: []byte(lockJSON)}

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
	manifestYAML := "schema_version: 1\npolicy:\n  minimum_release_age: 0h\nharnesses:\n  pi: {}\n"
	if err := os.MkdirAll(filepath.Dir(paths.Manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Manifest, []byte(manifestYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	if _, err := pool.RefreshHarness(ctx, n, paths, pool.SourceNPM, pkg, now); err != nil {
		t.Fatalf("RefreshHarness: %v", err)
	}
	if _, err := pool.RefreshNodeLine(ctx, paths, pool.NodeOptions{BaseURL: nodeSrv.URL}, "", now); err != nil {
		t.Fatalf("RefreshNodeLine: %v", err)
	}

	result, err := pool.UpdateHarness(ctx, n, hb, paths, pool.UpdateOptions{
		Harness: "pi", Package: pkg, BinName: "pi", Now: now, Advance: false,
	})
	if err != nil {
		t.Fatalf("UpdateHarness: got error %v, want nil (install must warn-and-continue on a realize failure, not fail the command — exit-0 equivalent)", err)
	}
	if !result.Changed {
		t.Error("Changed = false, want true (this is a fresh resolve — the first install)")
	}
	if result.RealizeErr == nil {
		t.Fatal("RealizeErr = nil, want the underlying npm failure (arbitrary bytes are not a real tarball) — install continues to the legacy path on any realize failure")
	}
	if !strings.Contains(result.RealizeErr.Error(), "npm cache add") {
		t.Errorf("RealizeErr = %v, want it to name the failing step (npm cache add)", result.RealizeErr)
	}
	if result.RealizedGenerationID != "" {
		t.Errorf("RealizedGenerationID = %q, want empty (realize failed)", result.RealizedGenerationID)
	}

	lockBytes, err := os.ReadFile(paths.Lock)
	if err != nil {
		t.Fatalf("reading harness-lock.json: %v", err)
	}
	lockDoc, err := pool.UnmarshalLock(lockBytes)
	if err != nil {
		t.Fatalf("harness-lock.json failed to validate: %v", err)
	}
	lh, ok := lockDoc.Harnesses["pi"]
	if !ok {
		t.Fatal("no persisted lock entry for pi")
	}
	if lh.NPMLock == nil || !lh.NPMLock.HasShrinkwrap {
		t.Error("persisted lock's NPMLock.HasShrinkwrap = false, want true — the condition must be recorded even though realize failed")
	}
}
