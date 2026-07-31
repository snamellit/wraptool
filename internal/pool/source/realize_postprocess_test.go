package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

// buildSyntheticNPMCITree constructs a stand-in for what a real `npm ci`
// (plus the node runtime unpack) would have left in stagingGenDir, entirely
// in Go with no backend involved:
//
//   - lib/node_modules/<pkg>/package.json ("bin": {binName: "bin/cli.js"})
//     and lib/node_modules/<pkg>/bin/cli.js (a node-shebang script) — the
//     package post-processing must generate the harness's own wrapper for;
//   - lib/node_modules/.bin/<binName> -> ../<pkg>/bin/cli.js, an ordinary
//     top-level npm .bin symlink to that same node-shebang script (the "npm-
//     style bin shim" case);
//   - lib/node_modules/nested/node_modules/.bin/tool -> a symlink nested two
//     levels deep, pointing at another node-shebang script — proves the
//     symlink walk isn't limited to a fixed depth;
//   - lib/node_modules/other/docs -> ../shared/README.md, a symlink to a
//     plain (non-script) file — the "file copy" branch;
//   - runtime/node/bin/node, a stand-in runtime binary post-processing must
//     leave (or make) executable.
func buildSyntheticNPMCITree(t *testing.T) (root, pkg, binName string) {
	t.Helper()
	root = t.TempDir()
	pkg = "@example/harness"
	binName = "example-harness"

	pkgDir := filepath.Join(root, "lib", "node_modules", pkg)
	mustMkdirAll(t, filepath.Join(pkgDir, "bin"))
	mustWriteFile(t, filepath.Join(pkgDir, "package.json"), `{"name":"@example/harness","bin":{"example-harness":"bin/cli.js"}}`, 0o644)
	mustWriteFile(t, filepath.Join(pkgDir, "bin", "cli.js"), "#!/usr/bin/env node\nconsole.log('hi');\n", 0o644)

	// A top-level npm .bin symlink (an "npm-style bin shim" target).
	binDir := filepath.Join(root, "lib", "node_modules", ".bin")
	mustMkdirAll(t, binDir)
	mustSymlink(t, filepath.Join("..", "@example", "harness", "bin", "cli.js"), filepath.Join(binDir, binName))

	// A nested .bin symlink two levels deep.
	nestedPkgDir := filepath.Join(root, "lib", "node_modules", "nested")
	mustMkdirAll(t, filepath.Join(nestedPkgDir, "bin"))
	mustWriteFile(t, filepath.Join(nestedPkgDir, "bin", "tool.js"), "#!/usr/bin/env node\nconsole.log('tool');\n", 0o644)
	nestedBinDir := filepath.Join(nestedPkgDir, "node_modules", ".bin")
	mustMkdirAll(t, nestedBinDir)
	mustSymlink(t, filepath.Join("..", "..", "bin", "tool.js"), filepath.Join(nestedBinDir, "tool"))

	// A plain-data (non-script) symlink.
	sharedDir := filepath.Join(root, "lib", "node_modules", "shared")
	mustMkdirAll(t, sharedDir)
	mustWriteFile(t, filepath.Join(sharedDir, "README.md"), "hello\n", 0o644)
	otherDir := filepath.Join(root, "lib", "node_modules", "other")
	mustMkdirAll(t, otherDir)
	mustSymlink(t, filepath.Join("..", "shared", "README.md"), filepath.Join(otherDir, "docs"))

	// Stand-in unpacked node runtime.
	runtimeBinDir := filepath.Join(root, "runtime", "node", "bin")
	mustMkdirAll(t, runtimeBinDir)
	mustWriteFile(t, filepath.Join(runtimeBinDir, "node"), "fake node binary\n", 0o644)

	return root, pkg, binName
}

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWriteFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func mustSymlink(t *testing.T, target, linkPath string) {
	t.Helper()
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("symlink %s -> %s: %v", linkPath, target, err)
	}
}

