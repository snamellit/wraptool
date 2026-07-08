package whitelist

import (
	"testing"

	"github.com/pti/wraptool/internal/config"
)

func TestCheckConstraints_IntegerRange(t *testing.T) {
	max100 := 100
	min1 := 1
	constraints := map[string]config.FlagConstraint{
		"--tail": {Type: "integer", Max: &max100, Min: &min1},
	}

	tests := []struct {
		name    string
		flags   map[string]string
		wantErr bool
	}{
		{"valid value", map[string]string{"--tail": "50"}, false},
		{"at max", map[string]string{"--tail": "100"}, false},
		{"at min", map[string]string{"--tail": "1"}, false},
		{"exceeds max", map[string]string{"--tail": "101"}, true},
		{"below min", map[string]string{"--tail": "0"}, true},
		{"not a number", map[string]string{"--tail": "abc"}, true},
		{"injection attempt", map[string]string{"--tail": "100; rm -rf /"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkConstraints(tt.flags, constraints)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkConstraints() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckConstraints_Required(t *testing.T) {
	constraints := map[string]config.FlagConstraint{
		"--message": {Required: true},
	}

	err := checkConstraints(map[string]string{}, constraints)
	if err == nil {
		t.Error("expected error for missing required flag")
	}

	err = checkConstraints(map[string]string{"--message": "hello"}, constraints)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCheckConstraints_Deny(t *testing.T) {
	constraints := map[string]config.FlagConstraint{
		"--follow": {Deny: true},
	}

	err := checkConstraints(map[string]string{"--follow": ""}, constraints)
	if err == nil {
		t.Error("expected error for denied flag")
	}

	err = checkConstraints(map[string]string{}, constraints)
	if err != nil {
		t.Errorf("unexpected error when flag is absent: %v", err)
	}
}

func TestCheckConstraints_Pattern(t *testing.T) {
	constraints := map[string]config.FlagConstraint{
		"--namespace": {Pattern: `^[a-z][a-z0-9-]*$`},
	}

	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"valid namespace", "my-namespace", false},
		{"valid simple", "default", false},
		{"starts with number", "123bad", true},
		{"contains uppercase", "Bad", true},
		{"contains special chars", "ns;drop", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkConstraints(map[string]string{"--namespace": tt.value}, constraints)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkConstraints() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckConstraints_PatternAnchored(t *testing.T) {
	// An unanchored pattern must still be enforced as an exact-value match, not a
	// substring match — otherwise `main` would admit `main-evil`. See F6.
	constraints := map[string]config.FlagConstraint{
		"--branch": {Pattern: `main`},
	}
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"exact match", "main", false},
		{"suffix injection", "main-evil", true},
		{"prefix injection", "evilmain", true},
		{"substring", "xmainx", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkConstraints(map[string]string{"--branch": tt.value}, constraints)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkConstraints() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	// A pattern with a top-level alternation must anchor both branches, not just
	// leave the last one open-ended.
	alt := map[string]config.FlagConstraint{"--mode": {Pattern: `read|write`}}
	if err := checkConstraints(map[string]string{"--mode": "read"}, alt); err != nil {
		t.Errorf("alternation branch should match exactly, got: %v", err)
	}
	if err := checkConstraints(map[string]string{"--mode": "writeXXL"}, alt); err == nil {
		t.Error("alternation must anchor every branch (writeXXL should fail)")
	}
}

func TestCheckConstraints_BooleanType(t *testing.T) {
	// A boolean flag is valueless: the handler renders it as a bare --flag,
	// reaching the policy with an empty value. It must be accepted, not rejected
	// as an "unknown constraint type".
	constraints := map[string]config.FlagConstraint{
		"--comments": {Type: "boolean"},
	}
	if err := checkConstraints(map[string]string{"--comments": ""}, constraints); err != nil {
		t.Errorf("a present boolean flag should be accepted, got: %v", err)
	}
	// Absent is fine too (not required).
	if err := checkConstraints(map[string]string{}, constraints); err != nil {
		t.Errorf("an absent boolean flag should be accepted, got: %v", err)
	}
}

func TestCheckConstraints_StringType(t *testing.T) {
	// An explicit type: string accepts any value (pattern, if set, still applies).
	constraints := map[string]config.FlagConstraint{
		"--title": {Type: "string"},
	}
	if err := checkConstraints(map[string]string{"--title": "anything goes"}, constraints); err != nil {
		t.Errorf("type: string should accept any value, got: %v", err)
	}

	patterned := map[string]config.FlagConstraint{
		"--title": {Type: "string", Pattern: `^[a-z]+$`},
	}
	if err := checkConstraints(map[string]string{"--title": "abc"}, patterned); err != nil {
		t.Errorf("value matching pattern should pass, got: %v", err)
	}
	if err := checkConstraints(map[string]string{"--title": "ABC"}, patterned); err == nil {
		t.Error("value violating pattern should fail even with type: string")
	}
}

func TestCheckConstraints_UnknownType(t *testing.T) {
	constraints := map[string]config.FlagConstraint{
		"--x": {Type: "float"},
	}
	if err := checkConstraints(map[string]string{"--x": "1.5"}, constraints); err == nil {
		t.Error("a genuinely unknown constraint type should still be rejected")
	}
}

func TestPolicy_ArgConstraints(t *testing.T) {
	tools := map[string]*config.ToolConfig{
		"git": {
			Allow: []config.AllowRule{
				{
					Subcommand: []string{"push"},
					Flags:      []string{},
					ArgConstraints: &config.ArgConstraints{
						PositionalMax: 2,
					},
				},
			},
		},
	}

	p := NewPolicy(tools)

	d := p.Evaluate("git", []string{"push"}, map[string]string{}, []string{"origin", "main"})
	if !d.Allowed {
		t.Errorf("2 args should be allowed: %s", d.Reason)
	}

	d = p.Evaluate("git", []string{"push"}, map[string]string{}, []string{"origin", "main", "extra"})
	if d.Allowed {
		t.Error("3 args should be denied (max 2)")
	}
}
