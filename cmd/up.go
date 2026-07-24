package cmd

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/harness"
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
	harness     string // which harness to wire in the container (guix runtime)
}

func defaultUpOptions() upOptions {
	return upOptions{
		shell:   "auto",
		runtime: "auto",
		harness: "claude",
	}
}

func newUpCmd() *cobra.Command {
	opts := defaultUpOptions()

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
	cmd.Flags().StringVar(&opts.shell, "shell", opts.shell, "open a shell in the container after bringup: auto (when attached to a terminal), always, never")
	cmd.Flags().StringVar(&opts.workspace, "workspace", "", "workspace root (default: the git worktree root of the current directory)")
	cmd.Flags().StringVar(&opts.runtime, "runtime", opts.runtime, "container runtime: auto (guix when available, else devcontainer), guix, or devcontainer")
	cmd.Flags().StringVar(&opts.harness, "harness", opts.harness, "coding harness to wire in the container (guix runtime): "+strings.Join(harness.Names(), ", "))

	return cmd
}

func runUp(opts upOptions) error {
	if isInContainer() {
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

	configPath := opts.configPath
	if configPath == "" {
		configPath = os.Getenv("WRAPTOOL_CONFIG")
	}
	configPath = configOrDefault(configPath)
	// First run: write a deny-by-default starter config if none exists, so the
	// load below (and the spawned server) find a valid file. Never clobbers.
	if created, err := config.EnsureConfigFile(configPath); err != nil {
		return fmt.Errorf("ensuring config: %w", err)
	} else if created {
		log.Printf("wrote default config to %s", configPath)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	var rt containerRuntime
	if !opts.noContainer {
		rt, err = selectRuntime(opts.runtime, root)
		if err != nil {
			return err
		}
		// Both container backends receive a writable workspace mount. Keep the
		// active policy and admin credential outside it before starting a
		// credential-holding server.
		if err := validateConfigOutsideWorkspace(configPath, root); err != nil {
			return err
		}
		if cfg.Admin.AuthTokenFile != "" {
			if err := validatePathOutsideWorkspace(cfg.Admin.AuthTokenFile, "admin auth token", root); err != nil {
				return err
			}
		}
	}

	if !opts.noServer {
		if err := adjustServerBindForRuntime(cfg, rt); err != nil {
			return err
		}
		if err := ensureServer(cfg, configPath); err != nil {
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

	fmt.Printf("container runtime: %s\n", rt)
	shell := wantShell(opts.shell)
	switch rt {
	case runtimeGuix:
		return bringUpGuixContainer(root, token, cfg.MCP.Listen, opts.harness, shell)
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

// adjustServerBindForRuntime widens the MCP bind for a bridged (devcontainer)
// runtime, whose container reaches the host over the bridge gateway — not the
// host's loopback — so a loopback bind is unreachable from inside it. A loopback
// listen is rebound to 0.0.0.0 on the same port, and because a non-loopback bind
// is reachable off-host it must be authenticated, so a token file is defaulted
// in when the operator did not configure one (ensureServer then generates it).
// The guix runtime shares the host netns, so loopback already works there and is
// left untouched (no LAN exposure) — this is a no-op for it.
func adjustServerBindForRuntime(cfg *config.Config, rt containerRuntime) error {
	if rt != runtimeDevcontainer || cfg.MCP.Transport != "sse" {
		return nil
	}
	if config.ListenIsLoopback(cfg.MCP.Listen) {
		_, port, err := net.SplitHostPort(cfg.MCP.Listen)
		if err != nil {
			return fmt.Errorf("parsing mcp.listen %q: %w", cfg.MCP.Listen, err)
		}
		cfg.MCP.Listen = net.JoinHostPort("0.0.0.0", port)
		fmt.Printf("devcontainer: binding %s so the container can reach the server via its bridge gateway\n", cfg.MCP.Listen)
	}
	if cfg.MCP.AuthTokenFile == "" {
		cfg.MCP.AuthTokenFile = defaultMCPTokenFile()
		fmt.Printf("devcontainer: enabling an MCP auth token at %s (a non-loopback bind must be authenticated)\n", cfg.MCP.AuthTokenFile)
	}
	return nil
}

func defaultMCPTokenFile() string {
	return filepath.Join(paths.ConfigDir(), "mcp-token")
}

// ensureServer makes the shared MCP server reachable, starting it detached if
// it is not. Idempotent and safe against a concurrent `up` via a file lock.
func ensureServer(cfg *config.Config, configPath string) error {
	if cfg.MCP.Transport != "sse" {
		return fmt.Errorf("`up` requires mcp.transport: sse (got %q) so the container can connect over the network", cfg.MCP.Transport)
	}
	if cfg.MCP.Listen == "" {
		return fmt.Errorf("mcp.listen is required (e.g. 127.0.0.1:8717)")
	}
	probe := probeAddr(cfg.MCP.Listen)

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
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating config after token setup: %w", err)
	}
	if err := spawnServer(configPath, cfg.MCP.Listen, cfg.MCP.AuthTokenFile); err != nil {
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

func spawnServer(configPath, listen, authTokenFile string) error {
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

	// up is authoritative for the effective bind/token (runtime-adjusted), so
	// pass them as overrides rather than relying on the config file the detached
	// serve re-reads.
	args := []string{"serve", "--config", configPath}
	if listen != "" {
		args = append(args, "--listen", listen)
	}
	if authTokenFile != "" {
		args = append(args, "--auth-token-file", authTokenFile)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting server: %w", err)
	}

	// The spawned `serve` writes the pidfile itself (its own pid), so every start
	// path records one — see writePidFile in serve. Detach: don't wait or reap.
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
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking: %w", err)
	}
	return func() {
		_ = unlockFile(f)
		f.Close()
	}, nil
}

// --- auth token ---

func ensureAuthToken(cfg *config.Config) error {
	if err := ensureTokenFile(cfg.MCP.AuthTokenFile, "MCP"); err != nil {
		return err
	}
	return ensureTokenFile(cfg.Admin.AuthTokenFile, "admin UI")
}

func ensureTokenFile(path, name string) error {
	path = expandTilde(path)
	if path == "" {
		return nil
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
	fmt.Printf("generated %s auth token -> %s\n", name, path)
	return nil
}

func readAuthToken(cfg *config.Config) (string, error) {
	path := expandTilde(cfg.MCP.AuthTokenFile)
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

// validateConfigOutsideWorkspace ensures an untrusted container cannot rewrite
// the server's policy through its writable workspace mount.
func validateConfigOutsideWorkspace(configPath, root string) error {
	return validatePathOutsideWorkspace(configPath, "config", root)
}

// validatePathOutsideWorkspace ensures a container cannot access a host-side
// credential or policy through its writable workspace mount.
func validatePathOutsideWorkspace(path, name, root string) error {
	path = expandTilde(path)
	resolvedPath, err := resolvePath(path)
	if err != nil {
		return fmt.Errorf("resolving %s path %s: %w", name, path, err)
	}
	resolvedRoot, err := resolvePath(root)
	if err != nil {
		return fmt.Errorf("resolving workspace path %s: %w", root, err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return fmt.Errorf("checking %s location: %w", name, err)
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing container: %s resolves to %s inside writable workspace %s", name, resolvedPath, resolvedRoot)
	}
	return nil
}

// resolvePath follows symlinks even when the final path does not exist yet, so
// token files generated by up cannot be placed in the writable workspace.
func resolvePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for redirects := 0; redirects < 255; redirects++ {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				return filepath.EvalSymlinks(path)
			}
			target, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			path = filepath.Clean(target)
			continue
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil
		}
		resolvedParent, err := resolvePath(parent)
		if err != nil {
			return "", err
		}
		return filepath.Join(resolvedParent, filepath.Base(path)), nil
	}
	return "", fmt.Errorf("too many symlink redirects")
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

	// Shared harness-state pool: bind-mount the host pool home/ onto the fixed
	// target the wraptool feature pins HOME to, so a harness's auth/skills/
	// config persist across every project (the developer toolbox, installed once).
	// Binary provisioning is the feature's job (per image); this is state only.
	// See design/harness-descriptor.md §7.
	_, _, _, poolHome, err := ensureHarnessPool()
	if err != nil {
		return err
	}

	fmt.Printf("bringing up dev container for %s ...\n", root)
	upArgs := []string{"up", "--workspace-folder", root}
	// Defer to a config that already declares the pool mount (e.g. one written by
	// `wraptool init devcontainer`, or a standalone Reopen-in-Container setup):
	// adding our own --mount on top would be a duplicate mount point and docker
	// rejects it. Only add the mount when the config does not carry it.
	if devcontainerDeclaresPoolMount(root) {
		fmt.Printf("harness pool: mount declared in devcontainer.json (%s)\n", devcontainerPoolHome)
	} else {
		fmt.Printf("harness pool: sharing %s -> %s (state persists across projects)\n", poolHome, devcontainerPoolHome)
		upArgs = append(upArgs, "--mount", "type=bind,source="+poolHome+",target="+devcontainerPoolHome)
	}
	up := exec.Command("devcontainer", upArgs...)
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

func devcontainerConfigPaths(root string) []string {
	return []string{
		filepath.Join(root, ".devcontainer", "devcontainer.json"),
		filepath.Join(root, ".devcontainer.json"),
	}
}

func hasDevcontainer(root string) bool {
	for _, p := range devcontainerConfigPaths(root) {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// devcontainerDeclaresPoolMount reports whether the project's devcontainer.json
// already bind-mounts the harness-state pool onto devcontainerPoolHome. It is a
// deliberate substring check, not a JSONC parse: the pool target path only ever
// appears in a config as the mount's target (the feature sets HOME to it via
// containerEnv, in the image — never in the user's config), so its presence is a
// reliable signal that `wraptool up` must not add a second, duplicate mount.
func devcontainerDeclaresPoolMount(root string) bool {
	for _, p := range devcontainerConfigPaths(root) {
		if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), devcontainerPoolHome) {
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

// isInContainer is a variable so runUp tests can exercise host-only startup
// validation without depending on the test runner's container environment.
var isInContainer = inContainer

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
