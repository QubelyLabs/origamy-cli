package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLockHelperProcess is not a test: the lock tests run it as another
// process (ORIGAMY_LOCK_HELPER names the project directory). It takes the
// project's lock as `origamy deploy`, with ORIGAMY_LOCK_HELPER_SIGNALS also
// a bundle download (an onExit hook removes it) and exitOnSignal, says
// "locked" on stdout, and waits to be killed or signalled. With
// ORIGAMY_LOCK_HELPER_NOFLOCK the file system has no advisory locks.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv("ORIGAMY_LOCK_HELPER")
	if dir == "" {
		t.Skip("run by the lock tests as another process")
	}
	if os.Getenv("ORIGAMY_LOCK_HELPER_NOFLOCK") != "" {
		tryLock = func(*os.File) error { return errLockUnsupported }
	}
	if _, err := lockProject(dir, "origamy deploy"); err != nil {
		fmt.Println("error:", err)
		os.Exit(2)
	}
	if os.Getenv("ORIGAMY_LOCK_HELPER_SIGNALS") != "" {
		tmp, err := os.MkdirTemp(dir, bundleTempPrefix)
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(2)
		}
		onExit(func() { _ = os.RemoveAll(tmp) })
		exitOnSignal()
	}
	fmt.Println("locked")
	time.Sleep(time.Minute)
	os.Exit(0)
}

