package doctor

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// exposureFilesTestInput builds a hand-crafted *Input directly (plan §1 rule
// 8: no test here asserts over the global Registered()).
func exposureFilesTestInput(env *MapEnv, cfg *config.Config) *Input {
	return &Input{
		Env:    env,
		GOOS:   "linux",
		Config: cfg,
	}
}

// ---------------------------------------------------------------- token-perms

func TestRunExposureFilesTokenPerms(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *config.Config
		build       func(env *MapEnv)
		wantOutcome Outcome
		wantCount   int
		wantLabels  []string // substrings expected somewhere in the findings' Detail, in order
	}{
		{
			name:        "no auth_token_file at all",
			cfg:         &config.Config{},
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "mcp token is 0600, clean",
			cfg:  &config.Config{MCP: config.MCPConfig{AuthTokenFile: "/etc/wraptool/mcp-token"}},
			build: func(env *MapEnv) {
				env.Files["/etc/wraptool/mcp-token"] = MapFile{Mode: 0o600, Data: []byte("x")}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "mcp token is world-readable",
			cfg:  &config.Config{MCP: config.MCPConfig{AuthTokenFile: "/etc/wraptool/mcp-token"}},
			build: func(env *MapEnv) {
				env.Files["/etc/wraptool/mcp-token"] = MapFile{Mode: 0o644, Data: []byte("x")}
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
			wantLabels:  []string{"mcp.auth_token_file"},
		},
		{
			name: "admin token is group-writable",
			cfg:  &config.Config{Admin: config.AdminConfig{AuthTokenFile: "/etc/wraptool/admin-token"}},
			build: func(env *MapEnv) {
				env.Files["/etc/wraptool/admin-token"] = MapFile{Mode: 0o620, Data: []byte("x")}
			},
			wantOutcome: OutcomeFinding,
			wantCount:   1,
			wantLabels:  []string{"admin.auth_token_file"},
		},
		{
			name: "both mcp and admin tokens are offending, mcp reported first (fixed order)",
			cfg: &config.Config{
				MCP:   config.MCPConfig{AuthTokenFile: "/etc/wraptool/mcp-token"},
				Admin: config.AdminConfig{AuthTokenFile: "/etc/wraptool/admin-token"},
			},
			build: func(env *MapEnv) {
				env.Files["/etc/wraptool/mcp-token"] = MapFile{Mode: 0o644, Data: []byte("x")}
				env.Files["/etc/wraptool/admin-token"] = MapFile{Mode: 0o604, Data: []byte("x")}
			},
			wantOutcome: OutcomeFinding,
			wantCount:   2,
			wantLabels:  []string{"mcp.auth_token_file", "admin.auth_token_file"},
		},
		{
			name: "token file does not exist: silently skipped, not a finding",
			cfg:  &config.Config{MCP: config.MCPConfig{AuthTokenFile: "/etc/wraptool/mcp-token"}},
			build: func(env *MapEnv) {
			},
			wantOutcome: OutcomePass,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			res := runExposureFilesTokenPerms(context.Background(), exposureFilesTestInput(env, tc.cfg))
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v (findings=%+v)", res.Outcome(), tc.wantOutcome, res.Findings)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			if len(res.Findings) != tc.wantCount {
				t.Fatalf("got %d findings, want %d", len(res.Findings), tc.wantCount)
			}
			for i, label := range tc.wantLabels {
				f := res.Findings[i]
				if f.Severity != SeverityFail {
					t.Errorf("finding[%d] severity = %v, want fail", i, f.Severity)
				}
				if !strings.Contains(f.Detail, label) {
					t.Errorf("finding[%d] Detail %q does not contain %q", i, f.Detail, label)
				}
				if !f.Path.Empty() {
					t.Errorf("finding[%d]: Path must be empty (this is a non-config subject, not config content)", i)
				}
				if f.Location == "" {
					t.Errorf("finding[%d]: Location must be set (a real file, not config content)", i)
				}
				if f.Remedy == "" {
					t.Errorf("finding[%d]: fail severity requires a Remedy", i)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- token-weak

func TestRunExposureFilesTokenWeak(t *testing.T) {
	cases := []struct {
		name        string
		content     []byte
		wantOutcome Outcome
		wantDetail  string // substring
	}{
		{"empty token", []byte(""), OutcomeFinding, "empty"},
		{"short token", []byte("short\n"), OutcomeFinding, "byte(s)"},
		{"known placeholder, with trailing newline", []byte("changeme\n"), OutcomeFinding, "placeholder"},
		{"known placeholder, mixed case", []byte("ChangeMe\n"), OutcomeFinding, "placeholder"},
		{"long random token", []byte("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2\n"), OutcomePass, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			env.Files["/etc/wraptool/mcp-token"] = MapFile{Mode: 0o600, Data: tc.content}
			cfg := &config.Config{MCP: config.MCPConfig{AuthTokenFile: "/etc/wraptool/mcp-token"}}

			res := runExposureFilesTokenWeak(context.Background(), exposureFilesTestInput(env, cfg))
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", res.Outcome(), tc.wantOutcome)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			if !strings.Contains(f.Detail, tc.wantDetail) {
				t.Errorf("Detail %q does not contain %q", f.Detail, tc.wantDetail)
			}
			// §10.1: report length and shape only, never the value.
			if strings.TrimSpace(string(tc.content)) != "" && strings.Contains(f.Detail, strings.TrimSpace(string(tc.content))) {
				t.Errorf("Detail %q leaks the token value %q", f.Detail, tc.content)
			}
		})
	}
}

func TestRunExposureFilesTokenWeakNoTokenConfigured(t *testing.T) {
	res := runExposureFilesTokenWeak(context.Background(), exposureFilesTestInput(NewMapEnv(), &config.Config{}))
	if res.Outcome() != OutcomeSkipped {
		t.Fatalf("outcome = %v, want skipped", res.Outcome())
	}
}

func TestRunExposureFilesTokenWeakUnreadableFileIsSkippedNotFinding(t *testing.T) {
	env := NewMapEnv() // no /etc/wraptool/mcp-token entry: ReadFile fails
	cfg := &config.Config{MCP: config.MCPConfig{AuthTokenFile: "/etc/wraptool/mcp-token"}}
	res := runExposureFilesTokenWeak(context.Background(), exposureFilesTestInput(env, cfg))
	if res.Outcome() != OutcomePass {
		t.Fatalf("outcome = %v, want pass (nothing readable to judge)", res.Outcome())
	}
}

// ---------------------------------------------------------------- unix-socket-dir

func TestRunExposureFilesUnixSocketDir(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *config.Config
		build       func(env *MapEnv)
		wantOutcome Outcome
	}{
		{
			name:        "transport is not unix",
			cfg:         &config.Config{MCP: config.MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}},
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name:        "unix transport, listen unset",
			cfg:         &config.Config{MCP: config.MCPConfig{Transport: "unix"}},
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "socket does not exist yet, parent dir is 0700 (clean)",
			cfg:  &config.Config{MCP: config.MCPConfig{Transport: "unix", Listen: "/run/wraptool/wraptool.sock"}},
			build: func(env *MapEnv) {
				env.Files["/run/wraptool"] = MapFile{Mode: fs.ModeDir | 0o700}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "socket does not exist yet, parent dir is world-writable",
			cfg:  &config.Config{MCP: config.MCPConfig{Transport: "unix", Listen: "/run/wraptool/wraptool.sock"}},
			build: func(env *MapEnv) {
				env.Files["/run/wraptool"] = MapFile{Mode: fs.ModeDir | 0o777}
			},
			wantOutcome: OutcomeFinding,
		},
		{
			name: "socket's parent dir is world-writable but STICKY (e.g. /tmp): never flagged",
			cfg:  &config.Config{MCP: config.MCPConfig{Transport: "unix", Listen: "/tmp/wraptool.sock"}},
			build: func(env *MapEnv) {
				env.Files["/tmp"] = MapFile{Mode: fs.ModeDir | fs.ModeSticky | 0o777}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "a STALE socket FILE itself is world-writable: flagged regardless of parent",
			cfg:  &config.Config{MCP: config.MCPConfig{Transport: "unix", Listen: "/tmp/wraptool.sock"}},
			build: func(env *MapEnv) {
				env.Files["/tmp"] = MapFile{Mode: fs.ModeDir | fs.ModeSticky | 0o777}
				env.Files["/tmp/wraptool.sock"] = MapFile{Mode: fs.ModeSocket | 0o777}
			},
			wantOutcome: OutcomeFinding,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			res := runExposureFilesUnixSocketDir(context.Background(), exposureFilesTestInput(env, tc.cfg))
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", res.Outcome(), tc.wantOutcome)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityFail {
				t.Errorf("severity = %v, want fail", f.Severity)
			}
			if !f.Path.Empty() || f.Location == "" {
				t.Errorf("finding must set Location (non-config subject), not Path: Path=%v Location=%q", f.Path, f.Location)
			}
			if f.Remedy == "" {
				t.Error("fail finding requires a Remedy")
			}
		})
	}
}

// ---------------------------------------------------------------- cwd-roots-unset

func TestRunExposureFilesCWDRootsUnset(t *testing.T) {
	cases := []struct {
		name        string
		roots       []string
		wantOutcome Outcome
	}{
		{"unset", nil, OutcomeFinding},
		{"empty slice", []string{}, OutcomeFinding},
		{"set", []string{"/home/user/project"}, OutcomePass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{MCP: config.MCPConfig{AllowedCWDRoots: tc.roots}}
			res := runExposureFilesCWDRootsUnset(context.Background(), exposureFilesTestInput(NewMapEnv(), cfg))
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", res.Outcome(), tc.wantOutcome)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityWarn {
				t.Errorf("severity = %v, want warn", f.Severity)
			}
			// This IS config content, so Path must be set and Location must NOT be
			// (RULING R5, plan §1 rule 9).
			if f.Path.Empty() {
				t.Error("finding must set Path (config content) — RULING R5")
			}
			if f.Location != "" {
				t.Errorf("finding must not set Location itself (engine-owned for config findings): got %q", f.Location)
			}
		})
	}
}

