package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrg/xdg"

	"github.com/pti/wraptool/internal/pool"
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

// withFakePoolRoot redirects pool.Root() (via $XDG_DATA_HOME + xdg.Reload())
// to a fresh t.TempDir() for the duration of one test, restoring the real
// value on cleanup. xdg.DataHome is a process-wide package var, not a
// per-call parameter, so a test using this must NOT run t.Parallel() with
// anything that itself resolves pool.Root() mid-swap — every other test in
// this file avoids the real pool.Root() entirely (guixPoolPlanTestLayout's
// own doc comment) specifically to sidestep this; the two ensureHarnessPool
// tests below are the sole, deliberate exception, and stay serial.
//
// Deliberately os.Setenv/os.Unsetenv, not t.Setenv — mirrors
// cmd/server_test.go's withIsolatedRuntime shape. t.Setenv registers its
// OWN env-restoring cleanup at call time; a SEPARATE t.Cleanup(xdg.Reload)
// registered after it would then run BEFORE that restore (t.Cleanup is
// LIFO), reloading xdg against the still-swapped env and leaving
// xdg.DataHome pointed at this test's (about to be deleted) temp dir for
// the rest of the process. One cleanup that restores the env and THEN
// reloads, in that order, is the only ordering that actually restores
// global state.
func withFakePoolRoot(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	old, had := os.LookupEnv("XDG_DATA_HOME")
	if err := os.Setenv("XDG_DATA_HOME", root); err != nil {
		t.Fatal(err)
	}
	xdg.Reload()
	t.Cleanup(func() {
		if had {
			os.Setenv("XDG_DATA_HOME", old)
		} else {
			os.Unsetenv("XDG_DATA_HOME")
		}
		xdg.Reload()
	})
	return root
}

// TestWithFakePoolRootRestoresGlobalStateOnCleanup is the leak probe for
// withFakePoolRoot's own cleanup-ordering fix: pool.Root() (backed by the
// process-wide xdg.DataHome) must be back to its pre-swap value once a test
// using the helper has finished, not left pointing at that test's (by then
// possibly already-deleted) temp dir.
func TestWithFakePoolRootRestoresGlobalStateOnCleanup(t *testing.T) {
	before := pool.Root()

	t.Run("inner", func(t *testing.T) {
		fake := withFakePoolRoot(t)
		if got := pool.Root(); !strings.HasPrefix(got, fake) {
			t.Fatalf("pool.Root() = %q inside the swap, want it under %q", got, fake)
		}
	})

	if got := pool.Root(); got != before {
		t.Errorf("pool.Root() after the helper's scope = %q, want the original %q (leaked global xdg.DataHome state)", got, before)
	}
}

// TestEnsureHarnessPoolAlwaysCreatesRuntimeDir is this round's sibling
// review finding: a scaffolded devcontainer config binds a host mount whose
// SOURCE is the pool's runtime/ directory; if ensureHarnessPool never
// creates it, rootful Docker creates it root-owned on the first
// `devcontainer up`, and every later `wraptool harness install` then fails
// EACCES writing runtime/generations/ underneath it. runtime/ must exist,
// developer-owned, the moment ensureHarnessPool first runs against a fresh
// pool — regardless of the legacy-removed marker (see the sibling test
// below), which gates ONLY bin/+lib/.
func TestEnsureHarnessPoolAlwaysCreatesRuntimeDir(t *testing.T) {
	withFakePoolRoot(t)

	poolRoot, bin, lib, home, err := ensureHarnessPool()
	if err != nil {
		t.Fatalf("ensureHarnessPool: %v", err)
	}
	runtimeDir := filepath.Join(poolRoot, "runtime")
	info, err := os.Stat(runtimeDir)
	if err != nil {
		t.Fatalf("expected runtime/ to exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("runtime/ exists but is not a directory")
	}
	// "developer-owned": os.MkdirAll creates it under the test process's own
	// uid — proven simply by having created it via this call at all, with
	// no separate privilege escalation in play; a portable check that makes
	// no root/non-root environment assumption.
	for _, d := range []string{bin, lib, home} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("expected %s to exist: %v", d, err)
		}
	}
}

