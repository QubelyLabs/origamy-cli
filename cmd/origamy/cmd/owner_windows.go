//go:build windows

package cmd

import "os"

// fileOwner reports nothing on Windows, where a file written next to .env
// and renamed over it takes its place with the directory's permissions.
func fileOwner(os.FileInfo) (uid, gid int, links uint64, ok bool) {
	return 0, 0, 1, false
}
