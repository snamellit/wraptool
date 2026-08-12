package introspect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

func readTestData(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Skipf("test data %s not available: %v", name, err)
	}
	return string(data)
}

func TestGitParser_ParseSubcommands(t *testing.T) {
	helpText := readTestData(t, "git_help.txt")
	p := &GitParser{}
	subcmds := p.ParseSubcommands(helpText)

	expected := []string{"clone", "init", "add", "commit", "log", "status", "push", "pull", "diff", "branch", "merge"}
	for _, name := range expected {
		if _, ok := subcmds[name]; !ok {
			t.Errorf("expected subcommand %q to be found", name)
		}
	}

	// Verify descriptions.
	if desc, ok := subcmds["commit"]; ok {
		if desc != "Record changes to the repository" {
			t.Errorf("commit description = %q", desc)
		}
	}
}

func TestGitParser_ParseFlags_CommitH(t *testing.T) {
	helpText := readTestData(t, "git_commit_h.txt")
	p := &GitParser{}
	result := p.ParseFlags(helpText)

	if result.Confidence < 0.7 {
		t.Errorf("expected high confidence, got %.2f", result.Confidence)
	}

	flags := result.Subcommand.Flags

	// Check --message was parsed.
	if f, ok := flags["--message"]; ok {
		if f.Short != "-m" {
			t.Errorf("--message short = %q, want -m", f.Short)
		}
		if f.Type != "string" {
			t.Errorf("--message type = %q, want string", f.Type)
		}
		if !f.TakesValue {
			t.Error("--message should take a value")
		}
	} else {
		t.Error("expected --message flag to be found")
	}

	// Check --amend was parsed as boolean.
	if f, ok := flags["--amend"]; ok {
		if f.Type != "boolean" {
			t.Errorf("--amend type = %q, want boolean", f.Type)
		}
		if f.TakesValue {
			t.Error("--amend should not take a value")
		}
	} else {
		t.Error("expected --amend flag to be found")
	}

	// Check --verbose was parsed.
	if f, ok := flags["--verbose"]; ok {
		if f.Short != "-v" {
			t.Errorf("--verbose short = %q, want -v", f.Short)
		}
	} else {
		t.Error("expected --verbose flag to be found")
	}
}

func TestGNUParser_ParseFlags_Curl(t *testing.T) {
	helpText := readTestData(t, "curl_help.txt")
	p := &GNUParser{}
	result := p.ParseFlags(helpText)

	if result.Confidence < 0.5 {
		t.Errorf("expected reasonable confidence, got %.2f", result.Confidence)
	}

	flags := result.Subcommand.Flags

	// Check --data was parsed.
	if f, ok := flags["--data"]; ok {
		if f.Short != "-d" {
			t.Errorf("--data short = %q, want -d", f.Short)
		}
		if !f.TakesValue {
			t.Error("--data should take a value")
		}
	} else {
		t.Error("expected --data flag to be found")
	}

	// Check --fail was parsed as boolean.
	if f, ok := flags["--fail"]; ok {
		if f.TakesValue {
			t.Error("--fail should not take a value")
		}
	} else {
		t.Error("expected --fail flag to be found")
	}
}

// TestDemoteBooleansWhenNoPlaceholders covers the glab-style case through
// BestParse — the demote runs on the WINNING parser. For glab that's the git
// parser (it also handles cobra-style help, confidence 0.8); demoting only in
// the GNU/fallback parsers missed it, which is what kept glab's --reviewer
// boolean even after the fix "shipped".
func TestDemoteBooleansWhenNoPlaceholders(t *testing.T) {
	// No placeholders anywhere (real glab FLAGS shape, short flags space-separated
	// so the git parser wins): --reviewer (a value flag) looks identical to
	// --draft (a switch). BestParse must type them unknown, not boolean.
	glabish := "  FLAGS\n" +
		"    -a --assignee           Assign users via username.\n" +
		"    --draft                 Mark merge request as a draft.\n" +
		"    --reviewer              Request review from users by their usernames.\n" +
		"    --target-branch         Set target branch.\n"
	flags := BestParse(glabish).Subcommand.Flags
	for _, name := range []string{"--reviewer", "--target-branch"} {
		f, ok := flags[name]
		if !ok {
			t.Fatalf("%s not parsed", name)
		}
		if f.Type != "" {
			t.Errorf("%s type = %q, want \"\" (no placeholder anywhere ⇒ unknown, not boolean)", name, f.Type)
		}
	}

	// Placeholders present (curl/git style): a valueless flag stays a real boolean.
	withPlaceholders := "  -d, --data <data>   HTTP POST data\n" +
		"  -f, --fail          Fail fast with no output\n"
	pflags := BestParse(withPlaceholders).Subcommand.Flags
	if f := pflags["--fail"]; f.Type != "boolean" {
		t.Errorf("--fail type = %q, want boolean (help uses placeholders elsewhere)", f.Type)
	}
	if f := pflags["--data"]; f.Type != "string" {
		t.Errorf("--data type = %q, want string", f.Type)
	}
}

