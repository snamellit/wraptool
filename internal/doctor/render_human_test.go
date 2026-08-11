package doctor

import (
	"context"
	"strings"
	"testing"
)

func TestRenderHumanGoldenReport(t *testing.T) {
	checks := []Check{
		findingCheck("policy.dangerous", SeverityFail, "git -c core.sshCommand is allowed"),
		findingCheck("config.loose-perm", SeverityWarn, "config.yaml is group-writable"),
		findingCheck("env.container-backend", SeverityInfo, "found: docker (/usr/bin/docker)"),
		skippedCheck("tools.binary-writable", "no readable config file"),
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
	}, RunOptions{Checks: []Check{findingCheck("env.container-backend", SeverityInfo, "none found on PATH")}})

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

func TestRenderHumanIncompleteBanner(t *testing.T) {
	r := Report{ConfigState: ConfigUnreadable, ConfigPath: "/x/config.yaml", Incomplete: true, IncompleteReason: "parsing config: yaml: bad indent"}
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if !strings.Contains(buf.String(), "doctor could not complete: parsing config: yaml: bad indent. Checks after this point did not run.") {
		t.Errorf("missing incomplete banner, got:\n%s", buf.String())
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

func TestRenderHumanNeverMentionsStdioTransport(t *testing.T) {
	r := Run(context.Background(), virginInput(), RunOptions{Checks: []Check{
		findingCheck("env.container-backend", SeverityInfo, "found: docker (/usr/bin/docker)"),
	}})
	var buf strings.Builder
	if err := RenderHuman(&buf, r); err != nil {
		t.Fatalf("RenderHuman: %v", err)
	}
	if strings.Contains(buf.String(), "stdio") {
		t.Errorf("doctor output must never name the stdio MCP transport, got:\n%s", buf.String())
	}
}
