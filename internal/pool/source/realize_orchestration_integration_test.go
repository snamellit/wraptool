package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"github.com/pti/wraptool/internal/pool/backend"
)

// THIS FILE IS EXPECTED NOT TO COMPILE ON impl/pool-realize-orchestration.
//
// It is the seam test for that work package's PINNED Source.Realize
// signature (design/harness/unified-harness-pool.md §11.3):
//
//	Realize(ctx context.Context, b backend.Backend, harnessName, binName string,
//	        h *pool.LockHarness, key pool.PlatformKey, cacheDir, stagingGenDir string) (*pool.Receipt, error)
//
// impl/npm-realize — a sibling, parallel work package — implements this on
// *NPM. Until that branch merges onto this one, *NPM has no Realize method,
// so this file fails to compile with exactly that error. It is committed
// last, on its own, clearly labeled commit for that reason: the architect
// runs it after both branches are integrated. Every other file in this
// package (and the rest of the repository) builds and tests green without
// this commit.

// hostExecBackend is a backend.Backend that runs RunSpec.Argv directly on
// the HOST, with no container — acceptable ONLY in this integration test,
// which exists to prove NPM.Realize's on-disk contract end-to-end against a
// real npm binary, not to test container isolation (that is backend's own
// test suite, and production `wraptool harness sync`/`update` always select
// a real guix/oci Backend, never this one). It ignores
// RunSpec.Network/Platform/User (there is no container to isolate) and
// treats RunSpec.Mounts as already-host paths (there is no remapping to
// do), merging Env onto the host's own environment the same way
// backend.execRunner does.
type hostExecBackend struct{}

func (hostExecBackend) Name() string    { return "host-exec (integration test only)" }
func (hostExecBackend) Available() bool { return true }

func (hostExecBackend) Run(ctx context.Context, spec backend.RunSpec) error {
	if len(spec.Argv) == 0 {
		return fmt.Errorf("hostExecBackend: RunSpec.Argv must not be empty")
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	if spec.Workdir != "" {
		cmd.Dir = spec.Workdir
	}
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// TestIntegration_RealizeGenerationThroughRealNPMRealize drives the REAL
// NPM.Realize through the REAL pool.RealizeGeneration on a t.TempDir pool: a
// synthetic npm-package tarball (a single CLI script npm would normally
// symlink into bin/, with a #!/usr/bin/env node shebang) is fetched into a
// real cache/sha256/ via the already-implemented NPM.Fetch, then realized
// with a host-exec Backend (no container runtime; requires a real `npm` on
// PATH, skipped otherwise — this test never touches the public registry).
func TestIntegration_RealizeGenerationThroughRealNPMRealize(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH; this integration test drives the real npm realize path")
	}

	const harnessName = "integration-harness"
	const pkgName = "@example/integration-harness"
	const version = "0.0.1"
	const binName = "integration-harness"

	tarballContent := buildNPMPackageTarball(t, pkgName, version, binName)

	packument := map[string]any{
		"name":     pkgName,
		"time":     map[string]string{"modified": "2026-01-01T00:00:00.000Z", version: "2026-01-01T00:00:00.000Z"},
		"versions": map[string]json.RawMessage{version: json.RawMessage(`{}`)},
	}
	packumentBody, err := json.Marshal(packument)
	if err != nil {
		t.Fatal(err)
	}
	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(packumentBody)
	}))
	defer registrySrv.Close()

	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarballContent)
	}))
	defer npmSrv.Close()

	n := NewNPM(registrySrv.URL)

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

	seedTime := pool.FormatTimestamp(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	h := pool.LockHarness{
		Source: "npm", Package: pkgName, Version: version,
		PublishedAt: seedTime, FirstSeenAt: seedTime,
		RecipeRevision: "sha256:" + strings.Repeat("1", 64),
		NPMLock: &pool.NPMLock{
			LockfileVersion: 3,
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/" + pkgName: {
					Version: version, Resolved: npmSrv.URL + "/pkg.tgz", Integrity: sriFor(tarballContent),
				},
			},
		},
	}

	// --- Fetch (already implemented, real, host, pure Go): admits the
	// synthetic tarball into cache/sha256/. ---
	if err := n.Fetch(context.Background(), &h, paths.Cache); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	key, err := pool.HostPlatformKey()
	if err != nil {
		t.Skipf("HostPlatformKey: %v", err)
	}

	// --- Realize (the seam under test): builds one platform generation
	// with no network access, using the fetched cache object only. ---
	receipt, err := pool.RealizeGeneration(context.Background(), n, hostExecBackend{}, paths, harnessName, binName, &h, key)
	if err != nil {
		t.Fatalf("RealizeGeneration: %v", err)
	}
	if len(receipt.Files) == 0 {
		t.Fatal("receipt has no files")
	}

	genID, err := pool.GenerationID(harnessName, version, key, receipt.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(paths.Runtime, "generations", genID, "bin", binName)
	info, err := os.Lstat(binPath)
	if err != nil {
		t.Fatalf("expected the generation's own bin/%s to exist: %v", binName, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("bin/%s must not be a symlink — realization must de-symlink npm's own bin/ layout (§6.4/§11.4 step 5)", binName)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/%s must be executable", binName)
	}

	// --- Idempotence (§10): realizing again from the SAME (now
	// Generations-populated) h must succeed without a second build. ---
	if _, err := pool.RealizeGeneration(context.Background(), n, hostExecBackend{}, paths, harnessName, binName, &h, key); err != nil {
		t.Fatalf("second RealizeGeneration (idempotent replay): %v", err)
	}

	// --- Position independence (§6.3/§11.4): relocate the whole pool root
	// and confirm the generation directory (and its receipt) survived the
	// move untouched — the dispatcher/wrapper scripts inside it must never
	// have encoded root's original path. ---
	relocated := root + "-relocated"
	if err := os.Rename(root, relocated); err != nil {
		t.Fatalf("relocating the pool root: %v", err)
	}
	relocatedBin := filepath.Join(relocated, "runtime", "generations", genID, "bin", binName)
	if _, err := os.Lstat(relocatedBin); err != nil {
		t.Errorf("expected the generation to survive relocation at %s: %v", relocatedBin, err)
	}
}

// buildNPMPackageTarball builds an in-memory gzipped tar archive shaped like
// a real npm package tarball (package/package.json declaring a "bin" entry,
// package/bin/<binName> with a #!/usr/bin/env node shebang) — exactly the
// npm-produced shape §11.4 step 5's de-symlink/de-shebang pass must handle:
// `npm install`/`npm ci` unpacking this tarball creates a
// node_modules/.bin/<binName> symlink pointing at a #!/usr/bin/env node
// shim, which realization must replace with a generated, $0-relative
// wrapper.
func buildNPMPackageTarball(t *testing.T, pkgName, version, binName string) []byte {
	t.Helper()

	packageJSON := fmt.Sprintf(`{"name":%q,"version":%q,"bin":{%q:"bin/%s.js"}}`, pkgName, version, binName, binName)
	script := "#!/usr/bin/env node\nconsole.log('integration-harness ok');\n"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	writeEntry := func(name string, mode int64, content string) {
		hdr := &tar.Header{
			Name: "package/" + name,
			Mode: mode,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("writing tar content for %s: %v", name, err)
		}
	}
	writeEntry("package.json", 0o644, packageJSON)
	writeEntry(fmt.Sprintf("bin/%s.js", binName), 0o755, script)

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	return buf.Bytes()
}
