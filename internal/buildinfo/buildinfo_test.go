package buildinfo

import (
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	i := Get()
	// The stamped version always starts with the base version; the VCS suffix is
	// present in a real build but may be absent under `go test`.
	if !strings.HasPrefix(i.Version, BaseVersion) {
		t.Errorf("Version %q must start with base version %q", i.Version, BaseVersion)
	}
	if i.GoVersion == "" {
		t.Error("GoVersion is empty")
	}
	if i.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	// When a revision is stamped, the version encodes its short form.
	if i.Revision != "" && !strings.Contains(i.Version, i.Revision[:min(12, len(i.Revision))]) {
		t.Errorf("Version %q should encode revision %q", i.Version, i.Revision)
	}
}

func TestVersion(t *testing.T) {
	if got, want := Version(), Get().Version; got != want {
		t.Errorf("Version() = %q, Get().Version = %q", got, want)
	}
}
