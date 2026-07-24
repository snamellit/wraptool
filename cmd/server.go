package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"

	"github.com/spf13/cobra"
)

// newServerCmd groups lifecycle commands for the shared MCP server that
// `wraptool up` starts detached.
func newServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Inspect or stop the shared wraptool server",
	}
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
			if cfg, err := config.Load(configOrDefault(configPath)); err == nil && cfg.MCP.Listen != "" {
				probe = probeAddr(cfg.MCP.Listen)
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
			if !processAlive(pid) {
				_ = os.Remove(pidFilePath())
				fmt.Printf("server (pid %d) was not running; cleared stale pidfile\n", pid)
				return nil
			}
			if err := terminateProcess(pid); err != nil {
				return fmt.Errorf("signaling pid %d: %w", pid, err)
			}
			// Give it a moment to exit, then clean up.
			for range 20 {
				if !processAlive(pid) {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			_ = os.Remove(pidFilePath())
			fmt.Printf("stopped server (pid %d)\n", pid)
			return nil
		},
	}
	return cmd
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

func addrSuffix(probe string) string {
	if probe == "" {
		return ""
	}
	return " at " + probe
}
