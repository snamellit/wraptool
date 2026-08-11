package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// withCleanRegistry gives a test its own empty registry state (registry,
// regSites, registryErrs) and restores the real, package-init-populated state
// afterward. Every test in this package shares one process, so registering a
// deliberately-broken check without this would poison every later test that
// asserts RegistryErr() == nil.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	origRegistry, origSites, origErrs := registry, regSites, registryErrs
	registry = nil
	regSites = map[CheckID]string{}
	registryErrs = nil
	t.Cleanup(func() {
		registry, regSites, registryErrs = origRegistry, origSites, origErrs
	})
}

func TestRegisterRecordsDuplicateIDWithBothSites(t *testing.T) {
	withCleanRegistry(t)

	m := Meta{ID: "env.dup-test", Needs: NeedsNothing, Summary: "first"}
	registerCheck(funcCheck{meta: m, run: func(_ context.Context, in *Input) Result { return Pass() }}, "file_a.go:10")
	registerCheck(funcCheck{meta: m, run: func(_ context.Context, in *Input) Result { return Pass() }}, "file_b.go:20")

	err := RegistryErr()
	if err == nil {
		t.Fatal("expected a registry error for a duplicate ID")
	}
	if !strings.Contains(err.Error(), "file_a.go:10") || !strings.Contains(err.Error(), "file_b.go:20") {
		t.Fatalf("error does not name both registration sites: %v", err)
	}
	if len(registry) != 1 {
		t.Fatalf("expected the first registration to stick, got %d entries", len(registry))
	}
}

func TestRegisterRecordsMalformedID(t *testing.T) {
	withCleanRegistry(t)

	registerCheck(funcCheck{meta: Meta{ID: "NotDotted", Needs: NeedsNothing, Summary: "x"}, run: passRun}, "site:1")

	if RegistryErr() == nil {
		t.Fatal("expected a registry error for a malformed ID")
	}
	if len(registry) != 0 {
		t.Fatalf("malformed check must not be registered, got %d entries", len(registry))
	}
}

func TestRegisterRecordsUnknownGroup(t *testing.T) {
	withCleanRegistry(t)

	registerCheck(funcCheck{meta: Meta{ID: "nosuchgroup.thing", Needs: NeedsNothing, Summary: "x"}, run: passRun}, "site:1")

	if RegistryErr() == nil {
		t.Fatal("expected a registry error for an unknown group")
	}
}

func TestRegisterRecordsEmptySummary(t *testing.T) {
	withCleanRegistry(t)

	registerCheck(funcCheck{meta: Meta{ID: "env.no-summary", Needs: NeedsNothing, Summary: "  "}, run: passRun}, "site:1")

	if RegistryErr() == nil {
		t.Fatal("expected a registry error for an empty Summary")
	}
}

func TestRegisterDoesNotPanic(t *testing.T) {
	withCleanRegistry(t)
	// A malformed registration must be RECORDED, never panic — register()
	// runs from init() at package load, and a panic there bricks every
	// importer of the package, including serve/up/harness.
	registerCheck(funcCheck{meta: Meta{}, run: passRun}, "site:1")
}

func passRun(_ context.Context, _ *Input) Result { return Pass() }

func TestRegisteredIsGroupThenIDOrdered(t *testing.T) {
	withCleanRegistry(t)

	ids := []CheckID{"policy.b", "env.z", "config.a", "env.a", "tools.a"}
	for _, id := range ids {
		registerCheck(funcCheck{meta: Meta{ID: id, Summary: "x"}, run: passRun}, "site")
	}
	if err := RegistryErr(); err != nil {
		t.Fatalf("unexpected registry error: %v", err)
	}

	got := Registered()
	want := []CheckID{"env.a", "env.z", "config.a", "tools.a", "policy.b"}
	if len(got) != len(want) {
		t.Fatalf("got %d checks, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.Meta().ID != want[i] {
			t.Errorf("position %d: got %q, want %q", i, c.Meta().ID, want[i])
		}
	}
}