// ---------------------------------------------------------------- request-log-writable

func TestRunExposureFilesRequestLogWritable(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *config.Config
		build       func(env *MapEnv)
		wantOutcome Outcome
	}{
		{
			name:        "request_log unset",
			cfg:         &config.Config{},
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "log does not exist yet, parent dir writable by current user (fresh install)",
			cfg:  &config.Config{MCP: config.MCPConfig{RequestLog: "/home/user/.local/share/wraptool/requests.jsonl"}},
			build: func(env *MapEnv) {
				env.UID, env.GID = 1000, 1000
				env.Files["/home/user/.local/share/wraptool"] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "log does not exist yet, no existing ancestor is writable by current user",
			cfg:  &config.Config{MCP: config.MCPConfig{RequestLog: "/home/user/.local/share/wraptool/requests.jsonl"}},
			build: func(env *MapEnv) {
				env.UID, env.GID = 1000, 1000
				env.Files["/home/user/.local/share/wraptool"] = MapFile{Mode: fs.ModeDir | 0o500, UID: 0, GID: 0}
			},
			wantOutcome: OutcomeFinding,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			res := runExposureFilesRequestLogWritable(context.Background(), exposureFilesTestInput(env, tc.cfg))
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", res.Outcome(), tc.wantOutcome)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityFail {
				t.Errorf("severity = %v, want fail", f.Severity)
			}
			if f.Remedy == "" {
				t.Error("fail finding requires a Remedy")
			}
		})
	}
}

