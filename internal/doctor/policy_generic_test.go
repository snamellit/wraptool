package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// policygenericMetaCases pairs each of this file's Meta literals with its
// expected ID/Needs — asserted directly against the SAME literal init()
// registers (plan §1 rule 8), never through Registered(), which by the time
// this package's Phase-B/C/D siblings have merged also carries every one of
// THEIR checks.
func TestPolicygenericMeta(t *testing.T) {
	cases := []struct {
		name string
		m    Meta
		id   CheckID
	}{
		{"uncompilable-pattern", policygenericUncompilablePatternMeta, policygenericUncompilablePattern},
		{"overbroad-pattern", policygenericOverbroadPatternMeta, policygenericOverbroadPattern},
		{"unbounded-positional-count", policygenericUnboundedPositionalCountMeta, policygenericUnboundedPositionalCount},
		{"deny-prefix-gap", policygenericDenyPrefixGapMeta, policygenericDenyPrefixGap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.m.ID != tc.id {
				t.Errorf("ID = %q, want %q", tc.m.ID, tc.id)
			}
			if tc.m.Needs != NeedsConfig {
				t.Errorf("Needs = %v, want NeedsConfig", tc.m.Needs)
			}
			if strings.TrimSpace(tc.m.Summary) == "" {
				t.Errorf("empty Summary")
			}
		})
	}
}

// ---------------------------------------------------------------- policygenericConstraintReachable

func TestPolicygenericConstraintReachable(t *testing.T) {
	base := config.AllowRule{
		Flags:     []string{"-c"},
		DenyFlags: nil,
	}
	tests := []struct {
		name string
		rule config.AllowRule
		flag string
		c    config.FlagConstraint
		want bool
	}{
		{"reachable string type", base, "-c", config.FlagConstraint{Type: config.ConstraintString}, true},
		{"reachable untyped", base, "-c", config.FlagConstraint{}, true},
		{"reachable integer type", base, "-c", config.FlagConstraint{Type: config.ConstraintInteger}, true},
		{"unreachable: boolean type", base, "-c", config.FlagConstraint{Type: config.ConstraintBoolean}, false},
		{"unreachable: constraint denies", base, "-c", config.FlagConstraint{Deny: true}, false},
		{
			"unreachable: flag itself deny_flags'd",
			config.AllowRule{Flags: []string{"-c"}, DenyFlags: []string{"-c"}},
			"-c", config.FlagConstraint{}, false,
		},
		{
			"unreachable: flag not in the rule's own Flags at all (orphan)",
			config.AllowRule{Flags: []string{"--other"}},
			"-c", config.FlagConstraint{}, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := policygenericConstraintReachable(tc.rule, tc.flag, tc.c); got != tc.want {
				t.Errorf("policygenericConstraintReachable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- policy.uncompilable-pattern

func TestRunPolicygenericUncompilablePattern(t *testing.T) {
	tests := []struct {
		name       string
		tools      map[string]config.ToolConfig
		wantFinds  int
		wantSev    Severity
		wantDetail string
	}{
		{
			name: "legitimate config with no flag_constraints at all: silent",
			tools: map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
			},
			wantFinds: 0,
		},
		{
			name: "a valid compiling pattern: silent",
			tools: map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{
					Subcommand:      []string{"push"},
					Flags:           []string{"--force-with-lease"},
					FlagConstraints: map[string]config.FlagConstraint{"--force-with-lease": {Pattern: "^[a-z]+$"}},
				}}},
			},
			wantFinds: 0,
		},
		{
			name: "an unterminated group does not compile even anchored: fail",
			tools: map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{
					Subcommand:      []string{"push"},
					Flags:           []string{"-c"},
					FlagConstraints: map[string]config.FlagConstraint{"-c": {Pattern: "(unterminated"}},
				}}},
			},
			wantFinds:  1,
			wantSev:    SeverityFail,
			wantDetail: "does not compile",
		},
		{
			name: "uncompilable pattern on a boolean-typed constraint is dead code: silent",
			tools: map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{
					Subcommand: []string{"push"},
					Flags:      []string{"--dry-run"},
					FlagConstraints: map[string]config.FlagConstraint{
						"--dry-run": {Type: config.ConstraintBoolean, Pattern: "(unterminated"},
					},
				}}},
			},
			wantFinds: 0,
		},
		{
			name: "uncompilable pattern on a deny:true constraint is unreachable: silent",
			tools: map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{
					Subcommand:      []string{"push"},
					Flags:           []string{"-c"},
					FlagConstraints: map[string]config.FlagConstraint{"-c": {Deny: true, Pattern: "(unterminated"}},
				}}},
			},
			wantFinds: 0,
		},
		{
			name: "uncompilable pattern on an orphaned flag_constraints entry: silent (config.constraint-orphan's own domain)",
			tools: map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{
					Subcommand:      []string{"push"},
					Flags:           []string{"--force"},
					FlagConstraints: map[string]config.FlagConstraint{"-c": {Pattern: "(unterminated"}},
				}}},
			},
			wantFinds: 0,
		},
		{
			name: "uncompilable pattern on a deny_flags'd flag: silent",
			tools: map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{
					Subcommand:      []string{"push"},
					Flags:           []string{"-c"},
					DenyFlags:       []string{"-c"},
					FlagConstraints: map[string]config.FlagConstraint{"-c": {Pattern: "(unterminated"}},
				}}},
			},
			wantFinds: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &Input{Env: NewMapEnv(), Config: &config.Config{Tools: tc.tools}}
			res := runPolicygenericUncompilablePattern(context.Background(), in)
			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("got %d findings, want %d: %+v", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				if res.Outcome() != OutcomePass {
					t.Errorf("outcome = %v, want pass", res.Outcome())
				}
				return
			}
			f := res.Findings[0]
			if f.Severity != tc.wantSev {
				t.Errorf("severity = %v, want %v", f.Severity, tc.wantSev)
			}
			if !strings.Contains(f.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", f.Detail, tc.wantDetail)
			}
			if f.Remedy == "" {
				t.Errorf("Remedy is empty for a fail finding")
			}
		})
	}
}

