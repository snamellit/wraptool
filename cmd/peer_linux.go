//go:build linux

package cmd

import (
	"context"
	"net"
	"syscall"

	"github.com/pti/wraptool/internal/paths"
)

func connContextWithPeerPID(ctx context.Context, c net.Conn) context.Context {
	if unixConn, ok := c.(*net.UnixConn); ok {
		rawConn, err := unixConn.SyscallConn()
		if err == nil {
			var ucred *syscall.Ucred
			var opErr error
			controlErr := rawConn.Control(func(fd uintptr) {
				ucred, opErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
			})
			if controlErr == nil && opErr == nil && ucred != nil {
				return paths.WithPeerPID(ctx, int(ucred.Pid))
			}
		}
	}
	return ctx
}
