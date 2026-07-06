// Package mcpproxy connects to and polices local stdio MCP servers that
// wraptool spawns itself. Each upstream server is a child process launched with
// executor.ChildEnv (never inheriting wraptool's own environment), giving it
// the same credential-isolation profile as a wrapped CLI tool. Its tools are
// exposed through the shared mcptools.Source seam, with policy, scrubbing, and
// audit applied identically to the CLI path.
//
// Phase 2 adds long-lived robustness on top of the Phase 1 connect/list/call
// MVP: reactive crash supervision (a dead transport triggers a bounded
// respawn-and-retry) and propagation of upstream tools/list_changed
// notifications, both funnelled through an onToolsChanged callback so the
// registrar can re-register a single source's tools. There is still no
// background liveness goroutine — a child's death is inferred from call
// failures, which is all mcp-go exposes (the transport's exit signal is
// unexported).
package mcpproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/executor"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Crash-supervision tuning: a child may be respawned at most maxRestarts times
// within restartWindow before it is treated as crashlooping and left down
// (contributing no tools) until the window elapses and the next call retries.
const (
	restartWindow  = 30 * time.Second
	maxRestarts    = 5
	restartBackoff = 250 * time.Millisecond
	maxBackoff     = 2 * time.Second
	stderrTailMax  = 4 << 10 // bytes of child stderr retained for crash diagnostics
)

// upstreamConn is the subset of *client.Client that Client drives. Abstracting
// it lets the respawn machinery be exercised with a fake that can "die" on
// demand; production always holds a real *client.Client.
type upstreamConn interface {
	ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	Close() error
}

// Client is a supervised lifecycle wrapper around one spawned stdio MCP server.
// It holds the connected upstream, the server's resolved env (the scrub secret
// set), the per-call timeout, and enough config to respawn the child after a
// crash. It does not embed policy or audit — those live in the handler — so it
// stays a transport concern.
type Client struct {
	serverName  string
	cfg         *config.MCPServerConfig
	version     string
	resolvedEnv map[string]string
	timeout     time.Duration

	// connect spawns and hands back a ready upstream; it is a field so tests
	// can inject a fake. Production sets it to (*Client).spawn.
	connect func(context.Context) (upstreamConn, error)
	// now is the clock used for the crashloop window; injectable for tests.
	now func() time.Time
	// backoffUnit is multiplied by the consecutive-restart count for the
	// respawn backoff (capped at maxBackoff). Zero disables the pause; tests
	// leave it zero to avoid real sleeps.
	backoffUnit time.Duration

	stderr *ringBuffer // bounded tail of the child's stderr, for crash logging

	mu             sync.Mutex
	upstream       upstreamConn
	onToolsChanged func() // invoked when the upstream tool set may have changed
	restarts       int
	windowStart    time.Time
}

