package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnrollHint(t *testing.T) {
	newToken := "generate a new one"
	connectivity := "reach your control plane"
	cases := map[string]string{
		// The control plane's own phrases (byod_handlers.go) → a new token is the fix.
		"could not enroll: invalid or already-used enrollment token":                      newToken,
		"could not redeem enrollment token: enrollment token expired; generate a new one": newToken,
		"token is missing required fields — generate a new one from your dashboard":       newToken,
		"could not decode token: illegal base64 data at input byte 4":                     newToken,
		// Anything else is connectivity: a new token would not help.
		"could not reach the control plane to enroll: x509: certificate has expired or is not yet valid": connectivity,
		"could not enroll: HTTP 502":                                  connectivity,
		"refusing to enroll over a non-HTTPS URL (http://cp.example)": connectivity,
	}
	for msg, want := range cases {
		got := enrollHint(errors.New(msg))
		if !strings.HasPrefix(got, msg) {
			t.Errorf("hint must start with the server's message: %q", got)
		}
		if !strings.Contains(got, want) {
			t.Errorf("enrollHint(%q) = %q, want it to mention %q", msg, got, want)
		}
	}
}

func TestDeployPendingReset(t *testing.T) {
	project := func(t *testing.T, env, compose, initSQL string) string {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), env)
		writeFile(t, filepath.Join(dir, "docker-compose.yml"), compose)
		writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), initSQL)
		return dir
	}
	pending := "DATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=0.1.20\n" + resetPendingVar + "=0.1.20\n"

	// Nothing pending: nothing asked.
	withAnswers(t, false, "")
	if got, err := deployPendingReset(project(t, "DATA_PLANE_ID=dp-1\n", compose248, stringSchemaSQL), "0.1.20"); got != "" || err != nil {
		t.Fatalf("no reset pending: %q, %v", got, err)
	}

	// Pending, with the current bundle downloaded: the operator is asked, and
	// the deploy finishes it.
	withAnswers(t, false, "yes\n")
	var got string
	var err error
	out := captureStdout(t, func() { got, err = deployPendingReset(project(t, pending, compose253, jsonSchemaSQL), "0.1.20") })
	if got != "0.1.20" || err != nil || !strings.Contains(out, "did not finish it") || !strings.Contains(out, "DISCARDED") {
		t.Fatalf("got %q, %v\n%s", got, err, out)
	}

	for _, tc := range []struct {
		name, compose, initSQL, tag, answers, want string
	}{
		{"declined", compose253, jsonSchemaSQL, "0.1.20", "no\n", "Cancelled."},
		{"to a release before the reset", compose253, jsonSchemaSQL, "0.1.17", "yes\n", "Finish it first: origamy upgrade --version 0.1.20"},
		{"the control plane's bundle predates the reset (ClickHouse)", compose248, jsonSchemaSQL, "0.1.20", "yes\n", "Update the control plane"},
		{"the control plane's bundle predates the reset (schema)", compose253, stringSchemaSQL, "0.1.20", "yes\n", "Update the control plane"},
	} {
		withAnswers(t, false, tc.answers)
		out := captureStdout(t, func() { got, err = deployPendingReset(project(t, pending, tc.compose, tc.initSQL), tc.tag) })
		if got != "" || err == nil || !strings.Contains(out, tc.want) || !strings.Contains(out, "Nothing has been enrolled") {
			t.Errorf("%s: got %q, %v\n%s", tc.name, got, err, out)
		}
	}
}
