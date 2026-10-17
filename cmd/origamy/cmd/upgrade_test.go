package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Answers of the events-table probe (clickhouseEventsPropertiesType), as
// ClickHouse prints them.
const (
	liveJSONRow   = "JSON\t" + eventsColumnMark + "\n"
	liveStringRow = "String\t" + eventsColumnMark + "\n"
)

// fakeCompose stands in for Docker in the storage reset and in a whole Docker
// upgrade or deploy, with a ClickHouse that keeps state: live is what the
// events-table probe answers ("" = no table); a DROP empties it, and a
// clickhouse-init run that succeeds leaves afterInit there (the new table,
// unless a test says the schema silently failed). Everything else succeeds,
// except: ClickHouse answers nothing while stopped (until an `up`; startFails
// makes `up -d clickhouse` fail), the first pingFails pings fail (all when
// < 0), the probe cannot be asked while probeFails (or for its first
// probeHangs answers, nor after an init while checkFails), the first
// initFails clickhouse-init runs fail, and a pull fails while failPull is
// set. onUp runs at every `up` of the whole project (compose reruns the init
// then). `docker compose config --format json` renders the clickhouse image
// of the -f files given (the last that sets it wins), after a compose
// warning, like compose does; `ps` lists one running service.
type fakeCompose struct {
	calls                           []string
	live, afterInit                 string
	pingFails, initFails            int
	probeHangs                      int
	stopped, probeFails, checkFails bool
	startFails, failPull            bool
	onUp                            func(*fakeCompose)
	pings, inits, probes            int
}

func newFakeCompose(t *testing.T) *fakeCompose {
	t.Helper()
	f := &fakeCompose{afterInit: liveJSONRow}
	prevTimeout, prevPoll := clickhouseReadyTimeout, clickhouseReadyPoll
	clickhouseReadyTimeout, clickhouseReadyPoll = 100*time.Millisecond, time.Millisecond
	prevSettle, prevWait, prevPoll2 := composeSettle, composeWait, composePoll
	composeSettle, composeWait, composePoll = 0, 100*time.Millisecond, time.Millisecond
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		f.calls = append(f.calls, strings.Join(append(append(append([]string{}, env...), name), args...), " "))
		switch last := args[len(args)-1]; {
		case slices.Contains(args, "up"):
			if last == "clickhouse" && f.startFails {
				return "Error response from daemon: no such image", errors.New("exit status 1")
			}
			f.stopped = false
			if last == "--remove-orphans" && f.onUp != nil {
				f.onUp(f)
			}
		case f.stopped && slices.Contains(args, "exec"):
			return "service \"clickhouse\" is not running", errors.New("exit status 1")
		case last == clickhouseEventsPropertiesType:
			if f.probes++; f.probeFails || f.probes <= f.probeHangs || (f.checkFails && f.inits > 0) {
				return "", errors.New("no answer within 20s")
			}
			return f.live, nil
		case last == clickhousePing:
			if f.pings++; f.pingFails < 0 || f.pings <= f.pingFails {
				return "Code: 210. DB::NetException: Connection refused (localhost:9000)", errors.New("exit status 210")
			}
		case last == clickhouseDropEvents:
			f.live = ""
		case last == "clickhouse-init" && slices.Contains(args, "run"):
			if f.initFails > 0 {
				f.initFails--
				return "Code: 44. DB::Exception: Cannot create table events.events", errors.New("exit status 44")
			}
			f.live, f.inits = f.afterInit, f.inits+1
			return "ClickHouse schema ready.", nil
		case last == "pull" && f.failPull:
			return "manifest unknown", errors.New("exit status 1")
		case last == "json" && slices.Contains(args, "config"):
			return renderComposeConfig(args)
		case slices.Contains(args, "ps") && last == "json":
			return `{"Service":"portal-agent","State":"running","Status":"Up 5 seconds"}` + "\n", nil
		}
		return "", nil
	})
	t.Cleanup(func() {
		clickhouseReadyTimeout, clickhouseReadyPoll = prevTimeout, prevPoll
		composeSettle, composeWait, composePoll = prevSettle, prevWait, prevPoll2
	})
	return f
}

var reTestClickHouseImage = regexp.MustCompile(`(?m)^  clickhouse:\n    image: (\S+)`)

// renderComposeConfig is the fake `docker compose config --format json`.
func renderComposeConfig(args []string) (string, error) {
	image := ""
	for i, a := range args {
		if a != "-f" {
			continue
		}
		b, err := os.ReadFile(args[i+1])
		if err != nil {
			return "open " + args[i+1] + ": no such file or directory", errors.New("exit status 1")
		}
		if m := reTestClickHouseImage.FindSubmatch(b); m != nil {
			image = string(m[1])
		}
	}
	return `time="2026-10-16T20:00:00Z" level=warning msg="The \"WRITE_KEYS\" variable is not set. Defaulting to a blank string."` + "\n" +
		fmt.Sprintf(`{"name": "dp", "services": {"clickhouse": {"image": %q, "volumes": [{"type": "volume", "source": "clickhouse-data", "target": "/var/lib/clickhouse"}]}}, "volumes": {"clickhouse-data": {"name": "dp_clickhouse-data"}}}`, image) + "\n", nil
}

// resetProject is a project directory whose clickhouse-init.sql is sql, for
// the reset alone.
func resetProject(t *testing.T, sql string) (dir, env string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), sql)
	return dir, filepath.Join(dir, ".env")
}

