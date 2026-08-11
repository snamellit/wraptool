package doctor

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// toolTestInput builds a *Input over env with cfg wired in, satisfying
// NeedsConfig|NeedsModeBits (GOOS "linux") the way every check in this file
// declares.
func toolTestInput(cfg *config.Config, env *MapEnv) *Input {
	return &Input{
		Env:         env,
		GOOS:        "linux",
		Paths:       HostPaths{ConfigDir: "/home/user/.config/wraptool"},
		Config:      cfg,
		ConfigPath:  "/home/user/.config/wraptool/config.yaml",
		ConfigState: ConfigLoaded,
	}
}

// ---------------------------------------------------------------- tools.binary-executable

func TestRunToolBinaryExecutable(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *config.Config
		build       func(env *MapEnv)
		wantOutcome Outcome
		wantCount   int
	}{
		{
			name: "no tools",
			cfg:  &config.Config{},
			build: func(env *MapEnv) {
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "binary is relative — config.validate's own finding, skipped here",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "git"},
			}},
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomePass,
		},
		{
			name: "binary does not exist — config.validate's own finding, skipped here",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git"},
			}},
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomePass,
		},
		{
			name: "binary is not regular — config.validate's own finding, skipped here",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git"},
			}},
			build: func(env *MapEnv) {
				env.Files["/usr/bin/git"] = MapFile{Mode: fs.ModeDir | 0o755}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "binary is 0644 — no execute bit for anyone",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git"},
			}},
			build: func(env *MapEnv) {
				env.Files["/usr/bin/git"] = MapFile{Mode: 0o644}
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
		},
		{
			name: "binary is 0755 — clean",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git"},
			}},
			build: func(env *MapEnv) {
				env.Files["/usr/bin/git"] = MapFile{Mode: 0o755}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "binary is a symlink to an executable target",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git"},
			}},
			build: func(env *MapEnv) {
				env.Links["/usr/bin/git"] = "/opt/real-git"
				env.Files["/opt/real-git"] = MapFile{Mode: 0o755}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "owner-executable only, running uid matches",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git"},
			}},
			build: func(env *MapEnv) {
				env.Files["/usr/bin/git"] = MapFile{Mode: 0o700, UID: 1000, GID: 1000}
				env.UID, env.GID = 1000, 1000
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "owner-executable only, running uid differs and gid differs — fail",
			cfg: &config.Config{Tools: map[string]config.ToolConfig{
				"git": {Binary: "/usr/bin/git"},
			}},
			build: func(env *MapEnv) {
				env.Files["/usr/bin/git"] = MapFile{Mode: 0o700, UID: 0, GID: 0}
				env.UID, env.GID = 1000, 1000
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			res := runToolBinaryExecutable(context.Background(), toolTestInput(tc.cfg, env))
			if got := res.Outcome(); got != tc.wantOutcome {
				t.Fatalf("Outcome() = %v, want %v (findings: %+v)", got, tc.wantOutcome, res.Findings)
			}
			if tc.wantOutcome == OutcomeFinding && len(res.Findings) != tc.wantCount {
				t.Errorf("len(Findings) = %d, want %d", len(res.Findings), tc.wantCount)
			}
			for _, f := range res.Findings {
				if f.Severity != SeverityFail {
					t.Errorf("Severity = %v, want fail", f.Severity)
				}
				if f.Remedy == "" {
					t.Errorf("Remedy is empty")
				}
			}
		})
	}
}

// ---------------------------------------------------------------- tools.binary-writable

