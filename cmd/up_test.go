package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbeAddr(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:8717":   "127.0.0.1:8717",
		"[::]:8717":      "127.0.0.1:8717",
		"::":             "::", // no port → returned as-is
		"127.0.0.1:8717": "127.0.0.1:8717",
		"192.168.1.5:80": "192.168.1.5:80",
	}
	for in, want := range cases {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~":            home,
		"~/x/y":        filepath.Join(home, "x", "y"),
		"/abs/path":    "/abs/path",
		"":             "",
		"rel/~notilde": "rel/~notilde",
	}
	for in, want := range cases {
		if got := expandTilde(in); got != want {
			t.Errorf("expandTilde(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWantShell(t *testing.T) {
	if !wantShell("always") {
		t.Error(`wantShell("always") = false, want true`)
	}
	if wantShell("never") {
		t.Error(`wantShell("never") = true, want false`)
	}
	// "auto" depends on the terminal; under `go test` stdin is not a TTY.
	if wantShell("auto") {
		t.Error(`wantShell("auto") = true with non-TTY stdin, want false`)
	}
}

func TestHasDevcontainer(t *testing.T) {
	dir := t.TempDir()
	if hasDevcontainer(dir) {
		t.Fatal("empty dir should have no devcontainer")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if !hasDevcontainer(dir) {
		t.Fatal("should detect .devcontainer/devcontainer.json")
	}
}
