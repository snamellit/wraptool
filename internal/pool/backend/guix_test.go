package backend

import (
	"context"
	"reflect"
	"runtime"
	"testing"
)

func TestGuixRunArgv(t *testing.T) {
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}

	cases := []struct {
		name     string
		pkgs     []string
		spec     RunSpec
		wantArgs []string
		wantDir  string
		wantEnv  map[string]string
	}{
		{
			name:     "zero value is no-network, no mounts, no env, no workdir",
			spec:     RunSpec{Argv: []string{"true"}},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--no-cwd", "--", "true"},
		},
		{
			name:     "network on adds --network",
			spec:     RunSpec{Argv: []string{"npm", "ci"}, Network: true},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--no-cwd", "--network", "--", "npm", "ci"},
		},
		{
			name: "read-only mount maps to --expose",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/pool/bin", Container: "/pool/bin", ReadOnly: true}},
			},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--no-cwd", "--expose=/pool/bin=/pool/bin", "--", "true"},
		},
		{
			name: "writable mount maps to --share",
			spec: RunSpec{
				Argv:   []string{"true"},
				Mounts: []Mount{{Host: "/pool/home", Container: "/pool/home", ReadOnly: false}},
			},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--no-cwd", "--share=/pool/home=/pool/home", "--", "true"},
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
				"shell", "--container", "--emulate-fhs", "--no-cwd",
				"--expose=/pool/bin=/pool/bin",
				"--share=/stage=/stage",
				"--", "true",
			},
		},
		{
			name: "env produces one anchored --preserve per key, sorted",
			spec: RunSpec{
				Argv: []string{"true"},
				Env:  map[string]string{"WRAPTOOL_TOKEN": "abc", "npm_config_cache": "/cache"},
			},
			wantArgs: []string{
				"shell", "--container", "--emulate-fhs", "--no-cwd",
				"--preserve=^WRAPTOOL_TOKEN$",
				"--preserve=^npm_config_cache$",
				"--", "true",
			},
			wantEnv: map[string]string{"WRAPTOOL_TOKEN": "abc", "npm_config_cache": "/cache"},
		},
		{
			name:     "invalid env var name is rejected, not smuggled through",
			spec:     RunSpec{Argv: []string{"true"}, Env: map[string]string{"BAD-KEY!": "x"}},
			wantArgs: nil, // Run returns an error before calling the runner
		},
		{
			name:     "mount host path containing ',' is rejected",
			spec:     RunSpec{Argv: []string{"true"}, Mounts: []Mount{{Host: "/cache,evil", Container: "/cache"}}},
			wantArgs: nil,
		},
		{
			name:     "mount container path containing ',' is rejected",
			spec:     RunSpec{Argv: []string{"true"}, Mounts: []Mount{{Host: "/cache", Container: "/cache,evil"}}},
			wantArgs: nil,
		},
		{
			name:     "workdir is realized via env --chdir, not the host cwd",
			spec:     RunSpec{Argv: []string{"true"}, Workdir: "/stage/gen-1"},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--no-cwd", "coreutils", "--", "env", "--chdir=/stage/gen-1", "true"},
			wantDir:  "",
		},
		{
			name:     "workdir does not duplicate an already-listed coreutils",
			pkgs:     []string{"coreutils", "node"},
			spec:     RunSpec{Argv: []string{"true"}, Workdir: "/stage/gen-1"},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--no-cwd", "coreutils", "node", "--", "env", "--chdir=/stage/gen-1", "true"},
			wantDir:  "",
		},
		{
			name:     "platform matching the host architecture is accepted",
			spec:     RunSpec{Argv: []string{"true"}, Platform: hostArch()},
			wantArgs: []string{"shell", "--container", "--emulate-fhs", "--no-cwd", "--", "true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			g := &Guix{pkgs: tc.pkgs, runner: runner}

			err := g.Run(context.Background(), tc.spec)
			if tc.wantArgs == nil {
				if err == nil {
					t.Fatal("Run: error = nil, want error")
				}
				if len(runner.calls) != 0 {
					t.Fatalf("got %d runner calls, want 0", len(runner.calls))
				}
				return
			}
			if err != nil {
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

	t.Run("platform mismatching the host architecture errors", func(t *testing.T) {
		runner := &fakeRunner{}
		g := &Guix{runner: runner}
		err := g.Run(context.Background(), RunSpec{Argv: []string{"true"}, Platform: otherArch})
		if err == nil {
			t.Fatal("Run: error = nil, want error")
		}
		if len(runner.calls) != 0 {
			t.Fatalf("got %d runner calls, want 0", len(runner.calls))
		}
	})

	t.Run("empty Argv is rejected", func(t *testing.T) {
		runner := &fakeRunner{}
		g := &Guix{runner: runner}
		if err := g.Run(context.Background(), RunSpec{}); err == nil {
			t.Fatal("Run: error = nil, want error")
		}
		if len(runner.calls) != 0 {
			t.Fatalf("got %d runner calls, want 0", len(runner.calls))
		}
	})

	t.Run("non-empty User is rejected (guix always runs as the invoking uid)", func(t *testing.T) {
		runner := &fakeRunner{}
		g := &Guix{runner: runner}
		spec := RunSpec{Argv: []string{"true"}, User: "1000:1000"}
		if err := g.Run(context.Background(), spec); err == nil {
			t.Fatal("Run: error = nil, want error")
		}
		if len(runner.calls) != 0 {
			t.Fatalf("got %d runner calls, want 0", len(runner.calls))
		}
	})
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
