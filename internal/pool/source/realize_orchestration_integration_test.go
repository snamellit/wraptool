package source

import (
	"bytes"
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

// This file is the SEAM test between this package's real NPM.Realize
// (internal/pool/source/realize.go, impl/npm-realize) and internal/pool's
// real RealizeGeneration (internal/pool/realize.go, this work package):
// realize_integration_test.go (impl/npm-realize's own, in this same package)
// already drives NPM.Realize directly and thoroughly (dependency install,
// nested deps, os-gating, wrapper content/execution, determinism across two
// Realize calls); it does NOT exercise RealizeGeneration's own
// responsibilities at all — host-side verification, receipt.json placement,
// the atomic rename into runtime/generations/<generation-id>/, the lock
// update (in memory and on disk), the audit event, and the idempotent fast
// path. This file exists for exactly that gap, reusing
// realize_integration_test.go's own fixture-building helpers
// (buildNPMTarballGZ/buildFakeNodeTarXZ/buildRawLockfile/hostExecBackend)
// rather than duplicating them.

// startTarballServer serves content on every path of a fresh httptest
// server, closed automatically via t.Cleanup. Source.Fetch (called below,
// separately from Realize) needs a real HTTP URL to download from — unlike
// Realize itself, which never touches the network.
func startTarballServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// orchestrationTestPaths is pool.PoolPaths plus the underlying root, so this
// file's relocation step can rename the whole pool by its host path.
type orchestrationTestPaths struct {
	pool.PoolPaths
	root string
}

func newOrchestrationTestPaths(t *testing.T) orchestrationTestPaths {
	t.Helper()
	root := t.TempDir()
	return orchestrationTestPaths{
		PoolPaths: pool.PoolPaths{
			Manifest:     filepath.Join(root, "manifest.yaml"),
			Lock:         filepath.Join(root, "harness-lock.json"),
			Observations: filepath.Join(root, "observations.json"),
			Audit:        filepath.Join(root, "audit.jsonl"),
			AdvisoryLock: filepath.Join(root, "lock"),
			Cache:        filepath.Join(root, "cache", "sha256"),
			Staging:      filepath.Join(root, "staging"),
			Runtime:      filepath.Join(root, "runtime"),
		},
		root: root,
	}
}

// seedLockFile writes a minimal, Validate-clean harness-lock.json at
// lockPath with one entry (harnessName -> *h), the way UpdateHarnessLocked's
// own resolve+fetch+lock write would have already persisted it before ever
// calling RealizeGeneration in production — RealizeGeneration's own
// persistGeneration only ADDS a Generations key to an existing entry, it
// never creates one from nothing.
func seedLockFile(t *testing.T, lockPath, harnessName string, h *pool.LockHarness) {
	t.Helper()
	l := &pool.Lock{
		SchemaVersion: pool.LockSchemaVersion,
		GeneratedAt:   pool.FormatTimestamp(realizeFixtureNow),
		PolicyHash:    "sha256:" + strings.Repeat("0", 64),
		Harnesses:     map[string]pool.LockHarness{harnessName: *h},
	}
	data, err := l.Marshal()
	if err != nil {
		t.Fatalf("marshaling seeded lock: %v", err)
	}
	if err := pool.WriteFileAtomic(lockPath, data, 0o644); err != nil {
		t.Fatalf("writing seeded lock: %v", err)
	}
}

// TestIntegration_RealizeOrchestrationThroughRealNPMRealize drives the REAL
// NPM.Realize through the REAL pool.RealizeGeneration on a t.TempDir pool,
// via the same host-exec hostExecBackend realize_integration_test.go defines
// (no container runtime; requires real npm on PATH, skipped otherwise — and
// that backend itself hard-fails on Network:true or a non-empty RunSpec.Env,
// a stronger check than this file re-implementing its own).
func TestIntegration_RealizeOrchestrationThroughRealNPMRealize(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH; this integration test drives the real npm realize path")
	}

	const harnessName = "tiny-harness"
	const version = "1.0.0"
	const binName = "tiny-harness"

	// A #!/bin/sh-shebang bin target, not a node one (unlike
	// realize_integration_test.go's default fixture): buildFakeNodeTarXZ's
	// bundled "node" is a placeholder, non-functional file (that package's
	// own doc comment: "Realize only unpacks and strips the top directory,
	// it never executes node"), so a node-routed wrapper can never actually
	// run here. realize_integration_test.go's own "bin target with a
	// non-node shebang execs directly" sub-test already proves the wrapper
	// execs a #!/bin/sh target directly, with no node dependency at all —
	// reusing that shape here is what makes this file's own "actually exec
	// the relocated wrapper" position-independence check (below) possible
	// without needing a real, working node binary.
	harnessTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": `{"name":"tiny-harness","version":"1.0.0","bin":{"tiny-harness":"bin/cli.sh"}}`,
		"bin/cli.sh":   "#!/bin/sh\necho hello from tiny-harness\n",
	})

	nodeTarXZ, ok := buildFakeNodeTarXZ(t, "node-v99.0.0-linux-x64")
	if !ok {
		t.Skip("host tar cannot produce .tar.xz output")
	}
	nodeDigest := sha256Hex(nodeTarXZ)

	// closedPort (§17, matching realize_integration_test.go's own fixture
	// convention): the embedded raw lockfile's OWN "resolved" field is read
	// only by REAL npm during `npm ci --offline`, which resolves the
	// package via its cached INTEGRITY, never by actually dialing this URL
	// — an unreachable closed port proves that. This is independent of
	// NPMLockPackage.Resolved below, which Fetch (this package's own Go
	// code) DOES really download from, and which must be a live, reachable
	// URL — the two "resolved" fields serve different readers and need not
	// match.
	const closedPort = "http://127.0.0.1:1"
	rawLockfile := buildRawLockfile(t,
		map[string]string{harnessName: version},
		map[string]fixtureLockPackage{
			"node_modules/" + harnessName: {
				Version: version, Resolved: closedPort + "/tiny-harness-1.0.0.tgz", Integrity: sriFor(harnessTarball),
			},
		})

	// buildLockHarness returns a fresh LockHarness pointing at a fresh
	// tarball-server URL for the harness package (Fetch, below, makes a
	// real HTTP GET) — every call site seeds its OWN server/cache/pool so
	// determinism across independent pools is genuinely independent, not
	// accidentally sharing state.
	buildLockHarness := func(t *testing.T) *pool.LockHarness {
		t.Helper()
		harnessSrv := startTarballServer(t, harnessTarball)
		return &pool.LockHarness{
			Source: "npm", Package: harnessName, Version: version,
			PublishedAt: pool.FormatTimestamp(realizeFixtureNow), FirstSeenAt: pool.FormatTimestamp(realizeFixtureNow),
			RecipeRevision: "sha256:" + strings.Repeat("1", 64),
			Runtime: &pool.LockRuntime{
				Name: "node", Version: "99.0.0",
				Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
					pool.PlatformLinuxAMD64Glibc: {URL: startTarballServer(t, nodeTarXZ).URL + "/node.tar.xz", SHA256: nodeDigest},
				},
			},
			NPMLock: &pool.NPMLock{
				LockfileVersion: 3,
				Packages: map[string]pool.NPMLockPackage{
					"node_modules/" + harnessName: {Version: version, Resolved: harnessSrv.URL + "/pkg.tgz", Integrity: sriFor(harnessTarball)},
				},
				Lockfile: rawLockfile,
			},
		}
	}

	n := NewNPM("")
	key := pool.PlatformLinuxAMD64Glibc
	b := hostExecBackend{}

	// --- pool 1 ---
	paths := newOrchestrationTestPaths(t)
	h := buildLockHarness(t)
	// Fetch admits the synthetic tarball AND the node runtime artifact into
	// cache/sha256/ — Realize refuses to run without both already verified
	// there.
	if err := n.Fetch(context.Background(), h, paths.Cache, allTargets); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// RealizeGeneration's persistGeneration re-reads harness-lock.json and
	// requires an existing entry for harnessName (it only ADDS a Generations
	// key to an already-persisted entry — see its doc comment) — mirroring
	// what UpdateHarnessLocked's own resolve+fetch+lock write would have
	// already done before ever calling RealizeGeneration in production.
	seedLockFile(t, paths.Lock, harnessName, h)

	// --- Realize (the seam under test): builds one platform generation via
	// the REAL NPM.Realize, orchestrated by the REAL RealizeGeneration. ---
	receipt, err := pool.RealizeGeneration(context.Background(), n, b, paths.PoolPaths, harnessName, binName, h, key)
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
	if _, err := os.Stat(filepath.Join(paths.Runtime, "generations", genID, "receipt.json")); err != nil {
		t.Errorf("expected receipt.json in the published generation: %v", err)
	}

	// --- Idempotence (§10), through RealizeGeneration's own fast path (not
	// NPM.Realize's — realize_integration_test.go already covers a second,
	// direct NPM.Realize call producing an identical digest). This asserts
	// only that h.Generations[key] is unchanged after a second call; it does
	// NOT observe whether Source.Realize was actually re-invoked (this test
	// double has no call-count instrumentation) — see
	// internal/pool/orchestrator_test.go's fakeSource.realizeCalls for a test
	// double that does assert non-re-entry. ---
	beforeDigest := receipt.ManifestDigest
	if _, err := pool.RealizeGeneration(context.Background(), n, b, paths.PoolPaths, harnessName, binName, h, key); err != nil {
		t.Fatalf("second RealizeGeneration (idempotent replay): %v", err)
	}
	if h.Generations[key] != beforeDigest {
		t.Errorf("h.Generations[%s] = %q after idempotent replay, want unchanged %q", key, h.Generations[key], beforeDigest)
	}

	// --- Position independence (§6.3/§11.4): relocate the whole pool root
	// and confirm the generation survived untouched, that the wrapper's own
	// bytes never encoded the OLD absolute root path, and — since npm (and
	// so, in practice, node) is on PATH — that the relocated wrapper
	// actually executes. ---
	relocated := paths.root + "-relocated"
	if err := os.Rename(paths.root, relocated); err != nil {
		t.Fatalf("relocating the pool root: %v", err)
	}
	relocatedBin := filepath.Join(relocated, "runtime", "generations", genID, "bin", binName)
	relocatedBinData, err := os.ReadFile(relocatedBin)
	if err != nil {
		t.Fatalf("expected the generation to survive relocation at %s: %v", relocatedBin, err)
	}
	if bytes.Contains(relocatedBinData, []byte(paths.root)) {
		t.Errorf("the relocated wrapper at %s still contains the OLD absolute pool root %q — it must be $0-relative, never host-path-absolute (§6.3/§11.4)", relocatedBin, paths.root)
	}
	if out, err := exec.Command(relocatedBin).CombinedOutput(); err != nil {
		t.Errorf("executing the relocated wrapper %s failed: %v\noutput:\n%s", relocatedBin, err, out)
	} else if strings.TrimSpace(string(out)) != "hello from tiny-harness" {
		t.Errorf("relocated wrapper output = %q, want %q", strings.TrimSpace(string(out)), "hello from tiny-harness")
	}

	// --- Digest determinism (§11.5/§17): realizing the SAME lock content
	// into a SECOND, independent pool root (fresh cache, fresh
	// LockHarness value with no Generations carried over, so
	// RealizeGeneration's fast path can't fire) must produce an identical
	// ManifestDigest. ---
	paths2 := newOrchestrationTestPaths(t)
	h2 := buildLockHarness(t)
	if err := n.Fetch(context.Background(), h2, paths2.Cache, allTargets); err != nil {
		t.Fatalf("Fetch into the second pool: %v", err)
	}
	seedLockFile(t, paths2.Lock, harnessName, h2)
	receipt2, err := pool.RealizeGeneration(context.Background(), n, b, paths2.PoolPaths, harnessName, binName, h2, key)
	if err != nil {
		t.Fatalf("RealizeGeneration into the second pool: %v", err)
	}
	if receipt2.ManifestDigest != receipt.ManifestDigest {
		t.Errorf("second pool's ManifestDigest = %q, want the first pool's %q — identical lock content realized independently must hash identically (§11.5/§17)",
			receipt2.ManifestDigest, receipt.ManifestDigest)
	}
}
