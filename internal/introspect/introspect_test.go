package introspect

import (
	"os"
	"testing"
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
