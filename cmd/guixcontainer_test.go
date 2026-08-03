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

// TestResolveGuixPoolPlan table-tests the pointer-file decision in isolation:
// no guix exec, no real pool root — pointerPath is whatever the test writes
// under t.TempDir(). This pins §12.1's "missing/unreadable/empty pointer ->
// legacy fallback with a one-line notice" rule and the "valid pointer ->
// runtime/ read-only, notice-free" rule.
func TestResolveGuixPoolPlan(t *testing.T) {
	const (
		runtimeDir    = "/pool/runtime"
		runtimeBinDir = "/pool/runtime/bin"
		legacyBin     = "/pool/bin"
		legacyLib     = "/pool/lib"
	)

	writeFile := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "claude.gen")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name        string
		pointerPath func(t *testing.T) string
		wantActive  bool
	}{
		{
			name:        "valid pointer activates runtime",
			pointerPath: func(t *testing.T) string { return writeFile(t, "claude-1.2.3-linux-amd64-glibc-a1b2c3d4\n") },
			wantActive:  true,
		},
		{
			name:        "missing pointer file falls back",
			pointerPath: func(t *testing.T) string { return filepath.Join(t.TempDir(), "claude.gen") },
			wantActive:  false,
		},
		{
			name:        "empty pointer file falls back",
			pointerPath: func(t *testing.T) string { return writeFile(t, "") },
			wantActive:  false,
		},
		{
			name:        "empty-after-trim pointer file falls back",
			pointerPath: func(t *testing.T) string { return writeFile(t, "\n") },
			wantActive:  false,
		},
		{
			name: "unreadable pointer path (a directory) falls back",
			pointerPath: func(t *testing.T) string {
				d := filepath.Join(t.TempDir(), "claude.gen")
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
				return d
			},
			wantActive: false,
		},
		{
			name:        "empty pointerPath (host platform key unresolved) falls back",
			pointerPath: func(t *testing.T) string { return "" },
			wantActive:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := resolveGuixPoolPlan("claude", tc.pointerPath(t), runtimeDir, runtimeBinDir, legacyBin, legacyLib)

			if tc.wantActive {
				wantExposes := []string{"--expose=" + runtimeDir + "=" + runtimeDir}
				if !reflect.DeepEqual(plan.exposes, wantExposes) {
					t.Errorf("exposes = %v, want %v", plan.exposes, wantExposes)
				}
				if plan.pathDir != runtimeBinDir {
					t.Errorf("pathDir = %q, want %q", plan.pathDir, runtimeBinDir)
				}
				if plan.notice != "" {
					t.Errorf("notice = %q, want none", plan.notice)
				}
				for _, e := range plan.exposes {
					if strings.Contains(e, legacyBin) || strings.Contains(e, legacyLib) {
						t.Errorf("active-pointer exposes leaked legacy dir: %v", plan.exposes)
					}
				}
				return
			}

			wantExposes := []string{
				"--expose=" + legacyBin + "=" + legacyBin,
				"--expose=" + legacyLib + "=" + legacyLib,
			}
			if !reflect.DeepEqual(plan.exposes, wantExposes) {
				t.Errorf("exposes = %v, want %v", plan.exposes, wantExposes)
			}
			if plan.pathDir != legacyBin {
				t.Errorf("pathDir = %q, want %q", plan.pathDir, legacyBin)
			}
			if plan.notice == "" {
				t.Error("expected a legacy-fallback deprecation notice, got none")
			}
			if !strings.Contains(plan.notice, "claude") || !strings.Contains(plan.notice, "wraptool harness sync") {
				t.Errorf("notice = %q, want it to name the harness and the fix command", plan.notice)
			}
			for _, e := range plan.exposes {
				if strings.Contains(e, runtimeDir) {
					t.Errorf("legacy-fallback exposes leaked runtime dir: %v", plan.exposes)
				}
			}
		})
	}
}

