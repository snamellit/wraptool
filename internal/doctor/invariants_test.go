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

// invariantInputs is the pair every registry-wide invariant test runs against:
// a virgin host (no config, no workspace — "wraptool doctor is safe as the
// very first command a new user runs") AND a fully-populated host (every
// optional Requirement bit satisfied). Six of nine Phase-B groups are
// config-gated (Meta.Needs&NeedsConfig != 0): running ONLY the virgin case
// would leave them permanently untested by these invariants, and running
// every check unconditionally against the virgin Input — ignoring Needs
// entirely — is what made A1's original invariant tests panic on nil
// in.Config, because the engine's own documented guarantee (Run: "a check
// whose Needs are unmet is never invoked") does not hold for a caller that
// calls c.Run directly instead of going through Run. Both cases below gate on
// the same provides()/Needs check the engine itself uses, so a check is only
// ever invoked here under an Input it actually declared it could handle.
func invariantInputs() []struct {
	name string
	in   *Input
} {
	return []struct {
		name string
		in   *Input
	}{
		{"virgin", &Input{
			Env:        NewMapEnv(),
			GOOS:       "linux",
			ConfigPath: "/home/user/.config/wraptool/config.yaml",
		}},
		{"fully-populated", fullyPopulatedInput()},
	}
}

// fullyPopulatedInput satisfies every Requirement bit doctor.go's provides()
// knows about, so a config-gated (or workspace-gated, probe-gated,
// pool-verify-gated) check gets exercised by the registry-wide invariants too
// — not just the checks that happen to need nothing.
func fullyPopulatedInput() *Input {
	return &Input{
		Env:  NewMapEnv(),
		GOOS: "linux",
		Paths: HostPaths{
			ConfigDir:  "/home/user/.config/wraptool",
			DataDir:    "/home/user/.local/share/wraptool",
			CacheDir:   "/home/user/.cache/wraptool",
			RuntimeDir: "/run/user/1000/wraptool",
			PoolRoot:   "/home/user/.local/share/wraptool/harness-pool",
		},
		ConfigPath:  "/home/user/.config/wraptool/config.yaml",
		ConfigState: ConfigLoaded,
		Config:      &config.Config{},
		Workspace:   "/home/user/project",
		Probe:       true,
		VerifyPool:  true,
	}
}

