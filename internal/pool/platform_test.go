package pool

import (
	"runtime"
	"testing"
)

func TestParsePlatformKey(t *testing.T) {
	valid := []PlatformKey{
		PlatformLinuxAMD64Glibc,
		PlatformLinuxARM64Glibc,
		PlatformLinuxAMD64Musl,
		PlatformLinuxARM64Musl,
	}
	for _, k := range valid {
		got, err := ParsePlatformKey(string(k))
		if err != nil {
			t.Errorf("ParsePlatformKey(%q): unexpected error: %v", k, err)
		}
		if got != k {
			t.Errorf("ParsePlatformKey(%q) = %q, want %q", k, got, k)
		}
	}
}

func TestParsePlatformKeyInvalid(t *testing.T) {
	for _, s := range []string{"", "darwin-arm64", "linux-riscv64-glibc", "linux-amd64", "LINUX-AMD64-GLIBC"} {
		if _, err := ParsePlatformKey(s); err == nil {
			t.Errorf("ParsePlatformKey(%q): expected error, got nil", s)
		}
	}
}

func TestHostPlatformKey(t *testing.T) {
	k, err := HostPlatformKey()
	switch runtime.GOARCH {
	case "amd64", "arm64":
		if err != nil {
			t.Fatalf("HostPlatformKey(): unexpected error: %v", err)
		}
		if _, perr := ParsePlatformKey(string(k)); perr != nil {
			t.Errorf("HostPlatformKey() returned invalid key %q: %v", k, perr)
		}
		// Guix runtime is always glibc (§5.1), regardless of host arch.
		if k != PlatformLinuxAMD64Glibc && k != PlatformLinuxARM64Glibc {
			t.Errorf("HostPlatformKey() = %q, want a glibc key", k)
		}
	default:
		if err == nil {
			t.Fatalf("HostPlatformKey(): expected error on unsupported arch %q, got %q", runtime.GOARCH, k)
		}
	}
}
