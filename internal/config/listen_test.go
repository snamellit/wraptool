package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8717":   true,
		"127.0.0.1":        true,
		"[::1]:8717":       true,
		"localhost:8717":   true,
		"0.0.0.0:8717":     false,
		":8717":            false,
		"192.168.1.5:8717": false,
		"[::]:8717":        false,
	}
	for listen, want := range cases {
		if got := ListenIsLoopback(listen); got != want {
			t.Errorf("ListenIsLoopback(%q) = %v, want %v", listen, got, want)
		}
	}
}

func TestValidateRequiresTokenForNonLoopbackListen(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := map[string]ToolConfig{"git": {Binary: bin, Allow: []AllowRule{{Subcommand: []string{"pull"}}}}}

	// Non-loopback bind without a token: rejected.
	cfg := &Config{MCP: MCPConfig{Transport: "sse", Listen: "0.0.0.0:8717"}, Tools: tool}
	if err := cfg.Validate(); err == nil {
		t.Error("expected an error for a non-loopback listen without auth_token_file")
	}

	// Same bind with a token: accepted.
	cfg.MCP.AuthTokenFile = tokenFile
	if err := cfg.Validate(); err != nil {
		t.Errorf("non-loopback listen with a token should validate: %v", err)
	}

	// Loopback bind needs no token.
	loop := &Config{MCP: MCPConfig{Transport: "sse", Listen: "127.0.0.1:8717"}, Tools: tool}
	if err := loop.Validate(); err != nil {
		t.Errorf("loopback listen without a token should validate: %v", err)
	}
}
