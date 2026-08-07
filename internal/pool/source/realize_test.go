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

// testFixtureLockfile is a hand-built, but structurally REAL, npm
// package-lock.json (lockfileVersion 3) for "@example/harness"@1.0.0
// depending on "leftpad"@1.0.0 — the edges live on each package's OWN
// "dependencies" entry, exactly as real npm output has them (ruling 1: this
// package never hand-reconstructs a lockfile; Realize materializes exactly
// this kind of document verbatim).
const testFixtureLockfile = `{
  "lockfileVersion": 3,
  "packages": {
    "": {
      "dependencies": { "@example/harness": "1.0.0" }
    },
    "node_modules/@example/harness": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/@example/harness/-/harness-1.0.0.tgz",
      "integrity": "sha512-AAAA==",
      "dependencies": { "leftpad": "1.0.0" }
    },
    "node_modules/leftpad": {
      "version": "1.0.0",
      "resolved": "https://registry.npmjs.org/leftpad/-/leftpad-1.0.0.tgz",
      "integrity": "sha512-BBBB=="
    }
  }
}`

// testLockHarness returns a minimal, valid *pool.LockHarness for
// "@example/harness" at version 1.0.0, one dependency ("leftpad"), and
// glibc runtime artifacts — enough for npmSynthesizePackageJSON and
// Realize's own pre-flight checks.
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
			Lockfile: json.RawMessage(testFixtureLockfile),
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

// --- npmSynthesizePackageJSON (ruling 1: the only thing Realize still
// synthesizes — package-lock.json itself is written verbatim) -------------

func TestNPMSynthesizePackageJSON(t *testing.T) {
	data, err := npmSynthesizePackageJSON(json.RawMessage(testFixtureLockfile))
	if err != nil {
		t.Fatalf("npmSynthesizePackageJSON: %v", err)
	}
	var parsed npmReconstructedPackageJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if len(parsed.Dependencies) != 1 || parsed.Dependencies["@example/harness"] != "1.0.0" {
		t.Errorf("Dependencies = %v, want only the root entry's own dependency", parsed.Dependencies)
	}
	// The transitive "leftpad" edge lives on @example/harness's OWN lock
	// entry, not the root's — package.json must not promote it.
	if _, ok := parsed.Dependencies["leftpad"]; ok {
		t.Error(`Dependencies contains "leftpad", want only the root's own direct dependency`)
	}
}

