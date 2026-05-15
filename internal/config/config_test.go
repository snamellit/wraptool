package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Create a dummy binary for validation.
	binPath := filepath.Join(dir, "git")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	cfgContent := `
server:
  transport: stdio
  log_level: debug
tools:
  git:
    binary: ` + binPath + `
    timeout: 60s
    allow:
      - subcommand: [status]
        flags: ["--short"]
      - subcommand: [commit]
        flags: ["--message"]
        flag_constraints:
          "--message":
            required: true
    deny:
      - subcommand: [push]
`

	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.Transport != "stdio" {
		t.Errorf("transport = %q, want stdio", cfg.Server.Transport)
	}

	git, ok := cfg.Tools["git"]
	if !ok {
		t.Fatal("git tool not found")
	}

	if git.Binary != binPath {
		t.Errorf("binary = %q, want %q", git.Binary, binPath)
	}

	if len(git.Allow) != 2 {
		t.Errorf("len(allow) = %d, want 2", len(git.Allow))
	}

	if len(git.Deny) != 1 {
		t.Errorf("len(deny) = %d, want 1", len(git.Deny))
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error: %v", err)
	}
}

func TestValidate_InvalidTransport(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Transport: "grpc"},
		Tools: map[string]ToolConfig{
			"git": {Binary: "/usr/bin/git", Allow: []AllowRule{{Subcommand: []string{"status"}}}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid transport")
	}
}

func TestValidate_SSERequiresListen(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Transport: "sse"},
		Tools: map[string]ToolConfig{
			"git": {Binary: "/usr/bin/git", Allow: []AllowRule{{Subcommand: []string{"status"}}}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for SSE without listen")
	}
}

func TestValidate_RelativeBinaryPath(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {Binary: "git", Allow: []AllowRule{{Subcommand: []string{"status"}}}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for relative binary path")
	}
}

func TestValidate_NoAllowRules(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "git")
	os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755)

	cfg := &Config{
		Server: ServerConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {Binary: binPath, Allow: []AllowRule{}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for no allow rules")
	}
}

func TestValidate_EmptySubcommand(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "git")
	os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755)

	cfg := &Config{
		Server: ServerConfig{Transport: "stdio"},
		Tools: map[string]ToolConfig{
			"git": {Binary: binPath, Allow: []AllowRule{{Subcommand: []string{}}}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error for empty subcommand")
	}
}

func TestResolvedEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "test.env")
	os.WriteFile(envFile, []byte("KEY1=value1\nKEY2=value2\n# comment\n\nKEY3=value3"), 0600)

	tc := &ToolConfig{
		EnvFiles: []string{envFile},
		Env: map[string]string{
			"KEY2": "override", // should override env file
		},
	}

	env, err := tc.ResolvedEnv()
	if err != nil {
		t.Fatalf("ResolvedEnv() error: %v", err)
	}

	if env["KEY1"] != "value1" {
		t.Errorf("KEY1 = %q, want value1", env["KEY1"])
	}
	if env["KEY2"] != "override" {
		t.Errorf("KEY2 = %q, want override", env["KEY2"])
	}
	if env["KEY3"] != "value3" {
		t.Errorf("KEY3 = %q, want value3", env["KEY3"])
	}
}
