package backend

import (
	"context"
	"reflect"
	"testing"
)

func TestGuixRunArgv(t *testing.T) {
	cases := []struct {
		name     string
		spec     RunSpec
		wantArgs []string
		wantDir  string
		wantEnv  map[string]string
	}{
		{
			name:     "zero value is no-network, no mounts, no env, no workdir",
			spec:     RunSpec{Argv: []string{"true"}},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--", "true"},
		},
		{
			name:     "network on adds --network",
			spec:     RunSpec{Argv: []string{"npm", "ci"}, Network: true},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--network", "--", "npm", "ci"},
		},
		{
			name: "read-only mount maps to --expose",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/pool/bin", Container: "/pool/bin", ReadOnly: true}},
			},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--expose=/pool/bin=/pool/bin", "--", "true"},
		},
		{
			name: "writable mount maps to --share",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/pool/home", Container: "/pool/home", ReadOnly: false}},
			},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--share=/pool/home=/pool/home", "--", "true"},
		},
		{
			name: "multiple mounts preserve given order and mix ro/rw",
			spec: RunSpec{
				Argv: []string{"true"},
				Mounts: []Mount{
					{Host: "/pool/bin", Container: "/pool/bin", ReadOnly: true},
					{Host: "/stage", Container: "/stage", ReadOnly: false},
				},
			},
			wantArgs: []string{
				"shell", "--container", "--emulate-fhs",
				"--expose=/pool/bin=/pool/bin",
				"--share=/stage=/stage",
				"--", "true",
			},
		},
		{
			name: "env produces one anchored --preserve per key, sorted",
			spec: RunSpec{
				Argv: []string{"true"},
				Env:  map[string]string{"WRAPTOOL_TOKEN": "abc", "HOME": "/pool/home"},
			},
			wantArgs: []string{
				"shell", "--container", "--emulate-fhs",
				"--preserve=^HOME$",
				"--preserve=^WRAPTOOL_TOKEN$",
				"--", "true",
			},
			wantEnv: map[string]string{"WRAPTOOL_TOKEN": "abc", "HOME": "/pool/home"},
		},
		{
			name:     "workdir is passed through as the host process directory",
			spec:     RunSpec{Argv: []string{"true"}, Workdir: "/stage/gen-1"},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--", "true"},
			wantDir:  "/stage/gen-1",
		},
		{
			name:     "platform is not guix's concern and produces no flag",
			spec:     RunSpec{Argv: []string{"true"}, Platform: "arm64"},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--", "true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			g := &Guix{runner: runner}

			if err := g.Run(context.Background(), tc.spec); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(runner.calls) != 1 {
				t.Fatalf("got %d runner calls, want 1", len(runner.calls))
			}
			call := runner.calls[0]

			if call.name != "guix" {
				t.Errorf("name = %q, want %q", call.name, "guix")
			}
			if !reflect.DeepEqual(call.args, tc.wantArgs) {
				t.Errorf("args = %v, want %v", call.args, tc.wantArgs)
			}
			if call.dir != tc.wantDir {
				t.Errorf("dir = %q, want %q", call.dir, tc.wantDir)
			}
			if tc.wantEnv != nil && !reflect.DeepEqual(call.env, tc.wantEnv) {
				t.Errorf("env = %v, want %v", call.env, tc.wantEnv)
			}
		})
	}
}

func TestGuixName(t *testing.T) {
	if got := (&Guix{}).Name(); got != "guix" {
		t.Errorf("Name() = %q, want %q", got, "guix")
	}
}

func TestGuixAvailable(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		defer stubLookPath("guix")()
		if !(&Guix{}).Available() {
			t.Error("Available() = false, want true when guix is on PATH")
		}
	})
	t.Run("absent", func(t *testing.T) {
		defer stubLookPath()()
		if (&Guix{}).Available() {
			t.Error("Available() = true, want false when guix is not on PATH")
		}
	})
}
