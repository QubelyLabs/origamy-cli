package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeEnv(t *testing.T) {
	rendered := "# Generated\nDATA_PLANE_ID=dp-1\nAUTH_TOKEN=new\nDB_PASSWORD=x\n"
	existing := "# old header\nDATA_PLANE_ID=dp-1\nAUTH_TOKEN=old\n\nCORS_ALLOWED_ORIGINS=https://app.acme.com\nRATE_LIMIT_RPS=1000\n"
	got := mergeEnv(existing, rendered)
	if !strings.HasPrefix(got, rendered) {
		t.Fatalf("rendered keys must come first and unchanged:\n%s", got)
	}
	if strings.Contains(got, "AUTH_TOKEN=old") {
		t.Fatal("a CLI-owned key must be the freshly rendered one")
	}
	for _, want := range []string{"CORS_ALLOWED_ORIGINS=https://app.acme.com", "RATE_LIMIT_RPS=1000", "Kept from your previous .env"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "old header") {
		t.Fatal("old comments are not carried over")
	}
	// Nothing extra → exactly the rendered file.
	if mergeEnv("DATA_PLANE_ID=dp-1\n", rendered) != rendered {
		t.Fatal("no foreign keys: output must equal rendered")
	}
}

func TestSetEnvVarTightensMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setEnvVar(p, "B", "2"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
}

// TestEnvDuplicateKeys: an .env is read as docker compose reads it, the last
// of several KEY= lines winning; setting a key leaves exactly one line, the
// value compose sees, and every other line byte for byte as it was.
func TestEnvDuplicateKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	const body = "# Release to run.\nDP_IMAGE_TAG=0.1.19\nCOMPOSE_PROFILES=full\n  # an indented comment\nDP_IMAGE_TAG_OLD=x\nDP_IMAGE_TAG=0.1.17\nWRITE_KEYS=\n"
	writeFile(t, p, body)
	if got := readEnvVar(p, "DP_IMAGE_TAG"); got != "0.1.17" {
		t.Fatalf("readEnvVar = %q, want the last line's", got)
	}
	if n := envKeyCount(p, "DP_IMAGE_TAG"); n != 2 {
		t.Fatalf("envKeyCount = %d", n)
	}
	if err := setEnvVar(p, "DP_IMAGE_TAG", "0.1.20"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "# Release to run.\nCOMPOSE_PROFILES=full\n  # an indented comment\nDP_IMAGE_TAG_OLD=x\nDP_IMAGE_TAG=0.1.20\nWRITE_KEYS=\n" {
		t.Fatalf("after setEnvVar:\n%s", got)
	}
	// A key that is not there is appended, the file kept newline-terminated.
	if err := setEnvVar(p, resetPendingVar, "0.1.20"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(readFile(t, p), "WRITE_KEYS=\n"+resetPendingVar+"=0.1.20\n") {
		t.Fatalf("append:\n%s", readFile(t, p))
	}
	// unsetEnvVar removes every line of the key, and only those.
	writeFile(t, p, "A=1\n"+resetPendingVar+"=0.1.20\nB=2\n"+resetPendingVar+"=0.1.19\n")
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := unsetEnvVar(p, resetPendingVar); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "A=1\nB=2\n" {
		t.Fatalf("after unsetEnvVar:\n%s", got)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Fatalf("unsetEnvVar changed the mode to %o", st.Mode().Perm())
	}
	if readEnvVar(p, "MISSING") != "" || envKeyCount(p, "MISSING") != 0 {
		t.Fatal("a missing key reads as empty")
	}
}

// TestWriteEnvFile: .env is replaced in one step (no temporary file left), a
// symbolic link written through to the file it points at.
func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	writeFile(t, target, "A=1\n")
	link := filepath.Join(dir, ".env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := setEnvVar(link, "B", "2"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v", err)
	}
	if got := readFile(t, target); got != "A=1\nB=2\n" {
		t.Fatalf("real.env:\n%s", got)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("left behind: %v", entries)
	}
}

// TestCommandEnv: what the CLI sets in a command's environment wins over the
// operator's shell, and a bare name leaves the shell's out.
func TestCommandEnv(t *testing.T) {
	t.Setenv("DP_IMAGE_TAG", "0.1.16")
	count := func(env []string) (n int, val string) {
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, "DP_IMAGE_TAG="); ok {
				n, val = n+1, v
			}
		}
		return n, val
	}
	if n, v := count(commandEnv(composeEnv("0.1.20"))); n != 1 || v != "0.1.20" {
		t.Fatalf("set: %d, %q", n, v)
	}
	if n, _ := count(commandEnv(composeEnv(""))); n != 0 {
		t.Fatal("a bare name must leave the shell's out")
	}
	if out, err := runCapturedEnv(composeEnv("0.1.20"), "sh", "-c", "echo ${DP_IMAGE_TAG-unset}"); err != nil || strings.TrimSpace(out) != "0.1.20" {
		t.Fatalf("%q, %v", out, err)
	}
	if out, err := runCapturedEnv(composeEnv(""), "sh", "-c", "echo ${DP_IMAGE_TAG-unset}"); err != nil || strings.TrimSpace(out) != "unset" {
		t.Fatalf("%q, %v", out, err)
	}
}