func TestResetDockerEventsStartsClickHouseFirst(t *testing.T) {
	f := newFakeCompose(t)
	f.live, f.pingFails = liveStringRow, 2
	dir, env := resetProject(t, jsonSchemaSQL)
	var outcome resetOutcome
	var err error
	out := captureStdout(t, func() { outcome, err = resetDockerEvents(dir, env, "0.1.20", "string") })
	if err != nil || outcome != resetDropped {
		t.Fatalf("%v, %v\n%s", outcome, err, out)
	}
	// ClickHouse comes up on the compose file's image (with the target tag)
	// and answers BEFORE anything else; it is asked which table it holds,
	// the old one dropped, the schema applied on the new server rather than
	// the container still running, and the result checked.
	dc := "DP_IMAGE_TAG=0.1.20 docker compose --project-directory " + dir + " --env-file " + env + " "
	want := []string{
		dc + "up -d clickhouse",
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhouseEventsPropertiesType,
		dc + "exec -T clickhouse sh -c " + clickhouseDropEvents,
		dc + "run --rm --no-deps clickhouse-init",
		dc + "exec -T clickhouse sh -c " + clickhouseEventsPropertiesType,
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out, "Events table recreated") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestResetDockerEventsClickHouseNeverReady(t *testing.T) {
	f := newFakeCompose(t)
	f.live, f.pingFails = liveStringRow, -1
	dir, env := resetProject(t, jsonSchemaSQL)
	var err error
	captureStdout(t, func() { _, err = resetDockerEvents(dir, env, "0.1.20", "string") })
	if err == nil {
		t.Fatal("a ClickHouse that never answers must fail the reset")
	}
	for _, c := range f.calls {
		if strings.Contains(c, "DROP TABLE") || strings.Contains(c, "clickhouse-init") {
			t.Fatalf("nothing may be dropped or applied before ClickHouse answers: %q", c)
		}
	}
}

// TestResetDockerEventsDropsOnlyThePreResetTable is the cardinal rule: the
// events table is dropped only when ClickHouse says it is the String one, and
// the run decided on that table (asked to drop it).
func TestResetDockerEventsDropsOnlyThePreResetTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		live       string
		probeFails bool
		found      string // what the run decided on
		want       resetOutcome
		dropped    bool
		init       bool
		failure    string // "" = the reset succeeds
	}{
		{"the pre-reset table", liveStringRow, false, "string", resetDropped, true, true, ""},
		{"no events table", "", false, liveMissing, resetCreated, false, true, ""},
		{"no events table, a drop agreed to", "", false, "string", resetCreated, false, true, ""},
		{"the new table already", liveJSONRow, false, "string", resetAlready, false, false, ""},
		{"the new table, bare answer", "JSON\n", false, "string", resetAlready, false, false, ""},
		{"ClickHouse cannot be asked", "", true, "string", resetNone, false, false, "so nothing was dropped"},
		{"another type", "Object('json')\t" + eventsColumnMark + "\n", false, "string", resetNone, false, false, "neither the pre-reset (String) nor the new (JSON)"},
		// A run that did not ask to drop accepts no pre-reset table, and one
		// that found the new table accepts only that.
		{"found the new table, still there", liveJSONRow, false, "json", resetAlready, false, false, ""},
		{"found the new table, now the pre-reset one", liveStringRow, false, "json", resetNone, false, false, "it asks again, before it drops anything"},
		{"found the new table, now none", "", false, "json", resetNone, false, false, "although it held the new events table"},
		{"found no table, now the pre-reset one", liveStringRow, false, liveMissing, resetNone, false, false, "although it held no events table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			f.live, f.probeFails = tc.live, tc.probeFails
			dir, env := resetProject(t, jsonSchemaSQL)
			var got resetOutcome
			var err error
			out := captureStdout(t, func() { got, err = resetDockerEvents(dir, env, "0.1.20", tc.found) })
			if (err == nil) != (tc.failure == "") || got != tc.want || !strings.Contains(out, tc.failure) {
				t.Fatalf("got %v, %v\n%s", got, err, out)
			}
			if (callIndex(f.calls, "DROP TABLE") >= 0) != tc.dropped || (callIndex(f.calls, "clickhouse-init") >= 0) != tc.init {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
			if tc.failure != "" && strings.Contains(out, "Events table recreated") {
				t.Fatalf("a failed reset said the table was recreated:\n%s", out)
			}
		})
	}
}

// TestResetDockerEventsNeedsTheNewSchemaFile: the init service applies the
// project's clickhouse-init.sql, so nothing is dropped or created unless it
// is the new schema.
func TestResetDockerEventsNeedsTheNewSchemaFile(t *testing.T) {
	for name, sql := range map[string]string{
		"the pre-reset schema": stringSchemaSQL,
		"neither schema":       "CREATE TABLE IF NOT EXISTS events.events (id UInt64) ENGINE = MergeTree ORDER BY id;\n",
		"no file":              "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCompose(t)
			f.live = liveStringRow
			dir, env := resetProject(t, sql)
			if sql == "" {
				_ = os.Remove(filepath.Join(dir, "clickhouse-init.sql"))
			}
			var err error
			out := captureStdout(t, func() { _, err = resetDockerEvents(dir, env, "0.1.20", "string") })
			if err == nil || !strings.Contains(out, "is not the new ClickHouse schema") || !strings.Contains(out, "nothing was dropped or created") {
				t.Fatalf("%v\n%s", err, out)
			}
			if callIndex(f.calls, "DROP") >= 0 || callIndex(f.calls, "clickhouse-init") >= 0 || f.live != liveStringRow {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
		})
	}
}

