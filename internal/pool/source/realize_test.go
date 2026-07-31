package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// realizeFixtureNow is an arbitrary fixed timestamp for this file's
// LockHarness fixtures — its exact value is immaterial to every test here.
var realizeFixtureNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// --- fixtures ----------------------------------------------------------

// testLockHarness returns a minimal, valid *pool.LockHarness for "@example/harness"
// at version 1.0.0, one dependency ("leftpad"), and glibc runtime artifacts —
// enough for reconstructNPMManifests and Realize's own pre-flight checks.
func testLockHarness() *pool.LockHarness {
	return &pool.LockHarness{
		Source:         "npm",
		Package:        "@example/harness",
		Version:        "1.0.0",
		PublishedAt:    pool.FormatTimestamp(realizeFixtureNow),
		FirstSeenAt:    pool.FormatTimestamp(realizeFixtureNow),
		RecipeRevision: "sha256:" + strings.Repeat("a", 64),
		Runtime: &pool.LockRuntime{
			Name:    "node",
			Version: "24.18.1",
			Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: "https://nodejs.org/dist/v24.18.1/node-v24.18.1-linux-x64.tar.xz", SHA256: strings.Repeat("b", 64)},
				pool.PlatformLinuxARM64Glibc: {URL: "https://nodejs.org/dist/v24.18.1/node-v24.18.1-linux-arm64.tar.xz", SHA256: strings.Repeat("c", 64)},
			},
		},
		NPMLock: &pool.NPMLock{
			LockfileVersion: 3,
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/@example/harness": {
					Version:   "1.0.0",
					Resolved:  "https://registry.npmjs.org/@example/harness/-/harness-1.0.0.tgz",
					Integrity: "sha512-AAAA==",
					SHA256:    strings.Repeat("d", 64),
				},
				"node_modules/leftpad": {
					Version:   "1.0.0",
					Resolved:  "https://registry.npmjs.org/leftpad/-/leftpad-1.0.0.tgz",
					Integrity: "sha512-BBBB==",
					SHA256:    strings.Repeat("e", 64),
				},
			},
		},
	}
}

// seedCache writes an empty (content-irrelevant to these tests) object at
// cacheDir/<digest> for every digest h references, so verifyCacheObject's
// existence check passes.
func seedCache(t *testing.T, cacheDir string, h *pool.LockHarness) {
	t.Helper()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("creating cache dir: %v", err)
	}
	write := func(digest string) {
		if err := os.WriteFile(filepath.Join(cacheDir, digest), []byte("x"), 0o644); err != nil {
			t.Fatalf("seeding cache object %s: %v", digest, err)
		}
	}
	for _, pkg := range h.NPMLock.Packages {
		write(pkg.SHA256)
	}
	for _, art := range h.Runtime.Artifacts {
		write(art.SHA256)
	}
}

// --- reconstructNPMManifests --------------------------------------------

func TestReconstructNPMManifests_Deterministic(t *testing.T) {
	h := testLockHarness()

	pkgJSON1, lock1, err := reconstructNPMManifests(h)
	if err != nil {
		t.Fatalf("reconstructNPMManifests: %v", err)
	}
	pkgJSON2, lock2, err := reconstructNPMManifests(h)
	if err != nil {
		t.Fatalf("reconstructNPMManifests (2nd call): %v", err)
	}

	if string(pkgJSON1) != string(pkgJSON2) {
		t.Errorf("package.json not deterministic:\n%s\n---\n%s", pkgJSON1, pkgJSON2)
	}
	if string(lock1) != string(lock2) {
		t.Errorf("package-lock.json not deterministic:\n%s\n---\n%s", lock1, lock2)
	}
}

func TestReconstructNPMManifests_NoSHA256Field(t *testing.T) {
	h := testLockHarness()
	_, lock, err := reconstructNPMManifests(h)
	if err != nil {
		t.Fatalf("reconstructNPMManifests: %v", err)
	}
	if strings.Contains(string(lock), "sha256") {
		t.Errorf("reconstructed package-lock.json contains a pool-internal sha256 field, want it never emitted:\n%s", lock)
	}
	if strings.Contains(string(lock), strings.Repeat("d", 64)) || strings.Contains(string(lock), strings.Repeat("e", 64)) {
		t.Errorf("reconstructed package-lock.json leaks a pool cache digest:\n%s", lock)
	}

	var parsed npmReconstructedLock
	if err := json.Unmarshal(lock, &parsed); err != nil {
		t.Fatalf("parsing reconstructed lock: %v", err)
	}
	if parsed.LockfileVersion != 3 {
		t.Errorf("lockfileVersion = %d, want 3", parsed.LockfileVersion)
	}
	root, ok := parsed.Packages[""]
	if !ok {
		t.Fatal(`packages[""] missing`)
	}
	if root.Dependencies["@example/harness"] != "1.0.0" || root.Dependencies["leftpad"] != "1.0.0" {
		t.Errorf(`root dependencies = %v, want both top-level packages present`, root.Dependencies)
	}
	harnessEntry, ok := parsed.Packages["node_modules/@example/harness"]
	if !ok {
		t.Fatal("missing node_modules/@example/harness entry")
	}
	if harnessEntry.Version != "1.0.0" || harnessEntry.Resolved == "" || harnessEntry.Integrity == "" {
		t.Errorf("harness entry incomplete: %+v", harnessEntry)
	}
	if len(harnessEntry.Dependencies) != 0 {
		t.Errorf("non-root entry has Dependencies set: %+v", harnessEntry)
	}
}

