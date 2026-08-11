package doctor

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// mcpMetaCases pairs each of this file's Meta literals with its expected
// ID/Needs — asserted directly against the SAME literal init() registers
// (plan §1 rule 8), never through Registered(), which by the time every
// Phase-B sibling has merged also carries every one of THEIR checks.
func TestMcpMeta(t *testing.T) {
	cases := []struct {
		name string
		m    Meta
		id   CheckID
	}{
		{"command", mcpServerCommandMeta, mcpServerCommand},
		{"args-unpinned", mcpServerArgsUnpinnedMeta, mcpServerArgsUnpinned},
		{"inert", mcpServerInertMeta, mcpServerInert},
		{"dangerous-tool", mcpDangerousToolMeta, mcpDangerousTool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.m.ID != tc.id {
				t.Errorf("ID = %q, want %q", tc.m.ID, tc.id)
			}
			if tc.m.Needs != NeedsConfig {
				t.Errorf("Needs = %v, want NeedsConfig", tc.m.Needs)
			}
			if tc.m.Summary == "" {
				t.Errorf("empty Summary")
			}
		})
	}
}

// ---------------------------------------------------------------- tools.mcp-server-command

func TestRunMcpServerCommand(t *testing.T) {
	tests := []struct {
		name       string
		servers    map[string]config.MCPServerConfig
		env        func(*MapEnv)
		wantFinds  int
		wantSev    Severity
		wantDetail string
	}{
		{
			name: "absolute, exists, regular file: pass",
			servers: map[string]config.MCPServerConfig{
				"fs": {Command: "/usr/bin/mcp-fs"},
			},
			env: func(e *MapEnv) {
				e.Files["/usr/bin/mcp-fs"] = MapFile{Mode: 0o755}
			},
			wantFinds: 0,
		},
		{
			name: "relative command (npx): fail, not absolute",
			servers: map[string]config.MCPServerConfig{
				"fetch": {Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-fetch@1.0.0"}},
			},
			env:        func(*MapEnv) {},
			wantFinds:  1,
			wantSev:    SeverityFail,
			wantDetail: "non-absolute command",
		},
		{
			name: "absolute but missing: fail, does not exist",
			servers: map[string]config.MCPServerConfig{
				"gone": {Command: "/opt/bin/gone"},
			},
			env:        func(*MapEnv) {},
			wantFinds:  1,
			wantSev:    SeverityFail,
			wantDetail: "could not be found",
		},
		{
			name: "absolute, exists, but a directory: fail, not a regular file",
			servers: map[string]config.MCPServerConfig{
				"dir": {Command: "/opt/bin"},
			},
			env: func(e *MapEnv) {
				e.Files["/opt/bin"] = MapFile{Mode: fs.ModeDir | 0o755}
			},
			wantFinds:  1,
			wantSev:    SeverityFail,
			wantDetail: "non-regular file",
		},
		{
			name: "empty command: not this check's concern (config.validate owns it)",
			servers: map[string]config.MCPServerConfig{
				"empty": {Command: ""},
			},
			env:       func(*MapEnv) {},
			wantFinds: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			tc.env(env)
			in := &Input{Env: env, Config: &config.Config{MCPServers: tc.servers}}

			res := runMcpServerCommand(context.Background(), in)

			if len(res.Findings) != tc.wantFinds {
				t.Fatalf("got %d findings, want %d: %+v", len(res.Findings), tc.wantFinds, res.Findings)
			}
			if tc.wantFinds == 0 {
				if res.Outcome() != OutcomePass {
					t.Errorf("outcome = %v, want pass", res.Outcome())
				}
				return
			}
			f := res.Findings[0]
			if f.Severity != tc.wantSev {
				t.Errorf("severity = %v, want %v", f.Severity, tc.wantSev)
			}
			if !strings.Contains(f.Detail, tc.wantDetail) {
				t.Errorf("Detail %q does not contain %q", f.Detail, tc.wantDetail)
			}
			if f.Remedy == "" {
				t.Errorf("empty Remedy for a fail finding")
			}
		})
	}
}