// TestResetDockerEventsChecksTheTable: the bundle's init service exits 0 even
// when a statement fails, so the reset is done only once ClickHouse reports
// the new table.
func TestResetDockerEventsChecksTheTable(t *testing.T) {
	for _, tc := range []struct{ name, afterInit, want string }{
		{"no table after the schema", "", "is missing afterwards"},
		{"still the old table", liveStringRow, "still the pre-reset one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			f.live, f.afterInit = liveStringRow, tc.afterInit
			dir, env := resetProject(t, jsonSchemaSQL)
			var err error
			out := captureStdout(t, func() { _, err = resetDockerEvents(dir, env, "0.1.20", "string") })
			if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, "Events table recreated") ||
				!strings.Contains(out, "ClickHouse schema ready.") || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20`") {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
	// The check itself cannot be asked: not done either.
	f := newFakeCompose(t)
	f.live, f.checkFails = liveStringRow, true
	dir, env := resetProject(t, jsonSchemaSQL)
	var err error
	out := captureStdout(t, func() { _, err = resetDockerEvents(dir, env, "0.1.20", "string") })
	if err == nil || !strings.Contains(out, "could not be asked whether") || strings.Contains(out, "Events table recreated") {
		t.Fatalf("%v\n%s", err, out)
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
	// The upgrade works on the absolute path the working directory resolves to.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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

// upgradeAsking is upgradeIn without --yes: the prompts read answers, from a
// pipe (a rejected or missing answer cancels).
func upgradeAsking(t *testing.T, dir, version string, mode bundleMode, requireSigned bool, answers string) (string, error) {
	t.Helper()
	chdir(t, dir)
	withAnswers(t, false, answers)
	var err error
	out := captureStdout(t, func() { err = upgradeDocker(version, "stable", false, false, requireSigned, false, mode) })
	return out, err
}

const asked = "Type 'yes' to continue"

// withPending records an unfinished reset to target in an .env renderer.
func withPending(env func(string) string, target string) func(string) string {
	return func(base string) string { return env(base) + resetPendingVar + "=" + target + "\n" }
}

const notTouched = "Named volumes (your data) were not touched."

func TestUpgradeDockerFailedResetIsFinishedByTheNextRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fail      func(*fakeCompose)
		dropped   bool   // the failed run got as far as the drop
		failedOut string // what the failed run says
		rerunSays string // the rerun's summary
	}{
		// The rerun finds no table (the failed run dropped it): it creates
		// one, and has nothing to drop.
		{"schema init fails after the drop", func(f *fakeCompose) { f.initFails = 1 }, true, "the new schema could not be applied", "the events table was created with the new schema"},
		// The rerun finds the old table still there: it drops it.
		{"ClickHouse never answers", func(f *fakeCompose) { f.pingFails = -1 }, false, "ClickHouse is not accepting queries", "Event history was discarded"},
		// Applied, and ClickHouse has no table afterwards: not finished.
		{"the schema leaves no table", func(f *fakeCompose) { f.afterInit = "" }, true, "is missing afterwards", "the events table was created with the new schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose248, stringSchemaSQL, envAt("0.1.16"), nil)
			f := newFakeCompose(t)
			f.live = liveStringRow
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
			if !strings.Contains(out, tc.failedOut) || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20`") ||
				strings.Contains(out, notTouched) || strings.Contains(out, "Events table recreated") {
				t.Fatalf("output:\n%s", out)
			}
			// The refresh is in (ClickHouse 25.3, the new schema).
			if p.read(t, "docker-compose.yml") != compose253 || p.read(t, "clickhouse-init.sql") != jsonSchemaSQL {
				t.Fatal("the bundle refresh was not applied before the reset")
			}

			// The rerun finds the files already on 0.1.20, and the tag would
			// say so for an earlier CLI's run; the record makes it finish the
			// reset before anything else starts.
			f.pingFails, f.pings, f.afterInit = 0, 0, liveJSONRow
			n := len(f.calls)
			out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
			if err != nil {
				t.Fatalf("rerun: %v\n%s", err, out)
			}
			calls := f.since(n)
			up, drop, init, all := callIndex(calls, "up -d clickhouse"), callIndex(calls, "DROP TABLE IF EXISTS events.events"), callIndex(calls, "run --rm --no-deps clickhouse-init"), callIndex(calls, "up -d --remove-orphans")
			if up < 0 || init < up || all < init || (drop >= 0) != (tc.rerunSays == "Event history was discarded") || (drop >= 0 && (drop < up || init < drop)) {
				t.Fatalf("rerun calls:\n%s", strings.Join(calls, "\n"))
			}
			if got := readEnvVar(p.env, "DP_IMAGE_TAG"); got != "0.1.20" || envHasKey(p.env, resetPendingVar) {
				t.Fatalf("after the rerun: DP_IMAGE_TAG=%q, pending %v", got, envHasKey(p.env, resetPendingVar))
			}
			if !strings.Contains(out, "did not finish") || !strings.Contains(out, tc.rerunSays) || strings.Contains(out, notTouched) {
				t.Fatalf("rerun output:\n%s", out)
			}
			// The rerun changed no file, yet the run before it swapped them
			// under ClickHouse: it is restarted.
			if callIndex(calls, "restart clickhouse") < all {
				t.Fatalf("rerun calls:\n%s", strings.Join(calls, "\n"))
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
	f.live, f.failPull = liveStringRow, true
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
	// the pre-reset bundle, ClickHouse stopped. ClickHouse is started on the
	// release and files the project runs now, asked, and says this crosses
	// the reset; the reset asks it again once it is up on the new bundle, and
	// drops the table it finds is the old one.
	p := newUpgradeProject(t, compose248, stringSchemaSQL, v018Env, nil)
	f := newFakeCompose(t)
	f.live, f.stopped = liveStringRow, true
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "upgrading main → 0.1.20 crosses the data plane's storage reset") || callIndex(f.calls, "DROP TABLE") < 0 {
		t.Fatalf("no reset:\n%s", out)
	}
	if first := callIndex(f.calls, "up -d clickhouse"); first < 0 || first > callIndex(f.calls, " pull") ||
		f.calls[first] != "DP_IMAGE_TAG=main docker compose --project-directory "+p.dir+" --env-file "+p.env+" up -d clickhouse" {
		t.Fatalf("ClickHouse was not started on the release it runs to be asked:\n%s", strings.Join(f.calls, "\n"))
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
	f.live = liveStringRow
	if out, err := upgradeIn(t, p.dir, "0.1.20", bundleReplace); err != nil || callIndex(f.calls, "DROP") < 0 || p.read(t, "docker-compose.yml") != compose253 {
		t.Fatalf("--replace-bundle: %v\n%s", err, out)
	}
}

func TestUpgradeDockerResetWithoutRefreshOnNewClickHouse(t *testing.T) {
	// The compose file already runs 25.3 (the 2026-09-10 bundle), so the
	// reset needs no refresh: it fetches the control plane's schema.
	p := newUpgradeProject(t, compose253, stringSchemaSQL, envAt("0.1.17"), map[string]string{"VERSION": ""})
	f := newFakeCompose(t)
	f.live = liveStringRow
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Fetching the current ClickHouse schema") || p.read(t, "clickhouse-init.sql") != jsonSchemaSQL || callIndex(f.calls, "DROP") < 0 {
		t.Fatalf("output:\n%s", out)
	}
	// The schema it replaced is backed up, and the box says what changed.
	if p.read(t, "clickhouse-init.sql.bak-main") != stringSchemaSQL || !strings.Contains(out, "not refreshed (control plane has no /byod/VERSION)") ||
		!strings.Contains(out, "clickhouse-init.sql replaced with your control plane's current schema for the storage reset — the previous one is in clickhouse-init.sql.bak-main") ||
		strings.Contains(out, "left as-is (") {
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

// TestUpgradeDockerNeverDropsTheNewTable: ClickHouse holding the JSON table
// decides, whatever the project's clickhouse-init.sql (the pre-reset one put
// back over it, say) and its tag say. Nothing is dropped and nothing asked.
func TestUpgradeDockerNeverDropsTheNewTable(t *testing.T) {
	edited := stringSchemaSQL + "\n-- mine\n"
	for _, tc := range []struct {
		name, installed, schema string
		mode                    bundleMode
		warnsFile               bool // the pre-reset file stays: said so
	}{
		{"0.1.20 → 0.1.20", "0.1.20", stringSchemaSQL, bundleAuto, false},
		{"0.1.19 → 0.1.20", "0.1.19", stringSchemaSQL, bundleAuto, false},
		{"main → 0.1.20", "main", stringSchemaSQL, bundleAuto, false},
		{"0.1.20 → 0.1.20, the file edited (kept)", "0.1.20", edited, bundleAuto, true},
		{"0.1.19 → 0.1.20, the file edited (kept)", "0.1.19", edited, bundleAuto, true},
		{"0.1.19 → 0.1.20, --keep-bundle", "0.1.19", stringSchemaSQL, bundleKeep, true},
		{"0.1.16 → 0.1.20 by tag, --keep-bundle", "0.1.16", stringSchemaSQL, bundleKeep, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, tc.schema, envAt(tc.installed), nil)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			// Not --yes, and no answer: a question would cancel the run.
			out, err := upgradeAsking(t, p.dir, "0.1.20", tc.mode, false, "")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, c := range f.calls {
				if strings.Contains(c, "DROP") || strings.Contains(c, "clickhouse-init") {
					t.Fatalf("ran %q\n%s", c, out)
				}
			}
			if strings.Contains(out, asked) || strings.Contains(out, "crosses") || !strings.Contains(out, notTouched) || readEnvVar(p.env, "DP_IMAGE_TAG") != "0.1.20" {
				t.Fatalf("output:\n%s", out)
			}
			if tc.warnsFile != strings.Contains(out, "clickhouse-init.sql here is the pre-reset schema") {
				t.Fatalf("warning about the file (want %v):\n%s", tc.warnsFile, out)
			}
		})
	}
}

// TestUpgradeDockerStaleResetRecord: a reset recorded in .env that ClickHouse
// has already finished (another CLI did, or the record outlived the reset) is
// finished without dropping or asking: the table is checked, the tag moved,
// the services that mount bundle files restarted and the record cleared.
func TestUpgradeDockerStaleResetRecord(t *testing.T) {
	for _, installed := range []string{"0.1.20", "0.1.16"} {
		t.Run(installed, func(t *testing.T) {
			env := func(base string) string {
				return strings.Replace(withPending(envAt(installed), "0.1.20")(base), "COMPOSE_PROFILES=full", "COMPOSE_PROFILES=full,ingress", 1)
			}
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, env, nil)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			out, err := upgradeAsking(t, p.dir, "0.1.20", bundleAuto, false, "")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if callIndex(f.calls, "DROP") >= 0 || callIndex(f.calls, "clickhouse-init") >= 0 || strings.Contains(out, asked) {
				t.Fatalf("calls:\n%s\n%s", strings.Join(f.calls, "\n"), out)
			}
			if !strings.Contains(out, "recorded an unfinished storage reset") || !strings.Contains(out, "already holds the new events table") ||
				!strings.Contains(out, "nothing was dropped") || strings.Contains(out, "Event history was discarded") {
				t.Fatalf("output:\n%s", out)
			}
			if readEnvVar(p.env, "DP_IMAGE_TAG") != "0.1.20" || envHasKey(p.env, resetPendingVar) {
				t.Fatalf(".env:\n%s", readFile(t, p.env))
			}
			// Nothing was replaced in this run, yet both services that mount
			// bundle files are restarted, after `up`.
			all := callIndex(f.calls, "up -d --remove-orphans")
			if ch, caddy := callIndex(f.calls, "restart clickhouse"), callIndex(f.calls, "restart caddy"); all < 0 || ch < all || caddy < all {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
		})
	}
}

// TestUpgradeDockerResetRecordKeptUntilRestarted: a reset is finished only
// once the services that mount bundle files restarted.
func TestUpgradeDockerResetRecordKeptUntilRestarted(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, withPending(envAt("0.1.20"), "0.1.20"), nil)
	f := newFakeCompose(t)
	f.live = liveJSONRow
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		if slices.Contains(args, "restart") {
			return "boom", errors.New("exit status 1")
		}
		if args[len(args)-1] == clickhouseEventsPropertiesType {
			return f.live, nil
		}
		return "", nil
	})
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || readEnvVar(p.env, resetPendingVar) != "0.1.20" || !strings.Contains(out, resetPendingVar+" stays in .env") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestUpgradeDockerPendingResetNeedsAReleaseTarget: only a numbered release
// from the reset on, or the edge channel, finishes a recorded reset; anything
// else is refused before Docker is asked anything.
func TestUpgradeDockerPendingResetNeedsAReleaseTarget(t *testing.T) {
	for _, tag := range []string{"main", "sha-881359f", "0.1.17"} {
		p := newUpgradeProject(t, compose253, jsonSchemaSQL, withPending(envAt("0.1.16"), "0.1.20"), nil)
		f := newFakeCompose(t)
		before := p.snapshot(t)
		out, err := upgradeIn(t, p.dir, tag, bundleAuto)
		if err == nil || !strings.Contains(out, "Finish it first: origamy upgrade --version 0.1.20") || len(f.calls) != 0 {
			t.Fatalf("%s: %v, calls %v\n%s", tag, err, f.calls, out)
		}
		if after := p.snapshot(t); !mapsEqual(before, after) {
			t.Fatalf("%s: the project was touched", tag)
		}
	}
	// The edge channel can.
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, withPending(envAt("0.1.16"), "0.1.20"), nil)
	f := newFakeCompose(t)
	f.live = liveJSONRow
	chdir(t, p.dir)
	var err error
	out := captureStdout(t, func() { err = upgradeDocker("", "edge", false, false, false, true, bundleAuto) })
	if err != nil || envHasKey(p.env, resetPendingVar) || readEnvVar(p.env, "DP_IMAGE_TAG") != dockerEdgeTag {
		t.Fatalf("edge: %v\n%s", err, out)
	}
}