func TestReconstructNPMManifests_NestedEntryExcludedFromRootDeps(t *testing.T) {
	h := testLockHarness()
	h.NPMLock.Packages["node_modules/@example/harness/node_modules/nested"] = pool.NPMLockPackage{
		Version: "2.0.0", Resolved: "https://registry.npmjs.org/nested/-/nested-2.0.0.tgz", Integrity: "sha512-CCCC==", SHA256: strings.Repeat("f", 64),
	}
	_, lock, err := reconstructNPMManifests(h)
	if err != nil {
		t.Fatalf("reconstructNPMManifests: %v", err)
	}
	var parsed npmReconstructedLock
	if err := json.Unmarshal(lock, &parsed); err != nil {
		t.Fatalf("parsing reconstructed lock: %v", err)
	}
	root := parsed.Packages[""]
	if _, ok := root.Dependencies["nested"]; ok {
		t.Error(`root dependencies contains "nested", want nested entries excluded (no reachable edge — documented schema gap)`)
	}
	if _, ok := parsed.Packages["node_modules/@example/harness/node_modules/nested"]; !ok {
		t.Error("nested entry itself missing from packages map, want it preserved verbatim even though unreachable")
	}
}

// --- Realize: RunSpec assertions via a recording fake Backend -----------

// realizeFakeBackend is a backend.Backend test double for Realize: it
// records every RunSpec and, for the two commands Realize actually issues
// that produce on-disk output (`npm ci` and the node runtime `tar` unpack),
// fabricates the files a real backend run would have produced — following
// npm_test.go's fakeBackend pattern (record + synthesize expected side
// effects) one level up, since Realize's container steps write into the
// staging tree rather than returning data to read back.
type realizeFakeBackend struct {
	stagingGenDir string
	pkg           string
	binName       string
	pkgJSON       []byte // content for lib/node_modules/<pkg>/package.json
	scriptRel     string // "bin" path (relative to the package dir) to create

	calls  []backend.RunSpec
	runErr error
}

func (f *realizeFakeBackend) Name() string    { return "fake" }
func (f *realizeFakeBackend) Available() bool { return true }

func (f *realizeFakeBackend) Run(_ context.Context, spec backend.RunSpec) error {
	f.calls = append(f.calls, spec)
	if f.runErr != nil {
		return f.runErr
	}
	if len(spec.Argv) < 1 {
		return fmt.Errorf("realizeFakeBackend: empty Argv")
	}
	switch spec.Argv[0] {
	case "npm":
		if len(spec.Argv) >= 2 && spec.Argv[1] == "ci" {
			return f.simulateNPMCI()
		}
		return nil // npm cache add: nothing to fabricate, just recorded
	case "tar":
		return f.simulateNodeUnpack()
	default:
		return fmt.Errorf("realizeFakeBackend: unexpected argv %v", spec.Argv)
	}
}

func (f *realizeFakeBackend) simulateNPMCI() error {
	pkgDir := filepath.Join(f.stagingGenDir, "lib", "node_modules", f.pkg)
	if err := os.MkdirAll(filepath.Join(pkgDir, filepath.Dir(f.scriptRel)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), f.pkgJSON, 0o644); err != nil {
		return err
	}
	script := []byte("#!/usr/bin/env node\nconsole.log('hello');\n")
	if err := os.WriteFile(filepath.Join(pkgDir, f.scriptRel), script, 0o644); err != nil {
		return err
	}
	// node_modules/.package-lock.json residue a real `npm ci` leaves behind.
	nmDir := filepath.Join(f.stagingGenDir, "lib", "node_modules")
	return os.WriteFile(filepath.Join(nmDir, ".package-lock.json"), []byte("{}"), 0o644)
}

func (f *realizeFakeBackend) simulateNodeUnpack() error {
	binDir := filepath.Join(f.stagingGenDir, "runtime", "node", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(binDir, "node"), []byte("fake node binary"), 0o644)
}

