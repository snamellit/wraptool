package doctor

import (
	"context"
	"strings"
	"testing"
)

func TestRenderHumanGoldenReport(t *testing.T) {
	checks := []Check{
		doctorFindingCheck("policy.dangerous", SeverityFail, "git -c core.sshCommand is allowed"),
		doctorFindingCheck("config.loose-perm", SeverityWarn, "config.yaml is group-writable"),
		doctorFindingCheck("env.container-backend", SeverityInfo, "found: docker (/usr/bin/docker)"),
		doctorSkippedCheck("tools.binary-writable", "no readable config file"),
	}
	in := &Input{Env: NewMapEnv(), GOOS: "linux", ConfigPath: "/home/user/.config/wraptool/config.yaml", ConfigState: ConfigLoaded}
	r := Run(context.Background(), in, RunOptions{Checks: checks})

	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}

	want := `wraptool doctor — /home/user/.config/wraptool/config.yaml

environment:
  info  env.container-backend: found: docker (/usr/bin/docker)
        → fix it

configuration:
  warn  config.loose-perm: config.yaml is group-writable
        → fix it

policy:
  FAIL  policy.dangerous: git -c core.sshCommand is allowed
        → fix it

1 failed, 1 warnings, 1 info, 0 passed, 1 skipped.
`
	if buf.String() != want {
		t.Errorf("RenderHuman output mismatch:\n--- got ---\n%s\n--- want ---\n%s", buf.String(), want)
	}
}

func TestRenderHumanLoadedConfigHeader(t *testing.T) {
	r := Report{ConfigState: ConfigLoaded, ConfigPath: "/x/config.yaml"}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "wraptool doctor — /x/config.yaml\n") {
		t.Errorf("unexpected header, got:\n%s", buf.String())
	}
}

func TestRenderHumanAbsentConfigHeader(t *testing.T) {
	r := Run(context.Background(), &Input{
		Env: NewMapEnv(), GOOS: "linux",
		ConfigPath:  "/home/user/.config/wraptool/config.yaml",
		ConfigState: ConfigAbsent,
	}, RunOptions{Checks: []Check{doctorFindingCheck("env.container-backend", SeverityInfo, "none found on PATH")}})

	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if !strings.Contains(buf.String(), "no config file yet (/home/user/.config/wraptool/config.yaml)") {
		t.Errorf("missing absent-config header, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "info  no config yet") {
		t.Errorf("missing absent-config info line, got:\n%s", buf.String())
	}
}

func TestRenderHumanUnreadableConfigHeader(t *testing.T) {
	r := Report{ConfigState: ConfigUnreadable, ConfigPath: "/x/config.yaml", Incomplete: true, IncompleteReason: "parsing config: yaml: bad indent"}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if !strings.Contains(buf.String(), "/x/config.yaml (unreadable)") {
		t.Errorf("missing unreadable-config header, got:\n%s", buf.String())
	}
}

// TestRenderHumanIncompleteBanner covers the ConfigUnreadable-only case
// (§10.1): every selected check still ran, so Aborted is false and the
// "did not run" clause must NOT appear.
func TestRenderHumanIncompleteBanner(t *testing.T) {
	r := Report{ConfigState: ConfigUnreadable, ConfigPath: "/x/config.yaml", Incomplete: true, IncompleteReason: "parsing config: yaml: bad indent"}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if !strings.Contains(buf.String(), "doctor could not complete: parsing config: yaml: bad indent.") {
		t.Errorf("missing incomplete banner, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "did not run") {
		t.Errorf("ConfigUnreadable-only banner must not claim checks did not run (every selected check ran), got:\n%s", buf.String())
	}
}

// TestRenderHumanAbortedBanner covers a run cut short (registry error,
// selection error, Result.Fatal, or a malformed Result): Aborted is true and
// the "did not run" clause is accurate.
func TestRenderHumanAbortedBanner(t *testing.T) {
	r := Report{Incomplete: true, Aborted: true, IncompleteReason: "config is unparseable"}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if !strings.Contains(buf.String(), "doctor could not complete: config is unparseable. Checks after this point did not run.") {
		t.Errorf("missing aborted banner, got:\n%s", buf.String())
	}
}

func TestRenderHumanFooterSuppressedParenthetical(t *testing.T) {
	r := Report{Summary: Summary{Warn: 2, Suppressed: 1, Pass: 3}}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if !strings.Contains(buf.String(), "2 warnings (1 suppressed)") {
		t.Errorf("missing suppressed parenthetical, got:\n%s", buf.String())
	}
}

func TestRenderHumanFooterOmitsSuppressedParentheticalWhenZero(t *testing.T) {
	r := Report{Summary: Summary{Warn: 2}}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if strings.Contains(buf.String(), "suppressed") {
		t.Errorf("unexpected suppressed parenthetical, got:\n%s", buf.String())
	}
}

// TestRenderHumanSuppressedFindingNotPrinted is C2's own edit to this file
// (design §6.2): a suppressed finding is counted (the footer's parenthetical
// is asserted separately above) but must not appear in the body at all —
// neither its own group section nor its Detail/Remedy text.
func TestRenderHumanSuppressedFindingNotPrinted(t *testing.T) {
	r := Report{
		Findings: []Finding{
			{ID: "policy.dangerous-flag", Severity: SeverityWarn, Title: "t", Detail: "SUPPRESSED-DETAIL", Remedy: "fix it",
				Suppressed: true, SuppressionReason: "reviewed"},
			{ID: "config.symlinked", Severity: SeverityWarn, Title: "t", Detail: "STILL-VISIBLE-DETAIL", Remedy: "fix it"},
		},
		Summary: Summary{Warn: 2, Suppressed: 1},
	}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "SUPPRESSED-DETAIL") {
		t.Errorf("suppressed finding was printed, want it omitted from the body: %s", out)
	}
	if !strings.Contains(out, "STILL-VISIBLE-DETAIL") {
		t.Errorf("missing the non-suppressed finding, got: %s", out)
	}
	if !strings.Contains(out, "2 warnings (1 suppressed)") {
		t.Errorf("missing suppressed footer accounting for the omitted finding, got: %s", out)
	}
}

func TestRenderHumanNeverMentionsStdioTransport(t *testing.T) {
	r := Run(context.Background(), doctorVirginInput(), RunOptions{Checks: []Check{
		doctorFindingCheck("env.container-backend", SeverityInfo, "found: docker (/usr/bin/docker)"),
	}})
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if strings.Contains(buf.String(), "stdio") {
		t.Errorf("doctor output must never name the stdio MCP transport, got:\n%s", buf.String())
	}
}