func TestRunPolicygenericUncompilablePattern_PathAndDeterministicOrder(t *testing.T) {
	tools := map[string]config.ToolConfig{
		"zeta": {Allow: []config.AllowRule{{
			Subcommand:      []string{"go"},
			Flags:           []string{"-c"},
			FlagConstraints: map[string]config.FlagConstraint{"-c": {Pattern: "(bad"}},
		}}},
		"alpha": {Allow: []config.AllowRule{{
			Subcommand:      []string{"go"},
			Flags:           []string{"-c"},
			FlagConstraints: map[string]config.FlagConstraint{"-c": {Pattern: "(bad"}},
		}}},
	}
	in := &Input{Env: NewMapEnv(), Config: &config.Config{Tools: tools}}

	res := runPolicygenericUncompilablePattern(context.Background(), in)
	if len(res.Findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(res.Findings))
	}
	if res.Findings[0].Path.String() != At("tools", "alpha", "allow").Item("subcommand", "go").
		Key("flag_constraints", "-c", "pattern").String() {
		t.Errorf("finding[0].Path = %q, want alpha first (sorted tool names)", res.Findings[0].Path.String())
	}
	if res.Findings[1].Path.String() != At("tools", "zeta", "allow").Item("subcommand", "go").
		Key("flag_constraints", "-c", "pattern").String() {
		t.Errorf("finding[1].Path = %q, want zeta second", res.Findings[1].Path.String())
	}
}

// ---------------------------------------------------------------- policy.overbroad-pattern

