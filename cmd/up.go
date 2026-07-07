package cmd

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"

	"github.com/spf13/cobra"
)

type upOptions struct {
	configPath  string
	noServer    bool
	noContainer bool
	shell       string // "auto" | "always" | "never"
	workspace   string
	runtime     string // "auto" | "guix" | "devcontainer"
}

func newUpCmd() *cobra.Command {
	opts := upOptions{}

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Ensure the wraptool server is running and bring up this project's dev container wired to it",
		Long: `Bring up an isolated coding environment for the current project.

Run in a git repository or worktree root. up is idempotent: it ensures the
shared wraptool MCP server is running (starting it detached if needed), then
brings up the project's dev container with the connection info (token + this
worktree's path) injected, so the wraptool-connect feature can wire the MCP
client to the host server.

Multiple worktrees share one server; each worktree's container is scoped to its
own path via the server's per-connection cwd.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUp(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.configPath, "config", "c", "", "path to config file (default: "+paths.DefaultConfigFile()+")")
	cmd.Flags().BoolVar(&opts.noServer, "no-server", false, "do not start the server (assume it is already running)")
	cmd.Flags().BoolVar(&opts.noContainer, "no-container", false, "only ensure the server; do not touch the container")
	cmd.Flags().StringVar(&opts.shell, "shell", "auto", "open a shell in the container after bringup: auto (when attached to a terminal), always, never")
	cmd.Flags().StringVar(&opts.workspace, "workspace", "", "workspace root (default: the git worktree root of the current directory)")
	cmd.Flags().StringVar(&opts.runtime, "runtime", "auto", "container runtime: auto (guix when available, else devcontainer), guix, or devcontainer")

	return cmd
}

func runUp(opts upOptions) error {
	if inContainer() {
		return fmt.Errorf("refusing to run inside a container; run `wraptool up` on the host")
	}

	root := opts.workspace
	if root == "" {
		var err error
		root, err = gitToplevel()
		if err != nil {
			return fmt.Errorf("not a git repository (run in a repo/worktree root, or pass --workspace): %w", err)
		}
	}
	root, _ = filepath.Abs(root)

	cfg, err := config.Load(configOrDefault(opts.configPath))
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if !opts.noServer {
		if err := ensureServer(cfg, configOrDefault(opts.configPath)); err != nil {
			return err
		}
	}

	if opts.noContainer {
		return nil
	}

	token, err := readAuthToken(cfg)
	if err != nil {
		return err
	}

	rt, err := selectRuntime(opts.runtime, root)
	if err != nil {
		return err
	}
	shell := wantShell(opts.shell)
	switch rt {
	case runtimeGuix:
		return bringUpGuixContainer(root, token, cfg.Server.Listen, shell)
	case runtimeDevcontainer:
		return bringUpContainer(root, token, shell)
	}
	return fmt.Errorf("unhandled runtime %v", rt)
}

// wantShell resolves the --shell mode. "auto" opens a shell only when stdin and
// stdout are a terminal, so interactive runs drop the dev straight in while CI /
// piped runs just bring the container up.
func wantShell(mode string) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	default: // "auto"
		return isTTY(os.Stdin) && isTTY(os.Stdout)
	}
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func configOrDefault(p string) string {
	if p != "" {
		return p
	}
	return paths.DefaultConfigFile()
}

// --- server supervision ---

// ensureServer makes the shared MCP server reachable, starting it detached if
// it is not. Idempotent and safe against a concurrent `up` via a file lock.
func ensureServer(cfg *config.Config, configPath string) error {
	if cfg.Server.Transport != "sse" {
		return fmt.Errorf("`up` requires server.transport: sse (got %q) so the container can connect over the network", cfg.Server.Transport)
	}
	if cfg.Server.Listen == "" {
		return fmt.Errorf("server.listen is required (e.g. 0.0.0.0:8717)")
	}
	probe := probeAddr(cfg.Server.Listen)

	if dialOK(probe) {
		fmt.Printf("wraptool server already running (%s)\n", probe)
		return nil
	}

	unlock, err := lockRuntime()
	if err != nil {
		return err
	}
	defer unlock()

	// Re-check under the lock; a racing `up` may have started it.
	if dialOK(probe) {
		fmt.Printf("wraptool server already running (%s)\n", probe)
		return nil
	}

	if err := ensureAuthToken(cfg); err != nil {
		return err
	}
	if err := spawnServer(configPath); err != nil {
		return err
	}

	// Wait for it to come up.
	for range 30 {
		if dialOK(probe) {
			fmt.Printf("wraptool server started (%s)\n", probe)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("server did not become reachable at %s within 6s (see %s)", probe, serveLogPath())
}

// probeAddr turns a listen address into one that is dialable from the host:
// a wildcard bind (0.0.0.0 / ::) is probed on loopback.
func probeAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func dialOK(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func spawnServer(configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating wraptool binary: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(serveLogPath()), 0700); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	logf, err := os.OpenFile(serveLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening server log: %w", err)
	}
	defer logf.Close()

	cmd := exec.Command(exe, "serve", "--config", configPath)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting server: %w", err)
	}

	if err := os.MkdirAll(paths.RuntimeDir(), 0700); err != nil {
		return fmt.Errorf("creating runtime dir: %w", err)
	}
	if err := os.WriteFile(pidFilePath(), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		return fmt.Errorf("writing pidfile: %w", err)
	}
	// Detach: don't wait on or reap the child.
	return cmd.Process.Release()
}

func pidFilePath() string  { return filepath.Join(paths.RuntimeDir(), "serve.pid") }
func lockFilePath() string { return filepath.Join(paths.RuntimeDir(), "serve.lock") }
func serveLogPath() string { return filepath.Join(paths.DataDir(), "logs", "serve.log") }

// lockRuntime serializes server startup across concurrent `up` invocations.
func lockRuntime() (func(), error) {
	if err := os.MkdirAll(paths.RuntimeDir(), 0700); err != nil {
		return nil, fmt.Errorf("creating runtime dir: %w", err)
	}
	f, err := os.OpenFile(lockFilePath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// --- auth token ---

func ensureAuthToken(cfg *config.Config) error {
	path := expandTilde(cfg.Server.AuthTokenFile)
	if path == "" {
		return nil // no auth configured; server trusts localhost
	}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generating auth token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(buf)+"\n"), 0600); err != nil {
		return fmt.Errorf("writing auth token: %w", err)
	}
	fmt.Printf("generated auth token -> %s\n", path)
	return nil
}

func readAuthToken(cfg *config.Config) (string, error) {
	path := expandTilde(cfg.Server.AuthTokenFile)
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading auth token %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// --- container ---

func bringUpContainer(root, token string, shell bool) error {
	if !hasDevcontainer(root) {
		return fmt.Errorf("no dev container found under %s/.devcontainer\n\nScaffold one (see examples/devcontainer) and add the wraptool-connect feature, then re-run `wraptool up`", root)
	}
	if _, err := exec.LookPath("devcontainer"); err != nil {
		return fmt.Errorf("the devcontainer CLI is not installed (npm i -g @devcontainers/cli); needed to bring up %s", root)
	}

	env := append(os.Environ(),
		"WRAPTOOL_TOKEN="+token,
		"WRAPTOOL_CWD="+root, // host path → server scopes tools to this worktree via ?cwd=
	)

	fmt.Printf("bringing up dev container for %s ...\n", root)
	up := exec.Command("devcontainer", "up", "--workspace-folder", root)
	up.Env = env
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		return fmt.Errorf("devcontainer up: %w", err)
	}

	if !shell {
		fmt.Printf("\ndev container is up. Attach with:\n  devcontainer exec --workspace-folder %s bash\n", root)
		return nil
	}

	fmt.Printf("\nopening a shell in the dev container (exit leaves it running; re-enter with `wraptool up`)...\n")
	sh := exec.Command("devcontainer", "exec", "--workspace-folder", root, "bash", "-l")
	sh.Env = env
	sh.Stdin = os.Stdin
	sh.Stdout = os.Stdout
	sh.Stderr = os.Stderr
	return sh.Run()
}

func hasDevcontainer(root string) bool {
	for _, p := range []string{
		filepath.Join(root, ".devcontainer", "devcontainer.json"),
		filepath.Join(root, ".devcontainer.json"),
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// --- helpers ---

func gitToplevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if v := os.Getenv("REMOTE_CONTAINERS"); v != "" {
		return true
	}
	if v := os.Getenv("WRAPTOOL_CONTAINER"); v != "" { // guix-container marker (bringUpGuixContainer)
		return true
	}
	return false
}
