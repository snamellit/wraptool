package doctor

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// configpermsPath is the fixed config path every test in this file uses.
const configpermsPath = "/home/user/.config/wraptool/config.yaml"

// configpermsInput builds a hand-built *Input against env, mirroring what the
// engine hands NeedsConfig-gated checks (plan §1 rule 8: every test here
// calls a check's RunFunc directly, never through Registered()).
func configpermsInput(env *MapEnv, cfg *config.Config) *Input {
	return &Input{
		Env:         env,
		ConfigPath:  configpermsPath,
		ConfigState: ConfigLoaded,
		Config:      cfg,
	}
}

// ---------------------------------------------------------------- Meta

// TestConfigPermsCheckMeta pins every check's ID and Needs against the SAME
// Meta literal init() registers, never through Registered() (plan §1 rule 8:
// see pool_test.go's TestPoolCheckMeta for the identical rationale — once
// this package's eight sibling Phase-B files have all merged, Registered()
// carries every one of THEIR checks too).
func TestConfigPermsCheckMeta(t *testing.T) {
	cases := []struct {
		meta      Meta
		wantID    CheckID
		wantNeeds Requirement
	}{
		{configpermsFilePermsMeta, configpermsFilePerms, NeedsConfig | NeedsModeBits},
		{configpermsDirPermsMeta, configpermsDirPerms, NeedsConfig | NeedsModeBits},
		{configpermsFileReadableSecretsMeta, configpermsFileReadableSecrets, NeedsConfig | NeedsModeBits},
		{configpermsSymlinkedMeta, configpermsSymlinked, NeedsConfig},
	}
	for _, tc := range cases {
		t.Run(string(tc.wantID), func(t *testing.T) {
			if tc.meta.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", tc.meta.ID, tc.wantID)
			}
			if tc.meta.Needs != tc.wantNeeds {
				t.Errorf("Needs = %v, want %v", tc.meta.Needs, tc.wantNeeds)
			}
		})
	}
}

// ---------------------------------------------------------------- config.file-perms

