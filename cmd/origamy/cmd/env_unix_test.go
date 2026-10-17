//go:build !windows

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteEnvFileKeepsOwner: .env keeps its owner, group and other hard
// links when the CLI writes it (`sudo origamy upgrade` must not leave it to
// root): the replacement is given the original's owner and group, a
// hard-linked file is written in place, and so is one whose owner this user
// cannot give the replacement; one this user may not write either is
// replaced as before.
func TestWriteEnvFileKeepsOwner(t *testing.T) {
	stat := func(t *testing.T, p string) *syscall.Stat_t {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t)
	}
	withChown := func(t *testing.T, fn func(string, int, int) error) {
		t.Helper()
		prev := chown
		chown = fn
		t.Cleanup(func() { chown = prev })
	}
	left := func(t *testing.T, dir string, n int) {
		t.Helper()
		if entries, _ := os.ReadDir(dir); len(entries) != n {
			t.Fatalf("left behind: %v", entries)
		}
	}

	t.Run("the original's owner and group", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, ".env")
		writeFile(t, p, "A=1\n")
		// A group of this user's other than the one a new file in the
		// directory gets, so a replacement that kept it is told apart.
		groups, _ := os.Getgroups()
		dirGid := int(stat(t, dir).Gid)
		for _, g := range groups {
			if g != dirGid && g != os.Getegid() && os.Chown(p, -1, g) == nil {
				break
			}
		}
		want := stat(t, p)
		var got [2]int
		withChown(t, func(name string, uid, gid int) error {
			got = [2]int{uid, gid}
			return os.Chown(name, uid, gid)
		})
		if err := setEnvVar(p, "B", "2"); err != nil {
			t.Fatal(err)
		}
		st := stat(t, p)
		if got != [2]int{int(want.Uid), int(want.Gid)} || st.Uid != want.Uid || st.Gid != want.Gid || readFile(t, p) != "A=1\nB=2\n" {
			t.Fatalf("chown %v; owner %d:%d, want %d:%d", got, st.Uid, st.Gid, want.Uid, want.Gid)
		}
		if want.Gid == uint32(dirGid) {
			t.Log("no other group to check the group with; checked the owner only")
		}
		left(t, dir, 1)
	})

	t.Run("hard links", func(t *testing.T) {
		dir := t.TempDir()
		p, other := filepath.Join(dir, ".env"), filepath.Join(dir, "plane.env")
		writeFile(t, p, "A=1\n")
		if err := os.Link(p, other); err != nil {
			t.Fatal(err)
		}
		before := stat(t, p).Ino
		if err := setEnvVar(p, "B", "2"); err != nil {
			t.Fatal(err)
		}
		if stat(t, p).Ino != before || readFile(t, other) != "A=1\nB=2\n" {
			t.Fatalf("the other link: %q", readFile(t, other))
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
		// Shorter: the rest of the old contents is cut off.
		if err := unsetEnvVar(p, "B"); err != nil || readFile(t, other) != "A=1\n" {
			t.Fatalf("%v: %q", err, readFile(t, other))
		}
		left(t, dir, 2)
	})

	t.Run("an owner this user cannot give", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, ".env")
		writeFile(t, p, "A=1\n")
		before := stat(t, p).Ino
		withChown(t, func(string, int, int) error { return &os.PathError{Op: "chown", Err: syscall.EPERM} })
		if err := setEnvVar(p, "B", "2"); err != nil {
			t.Fatal(err)
		}
		if stat(t, p).Ino != before || readFile(t, p) != "A=1\nB=2\n" {
			t.Fatal("not written in place")
		}
		left(t, dir, 1)
		if os.Geteuid() == 0 {
			return // root may write a file it may not
		}
		// Nor written by this user: replaced as before, as this user's.
		if err := os.Chmod(p, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := setEnvVar(p, "C", "3"); err != nil {
			t.Fatal(err)
		}
		if stat(t, p).Ino == before || readFile(t, p) != "A=1\nB=2\nC=3\n" {
			t.Fatal("not replaced")
		}
		left(t, dir, 1)
	})

	t.Run("a new file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, ".env")
		var called bool
		withChown(t, func(name string, uid, gid int) error {
			called = true
			return errors.New("not permitted")
		})
		// A failed chown of a new file keeps it this user's.
		if err := writeEnvFile(p, []byte("A=1\n"), 0o600); err != nil || !called || readFile(t, p) != "A=1\n" {
			t.Fatalf("%v, chown %v", err, called)
		}
		left(t, dir, 1)
	})
}
