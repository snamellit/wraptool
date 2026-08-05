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
	"strings"
	"time"

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
	cmd.Flags().BoolVar(&opts.noAdmin, "no-admin", false, "do not auto-enable the operator review UI")
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

// --- devcontainer harness-pool runtime mount (design/harness/unified-harness-pool.md §12.2) ---

// devcontainerPoolPlan is the harness-pool runtime-mount/PATH/notice decision
// for one `devcontainer up` invocation — the devcontainer analogue of
// cmd/guixcontainer.go's guixPoolPlan, adapted to the Phase-4 mixed state
// (§16 migration intro: "runtimes prefer runtime/ whenever an active pointer
// exists ... else fall back to legacy ... with a deprecation notice"). Unlike
// the guix plan, "legacy" here is simply "no runtime mount at all" — the
// devcontainer runtime never had a bin/+lib/ expose to keep alongside; absent
// an active pointer, the harness binary keeps coming from the wraptool
// Feature's per-image provisioning, unchanged (§16 Phase 4 item 5 retires
// that path in a LATER wave, not this one).
type devcontainerPoolPlan struct {
	mountRuntime bool   // true: bind pool runtime/ read-only at devcontainerPoolRuntime, append PATH
	runtimeDir   string // host path to mount when mountRuntime (ToSlash applied at argv-build time, §6.3)
	notice       string // non-empty: legacy-fallback notice for stderr, gated on pool-lockability
}

// devcontainerPoolPlanInput bundles the paths and lockability
// resolveDevcontainerPoolPlan needs — named fields for the same reason as
// guixPoolPlanInput (cmd/guixcontainer.go): several interchangeable path
// strings is exactly where an untested positional call silently transposes.
type devcontainerPoolPlanInput struct {
	// Harness is the harness name — the dispatcher/generation entrypoint file
	// name, and the name cited in any deprecation notice.
	Harness string
	// PoolLockable gates the notice (§16): a harness with no runtime/ path to
	// move to (no pool package/archive source) falls back silently — a notice
	// promising a fix command that can't exist for it would be noise.
	PoolLockable bool
	// PointerPath is pool.PointerFile(<target platform key>, Harness), or ""
	// when the platform key itself could not be resolved — treated
	// identically to "no active pointer" (mirrors guixPoolPlanInput.PointerPath).
	PointerPath string
	// RuntimeDir is pool.RuntimeDir() — mounted read-only at
	// devcontainerPoolRuntime when the plan is active.
	RuntimeDir string
	// RuntimeBinDir is pool.RuntimeBinDir(): RuntimeBinDir/Harness (the
	// generated dispatcher) must exist for the pointer to be considered live.
	RuntimeBinDir string
	// RuntimeGenerationsDir is pool.RuntimeGenerationsDir(); the pointer's
	// referenced generation's own RuntimeGenerationsDir/<genID>/bin/Harness
	// must also exist — a pointer can outlive its generation (a concurrent gc).
	RuntimeGenerationsDir string
}

// devcontainerPoolPathAppend is the §12.2 PATH value carried on --remote-env:
// the existing container PATH via the Dev Container spec's
// ${containerEnv:...} substitution, then the dispatcher directory. Append,
// never prepend (§12.1's never-shadow rule applies here too — a pool tool
// must not shadow whatever the image's own PATH already resolves to).
const devcontainerPoolPathAppend = "${containerEnv:PATH}:" + devcontainerPoolRuntimeBin

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

// legacyDevcontainerPoolPlan builds the no-runtime-mount plan, gating notice
// on in.PoolLockable exactly as guixcontainer.go's legacyGuixPoolPlan does.
func legacyDevcontainerPoolPlan(in devcontainerPoolPlanInput, notice string) devcontainerPoolPlan {
	plan := devcontainerPoolPlan{}
	if in.PoolLockable {
		plan.notice = notice
	}
	return plan
}