func TestRunToolBinaryWritable(t *testing.T) {
	cases := []struct {
		name        string
		build       func(env *MapEnv)
		wantOutcome Outcome
		wantSegment string // substring the Detail must name, when wantOutcome == OutcomeFinding
	}{
		{
			name: "clean tree",
			build: func(env *MapEnv) {
				env.Files["/"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local/bin"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local/bin/git"] = MapFile{Mode: 0o755}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "a root-level ancestor is group-writable — reported, not a deeper one",
			build: func(env *MapEnv) {
				env.Files["/"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local"] = MapFile{Mode: fs.ModeDir | 0o775, GID: 100} // Homebrew shape
				env.Files["/usr/local/bin"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local/bin/git"] = MapFile{Mode: 0o755}
			},
			wantOutcome: OutcomeFinding,
			wantSegment: "/usr/local",
		},
		{
			name: "the binary file itself is other-writable",
			build: func(env *MapEnv) {
				env.Files["/"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local/bin"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local/bin/git"] = MapFile{Mode: 0o757}
			},
			wantOutcome: OutcomeFinding,
			wantSegment: "/usr/local/bin/git",
		},
		{
			// Reproduces the reviewer's false negative: binary: names a
			// symlink living in a 0777 directory, whose RESOLVED target
			// lives under a perfectly clean tree. Inspecting only
			// resolved's ancestors (the old behaviour) reported "0 failed,
			// 0 warnings" here -- any local user can replace the symlink
			// and own every invocation of the tool.
			name: "binary is a symlink inside a world-writable directory, resolved target's tree is clean",
			build: func(env *MapEnv) {
				env.Files["/"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/usr/local/bin"] = MapFile{Mode: fs.ModeDir | 0o777} // world-writable: holds the symlink
				env.Links["/usr/local/bin/git"] = "/gnu/store/xyz-git/bin/git"
				env.Files["/gnu"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/gnu/store"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/gnu/store/xyz-git"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/gnu/store/xyz-git/bin"] = MapFile{Mode: fs.ModeDir | 0o755}
				env.Files["/gnu/store/xyz-git/bin/git"] = MapFile{Mode: 0o755}
			},
			wantOutcome: OutcomeFinding,
			wantSegment: "/usr/local/bin",
		},
	}
	cfg := &config.Config{Tools: map[string]config.ToolConfig{
		"git": {Binary: "/usr/local/bin/git"},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			res := runToolBinaryWritable(context.Background(), toolTestInput(cfg, env))
			if got := res.Outcome(); got != tc.wantOutcome {
				t.Fatalf("Outcome() = %v, want %v (findings: %+v)", got, tc.wantOutcome, res.Findings)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			if len(res.Findings) != 1 {
				t.Fatalf("len(Findings) = %d, want 1", len(res.Findings))
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("Severity = %v, want warn", f.Severity)
			}
			if !strings.Contains(f.Detail, tc.wantSegment) {
				t.Errorf("Detail %q does not name offending segment %q", f.Detail, tc.wantSegment)
			}
		})
	}
}

// ---------------------------------------------------------------- tools.working-dir

func TestRunToolWorkingDir(t *testing.T) {
	cases := []struct {
		name        string
		wd          string
		build       func(env *MapEnv)
		wantOutcome Outcome
	}{
		{name: "unset", wd: "", build: func(env *MapEnv) {}, wantOutcome: OutcomePass},
		{name: "relative", wd: "relative/dir", build: func(env *MapEnv) {}, wantOutcome: OutcomeFinding},
		{name: "absolute but missing", wd: "/var/lib/wraptool", build: func(env *MapEnv) {}, wantOutcome: OutcomeFinding},
		{
			name: "absolute but is a file, not a directory",
			wd:   "/var/lib/wraptool",
			build: func(env *MapEnv) {
				env.Files["/var/lib/wraptool"] = MapFile{Mode: 0o644}
			},
			wantOutcome: OutcomeFinding,
		},
		{
			name: "existing absolute directory",
			wd:   "/var/lib/wraptool",
			build: func(env *MapEnv) {
				env.Files["/var/lib/wraptool"] = MapFile{Mode: fs.ModeDir | 0o755}
			},
			wantOutcome: OutcomePass,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			cfg := &config.Config{Tools: map[string]config.ToolConfig{
				"gcloud": {Binary: "/usr/bin/gcloud", WorkingDir: tc.wd},
			}}
			res := runToolWorkingDir(context.Background(), toolTestInput(cfg, env))
			if got := res.Outcome(); got != tc.wantOutcome {
				t.Fatalf("Outcome() = %v, want %v (findings: %+v)", got, tc.wantOutcome, res.Findings)
			}
			for _, f := range res.Findings {
				if f.Severity != SeverityFail {
					t.Errorf("Severity = %v, want fail", f.Severity)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- env_files (both checks)

func toolEnvFilesFixtureConfig() *config.Config {
	return &config.Config{
		Tools: map[string]config.ToolConfig{
			"gcloud": {Binary: "/usr/bin/gcloud", EnvFiles: []string{"/etc/wraptool/gcloud.env"}},
		},
		MCPServers: map[string]config.MCPServerConfig{
			"github": {Command: "/usr/bin/mcp-github", EnvFiles: []string{"~/.config/wraptool/github.env"}},
		},
	}
}

func TestRunToolEnvFileMissing(t *testing.T) {
	t.Run("both present and readable", func(t *testing.T) {
		env := NewMapEnv()
		env.Vars["HOME"] = "/home/user"
		env.Files["/etc/wraptool/gcloud.env"] = MapFile{Mode: 0o600, Data: []byte("A=B\n")}
		env.Files["/home/user/.config/wraptool/github.env"] = MapFile{Mode: 0o600, Data: []byte("A=B\n")}
		res := runToolEnvFileMissing(context.Background(), toolTestInput(toolEnvFilesFixtureConfig(), env))
		if got := res.Outcome(); got != OutcomePass {
			t.Fatalf("Outcome() = %v, want pass (findings: %+v)", got, res.Findings)
		}
	})

	t.Run("both missing", func(t *testing.T) {
		env := NewMapEnv()
		env.Vars["HOME"] = "/home/user"
		res := runToolEnvFileMissing(context.Background(), toolTestInput(toolEnvFilesFixtureConfig(), env))
		if got := res.Outcome(); got != OutcomeFinding {
			t.Fatalf("Outcome() = %v, want finding (findings: %+v)", got, res.Findings)
		}
		if len(res.Findings) != 2 {
			t.Fatalf("len(Findings) = %d, want 2", len(res.Findings))
		}
		for _, f := range res.Findings {
			if f.Severity != SeverityFail {
				t.Errorf("Severity = %v, want fail", f.Severity)
			}
		}
		// Deterministic order: tools.* before mcp_servers.*.
		if !strings.Contains(res.Findings[0].Detail, "tools.gcloud") {
			t.Errorf("Findings[0].Detail = %q, want to name tools.gcloud", res.Findings[0].Detail)
		}
		if !strings.Contains(res.Findings[1].Detail, "mcp_servers.github") {
			t.Errorf("Findings[1].Detail = %q, want to name mcp_servers.github", res.Findings[1].Detail)
		}
	})

	t.Run("HOME expansion resolves the mcp_servers entry", func(t *testing.T) {
		env := NewMapEnv()
		env.Vars["HOME"] = "/home/user"
		env.Files["/etc/wraptool/gcloud.env"] = MapFile{Mode: 0o600, Data: []byte("A=B\n")}
		// github.env deliberately absent, to prove the path used for the
		// lookup really was the expanded one (a wrong expansion would report
		// the WRONG path in Detail while still failing, hiding a real bug).
		res := runToolEnvFileMissing(context.Background(), toolTestInput(toolEnvFilesFixtureConfig(), env))
		if len(res.Findings) != 1 {
			t.Fatalf("len(Findings) = %d, want 1", len(res.Findings))
		}
		if !strings.Contains(res.Findings[0].Detail, "/home/user/.config/wraptool/github.env") {
			t.Errorf("Detail = %q, want the expanded HOME path", res.Findings[0].Detail)
		}
	})
}

func TestRunToolEnvFilePerms(t *testing.T) {
	t.Run("tight perms — clean", func(t *testing.T) {
		env := NewMapEnv()
		env.Vars["HOME"] = "/home/user"
		env.Files["/etc/wraptool/gcloud.env"] = MapFile{Mode: 0o600, Data: []byte("A=B\n")}
		env.Files["/home/user/.config/wraptool/github.env"] = MapFile{Mode: 0o600, Data: []byte("A=B\n")}
		res := runToolEnvFilePerms(context.Background(), toolTestInput(toolEnvFilesFixtureConfig(), env))
		if got := res.Outcome(); got != OutcomePass {
			t.Fatalf("Outcome() = %v, want pass (findings: %+v)", got, res.Findings)
		}
	})

	t.Run("group-readable env_files entry", func(t *testing.T) {
		env := NewMapEnv()
		env.Vars["HOME"] = "/home/user"
		env.Files["/etc/wraptool/gcloud.env"] = MapFile{Mode: 0o640, Data: []byte("A=B\n")}
		env.Files["/home/user/.config/wraptool/github.env"] = MapFile{Mode: 0o600, Data: []byte("A=B\n")}
		res := runToolEnvFilePerms(context.Background(), toolTestInput(toolEnvFilesFixtureConfig(), env))
		if got := res.Outcome(); got != OutcomeFinding {
			t.Fatalf("Outcome() = %v, want finding (findings: %+v)", got, res.Findings)
		}
		if len(res.Findings) != 1 {
			t.Fatalf("len(Findings) = %d, want 1", len(res.Findings))
		}
		f := res.Findings[0]
		if f.Severity != SeverityFail {
			t.Errorf("Severity = %v, want fail", f.Severity)
		}
		if !strings.Contains(f.Detail, "tools.gcloud") {
			t.Errorf("Detail = %q, want to name tools.gcloud", f.Detail)
		}
	})

	t.Run("missing entry is skipped here — env-file-missing's own finding", func(t *testing.T) {
		env := NewMapEnv()
		env.Vars["HOME"] = "/home/user"
		// Neither file exists.
		res := runToolEnvFilePerms(context.Background(), toolTestInput(toolEnvFilesFixtureConfig(), env))
		if got := res.Outcome(); got != OutcomePass {
			t.Fatalf("Outcome() = %v, want pass (findings: %+v)", got, res.Findings)
		}
	})
}

// ---------------------------------------------------------------- tools.inert

func TestRunToolInert(t *testing.T) {
	cfg := &config.Config{Tools: map[string]config.ToolConfig{
		"git":    {Binary: "/usr/bin/git", Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		"gcloud": {Binary: "/usr/bin/gcloud"}, // no allow rules — inert
	}}
	env := NewMapEnv()
	res := runToolInert(context.Background(), toolTestInput(cfg, env))
	if got := res.Outcome(); got != OutcomeFinding {
		t.Fatalf("Outcome() = %v, want finding (findings: %+v)", got, res.Findings)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(res.Findings))
	}
	f := res.Findings[0]
	if f.Severity != SeverityInfo {
		t.Errorf("Severity = %v, want info", f.Severity)
	}
	if !strings.Contains(f.Detail, "tools.gcloud") {
		t.Errorf("Detail = %q, want to name tools.gcloud", f.Detail)
	}

	t.Run("every tool has allow rules — clean", func(t *testing.T) {
		clean := &config.Config{Tools: map[string]config.ToolConfig{
			"git": {Binary: "/usr/bin/git", Allow: []config.AllowRule{{Subcommand: []string{"pull"}}}},
		}}
		res := runToolInert(context.Background(), toolTestInput(clean, NewMapEnv()))
		if got := res.Outcome(); got != OutcomePass {
			t.Fatalf("Outcome() = %v, want pass (findings: %+v)", got, res.Findings)
		}
	})
}

// ---------------------------------------------------------------- pure helpers

func TestToolExpandHome(t *testing.T) {
	env := NewMapEnv()
	env.Vars["HOME"] = "/home/user"
	cases := []struct {
		in   string
		want string
	}{
		{"~", "/home/user"},
		{"~/.config/wraptool/env", "/home/user/.config/wraptool/env"},
		{"/etc/wraptool/env", "/etc/wraptool/env"},
		{"relative/env", "relative/env"},
	}
	for _, tc := range cases {
		if got := toolExpandHome(env, tc.in); got != tc.want {
			t.Errorf("toolExpandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	t.Run("HOME unset leaves the path unchanged", func(t *testing.T) {
		empty := NewMapEnv()
		if got := toolExpandHome(empty, "~/env"); got != "~/env" {
			t.Errorf("toolExpandHome(~/env) = %q, want unchanged", got)
		}
	})
}

func TestToolGroupLabel(t *testing.T) {
	if got := toolGroupLabel(-1); got != "gid unknown" {
		t.Errorf("toolGroupLabel(-1) = %q, want %q", got, "gid unknown")
	}
	if got := toolGroupLabel(100); got != "gid 100" {
		t.Errorf("toolGroupLabel(100) = %q, want %q", got, "gid 100")
	}
}

func TestToolAncestorDirs(t *testing.T) {
	got := toolAncestorDirs("/usr/local/bin/git")
	want := []string{"/", "/usr", "/usr/local", "/usr/local/bin"}
	if len(got) != len(want) {
		t.Fatalf("toolAncestorDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("toolAncestorDirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------- Meta

func TestToolCheckMeta(t *testing.T) {
	cases := []struct {
		meta      Meta
		wantID    CheckID
		wantNeeds Requirement
	}{
		{toolBinaryExecutableMeta, toolBinaryExecutable, NeedsConfig | NeedsModeBits},
		{toolBinaryWritableMeta, toolBinaryWritable, NeedsConfig | NeedsModeBits},
		{toolWorkingDirMeta, toolWorkingDir, NeedsConfig},
		{toolEnvFileMissingMeta, toolEnvFileMissing, NeedsConfig},
		{toolEnvFilePermsMeta, toolEnvFilePerms, NeedsConfig | NeedsModeBits},
		{toolInertMeta, toolInert, NeedsConfig},
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
