package config

import (
	"os"
	"strings"
	"testing"
)

const rulesBaseConfig = `# config
server:
  transport: stdio
tools:
  git:
    binary: {{BIN}}
    allow:
      - subcommand: [status]
        flags: [--short]
        flag_constraints:
          --short:
            type: boolean
      - subcommand: [log]
    deny:
      - subcommand: [push]
`

func TestAddRule_Allow(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := AddRule(cfgPath, "git", KindAllow, []string{"commit"}, []string{"--message", "--amend"}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	got := readFile(t, cfgPath)
	if !strings.Contains(got, "[commit]") || !strings.Contains(got, "--amend") {
		t.Errorf("new allow rule missing:\n%s", got)
	}
	// Pre-existing comment survives the round-trip.
	if !strings.Contains(got, "# config") {
		t.Errorf("comment lost:\n%s", got)
	}
}

func TestAddRule_Deny(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// Flags are ignored for deny rules.
	if err := AddRule(cfgPath, "git", KindDeny, []string{"reset", "--hard"}, []string{"--ignored"}); err != nil {
		t.Fatalf("AddRule deny: %v", err)
	}
	got := readFile(t, cfgPath)
	if !strings.Contains(got, "reset") {
		t.Errorf("deny rule missing:\n%s", got)
	}
	if strings.Contains(got, "--ignored") {
		t.Errorf("flags should not be written for deny rules:\n%s", got)
	}
}

func TestAddRule_BadKind(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := AddRule(cfgPath, "git", "nonsense", []string{"x"}, nil); err == nil {
		t.Fatal("expected error for invalid kind")
	}
}

func TestAddRule_UnknownTool(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := AddRule(cfgPath, "kubectl", KindAllow, []string{"get"}, nil); err == nil {
		t.Fatal("expected error for unconfigured tool")
	}
}

func TestUpdateRule_FlagsAndConstraintPreserved(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// Update the status rule's flags; its flag_constraints block must survive.
	if err := UpdateRule(cfgPath, "git", KindAllow, 0, []string{"status"}, []string{"status"}, []string{"--short", "--branch"}); err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	got := readFile(t, cfgPath)
	if !strings.Contains(got, "--branch") {
		t.Errorf("added flag missing:\n%s", got)
	}
	if !strings.Contains(got, "flag_constraints") {
		t.Errorf("flag_constraints dropped on update:\n%s", got)
	}
}

func TestUpdateRule_EmptyFlagsRemovesKey(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := UpdateRule(cfgPath, "git", KindAllow, 0, []string{"status"}, []string{"status"}, nil); err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "--short") {
		t.Errorf("flags not removed:\n%s", got)
	}
}

func TestUpdateRule_DriftRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// expect says [commit] but rule 0 is [status] → refuse.
	err := UpdateRule(cfgPath, "git", KindAllow, 0, []string{"commit"}, []string{"commit"}, nil)
	if err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("expected drift error, got %v", err)
	}
}

func TestUpdateRule_IndexOutOfRange(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := UpdateRule(cfgPath, "git", KindAllow, 9, nil, []string{"x"}, nil); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestDeleteRule_Allow(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// Delete the [log] rule (index 1).
	if err := DeleteRule(cfgPath, "git", KindAllow, 1, []string{"log"}); err != nil {
		t.Fatalf("DeleteRule: %v", err)
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "[log]") {
		t.Errorf("log rule not deleted:\n%s", got)
	}
	if !strings.Contains(got, "[status]") {
		t.Errorf("status rule should remain:\n%s", got)
	}
}

func TestDeleteRule_Deny(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := DeleteRule(cfgPath, "git", KindDeny, 0, []string{"push"}); err != nil {
		t.Fatalf("DeleteRule deny: %v", err)
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "subcommand: [push]") {
		t.Errorf("deny rule not deleted:\n%s", got)
	}
}

func TestDeleteRule_AllAllowedTooInert(t *testing.T) {
	// A tool may be emptied of allow rules: it becomes inert (registers no MCP
	// tools, default-denies), a valid intermediate state, so deleting the last
	// allow rule is no longer refused.
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := DeleteRule(cfgPath, "git", KindAllow, 1, []string{"log"}); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := DeleteRule(cfgPath, "git", KindAllow, 0, []string{"status"}); err != nil {
		t.Fatalf("deleting last allow rule should be allowed now, got %v", err)
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "[status]") {
		t.Errorf("status rule should be gone:\n%s", got)
	}
}

func TestDeleteRule_DriftRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := DeleteRule(cfgPath, "git", KindAllow, 0, []string{"log"}); err == nil {
		t.Fatal("expected drift error")
	}
}

