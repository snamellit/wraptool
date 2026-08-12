package introspect

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

	got := ProbeCatalog(context.Background(), tc, nil)
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

	got := ProbeCatalog(context.Background(), tc, []string{"--help"})
	if got.Exhaustive {
		t.Error("Exhaustive should be false for a catalog argv that parsed to zero subcommands")
	}
}

func TestProbeCatalog_ProbeErrorIsUnknown(t *testing.T) {
	tc := &config.ToolConfig{Binary: filepath.Join(t.TempDir(), "does-not-exist")}
	got := ProbeCatalog(context.Background(), tc, []string{"help", "-a"})
	if got.Exhaustive {
		t.Error("Exhaustive should be false when the probe itself errors")
	}
}

func TestProbeCatalog_RealCatalog(t *testing.T) {
	dir := t.TempDir()
	bin := fakeShellScript(t, dir, "tool", gitStyleHelp)
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeCatalog(context.Background(), tc, []string{"--help"})
	if !got.Exhaustive {
		t.Fatal("Exhaustive should be true for a genuine, non-empty catalog parse")
	}
	if got.Catalog["pr"] != "manage pull requests" {
		t.Errorf("Catalog[pr] = %q, want %q", got.Catalog["pr"], "manage pull requests")
	}
}

// TestProbeCatalog_NonZeroExitIsUnknown covers Ruling 3's core fix: a probe
// that produced catalog-shaped output but exited non-zero must not be
// trusted as exhaustive — swallowing exit status is what let a killed-or-
// failing `git help -a` be recorded as a COMPLETE catalog.
func TestProbeCatalog_NonZeroExitIsUnknown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script probing needs a POSIX shell")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	script := "#!/bin/sh\ncat <<'HELP'\n" + gitStyleHelp + "HELP\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeCatalog(context.Background(), tc, []string{"--help"})
	if got.Exhaustive {
		t.Error("Exhaustive should be false — the probe produced output but exited non-zero, a partial-or-failed catalog must not be trusted as complete")
	}
}

// TestProbeCatalog_DeadlineKilledIsUnknown covers Ruling 3's other exit-
// status case: a probe killed mid-listing by ITS OWN context deadline can
// leave plausible-looking partial output behind, and that must not be
// mistaken for a clean, complete catalog either.
func TestProbeCatalog_DeadlineKilledIsUnknown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script probing needs a POSIX shell")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	// Print a real catalog-shaped prefix, then hang — exec replaces the
	// shell process so the deadline's kill lands on the actual sleeper, see
	// runprobe_test.go's TestRunProbeEnv_DeadlineKill for why.
	script := "#!/bin/sh\ncat <<'HELP'\n" + gitStyleHelp + "HELP\nexec sleep 5\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	got := ProbeCatalog(ctx, tc, []string{"--help"})
	if got.Exhaustive {
		t.Error("Exhaustive should be false — the probe was killed by its own deadline mid-listing")
	}
}

// TestProbeCatalog_ErrorPageOutputIsUnknown covers the shape check: a
// one-line CLI error page must not become a catalog any more than it may
// become parsed help, even though it also happens to parse to zero
// subcommands (belt AND suspenders — see looksLikeHelp's own doc comment for
// why an anywhere-in-text scan alone is not safe here).
func TestProbeCatalog_ErrorPageOutputIsUnknown(t *testing.T) {
	garbage := readTestData(t, "git_bogus_h.txt")
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	script := "#!/bin/sh\nprintf '%s' " + shellQuote(garbage) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &config.ToolConfig{Binary: bin}

	got := ProbeCatalog(context.Background(), tc, []string{"help", "-a"})
	if got.Exhaustive {
		t.Error("Exhaustive should be false — a one-line CLI error page is not a catalog")
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