// lockHelper starts TestLockHelperProcess on dir and waits until it holds
// the lock; the test kills it at the end if it still runs.
func lockHelper(t *testing.T, dir string, signals bool) *exec.Cmd {
	t.Helper()
	return startLockHelper(t, exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$", "-test.count=1"), dir, signals)
}

// startLockHelper is lockHelper for a command that runs TestLockHelperProcess
// (through a shell that ignores a signal first, say).
func startLockHelper(t *testing.T, cmd *exec.Cmd, dir string, signals bool) *exec.Cmd {
	t.Helper()
	cmd.Env = append(os.Environ(), "ORIGAMY_LOCK_HELPER="+dir)
	if signals {
		cmd.Env = append(cmd.Env, "ORIGAMY_LOCK_HELPER_SIGNALS=1")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if l := lines.Text(); l == "locked" {
			return cmd
		} else if strings.HasPrefix(l, "error:") {
			t.Fatal(l)
		}
	}
	t.Fatal("the helper process never took the lock")
	return nil
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
	// Held: another run is refused, names it, and leaves the file as it was.
	body := readFile(t, path)
	if _, err := lockProject(dir, "origamy deploy"); err == nil || !strings.Contains(err.Error(), "origamy upgrade, pid ") {
		t.Fatalf("second lock: %v", err)
	}
	if readFile(t, path) != body {
		t.Fatal("a refused run changed the lock file")
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

	// A file a run that was killed left behind holds no lock: it is simply
	// taken, whatever it says, and rewritten.
	for _, left := range []string{projectLock{pid: os.Getppid(), host: host, started: "x", command: "origamy deploy"}.String(), "", "not a lock\n"} {
		writeFile(t, path, left)
		unlock, err := lockProject(dir, "origamy upgrade")
		if err != nil {
			t.Fatalf("%q: %v", left, err)
		}
		if held, _ := readProjectLock(path); held.pid != os.Getpid() {
			t.Fatalf("%q: not rewritten: %+v", left, held)
		}
		unlock()
	}

	// unlock removes only its own lock file: one put in its place (by hand)
	// stays.
	unlock, err = lockProject(dir, "origamy upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "pid=1\nhost=other\n")
	unlock()
	if readFile(t, path) != "pid=1\nhost=other\n" {
		t.Fatal("unlock removed a lock file that was not its own")
	}
}

// TestLockHeldByAnotherProcess: the lock is the operating system's, held by
// another process's open file; refused while it runs, and free the moment
// it is killed (no stale lock, though the file stays behind).
func TestLockHeldByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	helper := lockHelper(t, dir, false)
	_, err := lockProject(dir, "origamy upgrade")
	want := fmt.Sprintf("held by origamy deploy, pid %d on ", helper.Process.Pid)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	if _, err := os.Stat(filepath.Join(dir, projectLockFile)); err != nil {
		t.Fatalf("the killed run's file: %v", err)
	}
	unlock, err := lockProject(dir, "origamy upgrade")
	if err != nil {
		t.Fatalf("the lock of a killed run must be free: %v", err)
	}
	unlock()
}

func TestLockFail(t *testing.T) {
	out := captureStdout(t, func() {
		_ = lockFail(&errProjectLocked{path: "/dp/.origamy.lock", held: projectLock{pid: 42, host: "h", started: "s", command: "origamy deploy"}, ok: true}, "Nothing has been changed.")
	})
	if !strings.Contains(out, "Another origamy run is working on this data plane (origamy deploy, pid 42 on h, started s).") || !strings.Contains(out, "Wait for it to finish, then rerun. Nothing has been changed.") {
		t.Fatalf("%s", out)
	}
	out = captureStdout(t, func() { _ = lockFail(&errProjectLocked{path: "/dp/.origamy.lock"}, "Nothing has been changed.") })
	if !strings.Contains(out, "(it holds /dp/.origamy.lock)") {
		t.Fatalf("%s", out)
	}
}

func TestOnExit(t *testing.T) {
	hooks := func() int {
		exitMu.Lock()
		defer exitMu.Unlock()
		return len(exitHooks)
	}
	n0 := hooks()
	var ran []string
	a := onExit(func() { ran = append(ran, "a") })
	b := onExit(func() { ran = append(ran, "b") })
	a()
	a()
	if n := hooks() - n0; n != 1 || strings.Join(ran, "") != "a" {
		t.Fatalf("hooks %d, ran %v", n, ran)
	}
	b()
	if n := hooks() - n0; n != 0 {
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

// TestUninstallDockerLocked: uninstall takes the lock too, so it never tears
// down a plane an upgrade or a deploy is working on.
func TestUninstallDockerLocked(t *testing.T) {
	parent := t.TempDir()
	chdir(t, parent)
	dir := filepath.Join(parent, "origamy-dp-dp-1")
	writeProject(t, dir, "dp-1")
	unlock, err := lockProject(dir, "origamy upgrade")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var calls []string
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", nil
	})
	out := captureStdout(t, func() { err = uninstallDocker("dp-1", true) })
	if err == nil || !strings.Contains(out, "Another origamy run is working on this data plane (origamy upgrade, pid ") || !strings.Contains(out, "Nothing has been removed.") || len(calls) != 0 {
		t.Fatalf("%v, calls %v\n%s", err, calls, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		t.Fatal("the project was removed")
	}
	// Once that run is over, the plane goes, lock file and all.
	unlock()
	abs, _ := filepath.Abs("origamy-dp-dp-1")
	down := strings.Join(composeCmd(abs, filepath.Join(abs, ".env"), "down", "-v", "--remove-orphans"), " ")
	out = captureStdout(t, func() { err = uninstallDocker("dp-1", true) })
	// The plane's containers are looked for, and, with none, its volumes.
	if err != nil || len(calls) != 3 || !strings.HasPrefix(calls[0], "ps -a ") || !strings.HasPrefix(calls[1], "volume ls ") || calls[2] != down {
		t.Fatalf("%v, calls %v\n%s", err, calls, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the project is still there: %v", err)
	}
}

// TestUninstallDockerOwnProjectOnly: uninstall removes this plane's own
// containers and volumes only. `down -v` is pinned to the project's name
// whatever COMPOSE_PROJECT_NAME the shell exports, and refused, with nothing
// removed, when that name also has containers started from another
// directory, when this plane's containers run under another name, or when
// Docker cannot say.
func TestUninstallDockerOwnProjectOnly(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "victim")
	for _, tc := range []struct {
		name, owners string
		psFails      bool
		says         string
	}{
		{"its own", "origamy-dp-dp-1\t<dir>\nvictim\t/victim\n", false, ""},
		{"none running", "", false, ""},
		{"the name used by another directory", "origamy-dp-dp-1\t<dir>\norigamy-dp-dp-1\t<other>\n", false, "also has containers started from <other>"},
		{"its containers under another name", "custom\t<dir>\n", false, "run under the compose project name custom"},
		{"Docker cannot say", "", true, "Docker could not list the containers on this host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			chdir(t, parent)
			dir, other := filepath.Join(parent, "origamy-dp-dp-1"), filepath.Join(parent, "copy")
			writeProject(t, dir, "dp-1")
			if err := os.Mkdir(other, 0o755); err != nil {
				t.Fatal(err)
			}
			owners := strings.NewReplacer("<dir>", dir, "<other>", other).Replace(tc.owners)
			var calls []string
			stubDocker(t, func(env []string, name string, args ...string) (string, error) {
				calls = append(calls, strings.Join(append(append(slices.Clone(env), name), args...), " "))
				if args[0] == "ps" {
					if tc.psFails {
						return "Cannot connect to the Docker daemon", errors.New("exit status 1")
					}
					return owners, nil
				}
				return "", nil
			})
			down := dcCall(readEnvVar(filepath.Join(dir, ".env"), "DP_IMAGE_TAG"), dir, filepath.Join(dir, ".env")) + "down -v --remove-orphans"
			out := captureStdout(t, func() { err = uninstallDocker("dp-1", true) })
			if tc.says == "" {
				// With no container of its own under the name, its volumes are looked for too.
				n := map[bool]int{true: 3, false: 2}[tc.owners == ""]
				if err != nil || len(calls) != n || calls[n-1] != down || !strings.Contains(down, " -p origamy-dp-dp-1 ") {
					t.Fatalf("%v, calls %v\n%s", err, calls, out)
				}
				return
			}
			if err == nil || !strings.Contains(out, strings.NewReplacer("<other>", other).Replace(tc.says)) || !strings.Contains(out, "Nothing has been removed.") || len(calls) != 1 {
				t.Fatalf("%v, calls %v\n%s", err, calls, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
				t.Fatal("the project was removed")
			}
		})
	}
}

// noFlock stands in a file system without advisory locks (NFS without a lock
// manager) for one test.
func noFlock(t *testing.T) {
	t.Helper()
	prev := tryLock
	tryLock = func(*os.File) error { return errLockUnsupported }
	t.Cleanup(func() { tryLock = prev })
}

// TestLockExclusive: where flock answers ENOLCK, EOPNOTSUPP or ENOSYS the lock
// file itself is the lock, never a dead end: held by a running process (here
// or, unknowable, on another host) it is refused with how to clear it; a file
// a run that died left, on this host, or one that has said nothing for
// lockWriteGrace, is taken over; unlock removes the file; and of concurrent
// runs exactly one gets it.
func TestLockExclusive(t *testing.T) {
	noFlock(t)
	prev := lockWriteGrace
	lockWriteGrace = 2 * time.Second
	t.Cleanup(func() { lockWriteGrace = prev })
	dir := t.TempDir()
	path := filepath.Join(dir, projectLockFile)
	host, _ := os.Hostname()

	unlock, err := lockProject(dir, "origamy upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if held, ok := readProjectLock(path); !ok || held.pid != os.Getpid() || held.command != "origamy upgrade" {
		t.Fatalf("lock = %+v", held)
	}
	_, err = lockProject(dir, "origamy deploy")
	var locked *errProjectLocked
	if !errors.As(err, &locked) || !locked.exclusive || !strings.Contains(err.Error(), "origamy upgrade, pid ") {
		t.Fatalf("second lock: %v", err)
	}
	out := captureStdout(t, func() { _ = lockFail(err, "Nothing has been changed.") })
	if !strings.Contains(out, "no advisory locks") || !strings.Contains(out, "remove "+path) {
		t.Fatalf("%s", out)
	}
	unlock()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unlock left the file: %v", err)
	}

	// A run that died here: taken over. One on another host: refused.
	dead := exec.Command(os.Args[0], "-test.run=^$")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		age        time.Duration
		taken      bool
	}{
		{"a run that died", projectLock{pid: dead.Process.Pid, host: host, started: "x", command: "origamy upgrade"}.String(), 0, true},
		{"a running process", projectLock{pid: os.Getppid(), host: host, started: "x", command: "origamy upgrade"}.String(), 0, false},
		{"another host", projectLock{pid: dead.Process.Pid, host: "elsewhere", started: "x", command: "origamy upgrade"}.String(), 0, false},
		{"empty, just made", "", 0, false},
		{"empty, left behind", "", time.Minute, true},
		{"not a lock, left behind", "not a lock\n", time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, path, tc.body)
			if tc.age > 0 {
				old := time.Now().Add(-tc.age)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			unlock, err := lockProject(dir, "origamy upgrade")
			if (err == nil) != tc.taken {
				t.Fatalf("taken: %v", err)
			}
			if err != nil {
				if readFile(t, path) != tc.body {
					t.Fatal("a refused run changed the lock file")
				}
				_ = os.Remove(path)
				return
			}
			if held, _ := readProjectLock(path); held.pid != os.Getpid() {
				t.Fatalf("not rewritten: %+v", held)
			}
			unlock()
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("left behind: %v", entries)
			}
		})
	}

	// Concurrent runs in this process: exactly one gets it.
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
}

