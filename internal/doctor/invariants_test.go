package doctor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/config"
)

// TestRegistryIsWellFormed asserts the properties every registered check must
// have, independent of which Phase-B files happen to exist yet. This is what
// makes the seam a seam: nine future authors are covered by this one test.
func TestRegistryIsWellFormed(t *testing.T) {
	if err := RegistryErr(); err != nil {
		t.Fatalf("RegistryErr() = %v, want nil", err)
	}
	seen := map[CheckID]bool{}
	for _, c := range Registered() {
		m := c.Meta()
		if seen[m.ID] {
			t.Errorf("duplicate ID in Registered(): %q", m.ID)
		}
		seen[m.ID] = true
		if !idPattern.MatchString(string(m.ID)) {
			t.Errorf("%q does not match the catalog ID pattern", m.ID)
		}
		if !knownGroup(m.ID.Group()) {
			t.Errorf("%q has unknown group %q", m.ID, m.ID.Group())
		}
		if strings.TrimSpace(m.Summary) == "" {
			t.Errorf("%q has an empty Meta.Summary", m.ID)
		}
	}
}

// invariantsInputCases is the pair every registry-wide invariant test runs
// against: a virgin host (no config yet, but the real HostPaths a host
// always has — see invariantsVirginInput) AND a fully-populated host (every
// optional Requirement bit satisfied, invariantsFullyPopulatedInput). Six of
// nine Phase-B groups are config-gated (Meta.Needs&NeedsConfig != 0): running
// ONLY the virgin case would leave them permanently untested by these
// invariants. Both cases gate check selection on the same provides()/Needs
// test the engine itself uses (invariantsChecksSatisfying), so a check is
// only ever invoked here under an Input it actually declared it could
// handle — the engine's own documented guarantee (Run: "a check whose Needs
// are unmet is never invoked") applies just as much to a test calling c.Run
// directly as to Run itself.
func invariantsInputCases() []struct {
	name string
	in   *Input
} {
	return []struct {
		name string
		in   *Input
	}{
		{"virgin", invariantsVirginInput()},
		{"fully-populated", invariantsFullyPopulatedInput()},
	}
}

// invariantsHostPaths is the HostPaths literal shared by both invariant
// cases: a real host, virgin or not, always has these (cmd/doctor.go
// populates Paths unconditionally, before ever looking at ConfigState) —
// only ConfigState/Config distinguish "no config yet" from "fully set up".
// A check gated on NeedsPoolVerify with no other config dependency (§7.9's
// pool.generation-digest) resolves its own pool.PoolPaths from
// Input.Paths.PoolRoot; an empty PoolRoot there would silently resolve a
// root-parameterized pool call to a RELATIVE path and read the real
// filesystem from underneath a MapEnv-backed unit test.
var invariantsHostPaths = HostPaths{
	ConfigDir:  "/home/user/.config/wraptool",
	DataDir:    "/home/user/.local/share/wraptool",
	CacheDir:   "/home/user/.cache/wraptool",
	RuntimeDir: "/run/user/1000/wraptool",
	PoolRoot:   "/home/user/.local/share/wraptool/harness-pool",
}

// invariantsVirginInput is a host before any config exists — "virgin" means
// no config, never no paths.
func invariantsVirginInput() *Input {
	return &Input{
		Env:        NewMapEnv(),
		GOOS:       "linux",
		Paths:      invariantsHostPaths,
		ConfigPath: "/home/user/.config/wraptool/config.yaml",
	}
}

