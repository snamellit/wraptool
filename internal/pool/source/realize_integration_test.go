package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/pool/backend"
)

// hostExecBackend is a backend.Backend that runs spec.Argv directly on the
// host — no container runtime — translating every container-side path
// (Argv elements, Workdir, Env values) back to the corresponding host path
// via spec.Mounts before exec.CommandContext. It exists only so this test
// can drive the REAL Realize (real npm, real tar) without guix/docker,
// following npm_integration_test.go's local-fixtures-only pattern one level
// up (that file fakes the backend too; this one actually executes, since
// Realize's whole point is what happens INSIDE the container).
type hostExecBackend struct{}

func (hostExecBackend) Name() string    { return "host-exec-fake" }
func (hostExecBackend) Available() bool { return true }

func (hostExecBackend) Run(ctx context.Context, spec backend.RunSpec) error {
	if spec.Network {
		return fmt.Errorf("hostExecBackend: Network=true requested; this test only ever wants isolated execution")
	}
	if len(spec.Argv) == 0 {
		return fmt.Errorf("hostExecBackend: empty Argv")
	}

	argv := make([]string, len(spec.Argv))
	for i, a := range spec.Argv {
		argv[i] = translateContainerPath(spec.Mounts, a)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Drop any existing key os.Environ() carries that spec.Env overrides, so
	// the override actually takes effect regardless of how the OS/libc
	// resolves a duplicate-keyed envp (unspecified by POSIX).
	baseEnv := os.Environ()
	env := make([]string, 0, len(baseEnv)+len(spec.Env))
	for _, e := range baseEnv {
		key, _, _ := strings.Cut(e, "=")
		if _, overridden := spec.Env[key]; overridden {
			continue
		}
		env = append(env, e)
	}
	for _, k := range sortedEnvKeys(spec.Env) {
		env = append(env, k+"="+translateContainerPath(spec.Mounts, spec.Env[k]))
	}
	cmd.Env = env
	if spec.Workdir != "" {
		cmd.Dir = translateContainerPath(spec.Mounts, spec.Workdir)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, out)
	}
	return nil
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// translateContainerPath rewrites a container-side path back to its host
// equivalent using mounts' (Host, Container) pairs — the inverse of what a
// real backend's mount flag does.
func translateContainerPath(mounts []backend.Mount, p string) string {
	for _, m := range mounts {
		if p == m.Container {
			return m.Host
		}
		if rest, ok := strings.CutPrefix(p, m.Container+"/"); ok {
			return filepath.Join(m.Host, rest)
		}
	}
	return p
}

// buildNPMTarballGZ builds an in-memory npm-shaped tarball: every entry in
// files, keyed by its path relative to the package root, under a "package/"
// prefix (the convention real `npm pack` output uses, confirmed against an
// actual `npm pack` tarball; pacote's extraction strips exactly one
// top-level directory, name-agnostic, but this matches production shape
// exactly rather than relying on that leniency).
func buildNPMTarballGZ(t *testing.T, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		content := files[name]
		hdr := &tar.Header{Name: "package/" + name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("writing tar content for %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	return buf.Bytes()
}

// buildFakeNodeTarXZ builds a .tar.xz containing <topDir>/bin/node (content
// is a placeholder — Realize only unpacks and strips the top directory, it
// never executes node) via the host `tar` binary (Go stdlib has no xz).
// Returns ok=false if the host tar can't produce xz output, so the caller
// can skip.
func buildFakeNodeTarXZ(t *testing.T, topDir string) (data []byte, ok bool) {
	t.Helper()
	srcDir := t.TempDir()
	nodeBinDir := filepath.Join(srcDir, topDir, "bin")
	if err := os.MkdirAll(nodeBinDir, 0o755); err != nil {
		t.Fatalf("creating fake node source tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nodeBinDir, "node"), []byte("fake node binary\n"), 0o755); err != nil {
		t.Fatalf("writing fake node binary: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "node-fixture.tar.xz")
	cmd := exec.Command("tar", "-cJf", destPath, "-C", srcDir, topDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("host tar lacks xz support (or failed): %v\n%s", err, out)
		return nil, false
	}
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading fake node tarball: %v", err)
	}
	return data, true
}

// sriFor and sha256Hex (npm_test.go's helpers, same package) cover the
// sha512-SRI and sha256-hex needs here too.

// TestIntegration_RealizeRealNPM drives the REAL Realize (this package's
// NPM.Realize) with a host-exec fake Backend that runs npm/tar directly on
// the host — no guix/docker, no real network (registry URLs point at a
// closed port so any accidental network attempt fails loudly), following
// npm_integration_test.go's local-fixtures-only pattern. Skips if npm is not
// on PATH.
func TestIntegration_RealizeRealNPM(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}

	depTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": `{"name":"tiny-dep","version":"1.0.0"}`,
		"index.js":     "module.exports = 1;\n",
	})
	harnessTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": `{"name":"tiny-harness","version":"1.0.0","bin":{"tiny-harness":"bin/cli.js"},"dependencies":{"tiny-dep":"1.0.0"}}`,
		"bin/cli.js":   "#!/usr/bin/env node\nconsole.log('hello from tiny-harness');\n",
	})
	nodeTarXZ, ok := buildFakeNodeTarXZ(t, "node-v99.0.0-linux-x64")
	if !ok {
		t.Skip("host tar cannot produce .tar.xz output")
	}

	depDigest := sha256Hex(depTarball)
	harnessDigest := sha256Hex(harnessTarball)
	nodeDigest := sha256Hex(nodeTarXZ)

	cacheDir := t.TempDir()
	for digest, content := range map[string][]byte{
		depDigest:     depTarball,
		harnessDigest: harnessTarball,
		nodeDigest:    nodeTarXZ,
	} {
		if err := os.WriteFile(filepath.Join(cacheDir, digest), content, 0o644); err != nil {
			t.Fatalf("seeding cache object %s: %v", digest, err)
		}
	}

	// Closed-port URLs (§17): if Realize's offline recipe ever regresses to
	// touch the network, dialing these fails the test instead of silently
	// succeeding against a real registry.
	const closedPort = "http://127.0.0.1:1"

	h := &pool.LockHarness{
		Source:         "npm",
		Package:        "tiny-harness",
		Version:        "1.0.0",
		PublishedAt:    pool.FormatTimestamp(realizeFixtureNow),
		FirstSeenAt:    pool.FormatTimestamp(realizeFixtureNow),
		RecipeRevision: "sha256:" + strings.Repeat("a", 64),
		Runtime: &pool.LockRuntime{
			Name:    "node",
			Version: "99.0.0",
			Artifacts: map[pool.PlatformKey]pool.LockRuntimeArtifact{
				pool.PlatformLinuxAMD64Glibc: {URL: closedPort + "/node.tar.xz", SHA256: nodeDigest},
			},
		},
		NPMLock: &pool.NPMLock{
			LockfileVersion: 3,
			Packages: map[string]pool.NPMLockPackage{
				"node_modules/tiny-harness": {
					Version: "1.0.0", Resolved: closedPort + "/tiny-harness-1.0.0.tgz",
					Integrity: sriFor(harnessTarball), SHA256: harnessDigest,
				},
				"node_modules/tiny-dep": {
					Version: "1.0.0", Resolved: closedPort + "/tiny-dep-1.0.0.tgz",
					Integrity: sriFor(depTarball), SHA256: depDigest,
				},
			},
		},
	}

	n := NewNPM("")
	b := hostExecBackend{}
	ctx := context.Background()

	staging1 := filepath.Join(t.TempDir(), "gen1")
	receipt1, err := n.Realize(ctx, b, "claude", "tiny-harness", h, pool.PlatformLinuxAMD64Glibc, cacheDir, staging1)
	if err != nil {
		t.Fatalf("Realize: %v", err)
	}

	t.Run("wrapper present and executable", func(t *testing.T) {
		wrapperPath := filepath.Join(staging1, "bin", "tiny-harness")
		info, err := os.Stat(wrapperPath)
		if err != nil {
			t.Fatalf("stat wrapper: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("wrapper is not executable: mode %v", info.Mode().Perm())
		}
		data, err := os.ReadFile(wrapperPath)
		if err != nil {
			t.Fatalf("reading wrapper: %v", err)
		}
		if !strings.Contains(string(data), "tiny-harness/bin/cli.js") {
			t.Errorf("wrapper does not reference the installed package's bin script:\n%s", data)
		}
	})

	t.Run("dependency installed", func(t *testing.T) {
		p := filepath.Join(staging1, "lib", "node_modules", "tiny-dep", "package.json")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("tiny-dep was not installed (schema gap workaround failed?): %v", err)
		}
	})

	t.Run("zero symlinks and receipt verifies", func(t *testing.T) {
		if err := pool.VerifyGenerationTree(staging1, receipt1); err != nil {
			t.Errorf("VerifyGenerationTree: %v", err)
		}
	})

	t.Run("determinism across two independent Realize runs", func(t *testing.T) {
		staging2 := filepath.Join(t.TempDir(), "gen2")
		receipt2, err := n.Realize(ctx, b, "claude", "tiny-harness", h, pool.PlatformLinuxAMD64Glibc, cacheDir, staging2)
		if err != nil {
			t.Fatalf("Realize (2nd run): %v", err)
		}
		if receipt1.ManifestDigest != receipt2.ManifestDigest {
			t.Errorf("ManifestDigest differs across two Realize runs of the same lock: %s vs %s", receipt1.ManifestDigest, receipt2.ManifestDigest)
		}
	})
}
