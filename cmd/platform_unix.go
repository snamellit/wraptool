//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func configureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func terminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

func terminationSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

func handleSIGHUP(configPath string, reg *registrar) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	for range sighup {
		reloadConfig(configPath, reg, "SIGHUP")
	}
}

// isTTY reports whether f is a terminal. A character-device check is not
// enough: /dev/null is one too, and treating it as a terminal made `up`
// pass `sbx exec -it` with no terminal behind it. TIOCGWINSZ succeeds only
// on a terminal and has the same name on Linux and the BSDs.
func isTTY(f *os.File) bool {
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	return err == nil
}
