//go:build windows

package cmd

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockRangeHigh is the high half of the offset of the one byte that is
// locked, far past what the file holds: Windows locks are mandatory, and a
// run that is refused must still read who holds the lock.
const lockRangeHigh = 0x7fffffff

// lockFile takes the exclusive LockFileEx lock on f without waiting;
// errLockHeld when another handle holds it. Windows gives it up when the
// process ends.
func lockFile(f *os.File) error {
	ol := windows.Overlapped{OffsetHigh: lockRangeHigh}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	switch {
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return errLockHeld
	case errors.Is(err, windows.ERROR_NOT_SUPPORTED), errors.Is(err, windows.ERROR_INVALID_FUNCTION):
		// No byte-range locks on this file system: lockExclusive.
		return errLockUnsupported
	}
	return err
}

// pidAlive reports whether process pid runs on this host (one this user may
// not query counts).
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}

func unlockFile(f *os.File) error {
	ol := windows.Overlapped{OffsetHigh: lockRangeHigh}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

// releaseLock gives the lock up, then removes the lock file when it was still
// this run's. Windows cannot remove a file another run has open, so a run
// that opened it in between keeps it.
func releaseLock(f *os.File, path string) {
	same := samePath(f, path)
	_ = unlockFile(f)
	_ = f.Close()
	if same {
		_ = os.Remove(path)
	}
}
