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
		name     string
		tool     string
		subcmd   []string
		flags    map[string]string
		args     []string
		wantOk   bool
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
