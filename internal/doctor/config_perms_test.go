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
			// The printed mode includes the type bit and matches `ls -ld` (no
			// sticky bit here, so no trailing t/T).
			wantDetail: []string{"/home/user/.config/wraptool", "group-writable", "0775 (drwxrwxr-x)"},
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
		{
			// Original false positive (blocker 2): a 1777 dir (e.g. /tmp) is
			// group/other-writable, but the sticky bit means only the owner (or
			// root) can rename/unlink an entry they don't own — exactly the
			// risk this check exists to catch. Must not fail.
			name: "1777 sticky dir is not a fail — sticky prevents rename-in-place substitution",
			build: func(env *MapEnv) {
				env.Files["/home/user/.config/wraptool"] = MapFile{Mode: fs.ModeDir | fs.ModeSticky | 0o777}
				env.Files[configpermsPath] = MapFile{Mode: 0o600}
			},
			wantOutcome: OutcomePass,
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

// TestRunConfigPermsFileReadableSecrets_ReadableNonCredentialEnv is the
// ORIGINAL false positive (blocker 1): docs/configuration.qmd itself
// prescribes CLOUDSDK_CORE_PROJECT (an example inline env: entry, line 102)
// and an inline HOME override (line 244) in a config that may well be
// group/other-readable. Neither key name is credential-shaped, so this must
// get no finding at all — not even a warn.
func TestRunConfigPermsFileReadableSecrets_ReadableNonCredentialEnv(t *testing.T) {
	env := NewMapEnv()
	env.Files[configpermsPath] = MapFile{Mode: 0o644}
	in := configpermsInput(env, &config.Config{
		Tools: map[string]config.ToolConfig{
			"gcloud": {Env: map[string]string{"CLOUDSDK_CORE_PROJECT": "my-project", "HOME": "/work/home"}},
		},
	})

	res := runConfigPermsFileReadableSecrets(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("Outcome() = %v, want pass — CLOUDSDK_CORE_PROJECT/HOME are not credential-shaped names; "+
			"findings=%+v", res.Outcome(), res.Findings)
	}
}

// TestRunConfigPermsFileReadableSecrets_MixedCredentialAndNonCredentialEnv
// checks that a section mixing a credential-shaped key with ordinary ones
// reports only the credential-shaped key.
func TestRunConfigPermsFileReadableSecrets_MixedCredentialAndNonCredentialEnv(t *testing.T) {
	env := NewMapEnv()
	env.Files[configpermsPath] = MapFile{Mode: 0o644}
	in := configpermsInput(env, &config.Config{
		Tools: map[string]config.ToolConfig{
			"gcloud": {Env: map[string]string{
				"CLOUDSDK_CORE_PROJECT": "my-project",
				"GH_TOKEN":              "should-never-appear",
			}},
		},
	})

	res := runConfigPermsFileReadableSecrets(context.Background(), in)

	if res.Outcome() != OutcomeFinding {
		t.Fatalf("Outcome() = %v, want finding; res=%+v", res.Outcome(), res)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1; %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if strings.Contains(f.Detail, "CLOUDSDK_CORE_PROJECT") {
		t.Errorf("Detail = %q, must not name the non-credential-shaped key", f.Detail)
	}
	if !strings.Contains(f.Detail, "GH_TOKEN") {
		t.Errorf("Detail = %q, want it to name the credential-shaped key GH_TOKEN", f.Detail)
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

// ---------------------------------------------------------------- configpermsIsCredentialKey

// TestConfigPermsIsCredentialKey pins the explicit credential-shaped keyword
// set (design §7.2 ruling) directly: a whole `_`-delimited word or adjacent
// word-sequence, never a loose substring — the observed false positives
// (CLOUDSDK_CORE_PROJECT, a HOME override) are test cases here so the next
// widening of this heuristic has to argue with them, mirroring
// TestPoolIsCredentialPath's own rationale for pool.home-perms.
func TestConfigPermsIsCredentialKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// Observed false positives (blocker 1): must NOT match.
		{"CLOUDSDK_CORE_PROJECT", false},
		{"HOME", false},
		{"PATH", false},
		{"LANG", false},
		// The genuine articles.
		{"GH_TOKEN", true},
		{"TOKEN", true},
		{"API_SECRET", true},
		{"DB_PASSWORD", true},
		{"PASSWD", true},
		{"MY_CREDENTIAL", true},
		{"CREDENTIALS", true},
		{"MY_CREDENTIALS_FILE", true},
		{"API_KEY", true},
		{"MY_API_KEY_FOO", true},
		{"APIKEY", true},
		{"PRIVATE_KEY", true},
		{"ACCESS_KEY", true},
		{"SESSION_KEY", true},
		{"AUTH_TOKEN", true},
		{"BASIC_AUTH", true},
		// Case-insensitive.
		{"gh_token", true},
		// A near-miss must not match: KEY alone is not credential-shaped, and
		// "APIKEYFOO" (no underscore, not exactly APIKEY) is not either.
		{"KEY", false},
		{"APIKEYFOO", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := configpermsIsCredentialKey(tc.key); got != tc.want {
				t.Errorf("configpermsIsCredentialKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- configpermsModeString

// TestConfigPermsModeString pins the ls-matching rendering directly: type
// letter, permission bits, and the sticky bit shown as t/T in the operator-
// visible position (blocker 2 — the previous rendering dropped both).
func TestConfigPermsModeString(t *testing.T) {
	cases := []struct {
		name string
		mode fs.FileMode
		want string
	}{
		{"0700 dir", fs.ModeDir | 0o700, "0700 (drwx------)"},
		{"0775 dir", fs.ModeDir | 0o775, "0775 (drwxrwxr-x)"},
		{"1777 sticky dir (world-writable+executable other)", fs.ModeDir | fs.ModeSticky | 0o777, "1777 (drwxrwxrwt)"},
		{"1770 sticky dir (other has no execute bit)", fs.ModeDir | fs.ModeSticky | 0o770, "1770 (drwxrwx--T)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fi := FileInfo{Mode: tc.mode}
			if got := configpermsModeString(fi); got != tc.want {
				t.Errorf("configpermsModeString(%v) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}