// invariantsFullyPopulatedInput satisfies every Requirement bit doctor.go's
// provides() knows about, so a config-gated (or workspace-gated, probe-gated,
// pool-verify-gated) check gets exercised by the registry-wide invariants too
// — not just the checks that happen to need nothing. Config carries three
// tools and two mcp_servers entries (a 2-key map's two ranges agree ~78% of
// the time and a 3-key map's ~59%, so entry count alone cannot make
// TestChecksAreDeterministic reliable — invariantsDeterminismRuns is what
// does), a non-loopback mcp.listen and an auth_token_file: a config-gated
// check that ranges over cfg.Tools/cfg.MCPServers without sorting keys, or
// that only reaches its real logic past a non-empty-listen/non-empty-token
// branch, needs more than one entry and a non-default listener to ever be
// exercised at all.
func invariantsFullyPopulatedInput() *Input {
	return &Input{
		Env:   NewMapEnv(),
		GOOS:  "linux",
		Paths: invariantsHostPaths,
		Config: &config.Config{
			MCP: config.MCPConfig{
				Listen:        "0.0.0.0:8717",
				AuthTokenFile: "/home/user/.config/wraptool/mcp-token",
			},
			Tools: map[string]config.ToolConfig{
				"git": {
					Binary: "/usr/bin/git",
					Allow:  []config.AllowRule{{Subcommand: []string{"pull"}}},
				},
				"kubectl": {
					Binary: "/usr/bin/kubectl",
					Allow:  []config.AllowRule{{Subcommand: []string{"get", "pods"}}},
				},
				"gcloud": {
					Binary: "/usr/bin/gcloud",
					Allow:  []config.AllowRule{{Subcommand: []string{"compute", "instances", "list"}}},
				},
			},
			MCPServers: map[string]config.MCPServerConfig{
				"filesystem": {
					Command: "/usr/bin/mcp-filesystem",
					Allow:   []config.MCPAllowRule{{Tool: "read_file"}},
				},
				"github": {
					Command: "/usr/bin/mcp-github",
					Allow:   []config.MCPAllowRule{{Tool: "list_issues"}},
				},
			},
		},
		ConfigPath:  "/home/user/.config/wraptool/config.yaml",
		ConfigState: ConfigLoaded,
		Workspace:   "/home/user/project",
		Probe:       true,
		VerifyPool:  true,
	}
}

// invariantsChecksSatisfying returns Registered() filtered to the checks
// in's Input can actually satisfy — the same gate doctor.Run applies via
// provides()/Needs.
func invariantsChecksSatisfying(in *Input) []Check {
	have := provides(in)
	var out []Check
	for _, c := range Registered() {
		if c.Meta().Needs&^have == 0 {
			out = append(out, c)
		}
	}
	return out
}

// TestChecksSurviveAVirginHost runs every registered check whose Needs it can
// satisfy against BOTH a virgin host and a fully-populated one
// (invariantsInputCases), so no check is ever invoked under an Input it did
// not declare it could handle, and no check escapes coverage just because it
// happens to be config/workspace/probe/pool-gated.
func TestChecksSurviveAVirginHost(t *testing.T) {
	for _, tc := range invariantsInputCases() {
		for _, c := range invariantsChecksSatisfying(tc.in) {
			m := c.Meta()
			t.Run(tc.name+"/"+string(m.ID), func(t *testing.T) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("check %q panicked: %v", m.ID, p)
					}
				}()
				res := c.Run(context.Background(), tc.in)
				if res.Fatal != nil {
					t.Errorf("check %q returned Fatal: %v", m.ID, res.Fatal)
				}
				if reason := validateResult(m.ID, res); reason != "" {
					t.Errorf("check %q returned an invalid Result: %s", m.ID, reason)
				}
			})
		}
	}
}

// invariantsDeterminismRuns is how many times TestChecksAreDeterministic runs
// each check. Two is not enough: two independent ranges over the fixture's
// 3-key map happen to agree ~59% of the time, so an unsorted `range cfg.Tools`
// would pass CI more often than not. At eight runs the miss probability is
// under 1%.
const invariantsDeterminismRuns = 8

// TestChecksAreDeterministic runs each registered check (whose Needs the Input
// satisfies) repeatedly against the same Input and requires an identical
// Result every time. Catches the likeliest defect class: ranging over a Go map
// (e.g. cfg.Tools) without sorting keys — which needs >= 2 map entries to be
// able to fail at all, hence invariantsFullyPopulatedInput's Tools/MCPServers.
func TestChecksAreDeterministic(t *testing.T) {
	for _, tc := range invariantsInputCases() {
		for _, c := range invariantsChecksSatisfying(tc.in) {
			m := c.Meta()
			t.Run(tc.name+"/"+string(m.ID), func(t *testing.T) {
				first := c.Run(context.Background(), tc.in)
				for i := 1; i < invariantsDeterminismRuns; i++ {
					again := c.Run(context.Background(), tc.in)
					if !reflect.DeepEqual(first, again) {
						t.Fatalf("check %q is non-deterministic (run 1 vs run %d):\nrun 1: %+v\nrun %d: %+v", m.ID, i+1, first, i+1, again)
					}
				}
			})
		}
	}
}