// TestLockExclusiveRace: processes that all find the same stale lock file at
// once (its run died) take it over one at a time: exactly one holds the lock.
func TestLockExclusiveRace(t *testing.T) {
	host, _ := os.Hostname()
	dead := exec.Command(os.Args[0], "-test.run=^$")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	for trial := range 5 {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectLockFile), projectLock{pid: dead.Process.Pid, host: host, started: "x", command: "origamy upgrade"}.String())
		results := make(chan string, 6)
		var cmds []*exec.Cmd
		for range 6 {
			cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), "ORIGAMY_LOCK_HELPER="+dir, "ORIGAMY_LOCK_HELPER_NOFLOCK=1")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			cmds = append(cmds, cmd)
			go func() {
				lines := bufio.NewScanner(stdout)
				for lines.Scan() {
					if l := lines.Text(); l == "locked" || strings.HasPrefix(l, "error:") {
						results <- l
						return
					}
				}
				results <- "no answer"
			}()
		}
		holders := 0
		for range cmds {
			select {
			case r := <-results:
				if r == "locked" {
					holders++
				} else if !strings.Contains(r, "held by") {
					t.Errorf("trial %d: %s", trial, r)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("trial %d: a helper never answered", trial)
			}
		}
		for _, c := range cmds {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
		if holders != 1 {
			t.Fatalf("trial %d: %d holders", trial, holders)
		}
	}
}
