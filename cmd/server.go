package cmd

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/pool"

	"github.com/spf13/cobra"
)

// newServerCmd groups lifecycle commands for the shared MCP server that
// `wraptool up` starts detached.
func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Start, inspect, or stop the shared wraptool server",
	}
	cmd.AddCommand(newServeCmd("start", false))
	cmd.AddCommand(newServerStatusCmd())
	cmd.AddCommand(newServerStopCmd())
	return cmd
}

func newServerStatusCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the shared server is running and reachable",
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, _ := readPidFile()
			alive := pid > 0 && processAlive(pid)

			var probe string
			var adminEnabled bool
			if cfg, err := config.Load(configOrDefault(configPath)); err == nil {
				if cfg.MCP.Listen != "" {
					probe = probeAddr(cfg.MCP.Listen)
				}
				adminEnabled = cfg.Admin.Listen != ""
			}
			reachable := probe != "" && dialOK(probe)

			switch {
			case alive && reachable:
				fmt.Printf("running (pid %d, reachable at %s)\n", pid, probe)
			case alive:
				fmt.Printf("process alive (pid %d) but not reachable%s\n", pid, addrSuffix(probe))
			case reachable:
				fmt.Printf("reachable at %s (no pidfile — started externally or by an older wraptool)\n", probe)
			default:
				fmt.Println("not running")
			}
			if adminEnabled && (alive || reachable) {
				fmt.Println("review UI: wraptool admin open")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file")
	return cmd
}

func newServerStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the shared wraptool server",
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, err := readPidFile()
			if err != nil || pid <= 0 {
				return fmt.Errorf("no pidfile at %s; the server isn't running (or was started by an older wraptool that didn't record one — stop it however you started it)", pidFilePath())
			}
			stopped, err := stopByPid(pid)
			if err != nil {
				return err
			}
			if stopped {
				fmt.Printf("stopped server (pid %d)\n", pid)
			}
			return nil
		},
	}
	return cmd
}

// stopByPid terminates the server identified by pid, waits for it to exit,
// and clears the pidfile + serve state. Returns stopped=false (no error) when
// the pid was already gone (a stale pidfile is cleaned up). Shared by
// `server stop` and `up`'s stale-server reconciliation.
func stopByPid(pid int) (bool, error) {
	if !processAlive(pid) {
		_ = os.Remove(pidFilePath())
		_ = os.Remove(serveStatePath())
		fmt.Printf("server (pid %d) was not running; cleared stale pidfile\n", pid)
		return false, nil
	}
	if err := terminateProcess(pid); err != nil {
		return false, fmt.Errorf("signaling pid %d: %w", pid, err)
	}
	for range 20 {
		if !processAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		// The process didn't exit within the grace period. Leave the pidfile
		// and state in place so `server stop`/`status` still find it — removing
		// them here would orphan a live server with no way to stop it later.
		return false, fmt.Errorf("server (pid %d) did not exit within 2s after SIGTERM; stop it manually", pid)
	}
	_ = os.Remove(pidFilePath())
	_ = os.Remove(serveStatePath())
	return true, nil
}

// writePidFile records the current process's pid so `server stop`/`status` can
// find the server no matter how it was started (`up`, a direct `wraptool serve`,
// or a service manager). serve calls this for the persistent transports; it is
// the single writer of the pidfile.
func writePidFile() error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return fmt.Errorf("creating runtime dir: %w", err)
	}
	if err := os.WriteFile(pidFilePath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("writing pidfile: %w", err)
	}
	return nil
}

func removePidFile() { _ = os.Remove(pidFilePath()) }

