package backend

import "testing"

func TestSelect(t *testing.T) {
	cases := []struct {
		name       string
		preference string
		available  []string // executables lookPath resolves
		wantName   string
		wantErr    bool
	}{
		{name: "explicit guix returns guix without probing", preference: "guix", available: nil, wantName: "guix"},
		{name: "explicit oci returns oci without probing", preference: "oci", available: nil, wantName: "oci"},
		{name: "auto prefers guix when available", preference: "auto", available: []string{"guix", "docker"}, wantName: "guix"},
		{name: "auto falls back to oci when guix unavailable", preference: "auto", available: []string{"docker"}, wantName: "oci"},
		{name: "auto falls back to oci via podman", preference: "auto", available: []string{"podman"}, wantName: "oci"},
		{name: "empty preference behaves like auto", preference: "", available: []string{"docker"}, wantName: "oci"},
		{name: "auto errors when nothing is available", preference: "auto", available: nil, wantErr: true},
		{name: "unknown preference errors", preference: "bogus", available: []string{"guix", "docker"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer stubLookPath(tc.available...)()

			b, err := Select(tc.preference, "prov:pinned")
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
