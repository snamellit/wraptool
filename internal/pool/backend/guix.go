package backend

import "context"

// Guix is the Backend that runs provisioning steps in a native Guix
// container (design/unified-harness-pool.md §11.1): `guix shell --container
// --emulate-fhs`, network only for resolve, no host $HOME/workspace/pool
// `home/` in these containers (the caller controls that entirely through
// spec.Mounts — this backend never adds a mount on its own).
type Guix struct {
	runner commandRunner
}

// NewGuix returns a Guix backend wired to the production command runner.
func NewGuix() *Guix {
	return &Guix{runner: execRunner{}}
}

// Name implements Backend.
func (g *Guix) Name() string { return "guix" }

// Available reports whether the guix CLI is on PATH.
func (g *Guix) Available() bool {
	_, err := lookPath("guix")
	return err == nil
}

// Run builds and executes a `guix shell --container --emulate-fhs` argv per
// spec. Argument order: container/network/fhs flags, then mounts, then
// --preserve patterns for spec.Env (sorted for a deterministic argv), then
// `--` and spec.Argv.
func (g *Guix) Run(ctx context.Context, spec RunSpec) error {
	args := []string{"shell", "--container", "--emulate-fhs"}
	if spec.Network {
		args = append(args, "--network")
	}

	for _, m := range spec.Mounts {
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
		args = append(args, "--preserve=^"+k+"$")
	}

	// guix shell has no --workdir/-w flag. Per the Guix manual, `--container`
	// bind-mounts the invoking process's current working directory into the
	// container at the same path (read-write) and starts the command there,
	// unless --no-cwd is given. That is the only way to select the
	// container-side working directory without going through a shell `cd` —
	// so instead of `sh -c "cd ... && exec ..."`, we set the *host* exec.Cmd's
	// working directory to spec.Workdir and rely on guix's default cwd
	// sharing. This only lands the command in the right place when Workdir is
	// also visible inside the container (typically a Mount's Container path,
	// or a subdirectory of one) — the same host==container path idiom already
	// used throughout cmd/guixcontainer.go's --share/--expose calls.
	dir := spec.Workdir

	args = append(args, "--")
	args = append(args, spec.Argv...)

	return g.runner.Run(ctx, "guix", args, spec.Env, dir)
}
