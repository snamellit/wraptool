package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestRenderJSONSchemaFields decodes the emitted JSON back into the §9 shape
// and checks every field is carried through correctly, including the two C2
// fields (`suppressed`/`suppression_reason`) that no check sets yet — they
// still must round-trip when a Finding sets them directly, since C2 will rely
// on exactly this passthrough.
func TestRenderJSONSchemaFields(t *testing.T) {
	r := Report{
		Schema:           SchemaVersion,
		Version:          "0.5.1",
		ConfigPath:       "/home/user/.config/wraptool/config.yaml",
		ConfigState:      ConfigLoaded,
		Probed:           true,
		Incomplete:       true,
		IncompleteReason: "doctor: unknown check or group \"bogus\"",
		Summary:          Summary{Fail: 2, Warn: 5, Suppressed: 1, Info: 3, Pass: 22, Skipped: 6},
		Findings: []Finding{
			{
				ID: "policy.dangerous-flag", Severity: SeverityWarn,
				Title: "Allowed flag can execute an arbitrary command", Detail: "flag `-c` is allowed with no pattern constraint",
				Location: "/home/user/.config/wraptool/config.yaml:37",
				Remedy:   "add flag_constraints[\"-c\"].pattern, or deny the flag",
				DocRef:   "docs/configuration.qmd#flag-constraints",
			},
			{
				ID: "config.loose-perm", Severity: SeverityWarn, Title: "t", Detail: "d",
				Suppressed: true, SuppressionReason: "operator-acknowledged",
			},
		},
		Skipped: []SkipRecord{{ID: "drift.unknown-flag", Reason: "--no-probe"}},
	}

	var buf strings.Builder
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var got struct {
		Schema           int          `json:"schema"`
		ConfigPath       string       `json:"config_path"`
		WraptoolVersion  string       `json:"wraptool_version"`
		Probed           bool         `json:"probed"`
		Incomplete       bool         `json:"incomplete"`
		IncompleteReason string       `json:"incomplete_reason"`
		ConfigState      string       `json:"config_state"`
		Summary          Summary      `json:"summary"`
		Findings         []Finding    `json:"findings"`
		Skipped          []SkipRecord `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("decoding RenderJSON output: %v\noutput was:\n%s", err, buf.String())
	}

	if got.Schema != SchemaVersion {
		t.Errorf("schema = %d, want %d", got.Schema, SchemaVersion)
	}
	if got.ConfigPath != r.ConfigPath {
		t.Errorf("config_path = %q, want %q", got.ConfigPath, r.ConfigPath)
	}
	if got.WraptoolVersion != "0.5.1" {
		t.Errorf("wraptool_version = %q, want %q", got.WraptoolVersion, "0.5.1")
	}
	if !got.Probed {
		t.Error("probed = false, want true")
	}
	// item 9: incomplete/incomplete_reason/config_state are additive wire
	// fields (schema stays 1) so a --json consumer never mistakes an
	// incomplete run for a clean one just by reading stdout.
	if !got.Incomplete {
		t.Error("incomplete = false, want true")
	}
	if got.IncompleteReason != r.IncompleteReason {
		t.Errorf("incomplete_reason = %q, want %q", got.IncompleteReason, r.IncompleteReason)
	}
	if got.ConfigState != string(ConfigLoaded) {
		t.Errorf("config_state = %q, want %q", got.ConfigState, string(ConfigLoaded))
	}
	if got.Summary != r.Summary {
		t.Errorf("summary = %+v, want %+v", got.Summary, r.Summary)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(got.Findings))
	}
	if got.Findings[0].Location != r.Findings[0].Location {
		t.Errorf("finding[0].location = %q, want %q", got.Findings[0].Location, r.Findings[0].Location)
	}
	if got.Findings[0].Remedy != r.Findings[0].Remedy {
		t.Errorf("finding[0].remedy = %q, want %q", got.Findings[0].Remedy, r.Findings[0].Remedy)
	}
	if got.Findings[0].DocRef != r.Findings[0].DocRef {
		t.Errorf("finding[0].doc_ref = %q, want %q", got.Findings[0].DocRef, r.Findings[0].DocRef)
	}
	if !got.Findings[1].Suppressed || got.Findings[1].SuppressionReason != "operator-acknowledged" {
		t.Errorf("finding[1] suppression fields did not round-trip: %+v", got.Findings[1])
	}
	if len(got.Skipped) != 1 || got.Skipped[0].ID != "drift.unknown-flag" || got.Skipped[0].Reason != "--no-probe" {
		t.Errorf("skipped = %+v, want one drift.unknown-flag/--no-probe record", got.Skipped)
	}
}

// TestRenderJSONEmptyFindingsAndSkippedAreArraysNotNull covers a clean run: a
// nil Findings/Skipped slice must not surface to a consumer as JSON `null`,
// which would force every reader to special-case it before ranging.
func TestRenderJSONEmptyFindingsAndSkippedAreArraysNotNull(t *testing.T) {
	var buf strings.Builder
	if err := RenderJSON(&buf, Report{Schema: SchemaVersion}); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "null") {
		t.Errorf("output contains a null field, want empty arrays: %s", out)
	}
	if !strings.Contains(out, `"findings": []`) {
		t.Errorf("missing empty findings array, got: %s", out)
	}
	if !strings.Contains(out, `"skipped": []`) {
		t.Errorf("missing empty skipped array, got: %s", out)
	}
}

// TestRenderJSONThroughRun exercises RenderJSON against a real Run() output —
// the same integration seam RenderHuman's tests use — so the summary
// arithmetic Run computes is what a --json consumer actually sees.
func TestRenderJSONThroughRun(t *testing.T) {
	checks := []Check{
		doctorFindingCheck("policy.dangerous", SeverityFail, "git -c core.sshCommand is allowed"),
		doctorSkippedCheck("tools.binary-writable", "no readable config file"),
	}
	r := Run(context.Background(), doctorVirginInput(), RunOptions{Checks: checks})

	var buf strings.Builder
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var got struct {
		Summary  Summary      `json:"summary"`
		Findings []Finding    `json:"findings"`
		Skipped  []SkipRecord `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Summary.Fail != 1 || got.Summary.Skipped != 1 {
		t.Errorf("summary = %+v, want fail=1 skipped=1", got.Summary)
	}
	if len(got.Findings) != 1 || got.Findings[0].ID != "policy.dangerous" {
		t.Errorf("findings = %+v, want one policy.dangerous finding", got.Findings)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].ID != "tools.binary-writable" {
		t.Errorf("skipped = %+v, want one tools.binary-writable record", got.Skipped)
	}
}

// TestRenderJSONIncompleteRunSurfacesIncompleteFields is item 9's core case:
// `doctor --only bogus --json` must not write a clean all-zero report to
// stdout. An unknown --only selector makes Run itself return an Incomplete
// report (TestSelectionUnknownSelectorIsAnError pins that at the engine
// level); this test pins that the JSON wire format actually carries it.
func TestRenderJSONIncompleteRunSurfacesIncompleteFields(t *testing.T) {
	checks := []Check{doctorPassCheck("env.a")}
	r := Run(context.Background(), doctorVirginInput(), RunOptions{Checks: checks, Only: []string{"no-such-group"}})
	if !r.Incomplete || r.IncompleteReason == "" {
		t.Fatalf("precondition failed: Run did not return an Incomplete report: %+v", r)
	}

	var buf strings.Builder
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var got struct {
		Incomplete       bool   `json:"incomplete"`
		IncompleteReason string `json:"incomplete_reason"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatalf("decoding: %v\noutput was:\n%s", err, buf.String())
	}
	if !got.Incomplete {
		t.Errorf("incomplete = false, want true — a --json consumer parsing stdout must see this run did not complete")
	}
	if got.IncompleteReason != r.IncompleteReason {
		t.Errorf("incomplete_reason = %q, want %q", got.IncompleteReason, r.IncompleteReason)
	}
}

// TestRenderJSONConfigStateRoundTrips pins config_state for all three
// ConfigState members (doctor.go's closed enum), since it is emitted as a
// bare string on the wire.
func TestRenderJSONConfigStateRoundTrips(t *testing.T) {
	for _, cs := range []ConfigState{ConfigLoaded, ConfigAbsent, ConfigUnreadable} {
		cs := cs
		t.Run(string(cs), func(t *testing.T) {
			var buf strings.Builder
			if err := RenderJSON(&buf, Report{Schema: SchemaVersion, ConfigState: cs}); err != nil {
				t.Fatalf("RenderJSON: %v", err)
			}
			var got struct {
				ConfigState string `json:"config_state"`
			}
			if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if got.ConfigState != string(cs) {
				t.Errorf("config_state = %q, want %q", got.ConfigState, string(cs))
			}
		})
	}
}

// TestRenderJSONDoesNotEscapeHTML pins item 8: json.Encoder's default
// SetEscapeHTML(true) turns the ASCII characters less-than, greater-than,
// and ampersand into \uXXXX escapes, which is wrong for doctor's report — it
// is never embedded in HTML, and a Detail or Remedy legitimately contains a
// shell redirect or comparison (e.g. `git -c core.sshCommand=…`, or a flag
// pattern like `foo > bar`).
func TestRenderJSONDoesNotEscapeHTML(t *testing.T) {
	const detail = "value <redirect> & \"quoted\" here"
	const remedy = "use a < b or a > b"
	r := Report{
		Schema: SchemaVersion,
		Findings: []Finding{
			{ID: "policy.dangerous-flag", Severity: SeverityWarn, Title: "t", Detail: detail, Remedy: remedy},
		},
	}
	var buf strings.Builder
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	out := buf.String()
	// SetEscapeHTML(true)'s default output would contain these \uXXXX escape
	// sequences instead of the literal characters; their absence is exactly
	// what SetEscapeHTML(false) buys. Built from rune codes, not written as
	// literal escape text, so nothing in this source file could be mistaken
	// for (or silently normalized into) the character it denotes.
	lessThanEscape := "\\u" + fmt.Sprintf("%04x", '<')
	greaterThanEscape := "\\u" + fmt.Sprintf("%04x", '>')
	ampersandEscape := "\\u" + fmt.Sprintf("%04x", '&')
	for _, escaped := range []string{lessThanEscape, greaterThanEscape, ampersandEscape} {
		if strings.Contains(out, escaped) {
			t.Errorf("output contains %s, want SetEscapeHTML(false) to leave the literal character unescaped: %s", escaped, out)
		}
	}
	// The literal, unescaped characters must round-trip through Detail/Remedy.
	if !strings.Contains(out, `"detail": "value <redirect> & \"quoted\" here"`) {
		t.Errorf("Detail did not round-trip with literal <, >, & characters: %s", out)
	}
	if !strings.Contains(out, `"remedy": "use a < b or a > b"`) {
		t.Errorf("Remedy did not round-trip with literal <, > characters: %s", out)
	}
}

func TestRenderJSONNeverMentionsStdioTransport(t *testing.T) {
	r := Run(context.Background(), doctorVirginInput(), RunOptions{Checks: []Check{
		doctorFindingCheck("env.container-backend", SeverityInfo, "found: docker (/usr/bin/docker)"),
	}})
	var buf strings.Builder
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if strings.Contains(buf.String(), "stdio") {
		t.Errorf("doctor output must never name the stdio MCP transport, got:\n%s", buf.String())
	}
}
