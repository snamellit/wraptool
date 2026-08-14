package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// policyFlagsMetaCases pairs each of this file's Meta literals with its
// expected ID, asserted directly against the SAME literal init() registers
// (plan §1 rule 8), never through Registered().
func TestPolicyFlagsMeta(t *testing.T) {
	cases := []struct {
		name string
		m    Meta
		id   CheckID
	}{
		{"unpatterned-value", policyFlagsUnpatternedValueMeta, policyFlagsUnpatternedValue},
		{"dangerous", policyFlagsDangerousMeta, policyFlagsDangerous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.m.ID != tc.id {
				t.Errorf("ID = %q, want %q", tc.m.ID, tc.id)
			}
			if tc.m.Needs != NeedsConfig {
				t.Errorf("Needs = %v, want NeedsConfig", tc.m.Needs)
			}
			if tc.m.Summary == "" {
				t.Errorf("empty Summary")
			}
		})
	}
}

// ---------------------------------------------------------------- policy.unpatterned-value-flag

func TestRunPolicyFlagsUnpatternedValue(t *testing.T) {
	tests := []struct {
		name       string
		tools      map[string]config.ToolConfig
		wantFinds  int
		wantDetail string
		wantPath   string
	}{
		{
			name: "declared string flag with no pattern: fires",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"fetch"},
						Flags:           []string{"--depth"},
						FlagConstraints: map[string]config.FlagConstraint{"--depth": {Type: config.ConstraintString}},
					}},
				},
			},
			wantFinds:  1,
			wantDetail: "--depth",
			wantPath:   "tools.git.allow[subcommand=fetch].flags[--depth]",
		},
		{
			name: "explicit string type, no pattern: fires (design's own literal case)",
			tools: map[string]config.ToolConfig{
				"glab": {
					Binary: "/usr/bin/glab",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"mr", "update"},
						Flags:           []string{"--reviewer"},
						FlagConstraints: map[string]config.FlagConstraint{"--reviewer": {Type: config.ConstraintString}},
					}},
				},
			},
			wantFinds:  1,
			wantDetail: "--reviewer",
		},
		{
			name: "pattern set: silent",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"checkout"},
						Flags:           []string{"--branch"},
						FlagConstraints: map[string]config.FlagConstraint{"--branch": {Pattern: "^[a-zA-Z0-9/_-]+$"}},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "integer type, no pattern: silent (non-string type closes the gap)",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"log"},
						Flags:           []string{"--max-count"},
						FlagConstraints: map[string]config.FlagConstraint{"--max-count": {Type: config.ConstraintInteger, Min: policyFlagsIntPtr(1), Max: policyFlagsIntPtr(100)}},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "explicit boolean type, no pattern: silent (not value-taking)",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"status"},
						Flags:           []string{"--short"},
						FlagConstraints: map[string]config.FlagConstraint{"--short": {Type: config.ConstraintBoolean}},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "well-known boolean switch name, no constraint at all: silent",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand: []string{"pull"},
						Flags:      []string{"--force", "--dry-run", "--quiet", "--yes", "-y"},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "seeded -h/--help present in Flags: silent (a naive scan must not fire on every rule's seeded help flags)",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand: []string{"pull"},
						Flags:      []string{"-h", "--help"},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "flag_constraints[f].deny=true: exempt even though value-taking and unpatterned",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"pull"},
						Flags:           []string{"--depth"},
						FlagConstraints: map[string]config.FlagConstraint{"--depth": {Deny: true}},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "two rules, two tools, unsorted map: deterministic sorted findings",
			tools: map[string]config.ToolConfig{
				"kubectl": {
					Binary: "/usr/bin/kubectl",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"logs"},
						Flags:           []string{"--tail"},
						FlagConstraints: map[string]config.FlagConstraint{"--tail": {Type: config.ConstraintString}},
					}},
				},
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"fetch"},
						Flags:           []string{"--depth"},
						FlagConstraints: map[string]config.FlagConstraint{"--depth": {Type: config.ConstraintString}},
					}},
				},
			},
			wantFinds: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &Input{
				Env:         NewMapEnv(),
				GOOS:        "linux",
				Config:      &config.Config{Tools: tc.tools},
				ConfigState: ConfigLoaded,
			}
			res := runPolicyFlagsUnpatternedValue(context.Background(), in)
			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("len(Findings) = %d, want %d (findings: %+v)", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("Severity = %q, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Errorf("Remedy is empty")
			}
			if tc.wantDetail != "" && !strings.Contains(f.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", f.Detail, tc.wantDetail)
			}
			if tc.wantPath != "" && f.Path.String() != tc.wantPath {
				t.Errorf("Path = %q, want %q", f.Path.String(), tc.wantPath)
			}
		})
	}
}

// ---------------------------------------------------------------- policy.dangerous-flag