// ---------------------------------------------------------------- request-log-perms

func TestRunExposureFilesRequestLogPerms(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *config.Config
		build       func(env *MapEnv)
		wantOutcome Outcome
	}{
		{
			name:        "request_log unset",
			cfg:         &config.Config{},
			build:       func(env *MapEnv) {},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "log does not exist yet: nothing to repair, not a finding",
			cfg:  &config.Config{MCP: config.MCPConfig{RequestLog: "/home/user/.local/share/wraptool/requests.jsonl"}},
			build: func(env *MapEnv) {
			},
			wantOutcome: OutcomeSkipped,
		},
		{
			name: "log exists, 0600 (Store.append's own default)",
			cfg:  &config.Config{MCP: config.MCPConfig{RequestLog: "/home/user/.local/share/wraptool/requests.jsonl"}},
			build: func(env *MapEnv) {
				env.Files["/home/user/.local/share/wraptool/requests.jsonl"] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomePass,
		},
		{
			name: "log exists, group-readable (e.g. restored from a backup)",
			cfg:  &config.Config{MCP: config.MCPConfig{RequestLog: "/home/user/.local/share/wraptool/requests.jsonl"}},
			build: func(env *MapEnv) {
				env.Files["/home/user/.local/share/wraptool/requests.jsonl"] = MapFile{Mode: 0o640}
			},
			wantOutcome: OutcomeFinding,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.build(env)
			res := runExposureFilesRequestLogPerms(context.Background(), exposureFilesTestInput(env, tc.cfg))
			if res.Outcome() != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", res.Outcome(), tc.wantOutcome)
			}
			if tc.wantOutcome != OutcomeFinding {
				return
			}
			f := res.Findings[0]
			if f.Severity != SeverityFail {
				t.Errorf("severity = %v, want fail", f.Severity)
			}
			if !f.Path.Empty() || f.Location == "" {
				t.Errorf("finding must set Location (non-config subject), not Path: Path=%v Location=%q", f.Path, f.Location)
			}
			if f.Remedy == "" {
				t.Error("fail finding requires a Remedy")
			}
		})
	}
}

