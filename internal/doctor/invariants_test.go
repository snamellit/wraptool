package doctor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

// TestChecksSurviveAVirginHost runs every registered check against an Input
// with no config, no workspace, and a synthetic Env with nothing populated.
// This makes "wraptool doctor is safe as the very first command a new user
// runs" a property of the package, not of nine authors' individual care.
func TestChecksSurviveAVirginHost(t *testing.T) {
	in := &Input{Env: NewMapEnv(), GOOS: "linux"}
	for _, c := range Registered() {
		m := c.Meta()
		t.Run(string(m.ID), func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("check %q panicked on a virgin host: %v", m.ID, p)
				}
			}()
			res := c.Run(context.Background(), in)
			if res.Fatal != nil {
				t.Errorf("check %q returned Fatal on a virgin host: %v", m.ID, res.Fatal)
			}
			if reason := validateResult(m.ID, res); reason != "" {
				t.Errorf("check %q returned an invalid Result: %s", m.ID, reason)
			}
		})
	}
}

// TestChecksAreDeterministic runs each registered check twice against the same
// Input and requires an identical Result both times. Catches the likeliest
// defect class: ranging over a Go map (e.g. cfg.Tools) without sorting keys.
func TestChecksAreDeterministic(t *testing.T) {
	in := &Input{Env: NewMapEnv(), GOOS: "linux"}
	for _, c := range Registered() {
		m := c.Meta()
		t.Run(string(m.ID), func(t *testing.T) {
			r1 := c.Run(context.Background(), in)
			r2 := c.Run(context.Background(), in)
			if !reflect.DeepEqual(r1, r2) {
				t.Errorf("check %q is non-deterministic:\nrun 1: %+v\nrun 2: %+v", m.ID, r1, r2)
			}
		})
	}
}

// TestFindingsAreWellFormed checks the shape of every finding a registered
// check emits directly (i.e. before the engine's Location/ID stamping), on a
// virgin host: valid Severity, non-empty Title/Detail, a Remedy for fail/warn,
// and never both Path and Location set by the check itself (RULING R5 — that
// field is engine-owned for config findings).
func TestFindingsAreWellFormed(t *testing.T) {
	in := &Input{Env: NewMapEnv(), GOOS: "linux"}
	for _, c := range Registered() {
		m := c.Meta()
		res := c.Run(context.Background(), in)
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

// forbiddenHostSelectors: package identifier -> either "" (any selector on
// that package is forbidden) or a specific selector name.
var forbiddenHostSelectors = map[string]string{
	"os":       "",
	"exec":     "",
	"syscall":  "",
	"paths":    "",
	"time":     "Now",
	"filepath": "EvalSymlinks",
}

// TestChecksDoNotTouchTheRealHost statically rejects direct host access
// (os./exec./syscall./paths./time.Now/filepath.EvalSymlinks) outside the
// allowlisted files, so a check can only reach the host through Env — which is
// what makes MapEnv-backed unit tests trustworthy for every future check.
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
			want, forbidden := forbiddenHostSelectors[id.Name]
			if !forbidden {
				return true
			}
			if want == "" || want == sel.Sel.Name {
				pos := fset.Position(sel.Pos())
				t.Errorf("%s: direct host access %s.%s — must go through Env", pos, id.Name, sel.Sel.Name)
			}
			return true
		})
	}
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
