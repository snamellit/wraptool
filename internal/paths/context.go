package paths

import (
	"context"
	"fmt"
	"os"
)

type contextKey string

const (
	clientCwdKey contextKey = "client_cwd"
	peerPIDKey   contextKey = "peer_pid"
)

// WithClientCWD returns a new context containing the client's explicit CWD query parameter.
func WithClientCWD(ctx context.Context, cwd string) context.Context {
	return context.WithValue(ctx, clientCwdKey, cwd)
}

// WithPeerPID returns a new context containing the client connection's peer PID.
func WithPeerPID(ctx context.Context, pid int) context.Context {
	return context.WithValue(ctx, peerPIDKey, pid)
}

// ResolveCWD resolves the current working directory to run a command in.
// It checks the context for an explicit client CWD, then checks for a peer PID (resolving /proc/<pid>/cwd),
// and finally falls back to the configured tool working directory.
func ResolveCWD(ctx context.Context, configWorkingDir string) string {
	// 1. Explicit CWD passed from the client via query param
	if val, ok := ctx.Value(clientCwdKey).(string); ok && val != "" {
		return val
	}

	// 2. Peer PID from Unix socket, resolving /proc/<pid>/cwd
	if pid, ok := ctx.Value(peerPIDKey).(int); ok && pid > 0 {
		link := fmt.Sprintf("/proc/%d/cwd", pid)
		if target, err := os.Readlink(link); err == nil {
			return target
		}
	}

	// 3. Fallback to tool config's working directory
	return configWorkingDir
}
