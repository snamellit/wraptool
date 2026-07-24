package cmd

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"

	"github.com/pti/wraptool/internal/buildinfo"
	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/webui"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/server"

	"github.com/spf13/cobra"
)

// newServeCmd builds the server-start command. It is registered twice: as
// `server start` (canonical) and as a hidden root-level `serve` (compat —
// generated .mcp.json files and documented systemd units invoke `serve`, and
// those live on disk long after an upgrade, so the spelling can't be dropped).
func newServeCmd(use string, hidden bool) *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:    use,
		Hidden: hidden,
		Short:  "Start the MCP server",
		Long: `Start the wraptool MCP server.

Supported transports:
  sse   - HTTP Server-Sent Events on a TCP address (default, e.g. 127.0.0.1:8717)
  unix  - HTTP Server-Sent Events on a Unix domain socket (e.g. /run/wraptool.sock)

wraptool runs as an independent process. Claude Code connects to it via the
mcpServers config:

  {"mcpServers": {"wraptool": {"url": "http://127.0.0.1:8717/sse"}}}

This keeps credentials isolated in the wraptool process.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(configPath)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: "+paths.DefaultConfigFile()+")")

	return cmd
}

func runServe(configPath string) error {
	if configPath == "" {
		configPath = os.Getenv("WRAPTOOL_CONFIG")
	}
	if configPath == "" {
		configPath = paths.DefaultConfigFile()
	}

	// First run: write a deny-by-default starter config if none exists, so serve
	// comes up instead of erroring on a missing file. Never clobbers.
	if created, err := config.EnsureConfigFile(configPath); err != nil {
		return fmt.Errorf("ensuring config: %w", err)
	} else if created {
		log.Printf("wrote default config to %s", configPath)
	}

	// Strict load: reject unknown/misspelled keys rather than silently ignoring
	// them. A typo'd restrictive key (deny, deny_flags, flag_constraints) would
	// otherwise be dropped and weaken the policy with no error — see the F1
	// finding in design/security-audit-policy-enforcement.md.
	cfg, err := config.LoadStrict(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating config: %w", err)
	}

	policy := whitelist.NewPolicy(configPointers(cfg.Tools))
	mcpPolicy := whitelist.NewMCPPolicy(configPointers(cfg.MCPServers))
	store := requestlog.NewStore(cfg.MCP.RequestLog)

	// Create MCP server. serverInfo.version carries the stamped build identity so
	// clients can see which wraptool binary is serving.
	s := server.NewMCPServer(
		"wraptool",
		buildinfo.Version(),
		server.WithToolCapabilities(true),
	)

	// Register tools from every source (CLI tools + one proxy per MCP server).
	// The registrar owns the live source set and its registrations, spawning the
	// MCP-server children lazily as it enumerates each source's tools. Wire up
	// teardown BEFORE start so any early return — a mid-registration failure, a
	// later startup error, or the normal transport exit (e.g. stdin EOF under
	// stdio) — closes every child that was started. reload reconciles the set in
	// place; closeAll tears down whatever is current, under the registrar lock.
	reg := newRegistrar(s, policy, mcpPolicy, store)
	defer reg.closeAll()

	if err := reg.start(cfg); err != nil {
		return fmt.Errorf("registering tools: %w", err)
	}

	// Register the capability request tool + the server-info tool.
	mcptools.RegisterRequestCapabilityTool(s, store)
	mcptools.RegisterInfoTool(s, reg.CLIToolNames)
	mcptools.RegisterDiscoverTool(s, reg.config)

	// Set up SIGHUP and file-watch for hot-reload.
	go handleSIGHUP(configPath, reg)
	go watchConfigFile(configPath, reg)

	// Start the operator-facing Web UI on its own listener, if configured.
	// It runs in this process but is independent of the MCP transport, so
	// it works even under stdio.
	if cfg.Admin.Listen != "" {
		if err := startWebUI(cfg, store, configPath); err != nil {
			return fmt.Errorf("starting web ui: %w", err)
		}
	}

	// Record our pid for the persistent transports so `server stop`/`status`
	// find the server however it was started — `up`, a direct `wraptool serve`,
	// or a service manager. This is the single writer of the pidfile. stdio is a
	// per-session, harness-spawned transport (many run at once), so it must NOT
	// touch the shared pidfile. A default-signal exit skips defers, so also clear
	// the pidfile (and tear down MCP children) on SIGINT/SIGTERM — e.g. from
	// `wraptool server stop`.
	if cfg.MCP.Transport != "stdio" {
		if err := writePidFile(); err != nil {
			return err
		}
		defer removePidFile()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, terminationSignals()...)
		go func() {
			<-sigCh
			reg.closeAll()
			removePidFile()
			os.Exit(0)
		}()
	}

	// Start the appropriate transport.
	switch cfg.MCP.Transport {
	case "stdio":
		return serveStdio(s)
	case "sse":
		return serveSSE(s, cfg)
	case "unix":
		return serveUnix(s, cfg)
	default:
		return fmt.Errorf("unknown transport: %q (expected stdio, sse, or unix)", cfg.MCP.Transport)
	}
}

// toolNames returns the configured CLI tool names, for the wraptool_info tool's
// per-tool introspection-freshness report.
func toolNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	return names
}

// configPointers adapts a config's value map into the pointer map the policy
// engines expect. Each &v is a distinct per-iteration variable (Go 1.22+ loop
// semantics), so the pointers stay independent.
func configPointers[T any](m map[string]T) map[string]*T {
	out := make(map[string]*T, len(m))
	for name, v := range m {
		out[name] = &v
	}
	return out
}

// newSSEServer builds the SSE server shared by the sse and unix transports,
// wiring the client-CWD context func and appending query params to the message
// endpoint. Callers may pass extra transport-specific options.
func newSSEServer(s *server.MCPServer, allowedCWDRoots []string, extra ...server.SSEOption) *server.SSEServer {
	if len(allowedCWDRoots) == 0 {
		log.Printf("warning: mcp.allowed_cwd_roots is unset — a client-supplied cwd is not " +
			"contained and may point at any host directory the server can access. See F5 in " +
			"design/security-audit-policy-enforcement.md.")
	}
	opts := []server.SSEOption{
		server.WithSSEContextFunc(server.SSEContextFunc(cwdContextFunc(allowedCWDRoots))),
		server.WithAppendQueryToMessageEndpoint(),
	}
	opts = append(opts, extra...)
	return server.NewSSEServer(s, opts...)
}

// cwdContextFunc injects the operator's allowed-CWD roots and any client-supplied
// cwd query param into the request context, shared by the SSE and Streamable HTTP
// transports so both enforce F5 containment identically.
func cwdContextFunc(allowedCWDRoots []string) func(context.Context, *http.Request) context.Context {
	return func(ctx context.Context, r *http.Request) context.Context {
		ctx = paths.WithAllowedCWDRoots(ctx, allowedCWDRoots)
		if cwd := r.URL.Query().Get("cwd"); cwd != "" {
			ctx = paths.WithClientCWD(ctx, cwd)
		}
		return ctx
	}
}

// newStreamableServer builds the Streamable HTTP transport (single POST/GET
// endpoint at /mcp), used by clients such as Antigravity that negotiate the
// 2025-03 transport instead of legacy SSE. It shares the tool registry and the
// same CWD context func as the SSE server.
func newStreamableServer(s *server.MCPServer, allowedCWDRoots []string) *server.StreamableHTTPServer {
	return server.NewStreamableHTTPServer(s,
		server.WithEndpointPath("/mcp"),
		server.WithHTTPContextFunc(cwdContextFunc(allowedCWDRoots)),
	)
}

// newMCPHTTPHandler mounts both transports on one mux — Streamable HTTP at /mcp,
// legacy SSE at /sse (+ its /message endpoint) — behind a single Bearer-auth
// wrapper, so SSE clients (claude, cursor) and Streamable clients (Antigravity)
// hit the same listener with the same tool registry and auth.
func newMCPHTTPHandler(s *server.MCPServer, cfg *config.Config, sseExtra ...server.SSEOption) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("/mcp", newStreamableServer(s, cfg.MCP.AllowedCWDRoots))
	mux.Handle("/", newSSEServer(s, cfg.MCP.AllowedCWDRoots, sseExtra...))
	return authWrapped(mux, cfg.MCP.AuthTokenFile)
}

// authWrapped wraps handler with Bearer-token auth when tokenFile is set,
// returning the handler unchanged otherwise.
func authWrapped(handler http.Handler, tokenFile string) (http.Handler, error) {
	if tokenFile == "" {
		return handler, nil
	}
	return newAuthMiddleware(tokenFile, handler)
}

func serveStdio(s *server.MCPServer) error {
	log.Println("wraptool MCP server starting on stdio")
	stdio := server.NewStdioServer(s)
	return stdio.Listen(context.Background(), os.Stdin, os.Stdout)
}

func serveSSE(s *server.MCPServer, cfg *config.Config) error {
	if cfg.MCP.Listen == "" {
		return fmt.Errorf("mcp.listen is required for sse transport (e.g. 127.0.0.1:8717)")
	}

	handler, err := newMCPHTTPHandler(s, cfg)
	if err != nil {
		return err
	}

	log.Printf("wraptool MCP server starting on %s (SSE at /sse, Streamable HTTP at /mcp)", cfg.MCP.Listen)

	srv := &http.Server{
		Addr:    cfg.MCP.Listen,
		Handler: handler,
	}

	return srv.ListenAndServe()
}

func serveUnix(s *server.MCPServer, cfg *config.Config) error {
	if cfg.MCP.Listen == "" {
		return fmt.Errorf("mcp.listen is required for unix transport (e.g. /run/wraptool.sock)")
	}

	sockPath := cfg.MCP.Listen

	// Remove stale socket file if it exists.
	if _, err := os.Stat(sockPath); err == nil {
		if err := os.Remove(sockPath); err != nil {
			return fmt.Errorf("removing stale socket %s: %w", sockPath, err)
		}
	}

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listening on unix socket %s: %w", sockPath, err)
	}
	defer listener.Close()

	// Restrict socket permissions to owner only.
	if err := os.Chmod(sockPath, 0600); err != nil {
		return fmt.Errorf("setting socket permissions: %w", err)
	}

	handler, err := newMCPHTTPHandler(s, cfg, server.WithBaseURL("http://localhost"))
	if err != nil {
		return err
	}

	log.Printf("wraptool MCP server starting on unix:%s", sockPath)

	srv := &http.Server{
		Handler:     handler,
		ConnContext: connContextWithPeerPID,
	}

	// Clean up socket on shutdown signals.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, terminationSignals()...)
		<-sigCh
		log.Println("shutting down...")
		srv.Close()
		os.Remove(sockPath)
	}()

	return srv.Serve(listener)
}

// startWebUI launches the operator-facing request-review Web UI on its own
// listener in a background goroutine. configPath must be the resolved path
// already loaded by the serve process so that applied approvals trigger the
// file-watch reload.
func startWebUI(cfg *config.Config, store *requestlog.Store, configPath string) error {
	srv, err := webui.New(store, configPath)
	if err != nil {
		return err
	}

	handler := srv.Handler()
	if cfg.Admin.AuthTokenFile == "" {
		return fmt.Errorf("admin.auth_token_file is required when admin.listen is set")
	}
	token, err := webui.LoadToken(cfg.Admin.AuthTokenFile)
	if err != nil {
		return err
	}
	handler = srv.AuthMiddleware(token, handler)

	httpSrv := &http.Server{Addr: cfg.Admin.Listen, Handler: handler}
	log.Printf("wraptool admin ui: %s", webURL(cfg.Admin.Listen, token))
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("web ui server stopped: %s", err)
		}
	}()
	return nil
}

// webURL builds a clickable terminal URL for the Web UI. A wildcard bind
// (0.0.0.0 / [::]) is rewritten to 127.0.0.1 so the link is followable from
// the operator's machine. When a token is set it is embedded as a query
// param so the first click establishes the session cookie.
func webURL(listen, token string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = listen, ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	hostport := host
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	}
	u := "http://" + hostport + "/requests"
	if token != "" {
		u += "?token=" + url.QueryEscape(token)
	}
	return u
}

// newAuthMiddleware creates an HTTP middleware that validates a Bearer token.
func newAuthMiddleware(tokenFile string, next http.Handler) (http.Handler, error) {
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading auth token file %s: %w", tokenFile, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, fmt.Errorf("auth token file %s is empty", tokenFile)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing or invalid Authorization header", http.StatusUnauthorized)
			return
		}
		provided := strings.TrimPrefix(auth, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "invalid token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}), nil
}