// ---------------------------------------------------------------- shared helpers

func TestExposureFilesTokenFilesFixedOrder(t *testing.T) {
	cfg := &config.Config{
		MCP:   config.MCPConfig{AuthTokenFile: "/mcp-token"},
		Admin: config.AdminConfig{AuthTokenFile: "/admin-token"},
	}
	got := exposureFilesTokenFiles(cfg)
	if len(got) != 2 || got[0].Path != "/mcp-token" || got[1].Path != "/admin-token" {
		t.Fatalf("exposureFilesTokenFiles order = %+v, want [mcp, admin]", got)
	}
}

func TestExposureFilesTokenWeaknessNeverEchoesTheValue(t *testing.T) {
	longRandom := "z9y8x7w6v5u4t3s2r1q0p9o8n7m6l5k4j3i2h1g0f9e8d7c6b5a4"
	if _, weak := exposureFilesTokenWeakness(longRandom); weak {
		t.Fatalf("a long, non-placeholder token must not be classified weak")
	}
	reason, weak := exposureFilesTokenWeakness("changeme")
	if !weak || strings.Contains(reason, "changeme") {
		t.Fatalf("reason %q must describe the SHAPE only, never echo the value", reason)
	}
}

// ---------------------------------------------------------------- Meta

func TestExposureFilesCheckMeta(t *testing.T) {
	cases := []struct {
		meta      Meta
		wantID    CheckID
		wantNeeds Requirement
	}{
		{exposureFilesTokenPermsMeta, exposureFilesTokenPerms, NeedsConfig | NeedsModeBits},
		{exposureFilesTokenWeakMeta, exposureFilesTokenWeak, NeedsConfig},
		{exposureFilesUnixSocketDirMeta, exposureFilesUnixSocketDir, NeedsConfig | NeedsModeBits},
		{exposureFilesCWDRootsUnsetMeta, exposureFilesCWDRootsUnset, NeedsConfig},
		{exposureFilesRequestLogWritableMeta, exposureFilesRequestLogWritable, NeedsConfig | NeedsModeBits},
		{exposureFilesRequestLogPermsMeta, exposureFilesRequestLogPerms, NeedsConfig | NeedsModeBits},
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
