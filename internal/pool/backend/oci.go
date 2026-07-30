package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ociEngines is the detection order: docker preferred, podman fallback
// (design/unified-harness-pool.md §11.2).
var ociEngines = []string{"docker", "podman"}

// toSlash converts a host path to the forward-slash form Docker Desktop
// expects in a bind-mount source (§6.3: on Windows hosts this turns
// C:\Users\dev\... into C:/Users/dev/...; a no-op on POSIX). A package-level
// var — like lookPath — so a test can stub a Windows-shaped conversion on a
// POSIX test host and prove the mount serializer actually calls it.
var toSlash = filepath.ToSlash

// OCI is the Backend that runs provisioning steps in a pinned provisioning
// OCI image through Docker or Podman (design/unified-harness-pool.md §11.2) —
// the non-Guix-host path (Arch, macOS, Windows). It never mounts a Docker
// socket, host home, or project workspace; the caller controls all of that
// through spec.Mounts.
type OCI struct {
	// image is the provisioning image reference (and, per §11.2, its digest
	// is a lock input at a higher layer — this package just runs it).
	image  string
	runner commandRunner
}

// NewOCI returns an OCI backend that runs the given image reference. There is
// no default image — image pinning policy lives above this package (§11.2) —
// so an empty image is a caller bug, reported immediately here rather than
// deferred to a confusing failure the first time Run executes.
func NewOCI(image string) (*OCI, error) {
	if image == "" {
		return nil, fmt.Errorf("oci backend: image reference is required (no default provisioning image)")
	}
	return &OCI{image: image, runner: execRunner{}}, nil
}

// Name implements Backend.
func (o *OCI) Name() string { return "oci" }

// engine returns the first available container engine binary, docker
// preferred over podman.
func (o *OCI) engine() (string, error) { return detectOCIEngine() }

// detectOCIEngine probes for a usable container engine independent of any
// particular OCI instance. Select's auto-fallback needs to know whether OCI
// tooling exists at all before it necessarily has (or requires) a
// provisioning image, so this is a free function rather than a method that
// would force constructing an OCI (which, per NewOCI above, requires one).
func detectOCIEngine() (string, error) {
	for _, candidate := range ociEngines {
		if _, err := lookPath(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("oci backend: neither docker nor podman found on PATH")
}

// Available reports whether docker or podman is on PATH.
func (o *OCI) Available() bool {
	_, err := o.engine()
	return err == nil
}

// ociUser resolves the --user value for a run, or reports that none should
// be passed. Rootful Docker on Linux runs containers as root by default, so
// files it writes into the bind-mounted staging directory come out
// root-owned — host-side Go (running as the developer) can then never rename
// or clean them up, which is exactly the directory realize's atomic-rename
// step depends on being able to touch. Pinning --user to the invoking
// developer's uid:gid fixes that. Rootless podman's in-container root
// already maps to the host user on bind mounts (that's the point of
// rootless podman), so passing --user there would fight that mapping instead
// of helping it — always pass nothing for podman. Docker Desktop
// (darwin/windows) mediates file ownership inside its VM regardless, so
// neither engine needs a --user override there. An explicit spec.User always
// wins over all of the above and is passed through unchanged.
func ociUser(spec RunSpec, engine string) (string, bool) {
	if spec.User != "" {
		return spec.User, true
	}
	if engine == "docker" && runtime.GOOS == "linux" {
		return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), true
	}
	return "", false
}

// Run builds and executes a `docker|podman run --rm` argv per spec.
func (o *OCI) Run(ctx context.Context, spec RunSpec) error {
	if len(spec.Argv) == 0 {
		return fmt.Errorf("oci backend: empty Argv (a container started with no command would hang)")
	}

	engine, err := o.engine()
	if err != nil {
		return err
	}

	args := []string{"run", "--rm"}

	// Network off is the default posture: unless the caller opts in, disable
	// it explicitly (docker/podman default to a networked bridge).
	if !spec.Network {
		args = append(args, "--network=none")
	}

	if u, ok := ociUser(spec, engine); ok {
		args = append(args, "--user", u)
	}

	for _, m := range spec.Mounts {
		// `--mount type=bind,...` rather than `-v host:container[:ro]`:
		// -v's colon-delimited syntax breaks on a (POSIX-legal) host or
		// container path containing ':'; --mount's key=value form has no
		// such ambiguity. toSlash only matters when the pool runs on a
		// Windows host (§6.3); the container-side path is already a Linux
		// path and is left untouched.
		host := toSlash(m.Host)
		mount := "type=bind,src=" + host + ",dst=" + m.Container
		if m.ReadOnly {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}

	if spec.Platform != "" {
		args = append(args, "--platform", "linux/"+spec.Platform)
	}

	if spec.Workdir != "" {
		// OCI engines have a native workdir flag, unlike guix — no argv
		// trickery needed here.
		args = append(args, "-w", spec.Workdir)
	}

	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}

	args = append(args, o.image)
	args = append(args, spec.Argv...)

	// Host exec.Cmd.Dir stays unset: Workdir above is realized entirely via
	// the engine's own -w flag, never via the host process's cwd.
	return o.runner.Run(ctx, engine, args, nil, "")
}