// TestRunMcpServerCommandSortsKeys builds a multi-server fixture where two
// servers are both offending, and asserts the findings come back in the
// SORTED-by-server-name order mcpSortedServerNames imposes — the shape
// TestChecksAreDeterministic (invariants_test.go) already covers
// statistically; this pins the concrete order directly.
func TestRunMcpServerCommandSortsKeys(t *testing.T) {
	env := NewMapEnv()
	in := &Input{
		Env: env,
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"zeta":  {Command: "npx"},
			"alpha": {Command: "uvx"},
			"mid":   {Command: "/usr/bin/mcp-mid"},
		}},
	}
	env.Files["/usr/bin/mcp-mid"] = MapFile{Mode: 0o755}

	res := runMcpServerCommand(context.Background(), in)

	if len(res.Findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(res.Findings), res.Findings)
	}
	if res.Findings[0].Path.String() != At("mcp_servers", "alpha", "command").String() {
		t.Errorf("finding[0].Path = %q, want alpha first (sorted)", res.Findings[0].Path.String())
	}
	if res.Findings[1].Path.String() != At("mcp_servers", "zeta", "command").String() {
		t.Errorf("finding[1].Path = %q, want zeta second (sorted)", res.Findings[1].Path.String())
	}
}

// ---------------------------------------------------------------- mcpSpecPinned / mcpUnpinnedSpec

