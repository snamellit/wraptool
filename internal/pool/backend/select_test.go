package backend

import (
	"strings"
	"testing"
)

func TestSelect(t *testing.T) {
	cases := []struct {
		name       string
		preference string
		opts       Options
		available  []string // executables lookPath resolves
		wantName   string
		wantErr    bool
	}{
		{
			name: "explicit guix returns guix without probing", preference: "guix",
			opts: Options{GuixPackages: []string{"node"}}, available: nil, wantName: "guix",
		},
		{
			name: "explicit guix without packages errors", preference: "guix",
			opts: Options{}, available: []string{"guix"}, wantErr: true,
		},
		{
			name: "explicit oci returns oci without probing", preference: "oci",
			opts: Options{OCIImage: "prov:pinned"}, available: nil, wantName: "oci",
		},
		{
			name: "explicit oci without image errors", preference: "oci",
			opts: Options{}, available: []string{"docker"}, wantErr: true,
		},
		{
			name: "auto prefers guix when available", preference: "auto",
			opts:      Options{GuixPackages: []string{"node"}, OCIImage: "prov:pinned"},
			available: []string{"guix", "docker"}, wantName: "guix",
		},
		{
			name:       "auto resolves to guix but its packages are missing: errors, does not fall back to oci",
			preference: "auto", opts: Options{OCIImage: "prov:pinned"},
			available: []string{"guix", "docker"}, wantErr: true,
		},
		{
			name: "auto falls back to oci when guix unavailable", preference: "auto",
			opts: Options{OCIImage: "prov:pinned"}, available: []string{"docker"}, wantName: "oci",
		},
		{
			name: "auto falls back to oci via podman", preference: "auto",
			opts: Options{OCIImage: "prov:pinned"}, available: []string{"podman"}, wantName: "oci",
		},
		{
			name: "auto resolves to oci but its image is missing", preference: "auto",
			opts: Options{}, available: []string{"docker"}, wantErr: true,
		},
		{
			name: "empty preference behaves like auto", preference: "",
			opts: Options{OCIImage: "prov:pinned"}, available: []string{"docker"}, wantName: "oci",
		},
		{
			name: "auto errors when nothing is available", preference: "auto",
			opts:      Options{GuixPackages: []string{"node"}, OCIImage: "prov:pinned"},
			available: nil, wantErr: true,
		},
		{
			name: "unknown preference errors", preference: "bogus",
			opts:      Options{GuixPackages: []string{"node"}, OCIImage: "prov:pinned"},
			available: []string{"guix", "docker"}, wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer stubLookPath(tc.available...)()

			b, err := Select(tc.preference, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Select: error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if got := b.Name(); got != tc.wantName {
				t.Errorf("Name() = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestSelectGuixMissingPackagesMessage locks in the operator-actionable
// wording of the missing-GuixPackages error: no Go field names (an operator
// reading this didn't write the Go that called Select), and a concrete
// workaround.
func TestSelectGuixMissingPackagesMessage(t *testing.T) {
	defer stubLookPath()()
	_, err := Select("guix", Options{})
	if err == nil {
		t.Fatal("Select: error = nil, want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "GuixPackages") || strings.Contains(msg, "Options.") {
		t.Errorf("error message leaks a Go field name: %v", err)
	}
	if !strings.Contains(msg, "--backend=oci") {
		t.Errorf("error message should suggest a workaround: %v", err)
	}
}
