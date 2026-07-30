// Package backend implements the harness-pool provisioning seam
// (design/unified-harness-pool.md §11.3): the only place that knows how to
// invoke guix / docker / podman to run one Linux container step for the
// resolve and realize phases (§6.2). It is deliberately self-contained — no
// imports of sibling internal/pool packages — so it compiles and tests in
// isolation from the rest of the pool implementation.
package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
)

// Mount is one bind mount into the container: a host path exposed at a
// container path, optionally read-only. Both fields are plain strings; the
// backend that serializes a Mount into a CLI argument is responsible for any
// host-OS path-syntax conversion (see oci.go's toSlash use, §6.3).
type Mount struct {
	Host      string
	Container string
	ReadOnly  bool
}

// RunSpec describes one container invocation. The zero value is the most
// restrictive posture: no network, no mounts, no env, backend-default
// platform, user, and workdir.
type RunSpec struct {
	// Argv is the command to execute inside the container. Empty is
	// rejected by both backends: a bare `guix shell ... --` (or a container
	// started with no command) would drop into an interactive shell and
	// hang rather than fail fast.
	Argv []string
	// Mounts are the bind mounts to expose inside the container.
	Mounts []Mount
	// Network enables outbound network access. False (the zero value) means
	// an isolated container in both backends.
	Network bool
	// Workdir is the CONTAINER-side working directory the command starts
	// in. Empty means the backend's own default. This is always a container
	// path, never a host path, and must never be assigned to a host
	// exec.Cmd's working directory — see guix.go's `env --chdir=`
	// realization and oci.go's native -w flag, neither of which touches the
	// host process's own cwd.
	Workdir string
	// Env is the set of environment variables the child process (and, for
	// guix, the container it launches) should see.
	Env map[string]string
	// Platform is the container architecture ("amd64"/"arm64"). Empty means
	// the backend's default (the host architecture). Platform carries
	// architecture ONLY — libc (e.g. glibc vs musl) is not expressed here;
	// the caller encodes that choice through which provisioning image (oci)
	// or package set (guix) it selects (design/unified-harness-pool.md
	// §5.1/§6.1).
	Platform string
	// User is the in-container identity to run as, in the backend's own
	// syntax (oci: "uid:gid"). Empty means the backend's default — see
	// oci.go's ociUser for the per-engine default logic. guix's --container
	// always runs as its own synthetic single user regardless of this
	// field, so User is a no-op for the guix backend.
	User string
}

// Backend runs one Linux container step. It is the ONLY place that knows how
// to invoke guix / docker / podman (design/unified-harness-pool.md §11.3).
type Backend interface {
	// Name identifies the backend ("guix" or "oci").
	Name() string
	// Available reports whether this backend's tooling is usable on the
	// current host (e.g. the relevant binary is on PATH).
	Available() bool
	// Run executes spec.Argv in a Linux container. Network is off unless
	// spec.Network is true.
	Run(ctx context.Context, spec RunSpec) error
}

// commandRunner is the injectable process-execution seam. A Backend builds an
// argv and describes the *additional* environment variables the child
// process needs; env is a delta, not a full replacement — the production
// runner merges it onto the host's inherited environment (the guix/docker/
// podman client itself needs the ambient PATH, HOME, etc. to run at all; see
// execRunner.Run). Unit tests substitute a fake that records the call instead
// of executing it, so they can assert the exact generated argv without
// running any container tooling.
type commandRunner interface {
	Run(ctx context.Context, name string, args []string, env map[string]string, dir string) error
}

// maxStderrTail bounds how much stderr execRunner retains for a failed
// command's error message, so a runaway provisioning script's output can't
// balloon memory while still leaving enough context to diagnose a failure.
const maxStderrTail = 4096

// tailBuffer is an io.Writer that retains only the most recently written
// maxStderrTail bytes.
type tailBuffer struct {
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if excess := t.buf.Len() - maxStderrTail; excess > 0 {
		t.buf.Next(excess)
	}
	return len(p), nil
}

// execRunner is the production commandRunner: exec.CommandContext with an
// explicit argv array, never a shell (no `sh -c` string interpolation —
// CLAUDE.md: no shell execution in product code).
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, env map[string]string, dir string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		merged := os.Environ()
		for k, v := range env {
			merged = append(merged, k+"="+v)
		}
		cmd.Env = merged
	}
	cmd.Dir = dir
	// Provisioning containers run third-party npm lifecycle scripts, so
	// stdin is never wired to the host tty: a writable tty fd inside such a
	// container is a TIOCSTI-class escape surface, and resolve/realize are
	// non-interactive by design (§6.2) — there is nothing for them to read
	// from stdin anyway.
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	var tail tailBuffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &tail)

	if err := cmd.Run(); err != nil {
		if captured := tail.buf.String(); captured != "" {
			return fmt.Errorf("%s: %w\n--- stderr tail ---\n%s", name, err, captured)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// lookPath resolves an executable on PATH. A package-level var so tests can
// stub backend-tool detection (guix / docker / podman) without depending on
// what's actually installed on the machine running the tests — the same
// pattern as cmd/guixcontainer.go's hasGuix.
var lookPath = exec.LookPath

// sortedKeys returns m's keys in sorted order, for deterministic argv
// generation (map iteration order is randomized in Go).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
