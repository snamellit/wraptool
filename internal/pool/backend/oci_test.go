package backend

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
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
			name: "read-only mount gets ,readonly",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/cache", Container: "/cache", ReadOnly: true}},
			},
			wantArgs: []string{"run", "--rm", "--network=none", "--mount", "type=bind,src=/cache,dst=/cache,readonly", "prov:pinned", "true"},
		},
		{
			name: "writable mount has no readonly suffix",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/stage", Container: "/stage", ReadOnly: false}},
			},
			wantArgs: []string{"run", "--rm", "--network=none", "--mount", "type=bind,src=/stage,dst=/stage", "prov:pinned", "true"},
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
				"--mount", "type=bind,src=/cache,dst=/cache,readonly",
				"--mount", "type=bind,src=/stage,dst=/stage",
				"prov:pinned", "true",
			},
		},
		{
			name:     "mount host path containing ':' survives (the reason for --mount over -v)",
			spec:     RunSpec{Argv: []string{"true"}, Mounts: []Mount{{Host: "/cache/weird:name", Container: "/cache"}}},
			wantArgs: []string{"run", "--rm", "--network=none", "--mount", "type=bind,src=/cache/weird:name,dst=/cache", "prov:pinned", "true"},
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
				Env:  map[string]string{"WRAPTOOL_TOKEN": "abc", "npm_config_cache": "/cache"},
			},
			wantArgs: []string{
				"run", "--rm", "--network=none",
				"-e", "WRAPTOOL_TOKEN=abc",
				"-e", "npm_config_cache=/cache",
				"prov:pinned", "true",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// podman (not docker) so ociUser never injects a --user flag here
			// — that behavior gets its own dedicated golden table below.
			defer stubLookPath("podman")()
			runner := &fakeRunner{}
			o := &OCI{image: "prov:pinned", runner: runner}

			if err := o.Run(context.Background(), tc.spec); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("got %d runner calls, want 1", len(runner.calls))
			}
			call := runner.calls[0]

			if call.name != "podman" {
				t.Errorf("name = %q, want %q", call.name, "podman")
			}
			if !reflect.DeepEqual(call.args, tc.wantArgs) {
				t.Errorf("args = %v, want %v", call.args, tc.wantArgs)
			}
		})
	}
}

func TestOCIEmptyArgvRejected(t *testing.T) {
	defer stubLookPath("docker")()
	runner := &fakeRunner{}
	o := &OCI{image: "prov:pinned", runner: runner}
	if err := o.Run(context.Background(), RunSpec{}); err == nil {
		t.Fatal("Run: error = nil, want error")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("got %d runner calls, want 0", len(runner.calls))
	}
}

func TestOCIUser(t *testing.T) {
	t.Run("docker on linux defaults to host uid:gid", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("this backend's docker+linux default only applies on a linux host")
		}
		defer stubLookPath("docker")()
		runner := &fakeRunner{}
		o := &OCI{image: "prov:pinned", runner: runner}
		if err := o.Run(context.Background(), RunSpec{Argv: []string{"true"}}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := []string{"run", "--rm", "--network=none", "--user",
			fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "prov:pinned", "true"}
		if got := runner.calls[0].args; !reflect.DeepEqual(got, want) {
			t.Errorf("args = %v, want %v", got, want)
		}
	})

	t.Run("podman never gets a --user flag", func(t *testing.T) {
		defer stubLookPath("podman")()
		runner := &fakeRunner{}
		o := &OCI{image: "prov:pinned", runner: runner}
		if err := o.Run(context.Background(), RunSpec{Argv: []string{"true"}}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := []string{"run", "--rm", "--network=none", "prov:pinned", "true"}
		if got := runner.calls[0].args; !reflect.DeepEqual(got, want) {
			t.Errorf("args = %v, want %v (podman's rootless uid mapping must not be overridden)", got, want)
		}
	})

	t.Run("explicit User always passes through", func(t *testing.T) {
		defer stubLookPath("podman")()
		runner := &fakeRunner{}
		o := &OCI{image: "prov:pinned", runner: runner}
		spec := RunSpec{Argv: []string{"true"}, User: "1000:1000"}
		if err := o.Run(context.Background(), spec); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := []string{"run", "--rm", "--network=none", "--user", "1000:1000", "prov:pinned", "true"}
		if got := runner.calls[0].args; !reflect.DeepEqual(got, want) {
			t.Errorf("args = %v, want %v", got, want)
		}
	})
}

func TestOCIWindowsHostPathIsConverted(t *testing.T) {
	// filepath.ToSlash is a no-op on this (POSIX) test host, which would
	// leave the conversion untested. Stub toSlash with a Windows-shaped
	// backslash conversion instead, to prove the mount serializer actually
	// calls it (rather than, say, using filepath.ToSlash directly and
	// silently bypassing the seam) — see §17.
	origToSlash := toSlash
	toSlash = func(p string) string { return strings.ReplaceAll(p, `\`, "/") }
	defer func() { toSlash = origToSlash }()

	defer stubLookPath("podman")()
	runner := &fakeRunner{}
	o := &OCI{image: "prov:pinned", runner: runner}
	spec := RunSpec{
		Argv:   []string{"true"},
		Mounts: []Mount{{Host: `C:\Users\dev\pool`, Container: "/pool"}},
	}
	if err := o.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"run", "--rm", "--network=none", "--mount", "type=bind,src=C:/Users/dev/pool,dst=/pool", "prov:pinned", "true"}
	if got := runner.calls[0].args; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestOCIName(t *testing.T) {
	if got := (&OCI{}).Name(); got != "oci" {
		t.Errorf("Name() = %q, want %q", got, "oci")
	}
}

func TestNewOCIRequiresImage(t *testing.T) {
	if _, err := NewOCI(""); err == nil {
		t.Error("NewOCI(\"\"): error = nil, want error")
	}
	o, err := NewOCI("prov:pinned")
	if err != nil {
		t.Fatalf("NewOCI: %v", err)
	}
	if o == nil {
		t.Fatal("NewOCI returned nil *OCI with a nil error")
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
