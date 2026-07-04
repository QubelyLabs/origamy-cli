package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExistingOrRandom(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")

	// No file yet → generates a fresh secret each call, and they differ.
	a := existingOrRandom(env, "DP_REDIS_PASSWORD")
	b := existingOrRandom(env, "NATS_PASSWORD")
	if a == "" || b == "" {
		t.Fatal("expected generated secrets, got empty")
	}
	if a == b {
		t.Fatal("expected distinct secrets for distinct keys/calls")
	}
	if len(a) < 40 { // 32 raw bytes → 43 base64url chars
		t.Fatalf("secret too short: %d", len(a))
	}

	// Once written, the SAME key must be reused (stable across re-deploy).
	if err := os.WriteFile(env, []byte("DP_REDIS_PASSWORD="+a+"\nNATS_PASSWORD="+b+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := existingOrRandom(env, "DP_REDIS_PASSWORD"); got != a {
		t.Fatalf("expected reuse of %q, got %q", a, got)
	}
	if got := existingOrRandom(env, "NATS_PASSWORD"); got != b {
		t.Fatalf("expected reuse of %q, got %q", b, got)
	}
	// A key absent from an existing file still generates.
	if got := existingOrRandom(env, "CLICKHOUSE_PASSWORD"); got == "" {
		t.Fatal("expected generated secret for absent key")
	}
}
