//go:build !windows

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestExitOnSignal: SIGINT, SIGTERM or SIGHUP end a run that holds the lock
// and has a bundle download in the project with exit code 130, and leave
// neither behind.
func TestExitOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			helper := lockHelper(t, dir, true)
			if entries, _ := os.ReadDir(dir); len(entries) != 2 {
				t.Fatalf("the helper holds %v", entries)
			}
			if err := helper.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			var exit *exec.ExitError
			if err := helper.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 130 {
				t.Fatalf("exit: %v", err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("left behind: %v", entries)
			}
		})
	}
}
