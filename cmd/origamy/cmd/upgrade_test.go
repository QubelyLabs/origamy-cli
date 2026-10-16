package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeClickHouse stands in for dockerRun in the Docker storage reset: the
// ping fails until it has been tried failPings times (forever when < 0).
func fakeClickHouse(t *testing.T, failPings int) *[]string {
	t.Helper()
	var calls []string
	prevRun, prevTimeout, prevPoll := dockerRun, clickhouseReadyTimeout, clickhouseReadyPoll
	clickhouseReadyTimeout, clickhouseReadyPoll = 200*time.Millisecond, time.Millisecond
	pings := 0
	dockerRun = func(env []string, name string, args ...string) (string, error) {
		calls = append(calls, strings.Join(append(append(env, name), args...), " "))
		if strings.Contains(args[len(args)-1], "SELECT 1") {
			if pings++; failPings < 0 || pings <= failPings {
				return "Code: 210. DB::NetException: Connection refused (localhost:9000)", errors.New("exit status 210")
			}
		}
		return "", nil
	}
	t.Cleanup(func() { dockerRun, clickhouseReadyTimeout, clickhouseReadyPoll = prevRun, prevTimeout, prevPoll })
	return &calls
}

func TestResetDockerEventsStartsClickHouseFirst(t *testing.T) {
	calls := fakeClickHouse(t, 2)
	if err := resetDockerEvents("/dp", "/dp/.env", "0.1.20"); err != nil {
		t.Fatal(err)
	}
	// ClickHouse comes up on the compose file's image (with the target tag)
	// and answers BEFORE the old table is dropped and the schema applied, so
	// both run on the new server rather than the container still running.
	dc := "DP_IMAGE_TAG=0.1.20 docker compose --project-directory /dp --env-file /dp/.env "
	want := []string{
		dc + "up -d clickhouse",
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhouseDropEvents,
		dc + "run --rm --no-deps clickhouse-init",
	}
	if strings.Join(*calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(*calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestResetDockerEventsClickHouseNeverReady(t *testing.T) {
	calls := fakeClickHouse(t, -1)
	if err := resetDockerEvents("/dp", "/dp/.env", "0.1.20"); err == nil {
		t.Fatal("a ClickHouse that never answers must fail the reset")
	}
	for _, c := range *calls {
		if strings.Contains(c, "DROP TABLE") || strings.Contains(c, "clickhouse-init") {
			t.Fatalf("nothing may be dropped or applied before ClickHouse answers: %q", c)
		}
	}
}

// ── The Docker upgrade, end to end against a fake Docker and control plane ──

// Compose files of the two ClickHouse generations (the image version is all
// the reset checks), reading what the 0.1.20 bundle's services read.
const (
	compose248 = "services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:24.8\n  clickhouse-init:\n    image: clickhouse/clickhouse-server:24.8\n" +
		"  config-sync:\n    environment:\n      CONTROL_PLANE_URL: \"${CONFIG_URL}\"\n      TELEMETRY_URL: \"${TELEMETRY_URL}\"\n"
	compose253 = "services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:25.3\n  clickhouse-init:\n    image: clickhouse/clickhouse-server:25.3\n" +
		"  config-sync:\n    environment:\n      CONTROL_PLANE_URL: \"${CONFIG_URL}\"\n      TELEMETRY_URL: \"${TELEMETRY_URL}\"\n" +
		"  workflow-engine:\n    environment:\n      CONTROL_PLANE_URL: \"${CONTROL_PLANE_HTTP_URL}\"\n"
)

// fakeCompose stands in for dockerRun in a whole Docker upgrade. Everything
// succeeds except: the events table type query answers live, the first
// pingFails ClickHouse pings fail (all when < 0), the first initFails
// clickhouse-init runs fail, and a pull fails while failPull is set.
type fakeCompose struct {
	calls                []string
	live                 string
	pingFails, initFails int
	failPull             bool
	pings                int
}

func newFakeCompose(t *testing.T) *fakeCompose {
	t.Helper()
	f := &fakeCompose{}
	prevRun, prevTimeout, prevPoll := dockerRun, clickhouseReadyTimeout, clickhouseReadyPoll
	clickhouseReadyTimeout, clickhouseReadyPoll = 100*time.Millisecond, time.Millisecond
	dockerRun = func(env []string, name string, args ...string) (string, error) {
		f.calls = append(f.calls, strings.Join(append(append(append([]string{}, env...), name), args...), " "))
		switch last := args[len(args)-1]; {
		case last == clickhouseEventsPropertiesType:
			return f.live, nil
		case last == clickhousePing:
			if f.pings++; f.pingFails < 0 || f.pings <= f.pingFails {
				return "Code: 210. DB::NetException: Connection refused (localhost:9000)", errors.New("exit status 210")
			}
		case last == "clickhouse-init" && slices.Contains(args, "run") && f.initFails > 0:
			f.initFails--
			return "Code: 44. DB::Exception: Cannot create table events.events", errors.New("exit status 44")
		case last == "pull" && f.failPull:
			return "manifest unknown", errors.New("exit status 1")
		}
		return "", nil
	}
	t.Cleanup(func() { dockerRun, clickhouseReadyTimeout, clickhouseReadyPoll = prevRun, prevTimeout, prevPoll })
	return f
}

// since returns the calls made after the first n.
func (f *fakeCompose) since(n int) []string { return slices.Clone(f.calls[n:]) }

// callIndex is the position of the first call containing want, -1 if none.
func callIndex(calls []string, want string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, want) })
}