func TestRunPolicygenericOverbroadPattern(t *testing.T) {
	tests := []struct {
		name      string
		pattern   string
		wantFinds int
	}{
		{"dot-star", ".*", 1},
		{"dot-plus", ".+", 1},
		{"lazy dot-star", ".*?", 1},
		{"dotall dot-star", "(?s).*", 1},
		// An absent pattern is indistinguishable from an explicitly empty one at
		// the struct level, and docs/getting-started.qmd's own
		// {type: integer, max: 100} constraint has no pattern while constraining
		// its value perfectly well. This check speaks only about patterns that
		// were actually written.
		{"no pattern at all: silent, that is unpatterned-value-flag's subject", "", 0},
		{"a real, specific pattern: silent", "^[a-z]+$", 0},
		{"an unanchored but non-degenerate pattern: silent (F6 anchors it)", "main", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tools := map[string]config.ToolConfig{
				"git": {Allow: []config.AllowRule{{
					Subcommand:      []string{"checkout"},
					Flags:           []string{"-b"},
					FlagConstraints: map[string]config.FlagConstraint{"-b": {Pattern: tc.pattern}},
				}}},
			}
			in := &Input{Env: NewMapEnv(), Config: &config.Config{Tools: tools}}
			res := runPolicygenericOverbroadPattern(context.Background(), in)
			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("got %d findings, want %d: %+v", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				if res.Outcome() != OutcomePass {
					t.Errorf("outcome = %v, want pass", res.Outcome())
				}
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Errorf("Remedy is empty for a warn finding")
			}
		})
	}
}

// TestRunPolicygenericOverbroadPattern_Unreachable mirrors the uncompilable-
// pattern reachability cases: an overbroad pattern that whitelist can never
// evaluate must not be reported as leaving the flag "unconstrained" — that
// claim is only true of a flag a call can actually carry.
func TestRunPolicygenericOverbroadPattern_Unreachable(t *testing.T) {
	tests := []struct {
		name  string
		rule  config.AllowRule
		flags map[string]config.FlagConstraint
	}{
		{
			"boolean type",
			config.AllowRule{Flags: []string{"--dry-run"}},
			map[string]config.FlagConstraint{"--dry-run": {Type: config.ConstraintBoolean, Pattern: ".*"}},
		},
		{
			"deny:true",
			config.AllowRule{Flags: []string{"-c"}},
			map[string]config.FlagConstraint{"-c": {Deny: true, Pattern: ".*"}},
		},
		{
			"orphaned (not in Flags)",
			config.AllowRule{Flags: []string{"--force"}},
			map[string]config.FlagConstraint{"-c": {Pattern: ".*"}},
		},
		{
			"deny_flags'd",
			config.AllowRule{Flags: []string{"-c"}, DenyFlags: []string{"-c"}},
			map[string]config.FlagConstraint{"-c": {Pattern: ".*"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule := tc.rule
			rule.Subcommand = []string{"push"}
			rule.FlagConstraints = tc.flags
			in := &Input{Env: NewMapEnv(), Config: &config.Config{
				Tools: map[string]config.ToolConfig{"git": {Allow: []config.AllowRule{rule}}},
			}}
			res := runPolicygenericOverbroadPattern(context.Background(), in)
			if res.Outcome() != OutcomePass {
				t.Errorf("outcome = %v, want pass (unreachable pattern must not be flagged): %+v", res.Outcome(), res.Findings)
			}
		})
	}
}

// ---------------------------------------------------------------- policy.unbounded-positional-count

func TestRunPolicygenericUnboundedPositionalCount(t *testing.T) {
	tests := []struct {
		name      string
		rule      config.AllowRule
		wantFinds int
	}{
		{
			"no arg_constraints at all: warn",
			config.AllowRule{Subcommand: []string{"pull"}},
			1,
		},
		{
			"arg_constraints.positional_max set to zero: silent (explicitly bounded)",
			config.AllowRule{Subcommand: []string{"pull"}, ArgConstraints: &config.ArgConstraints{PositionalMax: 0}},
			0,
		},
		{
			"arg_constraints.positional_max set to a real bound: silent",
			config.AllowRule{Subcommand: []string{"pull"}, ArgConstraints: &config.ArgConstraints{PositionalMax: 2}},
			0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &Input{Env: NewMapEnv(), Config: &config.Config{
				Tools: map[string]config.ToolConfig{"git": {Allow: []config.AllowRule{tc.rule}}},
			}}
			res := runPolicygenericUnboundedPositionalCount(context.Background(), in)
			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("got %d findings, want %d: %+v", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if !strings.Contains(f.Detail, "COUNT ONLY") {
				t.Errorf("Detail = %q, must plainly say this bounds COUNT ONLY", f.Detail)
			}
			if strings.Contains(f.Detail, "arg_constraints closes") {
				t.Errorf("Detail must not imply arg_constraints closes the open gap: %q", f.Detail)
			}
			// The finding is per TOOL, not per rule, so it points at the allow
			// block rather than one rule inside it.
			wantPath := At("tools", "git", "allow").String()
			if f.Path.String() != wantPath {
				t.Errorf("Path = %q, want %q", f.Path.String(), wantPath)
			}
		})
	}
}

// ---------------------------------------------------------------- policy.deny-prefix-gap

func TestRunPolicygenericDenyPrefixGap(t *testing.T) {
	tests := []struct {
		name      string
		tc        config.ToolConfig
		wantFinds int
	}{
		{
			name: "deny is a strict prefix of a longer allowed subcommand: warn",
			tc: config.ToolConfig{
				Deny:  []config.DenyRule{{Subcommand: []string{"remote"}}},
				Allow: []config.AllowRule{{Subcommand: []string{"remote", "set-url"}}},
			},
			wantFinds: 1,
		},
		{
			name: "deny exactly matches the allowed subcommand's full length: silent (exact match denies it fully)",
			tc: config.ToolConfig{
				Deny:  []config.DenyRule{{Subcommand: []string{"remote", "set-url"}}},
				Allow: []config.AllowRule{{Subcommand: []string{"remote", "set-url"}}},
			},
			wantFinds: 0,
		},
		{
			name: "deny is LONGER than the allow rule: not a prefix relation, silent",
			tc: config.ToolConfig{
				Deny:  []config.DenyRule{{Subcommand: []string{"push", "--force"}}},
				Allow: []config.AllowRule{{Subcommand: []string{"push"}}},
			},
			wantFinds: 0,
		},
		{
			name: "deny with no allow rule extending it at all: silent (legitimate, fully-scoped deny)",
			tc: config.ToolConfig{
				Deny:  []config.DenyRule{{Subcommand: []string{"remote"}}},
				Allow: []config.AllowRule{{Subcommand: []string{"pull"}}},
			},
			wantFinds: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &Input{Env: NewMapEnv(), Config: &config.Config{Tools: map[string]config.ToolConfig{"git": tc.tc}}}
			res := runPolicygenericDenyPrefixGap(context.Background(), in)
			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("got %d findings, want %d: %+v", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				if res.Outcome() != OutcomePass {
					t.Errorf("outcome = %v, want pass", res.Outcome())
				}
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Errorf("Remedy is empty for a warn finding")
			}
			wantPath := At("tools", "git", "deny").Item("subcommand", strings.Join(tc.tc.Deny[0].Subcommand, " ")).String()
			if f.Path.String() != wantPath {
				t.Errorf("Path = %q, want %q", f.Path.String(), wantPath)
			}
		})
	}
}