// NewClient spawns the configured stdio MCP server, performs the MCP handshake
// (declaring no client capabilities), and returns a ready, supervised Client.
// The child's environment is built with executor.ChildEnv from the server's
// resolved env, so it is the minimal curated env — wraptool's own environment
// is never inherited. The handshake is bounded by cfg.InitTimeout.
func NewClient(ctx context.Context, serverName string, cfg *config.MCPServerConfig, version string) (*Client, error) {
	resolvedEnv, err := cfg.ResolvedEnv()
	if err != nil {
		return nil, fmt.Errorf("resolving env for mcp server %q: %w", serverName, err)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	c := &Client{
		serverName:  serverName,
		cfg:         cfg,
		version:     version,
		resolvedEnv: resolvedEnv,
		timeout:     timeout,
		now:         time.Now,
		backoffUnit: restartBackoff,
		stderr:      &ringBuffer{max: stderrTailMax},
	}
	c.connect = c.spawn

	up, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	c.upstream = up
	return c, nil
}

// spawn launches the child, wires the list_changed notification hook, starts
// the transport, drains stderr, and completes the MCP handshake. It is the
// production connector and is reused verbatim on every respawn.
func (c *Client) spawn(ctx context.Context) (upstreamConn, error) {
	// Full replacement environment — same builder as CLI tools, so isolation is
	// identical. NOTE: the default mcp-go stdio transport sets
	// cmd.Env = append(os.Environ(), env...), which would leak wraptool's own
	// environment into the child. We pass a custom command factory
	// (isolatedCommand) so the child gets *exactly* this curated env and nothing
	// inherited — otherwise the credential-isolation guarantee is void.
	env := executor.ChildEnv(c.cfg.Command, c.resolvedEnv)
	up, err := client.NewStdioMCPClientWithOptions(c.cfg.Command, env, c.cfg.Args,
		transport.WithCommandFunc(isolatedCommand))
	if err != nil {
		return nil, fmt.Errorf("spawning mcp server %q: %w", c.serverName, err)
	}

	// Register the tools/list_changed hook before Start so an early notification
	// is not missed. mcp-go delivers notifications on the transport read loop, a
	// separate goroutine, so invoking the callback here never re-enters us.
	up.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			c.notifyToolsChanged()
		}
	})

	if err := up.Start(ctx); err != nil {
		_ = up.Close()
		return nil, fmt.Errorf("starting mcp server %q: %w", c.serverName, err)
	}

	// Drain the child's stderr into a bounded ring buffer. This both keeps a
	// tail for crash diagnostics and prevents a chatty child from blocking on a
	// full stderr pipe that nobody reads. The goroutine ends when the pipe
	// closes (child exit / Close).
	if r, ok := client.GetStderr(up); ok {
		go func() { _, _ = io.Copy(c.stderr, r) }()
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ClientInfo = mcp.Implementation{Name: "wraptool", Version: c.version}
	// ProtocolVersion left empty → mcp-go defaults to the latest.
	// Capabilities left zero → declare none (no sampling/roots/elicitation).

	initTimeout := c.cfg.InitTimeout
	if initTimeout == 0 {
		initTimeout = 10 * time.Second
	}
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if _, err := up.Initialize(initCtx, initReq); err != nil {
		_ = up.Close()
		return nil, fmt.Errorf("initializing mcp server %q: %w", c.serverName, err)
	}

	return up, nil
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

// ListTools returns the upstream tool list, respawning the child once if the
// call reveals a dead transport. It never fires onToolsChanged: ListTools is
// only called from a (re-)registration path that already installs whatever list
// it gets back, so re-triggering registration here would recurse.
func (c *Client) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	res, err := withRespawn(c, ctx, false, func(up upstreamConn) (*mcp.ListToolsResult, error) {
		return up.ListTools(ctx, mcp.ListToolsRequest{})
	})
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool invokes an upstream tool by its (un-prefixed) name, forwarding the
// arguments object verbatim, bounded by the server's per-call timeout. A dead
// transport triggers one respawn-and-retry; a successful respawn fires
// onToolsChanged, since a fresh child may advertise a different tool set.
func (c *Client) CallTool(ctx context.Context, toolName string, args any) (*mcp.CallToolResult, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args
	return withRespawn(c, cctx, true, func(up upstreamConn) (*mcp.CallToolResult, error) {
		return up.CallTool(cctx, req)
	})
}

// withRespawn runs op against the current upstream and, if the error indicates
// a dead child, respawns once and retries. When fireOnChange is set and the
// retry ran on a fresh child, onToolsChanged is invoked (outside any lock) so
// the registrar can re-list — the new child's tools may differ.
func withRespawn[T any](c *Client, ctx context.Context, fireOnChange bool, op func(upstreamConn) (T, error)) (T, error) {
	up := c.currentUpstream()
	if up == nil {
		// A prior respawn failed outright and left no live child; try once more.
		if !c.restart(ctx, nil) {
			var zero T
			return zero, fmt.Errorf("mcp server %q is unavailable", c.serverName)
		}
		up = c.currentUpstream()
	}
	res, err := op(up)
	if err == nil || !isDead(err) {
		return res, err
	}
	if !c.restart(ctx, up) {
		return res, err // still down (crashlooping) → surface the dead-transport error
	}
	res, err = op(c.currentUpstream())
	if err == nil && fireOnChange {
		c.notifyToolsChanged()
	}
	return res, err
}

// isDead reports whether err means the upstream child has gone away: either the
// transport's own closed sentinel, or the broken-pipe write failure that
// precedes it. Any call against a dead child produces one of these.
func isDead(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, transport.ErrTransportClosed) ||
		strings.Contains(err.Error(), "failed to write request")
}

