package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func check(id CheckID, needs Requirement, run RunFunc) Check {
	return funcCheck{meta: Meta{ID: id, Needs: needs, Summary: "test check " + string(id)}, run: run}
}

func passCheck(id CheckID) Check {
	return check(id, NeedsNothing, func(context.Context, *Input) Result { return Pass() })
}

func findingCheck(id CheckID, sev Severity, detail string) Check {
	return check(id, NeedsNothing, func(context.Context, *Input) Result {
		return Found(Finding{Severity: sev, Title: "t", Detail: detail, Remedy: "fix it"})
	})
}

func skippedCheck(id CheckID, reason string) Check {
	return check(id, NeedsNothing, func(context.Context, *Input) Result { return Skipped("%s", reason) })
}

func virginInput() *Input {
	return &Input{Env: NewMapEnv(), GOOS: "linux", ConfigState: ConfigAbsent}
}

// -------------------------------------------------------------- ordering

func TestRunOrderingIsTotalAndPermutationInvariant(t *testing.T) {
	checks := []Check{
		findingCheck("policy.b", SeverityWarn, "z-detail"),
		findingCheck("policy.a", SeverityFail, "a-detail"),
		findingCheck("env.a", SeverityWarn, "a-detail"),
		findingCheck("config.a", SeverityInfo, "a-detail"),
	}

	r1 := Run(context.Background(), virginInput(), RunOptions{Checks: checks})

	// Reverse the input order — RunOptions.Checks is re-sorted by the engine,
	// and the resulting Findings order must be identical regardless.
	reversed := make([]Check, len(checks))
	for i, c := range checks {
		reversed[len(checks)-1-i] = c
	}
	r2 := Run(context.Background(), virginInput(), RunOptions{Checks: reversed})

	if len(r1.Findings) != len(r2.Findings) {
		t.Fatalf("got %d vs %d findings", len(r1.Findings), len(r2.Findings))
	}
	for i := range r1.Findings {
		if r1.Findings[i].ID != r2.Findings[i].ID || r1.Findings[i].Detail != r2.Findings[i].Detail {
			t.Errorf("position %d: %v/%v vs %v/%v", i, r1.Findings[i].ID, r1.Findings[i].Detail, r2.Findings[i].ID, r2.Findings[i].Detail)
		}
	}

	// Canonical order: fail before warn before info.
	want := []CheckID{"policy.a", "env.a", "policy.b"}
	// policy.a is fail (rank 0); env.a and policy.b are both warn (rank 1),
	// tie-broken by ID (env.a < policy.b); config.a is info (rank 2).
	for i, id := range want {
		if r1.Findings[i].ID != id {
			t.Errorf("position %d: got %q, want %q", i, r1.Findings[i].ID, id)
		}
	}
	if r1.Findings[3].ID != "config.a" {
		t.Errorf("last position: got %q, want config.a", r1.Findings[3].ID)
	}
}

// -------------------------------------------------------------- arithmetic

func TestSummaryArithmeticOneOfEachOutcome(t *testing.T) {
	checks := []Check{
		passCheck("env.pass"),
		findingCheck("env.find", SeverityWarn, "d"),
		skippedCheck("env.skip", "no binary"),
		check("env.needs-workspace", NeedsWorkspace, func(context.Context, *Input) Result { return Pass() }),
	}

	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks})

	if r.Summary.Total != len(checks) {
		t.Fatalf("Total = %d, want %d", r.Summary.Total, len(checks))
	}
	if got := r.Summary.Pass + r.Summary.WithFindings + r.Summary.Skipped; got != r.Summary.Total {
		t.Errorf("Pass+WithFindings+Skipped = %d, want Total = %d", got, r.Summary.Total)
	}
	if r.Summary.Pass != 1 || r.Summary.WithFindings != 1 || r.Summary.Skipped != 2 {
		t.Errorf("got pass=%d withFindings=%d skipped=%d, want 1/1/2",
			r.Summary.Pass, r.Summary.WithFindings, r.Summary.Skipped)
	}
	if len(r.Outcomes) != len(checks) {
		t.Errorf("len(Outcomes) = %d, want %d", len(r.Outcomes), len(checks))
	}
	if len(r.Skipped) != 2 {
		t.Errorf("len(Skipped) = %d, want 2", len(r.Skipped))
	}
	if r.Summary.Fail+r.Summary.Warn+r.Summary.Info != len(r.Findings) {
		t.Errorf("Fail+Warn+Info = %d, want len(Findings) = %d",
			r.Summary.Fail+r.Summary.Warn+r.Summary.Info, len(r.Findings))
	}
}