// TestFindingsAreWellFormed checks the shape of every finding a registered
// check emits directly (i.e. before the engine's Location/ID stamping), under
// both invariantsInputCases: valid Severity, non-empty Title/Detail, a
// Remedy for fail/warn, never both Path and Location set by the check itself
// (RULING R5 — that field is engine-owned for config findings), and — plan §1
// rule 9 — a check must never hand-roll a config Location by echoing
// in.ConfigPath into it; it must set Path so C1 can resolve a real file:line.
func TestFindingsAreWellFormed(t *testing.T) {
	for _, tc := range invariantsInputCases() {
		for _, c := range invariantsChecksSatisfying(tc.in) {
			m := c.Meta()
			res := c.Run(context.Background(), tc.in)
			for i, f := range res.Findings {
				if !f.Severity.Valid() {
					t.Errorf("%q finding[%d]: invalid severity %q", m.ID, i, f.Severity)
				}
				if strings.TrimSpace(f.Title) == "" {
					t.Errorf("%q finding[%d]: empty Title", m.ID, i)
				}
				if strings.TrimSpace(f.Detail) == "" {
					t.Errorf("%q finding[%d]: empty Detail", m.ID, i)
				}
				if (f.Severity == SeverityFail || f.Severity == SeverityWarn) && f.Remedy == "" {
					t.Errorf("%q finding[%d]: severity %s requires a Remedy", m.ID, i, f.Severity)
				}
				if !f.Path.Empty() && f.Location != "" {
					t.Errorf("%q finding[%d]: a check must set Path or Location, never both (RULING R5)", m.ID, i)
				}
				if f.Path.Empty() && f.Location != "" && tc.in.ConfigPath != "" &&
					strings.HasPrefix(f.Location, tc.in.ConfigPath) {
					t.Errorf("%q finding[%d]: Location %q looks like a hand-rolled config path — set Path instead so C1 can retrofit a real file:line (plan §1 rule 9)", m.ID, i, f.Location)
				}
			}
		}
	}
}

// invariantsHostAllowlist names the files permitted to touch the real host
// directly. Every other file in this package must go through Env.
var invariantsHostAllowlist = map[string]bool{
	"host.go":            true,
	"host_stat_unix.go":  true,
	"host_stat_other.go": true,
	"mapenv.go":          true,
}

// invariantsForbiddenHostSelectors: package identifier -> selector names
// forbidden on that package. A nil/empty slice means every selector on that
// package is forbidden.
//
// The "pool" entries are internal/pool's zero-argument, HOST-ROOTED layout
// accessors (internal/pool/layout.go): every one of them resolves through
// paths.DataDir() directly, bypassing Input.Paths.PoolRoot entirely, so
// calling e.g. pool.HomeDir() from a check reads the REAL host's
// $XDG_DATA_HOME even under a MapEnv-backed unit test and this guard test
// still passes unless the selector itself is named here. Deliberately NOT
// listed: pool.LegacyRemovedMarkerFileAt (takes an explicit root) and every
// root-parameterized pool function — ActiveGeneration/GenerationVersion (take
// a pool.PoolPaths) and VerifyGenerationTree (takes an explicit root string)
// — those ARE root-parameterized and are exactly how B8 is meant to reach the
// pool, building its own pool.PoolPaths from Input.Paths.PoolRoot.
var invariantsForbiddenHostSelectors = map[string][]string{
	"os":      nil,
	"exec":    nil,
	"syscall": nil,
	"paths":   nil,
	"time":    {"Now"},
	"filepath": {
		"EvalSymlinks",
	},
	"pool": {
		"Root", "ManifestFile", "LockFile", "ObservationsFile", "AuditFile",
		"LegacyRemovedMarkerFile", "CacheDir", "CacheSHA256Dir", "CacheObjectPath",
		"RuntimeDir", "RuntimeBinDir", "RuntimeBinFile", "RuntimeActiveDir",
		"RuntimeActivePlatformDir", "PointerFile", "RuntimeGenerationsDir",
		"RuntimeGenerationDir", "HomeDir", "StagingDir", "StagingTxnDir",
		"AdvisoryLockFile",
	},
}

