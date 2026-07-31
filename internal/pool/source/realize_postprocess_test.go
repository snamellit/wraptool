package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

// postProcess bundles generateHarnessWrapper + despymlinkTree +
// ensureRuntimeBinariesExecutable — the same sequence Realize itself runs
// (minus removeBuildResidue, which these synthetic-tree tests have no use
// for: there is no .build scratch dir to begin with) — so these tests don't
// have to repeat all three calls individually.
func postProcess(root, pkg, binName string) error {
	if err := generateHarnessWrapper(root, pkg, binName); err != nil {
		return err
	}
	if err := despymlinkTree(root); err != nil {
		return err
	}
	return ensureRuntimeBinariesExecutable(filepath.Join(root, "runtime", "node"))
}

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

	if err := postProcess(root, pkg, binName); err != nil {
		t.Fatalf("postProcess: %v", err)
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

	err := postProcess(root, pkg, binName)
	if err == nil {
		t.Fatal("postProcess: got nil error, want rejection of an absolute symlink target")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error = %q, want it to mention an absolute target", err.Error())
	}
}

// --- finding 4: shebang classification + symlink chain resolution ----------

// TestReplaceSymlink_NonNodeShebangIsCopiedVerbatim covers finding 4: a
// symlink to a script whose shebang does NOT name node (a real #!/bin/sh
// script) must be copied verbatim, not node-wrapped — it is already
// directly executable on its own.
func TestReplaceSymlink_NonNodeShebangIsCopiedVerbatim(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "runtime", "node", "bin"))
	mustWriteFile(t, filepath.Join(root, "runtime", "node", "bin", "node"), "fake node\n", 0o755)

	binDir := filepath.Join(root, "lib", "node_modules", ".bin")
	mustMkdirAll(t, binDir)
	shScript := "#!/bin/sh\necho hi\n"
	mustWriteFile(t, filepath.Join(binDir, "shtool.sh"), shScript, 0o755)
	mustSymlink(t, "shtool.sh", filepath.Join(binDir, "shtool"))

	if err := despymlinkTree(root); err != nil {
		t.Fatalf("despymlinkTree: %v", err)
	}

	p := filepath.Join(binDir, "shtool")
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("still a symlink, want replaced")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(data) != shScript {
		t.Errorf("content = %q, want the verbatim sh script %q (not a node wrapper)", data, shScript)
	}
	if strings.Contains(string(data), "node") {
		t.Errorf("content mentions node, want a verbatim copy that never routes through it: %q", data)
	}
}

