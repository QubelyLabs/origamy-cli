package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── One run per Docker project ───────────────────────────────────────────────

// projectLockFile, in a Docker project's directory, is held by the one
// `origamy upgrade` or in-place `origamy deploy` working on it. Two at once
// could interleave one's storage-reset drop with the other's checks of the
// events table, or swap bundle files under each other.
const projectLockFile = ".origamy.lock"

// projectLock is what a lock file says about the run holding it.
type projectLock struct {
	pid              int
	host, started    string
	command, rawText string
}

func (l projectLock) String() string {
	return fmt.Sprintf("pid=%d\nhost=%s\nstarted=%s\ncommand=%s\n", l.pid, l.host, l.started, l.command)
}

// describe names the run for a message ("origamy upgrade, pid 42 on host,
// started …").
func (l projectLock) describe() string {
	return fmt.Sprintf("%s, pid %d on %s, started %s", orDash(l.command), l.pid, orDash(l.host), orDash(l.started))
}

// readProjectLock parses a lock file; ok is false when it does not name a
// process (empty, truncated, someone else's file).
func readProjectLock(path string) (l projectLock, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return projectLock{}, false
	}
	l.rawText = string(b)
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch strings.TrimSpace(k) {
		case "pid":
			l.pid, _ = strconv.Atoi(strings.TrimSpace(v))
		case "host":
			l.host = strings.TrimSpace(v)
		case "started":
			l.started = strings.TrimSpace(v)
		case "command":
			l.command = strings.TrimSpace(v)
		}
	}
	return l, l.pid > 0
}

// errProjectLocked is a lock another run holds (or a lock file that cannot
// be read).
type errProjectLocked struct {
	path string
	held projectLock
	ok   bool
}

func (e *errProjectLocked) Error() string {
	if !e.ok {
		return e.path + " exists but does not name the run holding it"
	}
	return "held by " + e.held.describe()
}

// lockProject takes dir's lock for command, which a refused run names. The
// lock file is created exclusively (O_EXCL) and records the process, host and
// start time. A lock left on this host by a process that is gone (killed,
// crashed, interrupted) is taken over; one held by a live process, by another
// host (its process cannot be checked from here), or that cannot be read is
// refused with errProjectLocked. unlock removes it (only while it is still
// this run's), and so does a prompt that ends the CLI (exitNow).
func lockProject(dir, command string) (unlock func(), err error) {
	path := filepath.Join(dir, projectLockFile)
	host, _ := os.Hostname()
	me := projectLock{pid: os.Getpid(), host: host, started: time.Now().UTC().Format(time.RFC3339), command: command}
	body := []byte(me.String())
	for range 2 {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, werr := f.Write(body)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				_ = os.Remove(path)
				return nil, werr
			}
			return onExit(func() {
				if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, body) {
					_ = os.Remove(path)
				}
			}), nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		held, ok := readProjectLock(path)
		if !ok || held.host != host || held.pid == me.pid || processAlive(held.pid) {
			return nil, &errProjectLocked{path: path, held: held, ok: ok}
		}
		// Stale: removed only while it still says what was judged stale.
		if b, err := os.ReadFile(path); err == nil && string(b) == held.rawText {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			ui.Step("Removed the lock an earlier run left in %s (%s; that process is gone).", projectLockFile, held.describe())
		}
	}
	held, ok := readProjectLock(path)
	return nil, &errProjectLocked{path: path, held: held, ok: ok}
}

// lockFail explains a lock lockProject could not take; nothing was done.
func lockFail(err error, nothing string) error {
	var locked *errProjectLocked
	switch {
	case errors.As(err, &locked) && locked.ok:
		return fail("Another origamy run is working on this data plane ("+locked.held.describe()+").",
			"Wait for it to finish, then rerun. "+nothing+"\nIf no such run is going on (it was stopped on another host, say), remove "+locked.path+" and rerun.")
	case errors.As(err, &locked):
		return fail("Another origamy run may be working on this data plane: "+locked.path+" exists but does not say which.",
			"Wait for any other origamy run here to finish. "+nothing+"\nIf none is going on, remove "+locked.path+" and rerun.")
	}
	return fail("Could not take the lock that keeps two origamy runs off this data plane.", err.Error()+"\n"+nothing)
}

// ── Leaving the project as it was when a prompt ends the CLI ─────────────────

var (
	exitMu    sync.Mutex
	exitHooks []*func()
)

// onExit registers f, which undoes something a run put in the project (its
// lock, a bundle download), to run if a prompt ends the CLI (exitNow), which
// skips deferred calls. done runs f instead; either way f runs once.
func onExit(f func()) (done func()) {
	var once sync.Once
	h := new(func())
	*h = func() {
		once.Do(func() {
			exitMu.Lock()
			exitHooks = slices.DeleteFunc(exitHooks, func(x *func()) bool { return x == h })
			exitMu.Unlock()
			f()
		})
	}
	exitMu.Lock()
	exitHooks = append(exitHooks, h)
	exitMu.Unlock()
	return *h
}

// exitNow ends the CLI with code after the onExit hooks.
func exitNow(code int) {
	runExitHooks()
	os.Exit(code)
}

// runExitHooks runs the onExit hooks still registered, newest first.
func runExitHooks() {
	exitMu.Lock()
	hooks := slices.Clone(exitHooks)
	exitMu.Unlock()
	for i := len(hooks) - 1; i >= 0; i-- {
		(*hooks[i])()
	}
}
