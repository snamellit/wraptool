package whitelist

import (
	"testing"

	"github.com/pti/wraptool/internal/config"
)

func TestPolicy_AllowedCommand(t *testing.T) {
	tools := map[string]*config.ToolConfig{
		"git": {
			Allow: []config.AllowRule{
				{
					Subcommand: []string{"status"},
					Flags:      []string{"--short", "--branch"},
				},
				{
					Subcommand: []string{"commit"},
					Flags:      []string{"--message", "--amend"},
					FlagConstraints: map[string]config.FlagConstraint{
						"--message": {Required: true},
					},
				},
			},
		},
	}

	p := NewPolicy(tools)

	tests := []struct {
		name   string
		tool   string
		subcmd []string
		flags  map[string]string
		args   []string
		wantOk bool
	}{
		{
			name:   "allowed: git status",
			tool:   "git",
			subcmd: []string{"status"},
			flags:  map[string]string{"--short": ""},
			wantOk: true,
		},
		{
			name:   "allowed: git commit with message",
			tool:   "git",
			subcmd: []string{"commit"},
			flags:  map[string]string{"--message": "fix bug"},
			wantOk: true,
		},
		{
			name:   "denied: git commit without required --message",
			tool:   "git",
			subcmd: []string{"commit"},
			flags:  map[string]string{"--amend": ""},
			wantOk: false,
		},
		{
			name:   "denied: unknown subcommand",
			tool:   "git",
			subcmd: []string{"push"},
			flags:  map[string]string{},
			wantOk: false,
		},
		{
			name:   "denied: unlisted flag",
			tool:   "git",
			subcmd: []string{"status"},
			flags:  map[string]string{"--porcelain": ""},
			wantOk: false,
		},
		{
			name:   "denied: unknown tool",
			tool:   "kubectl",
			subcmd: []string{"get"},
			flags:  map[string]string{},
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.Evaluate(tt.tool, tt.subcmd, tt.flags, tt.args)
			if d.Allowed != tt.wantOk {
				t.Errorf("got Allowed=%v, want %v (reason: %s)", d.Allowed, tt.wantOk, d.Reason)
			}
		})
	}
}

// Help is an ordinary allowed flag: config load seeds -h/--help into every allow
// rule's Flags (config.SeedHelpFlags), so the policy engine permits them through
// the normal allowlist check with no special-casing. These tests seed the rules
// the same way load() does, then assert that orthogonal behaviour — including the
// deny_flags override and the fact that help does NOT bypass other checks.
func TestPolicy_HelpAlwaysAllowed(t *testing.T) {
	rules := []config.AllowRule{
		{Subcommand: []string{"status"}, Flags: []string{"--short"}},
		{
			Subcommand:      []string{"commit"},
			Flags:           []string{"--message"},
			FlagConstraints: map[string]config.FlagConstraint{"--message": {Required: true}},
		},
		// An explicit deny_flags on --help revokes the seeded help flag.
		{Subcommand: []string{"secret"}, Flags: []string{}, DenyFlags: []string{"--help"}},
	}
	// Seed exactly as config load does, so the test exercises the real path.
	for i := range rules {
		rules[i].Flags = config.SeedHelpFlags(rules[i].Flags, rules[i].DenyFlags)
	}
	p := NewPolicy(map[string]*config.ToolConfig{"git": {Allow: rules}})

	tests := []struct {
		name   string
		subcmd []string
		flags  map[string]string
		wantOk bool
	}{
		{"--help on allowed subcommand", []string{"status"}, map[string]string{"--help": ""}, true},
		{"-h short form", []string{"status"}, map[string]string{"-h": ""}, true},
		// Orthogonal: help is a normal allowed flag, so it does NOT short-circuit
		// other checks. A required-flag constraint still applies to a help-only
		// call; pass the required flag alongside --help to see usage.
		{"--help does not bypass a required-flag constraint", []string{"commit"}, map[string]string{"--help": ""}, false},
		{"--help with the required flag is allowed", []string{"commit"}, map[string]string{"--help": "", "--message": "x"}, true},
		// Orthogonal: help does not rescue a non-allowlisted flag in the same call.
		{"--help alongside a non-allowlisted flag is denied", []string{"status"}, map[string]string{"--help": "", "--bogus": "x"}, false},
		{"explicit deny_flags on --help still denies", []string{"secret"}, map[string]string{"--help": ""}, false},
		{"--help does not rescue a non-allowlisted subcommand", []string{"push"}, map[string]string{"--help": ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.Evaluate("git", tt.subcmd, tt.flags, nil)
			if d.Allowed != tt.wantOk {
				t.Errorf("got Allowed=%v, want %v (reason: %s)", d.Allowed, tt.wantOk, d.Reason)
			}
		})
	}
}