// TestReplaceSymlink_ChainResolvesToFinalTarget_BothOrders covers finding 4's
// "resolve symlink chains fully before classifying" fix: A -> B -> a real
// node-shebang file. Both wrappers (for A and for B) must reference the
// FINAL regular file directly — never the intermediate hop — so neither
// depends on which order filepath.WalkDir happens to visit A and B in.
// Without full-chain resolution, whichever of A/B is processed first would
// bake in a reference to the OTHER (still a symlink at that moment), and
// once that other link is independently rewritten into a wrapper script, the
// first-processed link would end up asking node to interpret a /bin/sh
// wrapper as JavaScript.
func TestReplaceSymlink_ChainResolvesToFinalTarget_BothOrders(t *testing.T) {
	runOrder := func(t *testing.T, aName, bName string) {
		root := t.TempDir()
		mustMkdirAll(t, filepath.Join(root, "runtime", "node", "bin"))
		mustWriteFile(t, filepath.Join(root, "runtime", "node", "bin", "node"), "fake node\n", 0o755)

		dir := filepath.Join(root, "lib", "node_modules", ".bin")
		mustMkdirAll(t, dir)
		mustWriteFile(t, filepath.Join(dir, "final.js"), "#!/usr/bin/env node\nconsole.log('final');\n", 0o644)
		// aName -> bName -> final.js (a 2-hop chain, both links in the same
		// directory so their WalkDir visit order is controlled purely by
		// aName/bName's lexical order).
		mustSymlink(t, bName, filepath.Join(dir, aName))
		mustSymlink(t, "final.js", filepath.Join(dir, bName))

		if err := despymlinkTree(root); err != nil {
			t.Fatalf("despymlinkTree: %v", err)
		}

		wantWrapper := "#!/bin/sh\n" +
			`d=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)` + "\n" +
			`exec "$d/../../../runtime/node/bin/node" "$d/final.js" "$@"` + "\n"

		for _, name := range []string{aName, bName} {
			p := filepath.Join(dir, name)
			info, err := os.Lstat(p)
			if err != nil {
				t.Fatalf("lstat %s: %v", name, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("%s is still a symlink, want replaced", name)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			if string(data) != wantWrapper {
				t.Errorf("%s content =\n%s\nwant (referencing the FINAL target directly)\n%s", name, data, wantWrapper)
			}
		}
	}

	// "a" sorts before "b" lexically: WalkDir visits the outer link first.
	t.Run("outer link visited first", func(t *testing.T) {
		runOrder(t, "a-outer", "b-inner")
	})
	// Reversed names: WalkDir visits the (functionally) inner-pointing link
	// first this time.
	t.Run("inner link visited first", func(t *testing.T) {
		runOrder(t, "z-outer", "m-inner")
	})
}

// --- finding 2: bin-target classification ----------------------------------

func TestGenerateHarnessWrapper_ELFTargetGetsDirectExecWrapper(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "lib", "node_modules", "claude-like")
	mustMkdirAll(t, filepath.Join(pkgDir, "bin"))
	mustWriteFile(t, filepath.Join(pkgDir, "package.json"), `{"name":"claude-like","bin":{"claude-like":"bin/claude-like.exe"}}`, 0o644)
	elfContent := "\x7fELF" + strings.Repeat("x", 60)
	mustWriteFile(t, filepath.Join(pkgDir, "bin", "claude-like.exe"), elfContent, 0o644)

	if err := generateHarnessWrapper(root, "claude-like", "claude-like"); err != nil {
		t.Fatalf("generateHarnessWrapper: %v", err)
	}

	wrapperPath := filepath.Join(root, "bin", "claude-like")
	data, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("reading wrapper: %v", err)
	}
	want := "#!/bin/sh\n" +
		`d=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)` + "\n" +
		`exec "$d/lib/node_modules/claude-like/bin/claude-like.exe" "$@"` + "\n"
	if string(data) != want {
		t.Errorf("wrapper =\n%s\nwant\n%s", data, want)
	}
	if strings.Contains(string(data), "runtime/node/bin/node") {
		t.Error("wrapper routes through node, want a direct exec of the ELF binary")
	}
	assertExecutable(t, wrapperPath)
	assertExecutable(t, filepath.Join(pkgDir, "bin", "claude-like.exe"))
}

