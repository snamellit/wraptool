package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ociEngines is the detection order: docker preferred, podman fallback
// (design/harness/unified-harness-pool.md §11.2).
var ociEngines = []string{"docker", "podman"}

// toSlash converts a host path to the forward-slash form Docker Desktop
// expects in a bind-mount source (§6.3: on Windows hosts this turns
// C:\Users\dev\... into C:/Users/dev/...; a no-op on POSIX). A package-level
// var — like lookPath — so a test can stub a Windows-shaped conversion on a
// POSIX test host and prove the mount serializer actually calls it.
var toSlash = filepath.ToSlash

// hostGOOS is the host OS ociUser branches on. A package-level var — like
// lookPath and toSlash — so tests can exercise the darwin/windows branches of
// ociUser's engine matrix without needing to actually run on those hosts.
var hostGOOS = runtime.GOOS

// dockerSecurityOptions runs `docker info -f {{.SecurityOptions}}` to learn
// whether the local docker daemon is rootless (its output contains
// "name=rootless" when it is). A package-level var — like lookPath and
// toSlash — so tests can stub the daemon's answer without a real docker
// installation.
var dockerSecurityOptions = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "info", "-f", "{{.SecurityOptions}}").Output()
	return string(out), err
}

// OCI is the Backend that runs provisioning steps in a pinned provisioning
// OCI image through Docker or Podman (design/harness/unified-harness-pool.md §11.2) —
// the non-Guix-host path (Arch, macOS, Windows). It never mounts a Docker
// socket, host home, or project workspace; the caller controls all of that
// through spec.Mounts.
type OCI struct {
	// image is the provisioning image reference (and, per §11.2, its digest
	// is a lock input at a higher layer — this package just runs it).
	image  string
	runner commandRunner

	// rootlessOnce/rootlessCached memoize the docker-rootless probe: the
	// answer describes the daemon, not any particular Run, so it's queried
	// at most once per OCI instance.
	rootlessOnce   sync.Once
	rootlessCached bool
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

// isDockerRootless reports whether the local docker daemon is running
// rootless, probing (and caching) at most once per OCI instance. On a probe
// failure it reports false: the failure mode of guessing wrong is different
// in each direction — treating a rootless daemon as rootful merely adds a
// redundant --user flag docker will honor anyway (rootless docker still maps
// that uid to itself), while treating a rootful daemon as rootless leaves
// staging writable only by root. Probe failure is also rare (it means
// `docker info` itself failed), so defaulting to the common, safer, rootful
// case is the right trade-off.
func (o *OCI) isDockerRootless(ctx context.Context) bool {
	o.rootlessOnce.Do(func() {
		out, err := dockerSecurityOptions(ctx)
		if err != nil {
			o.rootlessCached = false
			return
		}
		o.rootlessCached = strings.Contains(out, "name=rootless")
	})
	return o.rootlessCached
}

// ociUser resolves the --user value for a run, or reports that none should
// be passed. The full matrix:
//
//   - spec.User set                    -> always passed through unchanged.
//     This is the escape hatch: e.g. an explicit User:"0:0" runs as root
//     in-container even against the rootful-docker default below, should a
//     caller ever need that.
//   - docker, linux, rootful daemon    -> --user <host uid>:<host gid>.
//     Rootful Docker runs containers as root by default, so files it writes
//     into the bind-mounted staging directory come out root-owned —
//     host-side Go (running as the developer) can then never rename or
//     clean them up. Pinning --user fixes that.
//   - docker, linux, ROOTLESS daemon   -> no --user. Rootless docker's
//     in-container root already maps to the host uid on bind mounts, the
//     same as rootless podman below — passing --user would fight that
//     mapping instead of helping it.
//   - docker, linux, probe failed      -> --user <host uid>:<host gid>
//     (see isDockerRootless: defaulting to the common rootful case is safer
//     than silently guessing rootless).
//   - docker, darwin/windows           -> no --user. Docker Desktop mediates
//     file ownership inside its VM regardless of daemon mode.
//   - podman (any host)                -> no --user. Rootless podman's
//     in-container root already maps to the host user on bind mounts —
//     that's the entire point of rootless podman; --user would break it.
func (o *OCI) ociUser(ctx context.Context, spec RunSpec, engine string) (string, bool) {
	if spec.User != "" {
		return spec.User, true
	}
	if engine == "docker" && hostGOOS == "linux" {
		if o.isDockerRootless(ctx) {
			return "", false
		}
		return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), true
	}
	return "", false
}

// Run builds and executes a `docker|podman run --rm` argv per spec.
func (o *OCI) Run(ctx context.Context, spec RunSpec) error {
	if len(spec.Argv) == 0 {
		return fmt.Errorf("oci backend: empty Argv (a container started with no command would hang)")
	}

	if err := validateEnvKeys(spec.Env); err != nil {
		return fmt.Errorf("oci backend: %w", err)
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

	if u, ok := o.ociUser(ctx, spec, engine); ok {
		args = append(args, "--user", u)
	}

	for _, m := range spec.Mounts {
		if err := validateMount(m); err != nil {
			return fmt.Errorf("oci backend: %w", err)
		}
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
