package backend

import "fmt"

// Select resolves a --backend preference ("guix" | "oci" | "auto") to a
// concrete Backend. "auto" (the zero value "" behaves the same way) prefers
// guix when it's available, falls back to oci, and returns an error listing
// what was probed when neither is available. An explicit "guix" or "oci"
// choice is returned as-is without probing Available() itself, mirroring
// selectRuntime's style in cmd/guixcontainer.go: the explicit choice always
// wins, and it's up to the caller (or the first Run) to surface a "not
// installed" error for a backend the developer asked for by name.
//
// ociImage is threaded through to NewOCI when the oci backend is selected —
// see oci.go: there is no hardcoded default image, so a caller that wants the
// oci backend (explicitly or via auto-fallback) must supply one.
func Select(preference, ociImage string) (Backend, error) {
	switch preference {
	case "guix":
		return NewGuix(), nil
	case "oci":
		return NewOCI(ociImage), nil
	case "", "auto":
		guix := NewGuix()
		if guix.Available() {
			return guix, nil
		}
		oci := NewOCI(ociImage)
		if oci.Available() {
			return oci, nil
		}
		return nil, fmt.Errorf(
			"no provisioning backend available: guix not on PATH, and neither " +
				"docker nor podman found for the oci backend (install one, or pass " +
				"--backend=guix or --backend=oci once it is)")
	default:
		return nil, fmt.Errorf("unknown backend %q (want auto, guix, or oci)", preference)
	}
}
