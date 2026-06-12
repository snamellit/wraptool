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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/mcptools"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/requestlog"
	"github.com/pti/wraptool/internal/webui"
	"github.com/pti/wraptool/internal/whitelist"

	"github.com/fsnotify/fsnotify"
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

	// Build tool config map for the policy engine.
	toolConfigs := make(map[string]*config.ToolConfig, len(cfg.Tools))
	for name, tc := range cfg.Tools {
		tc := tc
		toolConfigs[name] = &tc
	}

	policy := whitelist.NewPolicy(toolConfigs)
	store := requestlog.NewStore(cfg.Server.RequestLog)

	// Create MCP server.
	s := server.NewMCPServer(
		"wraptool",
		"0.1.0",
		server.WithToolCapabilities(true),
	)

	// Register tools from config.
	registered, err := mcptools.RegisterAll(s, cfg, policy)
	if err != nil {
		return fmt.Errorf("registering tools: %w", err)
	}

	// Register the capability request tool.
	mcptools.RegisterRequestCapabilityTool(s, store)

	// Set up SIGHUP and file-watch for hot-reload.
	state := &reloadState{registered: registered}
	go handleSIGHUP(s, configPath, policy, state)
	go watchConfigFile(s, configPath, policy, state)

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

func serveStdio(s *server.MCPServer) error {
	log.Println("wraptool MCP server starting on stdio")
	stdio := server.NewStdioServer(s)
	return stdio.Listen(context.Background(), os.Stdin, os.Stdout)
}

func serveSSE(s *server.MCPServer, cfg *config.Config) error {
	if cfg.Server.Listen == "" {
		return fmt.Errorf("server.listen is required for sse transport (e.g. 127.0.0.1:8717)")
	}

	sseServer := server.NewSSEServer(s,
		server.WithSSEContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if cwd := r.URL.Query().Get("cwd"); cwd != "" {
				ctx = paths.WithClientCWD(ctx, cwd)
			}
			return ctx
		}),
		server.WithAppendQueryToMessageEndpoint(),
	)

	// Wrap with auth middleware if configured.
	var handler http.Handler = sseServer
	if cfg.Server.AuthTokenFile != "" {
		authHandler, err := newAuthMiddleware(cfg.Server.AuthTokenFile, handler)
		if err != nil {
			return err
		}
		handler = authHandler
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

	sseServer := server.NewSSEServer(s,
		server.WithBaseURL("http://localhost"),
		server.WithSSEContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if cwd := r.URL.Query().Get("cwd"); cwd != "" {
				ctx = paths.WithClientCWD(ctx, cwd)
			}
			return ctx
		}),
		server.WithAppendQueryToMessageEndpoint(),
	)

	// Wrap with auth middleware if configured.
	var handler http.Handler = sseServer
	if cfg.Server.AuthTokenFile != "" {
		authHandler, err := newAuthMiddleware(cfg.Server.AuthTokenFile, handler)
		if err != nil {
			return err
		}
		handler = authHandler
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

// reloadState tracks MCP tool names registered from config so SIGHUP
// can replace them atomically.
type reloadState struct {
	mu         sync.Mutex
	registered []string
}

func handleSIGHUP(s *server.MCPServer, configPath string, policy *whitelist.Policy, state *reloadState) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	for range sighup {
		reloadConfig(s, configPath, policy, state, "SIGHUP")
	}
}

// watchConfigFile watches the config file's parent directory for changes
// and triggers a reload. Watching the directory (not the file) survives
// editor save patterns that replace the file via rename.
func watchConfigFile(s *server.MCPServer, configPath string, policy *whitelist.Policy, state *reloadState) {
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		log.Printf("config watch: resolving %s: %s", configPath, err)
		return
	}
	dir := filepath.Dir(absPath)
	name := filepath.Base(absPath)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("config watch: %s (file-based reload disabled, SIGHUP still works)", err)
		return
	}
	defer w.Close()

	if err := w.Add(dir); err != nil {
		log.Printf("config watch: adding %s: %s (file-based reload disabled)", dir, err)
		return
	}
	log.Printf("watching %s for changes", absPath)

	// Debounce: a single save can emit CREATE+WRITE+CHMOD; coalesce
	// rapid events into one reload.
	var debounce *time.Timer
	const debounceDelay = 200 * time.Millisecond

	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != name {
				continue
			}
			// Ignore pure CHMOD; reload on write/create/rename.
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(debounceDelay, func() {
				reloadConfig(s, configPath, policy, state, "file change")
			})
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("config watch error: %s", err)
		}
	}
}

func reloadConfig(s *server.MCPServer, configPath string, policy *whitelist.Policy, state *reloadState, trigger string) {
	log.Printf("reloading config (%s)...", trigger)
	newCfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("reload failed: %s", err)
		return
	}
	if err := newCfg.Validate(); err != nil {
		log.Printf("reload validation failed: %s", err)
		return
	}
	newToolConfigs := make(map[string]*config.ToolConfig, len(newCfg.Tools))
	for name, tc := range newCfg.Tools {
		tc := tc
		newToolConfigs[name] = &tc
	}
	policy.Reload(newToolConfigs)

	// Swap MCP tool registrations: remove the previously registered
	// set, then register fresh from the new config. mcp-go sends
	// notifications/tools/list_changed on both calls, so connected
	// clients refresh their tool lists without needing to reconnect.
	state.mu.Lock()
	if len(state.registered) > 0 {
		s.DeleteTools(state.registered...)
	}
	names, err := mcptools.RegisterAll(s, newCfg, policy)
	state.registered = names
	state.mu.Unlock()
	if err != nil {
		log.Printf("reload tool registration failed: %s", err)
		return
	}
	log.Println("config reloaded successfully")
}
