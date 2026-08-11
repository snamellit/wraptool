//go:build !unix

package doctor

import "io/fs"

// statOwnership has no ownership bits to report on a non-Unix platform
// (Windows): both are reported unknown, matching os.Getuid's own -1
// convention there.
func statOwnership(fs.FileInfo) (uid, gid int) {
	return -1, -1
}
