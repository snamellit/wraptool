package mcpproxy

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/mcp"
)

// Source is an mcptools.Source backed by one spawned stdio MCP server. It
// lazily spawns the upstream child on the first Tools() call, lists its tools,
// filters them through the MCP policy (default-deny), and registers each
// allowed tool with the upstream JSON Schema passed through unchanged.
type Source struct {
	serverName string
	cfg        *config.MCPServerConfig
	policy     *whitelist.MCPPolicy
	store      *requestlog.Store
	version    string

	mu     sync.Mutex
	client *Client
}

// compile-time assertion that Source satisfies the shared seam.
var _ mcptools.Source = (*Source)(nil)

// NewSource builds an MCP proxy source for one configured server. Nothing is
// spawned until Tools() is called.
func NewSource(serverName string, cfg *config.MCPServerConfig, policy *whitelist.MCPPolicy, store *requestlog.Store, version string) *Source {
	return &Source{
		serverName: serverName,
		cfg:        cfg,
		policy:     policy,
		store:      store,
		version:    version,
	}
}

// Tools spawns the upstream (if not already running), discovers its tools, and
// returns one RegisteredTool per allow-listed upstream tool. The tool's input
// schema is the upstream's, passed through unchanged (mcp.NewToolWithRawSchema),
// and its final name is "<name_prefix>_<upstream_tool>".
//
// A spawn/handshake/list failure is non-fatal: it is logged and the server
// contributes no tools, mirroring how CLI introspection failures are tolerated.
// This keeps a broken upstream from failing startup or a whole reload.
//
// TODO(phase2): re-list on upstream notifications/tools/list_changed (via
// Client.OnNotification) and propagate a re-registration up to the server.
func (s *Source) Tools(ctx context.Context) ([]mcptools.RegisteredTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil {
		c, err := NewClient(ctx, s.serverName, s.cfg, s.version)
		if err != nil {
			log.Printf("mcpproxy: skipping mcp server %q: %v", s.serverName, err)
			return nil, nil
		}
		s.client = c
	}

	upstream, err := s.client.ListTools(ctx)
	if err != nil {
		log.Printf("mcpproxy: listing tools for mcp server %q: %v", s.serverName, err)
		return nil, nil
	}

	var out []mcptools.RegisteredTool
	for _, ut := range upstream {
		if !s.policy.Evaluate(s.serverName, ut.Name).Allowed {
			continue // default-deny: only allow-listed tools are exposed.
		}
		raw, err := json.Marshal(ut.InputSchema)
		if err != nil {
			log.Printf("mcpproxy: skipping tool %q on %q: marshaling schema: %v", ut.Name, s.serverName, err)
			continue
		}
		finalName := s.cfg.NamePrefix + "_" + ut.Name
		tool := mcp.NewToolWithRawSchema(finalName, ut.Description, raw)
		h := &handler{
			client:     s.client,
			serverName: s.serverName,
			toolName:   ut.Name,
			policy:     s.policy,
			store:      s.store,
		}
		out = append(out, mcptools.RegisteredTool{
			Name:    finalName,
			Tool:    tool,
			Handler: h.Handle,
		})
	}
	return out, nil
}

// Close shuts down the upstream child process, if one is running.
func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	err := s.client.Close()
	s.client = nil
	return err
}