func newRealizeFakeBackend(stagingGenDir string) *realizeFakeBackend {
	pkgJSON, _ := json.Marshal(map[string]any{
		"name": "@example/harness",
		"bin":  map[string]string{"example-harness": "bin/cli.js"},
	})
	return &realizeFakeBackend{
		stagingGenDir: stagingGenDir,
		pkg:           "@example/harness",
		binName:       "example-harness",
		pkgJSON:       pkgJSON,
		scriptRel:     filepath.Join("bin", "cli.js"),
	}
}

func TestRealize_RunSpecs(t *testing.T) {
	h := testLockHarness()
	cacheDir := t.TempDir()
	seedCache(t, cacheDir, h)
	stagingGenDir := t.TempDir()

	fb := newRealizeFakeBackend(stagingGenDir)
	n := NewNPM("")

	receipt, err := n.Realize(context.Background(), fb, "example-harness", fb.binName, h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}

	if len(fb.calls) != 3 {
		t.Fatalf("got %d backend.Run calls, want 3 (cache add, ci, tar unpack): %+v", len(fb.calls), fb.calls)
	}

	for i, spec := range fb.calls {
		t.Run(fmt.Sprintf("call %d: Network", i), func(t *testing.T) {
			if spec.Network {
				t.Errorf("Network = true, want false (§6.2: realize never touches the network)")
			}
		})
		t.Run(fmt.Sprintf("call %d: Mounts", i), func(t *testing.T) {
			if len(spec.Mounts) != 2 {
				t.Fatalf("got %d mounts, want 2", len(spec.Mounts))
			}
			cache, staging := spec.Mounts[0], spec.Mounts[1]
			if cache.Host != cacheDir {
				t.Errorf("mount[0].Host = %q, want cacheDir %q", cache.Host, cacheDir)
			}
			if !cache.ReadOnly {
				t.Error("cache mount is read-write, want read-only")
			}
			if staging.Host != stagingGenDir {
				t.Errorf("mount[1].Host = %q, want stagingGenDir %q", staging.Host, stagingGenDir)
			}
			if staging.ReadOnly {
				t.Error("staging mount is read-only, want read-write")
			}
		})
		t.Run(fmt.Sprintf("call %d: Platform", i), func(t *testing.T) {
			if spec.Platform != "amd64" {
				t.Errorf("Platform = %q, want amd64", spec.Platform)
			}
		})
		t.Run(fmt.Sprintf("call %d: User empty", i), func(t *testing.T) {
			if spec.User != "" {
				t.Errorf("User = %q, want empty (guix backend rejects a non-empty User)", spec.User)
			}
		})
	}

	t.Run("argv shapes", func(t *testing.T) {
		cacheAdd := fb.calls[0].Argv
		if cacheAdd[0] != "npm" || cacheAdd[1] != "cache" || cacheAdd[2] != "add" {
			t.Errorf("call 0 Argv = %v, want npm cache add ...", cacheAdd)
		}
		if len(cacheAdd) != 3+2+2 { // npm cache add --cache <dir> <tarball1> <tarball2>
			t.Errorf("call 0 Argv = %v, want one tarball arg per locked npm package (2)", cacheAdd)
		}
		ci := fb.calls[1].Argv
		wantCI := []string{"npm", "ci", "--offline", "--foreground-scripts", "--cache"}
		for i, w := range wantCI {
			if ci[i] != w {
				t.Errorf("call 1 Argv = %v, want prefix %v", ci, wantCI)
				break
			}
		}
		if fb.calls[1].Workdir == "" {
			t.Error("call 1 Workdir is empty, want the container lib dir")
		}
		tarArgv := fb.calls[2].Argv
		if tarArgv[0] != "tar" {
			t.Errorf("call 2 Argv = %v, want tar ...", tarArgv)
		}
	})

	t.Run("receipt", func(t *testing.T) {
		if receipt.Harness != "example-harness" {
			t.Errorf("Harness = %q", receipt.Harness)
		}
		if receipt.Version != "1.0.0" {
			t.Errorf("Version = %q", receipt.Version)
		}
		if receipt.Platform != pool.PlatformLinuxAMD64Glibc {
			t.Errorf("Platform = %q", receipt.Platform)
		}
		if receipt.Backend.Name != "fake" {
			t.Errorf("Backend.Name = %q, want fake", receipt.Backend.Name)
		}
		if err := receipt.Validate(); err != nil {
			t.Errorf("receipt.Validate: %v", err)
		}
		if err := pool.VerifyGenerationTree(stagingGenDir, receipt); err != nil {
			t.Errorf("VerifyGenerationTree: %v", err)
		}
	})

	t.Run("wrapper generated", func(t *testing.T) {
		wrapperPath := filepath.Join(stagingGenDir, "bin", fb.binName)
		info, err := os.Stat(wrapperPath)
		if err != nil {
			t.Fatalf("stat wrapper: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("wrapper mode = %v, want 0755", info.Mode().Perm())
		}
	})

	t.Run("build residue removed", func(t *testing.T) {
		for _, p := range []string{
			filepath.Join(stagingGenDir, ".build"),
			filepath.Join(stagingGenDir, "lib", "package.json"),
			filepath.Join(stagingGenDir, "lib", "package-lock.json"),
			filepath.Join(stagingGenDir, "lib", "node_modules", ".package-lock.json"),
		} {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s still exists, want removed", p)
			} else if !os.IsNotExist(err) {
				t.Errorf("stat %s: %v", p, err)
			}
		}
	})
}