func TestPolicygenericIsStrictPrefix(t *testing.T) {
	tests := []struct {
		name         string
		prefix, full []string
		want         bool
	}{
		{"strict, shorter prefix", []string{"remote"}, []string{"remote", "set-url"}, true},
		{"equal length: not strict", []string{"remote", "set-url"}, []string{"remote", "set-url"}, false},
		{"prefix longer than full", []string{"remote", "set-url"}, []string{"remote"}, false},
		{"empty prefix never counts", nil, []string{"remote"}, false},
		{"diverging tokens", []string{"remote"}, []string{"branch", "list"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := policygenericIsStrictPrefix(tc.prefix, tc.full); got != tc.want {
				t.Errorf("policygenericIsStrictPrefix(%v, %v) = %v, want %v", tc.prefix, tc.full, got, tc.want)
			}
		})
	}
}

// TestDenyPrefixGapExemptsAnExactDeny pins the exemption that separates a real
// gap from a hardened policy: when the longer subcommand is itself denied at
// exact length, whitelist matches that rule, the allow is dead, and the gap the
// check exists to report is already closed. Warning there nags an operator for
// having done precisely what the remedy asks — usually on the next line of the
// same file.
func TestDenyPrefixGapExemptsAnExactDeny(t *testing.T) {
	rules := func(deny ...[]string) map[string]config.ToolConfig {
		var d []config.DenyRule
		for _, s := range deny {
			d = append(d, config.DenyRule{Subcommand: s})
		}
		return map[string]config.ToolConfig{"git": {
			Allow: []config.AllowRule{{Subcommand: []string{"remote", "set-url"}}},
			Deny:  d,
		}}
	}

	gap := runPolicygenericDenyPrefixGap(context.Background(),
		&Input{Env: NewMapEnv(), Config: &config.Config{Tools: rules([]string{"remote"})}})
	if len(gap.Findings) != 1 {
		t.Fatalf("an unclosed prefix gap: got %d findings, want 1: %+v", len(gap.Findings), gap.Findings)
	}

	closed := runPolicygenericDenyPrefixGap(context.Background(),
		&Input{Env: NewMapEnv(), Config: &config.Config{Tools: rules([]string{"remote"}, []string{"remote", "set-url"})}})
	if len(closed.Findings) != 0 {
		t.Errorf("the longer subcommand is denied at exact length, so the gap is closed: got %+v", closed.Findings)
	}
}