func TestNPMSynthesizePackageJSON_Deterministic(t *testing.T) {
	a, err := npmSynthesizePackageJSON(json.RawMessage(testFixtureLockfile))
	if err != nil {
		t.Fatalf("npmSynthesizePackageJSON: %v", err)
	}
	b, err := npmSynthesizePackageJSON(json.RawMessage(testFixtureLockfile))
	if err != nil {
		t.Fatalf("npmSynthesizePackageJSON (2nd call): %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("not deterministic:\n%s\n---\n%s", a, b)
	}
}

func TestNPMSynthesizePackageJSON_MissingRootEntry(t *testing.T) {
	_, err := npmSynthesizePackageJSON(json.RawMessage(`{"lockfileVersion":3,"packages":{}}`))
	if err == nil {
		t.Fatal("npmSynthesizePackageJSON: got nil error, want rejection of a lockfile with no root entry")
	}
}

func TestNPMSynthesizePackageJSON_RejectsInvalidJSON(t *testing.T) {
	_, err := npmSynthesizePackageJSON(json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("npmSynthesizePackageJSON: got nil error, want rejection of invalid JSON")
	}
}

// --- Realize: RunSpec assertions via a recording fake Backend -----------

// realizeFakeBackend is a backend.Backend test double for Realize: it
// records every RunSpec and, for the two commands Realize actually issues
// that produce on-disk output (`npm ci` and the node runtime `tar` unpack),
// fabricates the files a real backend run would have produced — following
// npm_test.go's fakeBackend pattern (record + synthesize expected side
// effects) one level up, since Realize's container steps write into the
// staging tree rather than returning data to read back. Every Realize argv
// is itself `env HOME=<dir> <command>...` (finding 3: HOME is never carried
// via RunSpec.Env), so Run strips that fixed 2-element prefix before
// dispatching on the real command name.
type realizeFakeBackend struct {
	stagingGenDir string
	pkg           string
	binName       string
	pkgJSON       []byte // content for lib/node_modules/<pkg>/package.json
	scriptRel     string // "bin" path (relative to the package dir) to create

	calls               []backend.RunSpec
	runErr              error
	capturedPackageLock []byte // package-lock.json content as seen at `npm ci` time
}

func (f *realizeFakeBackend) Name() string    { return "fake" }
func (f *realizeFakeBackend) Available() bool { return true }

// stripEnvPrefix removes Realize's fixed `env HOME=<dir>` argv prefix so the
// fake can dispatch on the real command (argv[0] after the prefix).
func stripEnvPrefix(argv []string) []string {
	if len(argv) >= 2 && argv[0] == "env" && strings.HasPrefix(argv[1], "HOME=") {
		return argv[2:]
	}
	return argv
}

func (f *realizeFakeBackend) Run(_ context.Context, spec backend.RunSpec) error {
	f.calls = append(f.calls, spec)
	if f.runErr != nil {
		return f.runErr
	}
	argv := stripEnvPrefix(spec.Argv)
	if len(argv) < 1 {
		return fmt.Errorf("realizeFakeBackend: empty Argv after stripping the env prefix")
	}
	switch argv[0] {
	case "npm":
		if len(argv) >= 2 && argv[1] == "ci" {
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
	if err := os.WriteFile(filepath.Join(nmDir, ".package-lock.json"), []byte("{}"), 0o644); err != nil {
		return err
	}
	// Capture package-lock.json exactly as Realize wrote it, before
	// removeBuildResidue deletes it later in the same Realize call — this is
	// how TestRealize_MaterializesLockfileVerbatim observes it.
	data, err := os.ReadFile(filepath.Join(f.stagingGenDir, "lib", "package-lock.json"))
	if err != nil {
		return err
	}
	f.capturedPackageLock = append([]byte(nil), data...)
	return nil
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
		t.Run(fmt.Sprintf("call %d: Env is never used for HOME (finding 3)", i), func(t *testing.T) {
			if len(spec.Env) != 0 {
				t.Errorf("Env = %v, want empty — the guix backend merges Env onto the HOST client's own environment, so HOME must be an argv prefix instead", spec.Env)
			}
		})
		t.Run(fmt.Sprintf("call %d: argv carries the env HOME= prefix", i), func(t *testing.T) {
			if len(spec.Argv) < 2 || spec.Argv[0] != "env" || !strings.HasPrefix(spec.Argv[1], "HOME=") {
				t.Errorf("Argv = %v, want it to start with [\"env\", \"HOME=...\"]", spec.Argv)
			}
		})
	}

	t.Run("argv shapes", func(t *testing.T) {
		cacheAdd := fb.calls[0].Argv
		want := []string{"env", "", "npm", "cache", "add"} // want[1] (HOME=...) checked above
		for i, w := range want {
			if i == 1 {
				continue
			}
			if cacheAdd[i] != w {
				t.Errorf("call 0 Argv = %v, want prefix %v", cacheAdd, want)
				break
			}
		}
		// env(2) + npm cache add(3) + --cache <dir>(2) + 2 locked tarballs(2)
		if len(cacheAdd) != 2+3+2+2 {
			t.Errorf("call 0 Argv = %v, want one tarball arg per locked npm package (2)", cacheAdd)
		}
		ci := stripEnvPrefix(fb.calls[1].Argv)
		wantCI := []string{"npm", "ci", "--offline", "--foreground-scripts", "--cache"}
		for i, w := range wantCI {
			if ci[i] != w {
				t.Errorf("call 1 Argv (after env prefix) = %v, want prefix %v", ci, wantCI)
				break
			}
		}
		if fb.calls[1].Workdir == "" {
			t.Error("call 1 Workdir is empty, want the container lib dir")
		}
		tarArgv := stripEnvPrefix(fb.calls[2].Argv)
		if tarArgv[0] != "tar" {
			t.Errorf("call 2 Argv (after env prefix) = %v, want tar ...", tarArgv)
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

	t.Run("materialized package-lock.json is byte-identical to the stored raw bytes (ruling 1)", func(t *testing.T) {
		if len(fb.capturedPackageLock) == 0 {
			t.Fatal("fake backend never captured package-lock.json — simulateNPMCI didn't run?")
		}
		if string(fb.capturedPackageLock) != testFixtureLockfile {
			t.Errorf("materialized package-lock.json =\n%s\nwant byte-identical to the stored raw lockfile:\n%s", fb.capturedPackageLock, testFixtureLockfile)
		}
	})
}

func TestRealize_MissingLockfileErrors(t *testing.T) {
	h := testLockHarness()
	h.NPMLock.Lockfile = nil
	cacheDir := t.TempDir()
	seedCache(t, cacheDir, h)
	stagingGenDir := t.TempDir()
	fb := newRealizeFakeBackend(stagingGenDir)
	n := NewNPM("")

	_, err := n.Realize(context.Background(), fb, "example-harness", fb.binName, h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a lock with no embedded Lockfile")
	}
	if !strings.Contains(err.Error(), "wraptool harness update") {
		t.Errorf("error = %q, want it to suggest re-running wraptool harness update", err.Error())
	}
	if len(fb.calls) != 0 {
		t.Errorf("backend.Run called %d times, want 0 (must fail before touching the backend)", len(fb.calls))
	}
}

// TestRealize_RejectsShrinkwrappedHarness covers the npm-shrinkwrap backstop
// (a SEPARATE defect class from the RawComplete/missing-integrity checks): a
// lock whose NPMLock.HasShrinkwrap is set must be refused before touching
// the backend at all, naming the package and pointing at the legacy install
// path — not attempt `npm ci --offline` and let it die however npm's own
// unresolved-shrinkwrap-subtree failure happens to look.
func TestRealize_RejectsShrinkwrappedHarness(t *testing.T) {
	h := testLockHarness()
	h.NPMLock.HasShrinkwrap = true
	cacheDir := t.TempDir()
	seedCache(t, cacheDir, h)
	stagingGenDir := t.TempDir()
	fb := newRealizeFakeBackend(stagingGenDir)
	n := NewNPM("")

	_, err := n.Realize(context.Background(), fb, "example-harness", fb.binName, h, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingGenDir)
	if err == nil {
		t.Fatal("Realize: got nil error, want rejection of a shrinkwrapped harness")
	}
	if !strings.Contains(err.Error(), h.Package) {
		t.Errorf("error = %q, want it to name the package %q", err.Error(), h.Package)
	}
	if !strings.Contains(err.Error(), "npm-shrinkwrap.json") {
		t.Errorf("error = %q, want it to mention npm-shrinkwrap.json", err.Error())
	}
	if !strings.Contains(err.Error(), "legacy install path") {
		t.Errorf("error = %q, want it to mention the legacy install path", err.Error())
	}
	if len(fb.calls) != 0 {
		t.Errorf("backend.Run called %d times, want 0 (must fail before touching the backend)", len(fb.calls))
	}
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
