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
	if err := n.Fetch(ctx, &lh, cacheDir); err != nil {
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

	// --- Mutation: revert the raw-document patch (simulating the
	// pre-fix state — the summary map and cache are exactly as a real Fetch
	// left them, only the raw Lockfile's sibling entry integrity is
	// stripped back out) and confirm Realize now fails. This is the
	// regression guard: without the fix, this is exactly what shipped and
	// broke `wraptool harness install pi`. ---
	t.Run("reverting the raw-document patch reproduces the ENOTCACHED-class failure", func(t *testing.T) {
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
			t.Fatal("Realize: got nil error with a reverted (integrity-stripped) raw lockfile, want the ENOTCACHED-class failure the fix prevents")
		}
		if !strings.Contains(err.Error(), "ENOTCACHED") {
			t.Errorf("Realize error = %v, want it to reproduce the live bug's real npm ENOTCACHED failure", err)
		}
	})
}
