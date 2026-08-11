package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// policysubcmdMetaCases pairs each of this file's Meta literals with its
// expected ID — asserted directly against the SAME literal init() registers
// (plan §1 rule 8), never through Registered().
func TestPolicySubcmdMeta(t *testing.T) {
	cases := []struct {
		name string
		m    Meta
		id   CheckID
	}{
		{"shell-equivalent", policySubcmdShellEquivalentMeta, policySubcmdShellEquivalent},
		{"mutating", policySubcmdMutatingMeta, policySubcmdMutating},
		{"force-push", policySubcmdForcePushMeta, policySubcmdForcePush},
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

func policysubcmdInput(tools map[string]config.ToolConfig) *Input {
	return &Input{Env: NewMapEnv(), Config: &config.Config{Tools: tools}}
}

// ---------------------------------------------------------------- policy.shell-equivalent-subcommand

func TestRunPolicySubcmdShellEquivalent(t *testing.T) {
	tests := []struct {
		name       string
		tools      map[string]config.ToolConfig
		wantFinds  int
		wantDetail string
	}{
		{
			name: "kubectl exec: one-segment shell-equivalent shape matches",
			tools: map[string]config.ToolConfig{
				"kubectl": {Binary: "/usr/bin/kubectl", Allow: []config.AllowRule{{Subcommand: []string{"exec"}}}},
			},
			wantFinds:  1,
			wantDetail: "shell-equivalent",
		},
		{
			name: "gcloud compute ssh: two-segment shape matches",
			tools: map[string]config.ToolConfig{
				"gcloud": {Binary: "/usr/bin/gcloud", Allow: []config.AllowRule{{Subcommand: []string{"compute", "ssh"}}}},
			},
			wantFinds:  1,
			wantDetail: "compute ssh",
		},
		{
			name: "gcloud compute instances list: legitimate, does not match compute ssh",
			tools: map[string]config.ToolConfig{
				"gcloud": {Binary: "/usr/bin/gcloud", Allow: []config.AllowRule{{Subcommand: []string{"compute", "instances", "list"}}}},
			},
			wantFinds: 0,
		},
		{
			name: "kubectl get pods: legitimate, not a shell-equivalent subcommand",
			tools: map[string]config.ToolConfig{
				"kubectl": {Binary: "/usr/bin/kubectl", Allow: []config.AllowRule{{Subcommand: []string{"get", "pods"}}}},
			},
			wantFinds: 0,
		},
		{
			name: "unknown tool (glab): no table entry, no guess",
			tools: map[string]config.ToolConfig{
				"glab": {Binary: "/usr/bin/glab", Allow: []config.AllowRule{{Subcommand: []string{"exec"}}}},
			},
			wantFinds: 0,
		},
		{
			name:      "no tools at all",
			tools:     map[string]config.ToolConfig{},
			wantFinds: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runPolicySubcmdShellEquivalent(context.Background(), policysubcmdInput(tc.tools))
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
				t.Errorf("Severity = %v, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("empty Remedy on a warn finding")
			}
			if !strings.Contains(f.Detail, tc.wantDetail) {
				t.Errorf("Detail %q does not contain %q", f.Detail, tc.wantDetail)
			}
		})
	}
}

// ---------------------------------------------------------------- policy.mutating-subcommand

func TestRunPolicySubcmdMutating(t *testing.T) {
	tests := []struct {
		name       string
		tools      map[string]config.ToolConfig
		wantFinds  int
		wantDetail string
	}{
		{
			name: "kubectl delete: top-level mutating verb matches",
			tools: map[string]config.ToolConfig{
				"kubectl": {Binary: "/usr/bin/kubectl", Allow: []config.AllowRule{{Subcommand: []string{"delete"}}}},
			},
			wantFinds:  1,
			wantDetail: "delete",
		},
		{
			name: "gcloud compute instances delete: last-segment match through an unenumerable resource path",
			tools: map[string]config.ToolConfig{
				"gcloud": {Binary: "/usr/bin/gcloud", Allow: []config.AllowRule{{Subcommand: []string{"compute", "instances", "delete"}}}},
			},
			wantFinds:  1,
			wantDetail: "compute instances delete",
		},
		{
			name: "gcloud iam service-accounts delete: a DIFFERENT resource path, same verb, still matches",
			tools: map[string]config.ToolConfig{
				"gcloud": {Binary: "/usr/bin/gcloud", Allow: []config.AllowRule{{Subcommand: []string{"iam", "service-accounts", "delete"}}}},
			},
			wantFinds: 1,
		},
		{
			name: "terraform apply: matches",
			tools: map[string]config.ToolConfig{
				"terraform": {Binary: "/usr/bin/terraform", Allow: []config.AllowRule{{Subcommand: []string{"apply"}}}},
			},
			wantFinds: 1,
		},
		{
			name: "gcloud compute instances list: legitimate, list is not a mutating verb",
			tools: map[string]config.ToolConfig{
				"gcloud": {Binary: "/usr/bin/gcloud", Allow: []config.AllowRule{{Subcommand: []string{"compute", "instances", "list"}}}},
			},
			wantFinds: 0,
		},
		{
			name: "git pull: legitimate, git has no mutating-verb table entry at all",
			tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git", Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
			},
			wantFinds: 0,
		},
		{
			name: "unknown tool (glab): no table entry, no guess",
			tools: map[string]config.ToolConfig{
				"glab": {Binary: "/usr/bin/glab", Allow: []config.AllowRule{{Subcommand: []string{"delete"}}}},
			},
			wantFinds: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runPolicySubcmdMutating(context.Background(), policysubcmdInput(tc.tools))
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
				t.Errorf("Severity = %v, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("empty Remedy on a warn finding")
			}
			if tc.wantDetail != "" && !strings.Contains(f.Detail, tc.wantDetail) {
				t.Errorf("Detail %q does not contain %q", f.Detail, tc.wantDetail)
			}
		})
	}
}

