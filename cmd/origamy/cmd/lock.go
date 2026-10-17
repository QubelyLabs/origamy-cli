package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── One run per Docker project ───────────────────────────────────────────────

// projectLockFile, in a Docker project's directory, is locked by the one
// `origamy upgrade`, `origamy deploy` or `origamy uninstall` working on it.
// Two at once could interleave one's storage reset with the other's checks of
// the events table, or swap bundle files under each other.
const projectLockFile = ".origamy.lock"

// projectLock is what a lock file says about the run holding it.
type projectLock struct {
	pid           int
	host, started string
	command       string
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
// process (empty, not written yet, someone else's file).
func readProjectLock(path string) (l projectLock, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return projectLock{}, false
	}
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

// errProjectLocked is a lock another run holds; held is what its file says
// (ok false when it says nothing yet).
type errProjectLocked struct {
	path string
	held projectLock
	ok   bool
}

func (e *errProjectLocked) Error() string {
	if !e.ok {
		return e.path + " is held by another run"
	}
	return "held by " + e.held.describe()
}

// errLockHeld is what lockFile returns when another open file holds the lock.
var errLockHeld = errors.New("the lock is held")

// lockProject takes dir's lock for command, which a refused run names. The
// lock is the operating system's advisory lock on .origamy.lock (flock;
// LockFileEx on Windows), held by this process's open file: a run that is
// killed, crashes or is interrupted gives it up with its process, so there
// is no stale lock to take over, and a file such a run left behind is simply
// locked again. Held by another run, it is refused with errProjectLocked,
// naming the process, host, start time and command the file records.
// unlock removes the file and gives the lock up, and so does a prompt or a
// signal that ends the CLI (exitNow).
func lockProject(dir, command string) (unlock func(), err error) {
	path := filepath.Join(dir, projectLockFile)
	host, _ := os.Hostname()
	me := projectLock{pid: os.Getpid(), host: host, started: time.Now().UTC().Format(time.RFC3339), command: command}
	for range 10 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		if err := lockFile(f); err != nil {
			_ = f.Close()
			if errors.Is(err, errLockHeld) {
				held, ok := readProjectLock(path)
				return nil, &errProjectLocked{path: path, held: held, ok: ok}
			}
			return nil, err
		}
		// The run that held it may have removed the file between this run's
		// open and its lock: the lock is then on a file no other run opens.
		if !samePath(f, path) {
			_ = unlockFile(f)
			_ = f.Close()
			continue
		}
		body := []byte(me.String())
		err = f.Truncate(0)
		if err == nil {
			_, err = f.WriteAt(body, 0)
		}
		if err != nil {
			releaseLock(f, path)
			return nil, err
		}
		return onExit(func() { releaseLock(f, path) }), nil
	}
	return nil, fmt.Errorf("%s kept changing while it was being locked", path)
}

// samePath reports whether the open file f is still the one at path.
func samePath(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Stat(path)
	return err == nil && os.SameFile(fi, pi)
}

// lockFail explains a lock lockProject could not take; nothing was done.
func lockFail(err error, nothing string) error {
	var locked *errProjectLocked
	switch {
	case errors.As(err, &locked) && locked.ok:
		return fail("Another origamy run is working on this data plane ("+locked.held.describe()+").",
			"Wait for it to finish, then rerun. "+nothing+"\nThe lock goes with that process: once it has ended, however it ended, the next run takes it.")
	case errors.As(err, &locked):
		return fail("Another origamy run is working on this data plane (it holds "+locked.path+").",
			"Wait for it to finish, then rerun. "+nothing)
	}
	return fail("Could not take the lock that keeps two origamy runs off this data plane.", err.Error()+"\n"+nothing)
}

// ── Leaving the project as it was when a prompt or a signal ends the CLI ────

var (
	exitMu    sync.Mutex
	exitHooks []*func()
)

// onExit registers f, which undoes something a run put in the project (its
// lock, a bundle download), to run if a prompt or a signal ends the CLI
// (exitNow), which skips deferred calls. done runs f instead; either way f
// runs once.
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

// exiting is set once the CLI is ending through exitNow.
var exiting atomic.Bool

// exitNow ends the CLI with code after the onExit hooks.
func exitNow(code int) {
	exiting.Store(true)
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

// exitGate holds an interrupt (exitOnSignal) back while a step that must not
// be cut in half runs: .env being replaced, the bundle files being swapped
// in or put back.
var exitGate sync.Mutex

// interruptSignals end an upgrade or a deploy the way an answer that ends
// the CLI does.
var interruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// exitOnSignal makes SIGINT, SIGTERM and SIGHUP end the run with exit code
// 130 once the step under way (exitGate) is through, after the onExit hooks:
// the bundle download is removed and the lock given up rather than left in
// the project. stop undoes it; when a signal is already ending the run, the
// run stops there and leaves the exit to it (the command the signal cut
// short may have returned first).
func exitOnSignal() (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, interruptSignals...)
	done, caught := make(chan struct{}), make(chan struct{})
	go func() {
		select {
		case s := <-ch:
			close(caught)
			exitGate.Lock()
			fmt.Println()
			ui.Fail("Interrupted (%s).", s)
			exitNow(130)
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
		})
		select {
		case <-caught:
			if !exiting.Load() {
				select {} // exitNow(130) ends the process
			}
		default:
		}
	}
}
