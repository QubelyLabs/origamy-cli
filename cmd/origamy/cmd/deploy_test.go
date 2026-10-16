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
	t.Setenv("COMPOSE_FILE", "")
	project := func(t *testing.T, env, compose, initSQL string) string {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), env)
		writeFile(t, filepath.Join(dir, "docker-compose.yml"), compose)
		writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), initSQL)
		return dir
	}
	pending := "DATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=0.1.20\n" + resetPendingVar + "=0.1.20\n"
	run := func(t *testing.T, dir, tag, answers string) (got string, done bool, err error, out string) {
		withAnswers(t, false, answers)
		out = captureStdout(t, func() { got, done, err = deployPendingReset(dir, tag) })
		return got, done, err, out
	}

	// Nothing pending: nothing asked, Docker not asked either.
	f := newFakeCompose(t)
	if got, done, err, _ := run(t, project(t, "DATA_PLANE_ID=dp-1\n", compose248, stringSchemaSQL), "0.1.20", ""); got != "" || done || err != nil || len(f.calls) != 0 {
		t.Fatalf("no reset pending: %q, %v, %v, calls %v", got, done, err, f.calls)
	}

	// Pending, ClickHouse on the old table or none, the current bundle
	// downloaded: the operator is asked, and the deploy finishes it.
	for _, live := range []string{liveStringRow, ""} {
		f = newFakeCompose(t)
		f.live = live
		got, done, err, out := run(t, project(t, pending, compose253, jsonSchemaSQL), "0.1.20", "yes\n")
		if got != "0.1.20" || done || err != nil || !strings.Contains(out, "did not finish it") || !strings.Contains(out, "DISCARDED") {
			t.Fatalf("live %q: got %q, %v, %v\n%s", live, got, done, err, out)
		}
	}

	// Pending, and ClickHouse already holds the new table (another CLI
	// finished it, or the record outlived the reset): nothing to drop, so
	// nothing is asked, whatever the bundle.
	f = newFakeCompose(t)
	f.live = liveJSONRow
	got, done, err, out := run(t, project(t, pending, compose248, stringSchemaSQL), "0.1.20", "")
	if got != "0.1.20" || !done || err != nil || strings.Contains(out, asked) || !strings.Contains(out, "already holds the new events table") {
		t.Fatalf("live JSON: got %q, %v, %v\n%s", got, done, err, out)
	}

	for _, tc := range []struct {
		name, compose, initSQL, tag, answers, want string
		override                                   string
		noDocker                                   bool // refused before Docker is asked anything
	}{
		{"declined", compose253, jsonSchemaSQL, "0.1.20", "no\n", "Cancelled.", "", false},
		{"to a release before the reset", compose253, jsonSchemaSQL, "0.1.17", "yes\n", "Finish it first: origamy upgrade --version 0.1.20", "", true},
		{"to a moving tag", compose253, jsonSchemaSQL, "main", "yes\n", "the :main images are not a numbered release", "", true},
		{"the control plane's bundle predates the reset (ClickHouse)", compose248, jsonSchemaSQL, "0.1.20", "yes\n", "Update the control plane", "", false},
		{"the control plane's bundle predates the reset (schema)", compose253, stringSchemaSQL, "0.1.20", "yes\n", "Update the control plane", "", false},
		{"an override pins the old ClickHouse", compose253, jsonSchemaSQL, "0.1.20", "yes\n", "docker-compose.override.yml, which compose loads with docker-compose.yml here, changes the clickhouse service: it runs clickhouse/clickhouse-server:24.8",
			"services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:24.8\n", false},
	} {
		f = newFakeCompose(t)
		f.live = liveStringRow
		dir := project(t, pending, tc.compose, tc.initSQL)
		if tc.override != "" {
			writeFile(t, filepath.Join(dir, "docker-compose.override.yml"), tc.override)
		}
		got, done, err, out := run(t, dir, tc.tag, tc.answers)
		if got != "" || done || err == nil || !strings.Contains(out, tc.want) || !strings.Contains(out, "Nothing has been enrolled") {
			t.Errorf("%s: got %q, %v, %v\n%s", tc.name, got, done, err, out)
		}
		if tc.answers == "yes\n" && strings.Contains(out, asked) {
			t.Errorf("%s: refused after asking:\n%s", tc.name, out)
		}
		if tc.noDocker && len(f.calls) != 0 {
			t.Errorf("%s: Docker was asked: %v", tc.name, f.calls)
		}
	}
}
