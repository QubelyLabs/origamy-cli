//go:build windows

package cmd

// processAlive cannot tell here, so a lock is never taken over as stale.
func processAlive(int) bool { return true }
