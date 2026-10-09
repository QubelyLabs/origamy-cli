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
