package cmd

import (
	"bytes"
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
	return parseProjectLock(b)
}

// parseProjectLock parses what a lock file holds (readProjectLock).
func parseProjectLock(b []byte) (l projectLock, ok bool) {
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
// (ok false when it says nothing yet). exclusive: the file system has no
// advisory locks, and the file itself is the lock (lockExclusive).
type errProjectLocked struct {
	path      string
	held      projectLock
	ok        bool
	exclusive bool
}

func (e *errProjectLocked) Error() string {
	if !e.ok {
		return e.path + " is held by another run"
	}
	return "held by " + e.held.describe()
}

var (
	// errLockHeld is what lockFile returns when another open file holds the
	// lock.
	errLockHeld = errors.New("the lock is held")
	// errLockUnsupported is what lockFile returns when the file system has no
	// advisory locks: flock answers ENOLCK, EOPNOTSUPP or ENOSYS on network
	// file systems without a lock manager (NFS mounted nolock or without
	// lockd, some SMB and FUSE mounts).
	errLockUnsupported = errors.New("the file system has no advisory locks")
)

// tryLock takes the operating system's lock on an open lock file (lockFile).
// A variable so tests can stand in a file system without advisory locks.
var tryLock = lockFile

// lockProject takes dir's lock for command, which a refused run names. The
// lock is the operating system's advisory lock on .origamy.lock (flock;
// LockFileEx on Windows), held by this process's open file: a run that is
// killed, crashes or is interrupted gives it up with its process, so there
// is no stale lock to take over, and a file such a run left behind is simply
// locked again. Held by another run, it is refused with errProjectLocked,
// naming the process, host, start time and command the file records. Where
// the file system has no advisory locks (errLockUnsupported), the file
// itself is the lock instead (lockExclusive). unlock removes the file and
// gives the lock up, and so does a prompt or a signal that ends the CLI
// (exitNow).
func lockProject(dir, command string) (unlock func(), err error) {
	path := filepath.Join(dir, projectLockFile)
	host, _ := os.Hostname()
	me := projectLock{pid: os.Getpid(), host: host, started: time.Now().UTC().Format(time.RFC3339), command: command}
	for range 10 {
		// Created only when there is none, so that a run that made the file
		// knows it did (lockExclusive).
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		created := false
		if errors.Is(err, os.ErrNotExist) {
			f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
			if created = err == nil; errors.Is(err, os.ErrExist) {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		if err := tryLock(f); err != nil {
			switch {
			case errors.Is(err, errLockUnsupported) && created:
				// The file this run made with O_EXCL is the lock.
				return holdExclusive(f, path, me)
			case errors.Is(err, errLockUnsupported):
				_ = f.Close()
				return lockExclusive(path, me)
			case errors.Is(err, errLockHeld):
				_ = f.Close()
				held, ok := readProjectLock(path)
				return nil, &errProjectLocked{path: path, held: held, ok: ok}
			}
			_ = f.Close()
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

// lockWriteGrace is how long a lock file of the exclusive scheme may say
// nothing (lockExclusive): a run writes what it is right after it creates
// the file, so one still empty after this was left by a run that died in
// between, or by a run that only opened it for flock. lockTakeoverStale is
// how old the takeover file may get before it is taken for one a run that
// died left. Variables so tests need not wait.
var (
	lockWriteGrace    = 5 * time.Second
	lockTakeoverStale = 30 * time.Second
)

// lockExclusive is the lock where the file system has no advisory locks
// (errLockUnsupported): the lock file itself, created with O_EXCL and
// removed by the run that holds it. With no process to give it up, a file a
// run that ended without removing it (killed, crashed) left behind is
// stale when it names a process on this host that no longer runs, or says
// nothing after lockWriteGrace; it is then removed and created again. One
// that names a running process, or a process on another host (which cannot
// be checked), is held: the run is refused with errProjectLocked, whose
// message says to remove the file once that run is gone. A process that
// took over the pid of a run that died counts as that run too. Removing a
// stale file is serialized by a second O_EXCL file, <lock>.takeover, under
// which the file is read again and removed only when it is still the stale
// one: two runs that found it stale at once cannot remove the lock the
// first of them created next.
func lockExclusive(path string, me projectLock) (unlock func(), err error) {
	for range 10 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return holdExclusive(f, path, me)
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		fi, err := os.Stat(path)
		var body []byte
		if err == nil {
			body, err = os.ReadFile(path)
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		held, ok := parseProjectLock(body)
		if !lockStale(fi, held, ok, me.host) {
			return nil, &errProjectLocked{path: path, held: held, ok: ok, exclusive: true}
		}
		if err := takeOver(path, fi, body, me); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s kept changing while it was being locked", path)
}

// holdExclusive writes what this run is into f, the lock file at path it
// created with O_EXCL, and holds it as the exclusive scheme's lock.
func holdExclusive(f *os.File, path string, me projectLock) (unlock func(), err error) {
	_, err = f.WriteAt([]byte(me.String()), 0)
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		releaseLock(f, path)
		return nil, err
	}
	return onExit(func() { releaseLock(f, path) }), nil
}

// lockStale reports whether a lock file of the exclusive scheme (fi) that
// says held (ok: it names a run) is no lock any more: its run, on this host
// (host), no longer runs, or it names none and has said nothing for
// lockWriteGrace.
func lockStale(fi os.FileInfo, held projectLock, ok bool, host string) bool {
	if ok {
		return held.host == host && !pidAlive(held.pid)
	}
	return time.Since(fi.ModTime()) > lockWriteGrace
}

// takeOver removes the stale lock file at path (fi, which read body) under
// the takeover file: only while it is still that file and still reads body.
// A takeover file whose run died (on this host, or older than
// lockTakeoverStale) is removed first; another run's that is under way is
// waited for.
func takeOver(path string, fi os.FileInfo, body []byte, me projectLock) error {
	tpath := path + ".takeover"
	for range 50 {
		t, err := os.OpenFile(tpath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			held, ok := readProjectLock(tpath)
			fi, serr := os.Stat(tpath)
			if serr == nil && ((ok && held.host == me.host && !pidAlive(held.pid)) || time.Since(fi.ModTime()) > lockTakeoverStale) {
				_ = os.Remove(tpath)
			} else {
				time.Sleep(20 * time.Millisecond)
			}
			continue
		}
		if err != nil {
			return err
		}
		_, _ = t.Write([]byte(me.String()))
		now, err := os.Stat(path)
		if err == nil && os.SameFile(fi, now) && now.ModTime().Equal(fi.ModTime()) {
			if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, body) {
				_ = os.Remove(path)
			}
		}
		_ = t.Close()
		return os.Remove(tpath)
	}
	return fmt.Errorf("%s.takeover stayed in place: another run is taking the lock over", path)
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
	if errors.As(err, &locked) {
		who := "it holds " + locked.path
		if locked.ok {
			who = locked.held.describe()
		}
		hint := "Wait for it to finish, then rerun. " + nothing
		switch {
		case locked.exclusive:
			hint += "\nThis file system has no advisory locks (a network file system), so the lock is the file itself. A run on this host that is gone is noticed; once that run is gone and it still is not, remove " + locked.path + " and rerun."
		case locked.ok:
			hint += "\nThe lock goes with that process: once it has ended, however it ended, the next run takes it."
		}
		return fail("Another origamy run is working on this data plane ("+who+").", hint)
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

// ignoredAtStart are the interruptSignals the CLI was started with ignored:
// `nohup origamy upgrade` ignores SIGHUP so the run outlives the terminal
// (an SSH session that drops), and taking it over would end the run, maybe
// half-way through a storage reset, when the terminal goes. Read once, as
// the process starts: once os/signal has taken a signal over and given it
// back, it no longer reports it ignored.
var ignoredAtStart = func() map[os.Signal]bool {
	ignored := map[os.Signal]bool{}
	for _, s := range interruptSignals {
		ignored[s] = signal.Ignored(s)
	}
	return ignored
}()

// exitOnSignal makes SIGINT, SIGTERM and SIGHUP end the run with exit code
// 130 once the step under way (exitGate) is through, after the onExit hooks:
// the bundle download is removed and the lock given up rather than left in
// the project. A signal the CLI was started with ignored (ignoredAtStart:
// SIGHUP under nohup) stays ignored. stop undoes it; when a signal is
// already ending the run, the run stops there and leaves the exit to it
// (the command the signal cut short may have returned first).
func exitOnSignal() (stop func()) {
	ch := make(chan os.Signal, 1)
	var sigs []os.Signal
	for _, s := range interruptSignals {
		if !ignoredAtStart[s] {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) > 0 {
		signal.Notify(ch, sigs...)
	}
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
