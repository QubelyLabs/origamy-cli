package cmd

import (
	"encoding/base64"
	"strings"
	"testing"
)

// fakeVerifyKey is an obviously fake config-signing key: base64 of 32 ASCII
// bytes, the shape of an ed25519 public key.
var fakeVerifyKey = base64.StdEncoding.EncodeToString([]byte("test-config-verify-key-32-bytes!"))

func TestSigningSupported(t *testing.T) {
	for release, want := range map[string]bool{
		"0.1.17":            false, // the current release: rejects all config given the key
		"0.1.18":            false,
		minSigningVersion:   true,
		"0.1.20":            true,
		"v0.2.0":            true,
		"staging":           false, // edge images: unknown, so never trusted to verify
		"main":              false,
		"":                  false,
		"0.1.19-rc.1-dirty": false,
	} {
		if got := signingSupported(release); got != want {
			t.Errorf("signingSupported(%q) = %v, want %v", release, got, want)
		}
	}
}

func TestRequireSigningProblem(t *testing.T) {
	if why := requireSigningProblem("0.1.17", true); !strings.Contains(why, minSigningVersion) {
		t.Fatalf("below %s must be refused, got %q", minSigningVersion, why)
	}
	if why := requireSigningProblem("staging", true); why == "" {
		t.Fatal("edge images must be refused")
	}
	if why := requireSigningProblem(minSigningVersion, false); why == "" {
		t.Fatal("strict mode without a key must be refused")
	}
	if why := requireSigningProblem(minSigningVersion, true); why != "" {
		t.Fatalf("supported release with a key refused: %q", why)
	}
}

// --require-signed-config on a release below minSigningVersion stops deploy
// before anything is probed or the token is read.
func TestDeployRefusesRequireSignedBelowMin(t *testing.T) {
	prev := deployChartVersion
	t.Cleanup(func() { deployChartVersion = prev })
	deployChartVersion = "0.1.17"
	if err := runDeploy("dpe_not-a-real-token", "docker", false, true, false, true); err != errSilent {
		t.Fatalf("expected the styled refusal, got %v", err)
	}
}

func TestRenderDockerEnvSigning(t *testing.T) {
	render := func(key, tag string, require bool) string {
		return renderDockerEnv(dockerEnvParams{HTTPURL: "https://cp.example", ImageTag: tag, SigningKey: key, RequireSigned: require})
	}

	// Release before the fix: the key is only kept as the CLI's copy.
	env := render(fakeVerifyKey, "0.1.17", false)
	if !strings.Contains(env, "\n"+signingKeyCopyVar+"="+fakeVerifyKey+"\n") || !strings.Contains(env, "\nCONFIG_SIGNING_KEY=\n") {
		t.Fatalf("0.1.17 must store the key but not hand it to the services:\n%s", env)
	}

	// A release that verifies: the services get it.
	env = render(fakeVerifyKey, minSigningVersion, false)
	if !strings.Contains(env, "\nCONFIG_SIGNING_KEY="+fakeVerifyKey+"\n") {
		t.Fatalf("missing CONFIG_SIGNING_KEY:\n%s", env)
	}
	if strings.Contains(env, "CONFIG_SIGNING_REQUIRED") {
		t.Fatalf("strict mode must never be switched on without the flag:\n%s", env)
	}
	if env = render(fakeVerifyKey, minSigningVersion, true); !strings.Contains(env, "\nCONFIG_SIGNING_REQUIRED=true\n") {
		t.Fatalf("--require-signed-config not rendered:\n%s", env)
	}

	// No key from the control plane: both entries are written blank, so a
	// re-deploy (mergeEnv) drops a stale key from the previous .env.
	env = render("", minSigningVersion, false)
	if !strings.Contains(env, "\n"+signingKeyCopyVar+"=\n") || !strings.Contains(env, "\nCONFIG_SIGNING_KEY=\n") {
		t.Fatalf("blank key entries missing:\n%s", env)
	}
	merged := mergeEnv("CONFIG_SIGNING_KEY=stale\n"+signingKeyCopyVar+"=stale\n", env)
	if strings.Contains(merged, "stale") {
		t.Fatalf("stale key survived a re-deploy:\n%s", merged)
	}
}

// `origamy upgrade` on Docker moves the key with the tag, both ways.
func TestDockerSigningEnv(t *testing.T) {
	up := dockerSigningEnv(fakeVerifyKey, minSigningVersion)
	if up["CONFIG_SIGNING_KEY"] != fakeVerifyKey || up[signingKeyCopyVar] != fakeVerifyKey {
		t.Fatalf("upgrade to %s must apply the stored key: %v", minSigningVersion, up)
	}
	down := dockerSigningEnv(fakeVerifyKey, "0.1.17")
	if down["CONFIG_SIGNING_KEY"] != "" || down[signingKeyCopyVar] != fakeVerifyKey {
		t.Fatalf("a move below %s must blank CONFIG_SIGNING_KEY and keep the copy: %v", minSigningVersion, down)
	}
	if edge := dockerSigningEnv(fakeVerifyKey, dockerEdgeTag); edge["CONFIG_SIGNING_KEY"] != "" {
		t.Fatalf("edge images must not get the key: %v", edge)
	}
}

func TestSigningSetArgs(t *testing.T) {
	if got := signingSetArgs(false, false); len(got) != 0 {
		t.Fatalf("expected no args, got %v", got)
	}
	got := signingSetArgs(true, false)
	if !containsPair(got, "--set", "controlPlane.existingSecret="+configSigningSecret) {
		t.Fatalf("key Secret not wired: %v", got)
	}
	for _, a := range got {
		if strings.Contains(a, "signingRequired") || strings.Contains(a, "publicKey") {
			t.Fatalf("unexpected arg %q in %v", a, got)
		}
	}
	if got := signingSetArgs(true, true); !containsPair(got, "--set", "controlPlane.signingRequired=true") {
		t.Fatalf("strict mode not set: %v", got)
	}
}

func TestSigningConfigured(t *testing.T) {
	for name, tc := range map[string]struct {
		vals map[string]any
		want bool
	}{
		"no values":       {map[string]any{}, false},
		"no key":          {map[string]any{"controlPlane": map[string]any{"url": "x", "publicKey": ""}}, false},
		"plain key":       {map[string]any{"controlPlane": map[string]any{"publicKey": fakeVerifyKey}}, true},
		"key Secret":      {map[string]any{"controlPlane": map[string]any{"existingSecret": configSigningSecret}}, true},
		"strict mode":     {map[string]any{"controlPlane": map[string]any{"signingRequired": true}}, true},
		"strict mode off": {map[string]any{"controlPlane": map[string]any{"signingRequired": false}}, false},
	} {
		if got := signingConfigured(tc.vals); got != tc.want {
			t.Errorf("%s: signingConfigured = %v, want %v", name, got, tc.want)
		}
	}
}
