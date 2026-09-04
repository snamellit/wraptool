package config

import (
	"os"
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
		if got := FlagParamName(flag); got != want {
			t.Errorf("FlagParamName(%q) = %q, want %q", flag, got, want)
		}
	}
}

func TestSanitizeToolName(t *testing.T) {
	// The permitted set is the intersection wraptool mints into: Gemini's
	// `[A-Za-z_][A-Za-z0-9_.:-]{0,127}` minus ":".
	cases := []struct{ in, want string }{
		{"sigil_sigil/lookup", "sigil_sigil_lookup"},                   // the reported agy/Gemini failure
		{"kubectl_config_get-contexts", "kubectl_config_get-contexts"}, // dashes are fine, unchanged
		{"9lives", "_9lives"},                                          // must not start with a digit
		{".hidden", "_.hidden"},
		{"a b:c", "a_b_c"},
		{"café/x", "caf__x"}, // one "_" per RUNE, not per byte
		{"", "_"},
		{strings.Repeat("a", MaxToolNameLen), strings.Repeat("a", MaxToolNameLen)}, // exactly at the cap, untouched
	}
	for _, c := range cases {
		if got := SanitizeToolName(c.in); got != c.want {
			t.Errorf("SanitizeToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Over-long names are truncated to the cap and kept distinct by a digest
	// of the whole pre-truncation name — a shared prefix past the cut must not
	// collapse two tools onto one registered name.
	long := strings.Repeat("a", MaxToolNameLen*2)
	for _, in := range []string{long, long + "_zzz", strings.Repeat("a", MaxToolNameLen+1)} {
		if got := SanitizeToolName(in); len(got) != MaxToolNameLen {
			t.Errorf("SanitizeToolName(<%d chars>) has length %d, want %d", len(in), len(got), MaxToolNameLen)
		}
	}
	if a, b := SanitizeToolName(long), SanitizeToolName(long+"_zzz"); a == b {
		t.Errorf("two long names sharing a prefix past the cut both sanitize to %q", a)
	}

	// Idempotence is what lets mcptools.NewRegisteredTool re-apply this at the
	// registration choke point without changing an already-minted name.
	for _, in := range []string{"sigil_sigil/lookup", "9lives", "", long, "café/x"} {
		once := SanitizeToolName(in)
		if twice := SanitizeToolName(once); twice != once {
			t.Errorf("SanitizeToolName not idempotent for %q: %q then %q", in, once, twice)
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
