package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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
// (Argv elements, Workdir) back to the corresponding host path via
// spec.Mounts before exec.CommandContext. It exists only so this test can
// drive the REAL Realize (real npm, real tar) without guix/docker, following
// npm_integration_test.go's local-fixtures-only pattern one level up (that
// file fakes the backend too; this one actually executes, since Realize's
// whole point is what happens INSIDE the container). spec.Env is asserted
// empty (finding 3: Realize never uses it), so this backend does not need to
// merge/translate it at all — HOME arrives as an argv prefix instead, which
// exec.Command's own PATH-independent argv handling passes through
// untouched here (translateContainerPath rewrites the "HOME=<container
// path>" element like any other argv entry).
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
	if len(spec.Env) != 0 {
		return fmt.Errorf("hostExecBackend: RunSpec.Env = %v, want empty (finding 3: HOME must be an argv prefix, never RunSpec.Env)", spec.Env)
	}

	argv := make([]string, len(spec.Argv))
	for i, a := range spec.Argv {
		argv[i] = translateContainerPath(spec.Mounts, a)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	if spec.Workdir != "" {
		cmd.Dir = translateContainerPath(spec.Mounts, spec.Workdir)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, out)
	}
	return nil
}

// translateContainerPath rewrites a container-side path back to its host
// equivalent using mounts' (Host, Container) pairs — the inverse of what a
// real backend's mount flag does. Realize's own argv sometimes embeds a
// container path as part of a larger token (e.g. "HOME=<container path>"),
// so this replaces the container root as a substring, not just a whole-token
// match.
func translateContainerPath(mounts []backend.Mount, s string) string {
	for _, m := range mounts {
		if s == m.Container {
			return m.Host
		}
		if rest, ok := strings.CutPrefix(s, m.Container+"/"); ok {
			return filepath.Join(m.Host, rest)
		}
		if rest, ok := strings.CutPrefix(s, "HOME="+m.Container+"/"); ok {
			return "HOME=" + filepath.Join(m.Host, rest)
		}
		if s == "HOME="+m.Container {
			return "HOME=" + m.Host
		}
	}
	return s
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

// fixtureLockPackage is one "packages" map entry of a hand-built, but
// structurally real, npm package-lock.json fixture — see buildRawLockfile.
type fixtureLockPackage struct {
	Version              string            `json:"version"`
	Resolved             string            `json:"resolved,omitempty"`
	Integrity            string            `json:"integrity,omitempty"`
	Dependencies         map[string]string `json:"dependencies,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
	Optional             bool              `json:"optional,omitempty"`
	OS                   []string          `json:"os,omitempty"`
	HasShrinkwrap        bool              `json:"hasShrinkwrap,omitempty"`
}

// buildRawLockfile assembles a real-shaped (lockfileVersion 3) raw
// package-lock.json: rootDeps becomes the root ("") entry's "dependencies",
// and packages is copied in verbatim by key — exactly the document shape
// Resolve stores in NPMLock.Lockfile and Realize materializes byte for byte
// (ruling 1). Per-package edges (each entry's own "dependencies"/
// "optionalDependencies") live on the individual fixtureLockPackage values,
// not just the root — matching how a real npm lockfile represents a
// transitive dependency graph, which this package no longer hand-reconstructs.
func buildRawLockfile(t *testing.T, rootDeps map[string]string, packages map[string]fixtureLockPackage) json.RawMessage {
	t.Helper()
	all := make(map[string]any, len(packages)+1)
	all[""] = map[string]any{"dependencies": rootDeps}
	for k, v := range packages {
		all[k] = v
	}
	data, err := json.Marshal(map[string]any{"lockfileVersion": 3, "packages": all})
	if err != nil {
		t.Fatalf("marshaling fixture lockfile: %v", err)
	}
	return json.RawMessage(data)
}

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
		"package.json": `{"name":"tiny-harness","version":"1.0.0","bin":{"tiny-harness":"bin/cli.js"},"dependencies":{"tiny-dep":"1.0.0"},"optionalDependencies":{"tiny-mac-only":"1.0.0"}}`,
		"bin/cli.js":   "#!/usr/bin/env node\nconsole.log('hello from tiny-harness');\n",
	})
	nodeTarXZ, ok := buildFakeNodeTarXZ(t, "node-v99.0.0-linux-x64")
	if !ok {
		t.Skip("host tar cannot produce .tar.xz output")
	}

	depDigest := sha256Hex(depTarball)
	harnessDigest := sha256Hex(harnessTarball)
	nodeDigest := sha256Hex(nodeTarXZ)
	// tiny-mac-only IS still seeded as a well-formed tarball — `npm cache
	// add` runs (and would fail outright on a malformed archive) for every
	// locked package regardless of platform, matching today's
	// not-yet-platform-aware Fetch (§7 follow-up: it downloads/caches every
	// platform variant unconditionally). The point of this fixture is that
	// `npm ci` itself SKIPS it during dependency resolution (os mismatch),
	// never installing it — proven below — not that it's never cached.
	macOnlyPlaceholder := buildNPMTarballGZ(t, map[string]string{
		"package.json": `{"name":"tiny-mac-only","version":"1.0.0","os":["darwin"]}`,
	})
	macOnlyDigest := sha256Hex(macOnlyPlaceholder)

	// Finding E — the case ruling 1 was written for: a NESTED
	// version-conflict-shaped entry, node_modules/nest-a/node_modules/nest-b,
	// only reachable via nest-a's OWN "dependencies" edge (never a root-level
	// one). The old hand-reconstruction scheme could not represent this
	// shape at all; verbatim materialization must carry it through and let
	// npm install it exactly where the lockfile says.
	nestBTarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": `{"name":"nest-b","version":"1.0.0"}`,
	})
	nestATarball := buildNPMTarballGZ(t, map[string]string{
		"package.json": `{"name":"nest-a","version":"1.0.0","dependencies":{"nest-b":"1.0.0"}}`,
	})
	nestBDigest := sha256Hex(nestBTarball)
	nestADigest := sha256Hex(nestATarball)

	cacheDir := t.TempDir()
	for digest, content := range map[string][]byte{
		depDigest:     depTarball,
		harnessDigest: harnessTarball,
		nodeDigest:    nodeTarXZ,
		macOnlyDigest: macOnlyPlaceholder,
		nestADigest:   nestATarball,
		nestBDigest:   nestBTarball,
	} {
		if err := os.WriteFile(filepath.Join(cacheDir, digest), content, 0o644); err != nil {
			t.Fatalf("seeding cache object %s: %v", digest, err)
		}
	}

	// Closed-port URLs (§17): if Realize's offline recipe ever regresses to
	// touch the network, dialing these fails the test instead of silently
	// succeeding against a real registry.
	const closedPort = "http://127.0.0.1:1"

	rawLockfile := buildRawLockfile(t,
		map[string]string{"tiny-harness": "1.0.0", "nest-a": "1.0.0"},
		map[string]fixtureLockPackage{
			"node_modules/tiny-harness": {
				Version: "1.0.0", Resolved: closedPort + "/tiny-harness-1.0.0.tgz", Integrity: sriFor(harnessTarball),
				Dependencies:         map[string]string{"tiny-dep": "1.0.0"},
				OptionalDependencies: map[string]string{"tiny-mac-only": "1.0.0"},
			},
			"node_modules/tiny-dep": {
				Version: "1.0.0", Resolved: closedPort + "/tiny-dep-1.0.0.tgz", Integrity: sriFor(depTarball),
			},
			// os-mismatched (this test always runs on linux) optional
			// dependency: npm must SKIP it, never install it, never fetch it.
			"node_modules/tiny-mac-only": {
				Version: "1.0.0", Resolved: closedPort + "/tiny-mac-only-1.0.0.tgz", Integrity: sriFor(macOnlyPlaceholder),
				Optional: true, OS: []string{"darwin"},
			},
			// nest-a is a top-level, root-reachable package; nest-b is
			// reachable ONLY through nest-a's own "dependencies" edge and is
			// installed NESTED under it (node_modules/nest-a/node_modules/nest-b),
			// never promoted to the top level — the shape ruling 1 exists for.
			"node_modules/nest-a": {
				Version: "1.0.0", Resolved: closedPort + "/nest-a-1.0.0.tgz", Integrity: sriFor(nestATarball),
				Dependencies: map[string]string{"nest-b": "1.0.0"},
			},
			"node_modules/nest-a/node_modules/nest-b": {
				Version: "1.0.0", Resolved: closedPort + "/nest-b-1.0.0.tgz", Integrity: sriFor(nestBTarball),
			},
		})

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
				"node_modules/tiny-mac-only": {
					Version: "1.0.0", Resolved: closedPort + "/tiny-mac-only-1.0.0.tgz",
					Integrity: sriFor(macOnlyPlaceholder), SHA256: macOnlyDigest,
				},
				"node_modules/nest-a": {
					Version: "1.0.0", Resolved: closedPort + "/nest-a-1.0.0.tgz",
					Integrity: sriFor(nestATarball), SHA256: nestADigest,
				},
				"node_modules/nest-a/node_modules/nest-b": {
					Version: "1.0.0", Resolved: closedPort + "/nest-b-1.0.0.tgz",
					Integrity: sriFor(nestBTarball), SHA256: nestBDigest,
				},
			},
			Lockfile: rawLockfile,
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
			t.Errorf("tiny-dep was not installed: %v", err)
		}
	})

	// Finding E: the nested node_modules/nest-a/node_modules/nest-b entry
	// (reachable only through nest-a's own "dependencies" edge, never a
	// root-level one) must flow through verbatim and actually get installed
	// at its nested location — the exact case ruling 1's verbatim
	// materialization fix exists for.
	t.Run("nested entry flows through verbatim and installs nested", func(t *testing.T) {
		nestA := filepath.Join(staging1, "lib", "node_modules", "nest-a", "package.json")
		if _, err := os.Stat(nestA); err != nil {
			t.Errorf("nest-a was not installed: %v", err)
		}
		nestBTopLevel := filepath.Join(staging1, "lib", "node_modules", "nest-b")
		if _, err := os.Stat(nestBTopLevel); err == nil {
			t.Error("nest-b was installed at the TOP level, want it nested under nest-a (it should never have been promoted to the root)")
		}
		nestBNested := filepath.Join(staging1, "lib", "node_modules", "nest-a", "node_modules", "nest-b", "package.json")
		if _, err := os.Stat(nestBNested); err != nil {
			t.Errorf("nest-b was not installed nested under nest-a: %v", err)
		}
	})

	// Finding 1's regression test: a real (verbatim) lockfile lets npm apply
	// its own os-gating; a hand-reconstructed lockfile (the retired
	// npmTopLevelDependencies scheme) had no "optional"/"os" fields at all,
	// which made every such dependency unconditionally required.
	t.Run("os-mismatched optional dependency is skipped, not installed", func(t *testing.T) {
		p := filepath.Join(staging1, "lib", "node_modules", "tiny-mac-only")
		if _, err := os.Stat(p); err == nil {
			t.Error("tiny-mac-only was installed despite an os mismatch, want it skipped")
		} else if !os.IsNotExist(err) {
			t.Errorf("stat %s: %v", p, err)
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

	// Finding 2's regression test: a second, independent harness fixture
	// whose own bin entry is a plain #!/bin/sh script (not JS-under-node,
	// and not an ELF binary either — the third of classifyBinTarget's four
	// cases exercised end to end here). The wrapper must exec it directly.
	t.Run("bin target with a non-node shebang execs directly, not via node", func(t *testing.T) {
		shTarball := buildNPMTarballGZ(t, map[string]string{
			"package.json": `{"name":"tiny-shell-harness","version":"1.0.0","bin":{"tiny-shell-harness":"bin/cli.sh"}}`,
			"bin/cli.sh":   "#!/bin/sh\necho hello from tiny-shell-harness\n",
		})
		shDigest := sha256Hex(shTarball)
		if err := os.WriteFile(filepath.Join(cacheDir, shDigest), shTarball, 0o644); err != nil {
			t.Fatalf("seeding cache object %s: %v", shDigest, err)
		}

		shLockfile := buildRawLockfile(t,
			map[string]string{"tiny-shell-harness": "1.0.0"},
			map[string]fixtureLockPackage{
				"node_modules/tiny-shell-harness": {
					Version: "1.0.0", Resolved: closedPort + "/tiny-shell-harness-1.0.0.tgz", Integrity: sriFor(shTarball),
				},
			})

		shHarness := &pool.LockHarness{
			Source:         "npm",
			Package:        "tiny-shell-harness",
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
					"node_modules/tiny-shell-harness": {
						Version: "1.0.0", Resolved: closedPort + "/tiny-shell-harness-1.0.0.tgz",
						Integrity: sriFor(shTarball), SHA256: shDigest,
					},
				},
				Lockfile: shLockfile,
			},
		}

		stagingSh := filepath.Join(t.TempDir(), "gen-sh")
		shReceipt, err := n.Realize(ctx, b, "tiny-shell-harness", "tiny-shell-harness", shHarness, pool.PlatformLinuxAMD64Glibc, cacheDir, stagingSh)
		if err != nil {
			t.Fatalf("Realize (sh-bin harness): %v", err)
		}
		if err := pool.VerifyGenerationTree(stagingSh, shReceipt); err != nil {
			t.Errorf("VerifyGenerationTree (sh-bin harness): %v", err)
		}

		wrapperPath := filepath.Join(stagingSh, "bin", "tiny-shell-harness")
		data, err := os.ReadFile(wrapperPath)
		if err != nil {
			t.Fatalf("reading sh-bin wrapper: %v", err)
		}
		if strings.Contains(string(data), "runtime/node/bin/node") {
			t.Errorf("wrapper routes through node, want a direct exec of the #!/bin/sh target:\n%s", data)
		}
		if !strings.Contains(string(data), "tiny-shell-harness/bin/cli.sh") {
			t.Errorf("wrapper does not reference the sh script directly:\n%s", data)
		}

		// Finding D: the first behavioral proof in this suite that a
		// generated entrypoint actually RUNS, not just that its text looks
		// right — execute the wrapper for real and check its stdout.
		out, err := exec.Command(wrapperPath).CombinedOutput()
		if err != nil {
			t.Fatalf("executing wrapper: %v\n%s", err, out)
		}
		if strings.TrimSpace(string(out)) != "hello from tiny-shell-harness" {
			t.Errorf("wrapper output = %q, want %q", strings.TrimSpace(string(out)), "hello from tiny-shell-harness")
		}
	})
}