// TestUnboundedPositionalCountAggregatesPerTool pins the aggregation: a real
// config rarely sets arg_constraints anywhere, so one finding per allow rule
// produced a wall of near-identical warnings that buried the findings naming a
// specific, fixable hole.
func TestUnboundedPositionalCountAggregatesPerTool(t *testing.T) {
	subs := []string{"push", "pull", "fetch", "branch", "checkout", "switch", "status"}
	var rules []config.AllowRule
	for _, s := range subs {
		rules = append(rules, config.AllowRule{Subcommand: []string{s}})
	}
	in := &Input{Env: NewMapEnv(), Config: &config.Config{
		Tools: map[string]config.ToolConfig{"git": {Allow: rules}},
	}}

	res := runPolicygenericUnboundedPositionalCount(context.Background(), in)
	if len(res.Findings) != 1 {
		t.Fatalf("got %d findings for one tool, want 1: %+v", len(res.Findings), res.Findings)
	}
	d := res.Findings[0].Detail
	if !strings.Contains(d, "7 of tools.git's allow rules") {
		t.Errorf("Detail does not lead with the count: %q", d)
	}
	if !strings.Contains(d, "and 2 more") {
		t.Errorf("Detail does not fold the remainder past the cap: %q", d)
	}
}

// TestRunPolicygenericDenyFlagAsSubcommand pins the fail-open shape this check
// exists for: a deny rule reading like it blocks `git config --global` that in
// fact matches no call that can exist, because whitelist compares deny rules
// against subcommand tokens and a flag is never one.
func TestRunPolicygenericDenyFlagAsSubcommand(t *testing.T) {
	cases := []struct {
		name      string
		deny      [][]string
		wantFinds int
		wantIn    string
	}{
		{
			name:      "a flag as the second segment: fires",
			deny:      [][]string{{"config", "--global"}},
			wantFinds: 1,
			wantIn:    `"--global"`,
		},
		{
			name:      "a short flag: fires",
			deny:      [][]string{{"push", "-f"}},
			wantFinds: 1,
			wantIn:    `"-f"`,
		},
		{
			name:      "a genuine two-segment subcommand: silent",
			deny:      [][]string{{"remote", "set-url"}},
			wantFinds: 0,
		},
		{
			name:      "a single-segment deny: silent",
			deny:      [][]string{{"clone"}},
			wantFinds: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var deny []config.DenyRule
			for _, d := range tc.deny {
				deny = append(deny, config.DenyRule{Subcommand: d})
			}
			in := &Input{Env: NewMapEnv(), Config: &config.Config{
				Tools: map[string]config.ToolConfig{"git": {Deny: deny}},
			}}

			res := runPolicygenericDenyFlagAsSubcommand(context.Background(), in)
			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("got %d findings, want %d: %+v", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if !strings.Contains(f.Detail, tc.wantIn) {
				t.Errorf("Detail %q does not name the offending segment %s", f.Detail, tc.wantIn)
			}
			if !strings.Contains(f.Remedy, "deny_flags") {
				t.Errorf("Remedy must name deny_flags as the mechanism that actually blocks a flag: %q", f.Remedy)
			}
		})
	}
}