// -------------------------------------------------------------- requirements

func TestEachRequirementBitSkipsWithCannedReason(t *testing.T) {
	for _, bit := range []Requirement{NeedsConfig, NeedsWorkspace, NeedsModeBits, NeedsProbe, NeedsPoolVerify} {
		bit := bit
		t.Run(fmt.Sprintf("%d", bit), func(t *testing.T) {
			c := check("env.needs", bit, func(context.Context, *Input) Result { return Pass() })
			// virginInput provides none of the optional bits and is GOOS linux
			// (so NeedsModeBits IS satisfied) — build an input that satisfies
			// nothing so every bit is legitimately missing.
			in := &Input{Env: NewMapEnv(), GOOS: "windows"}
			r := Run(context.Background(), in, RunOptions{Checks: []Check{c}})

			if len(r.Skipped) != 1 {
				t.Fatalf("got %d skip records, want 1", len(r.Skipped))
			}
			if r.Skipped[0].Reason != firstMissingReason(bit) {
				t.Errorf("reason %q, want %q", r.Skipped[0].Reason, firstMissingReason(bit))
			}
			if r.Summary.Skipped != 1 || r.Summary.Total != 1 {
				t.Errorf("summary = %+v, want total=1 skipped=1", r.Summary)
			}
		})
	}
}

// -------------------------------------------------------------- fatal abort

func TestFatalAbortsRemainderAndPreservesArithmetic(t *testing.T) {
	fatalErr := errors.New("config is unparseable")
	checks := []Check{
		passCheck("env.a"),
		check("config.abort", NeedsNothing, func(context.Context, *Input) Result {
			return Abort(fatalErr, Finding{Severity: SeverityFail, Title: "t", Detail: "d", Remedy: "fix"})
		}),
		passCheck("tools.c"),
		passCheck("tools.d"),
	}

	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks})

	if !r.Incomplete {
		t.Fatal("expected Incomplete = true")
	}
	if r.IncompleteReason != fatalErr.Error() {
		t.Errorf("IncompleteReason = %q, want %q", r.IncompleteReason, fatalErr.Error())
	}
	if r.Summary.Total != 4 {
		t.Fatalf("Total = %d, want 4", r.Summary.Total)
	}
	if got := r.Summary.Pass + r.Summary.WithFindings + r.Summary.Skipped; got != r.Summary.Total {
		t.Errorf("Pass+WithFindings+Skipped = %d, want Total = %d", got, r.Summary.Total)
	}
	if r.Summary.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (tools.c, tools.d)", r.Summary.Skipped)
	}
	for _, id := range []CheckID{"tools.c", "tools.d"} {
		var reason string
		for _, s := range r.Skipped {
			if s.ID == id {
				reason = s.Reason
			}
		}
		if !strings.Contains(reason, "aborted after config.abort") {
			t.Errorf("%s: skip reason %q does not name the aborting check", id, reason)
		}
	}
	if len(r.Findings) != 1 || r.Findings[0].ID != "config.abort" {
		t.Errorf("expected the aborting check's own finding to be recorded, got %+v", r.Findings)
	}
}

// -------------------------------------------------------------- validation

