// Package mcpproxy connects to and polices local stdio MCP servers that
// wraptool spawns itself. Each upstream server is a child process launched with
// executor.ChildEnv (never inheriting wraptool's own environment), giving it
// the same credential-isolation profile as a wrapped CLI tool. Its tools are
// exposed through the shared mcptools.Source seam, with policy, scrubbing, and
// audit applied identically to the CLI path.
//
// This is the Phase 1 MVP: one spawn, Initialize, ListTools, and CallTool. It
// declares no client capabilities to the upstream (no sampling/roots/
// elicitation), so an upstream can only answer tools/call. There is no
// supervision or restart yet — that is Phase 2.
package mcpproxy

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Client is a thin lifecycle wrapper around one spawned stdio MCP server: it
// holds the connected upstream client, the server's resolved env (the scrub
// secret set), and the per-call timeout. It does not embed policy or audit —
// those live in the handler — so it stays a pure transport concern.
type Client struct {
	serverName  string
	upstream    *client.Client
	resolvedEnv map[string]string
	timeout     time.Duration
}

// NewClient spawns the configured stdio MCP server, performs the MCP handshake
// (declaring no client capabilities), and returns a ready Client. The child's
// environment is built with executor.ChildEnv from the server's resolved env,
// so it is the minimal curated env — wraptool's own environment is never
// inherited. The handshake is bounded by cfg.InitTimeout.
func NewClient(ctx context.Context, serverName string, cfg *config.MCPServerConfig, version string) (*Client, error) {
	resolvedEnv, err := cfg.ResolvedEnv()
	if err != nil {
		return nil, fmt.Errorf("resolving env for mcp server %q: %w", serverName, err)
	}

	// Full replacement environment — same builder as CLI tools, so isolation is
	// identical. NOTE: the default mcp-go stdio transport sets
	// cmd.Env = append(os.Environ(), env...), which would leak wraptool's own
	// environment into the child. We pass a custom command factory
	// (isolatedCommand) so the child gets *exactly* this curated env and nothing
	// inherited — otherwise the credential-isolation guarantee is void.
	env := executor.ChildEnv(cfg.Command, resolvedEnv)
	up, err := client.NewStdioMCPClientWithOptions(cfg.Command, env, cfg.Args,
		transport.WithCommandFunc(isolatedCommand))
	if err != nil {
		return nil, fmt.Errorf("spawning mcp server %q: %w", serverName, err)
	}

	// Start wires the notification handler; call it before Initialize.
	if err := up.Start(ctx); err != nil {
		_ = up.Close()
		return nil, fmt.Errorf("starting mcp server %q: %w", serverName, err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ClientInfo = mcp.Implementation{Name: "wraptool", Version: version}
	// ProtocolVersion left empty → mcp-go defaults to the latest.
	// Capabilities left zero → declare none (no sampling/roots/elicitation).

	initTimeout := cfg.InitTimeout
	if initTimeout == 0 {
		initTimeout = 10 * time.Second
	}
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if _, err := up.Initialize(initCtx, initReq); err != nil {
		_ = up.Close()
		return nil, fmt.Errorf("initializing mcp server %q: %w", serverName, err)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Client{
		serverName:  serverName,
		upstream:    up,
		resolvedEnv: resolvedEnv,
		timeout:     timeout,
	}, nil
}

// isolatedCommand is the stdio-transport command factory that gives a spawned
// MCP server exactly the curated env passed to it — no os.Environ() inheritance.
// It replaces the transport's default (append(os.Environ(), env...)), which
// would leak wraptool's own environment into the child.
func isolatedCommand(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = env
	return cmd, nil
}

// ListTools returns the upstream tool list. mcp-go follows NextCursor
// internally, so a single call returns every tool.
func (c *Client) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	res, err := c.upstream.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool invokes an upstream tool by its (un-prefixed) name, forwarding the
// arguments object verbatim, bounded by the server's per-call timeout.
func (c *Client) CallTool(ctx context.Context, toolName string, args any) (*mcp.CallToolResult, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args
	return c.upstream.CallTool(cctx, req)
}

// Close shuts down the upstream child process. No supervision is attempted
// (Phase 2); this is a plain teardown.
func (c *Client) Close() error {
	if c.upstream == nil {
		return nil
	}
	return c.upstream.Close()
}