func TestMcpSpecPinned(t *testing.T) {
	tests := []struct {
		spec string
		want bool
	}{
		{"left-pad@1.3.0", true},
		{"left-pad@latest", false},
		{"left-pad@LATEST", false}, // case-insensitive
		{"left-pad", false},
		{"@modelcontextprotocol/server-filesystem", false},      // scope marker only, no version
		{"@modelcontextprotocol/server-filesystem@2.0.1", true}, // scoped, pinned
		{"@modelcontextprotocol/server-filesystem@next", false}, // scoped, floating tag
		{"ruff@0.4.4", true},
		{"", false},
		// PEP 440 (uv/pip) exact-pin form — uvx's own documented pinning
		// syntax, distinct from npm's "@version" (ruling B7(a)).
		{"mcp-server-git==2024.11.5", true},
		{"mcp-server-git>=1.0", false}, // range operator, not an exact pin
		{"mcp-server-git==", false},    // "==" with no version after it
		{"==2024.11.5", false},         // "==" with no package name before it
	}
	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			if got := mcpSpecPinned(tc.spec); got != tc.want {
				t.Errorf("mcpSpecPinned(%q) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- mcpSpecArg

// TestMcpSpecArg exercises ruling B7(b): an explicit spec flag's VALUE is
// the package spec, not the trailing "bin name" positional uvx's "--from
// PKG CMD" form carries, and not some earlier, unrelated flag's own value —
// in both the split "--from X" and joined "--from=X" spellings. It also
// exercises ruling B7-1: the fallback positional scan is conservative, and
// each runner's spec flags are recognized only for that runner (uv's own
// "-p"/"--python" short flag must never be misread as npx's "-p"/"--package"
// spec flag, the same false-positive class as the "--python"/"--with" repro).
func TestMcpSpecArg(t *testing.T) {
	tests := []struct {
		name      string
		base      string
		args      []string
		wantSpec  string
		wantFound bool
	}{
		{
			name:      "uvx --from value, distinct trailing bin name",
			base:      "uvx",
			args:      []string{"--from", "black==24.4.2", "black"},
			wantSpec:  "black==24.4.2",
			wantFound: true,
		},
		{
			name:      "uvx --from value unpinned, distinct trailing bin name",
			base:      "uvx",
			args:      []string{"--from", "black", "black-cli"},
			wantSpec:  "black",
			wantFound: true,
		},
		{
			name:      "uvx --from=value joined spelling",
			base:      "uvx",
			args:      []string{"--from=mcp-server-git==2024.11.5", "mcp-server-git"},
			wantSpec:  "mcp-server-git==2024.11.5",
			wantFound: true,
		},
		{
			name:      "npx -p value, distinct trailing bin name",
			base:      "npx",
			args:      []string{"-p", "left-pad@1.3.0", "left-pad-cli"},
			wantSpec:  "left-pad@1.3.0",
			wantFound: true,
		},
		{
			name:      "npx --package=value joined spelling",
			base:      "npx",
			args:      []string{"--package=left-pad@1.3.0", "left-pad-cli"},
			wantSpec:  "left-pad@1.3.0",
			wantFound: true,
		},
		{
			name:      "no spec-naming flag: falls back to first non-flag positional",
			base:      "npx",
			args:      []string{"-y", "left-pad@1.3.0"},
			wantSpec:  "left-pad@1.3.0",
			wantFound: true,
		},
		{
			name:      "--from with no value at all: not found, no guess",
			base:      "uvx",
			args:      []string{"--from"},
			wantSpec:  "",
			wantFound: false,
		},
		// Ruling B7-1's own repro: uvx's documented `uvx --python 3.10 ruff`
		// form must never yield "3.10" as the spec.
		{
			name:      "uvx --python value ahead of positional: unidentifiable, no guess",
			base:      "uvx",
			args:      []string{"--python", "3.12", "mcp-server-git==2024.11.5"},
			wantSpec:  "",
			wantFound: false,
		},
		{
			name:      "uvx --with value ahead of positional: unidentifiable, no guess",
			base:      "uvx",
			args:      []string{"--with", "requests", "ruff"},
			wantSpec:  "",
			wantFound: false,
		},
		// uv's own "-p" short form for "--python" must not be misread as
		// npx's "-p" spec flag (mcpRunnerSpecFlags is per-runner).
		{
			name:      "uvx -p (python short flag, not npx's package flag): unidentifiable, no guess",
			base:      "uvx",
			args:      []string{"-p", "3.12", "ruff"},
			wantSpec:  "",
			wantFound: false,
		},
		{
			name:      "uvx --python=value joined spelling ahead of positional: skipped, positional found",
			base:      "uvx",
			args:      []string{"--python=3.12", "ruff"},
			wantSpec:  "ruff",
			wantFound: true,
		},
		{
			name:      "npx --with value ahead of positional: unidentifiable, no guess",
			base:      "npx",
			args:      []string{"--with", "typescript", "left-pad"},
			wantSpec:  "",
			wantFound: false,
		},
		{
			name:      "uvx known valueless flags ahead of positional: skipped",
			base:      "uvx",
			args:      []string{"--refresh", "--no-cache", "--isolated", "--offline", "--quiet", "--native-tls", "ruff"},
			wantSpec:  "ruff",
			wantFound: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, found := mcpSpecArg(tc.base, tc.args)
			if found != tc.wantFound || spec != tc.wantSpec {
				t.Errorf("mcpSpecArg(%q, %v) = (%q, %v), want (%q, %v)", tc.base, tc.args, spec, found, tc.wantSpec, tc.wantFound)
			}
		})
	}
}

func TestMcpUnpinnedSpec(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		args      []string
		wantSpec  string
		wantFound bool
	}{
		{"npx with -y and unpinned: fires", "npx", []string{"-y", "@foo/bar"}, "@foo/bar", true},
		{"npx with --yes and unpinned: fires", "npx", []string{"--yes", "left-pad"}, "left-pad", true},
		{"npx with -y and pinned: does not fire", "npx", []string{"-y", "left-pad@1.3.0"}, "", false},
		// Ruling B7-2: npx with NO -y/--yes at all still fires. Measured
		// against the built binary with stdin=/dev/null (internal/mcpproxy's
		// own spawn shape, no TTY): `npx cowsay hi </dev/null` printed npm's
		// install warning and RAN with no prompt — the old "-y gate" premise
		// ("would block on a confirmation prompt") is false, and this is the
		// exact reported false-NEGATIVE (a silently-passing dangerous config).
		{"npx WITHOUT -y/--yes and unpinned: still fires (no TTY, no prompt to block on)", "npx", []string{"@foo/bar"}, "@foo/bar", true},
		{"npx without -y, pinned: does not fire", "npx", []string{"left-pad@1.3.0"}, "", false},
		{"uvx unpinned, no flag needed: fires", "uvx", []string{"ruff"}, "ruff", true},
		{"uvx pinned: does not fire", "uvx", []string{"ruff@0.4.4"}, "", false},
		{"absolute path to npx still matches by basename", "/usr/local/bin/npx", []string{"-y", "left-pad"}, "left-pad", true},
		{"unrelated runner: never fires", "node", []string{"-y", "left-pad"}, "", false},
		{"npx with -y but no non-flag arg: does not fire", "npx", []string{"-y"}, "", false},
		// Ruling B7-1's own repro (uvx's documented `uvx --python 3.10 ruff`
		// form): a value-taking flag this check does not model must never
		// hand back its own value as the spec.
		{
			"uvx --python value ahead of positional spec: does not fire (unidentifiable, not evidence of unpinned)",
			"uvx", []string{"--python", "3.12", "mcp-server-git==2024.11.5"}, "", false,
		},
		{
			"uvx --with value ahead of positional spec: does not fire (unidentifiable)",
			"uvx", []string{"--with", "requests", "ruff"}, "", false,
		},
		// Ruling B7: uv's documented pin form (PEP 440 "==") must not be
		// misread as unpinned just because it carries no npm-style "@".
		{
			"uvx --from with a PEP 440 pin (uv's documented pinning form): does not fire",
			"uvx", []string{"--from", "mcp-server-git==2024.11.5", "mcp-server-git"}, "", false,
		},
		// The --from flag's VALUE is the spec, not the trailing bin name —
		// and firing must report the --from value, not the bin name, when
		// that value itself is unpinned.
		{
			"uvx --from unpinned, distinct trailing bin name: fires on the --from value",
			"uvx", []string{"--from", "mcp-server-git", "mcp-server-git-cli"}, "mcp-server-git", true,
		},
		// The --flag=value joined spelling must be parsed the same as the
		// split "--from X" form.
		{
			"uvx --from=value joined spelling, pinned: does not fire",
			"uvx", []string{"--from=mcp-server-git==2024.11.5", "mcp-server-git"}, "", false,
		},
		{
			"uvx --from=value joined spelling, unpinned: fires",
			"uvx", []string{"--from=mcp-server-git", "mcp-server-git"}, "mcp-server-git", true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, found := mcpUnpinnedSpec(tc.command, tc.args)
			if found != tc.wantFound || spec != tc.wantSpec {
				t.Errorf("mcpUnpinnedSpec(%q, %v) = (%q, %v), want (%q, %v)",
					tc.command, tc.args, spec, found, tc.wantSpec, tc.wantFound)
			}
		})
	}
}

