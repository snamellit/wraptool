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

func TestValidateConfigOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(inside, []byte("mcp: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigOutsideWorkspace(inside, root); err == nil {
		t.Fatal("config inside workspace was accepted")
	}

	outside := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(outside, []byte("mcp: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigOutsideWorkspace(outside, root); err != nil {
		t.Fatalf("config outside workspace rejected: %v", err)
	}

	alias := filepath.Join(t.TempDir(), "config-link.yaml")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigOutsideWorkspace(alias, root); err == nil {
		t.Fatal("symlinked config inside workspace was accepted")
	}
}

func TestValidateAdminTokenOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "admin-token")
	if err := os.WriteFile(inside, []byte("token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(inside, "admin auth token", root); err == nil {
		t.Fatal("admin token inside workspace was accepted")
	}

	alias := filepath.Join(t.TempDir(), "admin-token-link")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(alias, "admin auth token", root); err == nil {
		t.Fatal("symlinked admin token inside workspace was accepted")
	}

	missing := filepath.Join(root, "new-admin-token")
	if err := validatePathOutsideWorkspace(missing, "admin auth token", root); err == nil {
		t.Fatal("missing admin token inside workspace was accepted")
	}
	danglingAlias := filepath.Join(t.TempDir(), "new-admin-token-link")
	if err := os.Symlink(missing, danglingAlias); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(danglingAlias, "admin auth token", root); err == nil {
		t.Fatal("dangling symlink to admin token inside workspace was accepted")
	}

	outside := t.TempDir()
	existing := filepath.Join(outside, "admin-token")
	if err := os.WriteFile(existing, []byte("token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		existing,
		filepath.Join(outside, "missing-admin-token"),
	} {
		if err := validatePathOutsideWorkspace(path, "admin auth token", root); err != nil {
			t.Fatalf("admin token outside workspace rejected: %v", err)
		}
	}

	outsideAlias := filepath.Join(t.TempDir(), "admin-token-link")
	if err := os.Symlink(existing, outsideAlias); err != nil {
		t.Fatal(err)
	}
	if err := validatePathOutsideWorkspace(outsideAlias, "admin auth token", root); err != nil {
		t.Fatalf("symlinked admin token outside workspace rejected: %v", err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := validatePathOutsideWorkspace("~/.config/wraptool/admin-token", "admin auth token", root); err != nil {
		t.Fatalf("tilde-expanded admin token outside workspace rejected: %v", err)
	}
}
