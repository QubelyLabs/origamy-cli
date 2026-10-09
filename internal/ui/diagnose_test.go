package ui

import (
	"strings"
	"testing"
)

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