func TestRunPolicyFlagsDangerous(t *testing.T) {
	tests := []struct {
		name      string
		tools     map[string]config.ToolConfig
		wantFinds int
		wantSev   Severity
	}{
		{
			name: "git --upload-pack, unpatterned: fires",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand: []string{"pull"},
						Flags:      []string{"--upload-pack"},
					}},
				},
			},
			wantFinds: 1,
			wantSev:   SeverityWarn,
		},
		{
			// executor.buildArgs emits flags after the subcommand, so this is
			// `git switch -c <branch>` -- a branch name, not git's global
			// config flag.
			name: "git switch -c: a branch name, silent",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand: []string{"switch"},
						Flags:      []string{"-c"},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "kubectl --token, unpatterned: fires",
			tools: map[string]config.ToolConfig{
				"kubectl": {
					Binary: "/usr/bin/kubectl",
					Allow: []config.AllowRule{{
						Subcommand: []string{"exec"},
						Flags:      []string{"--token"},
					}},
				},
			},
			wantFinds: 1,
		},
		{
			name: "gcloud --impersonate-service-account, unpatterned: fires",
			tools: map[string]config.ToolConfig{
				"gcloud": {
					Binary: "/usr/bin/gcloud",
					Allow: []config.AllowRule{{
						Subcommand: []string{"compute"},
						Flags:      []string{"--impersonate-service-account"},
					}},
				},
			},
			wantFinds: 1,
		},
		{
			name: "generic --*-cmd shape on a tool with no toolknowledge entry at all: fires",
			tools: map[string]config.ToolConfig{
				"glab": {
					Binary: "/usr/bin/glab",
					Allow: []config.AllowRule{{
						Subcommand: []string{"alias"},
						Flags:      []string{"--exec-cmd"},
					}},
				},
			},
			wantFinds: 1,
		},
		{
			name: "generic --*-file shape: fires",
			tools: map[string]config.ToolConfig{
				"glab": {
					Binary: "/usr/bin/glab",
					Allow: []config.AllowRule{{
						Subcommand: []string{"mr"},
						Flags:      []string{"--env-file"},
					}},
				},
			},
			wantFinds: 1,
		},
		{
			name: "not on any dangerous list: silent",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand: []string{"pull"},
						Flags:      []string{"--depth"},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "git -c with a pattern: silent",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"pull"},
						Flags:           []string{"-c"},
						FlagConstraints: map[string]config.FlagConstraint{"-c": {Pattern: "^user\\.(name|email)=.+$"}},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "git -c neutralized via deny_flags: silent (SeedHelpFlags removes it before this file ever sees it)",
			tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow: []config.AllowRule{{
						Subcommand: []string{"pull"},
						// A real load would have already dropped "-c" from
						// Flags via config.SeedHelpFlags; simulate that
						// post-default state directly rather than reaching
						// into internal/config from this file.
						Flags:     []string{},
						DenyFlags: []string{"-c"},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "kubectl --token neutralized via flag_constraints deny: true: silent",
			tools: map[string]config.ToolConfig{
				"kubectl": {
					Binary: "/usr/bin/kubectl",
					Allow: []config.AllowRule{{
						Subcommand:      []string{"exec"},
						Flags:           []string{"--token"},
						FlagConstraints: map[string]config.FlagConstraint{"--token": {Deny: true}},
					}},
				},
			},
			wantFinds: 0,
		},
		{
			name: "terraform has no dangerous flags of its own, and none here are generic-shaped: silent",
			tools: map[string]config.ToolConfig{
				"terraform": {
					Binary: "/usr/bin/terraform",
					Allow: []config.AllowRule{{
						Subcommand: []string{"plan"},
						Flags:      []string{"--refresh"},
					}},
				},
			},
			wantFinds: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &Input{
				Env:         NewMapEnv(),
				GOOS:        "linux",
				Config:      &config.Config{Tools: tc.tools},
				ConfigState: ConfigLoaded,
			}
			res := runPolicyFlagsDangerous(context.Background(), in)
			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("len(Findings) = %d, want %d (findings: %+v)", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				return
			}
			f := res.Findings[0]
			wantSev := tc.wantSev
			if wantSev == "" {
				wantSev = SeverityWarn
			}
			if f.Severity != wantSev {
				t.Errorf("Severity = %q, want %q", f.Severity, wantSev)
			}
			if f.Remedy == "" {
				t.Errorf("Remedy is empty")
			}
		})
	}
}

// ---------------------------------------------------------------- legitimate config: both checks stay silent

