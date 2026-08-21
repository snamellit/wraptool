package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// baseConfig returns a minimal valid config with one tool whose binary is the
// running test executable (guaranteed to be an absolute, regular file).
func baseConfig(flags []string) *Config {
	return &Config{
		MCP: MCPConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {
				Binary: os.Args[0],
				Allow:  []AllowRule{{Subcommand: []string{"add"}, Flags: flags}},
			},
		},
	}
}

func TestValidateAcceptsNormalFlags(t *testing.T) {
	if err := baseConfig([]string{"-A", "--dry-run", "--no-edit"}).Validate(); err != nil {
		t.Fatalf("valid flags rejected: %v", err)
	}
}

func TestValidateRejectsBadFlagParamNames(t *testing.T) {
	cases := map[string]string{
		"end-of-options separator": "--",
		"bare dash":                "-",
		"embedded space":           "--foo bar",
		"slash":                    "--ns/x",
		"equals":                   "--opt=val",
		"comma-joined":             "-n, --namespace",
	}
	for name, flag := range cases {
		t.Run(name, func(t *testing.T) {
			err := baseConfig([]string{flag}).Validate()
			if err == nil {
				t.Fatalf("flag %q accepted, want rejection", flag)
			}
			if !strings.Contains(err.Error(), "invalid schema property") {
				t.Errorf("error = %q, want it to mention invalid schema property", err)
			}
		})
	}
}

func TestFlagParamName(t *testing.T) {
	cases := map[string]string{
		"--set-upstream": "set_upstream",
		"-m":             "m",
		"--":             "",
		"-":              "",
		"--no-edit":      "no_edit",
	}
	for flag, want := range cases {
		if got := flagParamName(flag); got != want {
			t.Errorf("flagParamName(%q) = %q, want %q", flag, got, want)
		}
	}
}

func TestSanitizeToolName(t *testing.T) {
	// The permitted set is the intersection wraptool mints into: Gemini's
	// `[A-Za-z_][A-Za-z0-9_.:-]{0,127}` minus ":".
	valid := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
	cases := []struct{ in, want string }{
		{"sigil_sigil/lookup", "sigil_sigil_lookup"},                   // the reported agy/Gemini failure
		{"kubectl_config_get-contexts", "kubectl_config_get-contexts"}, // dashes are fine, unchanged
		{"git_push", "git_push"},
		{"9lives", "_9lives"}, // must not start with a digit
		{"a b:c", "a_b_c"},
		{"", "_"},
	}
	for _, c := range cases {
		got := SanitizeToolName(c.in)
		if got != c.want {
			t.Errorf("SanitizeToolName(%q) = %q, want %q", c.in, got, c.want)
		}
		if !valid.MatchString(got) {
			t.Errorf("SanitizeToolName(%q) = %q, which still violates the tool-name grammar", c.in, got)
		}
	}
}

// Two upstream tools differing only in an unsafe character collapse to one
// registered name once sanitized — config validation must reject that rather
// than silently registering the same name twice.
func TestValidateRejectsCollisionCreatedBySanitizing(t *testing.T) {
	cfg := mcpBaseConfig(map[string]MCPServerConfig{
		"sigil": {
			Command:    os.Args[0],
			NamePrefix: "sigil",
			Allow:      []MCPAllowRule{{Tool: "a/b"}, {Tool: "a_b"}},
		},
	})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("err = %v, want a collision error for a/b and a_b (both sanitize to sigil_a_b)", err)
	}
}