func TestRunMcpServerArgsUnpinned(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"fetch":  {Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-fetch"}},
			"pinned": {Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-fetch@1.2.0"}},
			"local":  {Command: "/opt/bin/mcp-local"},
		}},
	}

	res := runMcpServerArgsUnpinned(context.Background(), in)

	if len(res.Findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if f.Severity != SeverityFail {
		t.Errorf("severity = %v, want fail", f.Severity)
	}
	if !strings.Contains(f.Detail, "fetch") || !strings.Contains(f.Detail, "@modelcontextprotocol/server-fetch") {
		t.Errorf("Detail %q does not name the offending server/spec", f.Detail)
	}
	wantPath := At("mcp_servers", "fetch", "args").Elem("@modelcontextprotocol/server-fetch").String()
	if f.Path.String() != wantPath {
		t.Errorf("Path = %q, want %q", f.Path.String(), wantPath)
	}
}

// TestRunMcpServerArgsUnpinnedPinnedUvxDoesNotFire is the exact reported
// false-positive shape (ruling B7): `uvx --from mcp-server-git==2024.11.5
// mcp-server-git` is correctly pinned per PEP 440 and must not fire at all —
// before this fix it produced a fail Finding with the invalid remedy
// "mcp-server-git==2024.11.5@<version>".
func TestRunMcpServerArgsUnpinnedPinnedUvxDoesNotFire(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"git": {
				Command: "/usr/bin/uvx",
				Args:    []string{"--from", "mcp-server-git==2024.11.5", "mcp-server-git"},
			},
		}},
	}

	res := runMcpServerArgsUnpinned(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("outcome = %v, want pass: %+v", res.Outcome(), res.Findings)
	}
}

