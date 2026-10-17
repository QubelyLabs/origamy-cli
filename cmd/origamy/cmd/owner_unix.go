//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// fileOwner returns the owner and group of the file fi describes, and how
// many hard links it has; ok is false when the system does not say.
func fileOwner(fi os.FileInfo) (uid, gid int, links uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 1, false
	}
	return int(st.Uid), int(st.Gid), uint64(st.Nlink), true
}
