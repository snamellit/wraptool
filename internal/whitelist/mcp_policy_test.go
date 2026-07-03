package whitelist

import (
	"testing"

	"github.com/pti/wraptool/internal/config"
)

func TestMCPPolicy_Evaluate(t *testing.T) {
	servers := map[string]*config.MCPServerConfig{
		"github": {
			Allow: []config.MCPAllowRule{
				{Tool: "create_issue"},
				{Tool: "get_file_contents"},
			},
			Deny: []config.MCPDenyRule{
				{Tool: "delete_repository"},
			},
		},
		// A server whose allow-listed tool is also deny-listed: deny wins.
		"filesystem": {
			Allow: []config.MCPAllowRule{
				{Tool: "read_file"},
				{Tool: "write_file"},
			},
			Deny: []config.MCPDenyRule{
				{Tool: "write_file"},
			},
		},
	}

	p := NewMCPPolicy(servers)

	tests := []struct {
		name   string
		server string
		tool   string
		wantOk bool
	}{
		{
			name:   "allow-listed tool is allowed",
			server: "github",
			tool:   "create_issue",
			wantOk: true,
		},
		{
			name:   "another allow-listed tool is allowed",
			server: "github",
			tool:   "get_file_contents",
			wantOk: true,
		},
		{
			name:   "default-deny: unlisted tool is denied",
			server: "github",
			tool:   "list_pull_requests",
			wantOk: false,
		},
		{
			name:   "deny-listed tool is denied",
			server: "github",
			tool:   "delete_repository",
			wantOk: false,
		},
		{
			name:   "deny overrides allow",
			server: "filesystem",
			tool:   "write_file",
			wantOk: false,
		},
		{
			name:   "allow-only tool on same server still allowed",
			server: "filesystem",
			tool:   "read_file",
			wantOk: true,
		},
		{
			name:   "unknown server is denied (default-deny)",
			server: "gitlab",
			tool:   "create_issue",
			wantOk: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Evaluate(tc.server, tc.tool)
			if got.Allowed != tc.wantOk {
				t.Fatalf("Evaluate(%q, %q).Allowed = %v, want %v",
					tc.server, tc.tool, got.Allowed, tc.wantOk)
			}
			if !got.Allowed && got.Reason == "" {
				t.Errorf("Evaluate(%q, %q) denied with empty Reason", tc.server, tc.tool)
			}
		})
	}
}

// TestMCPPolicy_EmptyAllowList confirms a server with no allow rules denies
// every tool (pure default-deny).
func TestMCPPolicy_EmptyAllowList(t *testing.T) {
	servers := map[string]*config.MCPServerConfig{
		"empty": {},
	}
	p := NewMCPPolicy(servers)

	d := p.Evaluate("empty", "anything")
	if d.Allowed {
		t.Fatalf("empty allow list should deny all tools, got Allowed=true")
	}
	if d.Reason == "" {
		t.Errorf("denial should carry a non-empty Reason")
	}
}

// TestMCPPolicy_Reload confirms hot-reload swaps the policy set.
func TestMCPPolicy_Reload(t *testing.T) {
	p := NewMCPPolicy(map[string]*config.MCPServerConfig{
		"github": {Allow: []config.MCPAllowRule{{Tool: "create_issue"}}},
	})

	if d := p.Evaluate("github", "create_issue"); !d.Allowed {
		t.Fatalf("pre-reload: create_issue should be allowed")
	}

	// Reload with a config that no longer allows create_issue but adds a new tool.
	p.Reload(map[string]*config.MCPServerConfig{
		"github": {Allow: []config.MCPAllowRule{{Tool: "get_file_contents"}}},
	})

	if d := p.Evaluate("github", "create_issue"); d.Allowed {
		t.Errorf("post-reload: create_issue should no longer be allowed")
	}
	if d := p.Evaluate("github", "get_file_contents"); !d.Allowed {
		t.Errorf("post-reload: get_file_contents should be allowed")
	}
}