// TestPolicyFlagsLegitimateConfigStaysSilent is the STANDING SEVERITY RULE
// check applied to this file's own two warns: the most plausible hardened
// config this file's author could construct — every dangerous/value-taking
// flag either patterned, non-string typed, or neutralized via deny_flags/
// constraint deny, plus ordinary boolean switches and the seeded help flags
// — must produce ZERO findings from either check.
func TestPolicyFlagsLegitimateConfigStaysSilent(t *testing.T) {
	tools := map[string]config.ToolConfig{
		"git": {
			Binary: "/usr/bin/git",
			Allow: []config.AllowRule{
				{
					Subcommand: []string{"pull"},
					// "-c" already dropped from Flags by SeedHelpFlags at
					// load time because it's in this rule's own deny_flags.
					Flags:     []string{"--depth", "-h", "--help"},
					DenyFlags: []string{"-c"},
					FlagConstraints: map[string]config.FlagConstraint{
						"--depth": {Type: config.ConstraintInteger, Min: policyFlagsIntPtr(1), Max: policyFlagsIntPtr(1000)},
					},
				},
				{
					Subcommand: []string{"push"},
					Flags:      []string{"--force-with-lease", "-h", "--help"},
					FlagConstraints: map[string]config.FlagConstraint{
						"--force-with-lease": {Type: config.ConstraintBoolean},
					},
				},
			},
		},
		"kubectl": {
			Binary: "/usr/bin/kubectl",
			Allow: []config.AllowRule{{
				Subcommand: []string{"logs"},
				Flags:      []string{"--tail", "-h", "--help"},
				FlagConstraints: map[string]config.FlagConstraint{
					"--tail": {Type: config.ConstraintInteger},
				},
			}},
		},
		"gcloud": {
			Binary: "/usr/bin/gcloud",
			Allow: []config.AllowRule{{
				Subcommand: []string{"compute", "instances", "list"},
				Flags:      []string{"--impersonate-service-account", "-h", "--help"},
				FlagConstraints: map[string]config.FlagConstraint{
					"--impersonate-service-account": {Pattern: "^ci-runner@my-project\\.iam\\.gserviceaccount\\.com$"},
				},
			}},
		},
	}

	in := &Input{
		Env:         NewMapEnv(),
		GOOS:        "linux",
		Config:      &config.Config{Tools: tools},
		ConfigState: ConfigLoaded,
	}

	if res := runPolicyFlagsUnpatternedValue(context.Background(), in); len(res.Findings) != 0 {
		t.Errorf("unpatterned-value-flag: got %d findings on a legitimate config, want 0: %+v", len(res.Findings), res.Findings)
	}
	if res := runPolicyFlagsDangerous(context.Background(), in); len(res.Findings) != 0 {
		t.Errorf("dangerous-flag: got %d findings on a legitimate config, want 0: %+v", len(res.Findings), res.Findings)
	}
}

// ---------------------------------------------------------------- helper predicates

func TestPolicyFlagsDangerousReason(t *testing.T) {
	gitTK, _ := LookupToolKnowledge("git")
	terraformTK, _ := LookupToolKnowledge("terraform")

	tests := []struct {
		name        string
		tk          ToolKnowledge
		hasTK       bool
		flag        string
		wantMatch   bool
		wantReason  string
		wantNoMatch bool
	}{
		{name: "tool-specific match wins", tk: gitTK, hasTK: true, flag: "--upload-pack", wantMatch: true},
		// git -c is dangerous only BEFORE a subcommand, which executor.buildArgs
		// cannot emit; after one it is git switch's branch-name flag.
		{name: "a flag dangerous only pre-subcommand: no match", tk: gitTK, hasTK: true, flag: "-c", wantNoMatch: true},
		{name: "unknown tool, generic shape matches", tk: ToolKnowledge{}, hasTK: false, flag: "--config-file", wantMatch: true},
		{name: "unknown tool, no generic shape: no match", tk: ToolKnowledge{}, hasTK: false, flag: "--verbose", wantNoMatch: true},
		{name: "known tool with empty DangerousFlags, no generic shape: no match", tk: terraformTK, hasTK: true, flag: "--refresh", wantNoMatch: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, dangerous := policyFlagsDangerousReason(tc.tk, tc.hasTK, tc.flag)
			if dangerous != tc.wantMatch {
				t.Fatalf("dangerous = %v, want %v", dangerous, tc.wantMatch)
			}
			if tc.wantMatch && reason == "" {
				t.Errorf("dangerous=true but reason is empty")
			}
			if tc.wantNoMatch && reason != "" {
				t.Errorf("reason = %q, want empty when dangerous=false", reason)
			}
		})
	}
}

func TestPolicyFlagsValueTaking(t *testing.T) {
	tests := []struct {
		name        string
		flag        string
		constraints map[string]config.FlagConstraint
		want        bool
	}{
		// Untyped is not evidence of a value: most flags in a real config carry no
		// flag_constraints entry at all, and guessing here produced nine false
		// findings on an ordinary git status/log/diff policy.
		{name: "no constraint: unknown, so not confirmed", flag: "--depth", want: false},
		{name: "constraint without a type: unknown, so not confirmed", flag: "--depth", constraints: map[string]config.FlagConstraint{"--depth": {Max: policyFlagsIntPtr(100)}}, want: false},
		{name: "well-known boolean name", flag: "--force", want: false},
		{name: "explicit boolean type", flag: "--short", constraints: map[string]config.FlagConstraint{"--short": {Type: config.ConstraintBoolean}}, want: false},
		{name: "explicit string type", flag: "--branch", constraints: map[string]config.FlagConstraint{"--branch": {Type: config.ConstraintString}}, want: true},
		{name: "help flag", flag: "--help", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := policyFlagsValueTaking(tc.flag, tc.constraints); got != tc.want {
				t.Errorf("policyFlagsValueTaking(%q) = %v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}

func policyFlagsIntPtr(n int) *int { return &n }
