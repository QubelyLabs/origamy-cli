package ui

import (
	"strings"
	"testing"
)

func TestDiagnoseDocker(t *testing.T) {
	const rerun = "rerun `origamy upgrade --version 0.1.20`"
	cases := map[string]string{
		`Error response from daemon: Conflict. The container name "/origamy-byod-nats" is already in use by container "abc"`: "already running on this host",
		`manifest for ghcr.io/qubelylabs/origamy-data-plane/ingestion-gateway:0.1.99 not found: manifest unknown`:            "could not be pulled",
		`Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?`:                  "Docker is not running",
		// Network failures are Docker's and the registry's, not a cluster's.
		`Get "https://ghcr.io/v2/": dial tcp: lookup ghcr.io: no such host`: "could not reach the image registry",
		`dial tcp 140.82.121.34:443: connect: connection refused`:           "could not reach the image registry",
		`something entirely different`:                                      "The Docker command failed.",
	}
	for out, want := range cases {
		d := DiagnoseDocker(out, rerun)
		if !strings.Contains(d.Headline, want) {
			t.Errorf("DiagnoseDocker(%q).Headline = %q, want it to contain %q", out, d.Headline, want)
		}
		for _, bad := range []string{"Kubernetes", "kubectl", "enrollment token", "pod"} {
			if strings.Contains(d.Headline+d.Hint, bad) {
				t.Errorf("DiagnoseDocker(%q) mentions %q: %+v", out, bad, d)
			}
		}
	}
	if d := DiagnoseDocker("boom", rerun); !strings.Contains(d.Hint, rerun) {
		t.Errorf("the fallback must say how to rerun: %q", d.Hint)
	}
}

func TestDiagnoseHelm(t *testing.T) {
	cases := map[string]string{
		`Error response from daemon: Conflict. The container name "/origamy-byod-nats" is already in use by container "abc"`: "already running on this host",
		`no matching manifest for linux/arm64/v8 in the manifest list entries`:                                               "linux/amd64 only",
		`exec /app/portal-agent: exec format error`:                                                                          "linux/amd64 only",
		`Error: UPGRADE FAILED: another operation (install/upgrade/rollback) is in progress`:                                 "still in progress",
		`something entirely different`:                                                                                       "The install command failed.",
	}
	for out, want := range cases {
		if d := DiagnoseHelm(out); !strings.Contains(d.Headline, want) {
			t.Errorf("DiagnoseHelm(%q).Headline = %q, want it to contain %q", out, d.Headline, want)
		}
	}
}
