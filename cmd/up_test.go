package cmd

import (
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
		shell:      "never",
	})
	if err == nil || !strings.Contains(err.Error(), "admin auth token resolves to") {
		t.Fatalf("disabled admin token inside workspace was not rejected: %v", err)
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
	in = devcontainerPoolPlanInput{
		Harness:               harnessName,
		PoolLockable:          poolLockable,
		RuntimeDir:            filepath.Join(root, "runtime"),
		RuntimeBinDir:         filepath.Join(root, "runtime", "bin"),
		RuntimeGenerationsDir: filepath.Join(root, "runtime", "generations"),
	}
	return in, root
}

// writeDevcontainerGeneration creates the two files resolveDevcontainerPoolPlan
// checks for an active pointer to genID: the dispatcher (runtime/bin/<harness>)
// and the generation's own entrypoint (runtime/generations/<genID>/bin/<harness>).
// Mirrors guixcontainer_test.go's writeGeneration.
func writeDevcontainerGeneration(t *testing.T, in devcontainerPoolPlanInput, genID string, dispatcher, genBin bool) {
	t.Helper()
	if dispatcher {
		if err := os.MkdirAll(in.RuntimeBinDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(in.RuntimeBinDir, in.Harness), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if genBin {
		dir := filepath.Join(in.RuntimeGenerationsDir, genID, "bin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, in.Harness), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
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
	writeDevcontainerGeneration(t, in, "claude-1.2.3-linux-arm64-glibc-a1b2c3d4", true, true)

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
					writeDevcontainerGeneration(t, in, genID, tc.dispatcher, tc.genBin)

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

// TestBuildDevcontainerUpArgsLegacyPoolPlan pins today's exact pre-runtime-mount
// `devcontainer up` argv byte-for-byte — no active pointer, no declared mounts —
// so the refactor that introduced buildDevcontainerUpArgs cannot silently
// change the common-case invocation. Mirrors
// TestBuildGuixShellArgsLegacyPoolPlan's "byte-identical" intent.
func TestBuildDevcontainerUpArgsLegacyPoolPlan(t *testing.T) {
	root := "/work/proj"
	poolHome := "/pool/home"

	got := buildDevcontainerUpArgs(root, poolHome, false, false, devcontainerPoolPlan{})

	want := []string{
		"up", "--workspace-folder", root,
		"--mount", "type=bind,source=" + poolHome + ",target=" + devcontainerPoolHome,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy devcontainer up args =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildDevcontainerUpArgsActivePointerPlan asserts the §12.2 active-pointer
// shape: home bind (unchanged), runtime bind read-only at devcontainerPoolRuntime,
// and a --remote-env PATH append using the fixed dispatcher bin dir.
func TestBuildDevcontainerUpArgsActivePointerPlan(t *testing.T) {
	root := "/work/proj"
	poolHome := "/pool/home"
	plan := devcontainerPoolPlan{mountRuntime: true, runtimeDir: "/pool/runtime"}

	got := buildDevcontainerUpArgs(root, poolHome, false, false, plan)

	want := []string{
		"up", "--workspace-folder", root,
		"--mount", "type=bind,source=" + poolHome + ",target=" + devcontainerPoolHome,
		"--mount", "type=bind,source=/pool/runtime,target=" + devcontainerPoolRuntime + ",readonly",
		"--remote-env", "PATH=" + devcontainerPoolPathAppend,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active-pointer devcontainer up args =\n%#v\nwant\n%#v", got, want)
	}
	if !strings.HasSuffix(devcontainerPoolPathAppend, devcontainerPoolRuntimeBin) ||
		!strings.HasPrefix(devcontainerPoolPathAppend, "${containerEnv:PATH}:") {
		t.Fatalf("devcontainerPoolPathAppend = %q, want existing PATH APPENDED with the dispatcher dir (never prepended)", devcontainerPoolPathAppend)
	}
}

// TestBuildDevcontainerUpArgsSkipsDeclaredMounts covers item 3: a
// devcontainer.json already declaring a mount at either target independently
// suppresses only that target's --mount, never the other's, and never the
// PATH remote-env (which is orthogonal to who wrote the mount).
func TestBuildDevcontainerUpArgsSkipsDeclaredMounts(t *testing.T) {
	root := "/work/proj"
	poolHome := "/pool/home"
	plan := devcontainerPoolPlan{mountRuntime: true, runtimeDir: "/pool/runtime"}

	t.Run("home declared", func(t *testing.T) {
		got := buildDevcontainerUpArgs(root, poolHome, true, false, plan)
		want := []string{
			"up", "--workspace-folder", root,
			"--mount", "type=bind,source=/pool/runtime,target=" + devcontainerPoolRuntime + ",readonly",
			"--remote-env", "PATH=" + devcontainerPoolPathAppend,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("args =\n%#v\nwant\n%#v", got, want)
		}
	})

	t.Run("runtime declared", func(t *testing.T) {
		got := buildDevcontainerUpArgs(root, poolHome, false, true, plan)
		want := []string{
			"up", "--workspace-folder", root,
			"--mount", "type=bind,source=" + poolHome + ",target=" + devcontainerPoolHome,
			"--remote-env", "PATH=" + devcontainerPoolPathAppend,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("args =\n%#v\nwant\n%#v", got, want)
		}
	})

	t.Run("both declared", func(t *testing.T) {
		got := buildDevcontainerUpArgs(root, poolHome, true, true, plan)
		want := []string{
			"up", "--workspace-folder", root,
			"--remote-env", "PATH=" + devcontainerPoolPathAppend,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("args =\n%#v\nwant\n%#v", got, want)
		}
	})
}

// TestBuildDevcontainerUpArgsToSlash pins §6.3: both mount sources are routed
// through filepath.ToSlash before being embedded in the --mount string. The
// expected value is computed via filepath.ToSlash of the SAME raw,
// Windows-shaped input rather than hardcoded forward slashes, so the
// assertion holds on every GOOS: on an actual Windows host (where
// filepath.Separator is '\\') this pins genuine backslash→forward-slash
// conversion — Docker Desktop expects forward slashes even for a Windows
// drive-letter source; on POSIX hosts filepath.ToSlash is a documented no-op,
// so here it only pins that buildDevcontainerUpArgs routes both sources
// through it (real cross-OS conversion is exercised by Go's own stdlib tests,
// not reproducible here without a GOOS=windows build).
func TestBuildDevcontainerUpArgsToSlash(t *testing.T) {
	root := "/work/proj"
	rawHome := `C:\Users\dev\AppData\Local\wraptool\harness-pool\home`
	rawRuntime := `C:\Users\dev\AppData\Local\wraptool\harness-pool\runtime`
	plan := devcontainerPoolPlan{mountRuntime: true, runtimeDir: rawRuntime}

	got := buildDevcontainerUpArgs(root, rawHome, false, false, plan)

	want := []string{
		"up", "--workspace-folder", root,
		"--mount", "type=bind,source=" + filepath.ToSlash(rawHome) + ",target=" + devcontainerPoolHome,
		"--mount", "type=bind,source=" + filepath.ToSlash(rawRuntime) + ",target=" + devcontainerPoolRuntime + ",readonly",
		"--remote-env", "PATH=" + devcontainerPoolPathAppend,
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

// TestDevcontainerDeclaresMountBothTargets extends
// TestDevcontainerDeclaresPoolMount to the generic devcontainerDeclaresMount
// helper: each target is detected independently.
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

// TestDevcontainerDeclaredMountSourceMissing covers the §12.3 mixed-OS warning:
// a declared mount whose source does not exist on THIS host warns, one that
// does exist is silent, and a substitution form other than ${localEnv:...} is
// never guessed at.
func TestDevcontainerDeclaredMountSourceMissing(t *testing.T) {
	writeConfig := func(t *testing.T, body string) string {
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
