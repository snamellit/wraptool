package executor

import (
	"strings"
	"testing"
)

func TestScrubCredentials(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantGone string // substring that should NOT appear in output
	}{
		{
			name:     "bearer token",
			input:    "Authorization: Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.abc123",
			wantGone: "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:     "api key assignment",
			input:    "API_KEY=sk-1234567890abcdef1234567890abcdef",
			wantGone: "sk-1234567890abcdef1234567890abcdef",
		},
		{
			name:     "aws access key",
			input:    "using key AKIAIOSFODNN7EXAMPLE for auth",
			wantGone: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:     "ssh private key",
			input:    "-----BEGIN RSA PRIVATE KEY-----\nMIIEow...",
			wantGone: "-----BEGIN RSA PRIVATE KEY-----",
		},
		{
			name:     "password in config",
			input:    `password: "supersecretpassword123"`,
			wantGone: "supersecretpassword123",
		},
		{
			name:     "client secret",
			input:    "client_secret=abcdefghijklmnopqrstuvwxyz1234567890abcd",
			wantGone: "abcdefghijklmnopqrstuvwxyz1234567890abcd",
		},
		{
			name:     "safe output unchanged",
			input:    "commit abc123\nAuthor: user <user@example.com>\nfixed the bug",
			wantGone: "", // nothing should be scrubbed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ScrubCredentials(tt.input)
			if tt.wantGone != "" && strings.Contains(result, tt.wantGone) {
				t.Errorf("expected %q to be scrubbed, but output contains it:\n%s", tt.wantGone, result)
			}
			if tt.wantGone != "" && !strings.Contains(result, redacted) {
				t.Errorf("expected [REDACTED] in output, got:\n%s", result)
			}
			if tt.wantGone == "" && result != tt.input {
				t.Errorf("expected output unchanged, but got:\n%s", result)
			}
		})
	}
}

func TestScrubEnvVars(t *testing.T) {
	env := map[string]string{
		"SECRET_TOKEN": "my-very-secret-token-value",
		"SHORT":        "abc", // too short, should not be scrubbed
	}

	input := "error: auth failed with my-very-secret-token-value for host"
	result := ScrubEnvVars(input, env)

	if strings.Contains(result, "my-very-secret-token-value") {
		t.Error("expected env var value to be scrubbed")
	}
	if !strings.Contains(result, redacted) {
		t.Error("expected [REDACTED] in output")
	}

	// Short values should not be scrubbed (too many false positives).
	input2 := "the value abc is fine"
	result2 := ScrubEnvVars(input2, env)
	if !strings.Contains(result2, "abc") {
		t.Error("short env var values should not be scrubbed")
	}
}