func TestBestParse_PicksHighestConfidence(t *testing.T) {
	helpText := readTestData(t, "git_commit_h.txt")
	result := BestParse(helpText)

	// Git parser should win over GNU and fallback.
	if result.Confidence < 0.7 {
		t.Errorf("BestParse should pick git parser (high confidence), got %.2f", result.Confidence)
	}

	if len(result.Subcommand.Flags) < 5 {
		t.Errorf("expected multiple flags, got %d", len(result.Subcommand.Flags))
	}
}

func TestFallbackParser_ExtractsFlags(t *testing.T) {
	// Minimal help text that no structured parser handles well.
	helpText := `Usage: mytool [options]
Options include --verbose, --output <file>, and --count <n>.
Use --help for more information.`

	p := &FallbackParser{}
	result := p.ParseFlags(helpText)

	if result.Confidence < 0.2 {
		t.Errorf("expected some confidence, got %.2f", result.Confidence)
	}

	flags := result.Subcommand.Flags
	if _, ok := flags["--verbose"]; !ok {
		t.Error("expected --verbose to be extracted")
	}
	if _, ok := flags["--output"]; !ok {
		t.Error("expected --output to be extracted")
	}
	// --help should be filtered out.
	if _, ok := flags["--help"]; ok {
		t.Error("--help should be filtered out")
	}
}

// gitStyleHelp is a cobra/git-style listing: two-column, indented subcommands.
const gitStyleHelp = `usage: fj <command> [<args>]

Available commands:
  pr        manage pull requests
  issue     manage issues
  repo      manage repositories
`

// flagsOnlyHelp is a GNU-style help with no subcommands at all.
const flagsOnlyHelp = `Usage: widget [OPTION]... [FILE]...

  -a, --append         append to the file
  -v, --verbose        explain what is being done
      --help           display this help and exit
`

func TestBestParseSubcommands(t *testing.T) {
	subs := BestParseSubcommands(gitStyleHelp)
	if len(subs) != 3 {
		t.Fatalf("got %d subcommands, want 3: %v", len(subs), subs)
	}
	if got := subs["pr"]; got != "manage pull requests" {
		t.Errorf("pr description = %q, want %q", got, "manage pull requests")
	}

	// A flags-only help must yield no catalog rather than mistaking option
	// lines for subcommands — a bogus catalog would send capability requests
	// after subcommands the tool does not have.
	if subs := BestParseSubcommands(flagsOnlyHelp); len(subs) != 0 {
		t.Errorf("flags-only help produced a catalog: %v", subs)
	}
}

// A tool with no allow rules is inert, but must still be discoverable — that
// is the whole bootstrap path for requesting its first capability.
func TestIntrospectCollectsCatalogWithoutAllowRules(t *testing.T) {
	isolateCacheDir(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "fj")
	script := "#!/bin/sh\ncat <<'HELP'\n" + gitStyleHelp + "HELP\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	schema, err := Introspect(context.Background(), "fj", &config.ToolConfig{Binary: bin})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if len(schema.Subcommands) != 0 {
		t.Errorf("no allow rules, but %d subcommands introspected", len(schema.Subcommands))
	}
	if len(schema.Available) != 3 {
		t.Fatalf("Available = %v, want 3 entries", schema.Available)
	}
	if got := schema.Available["issue"]; got != "manage issues" {
		t.Errorf("issue description = %q, want %q", got, "manage issues")
	}
}

// TestIntrospect_ParseOutcomeParsed covers items 4/5's happy path: a
// subcommand whose help genuinely parses gets ParseOutcomeParsed and
// coverage counts reflecting how much of the help's own flag vocabulary the
// parse captured.
func TestIntrospect_ParseOutcomeParsed(t *testing.T) {
	isolateCacheDir(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	help := "Usage: tool commit [OPTION]...\n\n" +
		"  -a, --append   append to the file\n" +
		"  -v, --verbose  explain what is being done\n"
	script := "#!/bin/sh\ncat <<'HELP'\n" + help + "HELP\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	tc := &config.ToolConfig{Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"commit"}}}}
	schema, err := Introspect(context.Background(), "tool", tc)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	sub, ok := schema.Subcommands["commit"]
	if !ok {
		t.Fatal("expected a \"commit\" entry")
	}
	if sub.ParseOutcome != ParseOutcomeParsed {
		t.Errorf("ParseOutcome = %q, want %q", sub.ParseOutcome, ParseOutcomeParsed)
	}
	if sub.CoverageTokens != 4 || sub.CoverageFound != 4 {
		t.Errorf("CoverageFound/CoverageTokens = %d/%d, want 4/4 (both flags' long AND short forms captured)", sub.CoverageFound, sub.CoverageTokens)
	}
	if len(sub.Flags) != 2 {
		t.Errorf("got %d flags, want 2", len(sub.Flags))
	}
}

// TestIntrospect_ParseOutcomeGarbage is the design's own worked example:
// `git bogus -h` returns a 53-byte, one-line error message that clears
// getHelp's 20-byte success threshold. Introspect must record
// ParseOutcomeGarbage, not parse it as if it were real help.
func TestIntrospect_ParseOutcomeGarbage(t *testing.T) {
	isolateCacheDir(t)

	garbage := readTestData(t, "git_bogus_h.txt")
	if len(garbage) <= 20 {
		t.Fatalf("test fixture bug: garbage fixture must clear the 20-byte threshold, got %d bytes", len(garbage))
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	// Every help form (-h, --help, help <sub>) returns the same one-line
	// error text — exactly what a genuinely nonexistent subcommand does.
	script := "#!/bin/sh\nprintf '%s' " + shellQuote(garbage) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	tc := &config.ToolConfig{Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"bogus"}}}}
	schema, err := Introspect(context.Background(), "git", tc)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	sub, ok := schema.Subcommands["bogus"]
	if !ok {
		t.Fatal("expected a \"bogus\" entry")
	}
	if sub.ParseOutcome != ParseOutcomeGarbage {
		t.Errorf("ParseOutcome = %q, want %q", sub.ParseOutcome, ParseOutcomeGarbage)
	}
	if len(sub.Flags) != 0 {
		t.Errorf("garbage output must not be parsed into flags, got %v", sub.Flags)
	}
}