// TestSeedHelpFlags covers the single-source seeding helper directly.
func TestSeedHelpFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		deny  []string
		want  []string
	}{
		{"empty rule gets help", nil, nil, []string{"-h", "--help"}},
		{"configured flags kept, help appended", []string{"--short"}, nil, []string{"--short", "-h", "--help"}},
		{"deny revokes --help", nil, []string{"--help"}, []string{"-h"}},
		{"deny revokes both help flags", nil, []string{"-h", "--help"}, []string{}},
		{"already-present help not duplicated", []string{"--help", "--x"}, nil, []string{"--help", "--x", "-h"}},
		{"deny also drops a configured flag", []string{"--force"}, []string{"--force"}, []string{"-h", "--help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.SeedHelpFlags(tt.flags, tt.deny)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestPolicy_DenyRulesTakePrecedence(t *testing.T) {
	tools := map[string]*config.ToolConfig{
		"git": {
			Allow: []config.AllowRule{
				{
					Subcommand: []string{"remote", "add"},
					Flags:      []string{},
				},
			},
			Deny: []config.DenyRule{
				{Subcommand: []string{"remote", "add"}},
			},
		},
	}

	p := NewPolicy(tools)
	d := p.Evaluate("git", []string{"remote", "add"}, map[string]string{}, nil)

	if d.Allowed {
		t.Error("expected deny rule to take precedence over allow")
	}
}

func TestPolicy_DenyFlags(t *testing.T) {
	tools := map[string]*config.ToolConfig{
		"git": {
			Allow: []config.AllowRule{
				{
					Subcommand: []string{"push"},
					Flags:      []string{"--set-upstream"},
					DenyFlags:  []string{"--force"},
				},
			},
		},
	}

	p := NewPolicy(tools)

	d := p.Evaluate("git", []string{"push"}, map[string]string{"--force": ""}, nil)
	if d.Allowed {
		t.Error("expected --force to be denied")
	}

	d = p.Evaluate("git", []string{"push"}, map[string]string{"--set-upstream": ""}, nil)
	if !d.Allowed {
		t.Errorf("expected --set-upstream to be allowed, got: %s", d.Reason)
	}
}

func TestPolicy_Reload(t *testing.T) {
	tools := map[string]*config.ToolConfig{
		"git": {
			Allow: []config.AllowRule{
				{Subcommand: []string{"status"}, Flags: []string{}},
			},
		},
	}

	p := NewPolicy(tools)

	d := p.Evaluate("git", []string{"log"}, map[string]string{}, nil)
	if d.Allowed {
		t.Error("git log should not be allowed before reload")
	}

	// Reload with expanded config.
	newTools := map[string]*config.ToolConfig{
		"git": {
			Allow: []config.AllowRule{
				{Subcommand: []string{"status"}, Flags: []string{}},
				{Subcommand: []string{"log"}, Flags: []string{}},
			},
		},
	}
	p.Reload(newTools)

	d = p.Evaluate("git", []string{"log"}, map[string]string{}, nil)
	if !d.Allowed {
		t.Errorf("git log should be allowed after reload, got: %s", d.Reason)
	}
}