// TestUpgradeDockerEffectiveClickHouse: the ClickHouse the reset needs is the
// one the project's effective compose configuration runs, override files and
// COMPOSE_FILE included; one whose version the tag does not tell is refused.
func TestUpgradeDockerEffectiveClickHouse(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("COMPOSE_PATH_SEPARATOR", "")
	pin := func(image string) string { return "services:\n  clickhouse:\n    image: " + image + "\n" }
	for _, tc := range []struct {
		name, file, body string
		env              string // extra .env line
		want             string // "" = the reset runs
	}{
		{"an override pins 24.8", "docker-compose.override.yml", pin("clickhouse/clickhouse-server:24.8"), "",
			"docker-compose.override.yml, which compose loads with docker-compose.yml here, changes the clickhouse service: it runs clickhouse/clickhouse-server:24.8"},
		{"an override pins latest", "compose.override.yml", pin("clickhouse/clickhouse-server:latest"), "",
			"compose.override.yml, which compose loads with docker-compose.yml here, changes the clickhouse service: it runs clickhouse/clickhouse-server:latest, whose ClickHouse version the tag does not tell"},
		{"COMPOSE_FILE adds a file that pins 24.8", "extra.yml", pin("docker.io/clickhouse/clickhouse-server:24.8@sha256:0000000000000000000000000000000000000000000000000000000000000000"), "COMPOSE_FILE=docker-compose.yml:extra.yml\n",
			"extra.yml, which compose loads with docker-compose.yml here"},
		{"an override pins a newer one", "docker-compose.override.yml", pin("clickhouse/clickhouse-server:25.8@sha256:0000000000000000000000000000000000000000000000000000000000000000"), "", ""},
		// The served compose file runs 25.3; an override that does not touch
		// ClickHouse is no problem.
		{"an override of another service", "docker-compose.override.yml", "services:\n  caddy:\n    image: caddy:2\n", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose248, stringSchemaSQL, func(base string) string { return envAt("0.1.16")(base) + tc.env }, nil)
			writeFile(t, filepath.Join(p.dir, tc.file), tc.body)
			f := newFakeCompose(t)
			f.live = liveStringRow
			before := p.snapshot(t)
			out, err := upgradeAsking(t, p.dir, "0.1.20", bundleAuto, false, "yes\n")
			if tc.want == "" {
				if err != nil || callIndex(f.calls, "DROP") < 0 {
					t.Fatalf("%v\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, asked) {
				t.Fatalf("%v\n%s", err, out)
			}
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
}

// TestUpgradeDockerSchemaFetchFailure: a control plane the schema cannot be
// fetched from is a network matter for the operator, not a cluster's.
func TestUpgradeDockerSchemaFetchFailure(t *testing.T) {
	p := newUpgradeProject(t, compose253, stringSchemaSQL, func(string) string { return envAt("0.1.17")("http://127.0.0.1:1") }, nil)
	f := newFakeCompose(t)
	f.live = liveStringRow
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "Could not download clickhouse-init.sql from your control plane (http://127.0.0.1:1)") ||
		!strings.Contains(out, "Check CONTROL_PLANE_HTTP_URL in .env") || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20` (nothing has been changed)") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, bad := range []string{"Kubernetes", "kubectl", "enrollment token"} {
		if strings.Contains(out, bad) {
			t.Errorf("output mentions %q:\n%s", bad, out)
		}
	}
	if after := p.snapshot(t); !mapsEqual(before, after) || callIndex(f.calls, "pull") >= 0 {
		t.Fatalf("the project was touched")
	}
}

// TestUpgradeDockerResetWritesSchemaSafely: across the reset without a
// refresh (--keep-bundle here), the schema written to clickhouse-init.sql
// backs up what was there, never goes through a link, and the box says what
// changed.
func TestUpgradeDockerResetWritesSchemaSafely(t *testing.T) {
	edited := stringSchemaSQL + "\n-- mine\n"
	p := newUpgradeProject(t, compose253, edited, envAt("0.1.17"), nil)
	f := newFakeCompose(t)
	f.live = liveStringRow
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleKeep)
	if err != nil || callIndex(f.calls, "DROP") < 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	if p.read(t, "clickhouse-init.sql") != jsonSchemaSQL || p.read(t, "clickhouse-init.sql.bak-0.1.17") != edited {
		t.Fatal("the edited schema was not backed up before the new one was written")
	}
	if !strings.Contains(out, "Replaced your edited clickhouse-init.sql with your control plane's current schema for the storage reset; yours is backed up as clickhouse-init.sql.bak-0.1.17") ||
		!strings.Contains(out, "Bundle files not refreshed (--keep-bundle); the storage reset still writes its schema to clickhouse-init.sql.") ||
		!strings.Contains(out, "Bundle     not refreshed (--keep-bundle)") || !strings.Contains(out, "replaced your edited clickhouse-init.sql with your control plane's current schema") ||
		strings.Contains(out, "left as-is") {
		t.Fatalf("output:\n%s", out)
	}
	// The other bundle files were left alone.
	if p.read(t, "docker-compose.yml") != compose253 {
		t.Fatal("--keep-bundle replaced the compose file")
	}

	// A link is refused before anything is touched or asked, the file it
	// points at untouched.
	p = newUpgradeProject(t, compose253, stringSchemaSQL, envAt("0.1.17"), nil)
	target := filepath.Join(t.TempDir(), "my-init.sql")
	writeFile(t, target, edited)
	if err := os.Remove(filepath.Join(p.dir, "clickhouse-init.sql")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(p.dir, "clickhouse-init.sql")); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []bundleMode{bundleKeep, bundleAuto} {
		f = newFakeCompose(t)
		f.live = liveStringRow
		before := p.snapshot(t)
		out, err = upgradeAsking(t, p.dir, "0.1.20", mode, false, "yes\n")
		if err == nil || !strings.Contains(out, "is a symbolic link (to "+target+")") || strings.Contains(out, asked) {
			t.Fatalf("mode %d: %v\n%s", mode, err, out)
		}
		if readFile(t, target) != edited || callIndex(f.calls, "pull") >= 0 || callIndex(f.calls, "DROP") >= 0 {
			t.Fatalf("mode %d: written through the link, or ran on", mode)
		}
		if after := p.snapshot(t); !mapsEqual(before, after) {
			t.Fatalf("mode %d: the project was touched:\nbefore %v\nafter  %v", mode, before, after)
		}
	}
}

// TestUpgradeDockerRefusesBeforeAsking: every refusal comes before the
// question that agrees to discard the event history.
func TestUpgradeDockerRefusesBeforeAsking(t *testing.T) {
	for _, tc := range []struct {
		name, compose string
		served        map[string]string
		requireSigned bool
		want          string
	}{
		{"strict signing without a key", compose248, nil, true, "config signing"},
		{"the control plane serves the pre-reset schema", compose253, map[string]string{"VERSION": "", "clickhouse-init.sql": stringSchemaSQL}, false, "serves the pre-reset ClickHouse schema"},
		{"the served compose runs the old ClickHouse", compose248, map[string]string{"docker-compose.yml": compose248 + "# served\n"}, false, "does not run it"},
		{"the project's compose runs the old ClickHouse", compose248, map[string]string{"VERSION": ""}, false, "does not run it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, tc.compose, stringSchemaSQL, envAt("0.1.16"), tc.served)
			f := newFakeCompose(t)
			f.live = liveStringRow
			out, err := upgradeAsking(t, p.dir, "0.1.20", bundleAuto, tc.requireSigned, "yes\n")
			if err == nil || !strings.Contains(strings.ToLower(out), strings.ToLower(tc.want)) || strings.Contains(out, asked) {
				t.Fatalf("%v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(p.dir, ".env")); err != nil || envHasKey(p.env, resetPendingVar) || len(removeStaleBundleDirs(p.dir)) != 0 {
				t.Fatal("a refusal left something behind")
			}
		})
	}
	// Declining leaves no download directory either.
	p := newUpgradeProject(t, compose248, stringSchemaSQL, envAt("0.1.16"), nil)
	f := newFakeCompose(t)
	f.live = liveStringRow
	before := p.snapshot(t)
	if out, err := upgradeAsking(t, p.dir, "0.1.20", bundleAuto, false, "no\n"); err == nil || !strings.Contains(out, asked) || !strings.Contains(out, "Cancelled.") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("declining touched the project:\nbefore %v\nafter  %v", before, after)
	}
}

// TestUpgradeDockerProbeTimeout: a ClickHouse that does not answer the probe
// in time is started (a no-op when it runs) and asked again; when it still
// does not answer, the upgrade is refused rather than decided from the files
// and tags, and nothing is touched.
func TestUpgradeDockerProbeTimeout(t *testing.T) {
	// It answers the second time: the upgrade goes on, on what it says.
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.20"), nil)
	f := newFakeCompose(t)
	f.live, f.probeHangs = liveStringRow, 1
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || callIndex(f.calls, "DROP") < 0 || !strings.Contains(out, "still holds the pre-reset events table") {
		t.Fatalf("%v\n%s", err, out)
	}

	for name, setup := range map[string]func(*fakeCompose){
		"the probe never answers":      func(f *fakeCompose) { f.live, f.probeFails = liveStringRow, true },
		"ClickHouse never answers":     func(f *fakeCompose) { f.live, f.stopped, f.pingFails = liveStringRow, true, -1 },
		"ClickHouse cannot be started": func(f *fakeCompose) { f.live, f.stopped, f.startFails = liveStringRow, true, true },
	} {
		t.Run(name, func(t *testing.T) {
			for _, schema := range []string{jsonSchemaSQL, stringSchemaSQL} {
				p := newUpgradeProject(t, compose253, schema, envAt("0.1.20"), nil)
				f := newFakeCompose(t)
				setup(f)
				before := p.snapshot(t)
				out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
				if err == nil || !strings.Contains(out, "ClickHouse could not be asked which events table it holds") || strings.Contains(out, "Upgraded") {
					t.Fatalf("%v\n%s", err, out)
				}
				for _, c := range f.calls {
					if strings.Contains(c, " pull") || strings.Contains(c, "DROP") || strings.Contains(c, "clickhouse-init") || strings.Contains(c, "--remove-orphans") {
						t.Fatalf("ran %q", c)
					}
				}
				if callIndex(f.calls, "DP_IMAGE_TAG=0.1.20 docker compose --project-directory "+p.dir+" --env-file "+p.env+" up -d clickhouse") < 0 {
					t.Fatalf("ClickHouse was not started to be asked:\n%s", strings.Join(f.calls, "\n"))
				}
				if after := p.snapshot(t); !mapsEqual(before, after) {
					t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
				}
			}
		})
	}
}

// TestUpgradeDockerStoppedClickHouseIsAsked: an in-place deploy by an earlier
// CLI put the 0.1.20 bundle over a :main project before it was refused, and
// ClickHouse was then stopped. Neither the files (the new schema) nor the tag
// (main) say this crosses the reset; ClickHouse, started and asked, does.
func TestUpgradeDockerStoppedClickHouseIsAsked(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, v018Env, nil)
	f := newFakeCompose(t)
	f.live, f.stopped = liveStringRow, true
	out, err := upgradeIn(t, p.dir, "", bundleAuto)
	if err != nil || callIndex(f.calls, "DROP TABLE") < 0 || strings.Contains(out, notTouched) || !strings.Contains(out, "Event history was discarded") {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.live != liveJSONRow || readEnvVar(p.env, "DP_IMAGE_TAG") != helmVersion || envHasKey(p.env, resetPendingVar) {
		t.Fatalf("after: table %q, .env:\n%s", f.live, readFile(t, p.env))
	}
}

// TestUpgradeDockerCreatesAMissingTable: a released CLI dropped the pre-reset
// table and its schema failed silently on ClickHouse 24.8, leaving no events
// table and the tag already moved on. The upgrade creates the table with the
// new schema (nothing to drop, so nothing is asked) and checks it.
func TestUpgradeDockerCreatesAMissingTable(t *testing.T) {
	for _, tc := range []struct {
		name, compose, schema, installed string
		mode                             bundleMode
	}{
		{"0.1.20 with the old bundle", compose248, jsonSchemaSQL, "0.1.20", bundleAuto},
		{"0.1.16 with the old bundle", compose248, stringSchemaSQL, "0.1.16", bundleAuto},
		{"the new bundle, --keep-bundle", compose253, jsonSchemaSQL, "0.1.20", bundleKeep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, tc.compose, tc.schema, envAt(tc.installed), nil)
			f := newFakeCompose(t)
			out, err := upgradeAsking(t, p.dir, "0.1.20", tc.mode, false, "")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if callIndex(f.calls, "DROP") >= 0 || callIndex(f.calls, "run --rm --no-deps clickhouse-init") < 0 || strings.Contains(out, asked) {
				t.Fatalf("calls:\n%s\n%s", strings.Join(f.calls, "\n"), out)
			}
			if f.live != liveJSONRow || !strings.Contains(out, "ClickHouse has no events table") || !strings.Contains(out, "the events table was created with the new schema") || strings.Contains(out, notTouched) {
				t.Fatalf("output:\n%s", out)
			}
			if readEnvVar(p.env, "DP_IMAGE_TAG") != "0.1.20" || envHasKey(p.env, resetPendingVar) {
				t.Fatalf(".env:\n%s", readFile(t, p.env))
			}
		})
	}

	// The operator's edited compose file pins ClickHouse 24.8 and the refresh
	// keeps it: the new schema cannot be created there, so the upgrade is
	// refused before anything is touched, instead of reporting success on a
	// plane with no events table.
	edited := compose248 + "# local edit\n"
	p := newUpgradeProject(t, edited, jsonSchemaSQL, envAt("0.1.20"), nil)
	f := newFakeCompose(t)
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "your edited docker-compose.yml, which the refresh keeps, does not run it") || strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) || callIndex(f.calls, " pull") >= 0 || callIndex(f.calls, "clickhouse-init") >= 0 {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v\ncalls %v", before, after, f.calls)
	}
}

// TestUpgradeDockerChecksTheTableAfterUp is cardinal rule 2: whatever the run
// did, it succeeds only when, after the last `up -d`, ClickHouse holds the
// events table the release writes. A missing or pre-reset one under a release
// that writes the new schema is recorded as a reset for the next run.
func TestUpgradeDockerChecksTheTableAfterUp(t *testing.T) {
	for _, tc := range []struct {
		name, live, target string
		onUp               func(*fakeCompose)
		ok                 bool
		says               string
		pending            string
	}{
		{"the new table stays", liveJSONRow, "0.1.20", nil, true, notTouched, ""},
		{"gone after up", liveJSONRow, "0.1.20", func(f *fakeCompose) { f.live = "" }, false, "ClickHouse holds no events table", "0.1.20"},
		{"the pre-reset one after up", liveJSONRow, "0.1.20", func(f *fakeCompose) { f.live = liveStringRow }, false, "which the 0.1.20 images cannot write", "0.1.20"},
		{"another type after up", liveJSONRow, "0.1.20", func(f *fakeCompose) { f.live = "Nullable(String)\t" + eventsColumnMark + "\n" }, false, "neither the pre-reset (String) nor the new (JSON) schema", ""},
		{"no answer after up", liveJSONRow, "0.1.20", func(f *fakeCompose) { f.probeFails = true }, false, "did not say which events table it holds", ""},
		// A release before the reset with no table: the init `up -d` runs
		// creates it, or the run fails.
		{"before the reset, created by up", "", "0.1.17", func(f *fakeCompose) { f.live = liveStringRow }, true, notTouched, ""},
		{"before the reset, not created", "", "0.1.17", nil, false, "ClickHouse holds no events table", ""},
		// The edge channel writes the new schema; :main is not known to write
		// either, so either is taken, but no table is not.
		{"edge, the new table", liveJSONRow, dockerEdgeTag, nil, true, notTouched, ""},
		{"main, the pre-reset table", liveStringRow, "main", nil, true, "which one they write is not known", ""},
		{"main, no table", "", "main", nil, false, "ClickHouse holds no events table", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.20"), map[string]string{"VERSION": ""})
			if releaseSchemaGeneration(tc.target) != "json" {
				p = newUpgradeProject(t, compose248, stringSchemaSQL, envAt("0.1.16"), map[string]string{"VERSION": ""})
			}
			f := newFakeCompose(t)
			f.live, f.onUp = tc.live, tc.onUp
			chdir(t, p.dir)
			var err error
			out := captureStdout(t, func() {
				if tc.target == dockerEdgeTag {
					err = upgradeDocker("", "edge", false, false, false, true, bundleAuto)
				} else {
					err = upgradeDocker(tc.target, "stable", false, false, false, true, bundleAuto)
				}
			})
			if (err == nil) != tc.ok || !strings.Contains(out, tc.says) || strings.Contains(out, "Upgraded") != tc.ok {
				t.Fatalf("%v\n%s", err, out)
			}
			if !tc.ok && (strings.Contains(out, notTouched) || strings.Contains(out, "were kept")) {
				t.Fatalf("a failed check still reported success:\n%s", out)
			}
			if got := readEnvVar(p.env, resetPendingVar); got != tc.pending {
				t.Fatalf("%s = %q", resetPendingVar, got)
			}
			// Checked after everything that restarts ClickHouse.
			if all, check := callIndex(f.calls, "up -d --remove-orphans"), len(f.calls)-1; all < 0 || !strings.Contains(strings.Join(f.calls[all:], "\n"), clickhouseEventsPropertiesType) || check < all {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
		})
	}

	// The recorded reset is finished by the next run, which creates the table.
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.20"), map[string]string{"VERSION": ""})
	f := newFakeCompose(t)
	f.live, f.onUp = liveJSONRow, func(f *fakeCompose) { f.live = "" }
	if _, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto); err == nil || readEnvVar(p.env, resetPendingVar) != "0.1.20" {
		t.Fatal("the first run must fail and record the reset")
	}
	f.onUp = nil
	n := len(f.calls)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || callIndex(f.since(n), "DROP") >= 0 || callIndex(f.since(n), "run --rm --no-deps clickhouse-init") < 0 || f.live != liveJSONRow || envHasKey(p.env, resetPendingVar) {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
}

// TestUpgradeDockerKeepsTheNewTableFromAnOlderRelease: ClickHouse holding the
// new table is never moved to a release that cannot write it, whatever the
// project's clickhouse-init.sql says (the operator put the pre-reset one back).
func TestUpgradeDockerKeepsTheNewTableFromAnOlderRelease(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		served       map[string]string
		mode         bundleMode
	}{
		{"the pre-reset file put back", stringSchemaSQL, map[string]string{"VERSION": "0.1.20\n"}, bundleAuto},
		{"the pre-reset file put back, --keep-bundle", stringSchemaSQL, nil, bundleKeep},
		{"the new file", jsonSchemaSQL, nil, bundleAuto},
		// The control plane serves the 0.1.17 bundle, which would refresh the
		// files to it.
		{"the control plane serves 0.1.17", jsonSchemaSQL, map[string]string{"VERSION": "0.1.17\n", "docker-compose.yml": compose248, "clickhouse-init.sql": stringSchemaSQL}, bundleAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, tc.schema, envAt("0.1.20"), tc.served)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			before := p.snapshot(t)
			out, err := upgradeIn(t, p.dir, "0.1.17", tc.mode)
			if err == nil || !strings.Contains(out, "holds the new events table (JSON payload columns), which release 0.1.17 cannot write") || strings.Contains(out, "Upgraded") {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, c := range f.calls {
				if strings.Contains(c, " pull") || strings.Contains(c, " up ") || strings.Contains(c, "DROP") {
					t.Fatalf("ran %q", c)
				}
			}
			if after := p.snapshot(t); !mapsEqual(before, after) {
				t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestUpgradeDockerServedPreResetSchema: a control plane whose bundle for the
// target release carries the pre-reset schema is refused, also when that file
// is already the project's (nothing to replace), as before the refresh.
func TestUpgradeDockerServedPreResetSchema(t *testing.T) {
	p := newUpgradeProject(t, compose253, stringSchemaSQL, envAt("0.1.19"), map[string]string{"clickhouse-init.sql": stringSchemaSQL})
	f := newFakeCompose(t)
	f.live = liveJSONRow
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "Your control plane serves the pre-0.1.18 ClickHouse schema (String payload columns), which data-plane release 0.1.20 no longer writes") ||
		!strings.Contains(out, "Update the control plane first") || strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) || callIndex(f.calls, " pull") >= 0 {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}
}

// TestUpgradeDockerRefusesAnotherType: an events table of neither generation
// is refused whether or not the run would reset, before anything is touched.
func TestUpgradeDockerRefusesAnotherType(t *testing.T) {
	for _, tc := range []struct {
		name, installed, target string
		pending                 bool
	}{
		{"to a release that writes the new schema", "0.1.16", "0.1.20", false},
		{"to a release before the reset", "0.1.16", "0.1.17", false},
		{"same release", "0.1.20", "0.1.20", false},
		{"an unfinished reset", "0.1.16", "0.1.20", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := envAt(tc.installed)
			if tc.pending {
				env = withPending(env, tc.target)
			}
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, env, nil)
			f := newFakeCompose(t)
			f.live = "Map(String, String)\t" + eventsColumnMark + "\n"
			before := p.snapshot(t)
			out, err := upgradeAsking(t, p.dir, tc.target, bundleAuto, false, "yes\n")
			if err == nil || !strings.Contains(out, "payload columns of type Map(String, String), neither the pre-reset (String) nor the new (JSON) schema") || strings.Contains(out, asked) {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, c := range f.calls {
				if strings.Contains(c, " pull") || strings.Contains(c, " up ") || strings.Contains(c, "DROP") {
					t.Fatalf("ran %q", c)
				}
			}
			if after := p.snapshot(t); !mapsEqual(before, after) {
				t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestUpgradeDockerUnknownServedSchema: a reset whose schema is of neither
// generation is refused before anything is dropped, whether it comes with
// the refresh or is fetched.
func TestUpgradeDockerUnknownServedSchema(t *testing.T) {
	neither := "CREATE TABLE IF NOT EXISTS events.events (id UInt64) ENGINE = MergeTree ORDER BY id;\n"
	for name, served := range map[string]map[string]string{
		"with the refresh": {"clickhouse-init.sql": neither},
		"fetched":          {"VERSION": "", "clickhouse-init.sql": neither},
	} {
		t.Run(name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, stringSchemaSQL, envAt("0.1.17"), served)
			f := newFakeCompose(t)
			f.live = liveStringRow
			before := p.snapshot(t)
			out, err := upgradeAsking(t, p.dir, "0.1.20", bundleAuto, false, "yes\n")
			if err == nil || !strings.Contains(out, "has neither the pre-reset (String) nor the new (JSON) payload columns") || strings.Contains(out, asked) {
				t.Fatalf("%v\n%s", err, out)
			}
			if after := p.snapshot(t); !mapsEqual(before, after) || callIndex(f.calls, "DROP") >= 0 || callIndex(f.calls, " pull") >= 0 {
				t.Fatalf("the project was touched, or dropped:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestUpgradeDockerLocked: a run that finds another one working on the
// project changes nothing and asks Docker nothing.
func TestUpgradeDockerLocked(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	host, _ := os.Hostname()
	writeFile(t, filepath.Join(p.dir, projectLockFile), projectLock{pid: os.Getppid(), host: host, started: "2026-10-17T09:00:00Z", command: "origamy deploy"}.String())
	f := newFakeCompose(t)
	f.live = liveJSONRow
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "Another origamy run is working on this data plane (origamy deploy, pid ") || len(f.calls) != 0 {
		t.Fatalf("%v, calls %v\n%s", err, f.calls, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}
	// Once that run is over the lock is its own to remove; a run that went
	// away without removing it (a dead pid here) is taken over, and the
	// upgrade removes its own lock again.
	writeFile(t, filepath.Join(p.dir, projectLockFile), projectLock{pid: deadPID(t), host: host, started: "2026-10-17T09:00:00Z", command: "origamy upgrade"}.String())
	if out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto); err != nil || !strings.Contains(out, "Removed the lock an earlier run left") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(p.dir, projectLockFile)); !os.IsNotExist(err) {
		t.Fatalf("the upgrade left its lock behind: %v", err)
	}
}
