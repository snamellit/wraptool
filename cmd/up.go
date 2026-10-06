package cmd

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/buildinfo"
	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/pool"
	"github.com/pti/wraptool/internal/webui"

	"github.com/spf13/cobra"
)

type upOptions struct {
	configPath  string
	noServer    bool
	noContainer bool
	noAdmin     bool
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
worktree's path) injected, so the wraptool feature can wire the MCP
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
	cmd.Flags().BoolVar(&opts.noAdmin, "no-admin", false, "do not auto-enable the operator review UI")
	cmd.Flags().StringVar(&opts.shell, "shell", opts.shell, "open a shell in the container after bringup: auto (when attached to a terminal), always, never")
	cmd.Flags().StringVar(&opts.workspace, "workspace", "", "workspace root (default: the git worktree root of the current directory)")
	cmd.Flags().StringVar(&opts.runtime, "runtime", opts.runtime, "container runtime: auto (guix when available, else devcontainer), guix, or devcontainer")
	cmd.Flags().StringVar(&opts.harness, "harness", opts.harness, "coding harness to wire in the container: "+strings.Join(harness.Names(), ", "))

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
	printStaleBinaryNotice(root)

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
		// Validate --harness once, here, before the server even starts —
		// both bringUpGuixContainer and bringUpContainer need a known
		// harness for their own pool-plan resolution, but failing this late
		// (inside either of them) means `up` has already spawned the shared
		// server for nothing. Both functions keep their own defensive
		// registry lookup (a missing key there would be a bug, not a normal
		// failure) rather than trusting this check silently.
		if _, ok := harness.Registry[opts.harness]; !ok {
			return fmt.Errorf("unknown harness %q (want one of: %s)", opts.harness, strings.Join(harness.Names(), ", "))
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

	// Provision even with --no-server: the already-running server requires
	// the token (serve provisions its own on start), so the harness config
	// written below must carry it — otherwise every MCP call 401s with no
	// diagnostic at up time.
	if err := provisionDefaultMCPToken(cfg); err != nil {
		return err
	}

	if !opts.noServer {
		if err := adjustServerBindForRuntime(cfg, rt); err != nil {
			return err
		}
		if !opts.noAdmin {
			enableAdminDefault(cfg)
		}
		if err := ensureServer(cfg, configPath); err != nil {
			return err
		}
		if cfg.Admin.Listen != "" && cfg.Admin.AuthTokenFile != "" {
			if adminToken, err := webui.LoadToken(cfg.Admin.AuthTokenFile); err == nil {
				fmt.Printf("review UI: %s\n", webURL(cfg.Admin.Listen, adminToken))
			}
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
		return bringUpContainer(root, token, opts.harness, shell)
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
// listen is rebound to 0.0.0.0 on the same port. The guix runtime shares the
// host netns, so loopback already works there and the bind is left untouched
// (no LAN exposure).
//
// Either way an sse listener gets a token file when the operator did not
// configure one (created here via provisionDefaultMCPToken; readAuthToken
// hands it to the harness config both runtimes write). A loopback bind is not
// a boundary against the browser — see browserGuard — so this is not
// devcontainer-only.
func adjustServerBindForRuntime(cfg *config.Config, rt containerRuntime) error {
	if cfg.MCP.Transport != "sse" {
		return nil
	}
	if rt == runtimeDevcontainer && config.ListenIsLoopback(cfg.MCP.Listen) {
		_, port, err := net.SplitHostPort(cfg.MCP.Listen)
		if err != nil {
			return fmt.Errorf("parsing mcp.listen %q: %w", cfg.MCP.Listen, err)
		}
		cfg.MCP.Listen = net.JoinHostPort("0.0.0.0", port)
		fmt.Printf("devcontainer: binding %s so the container can reach the server via its bridge gateway\n", cfg.MCP.Listen)
	}
	return provisionDefaultMCPToken(cfg)
}

// provisionDefaultMCPToken gives an sse listener a token file when the operator
// configured none, and creates it. An sse listener is TCP-reachable, and that
// includes every web page the developer visits: mcp-go answers the SSE stream
// with Access-Control-Allow-Origin: *, so a hostile page can read the session
// endpoint and POST tool calls as a CORS simple request. A loopback bind is not
// a boundary against the browser.
//
// Provisioning rather than failing validation keeps a first run working. Every
// path that needs the effective token calls this — `serve` (which serves it),
// `up` (which hands it to the container), and `init` (which writes it into a
// harness config, possibly before any server has ever run, hence the file must
// be created here and not merely named).
func provisionDefaultMCPToken(cfg *config.Config) error {
	if cfg.MCP.Transport != "sse" || cfg.MCP.AuthTokenFile != "" {
		return nil
	}
	cfg.MCP.AuthTokenFile = defaultMCPTokenFile()
	if err := ensureTokenFile(cfg.MCP.AuthTokenFile, "MCP"); err != nil {
		return err
	}
	fmt.Printf("mcp.auth_token_file is unset — using %s (clients must send it as `Authorization: Bearer ...`)\n", cfg.MCP.AuthTokenFile)
	return nil
}

func defaultMCPTokenFile() string {
	return filepath.Join(paths.ConfigDir(), "mcp-token")
}

// enableAdminDefault turns on the operator review UI when the operator has not
// configured it, so `wraptool up` yields a complete setup: the MCP server for
// the AI plus the UI to approve its capability requests. Unlike the MCP
// listener, admin is operator/host-facing and is NEVER widened past loopback —
// exposing it would let anything that can route to the port approve
// capabilities and edit policy. A token is defaulted in (ensureServer generates
// it) so the UI is never unauthenticated. Opt out with --no-admin.
func enableAdminDefault(cfg *config.Config) {
	if cfg.Admin.Listen == "" {
		cfg.Admin.Listen = "127.0.0.1:8718"
	}
	if cfg.Admin.AuthTokenFile == "" {
		cfg.Admin.AuthTokenFile = defaultAdminTokenFile()
	}
}

func defaultAdminTokenFile() string {
	return filepath.Join(paths.ConfigDir(), "admin-token")
}

// ensureServer makes the shared MCP server reachable, starting it detached if
// it is not. Idempotent and safe against a concurrent `up` via a file lock.
//
// It also reconciles a stale server whose bind/auth no longer matches the
// desired (runtime-adjusted) config: a server left over from a previous
// session (e.g. before a `rm -rf` of the config dir, or an upgrade) may be on
// a loopback+no-auth bind when `up` now needs a bridged 0.0.0.0+auth bind for
// a devcontainer. Reusing it silently would leave the container unable to
// reach the server (loopback isn't routable over the bridge) or unauthenticated.
// `serve` records its effective listen+auth in a state file next to the
// pidfile; `up` compares and restarts the stale server when they differ.
//
// Tokens are generated on every path (including reuse) so readAuthToken never
// fails against a server that was started before a token file was defaulted in.
func ensureServer(cfg *config.Config, configPath string) error {
	if cfg.MCP.Transport != "sse" {
		return fmt.Errorf("`up` requires mcp.transport: sse (got %q) so the container can connect over the network", cfg.MCP.Transport)
	}
	if cfg.MCP.Listen == "" {
		return fmt.Errorf("mcp.listen is required (e.g. 127.0.0.1:8717)")
	}
	probe := probeAddr(cfg.MCP.Listen)
	desired := serveState{Listen: cfg.MCP.Listen, Auth: cfg.MCP.AuthTokenFile != ""}

	// Always acquire the lock before touching tokens or the server: a pre-lock
	// "already running" fast path would race with a concurrent `up` writing the
	// same token file. The flock is cheap; correctness beats the micro-opt.
	unlock, err := lockRuntime()
	if err != nil {
		return err
	}
	defer unlock()

	// A matching server is up. Ensure tokens exist under the lock (a server
	// started before a token file was defaulted in, or whose token file was
	// later deleted, would otherwise leave readAuthToken with no file to read).
	if dialOK(probe) && runningMatches(desired) {
		if err := ensureAuthToken(cfg); err != nil {
			return err
		}
		fmt.Printf("wraptool server already running (%s)\n", probe)
		return nil
	}

	// Something is answering but it doesn't match the desired bind/auth (or we
	// can't tell — e.g. a server started by an older wraptool with no state
	// file). Stop it so we can bind the desired address with the right auth.
	if dialOK(probe) {
		fmt.Printf("wraptool server running with a stale bind/auth; restarting to apply %s\n", cfg.MCP.Listen)
		if err := stopRunningServer(); err != nil {
			return fmt.Errorf("stopping stale server: %w", err)
		}
		for range 30 {
			if !dialOK(probe) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if dialOK(probe) {
			return fmt.Errorf("stale server did not release %s", probe)
		}
	}

	if err := ensureAuthToken(cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating config after token setup: %w", err)
	}
	if err := spawnServer(configPath, serveOverrides{
		mcpListen: cfg.MCP.Listen, mcpAuthTokenFile: cfg.MCP.AuthTokenFile,
		adminListen: cfg.Admin.Listen, adminAuthTokenFile: cfg.Admin.AuthTokenFile,
	}); err != nil {
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

// runningMatches reports whether the recorded state of the running server
// matches the desired bind+auth. A missing state file (a server started by an
// older wraptool, or a hand-edited session) is treated as a mismatch so `up`
// restarts it under the current config — a one-time upgrade cost, and the
// only safe choice when the effective bind is unknown.
func runningMatches(desired serveState) bool {
	got, err := readServeState()
	if err != nil {
		return false
	}
	return got.Listen == desired.Listen && got.Auth == desired.Auth
}

// stopRunningServer stops the shared server via its pidfile. It is the body of
// `server stop`, shared with `up`'s stale-server reconciliation.
func stopRunningServer() error {
	pid, err := readPidFile()
	if err != nil || pid <= 0 {
		// No pidfile: the listener was started out-of-band (or by an older
		// wraptool). We can't stop it by pid, but a rebind to a different
		// address would just double-bind; bail loudly rather than guess.
		return fmt.Errorf("a server is answering but there is no pidfile at %s to stop it — run `wraptool server stop` or kill it manually", pidFilePath())
	}
	_, err = stopByPid(pid)
	return err
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

func spawnServer(configPath string, ov serveOverrides) error {
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

	// up is authoritative for the effective binds/tokens (runtime-adjusted MCP
	// address, auto-enabled admin UI), so pass them as overrides rather than
	// relying on the config file the detached serve re-reads.
	args := append([]string{"serve", "--config", configPath}, ov.flags()...)
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

// ensureTokenFile makes path hold a token, generating one if it does not.
// All provisioners (serve, up, init) serialize on a sidecar flock, so the
// read-then-write below is race-free among them: the first writer's token
// stands, a later caller reads it back, and a file left empty by an
// interrupted run is repaired under the same lock — no window where two
// processes hand out different tokens (the earlier O_EXCL scheme still had
// one around the empty-file repair). Once the file holds a value, that value
// never changes.
func ensureTokenFile(path, name string) error {
	path = expandTilde(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating token dir: %w", err)
	}
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("opening token lock: %w", err)
	}
	defer lf.Close()
	if err := lockFile(lf); err != nil {
		return fmt.Errorf("locking token file: %w", err)
	}
	defer func() { _ = unlockFile(lf) }()

	if b, rerr := os.ReadFile(path); rerr == nil && len(strings.TrimSpace(string(b))) > 0 {
		return nil // an existing token stands, whoever wrote it
	} else if rerr != nil && !os.IsNotExist(rerr) {
		return fmt.Errorf("reading auth token %s: %w", path, rerr)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generating auth token: %w", err)
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(buf)+"\n"), 0600); err != nil {
		return fmt.Errorf("writing auth token %s: %w", path, err)
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

// --- devcontainer harness-pool runtime mount (design/harness/unified-harness-pool.md §12.2) ---
//
// §12.2's Phase-4 implementation ruling (design doc, amended): the
// devcontainer CLI's `--mount` flag has no read-only field (verified against
// the shipped CLI's argument validator) and a read-write runtime mount would
// breach §5's trust boundary, so `up` never injects the runtime mount itself
// — it is config-DECLARED (a verbatim `,readonly` mounts string, owned by the
// §12.3 scaffolder and the wiring-only feature's docs) and `up` only
// VERIFIES: live plan + declared mount → info; live plan + no declared mount
// → an actionable notice with the exact snippet to add. Likewise PATH cannot
// ride a CLI-supplied `--remote-env` (the CLI substitutes `${containerEnv:…}`
// only in config-sourced values, and a CLI remote-env would OVERRIDE the
// feature's own containerEnv PATH instead of appending to it): the
// wiring-only feature's containerEnv is the permanent mechanism, and
// wraptool's OWN interactive shell entry-wraps PATH for legacy-feature
// containers that don't have it yet (mirrors buildContainerEntry's
// PATH-append style, cmd/guixcontainer.go).

// devcontainerPoolPlan is the harness-pool liveness decision for one
// `wraptool up` (devcontainer runtime) invocation — the devcontainer analogue
// of cmd/guixcontainer.go's guixPoolPlan, computing the SAME "is there a live
// locked generation" answer via the shared poolPlanInput, but doing nothing
// mount/argv-related with it (see the ruling above): callers use
// mountRuntime/runtimeDir only to decide what to VERIFY and PRINT.
type devcontainerPoolPlan struct {
	mountRuntime bool   // true: the harness has a live locked generation for this platform
	runtimeDir   string // pool.RuntimeDir() when mountRuntime — the concrete source for the verify-notice's mounts snippet
	notice       string // non-empty: no-live-plan deprecation notice for stderr, gated on pool-lockability
}

// devcontainerPoolPlanInput is poolPlanInput (cmd/guixcontainer.go) with no
// devcontainer-specific fields of its own today — the devcontainer runtime
// needs nothing beyond harness identity/lockability and the active-pointer/
// runtime-tree paths every plan needs. Still its own named type (embedding,
// not a bare alias) so it reads symmetrically with guixPoolPlanInput and can
// grow devcontainer-only fields later without disturbing guix's.
type devcontainerPoolPlanInput struct {
	poolPlanInput
}

// resolveDevcontainerPoolPlan decides devcontainerPoolPlan for in.Harness.
// Pure aside from the pool.ReadPointer file read and the two existence checks
// below, so tests drive it against a t.TempDir() pool layout with no
// devcontainer CLI. Mirrors resolveGuixPoolPlan's joint-liveness check
// (dispatcher file present AND the generation's own bin present) — see that
// function's comments for the "no active pointer" case enumeration (missing
// file, unreadable path, empty/whitespace content, unresolved platform key).
//
// TODO(§16 Phase 4 item 5 / §12.2): once the wiring-only Feature major ships
// and per-image provisioning is retired, a missing/stale pointer here must
// hard-fail `up` with the exact §12.2 text:
//
//	claude is locked but not realized for linux-arm64-glibc;
//	run: wraptool harness sync --platform linux-arm64-glibc
//
// This wave is mixed-state (§16 migration intro) and never hard-fails; it
// only falls back to the still-supported per-image provisioned binary.
//
// TODO(§5.1): the target platform key's arch comes from pool.HostPlatformKey()
// (this HOST's GOARCH), used as a practical stand-in for "the Docker daemon's
// default platform" — correct for every local Docker Desktop/Engine setup
// this wave supports, but not for a remote DOCKER_HOST whose daemon arch
// differs from the client's. A future wave should probe the daemon directly
// (`docker info`/`docker version -f '{{.Server.Arch}}'`) instead of assuming
// they match.
func resolveDevcontainerPoolPlan(in devcontainerPoolPlanInput) devcontainerPoolPlan {
	if in.PointerPath != "" {
		if raw, err := pool.ReadPointer(in.PointerPath); err == nil {
			// A missing file, an unreadable path, or an empty/whitespace-only
			// generation id all count as "no active pointer" — TrimSpace so
			// "\n\n" or " " cannot activate the runtime plan (mirrors
			// resolveGuixPoolPlan).
			if genID := strings.TrimSpace(raw); genID != "" {
				dispatcher := filepath.Join(in.RuntimeBinDir, in.Harness)
				genBin := filepath.Join(in.RuntimeGenerationsDir, genID, "bin", in.Harness)
				if fileExists(dispatcher) && fileExists(genBin) {
					return devcontainerPoolPlan{
						mountRuntime: true,
						runtimeDir:   in.RuntimeDir,
					}
				}
				// The pointer resolved but its generation is gone from disk
				// (e.g. a concurrent gc) — fall back, but with a distinct
				// notice naming what actually went wrong.
				return legacyDevcontainerPoolPlan(in, fmt.Sprintf(
					"notice: active generation %s for %s is missing on disk; using the per-image provisioned "+
						"binary — run `wraptool harness sync` to rebuild", genID, in.Harness))
			}
		}
	}
	return legacyDevcontainerPoolPlan(in, fmt.Sprintf(
		"notice: %s has no locked harness-pool runtime; run `wraptool harness install %s` (or `wraptool harness "+
			"sync` if already installed) to run it from the version-locked pool instead of the per-image install",
		in.Harness, in.Harness))
}

// legacyDevcontainerPoolPlan builds the no-live-plan result, gating notice on
// in.PoolLockable exactly as guixcontainer.go's legacyGuixPoolPlan does.
func legacyDevcontainerPoolPlan(in devcontainerPoolPlanInput, notice string) devcontainerPoolPlan {
	plan := devcontainerPoolPlan{}
	if in.PoolOnly {
		// No per-image install exists to fall back to for a pool-only
		// harness, so the generic notice's "per-image" wording would lie.
		plan.notice = fmt.Sprintf("notice: %s has no active harness-pool generation and no other installer; "+
			"run `wraptool harness install %s` (or `wraptool harness sync %s` if already installed)",
			in.Harness, in.Harness, in.Harness)
	} else if in.PoolLockable {
		plan.notice = notice
	}
	return plan
}

// buildDevcontainerUpArgs assembles the `devcontainer up` argv: workspace
// folder, plus the pool home/ bind (skipped when the config already declares
// a mount at devcontainerPoolHome — duplicate bind targets are a Docker
// error). The runtime mount is NEVER injected here (§12.2's ruling above) —
// it is config-declared, and `up` only verifies it separately. Pure string
// building, no exec, so tests can pin it exactly, including byte-identity
// with the pre-pool-verification argv (this package's "pin current behavior
// before refactoring" requirement) — this shape has not actually changed
// from what shipped before the runtime mount was ever considered.
func buildDevcontainerUpArgs(root, poolHome string, homeDeclared bool) []string {
	args := []string{"up", "--workspace-folder", root}
	if !homeDeclared {
		args = append(args, "--mount", "type=bind,source="+filepath.ToSlash(poolHome)+",target="+devcontainerPoolHome)
	}
	return args
}

// devcontainerMountReadOnlyState classifies what a devcontainer.json
// mounts-array entry at some container target ACTUALLY achieves, for the
// specific §5-critical question "is this genuinely read-only". This is a
// typed-const enum (the PlatformKey convention, design/policy/flag-constraint-type-single-source.md):
// a switch over it must name every member (exhaustive lint).
//
// The devcontainer CLI accepts mounts-array entries in two shapes:
//   - a Docker-style comma-separated STRING ("source=...,target=...,type=bind,readonly"),
//     parsed by Docker's own CSV mount syntax, where a bare `readonly`/`ro`
//     token IS honored;
//   - an OBJECT ({"source":...,"target":...,"type":...}), whose schema has
//     NO read-only field at all — a "readonly": true (or any other spelling)
//     key is silently dropped by the CLI, and the mount comes up read-write
//     regardless of what the JSON says.
//
// A read-write mount of the locked runtime/ tree into an untrusted project
// container breaches §5's trust boundary: an agent in that ONE project could
// rewrite the dispatcher scripts every OTHER project's container executes.
type devcontainerMountReadOnlyState int

const (
	// devcontainerMountAbsent: no mounts-array entry targets the container
	// path at all.
	devcontainerMountAbsent devcontainerMountReadOnlyState = iota
	// devcontainerMountReadOnly: a STRING-form, type=bind entry carrying a
	// bare `readonly` or `ro` token — the only shape the CLI actually
	// enforces as read-only.
	devcontainerMountReadOnly
	// devcontainerMountReadWrite: declared, but NOT provably read-only —
	// object-form (readonly is silently dropped by the CLI regardless of
	// what the JSON says), a string-form entry missing the readonly/ro
	// token, or any non-bind mount type (volume, tmpfs) at the target.
	devcontainerMountReadWrite
)

// devcontainerClassifyMount finds the mounts-array entry (object or string
// form) targeting containerTarget, across the project's devcontainer config
// files, and classifies it per devcontainerMountReadOnlyState — see that
// type's own doc comment for why object-form and non-bind entries can never
// be trusted as read-only regardless of their content. Mounts-ARRAY-aware
// (reuses stripJSONC + the same parse devcontainerDeclaredMountSource uses),
// not devcontainerDeclaresMount's cheap whole-file substring check — a PATH
// mention, a comment, or any other unrelated string containing
// containerTarget must not be mistaken for a declared mount at all.
// devcontainerDeclaresMount (whole-file substring) stays the HOME target's
// own declared-check unchanged: switching that one too carries real
// regression risk (it only gates whether `up` injects an rw --mount that is
// safe to inject unconditionally) for no correctness gain.
func devcontainerClassifyMount(root, containerTarget string) devcontainerMountReadOnlyState {
	for _, p := range devcontainerConfigPaths(root) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			Mounts []any `json:"mounts"`
		}
		if err := json.Unmarshal([]byte(stripJSONC(string(data))), &cfg); err != nil {
			continue
		}
		for _, m := range cfg.Mounts {
			switch v := m.(type) {
			case string:
				fields, flags := devcontainerParseMountString(v)
				if fields["target"] != containerTarget {
					continue
				}
				if fields["type"] == "bind" && (flags["readonly"] || flags["ro"]) {
					return devcontainerMountReadOnly
				}
				return devcontainerMountReadWrite
			case map[string]any:
				if t, _ := v["target"].(string); t == containerTarget {
					// Object-form: the CLI's schema has no read-only field at
					// all, so this is ALWAYS read-write no matter what extra
					// keys the JSON carries (a "readonly": true here would
					// look reassuring and do nothing).
					return devcontainerMountReadWrite
				}
			}
		}
	}
	return devcontainerMountAbsent
}

// devcontainerRuntimeMountSnippet is the exact `mounts` string entry §12.3's
// scaffolder/feature docs own declaring — read-only, source = THIS host's
// real, ToSlash'd pool runtime path (printed concretely, never
// ${localEnv}-templated: `up` already knows the right answer for the host
// it's running on). A source containing ',', '"', or '\' cannot be embedded
// verbatim in this illustrative, human-pasted (not machine-written) snippet
// without corrupting it — a comma would misparse as the mount string's own
// CSV field separator, and a quote or backslash needs JSON escaping this
// snippet doesn't do — so refuse to print a broken one and name the exact
// escaping problem instead (via strconv.Quote, which shows the developer the
// correctly escaped form). Mirrors cmd/init_devcontainer.go's
// poolRuntimeMountSourceUnsafeChars, which guards the same string form when
// it's written straight into a machine-generated devcontainer.json instead of
// printed for a human to paste. This should be vanishingly rare: it needs a
// pool root path containing one of these characters on a POSIX host — a
// Windows path's backslashes are already ToSlash'd away above.
func devcontainerRuntimeMountSnippet(runtimeDir string) string {
	src := filepath.ToSlash(runtimeDir)
	if strings.ContainsAny(src, `,"\`) {
		return fmt.Sprintf("<cannot render a mounts snippet: pool runtime path %s needs manual escaping (contains "+
			"a comma, quote, or backslash) — add it to \"mounts\" by hand>", strconv.Quote(src))
	}
	return "source=" + src + ",target=" + devcontainerPoolRuntime + ",type=bind,readonly"
}

// devcontainerRuntimeMissingMountNotice is the actionable stderr notice when
// the plan is live but devcontainer.json does not yet declare the runtime
// mount at all (§12.2 verify).
func devcontainerRuntimeMissingMountNotice(hname, runtimeDir string) string {
	return fmt.Sprintf(
		"notice: %s is pool-locked but devcontainer.json does not declare the runtime mount; add this entry to \"mounts\":\n"+
			"  \"%s\"\n"+
			"(wraptool init devcontainer scaffolds this automatically for new projects)",
		hname, devcontainerRuntimeMountSnippet(runtimeDir))
}

// devcontainerRuntimeNotReadOnlyNotice is the UNMISSABLE §5 security notice
// when devcontainer.json declares a mount at the runtime target that the
// devcontainer CLI will NOT actually honor as read-only — an object-form
// entry (readonly silently dropped), a string-form entry missing the
// readonly/ro token, or a non-bind mount type. A read-write mount of the
// locked runtime/ tree into this untrusted project container would let its
// agent rewrite the dispatcher scripts every OTHER project's container
// executes (§5). Printed to stderr, never silent, but never a hard failure
// either (mixed-state, §16) — it prints the exact read-only string-form
// snippet to replace the unsafe declaration with.
func devcontainerRuntimeNotReadOnlyNotice(hname, runtimeDir string) string {
	return fmt.Sprintf(
		"SECURITY: devcontainer.json declares a mount at %s that the devcontainer CLI will NOT enforce as "+
			"read-only (object-form mounts and non-bind types cannot express read-only through that CLI) — "+
			"the locked harness pool would be writable from inside the %s project container. Replace it with "+
			"this read-only string-form entry in \"mounts\":\n"+
			"  \"%s\"\n",
		devcontainerPoolRuntime, hname, devcontainerRuntimeMountSnippet(runtimeDir))
}

// devcontainerRuntimeDeclaredInfo is the stdout info line when the plan is
// live AND devcontainer.json already declares a genuinely read-only runtime
// mount — nothing to fix, PATH comes from the wiring-only feature's
// containerEnv.
func devcontainerRuntimeDeclaredInfo(hname string) string {
	return fmt.Sprintf("harness pool: runtime mount declared read-only in devcontainer.json (%s) — %s is pool-locked", devcontainerPoolRuntime, hname)
}

// devcontainerStaleRuntimeMountNotice is the stderr notice for the one case
// devcontainerClassifyMount cannot see: devcontainer.json declares the
// runtime mount correctly, but an EXISTING container for this workspace was
// created BEFORE that declaration. A container's mounts (and containerEnv)
// are fixed when it is created, and `devcontainer up` reuses whatever
// container already carries this workspace's `devcontainer.local_folder`
// label rather than recreating it — so the declaration has no effect at all
// until the container is recreated, and the harness silently keeps running
// from the per-image install. Verifying the config alone reports a false OK
// here, which is exactly how a container built against the deprecated `:0`
// provisioning major survives a config migration to the wiring-only `:1`.
func devcontainerStaleRuntimeMountNotice(hname string) string {
	return fmt.Sprintf(
		"notice: an existing dev container for this workspace has no mount at %s — a container's mounts are fixed "+
			"when it is created, so `devcontainer up` reuses it and the declaration in devcontainer.json does not "+
			"take effect (%s keeps running from the per-image install). Recreate it:\n"+
			"  wraptool down --rm && wraptool up\n",
		devcontainerPoolRuntime, hname)
}

// devcontainerRuntimeMountStale decides whether the destinations of the
// existing container(s) for this workspace warrant the notice above. No
// existing container is NOT stale (the next `up` creates a fresh one that
// picks the declaration up), which is also the answer when docker is absent
// or unreadable — see devcontainerExistingMountDestinations.
func devcontainerRuntimeMountStale(dests []string) bool {
	return len(dests) > 0 && !slices.Contains(dests, devcontainerPoolRuntime)
}

// devcontainerExistingMountDestinations lists the container-side mount
// destinations of every existing (running OR stopped — a stopped one is
// restarted by `up` with its original mounts) dev container for root.
// Diagnostic only: any docker failure yields no destinations and no error,
// so a missing or unhealthy docker never turns `up` itself into a failure.
func devcontainerExistingMountDestinations(root string) []string {
	ids, err := devcontainerIDs(root)
	if err != nil || len(ids) == 0 {
		return nil
	}
	out, err := exec.Command("docker", append([]string{"inspect", "-f", "{{range .Mounts}}{{.Destination}}\n{{end}}"}, ids...)...).Output()
	if err != nil {
		return nil
	}
	var dests []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dests = append(dests, line)
		}
	}
	return dests
}

// devcontainerPathExportHint is the standalone `export PATH=...` command a
// developer can paste into a legacy-feature-provisioned container's shell to
// reach the pool dispatcher — the same append buildDevcontainerShellEntry
// wraps wraptool's own interactive shell with.
func devcontainerPathExportHint() string {
	return "export PATH=$PATH:" + shellQuote(devcontainerPoolRuntimeBin)
}

// buildDevcontainerShellEntry is the shell snippet buildDevcontainerExecArgs
// runs when the pool plan is live: append (never prepend, §12.1) the
// dispatcher bin dir to PATH, then hand off to a real login shell. Mirrors
// buildContainerEntry's PATH-append style (cmd/guixcontainer.go) — a bare
// $PATH (not double-quoted) followed by a shellQuote'd directory — but
// touches nothing else (HOME/TERM/cd): those are the image's own concern
// (HOME via the feature's containerEnv invariant; workdir is `devcontainer
// exec`'s own default).
//
// Accepted duplication: this entry-wrap runs unconditionally whenever the
// pool plan is live, so on a wiring-only (:1) feature container — whose
// containerEnv already puts the dispatcher dir on PATH — the same directory
// ends up on PATH twice. Harmless (a duplicate PATH entry resolves the same
// binary either way) and deliberate: this wave doesn't special-case the
// feature major here, so the single code path also covers the legacy
// (pre-cutover) feature-provisioned container that has no containerEnv PATH
// entry of its own yet (see buildDevcontainerExecArgs's own comment).
//
// Accepted residual: the trailing `exec bash -l` is ITSELF a second login
// shell, which re-sources /etc/profile (and /etc/profile.d/*) on top of the
// PATH this snippet just exported. That is harmless on any profile written
// as `PATH="$PATH:..."` (append-preserving), but a bare `PATH=...`
// assignment (a stock Debian base image's /etc/profile without
// `bash-completion`/`common-utils` pulling in a more careful one) discards
// whatever this entry appended and starts the interactive shell without the
// dispatcher on PATH after all — the exact same class of limitation the
// wiring-only feature's containerEnv PATH mechanism has (a later shell
// init can still clobber an appended value); not fixed in this wave.
func buildDevcontainerShellEntry() string {
	return devcontainerPathExportHint() + "; exec bash -l"
}

// buildDevcontainerExecArgs assembles wraptool's own interactive
// `devcontainer exec` argv. When the pool plan is live, entry-wraps the shell
// so the dispatcher directory is on PATH for THIS session even on a legacy
// (pre-cutover) feature-provisioned container that has no containerEnv PATH
// of its own yet — the wiring-only feature's containerEnv is the permanent
// mechanism for every OTHER exec session (a manual terminal, an IDE), which
// this wave does not touch.
func buildDevcontainerExecArgs(root string, live bool) []string {
	args := []string{"exec", "--workspace-folder", root}
	if live {
		return append(args, "bash", "-lc", buildDevcontainerShellEntry())
	}
	return append(args, "bash", "-l")
}

// --- container ---

func bringUpContainer(root, token, hname string, shell bool) error {
	if !hasDevcontainer(root) {
		return fmt.Errorf(`no dev container found under %s/.devcontainer

Scaffold one, then re-run `+"`wraptool up`"+`:

  wraptool init devcontainer --lang go        # writes .devcontainer/devcontainer.json
                                              # (--harness claude|opencode|pi|agy, --base, --lang; see --help)

Or, with Guix installed, add a manifest.scm to the project root and wraptool
uses the Guix container runtime instead — no dev container needed`, root)
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
	// Binary provisioning is the feature's job (per image) absent an active
	// pointer below; this call is state only. See design/harness/harness-descriptor.md §7.
	_, _, _, poolHome, err := ensureHarnessPool()
	if err != nil {
		return err
	}

	// Locked runtime/ liveness (§12.2): mirrors cmd/guixcontainer.go's
	// resolveGuixPoolPlan, gated on the SAME per-harness active pointer, but
	// for the devcontainer TARGET platform (see resolveDevcontainerPoolPlan's
	// own §5.1 TODO on pool.HostPlatformKey() as the arch stand-in). Whether
	// the runtime is actually mounted is NOT decided here — see the §12.2
	// ruling above the type declarations: `up` only verifies below.
	var pointerPath string
	if key, err := pool.HostPlatformKey(); err == nil {
		pointerPath = pool.PointerFile(key, hname)
	}
	// hname is validated against harness.Registry by runUp (next to
	// selectRuntime) before either container runtime is invoked — this
	// lookup cannot miss today, but a missing key would silently yield a
	// zero Descriptor (non-lockable, quiet no-plan) rather than an error, so
	// make the dependency on that validation explicit (mirrors
	// bringUpGuixContainer's own comment).
	desc, knownHarness := harness.Registry[hname]
	if !knownHarness {
		return fmt.Errorf("unknown harness %q resolving the pool plan (validated by runUp — this is a bug)", hname)
	}
	plan := resolveDevcontainerPoolPlan(devcontainerPoolPlanInput{poolPlanInput: poolPlanInput{
		Harness:               hname,
		PoolLockable:          desc.PoolLockable(),
		PoolOnly:              poolOnly(desc),
		PointerPath:           pointerPath,
		RuntimeDir:            pool.RuntimeDir(),
		RuntimeBinDir:         pool.RuntimeBinDir(),
		RuntimeGenerationsDir: pool.RuntimeGenerationsDir(),
	}})
	if plan.notice != "" {
		fmt.Fprintln(os.Stderr, plan.notice)
	}

	fmt.Printf("bringing up dev container for %s ...\n", root)

	// Home mount: `up`-injected exactly as before (rw is expressible via
	// --mount, §12.2's ruling). Defer to a config that already declares it
	// (e.g. one written by `wraptool init devcontainer`, or a standalone
	// Reopen-in-Container setup): adding our own --mount on top would be a
	// duplicate mount point and docker rejects it. Warn when a declared
	// source does not actually exist on this host — the mixed-OS case, §12.3.
	// This stays the cheap whole-file substring check (devcontainerDeclaresMount)
	// — see devcontainerClassifyMount's own comment for why the runtime
	// target below needs the stricter array-aware, read-only-classifying
	// check instead.
	homeDeclared := devcontainerDeclaresMount(root, devcontainerPoolHome)
	if homeDeclared {
		fmt.Printf("harness pool: home mount declared in devcontainer.json (%s)\n", devcontainerPoolHome)
		if warn := devcontainerDeclaredMountSourceMissing(root, devcontainerPoolHome); warn != "" {
			fmt.Fprintln(os.Stderr, warn)
		}
	} else {
		fmt.Printf("harness pool: sharing %s -> %s (state persists across projects)\n", poolHome, devcontainerPoolHome)
	}

	// Runtime mount: VERIFY only (§12.2's ruling — `up` never injects it).
	// Exhaustive over devcontainerMountReadOnlyState (exhaustive lint).
	if plan.mountRuntime {
		switch devcontainerClassifyMount(root, devcontainerPoolRuntime) {
		case devcontainerMountReadOnly:
			fmt.Println(devcontainerRuntimeDeclaredInfo(hname))
			if warn := devcontainerDeclaredMountSourceMissing(root, devcontainerPoolRuntime); warn != "" {
				fmt.Fprintln(os.Stderr, warn)
			}
			// Declared correctly, but a container created before the
			// declaration is reused as-is — verify the live container too.
			if devcontainerRuntimeMountStale(devcontainerExistingMountDestinations(root)) {
				fmt.Fprintln(os.Stderr, devcontainerStaleRuntimeMountNotice(hname))
			}
		case devcontainerMountReadWrite:
			// Loud and unmissable: this is a §5 trust-boundary violation,
			// not a cosmetic notice, but this wave still proceeds (mixed
			// state, §16) rather than hard-failing `up`.
			fmt.Fprintln(os.Stderr, devcontainerRuntimeNotReadOnlyNotice(hname, plan.runtimeDir))
		case devcontainerMountAbsent:
			fmt.Fprintln(os.Stderr, devcontainerRuntimeMissingMountNotice(hname, plan.runtimeDir))
		}
	}
	// No live plan: plan.notice above already covers it (lockable-gated).
	// Proceed either way — the mixed state (§16) means a Feature-provisioned
	// container still works via legacy per-image installation.

	upArgs := buildDevcontainerUpArgs(root, poolHome, homeDeclared)
	up := exec.Command("devcontainer", upArgs...)
	up.Env = env
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		return fmt.Errorf("devcontainer up: %w", err)
	}

	if !shell {
		fmt.Printf("\ndev container is up. Attach with:\n  devcontainer exec --workspace-folder %s bash\n", root)
		if plan.mountRuntime {
			fmt.Printf("(%s's pool dispatcher needs %s on PATH — the wiring-only wraptool feature's containerEnv "+
				"provides this automatically; on a legacy feature-provisioned container, run inside the shell: %s)\n",
				hname, devcontainerPoolRuntimeBin, devcontainerPathExportHint())
		}
		return nil
	}

	fmt.Printf("\nopening a shell in the dev container (exit leaves it running; re-enter with `wraptool up`)...\n")
	sh := exec.Command("devcontainer", buildDevcontainerExecArgs(root, plan.mountRuntime)...)
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

// devcontainerDeclaresMount reports whether the project's devcontainer.json
// already bind-mounts something onto containerTarget. Deliberate substring
// check, not a JSONC parse — kept ONLY for the home target's own
// declared-check (deciding whether `up` injects the rw home --mount): the
// target path only ever appears in a user's config as a mount's target (a
// feature's own containerEnv/HOME pin never touches the user's file), so a
// false-positive substring match there just makes `up` skip a mount it would
// have written identically anyway. The runtime target's own declared-check
// needs the stricter mounts-array-aware devcontainerClassifyMount instead —
// an unrelated PATH mention or comment containing devcontainerPoolRuntime
// must not silently suppress the missing-mount notice, which THIS cheap
// check cannot tell apart.
func devcontainerDeclaresMount(root, containerTarget string) bool {
	for _, p := range devcontainerConfigPaths(root) {
		if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), containerTarget) {
			return true
		}
	}
	return false
}

// devcontainerDeclaresPoolMount reports whether the project's devcontainer.json
// already bind-mounts the harness-state pool onto devcontainerPoolHome.
func devcontainerDeclaresPoolMount(root string) bool {
	return devcontainerDeclaresMount(root, devcontainerPoolHome)
}

// devcontainerTrailingCommaRE removes a JSON-illegal trailing comma before a
// closing } or ] — JSONC/JSON5 permit them (and this repo's own hand-edited
// .devcontainer/devcontainer.json could grow one), encoding/json does not.
//
// Unlike stripJSONC's own comment removal, this regex is STRING-UNAWARE: it
// runs on stripJSONC's output, which still contains every original JSON
// string verbatim, and it will happily match ",]" or ",}" occurring INSIDE
// a string value too (e.g. a mount source path that happened to end in
// exactly that sequence). Accepted residual risk, not a fixed bug: this
// package only ever calls it before json.Unmarshal, whose failure on a
// corrupted string is caught by every caller (devcontainerDeclaredMountSource,
// devcontainerClassifyMount) and treated as found=false / devcontainerMountAbsent
// — the conservative, safe-by-construction failure mode (§12.2's "print the
// missing-mount notice" path), never a false "this is a properly declared
// read-only mount". A fully string-aware trailing-comma removal would need
// the same character-by-character scan as stripJSONC itself; not worth it
// for a residual that can only ever fail closed.
var devcontainerTrailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)

// stripJSONC removes `//` line comments and `/* */` block comments from raw
// JSONC text, tracking JSON string boundaries so a comment marker INSIDE a
// string is left alone — a naive whole-line regex (this function's earlier
// implementation) corrupted any string containing "//", e.g. a WRAPTOOL_URL
// of "http://172.18.0.1:8717" (examples/devcontainer/README.md's own idiom)
// or this repo's own documentationURL-shaped values. Also removes a single
// trailing comma before a closing } or ] (devcontainerTrailingCommaRE) —
// JSONC/JSON5 permit them, encoding/json does not.
func stripJSONC(data string) string {
	var b strings.Builder
	b.Grow(len(data))
	inString := false
	i, n := 0, len(data)
	for i < n {
		c := data[i]
		if inString {
			b.WriteByte(c)
			if c == '\\' && i+1 < n {
				// Copy the escaped character verbatim without interpreting
				// it, so an escaped quote (\") does not end the string.
				b.WriteByte(data[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		switch {
		case c == '"':
			inString = true
			b.WriteByte(c)
			i++
		case c == '/' && i+1 < n && data[i+1] == '/':
			for i < n && data[i] != '\n' {
				i++
			}
			// Leave the newline (if any) at data[i] for the next loop
			// iteration's default branch to copy — keeps line structure and
			// downstream error positions intact.
		case c == '/' && i+1 < n && data[i+1] == '*':
			i += 2
			for i+1 < n && (data[i] != '*' || data[i+1] != '/') {
				i++
			}
			if i+1 < n {
				i += 2 // skip past the closing "*/"
			} else {
				i = n // unterminated block comment: consume to EOF
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return devcontainerTrailingCommaRE.ReplaceAllString(b.String(), "$1")
}

// devcontainerLocalEnvRE matches a Dev Container ${localEnv:NAME} variable
// substitution — the only substitution form the shipped pool-mount sources
// use (poolHomeMountSource in cmd/init_devcontainer.go, docs/devcontainers.qmd).
var devcontainerLocalEnvRE = regexp.MustCompile(`\$\{localEnv:([A-Za-z_][A-Za-z0-9_]*)\}`)

// devcontainerDeclaredMountSource returns the "source" of the first mounts
// entry — object form {"source":...,"target":...} or Docker's
// comma-separated string form "source=...,target=...,type=bind" — targeting
// containerTarget, across the project's devcontainer config files.
// Best-effort: an unparsable config or no matching mount yields found=false.
// Feeds the mixed-OS missing-source warning (devcontainerDeclaredMountSourceMissing).
// The runtime target's own declared/read-only classification is
// devcontainerClassifyMount (which parses the mounts array itself, sharing
// the same stripJSONC + json.Unmarshal step, rather than calling this
// source-only helper) — the home target keeps devcontainerDeclaresMount's
// cheaper whole-file substring check for ITS declared-check instead (see its
// own doc comment for why the two targets differ).
func devcontainerDeclaredMountSource(root, containerTarget string) (source string, found bool) {
	for _, p := range devcontainerConfigPaths(root) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			Mounts []any `json:"mounts"`
		}
		if err := json.Unmarshal([]byte(stripJSONC(string(data))), &cfg); err != nil {
			continue
		}
		for _, m := range cfg.Mounts {
			switch v := m.(type) {
			case string:
				if fields, _ := devcontainerParseMountString(v); fields["target"] == containerTarget {
					return fields["source"], true
				}
			case map[string]any:
				if t, _ := v["target"].(string); t == containerTarget {
					s, _ := v["source"].(string)
					return s, true
				}
			}
		}
	}
	return "", false
}

// devcontainerParseMountString parses Docker's comma-separated mount string
// form ("source=...,target=...,type=bind,readonly") into a key=value field
// map AND a set of bare boolean flag tokens. Docker's own --mount/mounts
// syntax expresses read-only as a bare `readonly` (or `ro`) token with no
// "=" — the ORIGINAL implementation split only on "=" and silently dropped
// any such token, which made a read-only declaration undetectable.
func devcontainerParseMountString(s string) (fields map[string]string, flags map[string]bool) {
	fields = map[string]string{}
	flags = map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		} else {
			flags[part] = true
		}
	}
	return fields, flags
}

// devcontainerDeclaredMountSourceMissing returns a stderr notice when the
// project's devcontainer.json already declares a mount at containerTarget but
// its source — after expanding any ${localEnv:NAME} substitution against
// THIS host's own environment — does not exist here (§12.3's mixed-OS team
// case: a config committed by a teammate on a different OS). "" means
// nothing to warn about: no declared mount, a source using a substitution
// this function does not understand (not guessed at), or a source that
// resolves and exists.
func devcontainerDeclaredMountSourceMissing(root, containerTarget string) string {
	source, found := devcontainerDeclaredMountSource(root, containerTarget)
	if !found || source == "" {
		return ""
	}
	// Reject an unrecognized substitution form up front, checked against the
	// ORIGINAL source with every ${localEnv:...} occurrence stripped — not
	// against the resolved value below, which could coincidentally contain a
	// literal "${" of its own (e.g. inside a path) and produce a false
	// "unresolved" positive.
	if strings.Contains(devcontainerLocalEnvRE.ReplaceAllString(source, ""), "${") {
		return ""
	}
	resolved := devcontainerLocalEnvRE.ReplaceAllStringFunc(source, func(m string) string {
		name := devcontainerLocalEnvRE.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
	if _, err := os.Stat(filepath.FromSlash(resolved)); err != nil {
		return fmt.Sprintf("notice: devcontainer.json declares a mount at %s from %q, which does not exist on "+
			"this host (mixed-OS team? see design/harness/unified-harness-pool.md §12.3)", containerTarget, source)
	}
	return ""
}

// --- helpers ---

func gitToplevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// wraptoolModuleLine is go.mod's exact module declaration for wraptool's own
// source checkout — the only case the stale-binary notice below applies to.
const wraptoolModuleLine = "module github.com/pti/wraptool"

// isWraptoolModuleRoot reports whether root/go.mod declares wraptool's own
// module. Pure filesystem read; a missing/unreadable go.mod is simply "no".
// The match is deliberately exact (the whole trimmed line, nothing appended
// or prepended): a module line carrying a trailing comment, or one that only
// contains the import path as a substring (a fork, a vanity-suffixed
// module), reads as "not wraptool" rather than risk a false positive.
func isWraptoolModuleRoot(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == wraptoolModuleLine {
			return true
		}
	}
	return false
}

// gitHeadSHA reads the full commit SHA of root's checked-out HEAD.
func gitHeadSHA(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// shortSHA truncates a full SHA the same way buildinfo.Get does for a devel
// version, so the notice below and `wraptool version`'s own output agree.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// staleBinaryNotice returns a one-line stderr notice (or "" for nothing) when
// this wraptool binary was built from a different commit than the wraptool
// source checkout `up` is running in — bites during self-hosted development:
// build, forget to reinstall, and the harness keeps exercising yesterday's
// code. Pure and read-only; headSHA is injected so tests never shell to git.
// Silent whenever the comparison can't be made confidently: not a wraptool
// checkout, a release build with no stamped VCS revision, or git not
// answerable — this must never block `up`.
func staleBinaryNotice(root, buildRev string, headSHA func() (string, error)) string {
	if buildRev == "" {
		return ""
	}
	if !isWraptoolModuleRoot(root) {
		return ""
	}
	head, err := headSHA()
	if err != nil || head == "" {
		return ""
	}
	if head == buildRev {
		return ""
	}
	return fmt.Sprintf("notice: this wraptool binary was built from %s but this checkout is at %s — "+
		"rebuild (go build -o wraptool .) and reinstall to test current code",
		shortSHA(buildRev), shortSHA(head))
}

// printStaleBinaryNotice wires staleBinaryNotice to the real build info and a
// real git HEAD read; see staleBinaryNotice for the actual logic and when it
// stays silent.
func printStaleBinaryNotice(root string) {
	if msg := staleBinaryNotice(root, buildinfo.Get().Revision, func() (string, error) { return gitHeadSHA(root) }); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
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
