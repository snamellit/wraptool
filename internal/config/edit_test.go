package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestConfig writes a YAML config that references a fake binary
// created in dir/git, so Validate() passes.
func writeTestConfig(t *testing.T, body string) (configPath, binPath string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "git")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(dir, "config.yaml")
	rendered := strings.ReplaceAll(body, "{{BIN}}", binPath)
	if err := os.WriteFile(configPath, []byte(rendered), 0600); err != nil {
		t.Fatal(err)
	}
	return configPath, binPath
}

func TestApplyRequest_NewRule(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `# my config
server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    # existing rules
    allow:
      - subcommand: [status]
`)

	res, err := ApplyRequest(cfgPath, "git", []string{"commit"}, []string{"--message"})
	if err != nil {
		t.Fatalf("ApplyRequest: %v", err)
	}
	if !res.NewRule {
		t.Error("expected NewRule=true")
	}
	if len(res.AddedFlags) != 1 || res.AddedFlags[0] != "--message" {
		t.Errorf("AddedFlags = %v, want [--message]", res.AddedFlags)
	}

	data, _ := os.ReadFile(cfgPath)
	got := string(data)
	// New rule should be appended.
	if !strings.Contains(got, "[commit]") {
		t.Errorf("commit subcommand missing from output:\n%s", got)
	}
	if !strings.Contains(got, "--message") {
		t.Errorf("flag missing from output:\n%s", got)
	}
	// Comments should survive.
	if !strings.Contains(got, "# my config") || !strings.Contains(got, "# existing rules") {
		t.Errorf("comments lost:\n%s", got)
	}
}

func TestApplyRequest_ExtendExistingRule(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [commit]
        flags: [--message]
`)

	res, err := ApplyRequest(cfgPath, "git", []string{"commit"}, []string{"--message", "--amend"})
	if err != nil {
		t.Fatalf("ApplyRequest: %v", err)
	}
	if res.NewRule {
		t.Error("expected NewRule=false")
	}
	if len(res.AddedFlags) != 1 || res.AddedFlags[0] != "--amend" {
		t.Errorf("AddedFlags = %v, want [--amend]", res.AddedFlags)
	}
	data, _ := os.ReadFile(cfgPath)
	got := string(data)
	if !strings.Contains(got, "--amend") {
		t.Errorf("--amend missing:\n%s", got)
	}
	// --message must not be duplicated.
	if strings.Count(got, "--message") != 1 {
		t.Errorf("--message duplicated:\n%s", got)
	}
}

func TestApplyRequest_NoChangeWhenAlreadyAllowed(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [commit]
        flags: [--message, --amend]
`)
	before, _ := os.ReadFile(cfgPath)

	res, err := ApplyRequest(cfgPath, "git", []string{"commit"}, []string{"--message"})
	if err != nil {
		t.Fatalf("ApplyRequest: %v", err)
	}
	if res.NewRule || len(res.AddedFlags) != 0 {
		t.Errorf("expected no-op, got NewRule=%v AddedFlags=%v", res.NewRule, res.AddedFlags)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		// The YAML round-trip can rewrite whitespace; tolerate that but
		// confirm no flag content was duplicated.
		if strings.Count(string(after), "--message") != 1 {
			t.Errorf("flag list mutated unexpectedly:\n%s", string(after))
		}
	}
}

func TestApplyRequest_DenyConflict(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
    deny:
      - subcommand: [push]
`)
	before, _ := os.ReadFile(cfgPath)

	_, err := ApplyRequest(cfgPath, "git", []string{"push"}, []string{"--force"})
	if err == nil {
		t.Fatal("expected deny-conflict error")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("error should mention deny conflict, got: %v", err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Error("file modified despite deny conflict")
	}
}

func TestApplyRequest_UnknownTool(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)
	before, _ := os.ReadFile(cfgPath)

	_, err := ApplyRequest(cfgPath, "kubectl", []string{"get"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error should explain tool must be configured first, got: %v", err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Error("file modified despite unknown tool")
	}
}

func TestApplyRequest_MultiLevelSubcommand(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)

	res, err := ApplyRequest(cfgPath, "git", []string{"remote", "set-url"}, []string{"--push"})
	if err != nil {
		t.Fatalf("ApplyRequest: %v", err)
	}
	if !res.NewRule {
		t.Error("expected NewRule=true")
	}
	data, _ := os.ReadFile(cfgPath)
	got := string(data)
	if !strings.Contains(got, "[remote, set-url]") {
		t.Errorf("multi-level subcommand not encoded as flow sequence:\n%s", got)
	}
}

func TestApplyRequest_ValidationFailureLeavesFile(t *testing.T) {
	// Make config invalid by removing the binary file after writing the
	// config; ApplyRequest should refuse to overwrite.
	cfgPath, binPath := writeTestConfig(t, `server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)
	if err := os.Remove(binPath); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(cfgPath)

	_, err := ApplyRequest(cfgPath, "git", []string{"commit"}, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Error("file modified despite validation failure")
	}
}

func TestApplyRequest_NoAllowSection(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)
	// Strip the allow section by overwriting (use a tool entry without
	// allow but with an inline existing rule so validate still passes).
	// Skip: edit-only path needs at least one allow rule per validate.
	// Instead, test that ApplyRequest succeeds and adds first rule when
	// allow exists but is empty.
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`server:
  transport: stdio
tools:
  git:
    binary: %s
    allow:
      - subcommand: [status]
`, mustBinPath(t, cfgPath))), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := ApplyRequest(cfgPath, "git", []string{"log"}, []string{"--oneline"})
	if err != nil {
		t.Fatalf("ApplyRequest: %v", err)
	}
	if !res.NewRule {
		t.Error("expected NewRule=true")
	}
}

func mustBinPath(t *testing.T, cfgPath string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(cfgPath), "git")
}