// newUpgradeProject writes a Docker project: compose, schema and the .env
// env(base) renders for the fake control plane, which serves the 0.1.20
// bundle (served, VERSION included, overrides it; "" drops a file).
func newUpgradeProject(t *testing.T, compose, initSQL string, env func(base string) string, served map[string]string) bundleProject {
	t.Helper()
	files := map[string]string{
		"VERSION":              "0.1.20\n",
		"docker-compose.yml":   compose253,
		"clickhouse-init.sql":  jsonSchemaSQL,
		"clickhouse-users.xml": "<clickhouse/>\n",
		"Caddyfile":            "{$INGEST_DOMAIN} {\n\treverse_proxy ingestion-gateway:8081\n}\n",
	}
	for k, v := range served {
		if v == "" {
			delete(files, k)
		} else {
			files[k] = v
		}
	}
	base := fakeControlPlane(t, files)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), compose)
	writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), initSQL)
	envPath := filepath.Join(dir, ".env")
	writeFile(t, envPath, env(base))
	// The project's own files are versions the control plane served; the
	// pre-reset ones came with :main.
	withKnownHashes(t, map[string][]string{
		"docker-compose.yml":  {sha256Hex([]byte(compose248)), sha256Hex([]byte(compose253))},
		"clickhouse-init.sql": {sha256Hex([]byte(stringSchemaSQL)), sha256Hex([]byte(jsonSchemaSQL))},
	})
	withKnownReleases(t, map[string][]string{
		sha256Hex([]byte(compose248)):      {"main"},
		sha256Hex([]byte(stringSchemaSQL)): {"main"},
	})
	return bundleProject{dir: dir, env: envPath, base: base, served: files}
}

// envAt renders a current-CLI .env on release tag for base.
func envAt(tag string) func(base string) string {
	return func(base string) string {
		return "CONTROL_PLANE_HTTP_URL=" + base + "\nCONFIG_URL=" + base + "/api/v1/config\nTELEMETRY_URL=" + base + "/api/v1/telemetry\n" +
			"DATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=" + tag + "\nCOMPOSE_PROFILES=full\n"
	}
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	prev := os.Stdout
	func() {
		os.Stdout = w
		defer func() {
			os.Stdout = prev
			_ = w.Close()
		}()
		fn()
	}()
	return <-out
}

// upgradeIn runs `origamy upgrade --version version --yes` (Docker) in dir.
func upgradeIn(t *testing.T, dir, version string, mode bundleMode) (string, error) {
	t.Helper()
	chdir(t, dir)
	var err error
	out := captureStdout(t, func() { err = upgradeDocker(version, "stable", false, false, false, true, mode) })
	return out, err
}

const notTouched = "Named volumes (your data) were not touched."

