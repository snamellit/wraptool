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
	"syscall"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcpproxy"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/webui"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/mark3labs/mcp-go/server"

	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server",
		Long: `Start the wraptool MCP server.

Supported transports:
  stdio  - communicate over stdin/stdout (default, for direct MCP client launch)
  sse    - HTTP Server-Sent Events on a TCP address (e.g. 127.0.0.1:8717)
  unix   - HTTP Server-Sent Events on a Unix domain socket (e.g. /run/wraptool.sock)

For sse and unix transports, wraptool runs as an independent process.
Claude Code connects to it via the mcpServers config:

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

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating config: %w", err)
	}

	policy := whitelist.NewPolicy(toolConfigPointers(cfg.Tools))
	mcpPolicy := whitelist.NewMCPPolicy(mcpServerConfigPointers(cfg.MCPServers))
	store := requestlog.NewStore(cfg.Server.RequestLog)

	// Create MCP server.
	s := server.NewMCPServer(
		"wraptool",
		serverVersion,
		server.WithToolCapabilities(true),
	)

	// Register tools from every source (CLI tools + one proxy per MCP server).
	sources := buildSources(cfg, policy, mcpPolicy, store)
	registered, err := mcptools.RegisterAll(s, sources)
	if err != nil {
		return fmt.Errorf("registering tools: %w", err)
	}

	// Register the capability request tool.
	mcptools.RegisterRequestCapabilityTool(s, store)

	// Set up SIGHUP and file-watch for hot-reload.
	state := &reloadState{registered: registered, sources: sources}
	go handleSIGHUP(s, configPath, policy, mcpPolicy, state, store)
	go watchConfigFile(s, configPath, policy, mcpPolicy, state, store)

	// Close the live sources when the transport returns (e.g. stdin EOF under
	// stdio), tearing down any spawned MCP-server child processes so they do
	// not leak past wraptool's own exit. reload may have swapped the set, so
	// close whatever is current in state, under its lock.
	defer state.closeSources()

	// Start the operator-facing Web UI on its own listener, if configured.
	// It runs in this process but is independent of the MCP transport, so
	// it works even under stdio.
	if cfg.Server.Web.Listen != "" {
		if err := startWebUI(cfg, store, configPath); err != nil {
			return fmt.Errorf("starting web ui: %w", err)
		}
	}

	// Start the appropriate transport.
	switch cfg.Server.Transport {
	case "stdio":
		return serveStdio(s)
	case "sse":
		return serveSSE(s, cfg)
	case "unix":
		return serveUnix(s, cfg)
	default:
		return fmt.Errorf("unknown transport: %q (expected stdio, sse, or unix)", cfg.Server.Transport)
	}
}

// serverVersion is reported to MCP clients and forwarded to upstream MCP
// servers as wraptool's ClientInfo version.
const serverVersion = "0.1.0"

// buildSources assembles the tool sources registered on the MCP server: the CLI
// tool source plus one mcpproxy source per configured MCP server. It is shared
// by initial startup and hot-reload so both build the identical set.
func buildSources(cfg *config.Config, policy *whitelist.Policy, mcpPolicy *whitelist.MCPPolicy, store *requestlog.Store) []mcptools.Source {
	sources := []mcptools.Source{mcptools.NewCLISource(cfg, policy, store)}
	for name := range cfg.MCPServers {
		srv := cfg.MCPServers[name] // fresh copy; handler holds &srv
		sources = append(sources, mcpproxy.NewSource(name, &srv, mcpPolicy, store, serverVersion))
	}
	return sources
}

// toolConfigPointers adapts the config's value map into the pointer map the
// policy engine expects. Each &tc is a distinct per-iteration variable
// (Go 1.22+ loop semantics), so the pointers stay independent.
func toolConfigPointers(tools map[string]config.ToolConfig) map[string]*config.ToolConfig {
	m := make(map[string]*config.ToolConfig, len(tools))
	for name, tc := range tools {
		m[name] = &tc
	}
	return m
}

// mcpServerConfigPointers adapts the config's value map into the pointer map
// the MCP policy engine expects, mirroring toolConfigPointers.
func mcpServerConfigPointers(servers map[string]config.MCPServerConfig) map[string]*config.MCPServerConfig {
	m := make(map[string]*config.MCPServerConfig, len(servers))
	for name := range servers {
		srv := servers[name]
		m[name] = &srv
	}
	return m
}

// newSSEServer builds the SSE server shared by the sse and unix transports,
// wiring the client-CWD context func and appending query params to the message
// endpoint. Callers may pass extra transport-specific options.
func newSSEServer(s *server.MCPServer, extra ...server.SSEOption) *server.SSEServer {
	opts := []server.SSEOption{
		server.WithSSEContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if cwd := r.URL.Query().Get("cwd"); cwd != "" {
				ctx = paths.WithClientCWD(ctx, cwd)
			}
			return ctx
		}),
		server.WithAppendQueryToMessageEndpoint(),
	}
	opts = append(opts, extra...)
	return server.NewSSEServer(s, opts...)
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
	if cfg.Server.Listen == "" {
		return fmt.Errorf("server.listen is required for sse transport (e.g. 127.0.0.1:8717)")
	}

	handler, err := authWrapped(newSSEServer(s), cfg.Server.AuthTokenFile)
	if err != nil {
		return err
	}

	log.Printf("wraptool MCP server starting on %s (SSE)", cfg.Server.Listen)

	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: handler,
	}

	return srv.ListenAndServe()
}

func serveUnix(s *server.MCPServer, cfg *config.Config) error {
	if cfg.Server.Listen == "" {
		return fmt.Errorf("server.listen is required for unix transport (e.g. /run/wraptool.sock)")
	}

	sockPath := cfg.Server.Listen

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

	sseServer := newSSEServer(s, server.WithBaseURL("http://localhost"))

	handler, err := authWrapped(sseServer, cfg.Server.AuthTokenFile)
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
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
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

	var handler http.Handler = srv.Handler()
	var token string
	if cfg.Server.Web.AuthTokenFile != "" {
		token, err = webui.LoadToken(cfg.Server.Web.AuthTokenFile)
		if err != nil {
			return err
		}
		handler = srv.AuthMiddleware(token, handler)
	} else {
		log.Printf("web ui: no auth_token_file set — relying on localhost bind for access control")
	}

	httpSrv := &http.Server{Addr: cfg.Server.Web.Listen, Handler: handler}
	log.Printf("wraptool web ui: %s", webURL(cfg.Server.Web.Listen, token))
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
