package backend

import "fmt"

// Options carries the backend-specific inputs Select needs to construct a
// fully usable Backend. Exactly one of these is required, depending on which
// backend Select resolves to (including via the "auto" fallback) — Select
// validates that field and errors clearly naming what's missing, rather than
// returning a Backend that would only fail later, deep inside Run.
type Options struct {
	// OCIImage is the provisioning image reference for the oci backend (no
	// default — see NewOCI).
	OCIImage string
	// GuixPackages is the package list `guix shell` installs before running
	// spec.Argv (guix has no separate manifest/channel concept at this
	// layer — see NewGuix).
	GuixPackages []string
}

// Select resolves a --backend preference ("guix" | "oci" | "auto") to a
// concrete Backend. "auto" (the zero value "" behaves the same way) prefers
// guix when it's available, falls back to oci, and returns an error listing
// what was probed when neither is available. An explicit "guix" or "oci"
// choice is returned as-is without probing Available() itself, mirroring
// selectRuntime's style in cmd/guixcontainer.go: the explicit choice always
// wins.
//
// Whichever backend Select resolves to — explicitly, or via the auto
// fallback's availability probe — its required Options field must be
// non-empty; Select errors immediately naming the missing option instead of
// deferring to a confusing failure inside Run.
func Select(preference string, opts Options) (Backend, error) {
	switch preference {
	case "guix":
		if err := requireGuixPackages(opts); err != nil {
			return nil, err
		}
		return NewGuix(opts.GuixPackages), nil
	case "oci":
		if err := requireOCIImage(opts); err != nil {
			return nil, err
		}
		return newOCIBackend(opts.OCIImage)
	case "", "auto":
		guix := NewGuix(opts.GuixPackages)
		if guix.Available() {
			if err := requireGuixPackages(opts); err != nil {
				return nil, err
			}
			return guix, nil
		}
		if _, err := detectOCIEngine(); err != nil {
			return nil, fmt.Errorf(
				"no provisioning backend available: guix not on PATH, and neither " +
					"docker nor podman found for the oci backend (install one, or pass " +
					"--backend=guix or --backend=oci once it is)")
		}
		if err := requireOCIImage(opts); err != nil {
			return nil, err
		}
		return newOCIBackend(opts.OCIImage)
	default:
		return nil, fmt.Errorf("unknown backend %q (want auto, guix, or oci)", preference)
	}
}

// newOCIBackend wraps NewOCI so a construction error never ships as a
// non-nil Backend interface value wrapping a typed-nil *OCI (the classic
// "interface holding a nil pointer is not a nil interface" pitfall — a bare
// `return NewOCI(image)` here would pass that non-nil-but-broken Backend
// straight to the caller alongside the error).
func newOCIBackend(image string) (Backend, error) {
	o, err := NewOCI(image)
	if err != nil {
		return nil, err
	}
	return o, nil
}

func requireGuixPackages(opts Options) error {
	if len(opts.GuixPackages) == 0 {
		return fmt.Errorf("guix backend selected but no package set was configured for the " +
			"provisioning container — this is a wraptool bug; --backend=oci may work around it")
	}
	return nil
}

func requireOCIImage(opts Options) error {
	if opts.OCIImage == "" {
		return fmt.Errorf("oci backend selected but no provisioning image was configured — " +
			"this is a wraptool bug; --backend=guix may work around it")
	}
	return nil
}
