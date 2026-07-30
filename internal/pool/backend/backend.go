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
	"regexp"
	"sort"
	"strings"
	"time"
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
	// oci.go's ociUser for the per-engine default logic. The guix backend
	// REJECTS a non-empty User: guix --container always runs as the
	// invoking host uid, and that fixed uid-mapping — not a selectable
	// identity — is exactly what keeps files the container writes owned by
	// the developer, so a request for a different identity cannot be
	// honored and is a caller bug, not silently ignored.
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

// envKeyPattern is the only shape of environment-variable name either
// backend will turn into a CLI flag. An exotic key name is a caller bug we
// want surfaced immediately, not silently smuggled through — deliberately no
// regexp.QuoteMeta anywhere this is used; an invalid key is rejected, not
// escaped.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnvKeys checks every key in env against envKeyPattern, returning an
// error naming the first invalid key (in sorted order, for a deterministic
// message) it finds. Shared by both backends' Run so the same rule applies
// regardless of which one executes a given RunSpec.
func validateEnvKeys(env map[string]string) error {
	for _, k := range sortedKeys(env) {
		if !envKeyPattern.MatchString(k) {
			return fmt.Errorf("invalid env var name %q (want %s)", k, envKeyPattern.String())
		}
	}
	return nil
}

// validateMount rejects a Host or Container path containing ',' — reject,
// don't escape, the same stance as validateEnvKeys. OCI's --mount syntax is
// comma-delimited (type=bind,src=...,dst=...[,readonly]), so a comma there is
// an outright syntax break there; guix's --share=/--expose= syntax doesn't
// need comma-escaping today, but Mount is shared across both backends and a
// spec that's only valid for one of them would silently stop being portable
// between them (the §17 cross-backend equality expectation), so both
// backends enforce the same rule rather than only the one that strictly
// needs it.
func validateMount(m Mount) error {
	if strings.Contains(m.Host, ",") {
		return fmt.Errorf("mount host path %q contains ',' which is not supported", m.Host)
	}
	if strings.Contains(m.Container, ",") {
		return fmt.Errorf("mount container path %q contains ',' which is not supported", m.Container)
	}
	return nil
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
	// cmd.Stderr isn't an *os.File, so exec.Cmd copies it through a pipe on a
	// background goroutine; cmd.Wait() (inside cmd.Run() below) blocks until
	// that goroutine sees EOF, not just until the process exits. If a
	// grandchild spawned inside the container (or by a container runtime
	// itself) inherits the pipe's write end and outlives the main process,
	// EOF never arrives and Wait() hangs past ctx cancellation — wedging Run
	// forever and, with it, the §6.5 pool lock this call is almost always
	// made under. WaitDelay bounds that: once the process is done (or ctx is
	// cancelled), Go forces the pipe closed after the delay instead of
	// waiting on it indefinitely.
	cmd.WaitDelay = 10 * time.Second

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