func readPidFile() (int, error) {
	b, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// serveState records the *effective* runtime state of the running server —
// what the process is actually doing, never what the config file says —
// written next to the pidfile (design/doctor/wraptool-doctor.md §4.1). `up`
// reads Listen+Auth to detect a stale server whose bind/auth no longer
// matches the desired runtime-adjusted config (e.g. a loopback+no-auth
// server left over from a previous session when `up` now needs a bridged
// 0.0.0.0+auth bind for a devcontainer) and restarts it instead of silently
// reusing an incompatible listener; `wraptool doctor`'s runtime.* checks
// (a later step) read the rest.
//
// uid is deliberately not a plain field: 0 is root, a legitimate value, and
// also the zero value an absent "uid" key would decode to. It is tracked
// with its own presence flag and read back only through UID(), so a
// consumer can never mistake "unknown" for "root". Every other field is a
// plain string/bool whose Go zero value ("" / false) already means "absent
// or unknown" for every value it can legitimately hold, so no separate
// presence tracking is needed for those.
type serveState struct {
	Listen string
	Auth   bool

	Transport      string
	AdminListen    string
	ConfigPath     string
	Path           string
	StartedAt      string // RFC3339, process start.
	ConfigLoadedAt string // RFC3339, last successful load or reload.

	uid    int
	hasUID bool
}

// UID returns the uid this server is running as and whether it was
// recorded. It is absent from an old-format state file (written before this
// field existed) and on Windows, where os.Getuid() has no meaning and the
// key is omitted entirely on write (see effectiveUID).
func (s serveState) UID() (uid int, ok bool) { return s.uid, s.hasUID }

func serveStatePath() string { return filepath.Join(paths.RuntimeDir(), "serve.state") }

// effectiveUID reports the uid to record in the serve state, or ok=false
// where uid has no meaning: os.Getuid() always returns -1 on Windows, and
// that must not be recorded as if it were a real (and, worse, negative) uid.
func effectiveUID() (uid int, ok bool) {
	uid = os.Getuid()
	if uid < 0 {
		return 0, false
	}
	return uid, true
}

// effectiveConfigPath resolves configPath the same way a consumer comparing
// it against another path must: absolute, and with symlinks resolved, so two
// different spellings of the same file compare equal. Best-effort — a path
// that cannot be resolved (e.g. it doesn't exist yet) still yields the
// absolute form rather than failing the write.
func effectiveConfigPath(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return configPath
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// writeServeState writes the full effective state, via write-temp-then-rename
// in the same directory (paths.RuntimeDir()) — serve.state is rewritten on
// every reload while doctor and `up` may be reading it, so a plain
// os.WriteFile would make a torn read reachable.
func writeServeState(s serveState) error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return fmt.Errorf("creating runtime dir: %w", err)
	}
	var b strings.Builder
	serveStateWriteLine(&b, "listen", s.Listen)
	serveStateWriteLine(&b, "auth", strconv.FormatBool(s.Auth))
	serveStateWriteLine(&b, "transport", s.Transport)
	serveStateWriteLine(&b, "admin_listen", s.AdminListen)
	if s.hasUID {
		serveStateWriteLine(&b, "uid", strconv.Itoa(s.uid))
	}
	serveStateWriteLine(&b, "config_path", s.ConfigPath)
	serveStateWriteLine(&b, "path", s.Path)
	serveStateWriteLine(&b, "started_at", s.StartedAt)
	serveStateWriteLine(&b, "config_loaded_at", s.ConfigLoadedAt)

	if err := pool.WriteFileAtomic(serveStatePath(), []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("writing serve state: %w", err)
	}
	return nil
}

func serveStateWriteLine(b *strings.Builder, key, val string) {
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(serveStateEscape(val))
	b.WriteByte('\n')
}

// serveStateOwned records that THIS process wrote the serve state file at
// startup, and is the invariant that makes the reload update safe: the state
// file is a single well-known path in the XDG runtime dir, but not every
// wraptool process writes one. A per-session transport is excluded from the
// pidfile/state write entirely, yet it still hot-reloads its own config — so a
// reload hook that simply read-modify-wrote whatever file happened to be there
// would stamp config_loaded_at into a DIFFERENT, long-running server's state,
// recording a load that server never performed and destroying exactly the
// signal runtime.stale-policy reads.
//
// Bounding it by ownership rather than by naming the transports that do not
// write state also closes the read-modify-write's second hazard for free: only
// the process that wrote every key can rewrite them, so no binary can drop a
// key its own reader does not know.
var serveStateOwned atomic.Bool

// markServeStateOwned is called once, immediately after the startup write
// succeeds.
func markServeStateOwned() { serveStateOwned.Store(true) }

// updateServeStateConfigLoadedAt refreshes only config_loaded_at in the serve
// state file, on a successful reload (cmd/reload.go's reloadConfig, success
// path only). The process does not rebind on reload, so every other field —
// especially Listen/Auth/Transport/AdminListen — must keep its start-time
// value; a failed reload must not call this at all, since an untouched
// timestamp is exactly the stale-policy signal runtime.stale-policy needs.
//
// A no-op unless this process owns the state file (serveStateOwned).
func updateServeStateConfigLoadedAt(now time.Time) {
	if !serveStateOwned.Load() {
		return
	}
	s, err := readServeState()
	if err != nil {
		return
	}
	s.ConfigLoadedAt = now.Format(time.RFC3339)
	if err := writeServeState(s); err != nil {
		log.Printf("updating serve state: %s", err)
	}
}

func removeServeState() { _ = os.Remove(serveStatePath()) }

// readServeState parses key=value lines tolerantly: an unknown key is
// ignored (forward compat — an older wraptool reads a newer file) and an
// unparseable line is skipped, mirroring the original two-field reader. Keys
// are collected into a map first so presence (as opposed to zero value) can
// be tracked for uid, the one field whose zero value collides with a
// legitimate value.
func readServeState() (serveState, error) {
	b, err := os.ReadFile(serveStatePath())
	if err != nil {
		return serveState{}, err
	}
	m := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[key] = serveStateUnescape(val)
	}

	s := serveState{
		Listen:         m["listen"],
		Auth:           m["auth"] == "true",
		Transport:      m["transport"],
		AdminListen:    m["admin_listen"],
		ConfigPath:     m["config_path"],
		Path:           m["path"],
		StartedAt:      m["started_at"],
		ConfigLoadedAt: m["config_loaded_at"],
	}
	if v, present := m["uid"]; present {
		if n, err := strconv.Atoi(v); err == nil {
			s.uid, s.hasUID = n, true
		}
	}
	return s, nil
}

// serveStateEscape/serveStateUnescape keep the wire format key=value: a
// newline is the one hazard (a POSIX filename may contain one, and
// config_path/path are filesystem-derived), so it — and the escape character
// itself — are escaped on write and restored on read. strings.Cut on the
// first "=" (in readServeState) already keeps a value containing "=" intact,
// so "=" itself needs no escaping.
func serveStateEscape(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func serveStateUnescape(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			switch v[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

func addrSuffix(probe string) string {
	if probe == "" {
		return ""
	}
	return " at " + probe
}