// currentUpstream returns the live upstream under the lock.
func (c *Client) currentUpstream() upstreamConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.upstream
}

// restart tears down the dead child (failed) and connects a fresh one, subject
// to a crashloop cap. It returns true if, on return, c.upstream is a usable
// upstream to retry against — including the case where another goroutine
// already respawned the same failed child. It returns false when the child is
// crashlooping or the respawn itself fails, leaving c.upstream cleared.
func (c *Client) restart(ctx context.Context, failed upstreamConn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A concurrent caller may have already replaced the child we tripped on.
	if c.upstream != failed {
		return c.upstream != nil
	}

	nowFn := c.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	if c.windowStart.IsZero() || now.Sub(c.windowStart) > restartWindow {
		c.windowStart = now
		c.restarts = 0
	}
	if c.restarts >= maxRestarts {
		// Crashlooping: leave the dead child in place so calls keep failing
		// cleanly (rather than nil-dereferencing), and re-attempt only once the
		// window has elapsed. The source contributes no working tools meanwhile.
		log.Printf("mcpproxy: mcp server %q is crashlooping (%d restarts within %s); leaving it down.%s",
			c.serverName, c.restarts, restartWindow, c.stderrTail())
		return false
	}
	c.restarts++

	log.Printf("mcpproxy: mcp server %q transport died; respawning (attempt %d).%s",
		c.serverName, c.restarts, c.stderrTail())

	if c.upstream != nil {
		_ = c.upstream.Close()
		c.upstream = nil
	}

	// Bounded backoff proportional to consecutive restarts, so a fast crashloop
	// does not hammer exec().
	if c.backoffUnit > 0 {
		time.Sleep(min(c.backoffUnit*time.Duration(c.restarts), maxBackoff))
	}

	up, err := c.connect(ctx)
	if err != nil {
		log.Printf("mcpproxy: respawn of mcp server %q failed: %v", c.serverName, err)
		return false
	}
	c.upstream = up
	return true
}

// notifyToolsChanged invokes the onToolsChanged callback (if set) outside the
// lock, so the callee — the registrar's reregister — may freely call back into
// this Client (e.g. ListTools) without deadlocking.
func (c *Client) notifyToolsChanged() {
	c.mu.Lock()
	fn := c.onToolsChanged
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// setOnToolsChanged installs the tools-changed callback.
func (c *Client) setOnToolsChanged(fn func()) {
	c.mu.Lock()
	c.onToolsChanged = fn
	c.mu.Unlock()
}

// stderrTail returns the child's captured stderr tail, formatted for a log
// line, or an empty string if there is none.
func (c *Client) stderrTail() string {
	if c.stderr == nil {
		return ""
	}
	tail := strings.TrimSpace(c.stderr.String())
	if tail == "" {
		return ""
	}
	return " stderr tail:\n" + tail
}

// Close shuts down the upstream child process, if one is running.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.upstream == nil {
		return nil
	}
	err := c.upstream.Close()
	c.upstream = nil
	return err
}

// ringBuffer is a byte sink that retains only the last max bytes written. It is
// used to keep a bounded tail of a child's stderr for crash diagnostics.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
