package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"
)

const defaultMaxOutput = 1 << 20 // 1MB

// Result holds the output of a command execution.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool // true if output exceeded the limit
}

// Run executes a CLI command with the given tool configuration.
// It builds the argument list as: [subcommand...] [--flag value...] [-- positional...]
func Run(ctx context.Context, tc *config.ToolConfig, subcommand []string, flags map[string]string, positionalArgs []string) (*Result, error) {
	args := buildArgs(subcommand, flags, positionalArgs)

	// Sanitize all arguments.
	if err := SanitizeArgs(args); err != nil {
		return nil, fmt.Errorf("argument sanitization failed: %w", err)
	}

	// Sanitize positional args specifically (no leading dashes).
	if err := SanitizePositionalArgs(positionalArgs); err != nil {
		return nil, fmt.Errorf("positional argument check failed: %w", err)
	}

	// Resolve environment.
	env, err := tc.ResolvedEnv()
	if err != nil {
		return nil, fmt.Errorf("resolving environment: %w", err)
	}

	cmd := exec.CommandContext(ctx, tc.Binary, args...)

	// Set environment: always start with a minimal clean environment.
	// Never inherit the parent process environment — this prevents
	// credential leakage from the wraptool process to child commands.
	envSlice := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
	}
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}
	cmd.Env = envSlice

	cwd := paths.ResolveCWD(ctx, tc.WorkingDir)
	if cwd != "" {
		cmd.Dir = cwd
	}

	var stdout, stderr bytes.Buffer
	stdoutW := &limitedWriter{buf: &stdout, limit: defaultMaxOutput}
	stderrW := &limitedWriter{buf: &stderr, limit: defaultMaxOutput}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	err = cmd.Run()

	// Scrub credentials from output before returning.
	scrubbed := func(s string) string {
		s = ScrubCredentials(s)
		s = ScrubEnvVars(s, env)
		return s
	}

	result := &Result{
		Stdout:    scrubbed(stdout.String()),
		Stderr:    scrubbed(stderr.String()),
		Truncated: stdoutW.truncated || stderrW.truncated,
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}

	if err != nil {
		return nil, fmt.Errorf("executing %s: %w", tc.Binary, err)
	}

	return result, nil
}

func buildArgs(subcommand []string, flags map[string]string, positionalArgs []string) []string {
	args := make([]string, 0, len(subcommand)+len(flags)*2+len(positionalArgs)+1)

	args = append(args, subcommand...)

	for flag, value := range flags {
		args = append(args, flag)
		if value != "" {
			args = append(args, value)
		}
	}

	if len(positionalArgs) > 0 {
		args = append(args, "--")
		args = append(args, positionalArgs...)
	}

	return args
}

// limitedWriter wraps a buffer and stops writing after the limit.
type limitedWriter struct {
	buf       *bytes.Buffer
	limit     int
	written   int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil // silently discard
	}
	if len(p) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	n, err := w.buf.Write(p)
	w.written += n
	return n, err
}