func TestRealize_MissingRuntimeArtifact(t *testing.T) {
	h := testLockHarness()
	cacheDir := t.TempDir()
	seedCache(t, cacheDir, h)
	stagingGenDir := t.TempDir()
	fb := newRealizeFakeBackend(stagingGenDir)
	n := NewNPM("")

	_, err := n.Realize(context.Background(), fb, "example-harness", fb.binName, h, pool.PlatformLinuxARM64Musl, cacheDir, stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want one naming the missing platform")
	}
	if !strings.Contains(err.Error(), string(pool.PlatformLinuxARM64Musl)) {
		t.Errorf("error = %q, want it to name the platform key %q", err.Error(), pool.PlatformLinuxARM64Musl)
	}
	if len(fb.calls) != 0 {
		t.Errorf("backend.Run called %d times, want 0 (must fail before touching the backend)", len(fb.calls))
	}
}

func TestRealize_MissingNPMPackageCacheObject(t *testing.T) {
	h := testLockHarness()
	cacheDir := t.TempDir()
	seedCache(t, cacheDir, h)
	// Remove the leftpad cache object after seeding everything else.
	if err := os.Remove(filepath.Join(cacheDir, strings.Repeat("e", 64))); err != nil {
		t.Fatal(err)
	}
	stagingGenDir := t.TempDir()
	fb := newRealizeFakeBackend(stagingGenDir)
	n := NewNPM("")

	_, err := n.Realize(context.Background(), fb, "example-harness", fb.binName, h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want one naming the missing npm package cache object")
	}
	if !strings.Contains(err.Error(), "node_modules/leftpad") {
		t.Errorf("error = %q, want it to name node_modules/leftpad", err.Error())
	}
	if len(fb.calls) != 0 {
		t.Errorf("backend.Run called %d times, want 0 (must fail before touching the backend)", len(fb.calls))
	}
}

func TestRealize_BinEntryMismatch(t *testing.T) {
	h := testLockHarness()
	cacheDir := t.TempDir()
	seedCache(t, cacheDir, h)
	stagingGenDir := t.TempDir()
	fb := newRealizeFakeBackend(stagingGenDir)
	n := NewNPM("")

	_, err := n.Realize(context.Background(), fb, "example-harness", "wrong-bin-name", h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want one naming the missing bin entry")
	}
	if !strings.Contains(err.Error(), "wrong-bin-name") {
		t.Errorf("error = %q, want it to name wrong-bin-name", err.Error())
	}
}

func TestRealize_BinFieldStringMismatch(t *testing.T) {
	h := testLockHarness()
	cacheDir := t.TempDir()
	seedCache(t, cacheDir, h)
	stagingGenDir := t.TempDir()
	fb := newRealizeFakeBackend(stagingGenDir)
	// A string-valued "bin" naming a different package than the one installed.
	pkgJSON, _ := json.Marshal(map[string]any{
		"name": "@example/harness",
		"bin":  "bin/cli.js",
	})
	fb.pkgJSON = pkgJSON
	n := NewNPM("")

	_, err := n.Realize(context.Background(), fb, "example-harness", "example-harness", h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want one rejecting the string-bin/name mismatch")
	}
	if !strings.Contains(err.Error(), "@example/harness") {
		t.Errorf("error = %q, want it to name the package", err.Error())
	}
}

func TestPlatformArch(t *testing.T) {
	cases := []struct {
		key  pool.PlatformKey
		want string
	}{
		{pool.PlatformLinuxAMD64Glibc, "amd64"},
		{pool.PlatformLinuxAMD64Musl, "amd64"},
		{pool.PlatformLinuxARM64Glibc, "arm64"},
		{pool.PlatformLinuxARM64Musl, "arm64"},
	}
	for _, c := range cases {
		got, err := platformArch(c.key)
		if err != nil {
			t.Errorf("platformArch(%s): %v", c.key, err)
		}
		if got != c.want {
			t.Errorf("platformArch(%s) = %q, want %q", c.key, got, c.want)
		}
	}
}
