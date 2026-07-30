package backend

import (
	"context"
	"fmt"
	"path/filepath"
)

// ociEngines is the detection order: docker preferred, podman fallback
// (design/unified-harness-pool.md §11.2).
var ociEngines = []string{"docker", "podman"}

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
// no default image: the caller (which owns the provisioning-image pinning
// policy — out of scope here, see §11.2) must supply one.
func NewOCI(image string) *OCI {
	return &OCI{image: image, runner: execRunner{}}
}

// Name implements Backend.
func (o *OCI) Name() string { return "oci" }

// engine returns the first available container engine binary, docker
// preferred over podman.
func (o *OCI) engine() (string, error) {
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

// Run builds and executes a `docker|podman run --rm` argv per spec.
func (o *OCI) Run(ctx context.Context, spec RunSpec) error {
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

	for _, m := range spec.Mounts {
		// filepath.ToSlash is a no-op on POSIX hosts; it only matters when the
		// pool runs on a Windows host, converting a drive-letter source
		// (C:\Users\...) to the C:/Users/... form Docker Desktop expects in a
		// -v argument (§6.3). The container-side path is already a Linux path
		// and is left untouched.
		host := filepath.ToSlash(m.Host)
		v := host + ":" + m.Container
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}

	if spec.Platform != "" {
		args = append(args, "--platform", "linux/"+spec.Platform)
	}

	if spec.Workdir != "" {
		args = append(args, "-w", spec.Workdir)
	}

	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}

	args = append(args, o.image)
	args = append(args, spec.Argv...)

	return o.runner.Run(ctx, engine, args, nil, "")
}
