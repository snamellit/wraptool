package mcpproxy

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/toolname"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/mcp"
)

// Source is an mcptools.Source backed by one spawned stdio MCP server. It
// lazily spawns the upstream child on the first Tools() call, lists its tools,
// filters them through the MCP policy (default-deny), and registers each
// allowed tool with the upstream JSON Schema passed through unchanged.
type Source struct {
	serverName string
	policy     *whitelist.MCPPolicy
	store      *requestlog.Store
	version    string

	mu             sync.Mutex
	cfg            *config.MCPServerConfig
	client         *Client
	onToolsChanged func() // set by the registrar; re-registers this source's tools
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
// and its final name comes from toolname.Proxy (see there for why an
// upstream's own naming convention has to be sanitized). Only the EXPOSED name
// is rewritten — the handler still calls the upstream by its own unmodified
// ut.Name, so dispatch is unaffected.
//
// A spawn/handshake/list failure is non-fatal: it is logged and the server
// contributes no tools, mirroring how CLI introspection failures are tolerated.
// This keeps a broken (or crashlooping) upstream from failing startup or a
// whole reload.
//
// Tools is also the re-registration path: the registrar re-invokes it (via the
// onToolsChanged callback) after an upstream tools/list_changed notification or
// a crash respawn, so the freshly-listed tool set replaces the stale one.
func (s *Source) Tools(ctx context.Context) ([]mcptools.RegisteredTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil {
		c, err := NewClient(ctx, s.serverName, s.cfg, s.version)
		if err != nil {
			log.Printf("mcpproxy: skipping mcp server %q: %v", s.serverName, err)
			return nil, nil
		}
		c.setOnToolsChanged(s.onToolsChanged)
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
		finalName := toolname.Proxy(s.cfg.NamePrefix, ut.Name)
		if joined := s.cfg.NamePrefix + "_" + ut.Name; joined != finalName {
			// The operator sees the rewritten name in their client but greps
			// their config for the upstream's own spelling, so say both once
			// per registration rather than leaving the mapping invisible.
			log.Printf("mcpproxy: %s: exposing upstream tool %q as %q (the joined name %q is not a valid tool name)", s.serverName, ut.Name, finalName, joined)
		}
		tool := mcp.NewToolWithRawSchema(finalName, ut.Description, raw)
		h := &handler{
			client:     s.client,
			serverName: s.serverName,
			toolName:   ut.Name,
			policy:     s.policy,
			store:      s.store,
		}
		out = append(out, mcptools.NewRegisteredTool(tool, h.Handle))
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

// OnToolsChanged installs the callback the source invokes (via its Client) when
// the upstream tool set may have changed — an upstream tools/list_changed
// notification or a crash respawn. The registrar sets it to re-register just
// this source's tools. It is wired onto the live Client too, so it takes effect
// even if the child is already running.
func (s *Source) OnToolsChanged(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onToolsChanged = fn
	if s.client != nil {
		s.client.setOnToolsChanged(fn)
	}
}

// SetConfig swaps the server config used to build tool names on the next
// Tools() call. It is used by the reload reconciler for a policy-only change
// (e.g. a changed name_prefix or allow/deny set) where the child does not need
// to restart — only the registrations are refreshed. It must not be used to
// change spawn parameters of a live child; the reconciler recreates the source
// for those.
func (s *Source) SetConfig(cfg *config.MCPServerConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}
