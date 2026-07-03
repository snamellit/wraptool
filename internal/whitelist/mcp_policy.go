package whitelist

import (
	"fmt"
	"sync"

	"github.com/pti/wraptool/internal/config"
)

// MCPPolicy evaluates upstream MCP tool calls against the configured
// mcp_servers allow/deny lists. It is a sibling to Policy (which is CLI
// subcommand/flag shaped): MCP policy is tool-name granularity only, with the
// same "default-deny, deny before allow" semantics.
type MCPPolicy struct {
	mu      sync.RWMutex
	servers map[string]*config.MCPServerConfig
}

// NewMCPPolicy builds a policy over the given servers, keyed by server name.
func NewMCPPolicy(servers map[string]*config.MCPServerConfig) *MCPPolicy {
	return &MCPPolicy{servers: servers}
}

// Reload atomically swaps the policy configuration for hot-reload symmetry
// with Policy.Reload.
func (m *MCPPolicy) Reload(servers map[string]*config.MCPServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers = servers
}

// Evaluate decides whether the named upstream tool on the named server may be
// called. Semantics mirror the CLI engine: default-deny (the server must be
// configured and the tool must be allow-listed), and deny rules take absolute
// precedence over allow rules.
func (m *MCPPolicy) Evaluate(server, tool string) Decision {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sc, ok := m.servers[server]
	if !ok {
		return Decision{
			Allowed: false,
			Reason:  fmt.Sprintf("mcp server %q is not configured", server),
		}
	}

	// Deny rules are evaluated first and take absolute precedence.
	for _, deny := range sc.Deny {
		if deny.Tool == tool {
			return Decision{
				Allowed: false,
				Reason:  fmt.Sprintf("tool %q is explicitly denied on mcp server %q", tool, server),
			}
		}
	}

	// Default-deny: the tool must be explicitly allow-listed.
	for _, allow := range sc.Allow {
		if allow.Tool == tool {
			return Decision{Allowed: true}
		}
	}

	return Decision{
		Allowed: false,
		Reason:  fmt.Sprintf("tool %q is not in the allowlist for mcp server %q", tool, server),
	}
}