func TestUpgradeDockerFailedResetIsFinishedByTheNextRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fail      func(*fakeCompose)
		dropped   bool   // the failed run got as far as the drop
		failedOut string // what the failed run says
	}{
		{"schema init fails after the drop", func(f *fakeCompose) { f.initFails = 1 }, true, "the new schema could not be applied"},
		{"ClickHouse never answers", func(f *fakeCompose) { f.pingFails = -1 }, false, "ClickHouse is not accepting queries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose248, stringSchemaSQL, envAt("0.1.16"), nil)
			f := newFakeCompose(t)
			tc.fail(f)

			out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
			if err == nil {
				t.Fatalf("the reset must fail:\n%s", out)
			}
			// .env still names the release that runs, and records the reset.
			if got := readEnvVar(p.env, "DP_IMAGE_TAG"); got != "0.1.16" {
				t.Fatalf("DP_IMAGE_TAG = %q after a failed reset", got)
			}
			if got := readEnvVar(p.env, resetPendingVar); got != "0.1.20" {
				t.Fatalf("%s = %q", resetPendingVar, got)
			}
			if (callIndex(f.calls, "DROP TABLE") >= 0) != tc.dropped || callIndex(f.calls, "--remove-orphans") >= 0 {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
			if !strings.Contains(out, tc.failedOut) || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20`") || strings.Contains(out, notTouched) {
				t.Fatalf("output:\n%s", out)
			}
			// The refresh is in (ClickHouse 25.3, the new schema).
			if p.read(t, "docker-compose.yml") != compose253 || p.read(t, "clickhouse-init.sql") != jsonSchemaSQL {
				t.Fatal("the bundle refresh was not applied before the reset")
			}

			// The rerun finds the files already on 0.1.20, and the tag would
			// say so for an earlier CLI's run; the record makes it finish the
			// reset before anything else starts.
			f.pingFails, f.pings = 0, 0
			n := len(f.calls)
			out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
			if err != nil {
				t.Fatalf("rerun: %v\n%s", err, out)
			}
			calls := f.since(n)
			up, drop, init, all := callIndex(calls, "up -d clickhouse"), callIndex(calls, "DROP TABLE IF EXISTS events.events"), callIndex(calls, "run --rm --no-deps clickhouse-init"), callIndex(calls, "up -d --remove-orphans")
			if up < 0 || drop < up || init < drop || all < init {
				t.Fatalf("rerun calls:\n%s", strings.Join(calls, "\n"))
			}
			if got := readEnvVar(p.env, "DP_IMAGE_TAG"); got != "0.1.20" || envHasKey(p.env, resetPendingVar) {
				t.Fatalf("after the rerun: DP_IMAGE_TAG=%q, pending %v", got, envHasKey(p.env, resetPendingVar))
			}
			if !strings.Contains(out, "did not finish") || !strings.Contains(out, "Event history was discarded") || strings.Contains(out, notTouched) {
				t.Fatalf("rerun output:\n%s", out)
			}

			// And the run after that is a plain upgrade.
			n = len(f.calls)
			if out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto); err != nil || callIndex(f.since(n), "DROP") >= 0 || !strings.Contains(out, notTouched) {
				t.Fatalf("third run: %v, calls %v\n%s", err, f.since(n), out)
			}
		})
	}
}

func TestUpgradeDockerFailedPullRecordsNoReset(t *testing.T) {
	p := newUpgradeProject(t, compose248, stringSchemaSQL, envAt("0.1.16"), nil)
	f := newFakeCompose(t)
	f.failPull = true
	before := p.snapshot(t)
	if out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto); err == nil {
		t.Fatalf("the pull must fail:\n%s", out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("a failed pull touched the project:\nbefore %v\nafter  %v", before, after)
	}
}

func TestUpgradeDockerV018Project(t *testing.T) {
	// Deployed by CLI v0.1.15–v0.1.18: DP_IMAGE_TAG=main, a bare CONFIG_URL,
	// the pre-reset bundle. The schema file, not the tag, says this crosses
	// the reset.
	p := newUpgradeProject(t, compose248, stringSchemaSQL, v018Env, nil)
	f := newFakeCompose(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Upgrading main → 0.1.20 crosses the data plane's storage reset") || callIndex(f.calls, "DROP TABLE") < 0 {
		t.Fatalf("no reset:\n%s", out)
	}
	for _, bad := range []string{"Upgrade the control plane", "may be a downgrade", notTouched} {
		if strings.Contains(out, bad) {
			t.Errorf("output says %q:\n%s", bad, out)
		}
	}
	// Backups are named after the release the files came with.
	for _, bak := range []string{"docker-compose.yml.bak-main", "clickhouse-init.sql.bak-main"} {
		if _, err := os.Stat(filepath.Join(p.dir, bak)); err != nil {
			t.Errorf("%s: %v", bak, err)
		}
	}
	// The .env gains what the 0.1.20 compose reads, and the bare CONFIG_URL
	// that never returned config is corrected.
	for k, v := range map[string]string{
		"DP_IMAGE_TAG":           "0.1.20",
		"CONTROL_PLANE_HTTP_URL": p.base,
		"CONFIG_URL":             p.base + "/api/v1/config",
		"TELEMETRY_URL":          p.base + "/api/v1/telemetry",
		"DP_REDIS_PASSWORD":      "r",
	} {
		if got := readEnvVar(p.env, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if !strings.Contains(out, "Corrected CONFIG_URL") {
		t.Errorf("the correction is not explained:\n%s", out)
	}
}

func TestUpgradeDockerResetNeedsTheNewClickHouse(t *testing.T) {
	edited := strings.Replace(compose248, "services:\n", "services:\n  # mine\n", 1)
	for _, tc := range []struct {
		name    string
		compose string
		served  map[string]string
		mode    bundleMode
		want    []string
	}{
		{"control plane without /byod/VERSION", compose248, map[string]string{"VERSION": ""}, bundleAuto,
			[]string{"cannot be refreshed (control plane has no /byod/VERSION)", "clickhouse-server:24.8", "must serve /byod/VERSION for 0.1.20", "origamy deploy --token"}},
		{"control plane on another release", compose248, map[string]string{"VERSION": "0.1.19\n"}, bundleAuto,
			[]string{"control plane serves 0.1.19", "Pass --version 0.1.19"}},
		{"--keep-bundle", compose248, nil, bundleKeep,
			[]string{"(--keep-bundle)", "Rerun without --keep-bundle."}},
		{"edited compose file the refresh keeps", edited, nil, bundleAuto,
			[]string{"your edited docker-compose.yml, which the refresh keeps", "Pass --replace-bundle"}},
		{"the served compose file is a pre-reset one", compose248, map[string]string{"docker-compose.yml": compose248 + "# served\n"}, bundleAuto,
			[]string{"the docker-compose.yml your control plane serves for 0.1.20 does not run it", "Update the control plane"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, tc.compose, stringSchemaSQL, envAt("0.1.16"), tc.served)
			f := newFakeCompose(t)
			before := p.snapshot(t)
			out, err := upgradeIn(t, p.dir, "0.1.20", tc.mode)
			if err == nil {
				t.Fatalf("must refuse:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			// Refused before anything was touched: no pull, nothing dropped,
			// the project byte for byte as it was (no download directory).
			for _, c := range f.calls {
				if strings.Contains(c, " pull") || strings.Contains(c, " up ") || strings.Contains(c, "DROP") {
					t.Errorf("ran %q", c)
				}
			}
			if after := p.snapshot(t); !mapsEqual(before, after) {
				t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
			}
		})
	}

	// --replace-bundle takes the edited compose file out, and the reset runs.
	p := newUpgradeProject(t, edited, stringSchemaSQL, envAt("0.1.16"), nil)
	f := newFakeCompose(t)
	if out, err := upgradeIn(t, p.dir, "0.1.20", bundleReplace); err != nil || callIndex(f.calls, "DROP") < 0 || p.read(t, "docker-compose.yml") != compose253 {
		t.Fatalf("--replace-bundle: %v\n%s", err, out)
	}
}

func TestUpgradeDockerResetWithoutRefreshOnNewClickHouse(t *testing.T) {
	// The compose file already runs 25.3 (the 2026-09-10 bundle), so the
	// reset needs no refresh: it fetches the control plane's schema.
	p := newUpgradeProject(t, compose253, stringSchemaSQL, envAt("0.1.17"), map[string]string{"VERSION": ""})
	f := newFakeCompose(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Fetching the current ClickHouse schema") || p.read(t, "clickhouse-init.sql") != jsonSchemaSQL || callIndex(f.calls, "DROP") < 0 {
		t.Fatalf("output:\n%s", out)
	}
	if readBundleManifest(p.dir).Files["clickhouse-init.sql"] != sha256Hex([]byte(jsonSchemaSQL)) {
		t.Fatal("the fetched schema was not recorded")
	}

	// A control plane that still serves the pre-reset schema cannot reset.
	p = newUpgradeProject(t, compose253, stringSchemaSQL, envAt("0.1.17"), map[string]string{"VERSION": "", "clickhouse-init.sql": stringSchemaSQL})
	f = newFakeCompose(t)
	before := p.snapshot(t)
	if out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto); err == nil || !strings.Contains(out, "serves the pre-reset ClickHouse schema") || callIndex(f.calls, "pull") >= 0 {
		t.Fatalf("must refuse: %v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}
}

func TestUpgradeDockerLiveTableDecides(t *testing.T) {
	// An upgrade by an earlier CLI moved the tag and the schema file on but
	// left the old table: the running ClickHouse still holds String columns.
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.20"), nil)
	f := newFakeCompose(t)
	f.live = "String\n"
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || callIndex(f.calls, "DROP") < 0 || !strings.Contains(out, "Event history was discarded") || !strings.Contains(out, "still holds the pre-reset events table") {
		t.Fatalf("%v\n%s", err, out)
	}
	// On the JSON table, the same upgrade touches no data.
	f.live = "JSON\n"
	n := len(f.calls)
	if out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto); err != nil || callIndex(f.since(n), "DROP") >= 0 || !strings.Contains(out, notTouched) {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestUpgradeDockerJSONSchemaGuard(t *testing.T) {
	// A post-reset project moved to a release that cannot write its schema:
	// the hint says what to run, not to upgrade the control plane.
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.20"), nil)
	newFakeCompose(t)
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.17", bundleAuto)
	if err == nil || !strings.Contains(out, "release 0.1.17 cannot write") || !strings.Contains(out, "Pass --version "+minImageForJSONSchema+" or newer") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}
}
