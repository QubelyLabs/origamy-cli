package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qubelylabs/origamy-cli/internal/token"
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

// servedBundle is a refresh whose download directory holds the given served
// compose file and schema, for deployResetPlan.
func servedBundle(t *testing.T, dir, compose, initSQL string) *bundleRefresh {
	t.Helper()
	tmp := filepath.Join(dir, bundleTempPrefix+"1")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, "docker-compose.yml"), compose)
	writeFile(t, filepath.Join(tmp, "clickhouse-init.sql"), initSQL)
	return &bundleRefresh{dir: dir, tmp: tmp, files: []bundleFile{
		{name: "docker-compose.yml", state: bundlePristine},
		{name: "clickhouse-init.sql", state: bundlePristine},
	}}
}

func TestDeployResetPlan(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "")
	project := func(t *testing.T, env string) string {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, ".env"), env)
		// The project's own files are the pre-reset ones; the bundle the
		// deploy downloaded decides.
		writeFile(t, filepath.Join(dir, "docker-compose.yml"), compose248)
		writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), stringSchemaSQL)
		return dir
	}
	plain := "DATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=0.1.20\n"
	pending := plain + resetPendingVar + "=0.1.20\n"
	live := func(gen string) liveSchema {
		return map[string]liveSchema{"json": {gen: "json", typ: "JSON"}, "string": {gen: "string", typ: "String"}}[gen]
	}
	missing := liveSchema{gen: liveMissing}

	for _, tc := range []struct {
		name, env    string
		live         liveSchema
		tag, answers string
		target       string // "" = nothing to do
		found        string
		asked        bool
		says         string
	}{
		{"nothing pending, the new table", plain, live("json"), "0.1.20", "", "", "", false, ""},
		{"nothing pending, the pre-reset table (liveSchemaMismatch refuses it)", plain, live("string"), "0.1.20", "", "", "", false, ""},
		{"nothing pending, no table, a release before the reset", plain, missing, "0.1.17", "", "", "", false, ""},
		// No events table under a release that writes the new schema: the
		// deploy creates it; nothing to drop, so nothing is asked.
		{"nothing pending, no table", plain, missing, "0.1.20", "", "0.1.20", liveMissing, false, "this deploy creates it with the new schema"},
		{"pending, the pre-reset table", pending, live("string"), "0.1.20", "yes\n", "0.1.20", "string", true, "did not finish it"},
		{"pending, no table", pending, missing, "0.1.20", "", "0.1.20", liveMissing, false, "nothing is dropped: this deploy creates it"},
		// Another CLI finished it, or the record outlived the reset: nothing
		// to drop or create, so nothing is asked, whatever the bundle.
		{"pending, the new table", pending, live("json"), "0.1.20", "", "0.1.20", "json", false, "already holds the new events table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			withAnswers(t, false, tc.answers)
			dir := project(t, tc.env)
			r := servedBundle(t, dir, compose253, jsonSchemaSQL)
			if tc.live.gen == "json" {
				r = servedBundle(t, t.TempDir(), compose248, stringSchemaSQL)
			}
			var target, found string
			var err error
			out := captureStdout(t, func() { target, found, err = deployResetPlan(dir, tc.tag, tc.live, r) })
			if err != nil || target != tc.target || found != tc.found || strings.Contains(out, asked) != tc.asked || !strings.Contains(out, tc.says) {
				t.Fatalf("got %q, %q, %v\n%s", target, found, err, out)
			}
			if tc.target == "" && len(f.calls) != 0 {
				t.Fatalf("Docker was asked: %v", f.calls)
			}
		})
	}

	for _, tc := range []struct {
		name, env, compose, initSQL, tag, answers, want string
		live                                            liveSchema
		override                                        string
		noDocker                                        bool // refused before Docker is asked anything
	}{
		{"declined", pending, compose253, jsonSchemaSQL, "0.1.20", "no\n", "Cancelled.", live("string"), "", false},
		{"to a release before the reset", pending, compose253, jsonSchemaSQL, "0.1.17", "yes\n", "Finish it first: origamy upgrade --version 0.1.20", live("string"), "", true},
		{"to a moving tag", pending, compose253, jsonSchemaSQL, "main", "yes\n", "the :main images are not a numbered release", live("string"), "", true},
		{"the control plane's bundle predates the reset (ClickHouse)", pending, compose248, jsonSchemaSQL, "0.1.20", "yes\n", "Update the control plane", live("string"), "", false},
		{"the control plane's bundle predates the reset (schema)", pending, compose253, stringSchemaSQL, "0.1.20", "yes\n", "Update the control plane", live("string"), "", false},
		// A schema of neither generation cannot be told to create the table
		// the release writes: refused before anything is dropped.
		{"the control plane's schema is neither", pending, compose253, "CREATE TABLE IF NOT EXISTS events.events (id UInt64) ENGINE = MergeTree ORDER BY id;\n", "0.1.20", "yes\n", "the new schema (native JSON payload columns)", live("string"), "", false},
		{"an override pins the old ClickHouse", pending, compose253, jsonSchemaSQL, "0.1.20", "yes\n", "docker-compose.override.yml, which compose loads with docker-compose.yml here, changes the clickhouse service: it runs clickhouse/clickhouse-server:24.8",
			live("string"), "services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:24.8\n", false},
		// Creating a missing table needs the same.
		{"no table, the bundle runs the old ClickHouse", plain, compose248, jsonSchemaSQL, "0.1.20", "", "ClickHouse here has no events table, which this deploy would create", missing, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			withAnswers(t, false, tc.answers)
			dir := project(t, tc.env)
			if tc.override != "" {
				writeFile(t, filepath.Join(dir, "docker-compose.override.yml"), tc.override)
			}
			r := servedBundle(t, dir, tc.compose, tc.initSQL)
			var target, found string
			var err error
			out := captureStdout(t, func() { target, found, err = deployResetPlan(dir, tc.tag, tc.live, r) })
			if target != "" || found != "" || err == nil || !strings.Contains(out, tc.want) || !strings.Contains(out, "Nothing has been enrolled") {
				t.Fatalf("got %q, %q, %v\n%s", target, found, err, out)
			}
			if tc.answers == "yes\n" && strings.Contains(out, asked) {
				t.Fatalf("refused after asking:\n%s", out)
			}
			if tc.noDocker && len(f.calls) != 0 {
				t.Fatalf("Docker was asked: %v", f.calls)
			}
		})
	}
}

