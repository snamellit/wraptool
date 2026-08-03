package pool

import (
	"runtime"
	"sort"
	"testing"
)

// allValidPlatformKeysForTest is the closed PlatformKey set, spelled out
// once here (deliberately independent of allPlatformKeys' own slice
// literal) so TestAllPlatformKeysExhaustive can catch the two lists
// drifting apart — e.g. a new PlatformKey constant added to ParsePlatformKey's
// switch (and this list) but forgotten in allPlatformKeys.
var allValidPlatformKeysForTest = []PlatformKey{
	PlatformLinuxAMD64Glibc,
	PlatformLinuxARM64Glibc,
	PlatformLinuxAMD64Musl,
	PlatformLinuxARM64Musl,
}

func TestParsePlatformKey(t *testing.T) {
	valid := allValidPlatformKeysForTest
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

// TestAllPlatformKeysExhaustive pins allPlatformKeys() against every
// ParsePlatformKey-accepted value (allValidPlatformKeysForTest, spelled out
// independently above): every value ParsePlatformKey accepts must appear in
// allPlatformKeys(), and vice versa — a new PlatformKey constant wired into
// one but not the other (e.g. a future platform added to ParsePlatformKey's
// switch and this test's own list, but forgotten in allPlatformKeys) fails
// this test rather than silently under- or over-counting in every caller
// that iterates allPlatformKeys() (StatusHarness's ActiveGenerations scan).
func TestAllPlatformKeysExhaustive(t *testing.T) {
	got := append([]PlatformKey(nil), allPlatformKeys()...)
	want := append([]PlatformKey(nil), allValidPlatformKeysForTest...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

	if len(got) != len(want) {
		t.Fatalf("allPlatformKeys() has %d entries, want %d matching the closed ParsePlatformKey set: got %v, want %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("allPlatformKeys()[%d] = %q, want %q (sorted comparison)", i, got[i], want[i])
		}
	}
	for _, k := range got {
		if _, err := ParsePlatformKey(string(k)); err != nil {
			t.Errorf("allPlatformKeys() contains %q, which ParsePlatformKey rejects: %v", k, err)
		}
	}
}
