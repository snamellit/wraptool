// Package backend implements the harness-pool provisioning seam
// (design/unified-harness-pool.md §11.3): the only place that knows how to
// invoke guix / docker / podman to run one Linux container step for the
// resolve and realize phases (§6.2). It is deliberately self-contained — no
// imports of sibling internal/pool packages — so it compiles and tests in
// isolation from the rest of the pool implementation.
package backend

import (
	"context"
	"os"
	"os/exec"
	"sort"
)

// Mount is one bind mount into the container: a host path exposed at a
// container path, optionally read-only. Both fields are plain strings; the
// backend that serializes a Mount into a CLI argument is responsible for any
// host-OS path-syntax conversion (see oci.go's filepath.ToSlash use, §6.3).
type Mount struct {
	Host      string
	Container string
	ReadOnly  bool
}

// RunSpec describes one container invocation. The zero value is the most
// restrictive posture: no network, no mounts, no env, backend-default
// platform and workdir.
type RunSpec struct {
	// Argv is the command to execute inside the container.
	Argv []string
	// Mounts are the bind mounts to expose inside the container.
	Mounts []Mount
	// Network enables outbound network access. False (the zero value) means
	// an isolated container in both backends.
	Network bool
	// Workdir is the container-side working directory. Empty means the
	// backend's own default.
	Workdir string
	// Env is the set of environment variables the child process (and, for
	// guix, the container it launches) should see.
	Env map[string]string
	// Platform is the container architecture ("amd64"/"arm64"). Empty means
	// the backend's default (the host architecture).
	Platform string
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
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