func TestGenerateHarnessWrapper_NonNodeShebangGetsDirectExecWrapper(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "lib", "node_modules", "shtool")
	mustMkdirAll(t, filepath.Join(pkgDir, "bin"))
	mustWriteFile(t, filepath.Join(pkgDir, "package.json"), `{"name":"shtool","bin":{"shtool":"bin/shtool.sh"}}`, 0o644)
	mustWriteFile(t, filepath.Join(pkgDir, "bin", "shtool.sh"), "#!/bin/sh\necho hi\n", 0o644)

	if err := generateHarnessWrapper(root, "shtool", "shtool"); err != nil {
		t.Fatalf("generateHarnessWrapper: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "bin", "shtool"))
	if err != nil {
		t.Fatalf("reading wrapper: %v", err)
	}
	if strings.Contains(string(data), "runtime/node/bin/node") {
		t.Error("wrapper routes through node, want a direct exec of the sh script")
	}
	if !strings.Contains(string(data), `exec "$d/lib/node_modules/shtool/bin/shtool.sh" "$@"`) {
		t.Errorf("wrapper does not directly exec the target: %s", data)
	}
}

func TestGenerateHarnessWrapper_NoShebangDefaultsToNode(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "lib", "node_modules", "plainjs")
	mustMkdirAll(t, filepath.Join(pkgDir, "bin"))
	mustWriteFile(t, filepath.Join(pkgDir, "package.json"), `{"name":"plainjs","bin":{"plainjs":"bin/cli.js"}}`, 0o644)
	mustWriteFile(t, filepath.Join(pkgDir, "bin", "cli.js"), "console.log('no shebang');\n", 0o644)

	if err := generateHarnessWrapper(root, "plainjs", "plainjs"); err != nil {
		t.Fatalf("generateHarnessWrapper: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "bin", "plainjs"))
	if err != nil {
		t.Fatalf("reading wrapper: %v", err)
	}
	if !strings.Contains(string(data), "runtime/node/bin/node") {
		t.Errorf("wrapper does not route through node (npm's default for an extensionless bin with no shebang): %s", data)
	}
}

func TestGenerateHarnessWrapper_MissingBinTargetErrors(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "lib", "node_modules", "broken")
	mustMkdirAll(t, pkgDir)
	mustWriteFile(t, filepath.Join(pkgDir, "package.json"), `{"name":"broken","bin":{"broken":"bin/missing.js"}}`, 0o644)

	err := generateHarnessWrapper(root, "broken", "broken")
	if err == nil {
		t.Fatal("generateHarnessWrapper: got nil error, want rejection of a missing bin target")
	}
}

func TestGenerateHarnessWrapper_EmptyBinTargetErrors(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "lib", "node_modules", "placeholder")
	mustMkdirAll(t, filepath.Join(pkgDir, "bin"))
	mustWriteFile(t, filepath.Join(pkgDir, "package.json"), `{"name":"placeholder","bin":{"placeholder":"bin/placeholder.exe"}}`, 0o644)
	mustWriteFile(t, filepath.Join(pkgDir, "bin", "placeholder.exe"), "", 0o644) // postinstall never ran / never replaced it

	err := generateHarnessWrapper(root, "placeholder", "placeholder")
	if err == nil {
		t.Fatal("generateHarnessWrapper: got nil error, want rejection of an empty (never-replaced placeholder) bin target")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to mention the target is empty", err.Error())
	}
}

// --- finding 5: validateGeneratedShellSegment -------------------------------

func TestValidateGeneratedShellSegment_RejectsDangerousCharacters(t *testing.T) {
	for _, s := range []string{
		`has"quote`,
		"has`backtick",
		`has$dollar`,
		`has\backslash`,
		"has/../dotdot",
		"..",
	} {
		if err := validateGeneratedShellSegment("test", s); err == nil {
			t.Errorf("validateGeneratedShellSegment(%q): got nil error, want rejection", s)
		}
	}
}

func TestValidateGeneratedShellSegment_AcceptsOrdinaryPaths(t *testing.T) {
	for _, s := range []string{"bin/cli.js", "@scope/name", "lib/index.js"} {
		if err := validateGeneratedShellSegment("test", s); err != nil {
			t.Errorf("validateGeneratedShellSegment(%q): %v, want nil", s, err)
		}
	}
}

func TestGenerateHarnessWrapper_RejectsDangerousBinPath(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "lib", "node_modules", "evil")
	mustMkdirAll(t, pkgDir)
	mustWriteFile(t, filepath.Join(pkgDir, "package.json"), `{"name":"evil","bin":{"evil":"bin/$(rm -rf /).js"}}`, 0o644)

	err := generateHarnessWrapper(root, "evil", "evil")
	if err == nil {
		t.Fatal("generateHarnessWrapper: got nil error, want rejection of a dangerous bin path")
	}
}