// TestResolveGuixPoolPlanNoticeIsSingleLine pins plan.notice as one line
// containing exactly one occurrence of the notice marker — the actual
// "printed once, not per mount" behavior lives in bringUpGuixContainer, which
// calls resolveGuixPoolPlan exactly once and prints plan.notice with exactly
// one fmt.Fprintln(os.Stderr, ...) gated on a single non-empty check (never a
// loop over exposes/mounts), so a single-line, single-occurrence notice here
// is what guarantees no caller can accidentally duplicate it per mount.
func TestResolveGuixPoolPlanNoticeIsSingleLine(t *testing.T) {
	plan := resolveGuixPoolPlan("claude", "", "/pool/runtime", "/pool/runtime/bin", "/pool/bin", "/pool/lib")
	if n := strings.Count(plan.notice, "notice:"); n != 1 {
		t.Fatalf("notice contains %d occurrences of \"notice:\", want exactly 1: %q", n, plan.notice)
	}
	if strings.Contains(plan.notice, "\n") {
		t.Fatalf("notice must be a single line: %q", plan.notice)
	}
}

// TestBuildContainerEntryAppendsPathDir pins §12.1's append-not-prepend
// invariant: pathDir must land strictly after "$PATH:" in the exported PATH
// assignment, never before "$PATH" (which would let a pool binary shadow a
// project toolchain binary — git/go/bash).
func TestBuildContainerEntryAppendsPathDir(t *testing.T) {
	entry := buildContainerEntry("/work/proj", "/pool/home", "/pool/runtime/bin")

	pathAssign := "export PATH=$PATH:" + shellQuote("/pool/runtime/bin")
	idx := strings.Index(entry, pathAssign)
	if idx < 0 {
		t.Fatalf("entry does not append pathDir after $PATH: entry=%q", entry)
	}
	// The append form is "$PATH:<dir>" — assert the directory never appears as
	// a prefix ("<dir>:$PATH") anywhere in the entry.
	prependForm := "export PATH=" + shellQuote("/pool/runtime/bin") + ":$PATH"
	if strings.Contains(entry, prependForm) {
		t.Fatalf("entry prepends pathDir instead of appending: entry=%q", entry)
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

	plan := resolveGuixPoolPlan("claude", "", "/pool/runtime", "/pool/runtime/bin", poolBin, poolLib)
	entry := buildContainerEntry(root, poolHome, plan.pathDir)
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
// mount: runtime/ read-only at the same host path, PATH appended with
// runtime/bin, and — crucially — no legacy bin/+lib/ exposes at all. home/
// stays shared exactly as in the legacy case.
func TestBuildGuixShellArgsActivePointerPlan(t *testing.T) {
	root := "/work/proj"
	manifest := "/work/proj/manifest.scm"
	poolHome := "/pool/home"
	poolBin := "/pool/bin"
	poolLib := "/pool/lib"
	runtimeDir := "/pool/runtime"
	runtimeBinDir := "/pool/runtime/bin"

	pointerPath := filepath.Join(t.TempDir(), "claude.gen")
	if err := os.WriteFile(pointerPath, []byte("claude-1.2.3-linux-amd64-glibc-a1b2c3d4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	plan := resolveGuixPoolPlan("claude", pointerPath, runtimeDir, runtimeBinDir, poolBin, poolLib)
	entry := buildContainerEntry(root, poolHome, plan.pathDir)
	got := buildGuixShellArgs(root, manifest, poolHome, plan, entry)

	want := []string{
		"shell", "--container", "--network",
		"--emulate-fhs",
		"--share=" + root + "=" + root,
		"--expose=" + runtimeDir + "=" + runtimeDir,
		"--share=" + poolHome + "=" + poolHome,
		"--preserve=^WRAPTOOL_",
		"--preserve=^COLORTERM$",
		"-m", manifest,
		"--", "bash", "-lc", entry,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("active-pointer guix shell args =\n%#v\nwant\n%#v", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, poolBin) || strings.Contains(a, poolLib) {
			t.Fatalf("active-pointer args must not expose legacy bin/lib: %v", got)
		}
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
