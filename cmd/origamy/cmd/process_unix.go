//go:build !windows

package cmd

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with this pid runs on this host
// (one of another user's counts: signal 0 is refused, not unknown).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