// TestRunMcpServerArgsUnpinnedUvxRemedyUsesPEP440Syntax asserts ruling
// B7(c): a genuinely unpinned uvx spec's remedy suggests uv/pip's own
// "==<version>" syntax, never npm's "@<version>" (which would compose an
// invalid spec once the package name already carries a PEP 440 marker
// elsewhere, and is simply the wrong ecosystem's syntax for uvx regardless).
func TestRunMcpServerArgsUnpinnedUvxRemedyUsesPEP440Syntax(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"git": {
				Command: "/usr/bin/uvx",
				Args:    []string{"--from", "mcp-server-git", "mcp-server-git"},
			},
		}},
	}

	res := runMcpServerArgsUnpinned(context.Background(), in)

	if len(res.Findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if !strings.Contains(f.Remedy, "mcp-server-git==<version>") {
		t.Errorf("Remedy %q does not suggest PEP 440 pin syntax", f.Remedy)
	}
	if strings.Contains(f.Remedy, "@<version>") {
		t.Errorf("Remedy %q wrongly suggests npm pin syntax for a uvx invocation", f.Remedy)
	}
}

// TestRunMcpServerArgsUnpinnedNpxWithoutYesStillFires is the exact reported
// false-NEGATIVE shape (ruling B7-2): an npx invocation with NO -y/--yes at
// all used to silently pass this check on the premise that npx would block
// on a confirmation prompt no spawned child could answer. That premise is
// false — measured against the built binary with stdin=/dev/null
// (internal/mcpproxy's own spawn shape): `npx cowsay hi </dev/null` printed
// npm's install warning and RAN, no prompt. An unpinned npx server with no
// -y fetches and executes remote code at every serve start exactly like one
// that carries -y, so this must fire, and the Detail must say why no TTY
// changes nothing.
func TestRunMcpServerArgsUnpinnedNpxWithoutYesStillFires(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"fetch": {Command: "/usr/bin/npx", Args: []string{"@modelcontextprotocol/server-fetch"}},
		}},
	}

	res := runMcpServerArgsUnpinned(context.Background(), in)

	if len(res.Findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if f.Severity != SeverityFail {
		t.Errorf("severity = %v, want fail", f.Severity)
	}
	if !strings.Contains(f.Detail, "non-interactively") || !strings.Contains(f.Detail, "no TTY") {
		t.Errorf("Detail %q does not explain that npm proceeds non-interactively with no TTY", f.Detail)
	}
}

// TestRunMcpServerArgsUnpinnedUnidentifiableSpecDoesNotFire is the exact
// reported false-POSITIVE shape (ruling B7-1), reproduced against the built
// binary: `uvx --python 3.12 mcp-server-git==2024.11.5` — uv's own
// documented `uvx --python 3.10 ruff` form — used to fire with package spec
// "3.12" (the --python VALUE, not the actual spec) and remedy
// "3.12==<version>". A value-taking flag this check does not model must
// never be mistaken for the spec; an unidentifiable spec is not evidence of
// an unpinned one, so this must emit NOTHING.
func TestRunMcpServerArgsUnpinnedUnidentifiableSpecDoesNotFire(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"git": {
				Command: "/usr/bin/uvx",
				Args:    []string{"--python", "3.12", "mcp-server-git==2024.11.5"},
			},
			"lint": {
				Command: "/usr/bin/uvx",
				Args:    []string{"--with", "requests", "ruff"},
			},
		}},
	}

	res := runMcpServerArgsUnpinned(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("outcome = %v, want pass: %+v", res.Outcome(), res.Findings)
	}
}

