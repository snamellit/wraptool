package paths

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type contextKey string

const (
	clientCwdKey       contextKey = "client_cwd"
	peerPIDKey         contextKey = "peer_pid"
	allowedCWDRootsKey contextKey = "allowed_cwd_roots"
)

// WithClientCWD returns a new context containing the client's explicit CWD query parameter.
func WithClientCWD(ctx context.Context, cwd string) context.Context {
	return context.WithValue(ctx, clientCwdKey, cwd)
}

// WithPeerPID returns a new context containing the client connection's peer PID.
func WithPeerPID(ctx context.Context, pid int) context.Context {
	return context.WithValue(ctx, peerPIDKey, pid)
}

// WithAllowedCWDRoots stores the operator-configured allowlist of directory roots
// that a client may request as a working directory. An empty/absent list disables
// containment (warn-only default) — see F5 in
// design/security-audit-policy-enforcement.md.
func WithAllowedCWDRoots(ctx context.Context, roots []string) context.Context {
	return context.WithValue(ctx, allowedCWDRootsKey, roots)
}

// ResolveCWD resolves the working directory to run a command in, from (in order):
// the client's explicit CWD, the Unix peer's /proc/<pid>/cwd, then the tool's
// configured working directory. When the directory comes from an untrusted source
// (client or peer) and allowed_cwd_roots is configured, it must resolve to a
// location under one of those roots, else ResolveCWD returns an error and the call
// is refused. The configured working_dir (trusted) is never containment-checked.
func ResolveCWD(ctx context.Context, configWorkingDir string) (string, error) {
	roots, _ := ctx.Value(allowedCWDRootsKey).([]string)

	// 1. Explicit CWD passed from the client via query param.
	if val, ok := ctx.Value(clientCwdKey).(string); ok && val != "" {
		if err := containCWD(val, roots); err != nil {
			return "", err
		}
		return val, nil
	}

	// 2. Peer PID from a Unix socket, resolving /proc/<pid>/cwd.
	if pid, ok := ctx.Value(peerPIDKey).(int); ok && pid > 0 {
		link := fmt.Sprintf("/proc/%d/cwd", pid)
		if target, err := os.Readlink(link); err == nil {
			if err := containCWD(target, roots); err != nil {
				return "", err
			}
			return target, nil
		}
	}

	// 3. Fallback to the tool config's working directory (trusted; not contained).
	return configWorkingDir, nil
}

// containCWD enforces that dir resolves (symlinks included) to a location within
// one of roots. An empty roots list disables enforcement (warn-only default; the
// operator was warned at startup). A non-existent or escaping dir is rejected.
func containCWD(dir string, roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("working directory %q is not accessible: %w", dir, err)
	}
	for _, root := range roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rr, resolved)
		if err != nil {
			continue
		}
		// Inside the root iff the relative path doesn't climb out of it.
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return nil
		}
	}
	return fmt.Errorf("working directory %q is outside the configured mcp.allowed_cwd_roots", dir)
}