// deployProject is a Docker project for an in-place deploy of plane dp-1:
// ./origamy-dp-dp-1 under parent, on the pre-reset bundle (compose and
// schema as given) with tag in .env, and a control plane serving the 0.1.20
// bundle. The project's files are versions the control plane served.
func deployProject(t *testing.T, compose, initSQL, tag string) (p bundleProject, parent string) {
	t.Helper()
	// The plan works on the absolute path the working directory resolves to.
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "origamy-dp-dp-1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	served := map[string]string{
		"VERSION":              "0.1.20\n",
		"docker-compose.yml":   compose253,
		"clickhouse-init.sql":  jsonSchemaSQL,
		"clickhouse-users.xml": "<clickhouse/>\n",
		"Caddyfile":            "{$INGEST_DOMAIN} {\n\treverse_proxy ingestion-gateway:8081\n}\n",
	}
	base := fakeControlPlane(t, served)
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), compose)
	writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), initSQL)
	writeFile(t, filepath.Join(dir, "Caddyfile"), "my.domain {\n\treverse_proxy ingestion-gateway:8081\n}\n")
	env := filepath.Join(dir, ".env")
	writeFile(t, env, "CONTROL_PLANE_HTTP_URL="+base+"\nDATA_PLANE_ID=dp-1\nDP_IMAGE_TAG="+tag+"\nCOMPOSE_PROFILES=full\nCLICKHOUSE_PASSWORD=keep-me\n")
	withKnownHashes(t, map[string][]string{
		"docker-compose.yml":  {sha256Hex([]byte(compose248)), sha256Hex([]byte(compose253))},
		"clickhouse-init.sql": {sha256Hex([]byte(stringSchemaSQL)), sha256Hex([]byte(jsonSchemaSQL))},
	})
	withKnownReleases(t, map[string][]string{
		sha256Hex([]byte(compose248)):      {"main"},
		sha256Hex([]byte(stringSchemaSQL)): {"main"},
	})
	return bundleProject{dir: dir, env: env, base: base, served: served}, parent
}

