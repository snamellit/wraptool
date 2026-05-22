//go:build !linux

package cmd

import (
	"context"
	"net"
)

func connContextWithPeerPID(ctx context.Context, c net.Conn) context.Context {
	return ctx
}