// ---------------------------------------------------------------- policy.force-push-allowed

func TestRunPolicySubcmdForcePush(t *testing.T) {
	tests := []struct {
		name      string
		rule      config.AllowRule
		wantFinds int
		wantOpen  []string // substrings expected in Detail when wantFinds == 1
	}{
		{
			// Every force spelling is in the rule's own flags allowlist, so every
			// one is reachable and none is denied.
			name: "push allows all three force spellings, denies none: fires",
			rule: config.AllowRule{
				Subcommand: []string{"push"},
				Flags:      []string{"--force", "--force-with-lease", "-f"},
			},
			wantFinds: 1,
			wantOpen:  []string{"--force", "--force-with-lease", "-f"},
		},
		{
			name: "push allows all three, denies only --force: fires for the other two",
			rule: config.AllowRule{
				Subcommand: []string{"push"},
				Flags:      []string{"--force", "--force-with-lease", "-f"},
				DenyFlags:  []string{"--force"},
			},
			wantFinds: 1,
			wantOpen:  []string{"--force-with-lease", "-f"},
		},
		{
			// wraptool's OWN documented git push policy, printed verbatim in
			// docs/examples.qmd, docs/platform-linux.qmd, docs/platform-windows.qmd
			// and docs/presentation.qmd. No force spelling is in flags, so whitelist
			// rejects all three whatever deny_flags says.
			name: "the documented push policy: no force spelling is even allowed, silent",
			rule: config.AllowRule{
				Subcommand: []string{"push"},
				Flags:      []string{"--set-upstream"},
				DenyFlags:  []string{"--force", "--force-with-lease"},
			},
			wantFinds: 0,
		},
		{
			name:      "push with an empty flags allowlist: nothing is reachable, silent",
			rule:      config.AllowRule{Subcommand: []string{"push"}},
			wantFinds: 0,
		},
		{
			name: "push allows all three but denies all three: legitimate, silent",
			rule: config.AllowRule{
				Subcommand: []string{"push"},
				Flags:      []string{"--force", "--force-with-lease", "-f"},
				DenyFlags:  []string{"--force", "--force-with-lease", "-f"},
			},
			wantFinds: 0,
		},
		{
			name: "push allows all three, neutralized via flag_constraints.deny: legitimate, silent",
			rule: config.AllowRule{
				Subcommand: []string{"push"},
				Flags:      []string{"--force", "--force-with-lease", "-f"},
				FlagConstraints: map[string]config.FlagConstraint{
					"--force":            {Deny: true},
					"--force-with-lease": {Deny: true},
					"-f":                 {Deny: true},
				},
			},
			wantFinds: 0,
		},
		{
			name: "push allows all three, mixed deny_flags + flag_constraints: legitimate, silent",
			rule: config.AllowRule{
				Subcommand: []string{"push"},
				Flags:      []string{"--force", "--force-with-lease", "-f"},
				DenyFlags:  []string{"--force", "-f"},
				FlagConstraints: map[string]config.FlagConstraint{
					"--force-with-lease": {Deny: true},
				},
			},
			wantFinds: 0,
		},
		{
			name:      "git pull: not push at all, never considered",
			rule:      config.AllowRule{Subcommand: []string{"pull"}},
			wantFinds: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tools := map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git", Allow: []config.AllowRule{tc.rule}},
			}
			res := runPolicySubcmdForcePush(context.Background(), policysubcmdInput(tools))
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
				t.Errorf("Severity = %v, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("empty Remedy on a warn finding")
			}
			for _, want := range tc.wantOpen {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("Detail %q does not mention open spelling %q", f.Detail, want)
				}
			}
		})
	}
}

// TestPolicySubcmdOtherToolsIgnoredByForcePush confirms a non-git tool named
// "push" as its own subcommand (an unlikely but possible config) is never
// mistaken for git push — the check is gated on ToolBasename == "git" first.
func TestPolicySubcmdOtherToolsIgnoredByForcePush(t *testing.T) {
	tools := map[string]config.ToolConfig{
		"kubectl": {Binary: "/usr/bin/kubectl", Allow: []config.AllowRule{{Subcommand: []string{"push"}}}},
	}
	res := runPolicySubcmdForcePush(context.Background(), policysubcmdInput(tools))
	if res.Outcome() != OutcomePass {
		t.Errorf("outcome = %v, want pass (non-git tool never matches)", res.Outcome())
	}
}
