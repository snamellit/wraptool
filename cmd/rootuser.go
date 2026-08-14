package cmd

import (
	"errors"
	"fmt"
	"os"
)

// errRunningAsRoot is returned by refuseRoot when the effective uid is 0, so a
// caller can distinguish the refusal from an ordinary command failure.
var errRunningAsRoot = errors.New("wraptool refuses to run as root")

// refuseRoot reports an error when this process's effective uid is 0.
//
// wraptool exists to keep credentials on one side of a boundary and an AI
// harness on the other. Root dissolves that boundary in every direction at
// once: every wrapped tool it spawns inherits uid 0, so a policy allowing
// `git pull` allows it against any file on the host; the config whose
// permissions decide who may grant capabilities becomes writable by the very
// process reading it; and the harness pool stops being per-developer. No
// deployment needs it — a service manager runs wraptool as a dedicated user,
// a developer runs it as themselves.
//
// The EFFECTIVE uid is what matters, so a setuid binary is caught too. A no-op
// on Windows, where os.Geteuid returns -1.
func refuseRoot() error { return refuseRootFor(os.Geteuid()) }

// refuseRootFor is refuseRoot's testable core: the uid is a parameter because a
// test cannot become root, and a refusal that is never exercised is a refusal
// nobody has read.
func refuseRootFor(euid int) error {
	if euid != 0 {
		return nil
	}
	return fmt.Errorf("%w: it would spawn every wrapped tool as root, and a credential "+
		"boundary enforced by a root process is not a boundary. Run wraptool as the user that "+
		"owns the credentials — a dedicated service user, or yourself", errRunningAsRoot)
}