// -------------------------------------------------------------- Result

func TestResultConstructors(t *testing.T) {
	if o := Pass().Outcome(); o != OutcomePass {
		t.Errorf("Pass(): got outcome %q, want pass", o)
	}
	r := Skipped("no %s", "binary")
	if r.Outcome() != OutcomeSkipped {
		t.Errorf("Skipped(): got outcome %q, want skipped", r.Outcome())
	}
	if r.SkipReason != "no binary" {
		t.Errorf("Skipped(): got reason %q, want %q", r.SkipReason, "no binary")
	}

	// Found() with a zero-length slice must degrade to Pass — the most likely
	// way a check author would otherwise break the summary arithmetic.
	if o := Found().Outcome(); o != OutcomePass {
		t.Errorf("Found(): empty call got outcome %q, want pass", o)
	}
	f := Finding{Severity: SeverityInfo, Title: "t", Detail: "d"}
	if o := Found(f).Outcome(); o != OutcomeFinding {
		t.Errorf("Found(f): got outcome %q, want finding", o)
	}

	err := errors.New("boom")
	ab := Abort(err, f)
	if ab.Fatal != err {
		t.Errorf("Abort(): Fatal not preserved")
	}
	if len(ab.Findings) != 1 {
		t.Errorf("Abort(): findings not preserved")
	}
}

func TestSeverityRankOrdering(t *testing.T) {
	if SeverityFail.rank() >= SeverityWarn.rank() {
		t.Error("fail must rank before warn")
	}
	if SeverityWarn.rank() >= SeverityInfo.rank() {
		t.Error("warn must rank before info")
	}
}

func TestSeverityValid(t *testing.T) {
	for _, s := range []Severity{SeverityFail, SeverityWarn, SeverityInfo} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if Severity("bogus").Valid() {
		t.Error("bogus severity reported valid")
	}
	if Severity("").Valid() {
		t.Error("zero-value severity reported valid")
	}
}

func TestOutcomeValid(t *testing.T) {
	for _, o := range []Outcome{OutcomePass, OutcomeFinding, OutcomeSkipped} {
		if !o.Valid() {
			t.Errorf("%q should be valid", o)
		}
	}
	if Outcome("bogus").Valid() {
		t.Error("bogus outcome reported valid")
	}
}

// -------------------------------------------------------------- ConfigPath

func TestConfigPathString(t *testing.T) {
	cases := []struct {
		p    ConfigPath
		want string
	}{
		{At("tools", "git", "binary"), "tools.git.binary"},
		{At("tools", "git", "allow").Item("subcommand", "pull"), "tools.git.allow[subcommand=pull]"},
		{At("tools", "git", "allow").Item("subcommand", "pull").Key("flags").Elem("-c"), "tools.git.allow[subcommand=pull].flags[-c]"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("String(): got %q, want %q", got, c.want)
		}
	}
}

func TestConfigPathEmpty(t *testing.T) {
	if !(ConfigPath{}).Empty() {
		t.Error("zero-value ConfigPath must be Empty")
	}
	if At("x").Empty() {
		t.Error("At(\"x\") must not be Empty")
	}
}

func TestConfigPathBuildersAreImmutable(t *testing.T) {
	base := At("tools", "git")
	_ = base.Key("allow")
	if base.String() != "tools.git" {
		t.Errorf("base path mutated: got %q", base.String())
	}
}

func TestNullLocatorNeverResolves(t *testing.T) {
	var l Locator = nullLocator{}
	if _, ok := l.Locate(At("tools")); ok {
		t.Error("nullLocator must never resolve")
	}
}

func TestSiteString(t *testing.T) {
	if (Site{}).String() != "" {
		t.Error("zero Site must render empty")
	}
	if got := (Site{File: "config.yaml", Line: 12}).String(); got != "config.yaml:12" {
		t.Errorf("got %q, want config.yaml:12", got)
	}
}
