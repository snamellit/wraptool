package pool

import (
	"fmt"
	"runtime"
)

// PlatformKey identifies the target platform a harness generation is built
// for. Generations always target a Linux container regardless of host OS
// (§5.1, §6.1) — there are no darwin-* or windows-* keys. This is a closed,
// exhaustive-lint-enforced enum (design/policy/flag-constraint-type-single-source.md
// convention): a switch over PlatformKey must name every member.
type PlatformKey string

const (
	PlatformLinuxAMD64Glibc PlatformKey = "linux-amd64-glibc"
	PlatformLinuxARM64Glibc PlatformKey = "linux-arm64-glibc"
	PlatformLinuxAMD64Musl  PlatformKey = "linux-amd64-musl"
	PlatformLinuxARM64Musl  PlatformKey = "linux-arm64-musl"
)

// ParsePlatformKey validates s against the closed PlatformKey set, so a
// mistyped or unsupported key (e.g. from --platform or WRAPTOOL_PLATFORM)
// fails loudly instead of silently producing an unrealizable key.
func ParsePlatformKey(s string) (PlatformKey, error) {
	switch k := PlatformKey(s); k {
	case PlatformLinuxAMD64Glibc, PlatformLinuxARM64Glibc, PlatformLinuxAMD64Musl, PlatformLinuxARM64Musl:
		return k, nil
	default:
		return "", fmt.Errorf("unknown platform key %q (want one of linux-amd64-glibc, linux-arm64-glibc, linux-amd64-musl, linux-arm64-musl)", s)
	}
}

// allPlatformKeys lists the closed PlatformKey enum's values, for a caller
// that needs to probe every platform rather than switch over one (e.g.
// StatusHarness's ActiveGenerations scan, orchestrator.go) — kept next to
// this file's own exhaustive-lint-enforced switches (ParsePlatformKey,
// HostPlatformKey) rather than duplicated at each call site.
func allPlatformKeys() []PlatformKey {
	return []PlatformKey{PlatformLinuxAMD64Glibc, PlatformLinuxARM64Glibc, PlatformLinuxAMD64Musl, PlatformLinuxARM64Musl}
}

// HostPlatformKey returns the guix-runtime platform key for the current host:
// arch from runtime.GOARCH, libc always glibc — the Guix container always
// runs with --emulate-fhs (§5.1 "Guix runtime: arch = host arch ... libc =
// glibc (always ...)"). It is unrelated to Dev Container target selection,
// which instead uses the Docker daemon's platform (§5.1) — not implemented in
// this package.
func HostPlatformKey() (PlatformKey, error) {
	switch runtime.GOARCH {
	case "amd64":
		return PlatformLinuxAMD64Glibc, nil
	case "arm64":
		return PlatformLinuxARM64Glibc, nil
	default:
		return "", fmt.Errorf("unsupported host architecture %q for the guix runtime platform key", runtime.GOARCH)
	}
}
