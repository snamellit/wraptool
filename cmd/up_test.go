package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestProbeAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8717":   "127.0.0.1:8717",
		"[::]:8717":      "127.0.0.1:8717",
		"::":             "::", // no port → returned as-is
		"127.0.0.1:8717": "127.0.0.1:8717",
		"192.168.1.5:80": "192.168.1.5:80",
	}
	for in, want := range cases {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~":            home,
		"~/x/y":        filepath.Join(home, "x", "y"),
		"/abs/path":    "/abs/path",
		"":             "",
		"rel/~notilde": "rel/~notilde",
	}
	for in, want := range cases {
		if got := expandTilde(in); got != want {
			t.Errorf("expandTilde(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWantShell(t *testing.T) {
	if !wantShell("always") {
		t.Error(`wantShell("always") = false, want true`)
	}
	if wantShell("never") {
		t.Error(`wantShell("never") = true, want false`)
	}
	// "auto" depends on the terminal; under `go test` stdin is not a TTY.
	if wantShell("auto") {
		t.Error(`wantShell("auto") = true with non-TTY stdin, want false`)
	}
}

func TestHasDevcontainer(t *testing.T) {
	dir := t.TempDir()
	if hasDevcontainer(dir) {
		t.Fatal("empty dir should have no devcontainer")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if !hasDevcontainer(dir) {
		t.Fatal("should detect .devcontainer/devcontainer.json")
	}
}

func TestDevcontainerDeclaresPoolMount(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		dc := filepath.Join(dir, ".devcontainer")
		if err := os.MkdirAll(dc, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dc, "devcontainer.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("declares the pool mount", func(t *testing.T) {
		dir := write(t, `{"mounts":["source=${localEnv:HOME}/.local/share/wraptool/harness-pool/home,target=`+devcontainerPoolHome+`,type=bind"]}`)
		if !devcontainerDeclaresPoolMount(dir) {
			t.Error("should detect the pool mount target in the config")
		}
	})

	t.Run("no pool mount", func(t *testing.T) {
		dir := write(t, `{"image":"mcr.microsoft.com/devcontainers/base:ubuntu"}`)
		if devcontainerDeclaresPoolMount(dir) {
			t.Error("must not report a mount that is not declared")
		}
	})

	t.Run("no config at all", func(t *testing.T) {
		if devcontainerDeclaresPoolMount(t.TempDir()) {
			t.Error("no config → no declared mount")
		}
	})
}

func TestRunUpRejectsAdminTokenInsideWorkspaceWhenDisabled(t *testing.T) {
	origIsInContainer := isInContainer
	isInContainer = func() bool { return false }
	t.Cleanup(func() { isInContainer = origIsInContainer })

	workspace := t.TempDir()
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("admin:\n  auth_token_file: "+filepath.Join(workspace, "admin-token")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	err := runUp(upOptions{
		configPath: configPath,
		noServer:   true,
		workspace:  workspace,
		runtime:    "devcontainer",
		harness:    "claude", // exercise the admin-token check itself, not the (now-earlier) --harness validation
		shell:      "never",
	})
	if err == nil || !strings.Contains(err.Error(), "admin auth token resolves to") {
		t.Fatalf("disabled admin token inside workspace was not rejected: %v", err)
	}
}

// TestRunUpRejectsUnknownHarnessBeforeStartingServer pins item 6: an unknown
// --harness fails fast, next to selectRuntime, before any of the
// workspace/config/admin-token checks below it or the shared-server startup
// past them — spawning the server for a request that can never bring up a
// container would be wasted work and a confusing partial state.
// TestRunUpRejectsUnknownHarnessBeforeAdminTokenCheck pins item 6: --harness
// validation happens in runUp itself, immediately next to selectRuntime —
// BEFORE the admin-token-outside-workspace check a few lines later in the
// same `!opts.noContainer` block (TestRunUpRejectsAdminTokenInsideWorkspaceWhenDisabled).
// Proven by triggering BOTH failure conditions in one call: only the
// unknown-harness error can surface if that ordering holds; if harness
// validation instead ran later (e.g. deep inside bringUpContainer, as
// before this wave), the earlier admin-token check would fail first.
func TestRunUpRejectsUnknownHarnessBeforeAdminTokenCheck(t *testing.T) {
	origIsInContainer := isInContainer
	isInContainer = func() bool { return false }
	t.Cleanup(func() { isInContainer = origIsInContainer })

	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("admin:\n  auth_token_file: "+filepath.Join(workspace, "admin-token")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	err := runUp(upOptions{
		configPath: configPath,
		noServer:   true,
		workspace:  workspace,
		runtime:    "devcontainer",
		harness:    "not-a-real-harness",
		shell:      "never",
	})
	if err == nil || !strings.Contains(err.Error(), `unknown harness "not-a-real-harness"`) {
		t.Fatalf("unknown --harness was not rejected before the admin-token check: %v", err)
	}
	if strings.Contains(err.Error(), "admin auth token resolves to") {
		t.Fatalf("admin-token check ran before harness validation: %v", err)
	}
}

// --- devcontainer runtime/ mount (design/harness/unified-harness-pool.md §12.2) ---

// devcontainerPoolPlanTestLayout builds a t.TempDir()-rooted pool layout
// (runtime/ dir + bin/ + generations/) and returns a devcontainerPoolPlanInput
// seeded from it, mirroring guixPoolPlanTestLayout (cmd/guixcontainer_test.go)
// so tests can flip pointer content and file presence without a real
// $XDG_DATA_HOME or the devcontainer CLI.
func devcontainerPoolPlanTestLayout(t *testing.T, harnessName string, poolLockable bool) (in devcontainerPoolPlanInput, root string) {
	t.Helper()
	root = t.TempDir()
	in = devcontainerPoolPlanInput{poolPlanInput: poolPlanInput{
		Harness:               harnessName,
		PoolLockable:          poolLockable,
		RuntimeDir:            filepath.Join(root, "runtime"),
		RuntimeBinDir:         filepath.Join(root, "runtime", "bin"),
		RuntimeGenerationsDir: filepath.Join(root, "runtime", "generations"),
	}}
	return in, root
}

// TestResolveDevcontainerPoolPlanNoActivePointer covers every "no active
// pointer" case: missing pointer file, unreadable pointer path, empty or
// whitespace/newline-only content, and an empty pointerPath (simulating a
// platform-key resolution failure). All fall back to the no-mount plan; the
// deprecation notice is present for a pool-lockable harness and silent for
// one that is not (mirrors TestResolveGuixPoolPlanNoActivePointer).
func TestResolveDevcontainerPoolPlanNoActivePointer(t *testing.T) {
	cases := []struct {
		name        string
		pointerPath func(t *testing.T, root string) string
	}{
		{"missing pointer file", func(t *testing.T, root string) string { return filepath.Join(root, "active.gen") }},
		{"empty pointer file", func(t *testing.T, root string) string { return writePointerFile(t, root, "") }},
		{"whitespace-only pointer file", func(t *testing.T, root string) string { return writePointerFile(t, root, " ") }},
		{"newline-only pointer file", func(t *testing.T, root string) string { return writePointerFile(t, root, "\n\n") }},
		{"unreadable pointer path (a directory)", func(t *testing.T, root string) string {
			d := filepath.Join(root, "active.gen")
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			return d
		}},
		{"empty pointerPath (platform key unresolved)", func(t *testing.T, root string) string { return "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, lockable := range []bool{true, false} {
				t.Run(map[bool]string{true: "lockable", false: "not lockable"}[lockable], func(t *testing.T) {
					in, root := devcontainerPoolPlanTestLayout(t, "claude", lockable)
					in.PointerPath = tc.pointerPath(t, root)

					plan := resolveDevcontainerPoolPlan(in)

					if plan.mountRuntime {
						t.Fatalf("plan.mountRuntime = true, want no-mount fallback")
					}
					if plan.runtimeDir != "" {
						t.Errorf("runtimeDir = %q, want empty for a no-mount plan", plan.runtimeDir)
					}
					if lockable {
						if plan.notice == "" {
							t.Fatal("expected a fallback deprecation notice for a pool-lockable harness, got none")
						}
						if !strings.Contains(plan.notice, "claude") ||
							!strings.Contains(plan.notice, "wraptool harness install claude") ||
							!strings.Contains(plan.notice, "wraptool harness sync") {
							t.Errorf("notice = %q, want it to name the harness and both fix commands", plan.notice)
						}
					} else if plan.notice != "" {
						t.Errorf("notice = %q, want none for a non-pool-lockable harness (silent fallback)", plan.notice)
					}
				})
			}
		})
	}
}

// TestResolveDevcontainerPoolPlanActivePointer asserts the happy path: a
// pointer whose generation id resolves to real dispatcher + generation-bin
// files on disk activates the runtime mount, notice-free.
func TestResolveDevcontainerPoolPlanActivePointer(t *testing.T) {
	in, root := devcontainerPoolPlanTestLayout(t, "claude", true)
	in.PointerPath = writePointerFile(t, root, "claude-1.2.3-linux-arm64-glibc-a1b2c3d4\n")
	writePoolGeneration(t, in.poolPlanInput, "claude-1.2.3-linux-arm64-glibc-a1b2c3d4", true, true)

	plan := resolveDevcontainerPoolPlan(in)

	if !plan.mountRuntime {
		t.Fatalf("plan.mountRuntime = false, want true")
	}
	if plan.runtimeDir != in.RuntimeDir {
		t.Errorf("runtimeDir = %q, want %q", plan.runtimeDir, in.RuntimeDir)
	}
	if plan.notice != "" {
		t.Errorf("notice = %q, want none", plan.notice)
	}
}

// TestResolveDevcontainerPoolPlanStaleGeneration covers the pointer-resolves-
// but-generation-is-gone case (a concurrent gc, or a pointer copied from
// another machine): falls back with a distinct notice naming the missing
// generation, gated on lockability exactly like the no-pointer case.
func TestResolveDevcontainerPoolPlanStaleGeneration(t *testing.T) {
	const genID = "claude-1.2.3-linux-arm64-glibc-a1b2c3d4"

	cases := []struct {
		name       string
		dispatcher bool
		genBin     bool
	}{
		{"dispatcher missing", false, true},
		{"generation bin missing", true, false},
		{"both missing", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, lockable := range []bool{true, false} {
				t.Run(map[bool]string{true: "lockable", false: "not lockable"}[lockable], func(t *testing.T) {
					in, root := devcontainerPoolPlanTestLayout(t, "claude", lockable)
					in.PointerPath = writePointerFile(t, root, genID+"\n")
					writePoolGeneration(t, in.poolPlanInput, genID, tc.dispatcher, tc.genBin)

					plan := resolveDevcontainerPoolPlan(in)

					if plan.mountRuntime {
						t.Fatalf("plan.mountRuntime = true, want fallback (stale generation)")
					}
					if lockable {
						if plan.notice == "" {
							t.Fatal("expected a stale-generation deprecation notice, got none")
						}
						if !strings.Contains(plan.notice, genID) ||
							!strings.Contains(plan.notice, "claude") ||
							!strings.Contains(plan.notice, "missing on disk") ||
							!strings.Contains(plan.notice, "wraptool harness sync") {
							t.Errorf("notice = %q, want it to name the generation id, the harness, and `wraptool harness sync`", plan.notice)
						}
					} else if plan.notice != "" {
						t.Errorf("notice = %q, want none for a non-pool-lockable harness (silent fallback)", plan.notice)
					}
				})
			}
		})
	}
}

// TestResolveDevcontainerPoolPlanNoticeIsSingleLine mirrors
// TestResolveGuixPoolPlanNoticeIsSingleLine: the notice must be one line so a
// caller printing it once (bringUpContainer, exactly one
// fmt.Fprintln(os.Stderr, plan.notice)) can never accidentally duplicate it.
func TestResolveDevcontainerPoolPlanNoticeIsSingleLine(t *testing.T) {
	check := func(t *testing.T, notice string) {
		t.Helper()
		if n := strings.Count(notice, "notice:"); n != 1 {
			t.Fatalf("notice contains %d occurrences of \"notice:\", want exactly 1: %q", n, notice)
		}
		if strings.Contains(notice, "\n") {
			t.Fatalf("notice must be a single line: %q", notice)
		}
	}

	t.Run("no active pointer", func(t *testing.T) {
		in, _ := devcontainerPoolPlanTestLayout(t, "claude", true)
		check(t, resolveDevcontainerPoolPlan(in).notice)
	})
	t.Run("stale generation", func(t *testing.T) {
		in, root := devcontainerPoolPlanTestLayout(t, "claude", true)
		in.PointerPath = writePointerFile(t, root, "claude-1.2.3-linux-arm64-glibc-a1b2c3d4\n")
		check(t, resolveDevcontainerPoolPlan(in).notice)
	})
}

// TestBuildDevcontainerUpArgsLegacyPoolPlan pins today's exact `devcontainer
// up` argv byte-for-byte — home bind only, no runtime mount and no
// --remote-env EVER (§12.2's Phase-4 ruling: the runtime mount is
// config-declared, not `up`-injected, full stop — there is no "active
// pointer" argv shape to grow into). Mirrors TestBuildGuixShellArgsLegacyPoolPlan's
// "byte-identical" intent, and this exact shape must survive unchanged
// regardless of plan liveness (buildDevcontainerUpArgs doesn't even take a
// plan anymore).
func TestBuildDevcontainerUpArgsLegacyPoolPlan(t *testing.T) {
	root := "/work/proj"
	poolHome := "/pool/home"

	got := buildDevcontainerUpArgs(root, poolHome, false)

	want := []string{
		"up", "--workspace-folder", root,
		"--mount", "type=bind,source=" + poolHome + ",target=" + devcontainerPoolHome,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("devcontainer up args =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildDevcontainerUpArgsHomeDeclared asserts the only other shape: a
// config that already declares the home mount suppresses `up`'s own --mount,
// with nothing else in the argv (still no runtime mount, still no --remote-env).
func TestBuildDevcontainerUpArgsHomeDeclared(t *testing.T) {
	root := "/work/proj"
	poolHome := "/pool/home"

	got := buildDevcontainerUpArgs(root, poolHome, true)

	want := []string{"up", "--workspace-folder", root}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("devcontainer up args =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildDevcontainerUpArgsToSlash pins §6.3: the home mount source is
// routed through filepath.ToSlash before being embedded in the --mount
// string. The expected value is computed via filepath.ToSlash of the SAME
// raw, Windows-shaped input rather than hardcoded forward slashes, so the
// assertion holds on every GOOS: on an actual Windows host (where
// filepath.Separator is '\\') this pins genuine backslash→forward-slash
// conversion — Docker Desktop expects forward slashes even for a Windows
// drive-letter source; on POSIX hosts filepath.ToSlash is a documented no-op,
// so here it only pins that buildDevcontainerUpArgs routes the source
// through it (real cross-OS conversion is exercised by Go's own stdlib
// tests, not reproducible here without a GOOS=windows build).
func TestBuildDevcontainerUpArgsToSlash(t *testing.T) {
	root := "/work/proj"
	rawHome := `C:\Users\dev\AppData\Local\wraptool\harness-pool\home`

	got := buildDevcontainerUpArgs(root, rawHome, false)

	want := []string{
		"up", "--workspace-folder", root,
		"--mount", "type=bind,source=" + filepath.ToSlash(rawHome) + ",target=" + devcontainerPoolHome,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("windows-shaped devcontainer up args =\n%#v\nwant\n%#v", got, want)
	}
	if runtime.GOOS == "windows" {
		for _, a := range got {
			if strings.Contains(a, `\`) {
				t.Errorf("arg %q still contains a backslash after ToSlash", a)
			}
		}
	}
}

// TestDevcontainerDeclaresMountBothTargets pins devcontainerDeclaresMount's
// generic (whole-file substring) behavior for both targets — still real
// behavior worth pinning even though production code only calls it for the
// HOME target now (devcontainerClassifyMount, mounts-array-aware and
// read-only-classifying, covers the runtime target instead — see
// TestDevcontainerClassifyMountAbsent/ReadOnly/ReadWrite).
func TestDevcontainerDeclaresMountBothTargets(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		dc := filepath.Join(dir, ".devcontainer")
		if err := os.MkdirAll(dc, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dc, "devcontainer.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("declares only home", func(t *testing.T) {
		dir := write(t, `{"mounts":["source=/x,target=`+devcontainerPoolHome+`,type=bind"]}`)
		if !devcontainerDeclaresMount(dir, devcontainerPoolHome) {
			t.Error("home target not detected")
		}
		if devcontainerDeclaresMount(dir, devcontainerPoolRuntime) {
			t.Error("runtime target falsely detected")
		}
	})

	t.Run("declares only runtime", func(t *testing.T) {
		dir := write(t, `{"mounts":["source=/x,target=`+devcontainerPoolRuntime+`,type=bind,readonly"]}`)
		if devcontainerDeclaresMount(dir, devcontainerPoolHome) {
			t.Error("home target falsely detected")
		}
		if !devcontainerDeclaresMount(dir, devcontainerPoolRuntime) {
			t.Error("runtime target not detected")
		}
	})

	t.Run("declares neither", func(t *testing.T) {
		dir := write(t, `{"image":"mcr.microsoft.com/devcontainers/base:ubuntu"}`)
		if devcontainerDeclaresMount(dir, devcontainerPoolHome) || devcontainerDeclaresMount(dir, devcontainerPoolRuntime) {
			t.Error("must not report a mount that is not declared")
		}
	})
}

// devcontainerWriteConfig is the shared "write a .devcontainer/devcontainer.json
// with this body under a fresh t.TempDir()" test helper — used by every test
// below that exercises the mounts-array/JSONC machinery.
func devcontainerWriteConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	dc := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(dc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dc, "devcontainer.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDevcontainerRuntimeMountDeclared covers item 4: the runtime target's
// own declared-check must be mounts-ARRAY-aware, not a whole-file substring —
// a PATH mention or comment containing devcontainerPoolRuntime elsewhere in
// the file must NOT be mistaken for a declared mount (the false positive the
// review flagged against the original whole-file substring approach).
// TestDevcontainerClassifyMountAbsent covers the "nothing at the target"
// class (item 1c): a PATH mention or a comment naming the target must not be
// mistaken for a declared mount — the false positive the review flagged
// against the original whole-file substring approach.
func TestDevcontainerClassifyMountAbsent(t *testing.T) {
	t.Run("PATH mention outside mounts", func(t *testing.T) {
		dir := devcontainerWriteConfig(t, `{"remoteEnv":{"SOME_VAR":"`+devcontainerPoolRuntimeBin+`:$PATH"},"mounts":["source=/x,target=`+devcontainerPoolHome+`,type=bind"]}`)
		if got := devcontainerClassifyMount(dir, devcontainerPoolRuntime); got != devcontainerMountAbsent {
			t.Errorf("classify = %v, want devcontainerMountAbsent (a PATH mention outside the mounts array is not a declared mount)", got)
		}
	})

	t.Run("comment mentioning the runtime path", func(t *testing.T) {
		dir := devcontainerWriteConfig(t, "{\n  // don't forget to mount "+devcontainerPoolRuntime+" someday\n  \"mounts\":[]\n}\n")
		if got := devcontainerClassifyMount(dir, devcontainerPoolRuntime); got != devcontainerMountAbsent {
			t.Errorf("classify = %v, want devcontainerMountAbsent (a comment mentioning the path is not a declared mount)", got)
		}
	})

	t.Run("no mounts array at all", func(t *testing.T) {
		dir := devcontainerWriteConfig(t, `{"image":"mcr.microsoft.com/devcontainers/base:ubuntu"}`)
		if got := devcontainerClassifyMount(dir, devcontainerPoolRuntime); got != devcontainerMountAbsent {
			t.Errorf("classify = %v, want devcontainerMountAbsent", got)
		}
	})
}

// TestDevcontainerClassifyMountReadOnly covers the ONLY shape item 1a
// recognizes as properly read-only: a string-form, type=bind entry carrying
// a bare `readonly` or `ro` token.
func TestDevcontainerClassifyMountReadOnly(t *testing.T) {
	t.Run("readonly token", func(t *testing.T) {
		dir := devcontainerWriteConfig(t, `{"mounts":["source=/pool/runtime,target=`+devcontainerPoolRuntime+`,type=bind,readonly"]}`)
		if got := devcontainerClassifyMount(dir, devcontainerPoolRuntime); got != devcontainerMountReadOnly {
			t.Errorf("classify = %v, want devcontainerMountReadOnly", got)
		}
	})

	t.Run("ro token", func(t *testing.T) {
		dir := devcontainerWriteConfig(t, `{"mounts":["source=/pool/runtime,target=`+devcontainerPoolRuntime+`,type=bind,ro"]}`)
		if got := devcontainerClassifyMount(dir, devcontainerPoolRuntime); got != devcontainerMountReadOnly {
			t.Errorf("classify = %v, want devcontainerMountReadOnly", got)
		}
	})
}

// TestDevcontainerClassifyMountReadWrite is the security-critical class
// (item 1b): every shape that LOOKS like it might be read-only but is NOT
// actually enforced as such by the devcontainer CLI.
//
// The object-form-with-"readonly":true case is the sharpest one: the review
// caught this test asserting the OPPOSITE (devcontainerRuntimeMountDeclared
// treating it as a fine, fully-declared mount) — the devcontainer CLI's
// object-mount schema has no read-only field at all, so that key is
// silently dropped and the mount comes up read-write regardless of the
// JSON's own claim. This is the inverted assertion the review demanded.
func TestDevcontainerClassifyMountReadWrite(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"object-form WITH readonly:true (CLI drops it — this is the dangerous case)",
			`{"mounts":[{"source":"/pool/runtime","target":"` + devcontainerPoolRuntime + `","type":"bind","readonly":true}]}`,
		},
		{
			"object-form without readonly at all",
			`{"mounts":[{"source":"/pool/runtime","target":"` + devcontainerPoolRuntime + `","type":"bind"}]}`,
		},
		{
			"string-form WITHOUT a readonly/ro token",
			`{"mounts":["source=/pool/runtime,target=` + devcontainerPoolRuntime + `,type=bind"]}`,
		},
		{
			"string-form, type=volume (non-bind, even with a readonly token)",
			`{"mounts":["source=poolvol,target=` + devcontainerPoolRuntime + `,type=volume,readonly"]}`,
		},
		{
			"object-form, type=volume",
			`{"mounts":[{"source":"poolvol","target":"` + devcontainerPoolRuntime + `","type":"volume"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := devcontainerWriteConfig(t, tc.body)
			if got := devcontainerClassifyMount(dir, devcontainerPoolRuntime); got != devcontainerMountReadWrite {
				t.Errorf("classify = %v, want devcontainerMountReadWrite (this shape is NOT enforced read-only by the devcontainer CLI)", got)
			}
		})
	}
}

// TestDevcontainerRuntimeMountSnippet pins the exact §12.3 mounts-array
// string entry `up` tells the developer to add: read-only, bind, target
// devcontainerPoolRuntime, and source = the CONCRETE ToSlash'd host path
// (never a ${localEnv}/${containerEnv} template — up already knows the
// right answer for the host it's running on).
func TestDevcontainerRuntimeMountSnippet(t *testing.T) {
	// A plain POSIX-shaped path here, deliberately — the ToSlash conversion
	// itself is pinned by TestBuildDevcontainerUpArgsToSlash, and a raw
	// backslash surviving ToSlash's documented POSIX no-op would otherwise
	// trip THIS function's own exotic-path guard (TestDevcontainerRuntimeMountSnippetExoticPath),
	// which is a different, deliberately separate behavior.
	rawRuntime := "/home/dev/.local/share/wraptool/harness-pool/runtime"
	got := devcontainerRuntimeMountSnippet(rawRuntime)
	want := "source=" + rawRuntime + ",target=" + devcontainerPoolRuntime + ",type=bind,readonly"
	if got != want {
		t.Fatalf("snippet = %q, want %q", got, want)
	}
	if strings.Contains(got, "${") {
		t.Fatalf("snippet = %q, must not be templated — a concrete host path only", got)
	}
}

// TestDevcontainerRuntimeMountSnippetExoticPath covers item 2: a pool
// runtime path containing a comma, quote, or backslash cannot be embedded
// verbatim in the illustrative (human-pasted) mounts snippet without
// corrupting it — a comma misparses as the mount string's own CSV field
// separator, a quote or backslash needs escaping this snippet doesn't do —
// the function must refuse and name the escaping problem instead of emitting
// a corrupt mount string. This guard mirrors
// cmd/init_devcontainer.go's poolRuntimeMountSourceUnsafeChars.
func TestDevcontainerRuntimeMountSnippetExoticPath(t *testing.T) {
	for _, exotic := range []string{
		`/pool/weird"quote/runtime`,
		`/pool/weird\backslash/runtime`,
		`/pool/weird,comma/runtime`,
	} {
		t.Run(exotic, func(t *testing.T) {
			got := devcontainerRuntimeMountSnippet(exotic)
			if strings.Contains(got, "source="+exotic) {
				t.Fatalf("snippet = %q, must not embed the exotic path verbatim (would corrupt the illustrative JSON)", got)
			}
			if !strings.Contains(got, "manual") && !strings.Contains(got, "escap") {
				t.Fatalf("snippet = %q, want a warning that the path needs manual escaping", got)
			}
		})
	}
}

// TestDevcontainerRuntimeNotReadOnlyNotice covers item 1's distinct,
// unmissable security notice for a declared-but-not-read-only runtime
// mount: it must be visibly different from the plain missing-mount notice
// (a "SECURITY" marker), name the harness, and still offer the correct
// read-only string-form snippet to fix it with.
func TestDevcontainerRuntimeNotReadOnlyNotice(t *testing.T) {
	notice := devcontainerRuntimeNotReadOnlyNotice("claude", "/pool/runtime")
	if !strings.Contains(notice, "SECURITY") {
		t.Errorf("notice = %q, want an unmissable SECURITY marker", notice)
	}
	if !strings.Contains(notice, "claude") {
		t.Errorf("notice = %q, want it to name the harness", notice)
	}
	if !strings.Contains(notice, ",readonly") || !strings.Contains(notice, "source=/pool/runtime") {
		t.Errorf("notice = %q, want the correct read-only string-form snippet", notice)
	}
	if notice == devcontainerRuntimeMissingMountNotice("claude", "/pool/runtime") {
		t.Error("the not-read-only notice must be distinct from the missing-mount notice")
	}
}

// TestDevcontainerRuntimeMissingMountNotice asserts the actionable notice
// names the harness, contains the exact mounts snippet (with ,readonly and
// this host's real path), and points at `wraptool init devcontainer` for
// fresh projects.
func TestDevcontainerRuntimeMissingMountNotice(t *testing.T) {
	notice := devcontainerRuntimeMissingMountNotice("claude", "/pool/runtime")
	if !strings.Contains(notice, "claude") {
		t.Errorf("notice = %q, want it to name the harness", notice)
	}
	if !strings.Contains(notice, ",readonly") {
		t.Errorf("notice = %q, want the ,readonly mounts snippet", notice)
	}
	if !strings.Contains(notice, "source=/pool/runtime") {
		t.Errorf("notice = %q, want the concrete host runtime path", notice)
	}
	if !strings.Contains(notice, "wraptool init devcontainer") {
		t.Errorf("notice = %q, want a pointer to the scaffolder for fresh projects", notice)
	}
}

// TestDevcontainerRuntimeDeclaredInfo is a light sanity pin: the info line
// names the harness and the runtime target, distinguishing it from the
// notice (no mounts snippet, no stderr — bringUpContainer prints this one to
// stdout).
func TestDevcontainerRuntimeDeclaredInfo(t *testing.T) {
	info := devcontainerRuntimeDeclaredInfo("claude")
	if !strings.Contains(info, "claude") || !strings.Contains(info, devcontainerPoolRuntime) {
		t.Fatalf("info = %q, want it to name the harness and the runtime target", info)
	}
}

// TestBuildDevcontainerShellEntry pins the exact §12.2-ruling entry-wrap
// string wraptool's own interactive shell uses when the pool plan is live:
// a bare (unquoted) $PATH, appended (never prepended) with the shellQuote'd
// dispatcher bin dir, handing off to a real login shell — mirrors
// buildContainerEntry's own PATH-append convention (cmd/guixcontainer.go).
func TestBuildDevcontainerShellEntry(t *testing.T) {
	got := buildDevcontainerShellEntry()
	want := "export PATH=$PATH:" + shellQuote(devcontainerPoolRuntimeBin) + "; exec bash -l"
	if got != want {
		t.Fatalf("entry = %q, want %q", got, want)
	}
	// Append-not-prepend, pinned the same way TestBuildContainerEntryAppendsPathDir
	// pins it for the guix twin.
	if !strings.HasPrefix(got, "export PATH=$PATH:") {
		t.Fatalf("entry = %q, must append after $PATH, never prepend", got)
	}
}

// TestBuildDevcontainerExecArgs asserts the exec argv shape: a dead plan gets
// a plain login shell (identical to the pre-pool-verification behavior); a
// live plan entry-wraps it via bash -lc with the exact quoted export line.
// This test only checks the ARGV SHAPE (that the entry string, whatever it
// is, lands as the "-lc" argument) — TestBuildDevcontainerShellEntry is the
// one that pins the entry string's own exact literal content/quoting.
func TestBuildDevcontainerExecArgs(t *testing.T) {
	root := "/work/proj"

	t.Run("no live plan", func(t *testing.T) {
		got := buildDevcontainerExecArgs(root, false)
		want := []string{"exec", "--workspace-folder", root, "bash", "-l"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("exec args =\n%#v\nwant\n%#v", got, want)
		}
	})

	t.Run("live plan", func(t *testing.T) {
		got := buildDevcontainerExecArgs(root, true)
		want := []string{"exec", "--workspace-folder", root, "bash", "-lc", buildDevcontainerShellEntry()}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("exec args =\n%#v\nwant\n%#v", got, want)
		}
	})
}

// TestDevcontainerPathExportHint pins the standalone export line printed in
// the attach-hint for a legacy-feature-provisioned container.
func TestDevcontainerPathExportHint(t *testing.T) {
	got := devcontainerPathExportHint()
	want := "export PATH=$PATH:" + shellQuote(devcontainerPoolRuntimeBin)
	if got != want {
		t.Fatalf("hint = %q, want %q", got, want)
	}
}

// TestDevcontainerDeclaredMountSourceMissing covers the §12.3 mixed-OS warning:
// a declared mount whose source does not exist on THIS host warns, one that
// does exist is silent, and a substitution form other than ${localEnv:...} is
// never guessed at.
func TestDevcontainerDeclaredMountSourceMissing(t *testing.T) {
	writeConfig := devcontainerWriteConfig

	t.Run("object-form mount - literal source missing", func(t *testing.T) {
		dir := writeConfig(t, `{"mounts":[{"source":"/definitely/does/not/exist","target":"`+devcontainerPoolRuntime+`","type":"bind"}]}`)
		warn := devcontainerDeclaredMountSourceMissing(dir, devcontainerPoolRuntime)
		if warn == "" || !strings.Contains(warn, devcontainerPoolRuntime) || !strings.Contains(warn, "does not exist") {
			t.Fatalf("warn = %q, want a missing-source notice naming the target", warn)
		}
	})

	t.Run("string-form mount - literal source exists", func(t *testing.T) {
		existing := t.TempDir()
		dir := writeConfig(t, `{"mounts":["source=`+filepath.ToSlash(existing)+`,target=`+devcontainerPoolHome+`,type=bind"]}`)
		if warn := devcontainerDeclaredMountSourceMissing(dir, devcontainerPoolHome); warn != "" {
			t.Fatalf("warn = %q, want none (source exists)", warn)
		}
	})

	t.Run("localEnv substitution resolves and exists", func(t *testing.T) {
		existing := t.TempDir()
		t.Setenv("WRAPTOOL_TEST_POOL_HOME", existing)
		dir := writeConfig(t, `{"mounts":[{"source":"${localEnv:WRAPTOOL_TEST_POOL_HOME}","target":"`+devcontainerPoolHome+`","type":"bind"}]}`)
		if warn := devcontainerDeclaredMountSourceMissing(dir, devcontainerPoolHome); warn != "" {
			t.Fatalf("warn = %q, want none (substituted source exists)", warn)
		}
	})

	t.Run("localEnv substitution resolves and is missing", func(t *testing.T) {
		t.Setenv("WRAPTOOL_TEST_POOL_HOME_MISSING", filepath.Join(t.TempDir(), "nope"))
		dir := writeConfig(t, `{"mounts":[{"source":"${localEnv:WRAPTOOL_TEST_POOL_HOME_MISSING}","target":"`+devcontainerPoolHome+`","type":"bind"}]}`)
		warn := devcontainerDeclaredMountSourceMissing(dir, devcontainerPoolHome)
		if warn == "" {
			t.Fatal("want a missing-source notice for a substituted-but-nonexistent source")
		}
	})

	t.Run("unrecognized substitution form is not guessed at", func(t *testing.T) {
		dir := writeConfig(t, `{"mounts":[{"source":"${containerWorkspaceFolder}/pool","target":"`+devcontainerPoolHome+`","type":"bind"}]}`)
		if warn := devcontainerDeclaredMountSourceMissing(dir, devcontainerPoolHome); warn != "" {
			t.Fatalf("warn = %q, want none (unrecognized substitution left alone)", warn)
		}
	})

	t.Run("no mount declared at all", func(t *testing.T) {
		dir := writeConfig(t, `{"image":"mcr.microsoft.com/devcontainers/base:ubuntu"}`)
		if warn := devcontainerDeclaredMountSourceMissing(dir, devcontainerPoolRuntime); warn != "" {
			t.Fatalf("warn = %q, want none (no mount declared)", warn)
		}
	})
}

// --- JSONC robustness (item 5): stripJSONC must not corrupt a "//" inside a
// JSON string, must strip /* */ block comments, and must tolerate a trailing
// comma before a closing } or ] ---

// TestStripJSONCPreservesURLsInsideStrings pins the bug the naive whole-line
// "// to EOL" regex had: a value containing "//" (a URL, this repo's own
// idiom — examples/devcontainer/README.md's WRAPTOOL_URL) must survive
// intact, not get truncated at the "//".
func TestStripJSONCPreservesURLsInsideStrings(t *testing.T) {
	in := `{
  // a real line comment, safely removed
  "containerEnv": { "WRAPTOOL_URL": "http://172.18.0.1:8717" },
  "documentationURL": "https://forge.snamellit.com/pti/wraptool"
}`
	out := stripJSONC(in)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("stripJSONC output is not valid JSON: %v\nstripped:\n%s", err, out)
	}
	env, _ := cfg["containerEnv"].(map[string]any)
	if env["WRAPTOOL_URL"] != "http://172.18.0.1:8717" {
		t.Fatalf("WRAPTOOL_URL = %v, want the URL intact (not truncated at //)", env["WRAPTOOL_URL"])
	}
	if cfg["documentationURL"] != "https://forge.snamellit.com/pti/wraptool" {
		t.Fatalf("documentationURL = %v, want it intact", cfg["documentationURL"])
	}
}

// TestStripJSONCRemovesBlockComments covers a /* */ comment, including one
// that spans what would otherwise look like a line comment inside it.
func TestStripJSONCRemovesBlockComments(t *testing.T) {
	in := `{
  /* block comment
     mentioning // not a real comment marker here */
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu"
}`
	out := stripJSONC(in)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("stripJSONC output is not valid JSON: %v\nstripped:\n%s", err, out)
	}
	if cfg["image"] != "mcr.microsoft.com/devcontainers/base:ubuntu" {
		t.Fatalf("image = %v, want it intact", cfg["image"])
	}
}

// TestStripJSONCRemovesTrailingComma covers a JSONC/JSON5-style trailing
// comma before a closing } or ], which encoding/json rejects outright.
func TestStripJSONCRemovesTrailingComma(t *testing.T) {
	in := `{
  "mounts": [
    "source=/a,target=/b,type=bind",
  ],
  "image": "mcr.microsoft.com/devcontainers/base:ubuntu",
}`
	out := stripJSONC(in)
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("stripJSONC output is not valid JSON: %v\nstripped:\n%s", err, out)
	}
	mounts, _ := cfg["mounts"].([]any)
	if len(mounts) != 1 {
		t.Fatalf("mounts = %v, want exactly one entry", cfg["mounts"])
	}
}

// TestStripJSONCOnRealRepoDevcontainerConfig feeds this repo's own
// .devcontainer/devcontainer.json (comments, tab indentation, a real mounts
// array) through stripJSONC + json.Unmarshal end to end, and confirms
// devcontainerDeclaredMountSource correctly finds its home mount and
// devcontainerClassifyMount reads its runtime mount as genuinely read-only —
// the §16 Phase 4 item 4 cutover (design/harness/unified-harness-pool.md
// §12.3) migrated this fixture from the legacy per-image feature (home mount
// only) to the wiring-only feature declaring both pool mounts.
func TestStripJSONCOnRealRepoDevcontainerConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".devcontainer", "devcontainer.json"))
	if err != nil {
		t.Skipf("repo .devcontainer/devcontainer.json not found: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(stripJSONC(string(data))), &cfg); err != nil {
		t.Fatalf("this repo's own devcontainer.json failed to parse after stripJSONC: %v", err)
	}
	if _, ok := cfg["mounts"]; !ok {
		t.Fatal("expected a mounts array in the repo's own devcontainer.json")
	}

	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if src, found := devcontainerDeclaredMountSource(dir, devcontainerPoolHome); !found || src == "" {
		t.Errorf("expected the repo's own config to declare a home mount; found=%v src=%q", found, src)
	}
	if got := devcontainerClassifyMount(dir, devcontainerPoolRuntime); got != devcontainerMountReadOnly {
		t.Errorf("classify = %v, want devcontainerMountReadOnly (the repo's own devcontainer.json declares the runtime mount as a string ending in ,readonly)", got)
	}
}

// TestStaleBinaryNotice covers staleBinaryNotice's four silent cases plus the
// one case that actually prints — never shelling to git (headSHA is a plain
// injected func).
func TestStaleBinaryNotice(t *testing.T) {
	wraptoolRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(wraptoolRoot, "go.mod"),
		[]byte("module github.com/pti/wraptool\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherRoot, "go.mod"),
		[]byte("module example.com/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	noModRoot := t.TempDir()

	sameSHA := func() (string, error) { return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil }
	diffSHA := func() (string, error) { return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil }
	errSHA := func() (string, error) { return "", errors.New("git not answerable") }

	const buildRev = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name     string
		root     string
		buildRev string
		headSHA  func() (string, error)
		want     bool // whether a non-empty notice is expected
	}{
		{"match: same sha prints nothing", wraptoolRoot, buildRev, sameSHA, false},
		{"mismatch: different sha prints a notice", wraptoolRoot, buildRev, diffSHA, true},
		{"not a wraptool checkout: different module", otherRoot, buildRev, diffSHA, false},
		{"not a wraptool checkout: no go.mod at all", noModRoot, buildRev, diffSHA, false},
		{"no revision: release build with unstamped VCS info", wraptoolRoot, "", diffSHA, false},
		{"git not answerable", wraptoolRoot, buildRev, errSHA, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := staleBinaryNotice(tt.root, tt.buildRev, tt.headSHA)
			if (got != "") != tt.want {
				t.Errorf("staleBinaryNotice(%q, %q) = %q, want non-empty=%v", tt.root, tt.buildRev, got, tt.want)
			}
		})
	}

	t.Run("notice names both short shas and the rebuild command", func(t *testing.T) {
		got := staleBinaryNotice(wraptoolRoot, buildRev, diffSHA)
		for _, want := range []string{shortSHA(buildRev), shortSHA("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), "go build -o wraptool ."} {
			if !strings.Contains(got, want) {
				t.Errorf("notice %q missing %q", got, want)
			}
		}
	})
}

// TestIsWraptoolModuleRoot pins the exact-match module-line detection: a
// module line that merely contains the wraptool import path (a fork, a
// vanity-suffixed module) must not match.
func TestIsWraptoolModuleRoot(t *testing.T) {
	cases := map[string]bool{
		"module github.com/pti/wraptool\n":            true,
		"module github.com/pti/wraptool\n\ngo 1.23\n": true,
		"module github.com/pti/wraptool/v2\n":         false,
		"module github.com/example/wraptool\n":        false,
		"// module github.com/pti/wraptool\n":         false,
		"":                                            false,
	}
	for content, want := range cases {
		dir := t.TempDir()
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := isWraptoolModuleRoot(dir); got != want {
			t.Errorf("isWraptoolModuleRoot(%q) = %v, want %v", content, got, want)
		}
	}
}
