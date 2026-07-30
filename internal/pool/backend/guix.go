package backend

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
)

// envKeyPattern is the only shape of environment-variable name this backend
// will turn into a --preserve pattern. An exotic key name is a caller bug we
// want surfaced immediately, not silently smuggled through — deliberately no
// regexp.QuoteMeta here; an invalid key is rejected, not escaped.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Guix is the Backend that runs provisioning steps in a native Guix
// container (design/unified-harness-pool.md §11.1): `guix shell --container
// --emulate-fhs --no-cwd`, network only for resolve, no host $HOME/workspace/
// pool `home/` in these containers — every mount is an explicit spec.Mounts
// entry, never an implicit one (see the --no-cwd comment in Run). guix has no
// separate manifest/channel concept at this layer, so pkgs (supplied via
// NewGuix) are the package operands placed before "--" — the same
// "packages before the command" idiom internal/harness's npmInstall uses
// (`guix shell node -- npm install ...`).
type Guix struct {
	pkgs   []string
	runner commandRunner
}

// NewGuix returns a Guix backend that runs spec.Argv with pkgs available in
// the container's profile.
func NewGuix(pkgs []string) *Guix {
	return &Guix{pkgs: pkgs, runner: execRunner{}}
}

// Name implements Backend.
func (g *Guix) Name() string { return "guix" }

// Available reports whether the guix CLI is on PATH.
func (g *Guix) Available() bool {
	_, err := lookPath("guix")
	return err == nil
}

// hostArch reports this host's architecture in the "amd64"/"arm64" form
// RunSpec.Platform uses — Go's runtime.GOARCH already reports those exact
// strings on those architectures, so no further mapping is needed.
func hostArch() string { return runtime.GOARCH }

// containsString reports whether s is present in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Run builds and executes a `guix shell --container --emulate-fhs --no-cwd`
// argv per spec. Argument order: container/no-cwd/network flags, then
// mounts, then --preserve patterns for spec.Env (sorted for a deterministic
// argv), then the package operands, then `--` and the (possibly
// env-prefixed, see Workdir below) command.
func (g *Guix) Run(ctx context.Context, spec RunSpec) error {
	if len(spec.Argv) == 0 {
		return fmt.Errorf("guix backend: empty Argv (a bare `guix shell ... --` with nothing after it would drop into an interactive shell and hang)")
	}

	// guix has no cross-arch execution mode at this layer (unlike the oci
	// backend's --platform, which asks the engine to emulate): a host/target
	// architecture mismatch is fatal instead of a silent no-op.
	if spec.Platform != "" && spec.Platform != hostArch() {
		return fmt.Errorf("guix backend: requested platform %q does not match host architecture %q; guix shell has no cross-arch execution mode here (use the oci backend for cross-arch provisioning)", spec.Platform, hostArch())
	}

	pkgs := g.pkgs
	if spec.Workdir != "" && !containsString(pkgs, "coreutils") {
		// Workdir is realized below via coreutils' `env --chdir=`; make sure
		// coreutils is actually in the container's profile. Copy first: pkgs
		// may be g.pkgs's own backing array, reused across Run calls with
		// different specs, and must not be mutated in place.
		pkgs = append(append([]string(nil), pkgs...), "coreutils")
	}

	args := []string{
		"shell", "--container", "--emulate-fhs",
		// Without --no-cwd, `guix shell --container` bind-mounts the invoking
		// process's current working directory into the container
		// *read-write* and starts the command there — an implicit mount this
		// backend never asked for and does not want (every mount must be an
		// explicit spec.Mounts entry). Always pass --no-cwd.
		"--no-cwd",
	}
	if spec.Network {
		args = append(args, "--network")
	}

	for _, m := range spec.Mounts {
		// The guix backend is Linux-host-only (Available() requires the guix
		// CLI on PATH, which only exists on Linux/Guix System hosts), so
		// unlike the oci backend's mount serialization, no path-separator
		// conversion is applied here — that asymmetry is deliberate, not a
		// bug to "fix".
		if m.ReadOnly {
			args = append(args, "--expose="+m.Host+"="+m.Container)
		} else {
			args = append(args, "--share="+m.Host+"="+m.Container)
		}
	}

	// guix shell only forwards environment variables that are (a) already
	// present in its own process environment and (b) named by a --preserve
	// pattern. spec.Env is passed straight through to the runner below as the
	// env delta (execRunner merges it onto the host's inherited environment),
	// and mirrored here as one anchored --preserve pattern per key so guix
	// actually carries each one into the container.
	for _, k := range sortedKeys(spec.Env) {
		if !envKeyPattern.MatchString(k) {
			return fmt.Errorf("guix backend: invalid env var name %q (want %s)", k, envKeyPattern.String())
		}
		args = append(args, "--preserve=^"+k+"$")
	}

	args = append(args, pkgs...)
	args = append(args, "--")

	// guix shell has no --workdir/-w flag, and Workdir is always a
	// CONTAINER-side path — never the host process's working directory
	// (conflating the two would silently chdir the host `guix` process
	// instead of the containerized command, and — combined with --no-cwd
	// above — would have no effect on the container at all). The no-shell
	// way to select it is to prefix the containerized command with
	// coreutils' `env --chdir=<dir>`, instead of going through
	// `sh -c "cd ... && exec ..."`.
	if spec.Workdir != "" {
		args = append(args, "env", "--chdir="+spec.Workdir)
	}
	args = append(args, spec.Argv...)

	// Host exec.Cmd.Dir stays unset: Workdir is realized entirely in argv
	// above, never via the host process's own working directory.
	return g.runner.Run(ctx, "guix", args, spec.Env, "")
}