// buildDevcontainerUpArgs assembles the `devcontainer up` argv: workspace
// folder, the pool home/ bind (skipped when the config already declares a
// mount at devcontainerPoolHome — duplicate bind targets are a Docker error),
// and, when plan.mountRuntime, the pool runtime/ read-only bind (likewise
// skipped when already declared) plus the PATH remote-env append. Pure string
// building, no exec, so tests can pin it exactly — including byte-identity
// with the pre-runtime-mount argv when plan.mountRuntime is false and neither
// mount is declared (this package's "pin current behavior before
// refactoring" requirement).
func buildDevcontainerUpArgs(root, poolHome string, homeDeclared, runtimeDeclared bool, plan devcontainerPoolPlan) []string {
	args := []string{"up", "--workspace-folder", root}
	if !homeDeclared {
		args = append(args, "--mount", "type=bind,source="+filepath.ToSlash(poolHome)+",target="+devcontainerPoolHome)
	}
	if plan.mountRuntime {
		if !runtimeDeclared {
			args = append(args, "--mount",
				"type=bind,source="+filepath.ToSlash(plan.runtimeDir)+",target="+devcontainerPoolRuntime+",readonly")
		}
		args = append(args, "--remote-env", "PATH="+devcontainerPoolPathAppend)
	}
	return args
}

// --- container ---