// checksSatisfying returns Registered() filtered to the checks in's Input can
// actually satisfy — the same gate doctor.Run applies via provides()/Needs.
func checksSatisfying(in *Input) []Check {
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
// satisfy against BOTH a virgin host and a fully-populated one (see
// invariantInputs), so no check is ever invoked under an Input it did not
// declare it could handle, and no check escapes coverage just because it
// happens to be config/workspace/probe/pool-gated.
func TestChecksSurviveAVirginHost(t *testing.T) {
	for _, tc := range invariantInputs() {
		for _, c := range checksSatisfying(tc.in) {
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

// TestChecksAreDeterministic runs each registered check (whose Needs the
// Input satisfies) twice against the same Input and requires an identical
// Result both times. Catches the likeliest defect class: ranging over a Go
// map (e.g. cfg.Tools) without sorting keys.
func TestChecksAreDeterministic(t *testing.T) {
	for _, tc := range invariantInputs() {
		for _, c := range checksSatisfying(tc.in) {
			m := c.Meta()
			t.Run(tc.name+"/"+string(m.ID), func(t *testing.T) {
				r1 := c.Run(context.Background(), tc.in)
				r2 := c.Run(context.Background(), tc.in)
				if !reflect.DeepEqual(r1, r2) {
					t.Errorf("check %q is non-deterministic:\nrun 1: %+v\nrun 2: %+v", m.ID, r1, r2)
				}
			})
		}
	}
}

// TestFindingsAreWellFormed checks the shape of every finding a registered
// check emits directly (i.e. before the engine's Location/ID stamping), under
// both invariantInputs cases: valid Severity, non-empty Title/Detail, a
// Remedy for fail/warn, never both Path and Location set by the check itself
// (RULING R5 — that field is engine-owned for config findings), and — plan §1
// rule 9 — a check must never hand-roll a config Location by echoing
// in.ConfigPath into it; it must set Path so C1 can resolve a real file:line.
func TestFindingsAreWellFormed(t *testing.T) {
	for _, tc := range invariantInputs() {
		for _, c := range checksSatisfying(tc.in) {
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

// doctorHostAllowlist names the files permitted to touch the real host
// directly. Every other file in this package must go through Env.
var doctorHostAllowlist = map[string]bool{
	"host.go":            true,
	"host_stat_unix.go":  true,
	"host_stat_other.go": true,
	"mapenv.go":          true,
}

// forbiddenHostSelectors: package identifier -> selector names forbidden on
// that package. A nil/empty slice means every selector on that package is
// forbidden.
//
// The "pool" entries are internal/pool's zero-argument, HOST-ROOTED layout
// accessors (internal/pool/layout.go): every one of them resolves through
// paths.DataDir() directly, bypassing Input.Paths.PoolRoot entirely, so
// calling e.g. pool.HomeDir() from a check reads the REAL host's
// $XDG_DATA_HOME even under a MapEnv-backed unit test and this guard test
// still passes unless the selector itself is named here. Deliberately NOT
// listed: pool.LegacyRemovedMarkerFileAt (takes an explicit root) and every
// pool.PoolPaths-TAKING function (ActiveGeneration, VerifyGenerationTree,
// GenerationVersion, ...) — those ARE root-parameterized and are exactly how
// B8 is meant to reach the pool, building its own pool.PoolPaths from
// Input.Paths.PoolRoot.
var forbiddenHostSelectors = map[string][]string{
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
// the host through Env (or a root-parameterized pool.PoolPaths call) — which
// is what makes MapEnv-backed unit tests trustworthy for every future check.
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
		if doctorHostAllowlist[name] {
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
			names, forbidden := forbiddenHostSelectors[id.Name]
			if !forbidden {
				return true
			}
			if len(names) == 0 || slices.Contains(names, sel.Sel.Name) {
				pos := fset.Position(sel.Pos())
				t.Errorf("%s: direct host access %s.%s — must go through Env (or a root-parameterized pool.PoolPaths call)", pos, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// doctorFoundationFiles are A1's own seam files — the engine, types, and human
// renderer — exempt from the group-file naming convention (plan §1 rule 7)
// because they ARE the shared seam, not a Phase-B/C/D check group.
var doctorFoundationFiles = map[string]bool{
	"finding.go":         true,
	"doctor.go":          true,
	"render_human.go":    true,
	"host.go":            true,
	"host_stat_unix.go":  true,
	"host_stat_other.go": true,
	"mapenv.go":          true,
}

// filePrefix derives the naming-convention prefix for a group file: the
// FIRST underscore-delimited segment of its base name, e.g. "env.go" -> "env",
// "config_load.go" / "config_perms.go" -> "config", "mcpservers.go" ->
// "mcpservers" (no underscore). Two files that share this coarse prefix AND
// land in the SAME parallel wave (exposure_listen.go/exposure_files.go;
// tools.go/mcpservers.go are Group `tools` but do NOT share this file-derived
// prefix, so they are naturally disjoint) must extend it with their own
// distinguishing segment (exposureListen*/exposureFiles*) — see plan §1 rule
// 7. This check enforces the coarse, mechanically-verifiable half; the finer
// per-file disambiguation for same-prefix parallel files is reviewer-enforced.
func filePrefix(filename string) string {
	stem := strings.TrimSuffix(filename, ".go")
	seg, _, _ := strings.Cut(stem, "_")
	return seg
}

// TestGroupFileIdentifiersArePrefixedWithFileGroup enforces plan §1 rule 7: a
// package-level name collision between two concurrently authored group files
// is invisible to git and merges cleanly, breaking only on the merge commit.
// An unexported identifier must contain its file's own group prefix
// (case-insensitively — this also matches rule 8's `run<Something>` check
// functions, e.g. runEnvContainerBackend, not only a literal string prefix).
func TestGroupFileIdentifiersArePrefixedWithFileGroup(t *testing.T) {
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
		if doctorFoundationFiles[name] {
			continue
		}

		prefix := strings.ToLower(filePrefix(name))
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			for _, id := range topLevelIdents(decl) {
				if id.Name == "init" || id.Name == "_" || ast.IsExported(id.Name) {
					continue
				}
				if !strings.Contains(strings.ToLower(id.Name), prefix) {
					pos := fset.Position(id.Pos())
					t.Errorf("%s: unexported identifier %q does not reference its file's %q group prefix (plan §1 rule 7)", pos, id.Name, prefix)
				}
			}
		}
	}
}

// topLevelIdents extracts the identifier(s) a top-level declaration introduces
// into package scope: one name for a func/type, one name per spec for a
// var/const block (including grouped `var a, b = ...`). A method (non-nil
// Recv) introduces no package-scope name and is skipped.
func topLevelIdents(decl ast.Decl) []*ast.Ident {
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
