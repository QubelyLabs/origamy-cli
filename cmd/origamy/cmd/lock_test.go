package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// deadPID is the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

func TestLockProject(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, projectLockFile)
	host, _ := os.Hostname()

	unlock, err := lockProject(dir, "origamy upgrade")
	if err != nil {
		t.Fatal(err)
	}
	held, ok := readProjectLock(path)
	if !ok || held.pid != os.Getpid() || held.host != host || held.command != "origamy upgrade" || held.started == "" {
		t.Fatalf("lock = %+v", held)
	}
	// Held by this (live) process: another run is refused, and names it.
	if _, err := lockProject(dir, "origamy deploy"); err == nil || !strings.Contains(err.Error(), "origamy upgrade, pid ") {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock() // once
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unlock left the file: %v", err)
	}

	// Concurrent runs: exactly one gets it.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var got []func()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u, err := lockProject(dir, "origamy upgrade"); err == nil {
				mu.Lock()
				got = append(got, u)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 1 {
		t.Fatalf("%d runs took the lock", len(got))
	}
	got[0]()

	for _, tc := range []struct {
		name, body string
		taken      bool
	}{
		// A run on this host whose process is gone: stale, taken over.
		{"dead process here", projectLock{pid: deadPID(t), host: host, started: "x", command: "origamy upgrade"}.String(), true},
		// A live one (the test's parent), another host's (cannot be
		// checked), or one that names no process: refused.
		{"live process here", projectLock{pid: os.Getppid(), host: host, started: "x", command: "origamy deploy"}.String(), false},
		{"another host", projectLock{pid: deadPID(t), host: host + "-elsewhere", started: "x", command: "origamy deploy"}.String(), false},
		{"unreadable", "", false},
		{"garbled", "not a lock\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, path, tc.body)
			unlock, err := lockProject(dir, "origamy upgrade")
			if (err == nil) != tc.taken {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				if readFile(t, path) != tc.body {
					t.Fatal("a refused run changed the lock")
				}
				_ = os.Remove(path)
				return
			}
			unlock()
		})
	}

	// unlock removes only its own lock: one that was replaced (by hand) stays.
	unlock, err = lockProject(dir, "origamy upgrade")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "pid=1\nhost=other\n")
	unlock()
	if readFile(t, path) != "pid=1\nhost=other\n" {
		t.Fatal("unlock removed a lock that was not its own")
	}
}

func TestLockFail(t *testing.T) {
	out := captureStdout(t, func() {
		_ = lockFail(&errProjectLocked{path: "/dp/.origamy.lock", held: projectLock{pid: 42, host: "h", started: "s", command: "origamy deploy"}, ok: true}, "Nothing has been changed.")
	})
	if !strings.Contains(out, "Another origamy run is working on this data plane (origamy deploy, pid 42 on h, started s).") || !strings.Contains(out, "remove /dp/.origamy.lock") {
		t.Fatalf("%s", out)
	}
	out = captureStdout(t, func() { _ = lockFail(&errProjectLocked{path: "/dp/.origamy.lock"}, "Nothing has been changed.") })
	if !strings.Contains(out, "exists but does not say which") {
		t.Fatalf("%s", out)
	}
}

func TestOnExit(t *testing.T) {
	var ran []string
	a := onExit(func() { ran = append(ran, "a") })
	b := onExit(func() { ran = append(ran, "b") })
	a()
	a()
	exitMu.Lock()
	n := len(exitHooks)
	exitMu.Unlock()
	if n != 1 || strings.Join(ran, "") != "a" {
		t.Fatalf("hooks %d, ran %v", n, ran)
	}
	b()
	exitMu.Lock()
	n = len(exitHooks)
	exitMu.Unlock()
	if n != 0 {
		t.Fatalf("hooks left: %d", n)
	}

	// A prompt ending the CLI in the middle of a deploy plan: the plan's
	// hook (which unlocks) and the lock's own each run once, and the project
	// is left with neither the lock nor the download.
	dir := t.TempDir()
	unlock, err := lockProject(dir, "origamy deploy")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp(dir, bundleTempPrefix)
	if err != nil {
		t.Fatal(err)
	}
	ran = nil
	onExit(func() {
		ran = append(ran, "plan")
		_ = os.RemoveAll(tmp)
		unlock()
	})
	runExitHooks()
	runExitHooks()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 || strings.Join(ran, ",") != "plan" {
		t.Fatalf("left %v, ran %v", entries, ran)
	}
}
