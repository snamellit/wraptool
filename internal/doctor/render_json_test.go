package doctor

import (
	"context"
	"encoding/json"
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
		Schema:     SchemaVersion,
		Version:    "0.5.1",
		ConfigPath: "/home/user/.config/wraptool/config.yaml",
		Probed:     true,
		Summary:    Summary{Fail: 2, Warn: 5, Suppressed: 1, Info: 3, Pass: 22, Skipped: 6},
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
		Schema          int          `json:"schema"`
		ConfigPath      string       `json:"config_path"`
		WraptoolVersion string       `json:"wraptool_version"`
		Probed          bool         `json:"probed"`
		Summary         Summary      `json:"summary"`
		Findings        []Finding    `json:"findings"`
		Skipped         []SkipRecord `json:"skipped"`
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
