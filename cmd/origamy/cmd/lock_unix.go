//go:build !windows

package cmd

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes the exclusive flock on f without waiting; errLockHeld when
// another open file holds it. The kernel gives it up when the process ends.
func lockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return errLockHeld
		case errors.Is(err, unix.ENOLCK), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.ENOSYS):
			// No advisory locks on this file system (NFS without a lock
			// manager, some SMB and FUSE mounts): lockExclusive.
			return errLockUnsupported
		}
		return err
	}
}

// pidAlive reports whether process pid runs on this host (one that runs as
// another user counts).
func pidAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

// releaseLock removes the lock file while still holding it (only when it is
// still this run's file), then gives the lock up: a run that opened the file
// before it was removed finds, once it has the lock, that the file is gone
// (samePath) and takes a new one.
func releaseLock(f *os.File, path string) {
	if samePath(f, path) {
		_ = os.Remove(path)
	}
	_ = unlockFile(f)
	_ = f.Close()
}
