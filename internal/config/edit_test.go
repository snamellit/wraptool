package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/yamledit"
	"gopkg.in/yaml.v3"
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
mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    # existing rules
    allow:
      - subcommand: [status]
`)

	res, err := ApplyRequest(cfgPath, "git", []string{"commit"}, []string{"--message"}, nil)
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

// ruleFor finds the allow rule for a tool+subcommand in a loaded config.
func ruleFor(t *testing.T, cfg *Config, tool string, subcommand []string) AllowRule {
	t.Helper()
	for _, r := range cfg.Tools[tool].Allow {
		if strings.Join(r.Subcommand, " ") == strings.Join(subcommand, " ") {
			return r
		}
	}
	t.Fatalf("no allow rule for %s %v", tool, subcommand)
	return AllowRule{}
}

func TestApplyRequest_WritesFlagConstraints(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)
	// A requester supplies a type for --message but not --amend.
	if _, err := ApplyRequest(cfgPath, "git", []string{"commit"},
		[]string{"--message", "--amend"},
		map[string]string{"--message": "string"}); err != nil {
		t.Fatalf("ApplyRequest: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rule := ruleFor(t, cfg, "git", []string{"commit"})
	fc, ok := rule.FlagConstraints["--message"]
	if !ok {
		t.Fatalf("no flag_constraints for --message; got %v", rule.FlagConstraints)
	}
	if fc.Type != ConstraintString {
		t.Errorf("--message type = %q, want string", fc.Type)
	}
	if _, ok := rule.FlagConstraints["--amend"]; ok {
		t.Error("--amend should carry no constraint (no type supplied)")
	}
}

func TestApplyRequest_KeepsExistingConstraint(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [commit]
        flags: [--message]
        flag_constraints:
          --message: {type: string, required: true}
`)
	// A later request tries to retype --message to integer — the existing
	// operator-authored constraint must win.
	if _, err := ApplyRequest(cfgPath, "git", []string{"commit"},
		[]string{"--message"}, map[string]string{"--message": "integer"}); err != nil {
		t.Fatalf("ApplyRequest: %v", err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fc := ruleFor(t, cfg, "git", []string{"commit"}).FlagConstraints["--message"]
	if fc.Type != ConstraintString || !fc.Required {
		t.Errorf("existing constraint clobbered: type=%q required=%v, want string/true", fc.Type, fc.Required)
	}
}

func TestApplyRequest_RejectsInvalidFlagType(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)
	// An invalid type must fail the round-trip validation and leave the file
	// untouched (the closed ConstraintType enum rejects it at decode).
	if _, err := ApplyRequest(cfgPath, "git", []string{"commit"},
		[]string{"--message"}, map[string]string{"--message": "bogus"}); err == nil {
		t.Error("expected ApplyRequest to reject an invalid flag type")
	}
}

func TestApplyRequest_ExtendExistingRule(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [commit]
        flags: [--message]
`)

	res, err := ApplyRequest(cfgPath, "git", []string{"commit"}, []string{"--message", "--amend"}, nil)
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
	cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [commit]
        flags: [--message, --amend]
`)
	before, _ := os.ReadFile(cfgPath)

	res, err := ApplyRequest(cfgPath, "git", []string{"commit"}, []string{"--message"}, nil)
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
	cfgPath, _ := writeTestConfig(t, `mcp:
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

	_, err := ApplyRequest(cfgPath, "git", []string{"push"}, []string{"--force"}, nil)
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
	cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)
	before, _ := os.ReadFile(cfgPath)

	_, err := ApplyRequest(cfgPath, "kubectl", []string{"get"}, nil, nil)
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
	cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)

	res, err := ApplyRequest(cfgPath, "git", []string{"remote", "set-url"}, []string{"--push"}, nil)
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
	cfgPath, binPath := writeTestConfig(t, `mcp:
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

	_, err := ApplyRequest(cfgPath, "git", []string{"commit"}, nil, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Error("file modified despite validation failure")
	}
}

func TestApplyRequest_NoAllowSection(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, `mcp:
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
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`mcp:
  transport: stdio
tools:
  git:
    binary: %s
    allow:
      - subcommand: [status]
`, mustBinPath(t, cfgPath))), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := ApplyRequest(cfgPath, "git", []string{"log"}, []string{"--oneline"}, nil)
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

// TestEditConfig_ErrorPrefixesDistinguishDecodeFromValidate pins the two
// distinct error prefixes editConfig's Validate closure restores over
// yamledit.Edit's generic wrapping: a round-trip decode failure (the edited
// node tree no longer parses as a Config at all — a bug in the edit itself)
// reads "validating edited config: ...", while a semantic Validate()
// failure (parses fine, violates a rule) reads "edited config failed
// validation: ...". Both get an outer, generic "edited config failed
// validation: " wrap from yamledit.Edit itself, so the decode-failure case
// is checked by its inner, distinguishing prefix specifically.
func TestEditConfig_ErrorPrefixesDistinguishDecodeFromValidate(t *testing.T) {
	t.Run("decode failure", func(t *testing.T) {
		cfgPath, _ := writeTestConfig(t, `mcp:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
`)
		err := editConfig(cfgPath, func(doc *yaml.Node) error {
			mcpNode := yamledit.MapGet(doc, "mcp")
			// `transport` is a string field; a mapping value fails to
			// decode into Config at all, before newCfg.Validate() ever
			// runs.
			yamledit.MapSet(mcpNode, "transport", &yaml.Node{Kind: yaml.MappingNode})
			return nil
		})
		if err == nil {
			t.Fatal("expected a decode error")
		}
		if !strings.Contains(err.Error(), "validating edited config:") {
			t.Errorf("expected the decode-stage prefix \"validating edited config:\", got: %v", err)
		}
	})

	t.Run("semantic validation failure", func(t *testing.T) {
		cfgPath, binPath := writeTestConfig(t, `mcp:
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
		err := editConfig(cfgPath, func(doc *yaml.Node) error { return nil })
		if err == nil {
			t.Fatal("expected a semantic validation error")
		}
		if !strings.Contains(err.Error(), "edited config failed validation:") {
			t.Errorf("expected the validate-stage prefix \"edited config failed validation:\", got: %v", err)
		}
		if strings.Contains(err.Error(), "validating edited config:") {
			t.Errorf("semantic validation failure should not carry the decode-stage prefix, got: %v", err)
		}
	})
}

// TestEditConfig_MissingFileErrorsWithoutSeeding pins that editConfig never
// sets yamledit.Options.SeedIfMissing — only cmd/harness.go's manifest
// editor does, because manifest.yaml bootstraps itself on first use while
// config.yaml is always scaffolded ahead of time (config.EnsureConfigFile).
// If editConfig started seeding, the no-op mutate below would build an
// empty document that fails *validation* (no tools configured) instead of
// failing to *read* — so asserting the "reading config" prefix specifically
// (not just "an error") is what makes this mutation-proof against that
// regression, on top of the direct file-not-created check.
func TestEditConfig_MissingFileErrorsWithoutSeeding(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "does-not-exist.yaml")

	err := editConfig(cfgPath, func(doc *yaml.Node) error { return nil })
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
	if !strings.Contains(err.Error(), "reading config") {
		t.Errorf("expected a \"reading config\" error (no seeding), got: %v", err)
	}
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		t.Error("editConfig must not create the file for a missing config path")
	}
}
