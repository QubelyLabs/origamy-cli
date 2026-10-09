package cmd

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveTarget(t *testing.T) {
	cases := []struct {
		name        string
		want        string
		k8s, docker bool
		target      installTarget
		errContains string
	}{
		// auto: Kubernetes wins when reachable, Docker otherwise.
		{"auto prefers kubernetes", "auto", true, true, targetKubernetes, ""},
		{"auto falls back to docker", "auto", false, true, targetDocker, ""},
		{"empty means auto", "", true, false, targetKubernetes, ""},
		{"auto with nothing", "auto", false, false, "", errNoTarget.Error()},
		// explicit: verified, never silently swapped.
		{"explicit docker beside a cluster", "docker", true, true, targetDocker, ""},
		{"explicit docker, case-insensitive", " Docker ", false, true, targetDocker, ""},
		{"explicit docker missing", "docker", true, false, "", "Docker is not available"},
		{"explicit kubernetes", "kubernetes", true, true, targetKubernetes, ""},
		{"k8s alias", "k8s", true, false, targetKubernetes, ""},
		{"explicit kubernetes unreachable", "kubernetes", false, true, "", "no Kubernetes cluster is reachable"},
		{"typo", "compose", true, true, "", `unknown --target "compose"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveTarget(c.want, c.k8s, c.docker)
			if c.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.errContains) {
					t.Fatalf("want error containing %q, got %v", c.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.target {
				t.Fatalf("want %q, got %q", c.target, got)
			}
		})
	}
	// The "nothing here" case is distinguishable so deploy can print its
	// install-Docker hint instead of a generic flag error.
	if _, err := resolveTarget("auto", false, false); !errors.Is(err, errNoTarget) {
		t.Fatalf("expected errNoTarget, got %v", err)
	}
}

func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64": "amd64", "amd64": "amd64", " AMD64\n": "amd64",
		"aarch64": "arm64", "arm64": "arm64",
		"ppc64le": "ppc64le", "": "",
	} {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDockerArchWarning(t *testing.T) {
	if w := dockerArchWarning("amd64"); w != "" {
		t.Fatalf("amd64 host must not warn, got %q", w)
	}
	if w := dockerArchWarning(""); w != "" {
		t.Fatalf("unknown arch must not warn, got %q", w)
	}
	w := dockerArchWarning("arm64")
	for _, must := range []string{"linux/amd64", "arm64", "emulation", "exec format error", "tonistiigi/binfmt"} {
		if !strings.Contains(w, must) {
			t.Errorf("arm64 warning should mention %q: %q", must, w)
		}
	}
}

func TestNodeArchProblem(t *testing.T) {
	if b, w := nodeArchProblem(nil); b != "" || w != "" {
		t.Fatalf("unknown archs must be silent, got %q / %q", b, w)
	}
	if b, w := nodeArchProblem([]string{"amd64"}); b != "" || w != "" {
		t.Fatalf("all-amd64 must be silent, got %q / %q", b, w)
	}
	if b, w := nodeArchProblem([]string{"arm64"}); b == "" || w != "" || !strings.Contains(b, "never schedule") {
		t.Fatalf("arm64-only must block, got %q / %q", b, w)
	}
	if b, w := nodeArchProblem([]string{"amd64", "arm64"}); b != "" || w == "" || !strings.Contains(w, "mixes") {
		t.Fatalf("mixed must warn, got %q / %q", b, w)
	}
}

const jsonSchemaSQL = `CREATE TABLE IF NOT EXISTS events (
    event_id              String,
    properties            JSON,
    context               JSON,
    traits                JSON,
    transform_timestamp   DateTime64(3)
) ENGINE = MergeTree`

const stringSchemaSQL = `CREATE TABLE IF NOT EXISTS events (
    event_id              String,
    properties            String DEFAULT '',
    context               String DEFAULT '',
    transform_timestamp   DateTime64(3)
) ENGINE = MergeTree`

func TestSchemaGeneration(t *testing.T) {
	if g := schemaGeneration(jsonSchemaSQL); g != "json" {
		t.Fatalf("json schema classified as %q", g)
	}
	if g := schemaGeneration(stringSchemaSQL); g != "string" {
		t.Fatalf("string schema classified as %q", g)
	}
	if g := schemaGeneration("CREATE TABLE user_traits (trait_value String)"); g != "unknown" {
		t.Fatalf("no events table classified as %q", g)
	}
	// A column that merely contains the word must not match (JSON-ish names).
	if g := schemaGeneration("    properties_json String,\n"); g != "unknown" {
		t.Fatalf("properties_json classified as %q", g)
	}
}

func TestBundleSchemaMismatch(t *testing.T) {
	// The real pairing today: 0.1.17 images against the served JSON schema.
	if why := bundleSchemaMismatch(jsonSchemaSQL, "0.1.17"); why == "" || !strings.Contains(why, "0.1.18") {
		t.Fatalf("0.1.17 + json schema must be refused and name the first good release, got %q", why)
	}
	// Releases from the storage reset on match the served schema.
	for _, tag := range []string{"0.1.18", "0.1.19", "0.2.0", "1.0.0"} {
		if why := bundleSchemaMismatch(jsonSchemaSQL, tag); why != "" {
			t.Errorf("%s + json schema must pass, got %q", tag, why)
		}
	}
	// And the inverse: a new release against a control plane still serving the old schema.
	if why := bundleSchemaMismatch(stringSchemaSQL, "0.1.18"); why == "" {
		t.Fatal("0.1.18 + string schema must be refused")
	}
	if why := bundleSchemaMismatch(stringSchemaSQL, "0.1.17"); why != "" {
		t.Fatalf("0.1.17 + string schema must pass, got %q", why)
	}
	// Moving tags and unknown schemas are never blocked (upgrade --channel edge, dev planes).
	for _, tag := range []string{"main", "staging", "sha-881359f", ""} {
		if why := bundleSchemaMismatch(jsonSchemaSQL, tag); why != "" {
			t.Errorf("non-semver tag %q must pass, got %q", tag, why)
		}
	}
	if why := bundleSchemaMismatch("-- nothing recognisable", "0.1.17"); why != "" {
		t.Fatalf("unknown schema must pass, got %q", why)
	}
}

func TestLegacyChartSetArgs(t *testing.T) {
	// The pinned 0.1.17 chart needs the slow probe; the fixed release and
	// moving tags do not.
	if got := legacyChartSetArgs("0.1.17"); len(got) != 4 || got[1] != "healthCheck.livenessProbe.initialDelaySeconds=90" || got[3] != "healthCheck.livenessProbe.failureThreshold=6" {
		t.Fatalf("0.1.17: got %v", got)
	}
	for _, v := range []string{"0.1.18", "0.2.0", "main", ""} {
		if got := legacyChartSetArgs(v); got != nil {
			t.Errorf("%q: expected no overrides, got %v", v, got)
		}
	}
}

func TestPruneLegacyValues(t *testing.T) {
	vals := map[string]any{
		"controlPlane": map[string]any{"url": "x"},
		"healthCheck": map[string]any{
			"livenessProbe": map[string]any{"initialDelaySeconds": 90.0, "failureThreshold": 6.0},
		},
	}
	pruneLegacyValues(vals)
	if _, ok := vals["healthCheck"]; ok {
		t.Fatalf("healthCheck should be gone when only the overrides were set: %v", vals)
	}
	if vals["controlPlane"].(map[string]any)["url"] != "x" {
		t.Fatal("unrelated values must survive")
	}
	// A customer's own probe tweak next to ours is kept.
	vals = map[string]any{"healthCheck": map[string]any{"livenessProbe": map[string]any{"initialDelaySeconds": 90.0, "timeoutSeconds": 9.0}}}
	pruneLegacyValues(vals)
	lp := vals["healthCheck"].(map[string]any)["livenessProbe"].(map[string]any)
	if _, ok := lp["initialDelaySeconds"]; ok || lp["timeoutSeconds"] != 9.0 {
		t.Fatalf("expected only our keys removed, got %v", vals)
	}
	pruneLegacyValues(map[string]any{}) // no healthCheck: no panic
}

func TestCrossesSchemaReset(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"0.1.17", "0.1.18", true},
		{"0.1.15", "0.2.0", true},
		{"0.1.18", "0.1.19", false},
		{"0.1.16", "0.1.17", false},
		{"0.1.18", "0.1.17", false}, // downgrade: not this guard's job
		{"main", "0.1.18", false},
		{"0.1.17", "staging", false},
		{"", "0.1.18", false},
	} {
		if got := crossesSchemaReset(c.from, c.to); got != c.want {
			t.Errorf("crossesSchemaReset(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestIsDesktopContext(t *testing.T) {
	for ctx, want := range map[string]bool{
		"docker-desktop": true, "kind-kind": true, "minikube": true, "orbstack": true,
		"rancher-desktop": true, "colima": true, "k3d-dev": true,
		"arn:aws:eks:eu-west-1:123:cluster/prod": false, "gke_proj_europe-west1_prod": false, "": false,
	} {
		if got := isDesktopContext(ctx); got != want {
			t.Errorf("isDesktopContext(%q) = %v, want %v", ctx, got, want)
		}
	}
}

func TestLiveSchemaMismatch(t *testing.T) {
	if why := liveSchemaMismatch("json", "0.1.17"); why == "" {
		t.Fatal("0.1.17 against a JSON volume must be refused")
	}
	if why := liveSchemaMismatch("string", "0.1.18"); why == "" || !strings.Contains(why, "origamy upgrade") {
		t.Fatalf("0.1.18 against a String volume must point at upgrade, got %q", why)
	}
	for gen, tag := range map[string]string{"json": "0.1.18", "string": "0.1.17", "": "0.1.17"} {
		if why := liveSchemaMismatch(gen, tag); why != "" {
			t.Errorf("%s/%s must pass, got %q", gen, tag, why)
		}
	}
	if why := liveSchemaMismatch("json", "staging"); why != "" {
		t.Fatalf("moving tags never blocked, got %q", why)
	}
}
