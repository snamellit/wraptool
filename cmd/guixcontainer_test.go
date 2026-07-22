package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelectRuntime(t *testing.T) {
	origHasGuix := hasGuix
	t.Cleanup(func() { hasGuix = origHasGuix })

	// root with manifest.scm present; a separate one without.
	withManifest := t.TempDir()
	if err := os.WriteFile(filepath.Join(withManifest, "manifest.scm"), []byte("()"), 0o600); err != nil {
		t.Fatal(err)
	}
	noManifest := t.TempDir()

	cases := []struct {
		name    string
		mode    string
		root    string
		guix    bool
		want    containerRuntime
		wantErr bool
	}{
		{"explicit guix", "guix", noManifest, false, runtimeGuix, false},
		{"explicit devcontainer", "devcontainer", withManifest, true, runtimeDevcontainer, false},
		{"unknown mode errors", "podman", withManifest, true, 0, true},
		{"auto: guix + manifest -> guix", "auto", withManifest, true, runtimeGuix, false},
		{"auto: guix + no manifest -> devcontainer", "auto", noManifest, true, runtimeDevcontainer, false},
		{"auto: no guix + manifest -> devcontainer", "auto", withManifest, false, runtimeDevcontainer, false},
		{"empty is auto", "", withManifest, true, runtimeGuix, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hasGuix = func() bool { return tc.guix }
			got, err := selectRuntime(tc.mode, tc.root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got runtime %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("runtime = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateGuixConfigOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(inside, []byte("mcp: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateGuixConfigOutsideWorkspace(inside, root); err == nil {
		t.Fatal("config inside workspace was accepted")
	}

	outside := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(outside, []byte("mcp: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateGuixConfigOutsideWorkspace(outside, root); err != nil {
		t.Fatalf("config outside workspace rejected: %v", err)
	}

	alias := filepath.Join(t.TempDir(), "config-link.yaml")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateGuixConfigOutsideWorkspace(alias, root); err == nil {
		t.Fatal("symlinked config inside workspace was accepted")
	}
}