func TestRunConfigPermsFilePerms(t *testing.T) {
	cases := []struct {
		name        string
		build       func(env *MapEnv)
		wantOutcome Outcome
		wantDetail  []string // substrings that must appear in Detail
	}{
		{
			name:        "config path does not resolve",
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "0600 is clean",
			build: func(env *MapEnv) {
				env.Files[configpermsPath] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "0644 (group/other readable, not writable) is clean",
			build: func(env *MapEnv) {
				env.Files[configpermsPath] = MapFile{Mode: 0o644}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "0664 is group-writable",
			build: func(env *MapEnv) {
				env.Files[configpermsPath] = MapFile{Mode: 0o664}
			},
			wantOutcome: OutcomeFinding,
			wantDetail:  []string{"0664", "group-writable"},
		},
		{
			name: "0666 is group- and other-writable",
			build: func(env *MapEnv) {
				env.Files[configpermsPath] = MapFile{Mode: 0o666}
			},
			wantOutcome: OutcomeFinding,
			wantDetail:  []string{"0666", "group- and other-writable"},
		},
		{
			name: "writable through a symlink target is caught (symlinks resolved)",
			build: func(env *MapEnv) {
				env.Links[configpermsPath] = "/home/user/dotfiles/wraptool.yaml"
				env.Files["/home/user/dotfiles/wraptool.yaml"] = MapFile{Mode: 0o664}
			},
			wantOutcome: OutcomeFinding,
			wantDetail:  []string{"/home/user/dotfiles/wraptool.yaml", "group-writable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			in := configpermsInput(env, &config.Config{})

			res := runConfigPermsFilePerms(context.Background(), in)

			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("Outcome() = %v, want %v; res=%+v", res.Outcome(), tc.wantOutcome, res)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityFail {
				t.Errorf("Severity = %q, want fail", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("Remedy is empty for a fail finding")
			}
			if !f.Path.Empty() {
				t.Errorf("Path = %v, want empty — this is a file-posture finding, not config content (RULING R5)", f.Path)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", f.Detail, want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- config.dir-perms

func TestRunConfigPermsDirPerms(t *testing.T) {
	cases := []struct {
		name        string
		build       func(env *MapEnv)
		wantOutcome Outcome
		wantDetail  []string
	}{
		{
			name:        "config path does not resolve",
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "0700 dir is clean",
			build: func(env *MapEnv) {
				env.Files["/home/user/.config/wraptool"] = MapFile{Mode: fs.ModeDir | 0o700}
				env.Files[configpermsPath] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "0755 dir (group/other readable+executable, not writable) is clean",
			build: func(env *MapEnv) {
				env.Files["/home/user/.config/wraptool"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files[configpermsPath] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "0775 dir is group-writable",
			build: func(env *MapEnv) {
				env.Files["/home/user/.config/wraptool"] = MapFile{Mode: fs.ModeDir | 0o775}
				env.Files[configpermsPath] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomeFinding,
			wantDetail:  []string{"/home/user/.config/wraptool", "group-writable"},
		},
		{
			name: "the directory holding a symlink's TARGET is checked, not the link's own directory",
			build: func(env *MapEnv) {
				env.Links[configpermsPath] = "/home/user/dotfiles/wraptool.yaml"
				env.Files["/home/user/.config/wraptool"] = MapFile{Mode: fs.ModeDir | 0o700}
				env.Files["/home/user/dotfiles"] = MapFile{Mode: fs.ModeDir | 0o777}
				env.Files["/home/user/dotfiles/wraptool.yaml"] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomeFinding,
			wantDetail:  []string{"/home/user/dotfiles", "group- and other-writable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			in := configpermsInput(env, &config.Config{})

			res := runConfigPermsDirPerms(context.Background(), in)

			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("Outcome() = %v, want %v; res=%+v", res.Outcome(), tc.wantOutcome, res)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityFail {
				t.Errorf("Severity = %q, want fail", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("Remedy is empty for a fail finding")
			}
			if !f.Path.Empty() {
				t.Errorf("Path = %v, want empty — this is a file-posture finding, not config content (RULING R5)", f.Path)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", f.Detail, want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- config.file-readable-secrets

func TestRunConfigPermsFileReadableSecrets(t *testing.T) {
	env := NewMapEnv()
	env.Files[configpermsPath] = MapFile{Mode: 0o600}
	in := configpermsInput(env, &config.Config{
		Tools: map[string]config.ToolConfig{
			"git": {Env: map[string]string{"GH_TOKEN": "secret-value"}},
		},
	})

	res := runConfigPermsFileReadableSecrets(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("Outcome() = %v, want pass — config is not group/other readable; findings=%+v", res.Outcome(), res.Findings)
	}
}

func TestRunConfigPermsFileReadableSecrets_ReadableNoEnv(t *testing.T) {
	env := NewMapEnv()
	env.Files[configpermsPath] = MapFile{Mode: 0o644}
	in := configpermsInput(env, &config.Config{
		Tools: map[string]config.ToolConfig{
			"git": {Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		},
	})

	res := runConfigPermsFileReadableSecrets(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("Outcome() = %v, want pass — no inline env: entries at all; findings=%+v", res.Outcome(), res.Findings)
	}
}

func TestRunConfigPermsFileReadableSecrets_ReadableWithSecrets(t *testing.T) {
	env := NewMapEnv()
	env.Files[configpermsPath] = MapFile{Mode: 0o644}
	in := configpermsInput(env, &config.Config{
		Tools: map[string]config.ToolConfig{
			"git":     {Env: map[string]string{"GH_TOKEN": "should-never-appear"}},
			"kubectl": {}, // no env: entries — must not produce a finding
		},
		MCPServers: map[string]config.MCPServerConfig{
			"fetch": {Env: map[string]string{"API_KEY": "also-secret"}},
		},
	})

	res := runConfigPermsFileReadableSecrets(context.Background(), in)

	if res.Outcome() != OutcomeFinding {
		t.Fatalf("Outcome() = %v, want finding; res=%+v", res.Outcome(), res)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("len(Findings) = %d, want 2 (one per tools/mcp_servers entry with env:): %+v", len(res.Findings), res.Findings)
	}

	byPath := map[string]Finding{}
	for _, f := range res.Findings {
		byPath[f.Path.String()] = f
		if f.Severity != SeverityFail {
			t.Errorf("Severity = %q, want fail", f.Severity)
		}
		if f.Remedy == "" {
			t.Error("Remedy is empty for a fail finding")
		}
		if strings.Contains(f.Detail, "should-never-appear") || strings.Contains(f.Detail, "also-secret") {
			t.Errorf("Detail leaks a secret VALUE (§10.1 forbids this): %q", f.Detail)
		}
	}

	git, ok := byPath["tools.git.env"]
	if !ok {
		t.Fatalf("no finding for tools.git.env; got paths %v", configpermsKeysOf(byPath))
	}
	if !strings.Contains(git.Detail, "GH_TOKEN") {
		t.Errorf("git Detail = %q, want it to name the key GH_TOKEN", git.Detail)
	}

	fetch, ok := byPath["mcp_servers.fetch.env"]
	if !ok {
		t.Fatalf("no finding for mcp_servers.fetch.env; got paths %v", configpermsKeysOf(byPath))
	}
	if !strings.Contains(fetch.Detail, "API_KEY") {
		t.Errorf("fetch Detail = %q, want it to name the key API_KEY", fetch.Detail)
	}
}

func configpermsKeysOf(m map[string]Finding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRunConfigPermsFileReadableSecrets_NilConfigDoesNotPanic(t *testing.T) {
	env := NewMapEnv()
	env.Files[configpermsPath] = MapFile{Mode: 0o644}
	in := configpermsInput(env, nil)

	res := runConfigPermsFileReadableSecrets(context.Background(), in)

	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("Outcome() = %v, want skipped (nil Config is not this check's production shape, "+
			"but a hand-built test Input must not panic)", res.Outcome())
	}
}

func TestRunConfigPermsFileReadableSecrets_UnresolvableConfigSkips(t *testing.T) {
	env := NewMapEnv()
	in := configpermsInput(env, &config.Config{})

	res := runConfigPermsFileReadableSecrets(context.Background(), in)

	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("Outcome() = %v, want skipped", res.Outcome())
	}
}

// ---------------------------------------------------------------- config.symlinked

func TestRunConfigPermsSymlinked(t *testing.T) {
	cases := []struct {
		name        string
		build       func(env *MapEnv)
		wantOutcome Outcome
		wantDetail  []string
	}{
		{
			name:        "not even present",
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "a plain file is not a symlink",
			build: func(env *MapEnv) {
				env.Files[configpermsPath] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "a symlink warns and names the target",
			build: func(env *MapEnv) {
				env.Links[configpermsPath] = "/home/user/dotfiles/wraptool.yaml"
				env.Files["/home/user/dotfiles/wraptool.yaml"] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomeFinding,
			wantDetail:  []string{configpermsPath, "/home/user/dotfiles/wraptool.yaml"},
		},
		{
			name: "a symlink to a target that cannot itself be resolved still warns",
			build: func(env *MapEnv) {
				// Links a target that has no Files entry: EvalSymlinks(configpermsPath)
				// fails even though Lstat(configpermsPath) succeeds and reports a symlink.
				env.Links[configpermsPath] = "/home/user/dotfiles/gone.yaml"
			},
			wantOutcome: OutcomeFinding,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			in := configpermsInput(env, &config.Config{})

			res := runConfigPermsSymlinked(context.Background(), in)

			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("Outcome() = %v, want %v; res=%+v", res.Outcome(), tc.wantOutcome, res)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("Severity = %q, want warn", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("Remedy is empty for a warn finding")
			}
			if !f.Path.Empty() {
				t.Errorf("Path = %v, want empty — this is a file-posture finding, not config content (RULING R5)", f.Path)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(f.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", f.Detail, want)
				}
			}
		})
	}
}