// TestChecksDoNotTouchTheRealHost statically rejects direct host access
// (os./exec./syscall./paths./time.Now/filepath.EvalSymlinks/pool's zero-arg
// layout accessors) outside the allowlisted files, so a check can only reach
// the host through Env (or a root-parameterized pool call) — which is what
// makes MapEnv-backed unit tests trustworthy for every future check. It does
// NOT see through a check that instead calls a sibling package's own
// host-touching function (config.LoadStrict/Validate, introspect.CacheStamp)
// — host.go's Env doc comment names that sanctioned, but untestable-by-this-
// guard, exception.
func TestChecksDoNotTouchTheRealHost(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if invariantsHostAllowlist[name] {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			names, forbidden := invariantsForbiddenHostSelectors[id.Name]
			if !forbidden {
				return true
			}
			if len(names) == 0 || slices.Contains(names, sel.Sel.Name) {
				pos := fset.Position(sel.Pos())
				t.Errorf("%s: direct host access %s.%s — must go through Env (or a root-parameterized pool call)", pos, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// invariantsFoundationFiles are A1/A2's own seam files, plus the two DATA-only
// files the plan's file map calls out by name — none of these is a Phase-B/
// C/D check group, so none carries a per-group identifier prefix and none is
// scanned by TestGroupFileIdentifiersArePrefixedWithFileGroup:
//
//   - finding.go, doctor.go, render_human.go, host.go, host_stat_unix.go,
//     host_stat_other.go, mapenv.go: the engine/type seam itself.
//   - render_json.go (A2): the --json report renderer, a sibling of
//     render_human.go, not a check group.
//   - toolknowledge.go (D0): "table shape + data, no checks yet" per plan §2 —
//     a lookup table, not a file that calls register().
var invariantsFoundationFiles = map[string]bool{
	"finding.go":         true,
	"doctor.go":          true,
	"render_human.go":    true,
	"render_json.go":     true,
	"host.go":            true,
	"host_stat_unix.go":  true,
	"host_stat_other.go": true,
	"mapenv.go":          true,
	"toolknowledge.go":   true,
}

// invariantsGroupStem returns the group-identifying stem a group file shares
// with its own test file: the .go suffix and a trailing _test removed, so
// "tools.go" and "tools_test.go" (and "invariants_test.go" itself) resolve to
// the same stem and therefore the same required prefix — this is what makes
// plan §1 rule 7 cover a step's own test file at zero extra cost to the
// author: name your test file the normal way and it is already covered.
func invariantsGroupStem(filename string) string {
	stem := strings.TrimSuffix(filename, ".go")
	stem = strings.TrimSuffix(stem, "_test")
	return stem
}

// invariantsFilePrefix derives the MECHANICAL prefix for a group stem: the
// WHOLE stem with its `_` separators removed ("env" -> "env", "config_load"
// -> "configload", "exposure_listen" -> "exposurelisten"). Using the whole
// stem rather than its first segment is what makes rule 7 mechanical for the
// case it exists to prevent: exposure_listen.go and exposure_files.go land in
// the same parallel wave, and a shared coarse "exposure" prefix would let both
// authors write exposureInput() — each branch green, the merge commit broken.
// Requiring the full stem rejects that on each author's own branch instead.
func invariantsFilePrefix(stem string) string {
	return strings.ReplaceAll(stem, "_", "")
}

// invariantsFilePrefixOverrides names the stems (see invariantsGroupStem)
// whose natural, idiomatic Go identifiers do not literally contain the
// mechanical invariantsFilePrefix derivation: a plural file name with
// singular helpers (tools.go's runToolBinaryExecutable), a compound name
// whose real group is its shorter head noun (mcpservers.go's
// mcpServerCommand). Every stem here is drawn verbatim from plan §2's file
// map, so a Phase-B/C/D author never edits this table themselves — their
// file's entry already exists before their branch does.
var invariantsFilePrefixOverrides = map[string][]string{
	"tools":      {"tool"},
	"mcpservers": {"mcp"},
	"locate":     {"locate", "yaml"},
	"suppress":   {"suppress", "ignore"},
}

// invariantsRequiredPrefixes returns the set of prefixes an unexported
// top-level identifier in filename may reference, per plan §1 rule 7.
func invariantsRequiredPrefixes(filename string) []string {
	stem := invariantsGroupStem(filename)
	if p, ok := invariantsFilePrefixOverrides[stem]; ok {
		return p
	}
	return []string{invariantsFilePrefix(stem)}
}

// TestGroupFileIdentifiersArePrefixedWithFileGroup enforces plan §1 rule 7: a
// package-level name collision between two concurrently authored files —
// INCLUDING their own _test.go files, the one namespace every one of the nine
// Phase-B authors is guaranteed to touch — is invisible to git and merges
// cleanly, breaking only on the merge commit. An unexported identifier must
// contain at least one of its file's allowed prefixes (invariantsRequiredPrefixes,
// case-insensitively — this also matches rule 8's `run<Something>` check
// functions, e.g. runEnvContainerBackend, not only a literal string prefix),
// over every .go file in the package except invariantsFoundationFiles. This
// file's own helpers (below) are covered too, at the "invariants" prefix its
// own stem derives — modeling the rule rather than being exempt from it.
func TestGroupFileIdentifiersArePrefixedWithFileGroup(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Keyed on the stem, not the raw name, so a foundation file's own
		// _test.go (render_json_test.go, toolknowledge_test.go) is exempt with
		// its production file — the same stem unification invariantsGroupStem
		// applies to group files.
		if invariantsFoundationFiles[invariantsGroupStem(name)+".go"] {
			continue
		}

		prefixes := invariantsRequiredPrefixes(name)
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			for _, id := range invariantsTopLevelIdents(decl) {
				if id.Name == "init" || id.Name == "_" || ast.IsExported(id.Name) {
					continue
				}
				lower := strings.ToLower(id.Name)
				ok := false
				for _, p := range prefixes {
					if strings.Contains(lower, p) {
						ok = true
						break
					}
				}
				if !ok {
					pos := fset.Position(id.Pos())
					t.Errorf("%s: unexported identifier %q does not reference any of its file's %q group prefixes (plan §1 rule 7)", pos, id.Name, prefixes)
				}
			}
		}
	}
}

// invariantsTopLevelIdents extracts the identifier(s) a top-level declaration
// introduces into package scope: one name for a func/type, one name per spec
// for a var/const block (including grouped `var a, b = ...`). A method
// (non-nil Recv) introduces no package-scope name and is skipped.
func invariantsTopLevelIdents(decl ast.Decl) []*ast.Ident {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil {
			return nil
		}
		return []*ast.Ident{d.Name}
	case *ast.GenDecl:
		var out []*ast.Ident
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				out = append(out, s.Name)
			case *ast.ValueSpec:
				out = append(out, s.Names...)
			}
		}
		return out
	}
	return nil
}

// TestNoAIFacingImport makes "doctor is never AI-reachable" a build-time fact:
// neither internal/mcptools nor internal/webui may import this package.
func TestNoAIFacingImport(t *testing.T) {
	for _, pkgDir := range []string{
		filepath.Join("..", "mcptools"),
		filepath.Join("..", "webui"),
	} {
		entries, err := os.ReadDir(pkgDir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", pkgDir, err)
		}
		fset := token.NewFileSet()
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", name, err)
			}
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if strings.Contains(path, "internal/doctor") {
					t.Errorf("%s imports %q — internal/doctor must never be AI-reachable", filepath.Join(pkgDir, name), path)
				}
			}
		}
	}
}