// TestEnsureHarnessPoolMarkerGatesOnlyBinLib confirms the legacy-removed
// marker (§16 Phase 4 item 5) gates bin/+lib/ ONLY: with the marker
// present, ensureHarnessPool must still create home/ and runtime/ — the
// runtime/ requirement (TestEnsureHarnessPoolAlwaysCreatesRuntimeDir) is
// unrelated to whether the pool has been gc'd.
func TestEnsureHarnessPoolMarkerGatesOnlyBinLib(t *testing.T) {
	withFakePoolRoot(t)

	poolRoot := harnessPoolDir()
	if err := pool.WriteFileAtomic(pool.LegacyRemovedMarkerFileAt(poolRoot), []byte("2026-01-01T00:00:00Z\nremoved by `wraptool harness gc --legacy`\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, bin, lib, home, err := ensureHarnessPool()
	if err != nil {
		t.Fatalf("ensureHarnessPool: %v", err)
	}
	for _, d := range []string{bin, lib} {
		if _, statErr := os.Stat(d); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to NOT be created (legacy-removed marker present), stat err = %v", d, statErr)
		}
	}
	for _, d := range []string{home, filepath.Join(poolRoot, "runtime")} {
		if _, statErr := os.Stat(d); statErr != nil {
			t.Errorf("expected %s to still be created despite the marker: %v", d, statErr)
		}
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
// dir + bin/ + generations/, legacy bin/ + lib/ both actually created on
// disk — legacyPoolPresent's fileExists check needs them to be real
// directories, not just path strings, so every test built from this helper
// starts in the "legacy present" state unless it removes them itself, e.g.
// os.RemoveAll(in.LegacyBin)/os.RemoveAll(in.LegacyLib), §16 Phase 4 item 5)
// and returns a guixPoolPlanInput seeded from it, so tests can flip pointer
// content and file presence without touching a real $XDG_DATA_HOME or
// exec'ing guix.
func guixPoolPlanTestLayout(t *testing.T, harness string, poolLockable bool) (in guixPoolPlanInput, root string) {
	t.Helper()
	root = t.TempDir()
	in = guixPoolPlanInput{
		poolPlanInput: poolPlanInput{
			Harness:               harness,
			PoolLockable:          poolLockable,
			RuntimeDir:            filepath.Join(root, "runtime"),
			RuntimeBinDir:         filepath.Join(root, "runtime", "bin"),
			RuntimeGenerationsDir: filepath.Join(root, "runtime", "generations"),
		},
		LegacyBin: filepath.Join(root, "bin"),
		LegacyLib: filepath.Join(root, "lib"),
	}
	if err := os.MkdirAll(in.LegacyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(in.LegacyLib, 0o755); err != nil {
		t.Fatal(err)
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

// writePoolGeneration creates the two files resolveGuixPoolPlan and
// resolveDevcontainerPoolPlan (cmd/up_test.go) both check for an active
// pointer to genID: the dispatcher (RuntimeBinDir/<harness>) and the
// generation's own entrypoint (RuntimeGenerationsDir/<genID>/bin/<harness>).
// Takes the shared poolPlanInput (not either runtime-specific embedding
// struct) so both twins' tests use one helper — the on-disk shape it writes
// is identical either way.
func writePoolGeneration(t *testing.T, in poolPlanInput, genID string, dispatcher, genBin bool) {
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
	writePoolGeneration(t, in.poolPlanInput, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4", true, true)

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
					writePoolGeneration(t, in.poolPlanInput, genID, tc.dispatcher, tc.genBin)

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

// --- §16 Phase 4 item 5 interplay: legacy bin/+lib/ already gc'd away ---

// TestResolveGuixPoolPlanLegacyGoneWithActivePointer covers "legacy-gone +
// pointer-present": an active pointer/generation exists, but
// `wraptool harness gc --legacy` has since removed the legacy trees. The
// plan must still work — runtime/-only, no legacy expose/pathDir, no error —
// rather than including an --expose of a host path that no longer exists.
func TestResolveGuixPoolPlanLegacyGoneWithActivePointer(t *testing.T) {
	in, root := guixPoolPlanTestLayout(t, "claude", true)
	if err := os.RemoveAll(in.LegacyBin); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(in.LegacyLib); err != nil {
		t.Fatal(err)
	}
	in.PointerPath = writePointerFile(t, root, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4\n")
	writePoolGeneration(t, in.poolPlanInput, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4", true, true)

	plan := resolveGuixPoolPlan(in)

	if plan.err != nil {
		t.Fatalf("plan.err = %v, want nil (an active generation needs no legacy fallback at all)", plan.err)
	}
	if !plan.active {
		t.Fatalf("plan.active = false, want true")
	}
	wantExposes := []string{"--expose=" + in.RuntimeDir + "=" + in.RuntimeDir}
	if !reflect.DeepEqual(plan.exposes, wantExposes) {
		t.Errorf("exposes = %v, want %v (runtime/ only — legacy is gone)", plan.exposes, wantExposes)
	}
	if !reflect.DeepEqual(plan.pathDirs, []string{in.RuntimeBinDir}) {
		t.Errorf("pathDirs = %v, want %v", plan.pathDirs, []string{in.RuntimeBinDir})
	}
	for _, e := range plan.exposes {
		if strings.Contains(e, in.LegacyBin) || strings.Contains(e, in.LegacyLib) {
			t.Errorf("exposes must not name a removed legacy dir: %v", plan.exposes)
		}
	}
}

// TestResolveGuixPoolPlanLegacyGoneNoPointer covers "legacy-gone + NO
// pointer": nothing at all can serve Harness's binary (no active
// generation, and legacy bin/+lib/ are gone too) — the plan must surface an
// actionable error instead of a plan that would fail deep inside
// `guix shell --container --expose` on a nonexistent host path.
func TestResolveGuixPoolPlanLegacyGoneNoPointer(t *testing.T) {
	for _, lockable := range []bool{true, false} {
		t.Run(map[bool]string{true: "lockable", false: "not lockable"}[lockable], func(t *testing.T) {
			in, _ := guixPoolPlanTestLayout(t, "claude", lockable)
			if err := os.RemoveAll(in.LegacyBin); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(in.LegacyLib); err != nil {
				t.Fatal(err)
			}
			// No pointer at all (in.PointerPath left "").

			plan := resolveGuixPoolPlan(in)

			if plan.err == nil {
				t.Fatal("plan.err = nil, want an actionable error (nothing can serve this harness's binary)")
			}
			if plan.active {
				t.Errorf("plan.active = true, want false")
			}
			if len(plan.exposes) != 0 || len(plan.pathDirs) != 0 {
				t.Errorf("plan = %+v, want every other field zero alongside a non-nil err", plan)
			}
			if !strings.Contains(plan.err.Error(), "claude") {
				t.Errorf("err = %q, want it to name the harness", plan.err)
			}
			if !strings.Contains(plan.err.Error(), "gc --legacy") {
				t.Errorf("err = %q, want it to state the TRUE cause (removed by `gc --legacy`)", plan.err)
			}
			if !strings.Contains(plan.err.Error(), in.LegacyBin) || !strings.Contains(plan.err.Error(), in.LegacyLib) {
				t.Errorf("err = %q, want it to name both actual legacy paths", plan.err)
			}
			if lockable {
				if !strings.Contains(plan.err.Error(), "wraptool harness install claude") {
					t.Errorf("err = %q, want the fix command for a pool-lockable harness", plan.err)
				}
				if !strings.Contains(plan.err.Error(), "un-gc") {
					t.Errorf("err = %q, want it to explain that reinstalling un-gc's the pool", plan.err)
				}
			} else if strings.Contains(plan.err.Error(), "wraptool harness install") {
				t.Errorf("err = %q, want no install-fix command for a non-pool-lockable harness (no such path exists)", plan.err)
			}
		})
	}
}

// TestResolveGuixPoolPlanHalfGoneNoPointerRefusesNotExposes is review round
// 2's mutation pin (finding 7): classifyLegacyPool's "both present" check
// MUST be an AND, not an OR — a half-gone pair (bin exists, lib doesn't)
// with no active pointer must still produce an actionable error, never a
// plan that --exposes the missing directory. Mutating either && in
// classifyLegacyPool to || would make this test fail (the half-gone pair
// would be misclassified as legacyBothPresent, producing a plan.err == nil
// legacy-fallback plan that names the nonexistent lib dir in its exposes).
// The error must also name BOTH expected paths and describe this as
// inconsistent/tampering, distinct from the "removed by gc --legacy"
// wording TestResolveGuixPoolPlanLegacyGoneNoPointer pins.
func TestResolveGuixPoolPlanHalfGoneNoPointerRefusesNotExposes(t *testing.T) {
	in, _ := guixPoolPlanTestLayout(t, "claude", true)
	if err := os.RemoveAll(in.LegacyLib); err != nil {
		t.Fatal(err) // bin/ survives, lib/ doesn't — half-gone.
	}
	// No pointer at all.

	plan := resolveGuixPoolPlan(in)

	if plan.err == nil {
		t.Fatal("plan.err = nil, want an actionable error for a half-gone legacy pair")
	}
	if len(plan.exposes) != 0 {
		t.Errorf("exposes = %v, want none — must never --expose only the surviving half", plan.exposes)
	}
	if !strings.Contains(plan.err.Error(), in.LegacyBin) || !strings.Contains(plan.err.Error(), in.LegacyLib) {
		t.Errorf("err = %q, want it to name BOTH expected paths", plan.err)
	}
	if strings.Contains(plan.err.Error(), "removed by") {
		t.Errorf("err = %q, want it to NOT claim `gc --legacy` as the cause (half-gone is not what gc --legacy produces)", plan.err)
	}
}

// TestResolveGuixPoolPlanHalfGoneActivePointerDropsLegacyEntirely proves the
// active-mode side of the same half-gone classification: activating a
// harness must never --expose only the surviving half of a half-gone
// legacy pair either — it degrades to runtime/-only, exactly like a fully-
// gone pair, with no error (an active generation needs no legacy fallback).
func TestResolveGuixPoolPlanHalfGoneActivePointerDropsLegacyEntirely(t *testing.T) {
	in, root := guixPoolPlanTestLayout(t, "claude", true)
	if err := os.RemoveAll(in.LegacyBin); err != nil {
		t.Fatal(err) // lib/ survives, bin/ doesn't — half-gone.
	}
	in.PointerPath = writePointerFile(t, root, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4\n")
	writePoolGeneration(t, in.poolPlanInput, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4", true, true)

	plan := resolveGuixPoolPlan(in)

	if plan.err != nil {
		t.Fatalf("plan.err = %v, want nil (an active generation needs no legacy fallback at all)", plan.err)
	}
	if !plan.active {
		t.Fatal("plan.active = false, want true")
	}
	wantExposes := []string{"--expose=" + in.RuntimeDir + "=" + in.RuntimeDir}
	if !reflect.DeepEqual(plan.exposes, wantExposes) {
		t.Errorf("exposes = %v, want %v (runtime/ only — must never expose only the surviving half)", plan.exposes, wantExposes)
	}
}

// TestResolveGuixPoolPlanLegacyGoneStaleGeneration covers the third
// legacy-gone permutation: a pointer resolves to a generation id, but the
// generation itself is missing from disk (as in
// TestResolveGuixPoolPlanStaleGeneration) AND legacy bin/+lib/ are also
// gone — same actionable-error outcome as the no-pointer case, since a
// stale pointer is "no active generation" either way.
func TestResolveGuixPoolPlanLegacyGoneStaleGeneration(t *testing.T) {
	in, root := guixPoolPlanTestLayout(t, "claude", true)
	if err := os.RemoveAll(in.LegacyBin); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(in.LegacyLib); err != nil {
		t.Fatal(err)
	}
	in.PointerPath = writePointerFile(t, root, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4\n")
	// Neither dispatcher nor generation-bin written: the generation is gone.

	plan := resolveGuixPoolPlan(in)

	if plan.err == nil {
		t.Fatal("plan.err = nil, want an actionable error")
	}
	if !strings.Contains(plan.err.Error(), "wraptool harness install claude") {
		t.Errorf("err = %q, want the fix command", plan.err)
	}
	if !strings.Contains(plan.err.Error(), "gc --legacy") {
		t.Errorf("err = %q, want it to state the TRUE cause (removed by `gc --legacy`)", plan.err)
	}
	if !strings.Contains(plan.err.Error(), in.LegacyBin) || !strings.Contains(plan.err.Error(), in.LegacyLib) {
		t.Errorf("err = %q, want it to name both actual legacy paths", plan.err)
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
	legacyRoot := t.TempDir() // legacyPoolPresent needs real, existing dirs
	poolBin := filepath.Join(legacyRoot, "bin")
	poolLib := filepath.Join(legacyRoot, "lib")
	if err := os.MkdirAll(poolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(poolLib, 0o755); err != nil {
		t.Fatal(err)
	}

	plan := resolveGuixPoolPlan(guixPoolPlanInput{
		poolPlanInput: poolPlanInput{
			Harness:               "claude",
			PoolLockable:          true,
			RuntimeDir:            "/pool/runtime",
			RuntimeBinDir:         "/pool/runtime/bin",
			RuntimeGenerationsDir: "/pool/runtime/generations",
		},
		LegacyBin: poolBin,
		LegacyLib: poolLib,
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
	legacyRoot := t.TempDir() // legacyPoolPresent needs real, existing dirs
	poolBin := filepath.Join(legacyRoot, "bin")
	poolLib := filepath.Join(legacyRoot, "lib")
	if err := os.MkdirAll(poolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(poolLib, 0o755); err != nil {
		t.Fatal(err)
	}

	in, tmpRoot := guixPoolPlanTestLayout(t, "claude", true)
	in.LegacyBin, in.LegacyLib = poolBin, poolLib // exercise distinct legacy-vs-tmp paths
	const genID = "claude-1.2.3-linux-amd64-glibc-a1b2c3d4"
	in.PointerPath = writePointerFile(t, tmpRoot, genID+"\n")
	writePoolGeneration(t, in.poolPlanInput, genID, true, true)

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

// TestGuixPoolStatusMessage pins the printed harness-pool status line for the
// four cases guixPoolStatusMessage distinguishes — in particular, the
// active-with-no-legacy case must attribute the missing legacy trees
// correctly: `gc --legacy` (which always removes both directories together)
// when both are genuinely gone, but a neutral, gc-uninvolved message naming
// both expected paths when only one survives (classifyLegacyPool's own doc
// comment — that state can never be gc --legacy's doing).
func TestGuixPoolStatusMessage(t *testing.T) {
	const poolRoot, poolBin, poolLib = "/pool", "/pool/bin", "/pool/lib"

	tests := []struct {
		name        string
		plan        guixPoolPlan
		legacyState legacyPoolState
		want        []string // substrings that must all be present
		mustNot     []string // substrings that must be absent
	}{
		{
			name:        "active with legacy present",
			plan:        guixPoolPlan{active: true, pathDirs: []string{"/pool/runtime/bin", poolBin}},
			legacyState: legacyBothPresent,
			want:        []string{poolRoot, "legacy bin/ also mounted", poolBin},
		},
		{
			name:        "active, legacy both absent (genuine gc --legacy)",
			plan:        guixPoolPlan{active: true, pathDirs: []string{"/pool/runtime/bin"}},
			legacyState: legacyBothAbsent,
			want:        []string{poolRoot, "runtime/ only", "gc --legacy"},
		},
		{
			name:        "active, legacy half-gone (not gc --legacy's doing)",
			plan:        guixPoolPlan{active: true, pathDirs: []string{"/pool/runtime/bin"}},
			legacyState: legacyHalfGone,
			want:        []string{poolRoot, "runtime/ only", "incomplete", poolBin, poolLib},
			mustNot:     []string{"gc --legacy"},
		},
		{
			name:        "legacy-only (no active pointer)",
			plan:        guixPoolPlan{active: false, pathDirs: []string{poolBin}},
			legacyState: legacyBothPresent,
			want:        []string{poolRoot, "install harness binaries into", poolBin},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guixPoolStatusMessage(poolRoot, poolBin, poolLib, tt.plan, tt.legacyState)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("message = %q, want it to contain %q", got, want)
				}
			}
			for _, mustNot := range tt.mustNot {
				if strings.Contains(got, mustNot) {
					t.Errorf("message = %q, must NOT contain %q (misattributes the cause)", got, mustNot)
				}
			}
		})
	}
}

// legacyPoolRemoved must treat any stat outcome other than a definite
// "does not exist" as removed — an unreadable-but-present marker (here: a
// directory occupying the marker path, which makes a plain file read fail)
// must not let ensureHarnessPool resurrect a deliberately gc'd pool. The
// review caught the original err == nil form doing the opposite of its own
// doc comment.
func TestLegacyPoolRemovedUnreadableMarkerCountsAsRemoved(t *testing.T) {
	root := t.TempDir()
	if legacyPoolRemoved(root) {
		t.Fatal("fresh pool root must not count as removed")
	}
	if err := os.MkdirAll(pool.LegacyRemovedMarkerFileAt(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if !legacyPoolRemoved(root) {
		t.Error("marker path occupied by a directory must still count as removed")
	}
}