// ---------------------------------------------------------------- tools.mcp-server-inert

func TestRunMcpServerInert(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"inert":  {Command: "/opt/bin/inert"},
			"active": {Command: "/opt/bin/active", Allow: []config.MCPAllowRule{{Tool: "read_file"}}},
		}},
	}

	res := runMcpServerInert(context.Background(), in)

	if len(res.Findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %v, want warn", f.Severity)
	}
	if !strings.Contains(f.Detail, "inert") {
		t.Errorf("Detail %q does not name the inert server", f.Detail)
	}
	wantPath := At("mcp_servers", "inert").String()
	if f.Path.String() != wantPath {
		t.Errorf("Path = %q, want %q", f.Path.String(), wantPath)
	}
}

func TestRunMcpServerInertAllGranted(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"active": {Command: "/opt/bin/active", Allow: []config.MCPAllowRule{{Tool: "read_file"}}},
		}},
	}

	res := runMcpServerInert(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Errorf("outcome = %v, want pass", res.Outcome())
	}
}

// ---------------------------------------------------------------- policy.mcp-dangerous-tool

func TestMcpDangerousShape(t *testing.T) {
	tests := []struct {
		tool      string
		wantShape string
		wantMatch bool
	}{
		{"read_file", "", false},
		{"list_issues", "", false},
		{"execute_command", "execute", true}, // matches the first substring in list order
		{"run_shell_command", "command", true},
		{"write_file", "write_file", true},
		{"delete_repo", "delete", true},
		{"SHELL_exec", "shell", true}, // case-insensitive
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			shape, match := mcpDangerousShape(tc.tool)
			if match != tc.wantMatch {
				t.Errorf("mcpDangerousShape(%q) match = %v, want %v", tc.tool, match, tc.wantMatch)
			}
			if match && shape != tc.wantShape {
				t.Errorf("mcpDangerousShape(%q) shape = %q, want %q", tc.tool, shape, tc.wantShape)
			}
		})
	}
}

func TestRunMcpDangerousTool(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"github": {
				Command: "/opt/bin/mcp-github",
				Allow: []config.MCPAllowRule{
					{Tool: "list_issues"},
					{Tool: "execute_workflow"},
				},
			},
			"fs": {
				Command: "/opt/bin/mcp-fs",
				Allow:   []config.MCPAllowRule{{Tool: "write_file"}},
			},
		}},
	}

	res := runMcpDangerousTool(context.Background(), in)

	if len(res.Findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(res.Findings), res.Findings)
	}
	// Sorted by server name: "fs" (write_file) before "github" (execute_workflow).
	if res.Findings[0].Path.String() != At("mcp_servers", "fs", "allow").Item("tool", "write_file").String() {
		t.Errorf("finding[0].Path = %q, want fs/write_file first (sorted)", res.Findings[0].Path.String())
	}
	if res.Findings[1].Path.String() != At("mcp_servers", "github", "allow").Item("tool", "execute_workflow").String() {
		t.Errorf("finding[1].Path = %q, want github/execute_workflow second", res.Findings[1].Path.String())
	}
	for _, f := range res.Findings {
		if f.Severity != SeverityWarn {
			t.Errorf("severity = %v, want warn", f.Severity)
		}
	}
}

func TestRunMcpDangerousToolNoneMatch(t *testing.T) {
	in := &Input{
		Env: NewMapEnv(),
		Config: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"github": {
				Command: "/opt/bin/mcp-github",
				Allow:   []config.MCPAllowRule{{Tool: "list_issues"}, {Tool: "read_file"}},
			},
		}},
	}

	res := runMcpDangerousTool(context.Background(), in)

	if res.Outcome() != OutcomePass {
		t.Fatalf("outcome = %v, want pass: %+v", res.Outcome(), res.Findings)
	}
}