// TestIntrospect_ParseOutcomeHelpFailed covers the third distinguishable
// state: no help output was obtained at all (every form exits with nothing).
func TestIntrospect_ParseOutcomeHelpFailed(t *testing.T) {
	isolateCacheDir(t)

	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	tc := &config.ToolConfig{Binary: bin, Allow: []config.AllowRule{{Subcommand: []string{"noop"}}}}
	schema, err := Introspect(context.Background(), "tool", tc)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	sub, ok := schema.Subcommands["noop"]
	if !ok {
		t.Fatal("expected a \"noop\" entry")
	}
	if sub.ParseOutcome != ParseOutcomeHelpFailed {
		t.Errorf("ParseOutcome = %q, want %q", sub.ParseOutcome, ParseOutcomeHelpFailed)
	}
}

// shellQuote wraps s in single quotes for embedding in a generated shell
// script's printf argument, escaping any single quote s itself contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestParseIsNeverDiscardedForShape pins the ordering invariant that keeps real
// flag metadata out of the bin: looksLikeHelp is consulted only when the parse
// found nothing, so a text the parsers DID extract flags from can never be
// classified garbage on layout grounds.
//
// The regression it guards is not hypothetical. git's stash, config, worktree,
// submodule and bisect help pages are pure usage blocks — no line begins with
// '-', no indented two-column block — yet the parsers extract 12, 9, 7, 18 and
// 3 flags from them respectively. A shape gate placed BEFORE the parse dropped
// every one of those flags from the MCP tool schema, silently removing
// capabilities the policy had granted.
func TestParseIsNeverDiscardedForShape(t *testing.T) {
	// Prose mentioning flags: no usage line, no line beginning with '-', no
	// indented two-column block, so the shape check rejects it — while the
	// fallback parser, which scans for flag tokens anywhere, reads flags out of
	// it. Exactly the disagreement the ordering exists to resolve.
	const usageBlock = "Supported options are --foo and --bar for this command.\n"
	if looksLikeHelp(usageBlock) {
		t.Skip("shape check now accepts this text; the ordering it guards no longer applies to this fixture")
	}
	if got := len(BestParse(usageBlock).Subcommand.Flags); got == 0 {
		t.Skip("parsers extract nothing from this fixture; it cannot demonstrate the ordering")
	}

	sub := classifySubcommandHelp("git", "stash", usageBlock)
	if sub.ParseOutcome == ParseOutcomeGarbage {
		t.Errorf("a text the parsers extracted flags from was classified garbage on shape alone")
	}
	if len(sub.Flags) == 0 {
		t.Errorf("flags the parsers found were discarded; got %+v", sub)
	}
}

// TestRealGitUsageBlocksKeepTheirFlags is the live counterpart to
// TestParseIsNeverDiscardedForShape: the synthetic fixture proves the ordering,
// this proves the ordering was needed. Every subcommand here is one whose flags
// vanished from the tool schema while the shape gate ran first.
func TestRealGitUsageBlocksKeepTheirFlags(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	tc := &config.ToolConfig{Binary: git}

	for _, sub := range []string{"stash", "config", "worktree", "submodule", "bisect"} {
		helpText, err := RunProbe(context.Background(), tc, []string{sub, "-h"})
		if err != nil || helpText == "" {
			t.Logf("%s: probe produced nothing on this host, skipping", sub)
			continue
		}
		got := classifySubcommandHelp("git", sub, helpText)
		if got.ParseOutcome == ParseOutcomeGarbage {
			t.Errorf("git %s: real help classified garbage", sub)
		}
		if len(got.Flags) == 0 {
			t.Errorf("git %s: real help yielded zero flags", sub)
		}
	}
}
