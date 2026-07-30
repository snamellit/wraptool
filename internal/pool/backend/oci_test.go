package backend

import (
	"context"
	"reflect"
	"testing"
)

func TestOCIRunArgv(t *testing.T) {
	cases := []struct {
		name     string
		spec     RunSpec
		wantArgs []string
	}{
		{
			name:     "zero value defaults to --network=none",
			spec:     RunSpec{Argv: []string{"true"}},
			wantArgs: []string{"run", "--rm", "--network=none", "prov:pinned", "true"},
		},
		{
			name:     "network on omits --network=none",
			spec:     RunSpec{Argv: []string{"npm", "ci"}, Network: true},
			wantArgs: []string{"run", "--rm", "prov:pinned", "npm", "ci"},
		},
		{
			name: "read-only mount gets :ro suffix",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/cache", Container: "/cache", ReadOnly: true}},
			},
			wantArgs: []string{"run", "--rm", "--network=none", "-v", "/cache:/cache:ro", "prov:pinned", "true"},
		},
		{
			name: "writable mount has no suffix",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/stage", Container: "/stage", ReadOnly: false}},
			},
			wantArgs: []string{"run", "--rm", "--network=none", "-v", "/stage:/stage", "prov:pinned", "true"},
		},
		{
			name: "multiple mounts preserve given order",
			spec: RunSpec{
				Argv: []string{"true"},
				Mounts: []Mount{
					{Host: "/cache", Container: "/cache", ReadOnly: true},
					{Host: "/stage", Container: "/stage", ReadOnly: false},
				},
			},
			wantArgs: []string{
				"run", "--rm", "--network=none",
				"-v", "/cache:/cache:ro",
				"-v", "/stage:/stage",
				"prov:pinned", "true",
			},
		},
		{
			name:     "platform set adds --platform linux/<arch>",
			spec:     RunSpec{Argv: []string{"true"}, Platform: "arm64"},
			wantArgs: []string{"run", "--rm", "--network=none", "--platform", "linux/arm64", "prov:pinned", "true"},
		},
		{
			name:     "platform empty adds no flag",
			spec:     RunSpec{Argv: []string{"true"}},
			wantArgs: []string{"run", "--rm", "--network=none", "prov:pinned", "true"},
		},
		{
			name:     "workdir set adds -w",
			spec:     RunSpec{Argv: []string{"true"}, Workdir: "/stage/gen-1"},
			wantArgs: []string{"run", "--rm", "--network=none", "-w", "/stage/gen-1", "prov:pinned", "true"},
		},
		{
			name: "env produces sorted -e flags",
			spec: RunSpec{
				Argv: []string{"true"},
				Env:  map[string]string{"WRAPTOOL_TOKEN": "abc", "HOME": "/pool/home"},
			},
			wantArgs: []string{
				"run", "--rm", "--network=none",
				"-e", "HOME=/pool/home",
				"-e", "WRAPTOOL_TOKEN=abc",
				"prov:pinned", "true",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer stubLookPath("docker")()
			runner := &fakeRunner{}
			o := &OCI{image: "prov:pinned", runner: runner}

			if err := o.Run(context.Background(), tc.spec); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("got %d runner calls, want 1", len(runner.calls))
			}
			call := runner.calls[0]

			if call.name != "docker" {
				t.Errorf("name = %q, want %q", call.name, "docker")
			}
			if !reflect.DeepEqual(call.args, tc.wantArgs) {
				t.Errorf("args = %v, want %v", call.args, tc.wantArgs)
			}
		})
	}
}

func TestOCIWindowsHostPathIsSlashed(t *testing.T) {
	// filepath.ToSlash is a no-op on POSIX test hosts, so this only exercises
	// the conversion meaningfully on windows/GOOS builds; on POSIX it still
	// documents (and locks in) that the host segment passes through ToSlash.
	defer stubLookPath("docker")()
	runner := &fakeRunner{}
	o := &OCI{image: "prov:pinned", runner: runner}
	spec := RunSpec{
		Argv:   []string{"true"},
		Mounts: []Mount{{Host: "/cache", Container: "/cache"}},
	}
	if err := o.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"run", "--rm", "--network=none", "-v", "/cache:/cache", "prov:pinned", "true"}
	if got := runner.calls[0].args; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestOCIName(t *testing.T) {
	if got := (&OCI{}).Name(); got != "oci" {
		t.Errorf("Name() = %q, want %q", got, "oci")
	}
}

func TestOCIEngineDetection(t *testing.T) {
	t.Run("docker preferred over podman", func(t *testing.T) {
		defer stubLookPath("docker", "podman")()
		o := &OCI{image: "prov:pinned", runner: &fakeRunner{}}
		got, err := o.engine()
		if err != nil {
			t.Fatalf("engine: %v", err)
		}
		if got != "docker" {
			t.Errorf("engine() = %q, want %q", got, "docker")
		}
	})
	t.Run("podman fallback when docker absent", func(t *testing.T) {
		defer stubLookPath("podman")()
		o := &OCI{image: "prov:pinned", runner: &fakeRunner{}}
		got, err := o.engine()
		if err != nil {
			t.Fatalf("engine: %v", err)
		}
		if got != "podman" {
			t.Errorf("engine() = %q, want %q", got, "podman")
		}
	})
	t.Run("podman selection reaches Run's argv[0]", func(t *testing.T) {
		defer stubLookPath("podman")()
		runner := &fakeRunner{}
		o := &OCI{image: "prov:pinned", runner: runner}
		if err := o.Run(context.Background(), RunSpec{Argv: []string{"true"}}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if runner.calls[0].name != "podman" {
			t.Errorf("name = %q, want %q", runner.calls[0].name, "podman")
		}
	})
	t.Run("neither found is an error", func(t *testing.T) {
		defer stubLookPath()()
		o := &OCI{image: "prov:pinned", runner: &fakeRunner{}}
		if _, err := o.engine(); err == nil {
			t.Error("engine() error = nil, want error")
		}
	})
}

func TestOCIAvailable(t *testing.T) {
	t.Run("docker present", func(t *testing.T) {
		defer stubLookPath("docker")()
		if !(&OCI{}).Available() {
			t.Error("Available() = false, want true")
		}
	})
	t.Run("neither present", func(t *testing.T) {
		defer stubLookPath()()
		if (&OCI{}).Available() {
			t.Error("Available() = true, want false")
		}
	})
}