// planIn runs planDocker for plane dp-1 from dir, answering the questions
// with their defaults.
func planIn(t *testing.T, dir, tag, base string) (*dockerPlan, string, error) {
	t.Helper()
	chdir(t, dir)
	withAnswers(t, false, "\n\n\n")
	var p *dockerPlan
	var err error
	out := captureStdout(t, func() { p, err = planDocker(tag, false, true, "dp-1", base) })
	return p, out, err
}

// TestPlanDockerTouchesNothing (cardinal rule 3): an in-place deploy downloads
// the bundle next to the project and checks it, and what ClickHouse holds,
// before anything is swapped in; a refusal, or an enrollment that fails
// after the plan, leaves the project byte for byte as it was.
func TestPlanDockerTouchesNothing(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "")
	for _, tc := range []struct {
		name    string
		setup   func(*fakeCompose)
		tag     string
		refused string
	}{
		// CLI v0.1.15–v0.1.18 era (tag main), the pre-reset table: deploy
		// points at the upgrade, and the 0.1.20 bundle never lands.
		{"the pre-reset table", func(f *fakeCompose) { f.live = liveStringRow }, "0.1.20", "Run `origamy upgrade --version 0.1.20` here instead"},
		// ClickHouse stopped: it is started on what the project runs now and
		// asked, rather than the guard passing for want of an answer.
		{"the pre-reset table, ClickHouse stopped", func(f *fakeCompose) { f.live, f.stopped = liveStringRow, true }, "0.1.20", "Run `origamy upgrade --version 0.1.20` here instead"},
		{"ClickHouse cannot be asked", func(f *fakeCompose) { f.live, f.probeFails = liveStringRow, true }, "0.1.20", "ClickHouse could not be asked which events table this plane holds"},
		{"ClickHouse cannot be started", func(f *fakeCompose) { f.stopped, f.startFails = true, true }, "0.1.20", "ClickHouse could not be asked which events table this plane holds"},
		{"another type", func(f *fakeCompose) { f.live = "Nullable(String)\t" + eventsColumnMark + "\n" }, "0.1.20", "neither the pre-reset (String) nor the new (JSON) schema"},
		{"the new table, a release before the reset", func(f *fakeCompose) { f.live = liveJSONRow }, "0.1.17", "which data-plane release 0.1.17 cannot write to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, parent := deployProject(t, compose248, stringSchemaSQL, "main")
			f := newFakeCompose(t)
			tc.setup(f)
			before := p.snapshot(t)
			plan, out, err := planIn(t, parent, tc.tag, p.base)
			if err == nil || plan != nil || !strings.Contains(out, tc.refused) || !strings.Contains(out, "Nothing has been enrolled") {
				t.Fatalf("%v\n%s", err, out)
			}
			if after := p.snapshot(t); !mapsEqual(before, after) {
				t.Fatalf("a refused deploy touched the project:\nbefore %v\nafter  %v", before, after)
			}
			for _, c := range f.calls {
				if strings.Contains(c, "DROP") || strings.Contains(c, "run --rm --no-deps clickhouse-init") || strings.Contains(c, "--remove-orphans") {
					t.Fatalf("ran %q", c)
				}
				// Started, if at all, on the release and files it runs now.
				if strings.Contains(c, "up -d clickhouse") && (!strings.HasPrefix(c, "DP_IMAGE_TAG=main docker compose --project-directory "+p.dir+" --env-file "+p.env+" up") || strings.Contains(c, " -f ")) {
					t.Fatalf("started ClickHouse with %q", c)
				}
			}
			if tc.name == "the pre-reset table, ClickHouse stopped" && callIndex(f.calls, "up -d clickhouse") < 0 {
				t.Fatalf("ClickHouse was not started to be asked:\n%s", strings.Join(f.calls, "\n"))
			}
		})
	}

	// A plan that goes through touches nothing either until it is applied:
	// an enrollment that fails then leaves the project as it was.
	p, parent := deployProject(t, compose253, jsonSchemaSQL, "0.1.19")
	f := newFakeCompose(t)
	f.live = liveJSONRow
	before := p.snapshot(t)
	plan, out, err := planIn(t, parent, "0.1.20", p.base)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(p.dir, projectLockFile)); err != nil {
		t.Fatal("the plan does not hold the project's lock")
	}
	plan.close()
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("a plan that was never applied touched the project:\nbefore %v\nafter  %v", before, after)
	}

	// A fresh deploy's directory goes again with it.
	parent = t.TempDir()
	newFakeCompose(t)
	plan, out, err = planIn(t, parent, "0.1.20", p.base)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	plan.close()
	if _, err := os.Stat(filepath.Join(parent, "origamy-dp-dp-1")); !os.IsNotExist(err) {
		t.Fatalf("the directory a fresh plan made is still there: %v", err)
	}
}