// TestMcpSpecArgStopsAtTheFirstPositional pins the bound that makes the
// spec scan correct rather than merely fixed: arguments after the spec belong
// to the spawned SERVER, so a spec flag appearing among them is the child's,
// not the runner's. This defect was closed twice before — once for the
// positional scan, once for the flag scan — and relocated each time.
func TestMcpSpecArgStopsAtTheFirstPositional(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		args     []string
		wantSpec string
		wantOK   bool
	}{
		{"child flag after the spec is not the spec", "npx", []string{"-y", "pkg@1.0.0", "--package", "child-arg"}, "pkg@1.0.0", true},
		{"uvx --from wins ahead of the bin name", "uvx", []string{"--from", "mcp-server-git==1.0", "mcp-server-git"}, "mcp-server-git==1.0", true},
		{"uvx --python value is not the spec", "uvx", []string{"--python", "3.12", "mcp-server-git==2024.11.5"}, "", false},
		{"npx unknown flag ahead of the positional", "npx", []string{"--unknown-thing", "value", "pkg"}, "", false},
		{"joined spelling keeps its value with its flag", "uvx", []string{"--python=3.12", "mcp-server-git==1.0"}, "mcp-server-git==1.0", true},
		{"joined spec flag yields its value", "uvx", []string{"--from=pkg==1.0", "bin"}, "pkg==1.0", true},
		{"double dash ends the runner's own arguments", "npx", []string{"--", "pkg"}, "", false},
		{"bare positional is the spec", "npx", []string{"-y", "@scope/name@1.2.3"}, "@scope/name@1.2.3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := mcpSpecArg(tc.base, tc.args)
			if spec != tc.wantSpec || ok != tc.wantOK {
				t.Errorf("mcpSpecArg(%q, %v) = (%q, %v), want (%q, %v)", tc.base, tc.args, spec, ok, tc.wantSpec, tc.wantOK)
			}
		})
	}
}

// TestMcpSpecPinnedRejectsRanges: an npm range resolves to whatever the
// registry serves at that moment, which is the moving target "@latest" names.
// The dist-tag list alone let every one of these through.
func TestMcpSpecPinnedRejectsRanges(t *testing.T) {
	unpinned := []string{"@foo/bar@*", "@foo/bar@>=1.0.0", "@foo/bar@~1.2", "@foo/bar@^1.2.3", "@foo/bar@1.x", "@foo/bar@latest"}
	for _, spec := range unpinned {
		if mcpSpecPinned(spec) {
			t.Errorf("mcpSpecPinned(%q) = true, want false (a range is not a pin)", spec)
		}
	}
	for _, spec := range []string{"@foo/bar@1.2.3", "pkg@0.0.1", "mcp-server-git==2024.11.5"} {
		if !mcpSpecPinned(spec) {
			t.Errorf("mcpSpecPinned(%q) = false, want true", spec)
		}
	}
}

// TestMcpUnpinnedSpecLocalAndWindows covers the two shapes that made the check
// say something false and say nothing at all, respectively.
func TestMcpUnpinnedSpecLocalAndWindows(t *testing.T) {
	// A local path contacts no registry, so the finding's central claim would
	// be false of it.
	for _, spec := range []string{"./local-server", "/opt/server", "~/src/server"} {
		if _, unpinned := mcpUnpinnedSpec("/usr/bin/npx", []string{"-y", spec}); unpinned {
			t.Errorf("mcpUnpinnedSpec(npx, %q) reported unpinned; a local path fetches nothing", spec)
		}
	}
	// On Windows the runner IS npx.cmd, and this check declares no
	// NeedsModeBits, so without the suffix strip it silently passes there.
	for _, cmd := range []string{`C:\npm\npx.cmd`, "/usr/bin/npx.exe", `C:\npm\NPX.CMD`, "/usr/bin/uvx.exe"} {
		if _, unpinned := mcpUnpinnedSpec(cmd, []string{"@foo/bar"}); !unpinned {
			t.Errorf("mcpUnpinnedSpec(%q, unpinned spec) reported nothing; the runner name was not recognized", cmd)
		}
	}
}
