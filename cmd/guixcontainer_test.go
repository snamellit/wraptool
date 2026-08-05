package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectRuntime(t *testing.T) {
	origHasGuix := hasGuix
	t.Cleanup(func() { hasGuix = origHasGuix })

	// root with manifest.scm present; a separate one without.
	withManifest := t.TempDir()
	if err := os.WriteFile(filepath.Join(withManifest, "manifest.scm"), []byte("()"), 0o600); err != nil {
		t.Fatal(err)
	}
	noManifest := t.TempDir()

	cases := []struct {
		name    string
		mode    string
		root    string
		guix    bool
		want    containerRuntime
		wantErr bool
	}{
		{"explicit guix", "guix", noManifest, false, runtimeGuix, false},
		{"explicit devcontainer", "devcontainer", withManifest, true, runtimeDevcontainer, false},
		{"unknown mode errors", "podman", withManifest, true, 0, true},
		{"auto: guix + manifest -> guix", "auto", withManifest, true, runtimeGuix, false},
		{"auto: guix + no manifest -> devcontainer", "auto", noManifest, true, runtimeDevcontainer, false},
		{"auto: no guix + manifest -> devcontainer", "auto", withManifest, false, runtimeDevcontainer, false},
		{"empty is auto", "", withManifest, true, runtimeGuix, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hasGuix = func() bool { return tc.guix }
			got, err := selectRuntime(tc.mode, tc.root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got runtime %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("runtime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBringUpGuixContainerReportsMissingGuixDuringWarmup(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.scm"), []byte("()"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	err := bringUpGuixContainer(root, "", "127.0.0.1:8717", "claude", false)
	if err == nil || !strings.Contains(err.Error(), "realizing guix container profile") {
		t.Fatalf("missing guix warm-up error not reported: %v", err)
	}
}

func TestValidateConfigOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(inside, []byte("mcp: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigOutsideWorkspace(inside, root); err == nil {
		t.Fatal("config inside workspace was accepted")
	}

	outside := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(outside, []byte("mcp: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigOutsideWorkspace(outside, root); err != nil {
		t.Fatalf("config outside workspace rejected: %v", err)
	}

	alias := filepath.Join(t.TempDir(), "config-link.yaml")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigOutsideWorkspace(alias, root); err == nil {
		t.Fatal("symlinked config inside workspace was accepted")
	}
}

func TestValidateAdminTokenOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "admin-token")
	if err := os.WriteFile(inside, []byte("token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(inside, "admin auth token", root); err == nil {
		t.Fatal("admin token inside workspace was accepted")
	}

	alias := filepath.Join(t.TempDir(), "admin-token-link")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(alias, "admin auth token", root); err == nil {
		t.Fatal("symlinked admin token inside workspace was accepted")
	}

	missing := filepath.Join(root, "new-admin-token")
	if err := validatePathOutsideWorkspace(missing, "admin auth token", root); err == nil {
		t.Fatal("missing admin token inside workspace was accepted")
	}
	danglingAlias := filepath.Join(t.TempDir(), "new-admin-token-link")
	if err := os.Symlink(missing, danglingAlias); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(danglingAlias, "admin auth token", root); err == nil {
		t.Fatal("dangling symlink to admin token inside workspace was accepted")
	}

	outside := t.TempDir()
	existing := filepath.Join(outside, "admin-token")
	if err := os.WriteFile(existing, []byte("token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		existing,
		filepath.Join(outside, "missing-admin-token"),
	} {
		if err := validatePathOutsideWorkspace(path, "admin auth token", root); err != nil {
			t.Fatalf("admin token outside workspace rejected: %v", err)
		}
	}

	outsideAlias := filepath.Join(t.TempDir(), "admin-token-link")
	if err := os.Symlink(existing, outsideAlias); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(outsideAlias, "admin auth token", root); err != nil {
		t.Fatalf("symlinked admin token outside workspace rejected: %v", err)
	}
	outsideParentAlias := filepath.Join(t.TempDir(), "tokens")
	if err := os.Symlink(outside, outsideParentAlias); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(filepath.Join(outsideParentAlias, "generated-admin-token"), "admin auth token", root); err != nil {
		t.Fatalf("missing admin token under symlinked external parent rejected: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := validatePathOutsideWorkspace("~/.config/wraptool/admin-token", "admin auth token", root); err != nil {
		t.Fatalf("tilde-expanded admin token outside workspace rejected: %v", err)
	}
}

// --- guix runtime/ mount (design/harness/unified-harness-pool.md §12.1) ---

// guixPoolPlanTestLayout builds a t.TempDir()-rooted pool layout (runtime/
// dir + bin/ + generations/, legacy bin/ + lib/) and returns a
// guixPoolPlanInput seeded from it, so tests can flip pointer content and
// file presence without touching a real $XDG_DATA_HOME or exec'ing guix.
func guixPoolPlanTestLayout(t *testing.T, harness string, poolLockable bool) (in guixPoolPlanInput, root string) {
	t.Helper()
	root = t.TempDir()
	in = guixPoolPlanInput{
		Harness:               harness,
		PoolLockable:          poolLockable,
		RuntimeDir:            filepath.Join(root, "runtime"),
		RuntimeBinDir:         filepath.Join(root, "runtime", "bin"),
		RuntimeGenerationsDir: filepath.Join(root, "runtime", "generations"),
		LegacyBin:             filepath.Join(root, "bin"),
		LegacyLib:             filepath.Join(root, "lib"),
	}
	return in, root
}

// writePointerFile writes content at <root>/active.gen and returns its path.
func writePointerFile(t *testing.T, root, content string) string {
	t.Helper()
	p := filepath.Join(root, "active.gen")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeGeneration creates the two files resolveGuixPoolPlan checks for an
// active pointer to genID: the dispatcher (runtime/bin/<harness>) and the
// generation's own entrypoint (runtime/generations/<genID>/bin/<harness>).
func writeGeneration(t *testing.T, in guixPoolPlanInput, genID string, dispatcher, genBin bool) {
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

// TestResolveGuixPoolPlanNoActivePointer covers every "no active pointer"
// case (§12.1 point 2/3): missing pointer file, unreadable pointer path, an
// empty or whitespace/newline-only pointer content, and an empty pointerPath
// (simulating a HostPlatformKey resolution failure). All fall back to the
// legacy plan; the deprecation notice is present for a pool-lockable harness
// and silent for one that is not (item 1: a harness with no runtime/ path
// gets no notice promising a fix that can't exist for it).
func TestResolveGuixPoolPlanNoActivePointer(t *testing.T) {
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
		{"empty pointerPath (host platform key unresolved)", func(t *testing.T, root string) string { return "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, lockable := range []bool{true, false} {
				t.Run(map[bool]string{true: "lockable", false: "not lockable"}[lockable], func(t *testing.T) {
					in, root := guixPoolPlanTestLayout(t, "claude", lockable)
					in.PointerPath = tc.pointerPath(t, root)

					plan := resolveGuixPoolPlan(in)

					if plan.active {
						t.Fatalf("plan.active = true, want legacy fallback")
					}
					wantExposes := []string{
						"--expose=" + in.LegacyBin + "=" + in.LegacyBin,
						"--expose=" + in.LegacyLib + "=" + in.LegacyLib,
					}
					if !reflect.DeepEqual(plan.exposes, wantExposes) {
						t.Errorf("exposes = %v, want %v", plan.exposes, wantExposes)
					}
					if !reflect.DeepEqual(plan.pathDirs, []string{in.LegacyBin}) {
						t.Errorf("pathDirs = %v, want %v", plan.pathDirs, []string{in.LegacyBin})
					}
					for _, e := range plan.exposes {
						if strings.Contains(e, in.RuntimeDir) {
							t.Errorf("legacy-fallback exposes leaked runtime dir: %v", plan.exposes)
						}
					}

					if lockable {
						if plan.notice == "" {
							t.Fatal("expected a legacy-fallback deprecation notice for a pool-lockable harness, got none")
						}
						if !strings.Contains(plan.notice, "claude") ||
							!strings.Contains(plan.notice, "wraptool harness install claude") ||
							!strings.Contains(plan.notice, "wraptool harness sync") {
							t.Errorf("notice = %q, want it to name the harness and both the install and sync fix commands", plan.notice)
						}
					} else if plan.notice != "" {
						t.Errorf("notice = %q, want none for a non-pool-lockable harness (silent legacy fallback)", plan.notice)
					}
				})
			}
		})
	}
}

// TestResolveGuixPoolPlanActivePointer asserts the happy path: a pointer
// whose generation id resolves to real dispatcher + generation-bin files on
// disk activates the runtime/ plan, notice-free, regardless of lockability
// (an active pointer can only exist for a lockable harness in practice, but
// the plan itself doesn't need to re-derive that). wave2-integration-fixes
// item 3 (mixed-state Phase 2, §12.1 RULING): active mode mounts BOTH
// runtime/ AND legacy bin/+lib/ — never legacy-exclusive-of-runtime, but
// also never runtime-exclusive-of-legacy — with PATH carrying runtime/bin
// FIRST (this harness's freshly activated generation wins) then legacy bin
// (every other harness/hand-dropped tool stays reachable).
func TestResolveGuixPoolPlanActivePointer(t *testing.T) {
	in, root := guixPoolPlanTestLayout(t, "claude", true)
	in.PointerPath = writePointerFile(t, root, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4\n")
	writeGeneration(t, in, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4", true, true)

	plan := resolveGuixPoolPlan(in)

	if !plan.active {
		t.Fatalf("plan.active = false, want true")
	}
	wantExposes := []string{
		"--expose=" + in.RuntimeDir + "=" + in.RuntimeDir,
		"--expose=" + in.LegacyBin + "=" + in.LegacyBin,
		"--expose=" + in.LegacyLib + "=" + in.LegacyLib,
	}
	if !reflect.DeepEqual(plan.exposes, wantExposes) {
		t.Errorf("exposes = %v, want %v (both mounts, mixed-state Phase 2)", plan.exposes, wantExposes)
	}
	wantPathDirs := []string{in.RuntimeBinDir, in.LegacyBin}
	if !reflect.DeepEqual(plan.pathDirs, wantPathDirs) {
		t.Errorf("pathDirs = %v, want %v (runtime/bin FIRST, then legacy bin)", plan.pathDirs, wantPathDirs)
	}
	if plan.notice != "" {
		t.Errorf("notice = %q, want none", plan.notice)
	}
}

// TestResolveGuixPoolPlanStaleGeneration covers item 3: the pointer resolves
// to a real generation id, but the generation is gone from disk (dispatcher
// missing, generation-bin missing, or both) — a concurrent gc, or a pointer
// copied from another machine. Falls back to legacy with a distinct notice
// naming the missing generation, again gated on lockability.
func TestResolveGuixPoolPlanStaleGeneration(t *testing.T) {
	const genID = "claude-1.2.3-linux-amd64-glibc-a1b2c3d4"

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
					in, root := guixPoolPlanTestLayout(t, "claude", lockable)
					in.PointerPath = writePointerFile(t, root, genID+"\n")
					writeGeneration(t, in, genID, tc.dispatcher, tc.genBin)

					plan := resolveGuixPoolPlan(in)

					if plan.active {
						t.Fatalf("plan.active = true, want legacy fallback (stale generation)")
					}
					wantExposes := []string{
						"--expose=" + in.LegacyBin + "=" + in.LegacyBin,
						"--expose=" + in.LegacyLib + "=" + in.LegacyLib,
					}
					if !reflect.DeepEqual(plan.exposes, wantExposes) {
						t.Errorf("exposes = %v, want %v", plan.exposes, wantExposes)
					}
					if !reflect.DeepEqual(plan.pathDirs, []string{in.LegacyBin}) {
						t.Errorf("pathDirs = %v, want %v", plan.pathDirs, []string{in.LegacyBin})
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
						t.Errorf("notice = %q, want none for a non-pool-lockable harness (silent legacy fallback)", plan.notice)
					}
				})
			}
		})
	}
}

// TestResolveGuixPoolPlanNoticeIsSingleLine pins each notice as one line
// containing exactly one occurrence of the notice marker — the actual
// "printed once, not per mount" behavior lives in bringUpGuixContainer, which
// calls resolveGuixPoolPlan exactly once and prints plan.notice with exactly
// one fmt.Fprintln(os.Stderr, ...) gated on a single non-empty check (never a
// loop over exposes/mounts), so a single-line, single-occurrence notice here
// is what guarantees no caller can accidentally duplicate it per mount.
func TestResolveGuixPoolPlanNoticeIsSingleLine(t *testing.T) {
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
		in, _ := guixPoolPlanTestLayout(t, "claude", true)
		check(t, resolveGuixPoolPlan(in).notice)
	})
	t.Run("stale generation", func(t *testing.T) {
		in, root := guixPoolPlanTestLayout(t, "claude", true)
		in.PointerPath = writePointerFile(t, root, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4\n")
		check(t, resolveGuixPoolPlan(in).notice)
	})
}

// TestBuildContainerEntryAppendsPathDir pins §12.1's append-not-prepend
// invariant: every pathDirs entry must land strictly after "$PATH:" in the
// exported PATH assignment, in argument order, never before "$PATH" (which
// would let a pool binary shadow a project toolchain binary — git/go/bash).
func TestBuildContainerEntryAppendsPathDir(t *testing.T) {
	entry := buildContainerEntry("/work/proj", "/pool/home", []string{"/pool/runtime/bin", "/pool/bin"})

	pathAssign := "export PATH=$PATH:" + shellQuote("/pool/runtime/bin") + ":" + shellQuote("/pool/bin")
	idx := strings.Index(entry, pathAssign)
	if idx < 0 {
		t.Fatalf("entry does not append pathDirs, in order, after $PATH: entry=%q", entry)
	}
	// The append form is "$PATH:<dir>..." — assert a directory never appears
	// as a prefix ("<dir>:$PATH") anywhere in the entry.
	prependForm := "export PATH=" + shellQuote("/pool/runtime/bin") + ":$PATH"
	if strings.Contains(entry, prependForm) {
		t.Fatalf("entry prepends pathDirs instead of appending: entry=%q", entry)
	}
}

// TestBuildGuixShellArgsLegacyPoolPlan pins today's exact legacy bin/+lib/
// mount argv byte-for-byte — no active pointer means the container's argv
// must be identical to the pre-runtime-mount behavior, per the brief's
// "byte-identical" requirement.
func TestBuildGuixShellArgsLegacyPoolPlan(t *testing.T) {
	root := "/work/proj"
	manifest := "/work/proj/manifest.scm"
	poolHome := "/pool/home"
	poolBin := "/pool/bin"
	poolLib := "/pool/lib"

	plan := resolveGuixPoolPlan(guixPoolPlanInput{
		Harness:               "claude",
		PoolLockable:          true,
		RuntimeDir:            "/pool/runtime",
		RuntimeBinDir:         "/pool/runtime/bin",
		RuntimeGenerationsDir: "/pool/runtime/generations",
		LegacyBin:             poolBin,
		LegacyLib:             poolLib,
	})
	entry := buildContainerEntry(root, poolHome, plan.pathDirs)
	got := buildGuixShellArgs(root, manifest, poolHome, plan, entry)

	want := []string{
		"shell", "--container", "--network",
		"--emulate-fhs",
		"--share=" + root + "=" + root,
		"--expose=" + poolBin + "=" + poolBin,
		"--expose=" + poolLib + "=" + poolLib,
		"--share=" + poolHome + "=" + poolHome,
		"--preserve=^WRAPTOOL_",
		"--preserve=^COLORTERM$",
		"-m", manifest,
		"--", "bash", "-lc", entry,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy guix shell args =\n%#v\nwant\n%#v", got, want)
	}
	wantEntry := "export WRAPTOOL_CONTAINER=1" +
		"; export TERM=xterm-256color" +
		"; export PATH=$PATH:" + shellQuote(poolBin) +
		"; export HOME=" + shellQuote(poolHome) +
		"; cd " + shellQuote(root) + "; exec bash -l"
	if entry != wantEntry {
		t.Fatalf("legacy entry = %q, want %q", entry, wantEntry)
	}
}

// TestBuildGuixShellArgsActivePointerPlan asserts the §12.1 pointer-active
// mount, WIDENED by wave2-integration-fixes item 3 (mixed-state Phase 2):
// runtime/ read-only at the same host path, AND legacy bin/+lib/ read-only
// exposes too (never dropped just because a pointer is active) — PATH
// appended with runtime/bin FIRST, then legacy bin, both strictly after
// $PATH. home/ stays shared exactly as in the legacy case.
func TestBuildGuixShellArgsActivePointerPlan(t *testing.T) {
	root := "/work/proj"
	manifest := "/work/proj/manifest.scm"
	poolHome := "/pool/home"
	poolBin := "/pool/bin"
	poolLib := "/pool/lib"

	in, tmpRoot := guixPoolPlanTestLayout(t, "claude", true)
	in.LegacyBin, in.LegacyLib = poolBin, poolLib // exercise distinct legacy-vs-tmp paths
	const genID = "claude-1.2.3-linux-amd64-glibc-a1b2c3d4"
	in.PointerPath = writePointerFile(t, tmpRoot, genID+"\n")
	writeGeneration(t, in, genID, true, true)

	plan := resolveGuixPoolPlan(in)
	if !plan.active {
		t.Fatalf("plan.active = false, want true (setup bug)")
	}
	entry := buildContainerEntry(root, poolHome, plan.pathDirs)
	got := buildGuixShellArgs(root, manifest, poolHome, plan, entry)

	want := []string{
		"shell", "--container", "--network",
		"--emulate-fhs",
		"--share=" + root + "=" + root,
		"--expose=" + in.RuntimeDir + "=" + in.RuntimeDir,
		"--expose=" + poolBin + "=" + poolBin,
		"--expose=" + poolLib + "=" + poolLib,
		"--share=" + poolHome + "=" + poolHome,
		"--preserve=^WRAPTOOL_",
		"--preserve=^COLORTERM$",
		"-m", manifest,
		"--", "bash", "-lc", entry,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active-pointer guix shell args =\n%#v\nwant\n%#v", got, want)
	}
	// PATH order: runtime/bin strictly before legacy bin, both strictly
	// after $PATH.
	wantPathAssign := "export PATH=$PATH:" + shellQuote(in.RuntimeBinDir) + ":" + shellQuote(poolBin)
	if !strings.Contains(entry, wantPathAssign) {
		t.Fatalf("entry PATH assignment = %q, want it to contain %q (runtime/bin before legacy bin, both after $PATH)", entry, wantPathAssign)
	}
	// home/ share is present and identical to the legacy-mode assertion above.
	wantHomeShare := "--share=" + poolHome + "=" + poolHome
	found := false
	for _, a := range got {
		if a == wantHomeShare {
			found = true
		}
	}
	if !found {
		t.Fatalf("home/ share missing or altered: %v", got)
	}
}
