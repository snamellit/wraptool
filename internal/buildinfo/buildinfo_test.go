package buildinfo

import (
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	i := Get()
	if !strings.HasPrefix(i.Version, "devel") {
		t.Errorf("Version %q must identify a development build", i.Version)
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

func TestGetInjectedReleaseVersion(t *testing.T) {
	oldVersion := version
	version = "1.2.3"
	t.Cleanup(func() { version = oldVersion })

	if got := Get().Version; got != "1.2.3" {
		t.Errorf("Version = %q, want linker-injected release version", got)
	}
}

func TestVersion(t *testing.T) {
	if got, want := Version(), Get().Version; got != want {
		t.Errorf("Version() = %q, Get().Version = %q", got, want)
	}
}
