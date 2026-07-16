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

func TestAIToggleArgs(t *testing.T) {
	// --enable-ai (deploy install and upgrade --enable-ai both feed this) →
	// the helm arg slice sets orchestratorEngine.enabled=true.
	got, err := aiToggleArgs(true, false)
	if err != nil {
		t.Fatalf("enable: unexpected error: %v", err)
	}
	if !containsPair(got, "--set", "orchestratorEngine.enabled=true") {
		t.Fatalf("enable: expected orchestratorEngine.enabled=true, got %v", got)
	}

	// --disable-ai → orchestratorEngine.enabled=false.
	got, err = aiToggleArgs(false, true)
	if err != nil {
		t.Fatalf("disable: unexpected error: %v", err)
	}
	if !containsPair(got, "--set", "orchestratorEngine.enabled=false") {
		t.Fatalf("disable: expected orchestratorEngine.enabled=false, got %v", got)
	}

	// Neither flag → no args, no error (default install leaves AI off).
	got, err = aiToggleArgs(false, false)
	if err != nil {
		t.Fatalf("neither: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("neither: expected no args, got %v", got)
	}

	// --enable-ai + --disable-ai together is a user error.
	if _, err := aiToggleArgs(true, true); err == nil {
		t.Fatal("both: expected an error when --enable-ai and --disable-ai are combined")
	}
}

// containsPair reports whether args holds k immediately followed by v (a helm
// "--set", "key=value" pair).
func containsPair(args []string, k, v string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}
