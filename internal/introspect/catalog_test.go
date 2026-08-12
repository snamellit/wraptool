package introspect

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// fakeShellScript writes an executable POSIX shell script at dir/name that
// prints stdout on any invocation, for testing a probe against a
// script-controlled binary rather than a real installed tool. Skips (not
// fails) on a platform without a POSIX shell, matching the rest of this
// package's testdata-driven tests' skip-when-unavailable convention.
func fakeShellScript(t *testing.T, dir, name, stdout string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script probing needs a POSIX shell")
	}
	bin := filepath.Join(dir, name)
	script := "#!/bin/sh\ncat <<'HELP'\n" + stdout + "HELP\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestProbeCatalog_NoArgvIsUnknown(t *testing.T) {
	dir := t.TempDir()
	bin := fakeShellScript(t, dir, "tool", gitStyleHelp)
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeCatalog(context.Background(), tc, nil, nil)
	if got.Exhaustive {
		t.Error("Exhaustive should be false when argv is empty — no catalog argv known")
	}
	if got.Catalog != nil {
		t.Errorf("Catalog = %v, want nil", got.Catalog)
	}
}

func TestProbeCatalog_EmptyParseIsUnknown(t *testing.T) {
	dir := t.TempDir()
	// A "catalog" argv whose output doesn't parse as a subcommand listing at
	// all (flags-only help) must not be reported as "this tool has zero
	// subcommands".
	bin := fakeShellScript(t, dir, "tool", flagsOnlyHelp)
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeCatalog(context.Background(), tc, []string{"--help"}, nil)
	if got.Exhaustive {
		t.Error("Exhaustive should be false for a catalog argv that parsed to zero subcommands")
	}
}

func TestProbeCatalog_ProbeErrorIsUnknown(t *testing.T) {
	tc := &config.ToolConfig{Binary: filepath.Join(t.TempDir(), "does-not-exist")}
	got := ProbeCatalog(context.Background(), tc, []string{"help", "-a"}, nil)
	if got.Exhaustive {
		t.Error("Exhaustive should be false when the probe itself errors")
	}
}

func TestProbeCatalog_RealCatalog(t *testing.T) {
	dir := t.TempDir()
	bin := fakeShellScript(t, dir, "tool", gitStyleHelp)
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeCatalog(context.Background(), tc, []string{"--help"}, nil)
	if !got.Exhaustive {
		t.Fatal("Exhaustive should be true for a genuine, non-empty catalog parse")
	}
	if got.Catalog["pr"] != "manage pull requests" {
		t.Errorf("Catalog[pr] = %q, want %q", got.Catalog["pr"], "manage pull requests")
	}
}

// TestProbeCatalog_AgainstGitFixtures exercises the real design §5.3 example
// end to end from captured fixtures: `git -h`'s catalog is NOT exhaustive
// (it's missing config/stash/etc — the top-level help path, not a catalog
// probe at all), while `git help -a` IS.
func TestProbeCatalog_AgainstGitFixtures(t *testing.T) {
	helpA := readTestData(t, "git_help_a.txt")
	catalog := BestParseSubcommands(helpA)
	for _, name := range []string{"stash", "config", "remote", "worktree", "cherry-pick", "submodule"} {
		if _, ok := catalog[name]; !ok {
			t.Errorf("git help -a catalog missing %q — the whole reason -h alone is not a catalog", name)
		}
	}

	dashH := readTestData(t, "git_help.txt")
	catalogH := BestParseSubcommands(dashH)
	for _, name := range []string{"stash", "config", "remote", "worktree", "cherry-pick", "submodule"} {
		if _, ok := catalogH[name]; ok {
			t.Errorf("git -h unexpectedly listed %q — if this now fails, git's -h output grew a common-commands entry and the design §5.3 rationale needs re-verifying", name)
		}
	}
}