func bringUpContainer(root, token, hname string, shell bool) error {
	if !hasDevcontainer(root) {
		return fmt.Errorf(`no dev container found under %s/.devcontainer

Scaffold one, then re-run `+"`wraptool up`"+`:

  wraptool init devcontainer --lang go        # writes .devcontainer/devcontainer.json
                                              # (--harness claude|opencode|pi|agy|gemini, --base, --lang; see --help)

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

	// Locked runtime/ mount (§12.2): mirrors cmd/guixcontainer.go's
	// resolveGuixPoolPlan, gated on the SAME per-harness active pointer, but
	// for the devcontainer TARGET platform. §5.1: the devcontainer target's
	// arch is the Docker daemon's default platform, which the daemon shares
	// with the host in every setup this wave supports (no remote
	// DOCKER_HOST); pool.HostPlatformKey() (host GOARCH, glibc always) is
	// therefore used as the practical stand-in, not a `docker info` probe.
	var pointerPath string
	if key, err := pool.HostPlatformKey(); err == nil {
		pointerPath = pool.PointerFile(key, hname)
	}
	// Unlike the guix runtime — whose bringUpGuixContainer calls harness.Write
	// first, which rejects an unknown --harness before reaching its own pool
	// plan — the devcontainer runtime writes no MCP config on the host at all
	// (that is the container-side Feature's connect.sh's job here), so hname
	// has NOT already been validated. Reject an unknown one plainly rather
	// than silently treating it as "not lockable".
	desc, knownHarness := harness.Registry[hname]
	if !knownHarness {
		return fmt.Errorf("unknown harness %q (want one of: %s)", hname, strings.Join(harness.Names(), ", "))
	}
	plan := resolveDevcontainerPoolPlan(devcontainerPoolPlanInput{
		Harness:               hname,
		PoolLockable:          desc.PoolLockable(),
		PointerPath:           pointerPath,
		RuntimeDir:            pool.RuntimeDir(),
		RuntimeBinDir:         pool.RuntimeBinDir(),
		RuntimeGenerationsDir: pool.RuntimeGenerationsDir(),
	})
	if plan.notice != "" {
		fmt.Fprintln(os.Stderr, plan.notice)
	}

	fmt.Printf("bringing up dev container for %s ...\n", root)

	// Defer to a config that already declares a mount at either target (e.g.
	// one written by `wraptool init devcontainer`, or a standalone
	// Reopen-in-Container setup): adding our own --mount on top would be a
	// duplicate mount point and docker rejects it (§12.2 extends the
	// pre-existing home-only check to both targets). Either way, warn when
	// the declared source does not actually exist on this host — the
	// mixed-OS case, §12.3.
	homeDeclared := devcontainerDeclaresMount(root, devcontainerPoolHome)
	if homeDeclared {
		fmt.Printf("harness pool: home mount declared in devcontainer.json (%s)\n", devcontainerPoolHome)
		if warn := devcontainerDeclaredMountSourceMissing(root, devcontainerPoolHome); warn != "" {
			fmt.Fprintln(os.Stderr, warn)
		}
	} else {
		fmt.Printf("harness pool: sharing %s -> %s (state persists across projects)\n", poolHome, devcontainerPoolHome)
	}

	runtimeDeclared := devcontainerDeclaresMount(root, devcontainerPoolRuntime)
	if runtimeDeclared {
		if warn := devcontainerDeclaredMountSourceMissing(root, devcontainerPoolRuntime); warn != "" {
			fmt.Fprintln(os.Stderr, warn)
		}
	}
	if plan.mountRuntime {
		if runtimeDeclared {
			fmt.Printf("harness pool: runtime mount declared in devcontainer.json (%s)\n", devcontainerPoolRuntime)
		} else {
			fmt.Printf("harness pool: locked runtime %s -> %s, read-only (dispatcher appended to PATH)\n", plan.runtimeDir, devcontainerPoolRuntime)
		}
	}

	upArgs := buildDevcontainerUpArgs(root, poolHome, homeDeclared, runtimeDeclared, plan)
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
	execArgs := []string{"exec", "--workspace-folder", root}
	if plan.mountRuntime {
		// remoteEnv does not persist across separate CLI invocations — the
		// devcontainer CLI's own user-env probe reverts it otherwise (see
		// features/src/wraptool/devcontainer-feature.json's containerEnv
		// comment on HOME, the same limitation) — so every invocation WE
		// issue must repeat it. A plain `devcontainer exec` run outside
		// `wraptool up` (a manual terminal, an IDE) will not see the
		// appended PATH until the Phase 4 Feature cutover bakes it into
		// containerEnv instead (§16 item 1; features/src changes are a
		// non-goal of this package).
		execArgs = append(execArgs, "--remote-env", "PATH="+devcontainerPoolPathAppend)
	}
	execArgs = append(execArgs, "bash", "-l")
	sh := exec.Command("devcontainer", execArgs...)
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
// check, not a JSONC parse: the target path only ever appears in a user's
// config as a mount's target (a feature's own containerEnv/HOME pin never
// touches the user's file), so its presence is a reliable signal that
// `wraptool up` must not add a second, duplicate mount at that target. §12.2
// extends the original home-only check to both mount targets (home and,
// newly, runtime).
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

// devcontainerMountCommentRE strips // line comments before JSON-decoding a
// devcontainer.json (JSONC) — the same technique the scaffolder's own tests
// use (cmd/init_devcontainer_test.go's parseJSONC): the configs this codebase
// generates or expects never carry "//" inside a string value.
var devcontainerMountCommentRE = regexp.MustCompile(`(?m)\s*//.*$`)

// devcontainerLocalEnvRE matches a Dev Container ${localEnv:NAME} variable
// substitution — the only substitution form the shipped pool-mount sources
// use (poolHomeMountSource in cmd/init_devcontainer.go, docs/devcontainers.qmd).
var devcontainerLocalEnvRE = regexp.MustCompile(`\$\{localEnv:([A-Za-z_][A-Za-z0-9_]*)\}`)

// devcontainerDeclaredMountSource returns the "source" of the first mounts
// entry — object form {"source":...,"target":...} or Docker's
// comma-separated string form "source=...,target=...,type=bind" — targeting
// containerTarget, across the project's devcontainer config files.
// Best-effort: an unparsable config or no matching mount yields found=false.
// This feeds only the mixed-OS missing-source warning below, never a hard
// failure — devcontainerDeclaresMount's cheaper substring check remains the
// source of truth for "is a mount declared at all".
func devcontainerDeclaredMountSource(root, containerTarget string) (source string, found bool) {
	for _, p := range devcontainerConfigPaths(root) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			Mounts []any `json:"mounts"`
		}
		stripped := devcontainerMountCommentRE.ReplaceAllString(string(data), "")
		if err := json.Unmarshal([]byte(stripped), &cfg); err != nil {
			continue
		}
		for _, m := range cfg.Mounts {
			switch v := m.(type) {
			case string:
				if fields := devcontainerParseMountString(v); fields["target"] == containerTarget {
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
// form ("source=...,target=...,type=bind") into a key/value map.
func devcontainerParseMountString(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
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
