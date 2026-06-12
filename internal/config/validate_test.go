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
		Server: ServerConfig{Transport: "stdio"},
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
