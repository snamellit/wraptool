//go:build unix

package doctor

import (
	"io/fs"
	"syscall"
)

// statOwnership extracts uid/gid from a stdlib fs.FileInfo on a Unix-family
// platform. This file, its !unix sibling, and host.go are the only places in
// this package that touch syscall/os directly (see host.go's Env doc comment).
func statOwnership(fi fs.FileInfo) (uid, gid int) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(st.Uid), int(st.Gid)
}