func TestSetFlagConstraint_ReplacesEntry(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// --short is boolean in the base config; retype it as integer with a range.
	min, max := 0, 10
	fc := FlagConstraint{Type: ConstraintInteger, Min: &min, Max: &max}
	if err := SetFlagConstraint(cfgPath, "git", 0, []string{"status"}, "--short", fc); err != nil {
		t.Fatalf("SetFlagConstraint: %v", err)
	}
	got := readFile(t, cfgPath)
	if !strings.Contains(got, "type: integer") {
		t.Errorf("type not replaced:\n%s", got)
	}
	if !strings.Contains(got, "min: 0") || !strings.Contains(got, "max: 10") {
		t.Errorf("min/max not written as ints:\n%s", got)
	}
	if strings.Contains(got, "type: boolean") {
		t.Errorf("old boolean constraint not replaced:\n%s", got)
	}
	if !strings.Contains(got, "# config") {
		t.Errorf("comment lost:\n%s", got)
	}
}

func TestSetFlagConstraint_AddsToNewFlag(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// Rule 0 already carries a flag_constraints block; add a second flag to it.
	// --branch must be an allowed flag or validation rejects the constraint.
	if err := UpdateRule(cfgPath, "git", KindAllow, 0, []string{"status"}, []string{"status"}, []string{"--short", "--branch"}); err != nil {
		t.Fatalf("UpdateRule: %v", err)
	}
	fc := FlagConstraint{Type: ConstraintString, Pattern: "*.log"}
	if err := SetFlagConstraint(cfgPath, "git", 0, []string{"status"}, "--branch", fc); err != nil {
		t.Fatalf("SetFlagConstraint: %v", err)
	}
	got := readFile(t, cfgPath)
	// A pattern that would misparse as a YAML alias (leading '*') must be quoted.
	if !strings.Contains(got, `"*.log"`) && !strings.Contains(got, "'*.log'") {
		t.Errorf("pattern not quoted:\n%s", got)
	}
	// Existing --short entry survives.
	if !strings.Contains(got, "--short:") {
		t.Errorf("existing constraint dropped:\n%s", got)
	}
}

func TestSetFlagConstraint_EmptyFlagRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := SetFlagConstraint(cfgPath, "git", 0, []string{"status"}, "", FlagConstraint{Type: ConstraintString}); err == nil {
		t.Fatal("expected error for empty flag name")
	}
}

func TestSetFlagConstraint_DriftRejected(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := SetFlagConstraint(cfgPath, "git", 0, []string{"log"}, "--short", FlagConstraint{Type: ConstraintString}); err == nil {
		t.Fatal("expected drift error")
	}
}

func TestDeleteFlagConstraint_RemovesBlockWhenEmpty(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	if err := DeleteFlagConstraint(cfgPath, "git", 0, []string{"status"}, "--short"); err != nil {
		t.Fatalf("DeleteFlagConstraint: %v", err)
	}
	got := readFile(t, cfgPath)
	if strings.Contains(got, "flag_constraints") {
		t.Errorf("empty flag_constraints block not removed:\n%s", got)
	}
}

func TestDeleteFlagConstraint_AbsentIsNoop(t *testing.T) {
	cfgPath, _ := writeTestConfig(t, rulesBaseConfig)
	// Rule 1 (log) has no constraints; deleting one must not error.
	if err := DeleteFlagConstraint(cfgPath, "git", 1, []string{"log"}, "--nope"); err != nil {
		t.Fatalf("DeleteFlagConstraint on absent entry: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
