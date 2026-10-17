package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLockHelperProcess is not a test: the lock tests run it as another
// process (ORIGAMY_LOCK_HELPER names the project directory). It takes the
// project's lock as `origamy deploy`, with ORIGAMY_LOCK_HELPER_SIGNALS also
// a bundle download (an onExit hook removes it) and exitOnSignal, says
// "locked" on stdout, and waits to be killed or signalled.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv("ORIGAMY_LOCK_HELPER")
	if dir == "" {
		t.Skip("run by the lock tests as another process")
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$", "-test.count=1")
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
	out = captureStdout(t, func() { err = uninstallDocker("dp-1", true) })
	if err != nil || len(calls) != 1 || calls[0] != "compose -f origamy-dp-dp-1/docker-compose.yml down -v --remove-orphans" {
		t.Fatalf("%v, calls %v\n%s", err, calls, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the project is still there: %v", err)
	}
}