// TestApplyDockerInPlace: the bundle goes in once the token is redeemed, the
// files it replaces backed up, and the deploy succeeds only when the events
// table is the one the release writes (cardinal rule 2).
func TestApplyDockerInPlace(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "")
	tok := func(base string) *token.Enrollment {
		return &token.Enrollment{ID: "dp-1", Tok: "tok", Addr: "tunnel.example.com:443", URL: base}
	}
	for _, tc := range []struct {
		name    string
		live    string
		onUp    func(*fakeCompose)
		ok      bool
		says    string
		pending string // the record in .env afterwards
		creates bool   // the plan creates the missing table first
	}{
		{"the new table", liveJSONRow, nil, true, "Deployed", "", false},
		// No events table (a released CLI dropped it): created before the
		// services start, nothing dropped, the record cleared after.
		{"no events table", "", nil, true, "the events table was created with the new schema (nothing was dropped)", "", true},
		// Gone, or the pre-reset one, once the services are up: a failure,
		// recorded as a reset for the next upgrade to finish.
		{"gone after up", liveJSONRow, func(f *fakeCompose) { f.live = "" }, false, "ClickHouse holds no events table", "0.1.20", false},
		{"pre-reset after up", liveJSONRow, func(f *fakeCompose) { f.live = liveStringRow }, false, "the pre-reset events table (String payload columns), which the 0.1.20 images cannot write", "0.1.20", false},
		{"not answering after up", liveJSONRow, func(f *fakeCompose) { f.probeFails = true }, false, "did not say which events table it holds", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, parent := deployProject(t, compose253, jsonSchemaSQL, "0.1.19")
			f := newFakeCompose(t)
			f.live, f.onUp = tc.live, tc.onUp
			plan, out, err := planIn(t, parent, "0.1.20", p.base)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			defer plan.close()
			if (plan.resetPending == "0.1.20") != tc.creates {
				t.Fatalf("plan: %+v", plan)
			}
			out = captureStdout(t, func() { err = applyDocker(tok(p.base), nil, plan) })
			if (err == nil) != tc.ok || !strings.Contains(out, tc.says) || strings.Contains(out, "Deployed") != tc.ok {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := readEnvVar(p.env, resetPendingVar); got != tc.pending {
				t.Fatalf("%s = %q", resetPendingVar, got)
			}
			if (callIndex(f.calls, "run --rm --no-deps clickhouse-init") >= 0) != tc.creates || callIndex(f.calls, "DROP") >= 0 {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
			// The bundle went in: the operator's Caddyfile backed up, the
			// manifest written, the passwords kept.
			if p.read(t, "Caddyfile") != p.served["Caddyfile"] || !strings.HasPrefix(p.read(t, "Caddyfile.bak-0.1.19"), "my.domain") ||
				readBundleManifest(p.dir).Version != "0.1.20" || readEnvVar(p.env, "CLICKHOUSE_PASSWORD") != "keep-me" {
				t.Fatalf("project after the deploy:\n%v", p.snapshot(t))
			}
		})
	}
}
