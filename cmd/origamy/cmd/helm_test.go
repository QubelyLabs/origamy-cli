package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
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

func TestPredictorToggleArgs(t *testing.T) {
	// --enable-predictor → predictor.enabled=true.
	got, err := predictorToggleArgs(true, false)
	if err != nil {
		t.Fatalf("enable: unexpected error: %v", err)
	}
	if !containsPair(got, "--set", "predictor.enabled=true") {
		t.Fatalf("enable: expected predictor.enabled=true, got %v", got)
	}

	// --disable-predictor → predictor.enabled=false.
	got, err = predictorToggleArgs(false, true)
	if err != nil {
		t.Fatalf("disable: unexpected error: %v", err)
	}
	if !containsPair(got, "--set", "predictor.enabled=false") {
		t.Fatalf("disable: expected predictor.enabled=false, got %v", got)
	}

	// Neither flag → no args, no error (predictor stays as the release has it).
	got, err = predictorToggleArgs(false, false)
	if err != nil {
		t.Fatalf("neither: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("neither: expected no args, got %v", got)
	}

	// Both together is a user error.
	if _, err := predictorToggleArgs(true, true); err == nil {
		t.Fatal("both: expected an error when --enable-predictor and --disable-predictor are combined")
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

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.15", "0.1.16", true},
		{"0.1.16", "0.1.16", false},
		{"0.1.17", "0.1.16", false},
		{"0.1.9", "0.1.10", true},   // numeric, not lexical
		{"v0.1.15", "0.1.16", true}, // tag-style prefix tolerated
		{"main", "0.1.16", false},   // non-semver never blocks
		{"", "0.1.16", false},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFeatureGate(t *testing.T) {
	if err := featureGate("0.1.15", true, false); err == nil {
		t.Fatal("AI on 0.1.15 must be refused (no orchestrator templates)")
	}
	if err := featureGate("0.1.16", true, true); err == nil {
		t.Fatal("predictor on 0.1.16 must be refused (no predictor templates)")
	}
	if err := featureGate("0.1.17", true, true); err != nil {
		t.Fatalf("0.1.17 carries both: %v", err)
	}
	if err := featureGate(helmVersion, true, true); err != nil {
		t.Fatalf("the pinned install chart must carry every toggle: %v", err)
	}
	if err := featureGate("main", true, true); err != nil {
		t.Fatalf("edge must pass through: %v", err)
	}
}

func TestExistingOrRandomKEK(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	kek := existingOrRandomKEK(env, "ORCH_KEK")
	raw, err := base64.StdEncoding.DecodeString(kek)
	if err != nil || len(raw) != 32 {
		t.Fatalf("KEK must be standard base64 of exactly 32 bytes, got %q (%v)", kek, err)
	}
	if err := os.WriteFile(env, []byte("ORCH_KEK="+kek+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := existingOrRandomKEK(env, "ORCH_KEK"); got != kek {
		t.Fatal("an existing KEK must never be regenerated")
	}
}

func TestComposeProfiles(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("COMPOSE_PROFILES=full, ingress\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := composeProfiles(env)
	if strings.Join(p, ",") != "full,ingress" {
		t.Fatalf("parse: %v", p)
	}
	p = withProfile(p, "agentic", true)
	p = withProfile(p, "agentic", true) // idempotent
	if strings.Join(p, ",") != "full,ingress,agentic" {
		t.Fatalf("add: %v", p)
	}
	p = withProfile(p, "ingress", false)
	if strings.Join(p, ",") != "full,agentic" {
		t.Fatalf("remove: %v", p)
	}
	if got := composeProfiles(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("missing file → no profiles, got %v", got)
	}
}

func TestSetEnvVarKeepsTrailingNewline(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setEnvVar(env, "B", "2"); err != nil {
		t.Fatal(err)
	}
	if err := setEnvVar(env, "A", "9"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(env)
	if string(got) != "A=9\nB=2\n" {
		t.Fatalf("unexpected .env: %q", got)
	}
}

func TestRenderDockerEnv(t *testing.T) {
	env := renderDockerEnv(dockerEnvParams{
		TunnelAddr: "tunnel.origamy.io:443", HTTPURL: "https://v1.origamy.io/",
		DataPlaneID: "dp-1", AuthToken: "dpt_x", ImageTag: "0.1.17", Preset: "starter",
		Profiles: []string{"full", "ingress"}, IngestDomain: "events.acme.com",
		RedisPw: "r", NatsPw: "n", ClickHousePw: "c", DBPw: "d",
		OrchKEK: "kek", OrchToken: "tok", AIEnabled: true, MTLS: true,
	})
	// Every key the served compose bundle reads without a default, plus the
	// FULL endpoint paths config-sync/telemetry expect (not the bare base URL).
	for _, want := range []string{
		"CONTROL_PLANE_ADDR=tunnel.origamy.io:443\n",
		"CONTROL_PLANE_HTTP_URL=https://v1.origamy.io\n",
		"CONFIG_URL=https://v1.origamy.io/api/v1/config\n",
		"TELEMETRY_URL=https://v1.origamy.io/api/v1/telemetry\n",
		"DATA_PLANE_ID=dp-1\n", "AUTH_TOKEN=dpt_x\n", "DP_IMAGE_TAG=0.1.17\n",
		"WRITE_KEYS=\n", "COMPOSE_PROFILES=full,ingress\n", "INGEST_DOMAIN=events.acme.com\n",
		"DP_REDIS_PASSWORD=r\n", "NATS_PASSWORD=n\n", "CLICKHOUSE_PASSWORD=c\n", "DB_PASSWORD=d\n",
		"ORCH_KEK=kek\n", "ORCH_ENGINE_API_TOKEN=tok\n", "ORCHESTRATOR_ENGINE_URL=" + orchestratorEngineURL + "\n",
		"TUNNEL_TLS_CERT=/certs/tls.crt\n", "TUNNEL_TLS_KEY=/certs/tls.key\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %q in:\n%s", want, env)
		}
	}
	// Disabled AI: no engine pointer, but a carried-forward KEK is still kept.
	off := renderDockerEnv(dockerEnvParams{HTTPURL: "http://localhost:8080", OrchKEK: "kek"})
	if strings.Contains(off, "ORCHESTRATOR_ENGINE_URL") || !strings.Contains(off, "ORCH_KEK=kek\n") {
		t.Fatalf("AI-off env wrong:\n%s", off)
	}
	if strings.Contains(off, "TUNNEL_TLS") {
		t.Fatal("bearer-only install must not reference tunnel cert files")
	}
}

func TestLooksLikeHTML(t *testing.T) {
	if !looksLikeHTML([]byte("<!DOCTYPE html>\n<html>")) || !looksLikeHTML([]byte("  <html lang=en>")) || !looksLikeHTML(nil) {
		t.Fatal("SPA fallback pages must be detected")
	}
	if looksLikeHTML([]byte("# compose\nservices:\n")) || looksLikeHTML([]byte("<clickhouse>\n")) {
		t.Fatal("real bundle files must pass")
	}
}