func TestPostProcessGeneration_ReplacesEverySymlink(t *testing.T) {
	root, pkg, binName := buildSyntheticNPMCITree(t)

	if err := postProcessGeneration(root, pkg, binName); err != nil {
		t.Fatalf("postProcessGeneration: %v", err)
	}

	// Zero symlinks remain anywhere in the tree — the same check
	// pool.ScanGenerationFiles performs when Realize builds the receipt.
	files, err := pool.ScanGenerationFiles(root)
	if err != nil {
		t.Fatalf("ScanGenerationFiles found a surviving symlink (or other problem): %v", err)
	}
	if len(files) == 0 {
		t.Fatal("ScanGenerationFiles returned no files")
	}

	t.Run("harness wrapper golden", func(t *testing.T) {
		wrapperPath := filepath.Join(root, "bin", binName)
		data, err := os.ReadFile(wrapperPath)
		if err != nil {
			t.Fatalf("reading wrapper: %v", err)
		}
		want := "#!/bin/sh\n" +
			`d=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)` + "\n" +
			`exec "$d/runtime/node/bin/node" "$d/lib/node_modules/@example/harness/bin/cli.js" "$@"` + "\n"
		if string(data) != want {
			t.Errorf("wrapper content =\n%s\nwant\n%s", data, want)
		}
		assertWrapperInvariants(t, string(data), root)
		assertExecutable(t, wrapperPath)
	})

	t.Run("top-level .bin wrapper golden", func(t *testing.T) {
		p := filepath.Join(root, "lib", "node_modules", ".bin", binName)
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("still a symlink, want replaced with a wrapper")
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		want := "#!/bin/sh\n" +
			`d=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)` + "\n" +
			`exec "$d/../../../runtime/node/bin/node" "$d/../@example/harness/bin/cli.js" "$@"` + "\n"
		if string(data) != want {
			t.Errorf(".bin wrapper content =\n%s\nwant\n%s", data, want)
		}
		assertWrapperInvariants(t, string(data), root)
		assertExecutable(t, p)
	})

	t.Run("nested .bin wrapper", func(t *testing.T) {
		p := filepath.Join(root, "lib", "node_modules", "nested", "node_modules", ".bin", "tool")
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("still a symlink, want replaced with a wrapper")
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		assertWrapperInvariants(t, string(data), root)
		assertExecutable(t, p)
	})

	t.Run("plain-data symlink replaced with a copy", func(t *testing.T) {
		p := filepath.Join(root, "lib", "node_modules", "other", "docs")
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat: %v", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatal("still a symlink, want replaced with a copy")
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		if string(data) != "hello\n" {
			t.Errorf("copied content = %q, want %q", data, "hello\n")
		}
	})

	t.Run("runtime node binary executable", func(t *testing.T) {
		assertExecutable(t, filepath.Join(root, "runtime", "node", "bin", "node"))
	})
}

// assertWrapperInvariants checks the §6.3 properties every generated wrapper
// must have: starts with the shebang, and contains no absolute path — in
// particular never the staging root's own (host-specific) path.
func assertWrapperInvariants(t *testing.T, content, root string) {
	t.Helper()
	if !strings.HasPrefix(content, "#!/bin/sh\n") {
		t.Errorf("wrapper does not start with #!/bin/sh:\n%s", content)
	}
	if strings.Contains(content, root) {
		t.Errorf("wrapper contains the staging root's host path %q, want it absent:\n%s", root, content)
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "\"/") {
			t.Errorf("wrapper line looks like it embeds an absolute path: %q", line)
		}
	}
}

func assertExecutable(t *testing.T, p string) {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v)", p, info.Mode().Perm())
	}
}

func TestPostProcessGeneration_RejectsAbsoluteSymlinkTarget(t *testing.T) {
	root, pkg, binName := buildSyntheticNPMCITree(t)
	// Overwrite one symlink with an absolute target.
	p := filepath.Join(root, "lib", "node_modules", "other", "docs")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(root, "lib", "node_modules", "shared", "README.md"), p)

	err := postProcessGeneration(root, pkg, binName)
	if err == nil {
		t.Fatal("postProcessGeneration: got nil error, want rejection of an absolute symlink target")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error = %q, want it to mention an absolute target", err.Error())
	}
}
