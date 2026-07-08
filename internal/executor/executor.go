package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"
)

const defaultMaxOutput = 1 << 20 // 1MB

// defaultPath is the minimal base PATH for child processes.
const defaultPath = "/usr/bin:/bin"

// childPath returns the PATH for a child process: the tool binary's own
// directory prepended to the server's inherited PATH (falling back to a minimal
// base when unset). We inherit PATH — and only PATH, never the rest of
// wraptool's environment — so a tool can find its genuine runtime dependencies
// (e.g. git → ssh, which on a store-based distro like Guix lives in a separate
// prefix, not next to the git binary) without any parent credentials leaking.
// The binary's own directory is still prepended for co-located helpers (e.g.
// kubectl → gke-gcloud-auth-plugin), and skipped when empty or already present.
func childPath(binary string) string {
	base := os.Getenv("PATH")
	if base == "" {
		base = defaultPath
	}
	dir := filepath.Dir(binary)
	if dir == "" || dir == "." || pathContains(base, dir) {
		return base
	}
	return dir + ":" + base
}

// pathContains reports whether dir is already an entry in a PATH-style list.
func pathContains(pathList, dir string) bool {
	for _, p := range filepath.SplitList(pathList) {
		if p == dir {
			return true
		}
	}
	return false
}

// ChildEnv builds the minimal, clean environment for a wrapped child process:
// PATH (the binary's own dir prepended to the server's PATH, see childPath),
// HOME, LANG, plus the tool's resolved env vars. Apart from PATH it never
// inherits wraptool's own environment, so credentials cannot leak from the
// parent. Both CLI tools and spawned stdio MCP servers use this, keeping their
// env isolation identical.
func ChildEnv(binary string, resolved map[string]string) []string {
	envSlice := []string{
		"PATH=" + childPath(binary),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
	}
	for k, v := range resolved {
		envSlice = append(envSlice, k+"="+v)
	}
	return envSlice
}

// Scrub removes credentials and the configured env-var values from a piece of
// output before it is returned to the AI context.
func Scrub(s string, env map[string]string) string {
	s = ScrubCredentials(s)
	s = ScrubEnvVars(s, env)
	return s
}

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

	// Reject flag values that would be reparsed as options (option injection).
	if err := SanitizeFlagValues(flags); err != nil {
		return nil, fmt.Errorf("flag value check failed: %w", err)
	}

	// Resolve environment.
	env, err := tc.ResolvedEnv()
	if err != nil {
		return nil, fmt.Errorf("resolving environment: %w", err)
	}

	cmd := exec.CommandContext(ctx, tc.Binary, args...)

	// Minimal clean environment — never inherit wraptool's own env (prevents
	// credential leakage to child commands). See ChildEnv for the PATH rationale
	// (co-located helper executables like gke-gcloud-auth-plugin must resolve).
	cmd.Env = ChildEnv(tc.Binary, env)

	cwd, err := paths.ResolveCWD(ctx, tc.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("resolving working directory: %w", err)
	}
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
	result := &Result{
		Stdout:    Scrub(stdout.String(), env),
		Stderr:    Scrub(stderr.String(), env),
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