func TestEmptySeverityYieldsIncompleteNotMisorderedReport(t *testing.T) {
	checks := []Check{
		check("env.bad", NeedsNothing, func(context.Context, *Input) Result {
			return Found(Finding{Title: "t", Detail: "d"}) // Severity left zero-value
		}),
	}
	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks})
	if !r.Incomplete {
		t.Fatal("expected Incomplete = true for an invalid severity")
	}
	if len(r.Findings) != 0 {
		t.Errorf("an incomplete run must not publish the offending finding: got %+v", r.Findings)
	}
}

func TestUnregisteredExplicitFindingIDIsIncomplete(t *testing.T) {
	checks := []Check{
		check("env.bad", NeedsNothing, func(context.Context, *Input) Result {
			return Found(Finding{ID: "drift.no-such-check", Severity: SeverityWarn, Title: "t", Detail: "d", Remedy: "x"})
		}),
	}
	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks})
	if !r.Incomplete {
		t.Fatal("expected Incomplete = true for an unregistered explicit finding ID")
	}
}

// -------------------------------------------------------------- selection

func TestSelectionUnknownSelectorIsAnError(t *testing.T) {
	checks := []Check{passCheck("env.a")}
	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks, Only: []string{"no-such-group"}})
	if !r.Incomplete {
		t.Fatal("expected Incomplete = true for an unknown --only selector")
	}
}

func TestSelectionDeselectedChecksAreExcludedNotSkipped(t *testing.T) {
	checks := []Check{passCheck("env.a"), passCheck("tools.b")}
	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks, Only: []string{"env"}})

	if r.Summary.Total != 1 {
		t.Fatalf("Total = %d, want 1 (deselected checks must not be counted at all)", r.Summary.Total)
	}
	if len(r.Skipped) != 0 {
		t.Errorf("a deselected check must not appear in Skipped, got %+v", r.Skipped)
	}
	if len(r.Outcomes) != 1 || r.Outcomes[0].ID != "env.a" {
		t.Errorf("got outcomes %+v, want just env.a", r.Outcomes)
	}
}

func TestSelectionSkipWinsOverOnly(t *testing.T) {
	checks := []Check{passCheck("env.a"), passCheck("env.b")}
	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks, Only: []string{"env"}, Skip: []string{"env.a"}})
	if r.Summary.Total != 1 || r.Outcomes[0].ID != "env.b" {
		t.Errorf("got total=%d outcomes=%+v, want just env.b selected", r.Summary.Total, r.Outcomes)
	}
}

// -------------------------------------------------------------- exit codes

func TestExitCodeMatrix(t *testing.T) {
	cases := []struct {
		name string
		r    Report
		want int
	}{
		{"clean", Report{}, 0},
		{"warn-only", Report{Summary: Summary{Warn: 1}}, 0},
		{"warn-only-strict", Report{Strict: true, Summary: Summary{Warn: 1}}, 1},
		{"warn-only-strict-all-suppressed", Report{Strict: true, Summary: Summary{Warn: 1, Suppressed: 1}}, 0},
		{"fail", Report{Summary: Summary{Fail: 1}}, 1},
		{"fail-beats-strict-irrelevant", Report{Strict: true, Summary: Summary{Fail: 1}}, 1},
		{"incomplete", Report{Incomplete: true}, 2},
		{"incomplete-beats-fail", Report{Incomplete: true, Summary: Summary{Fail: 1}}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.r.ExitCode(); got != c.want {
				t.Errorf("ExitCode() = %d, want %d", got, c.want)
			}
		})
	}
}

// -------------------------------------------------------------- ID stamping

func TestFindingIDIsStampedFromCheckMetaWhenEmpty(t *testing.T) {
	checks := []Check{findingCheck("env.x", SeverityInfo, "d")}
	r := Run(context.Background(), virginInput(), RunOptions{Checks: checks})
	if len(r.Findings) != 1 || r.Findings[0].ID != "env.x" {
		t.Errorf("got %+v, want ID stamped as env.x", r.Findings)
	}
}
