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
// upgrade, with a ClickHouse that keeps state: live is what the events-table
// probe answers ("" = no table); a DROP empties it, and a clickhouse-init run
// that succeeds leaves afterInit there (the new table, unless a test says the
// schema silently failed). Everything else succeeds, except: ClickHouse
// answers nothing while stopped (until an `up`), the first pingFails pings
// fail (all when < 0), the probe cannot be asked while probeFails (nor after
// an init while checkFails), the first initFails clickhouse-init runs fail,
// and a pull fails while failPull is set. `docker compose config --format json` renders
// the clickhouse image of the -f files given (the last that sets it wins),
// after a compose warning, like compose does.
type fakeCompose struct {
	calls                           []string
	live, afterInit                 string
	pingFails, initFails            int
	stopped, probeFails, checkFails bool
	failPull                        bool
	pings, inits                    int
}

func newFakeCompose(t *testing.T) *fakeCompose {
	t.Helper()
	f := &fakeCompose{afterInit: liveJSONRow}
	prevTimeout, prevPoll := clickhouseReadyTimeout, clickhouseReadyPoll
	clickhouseReadyTimeout, clickhouseReadyPoll = 100*time.Millisecond, time.Millisecond
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		f.calls = append(f.calls, strings.Join(append(append(append([]string{}, env...), name), args...), " "))
		switch last := args[len(args)-1]; {
		case slices.Contains(args, "up"):
			f.stopped = false
		case f.stopped && slices.Contains(args, "exec"):
			return "service \"clickhouse\" is not running", errors.New("exit status 1")
		case last == clickhouseEventsPropertiesType:
			if f.probeFails || (f.checkFails && f.inits > 0) {
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
		}
		return "", nil
	})
	t.Cleanup(func() { clickhouseReadyTimeout, clickhouseReadyPoll = prevTimeout, prevPoll })
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

func TestResetDockerEventsStartsClickHouseFirst(t *testing.T) {
	f := newFakeCompose(t)
	f.live, f.pingFails = liveStringRow, 2
	var outcome resetOutcome
	var err error
	out := captureStdout(t, func() { outcome, err = resetDockerEvents("/dp", "/dp/.env", "0.1.20", true) })
	if err != nil || outcome != resetDropped {
		t.Fatalf("%v, %v\n%s", outcome, err, out)
	}
	// ClickHouse comes up on the compose file's image (with the target tag)
	// and answers BEFORE anything else; it is asked which table it holds,
	// the old one dropped, the schema applied on the new server rather than
	// the container still running, and the result checked.
	dc := "DP_IMAGE_TAG=0.1.20 docker compose --project-directory /dp --env-file /dp/.env "
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
	var err error
	captureStdout(t, func() { _, err = resetDockerEvents("/dp", "/dp/.env", "0.1.20", true) })
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
// events table is dropped only when ClickHouse says it is the String one.
func TestResetDockerEventsDropsOnlyThePreResetTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		live       string
		probeFails bool
		mayDrop    bool
		want       resetOutcome
		dropped    bool
		init       bool
		failure    string // "" = the reset succeeds
	}{
		{"the pre-reset table", liveStringRow, false, true, resetDropped, true, true, ""},
		{"no events table", "", false, true, resetCreated, false, true, ""},
		{"the new table already", liveJSONRow, false, true, resetAlready, false, false, ""},
		{"the new table, bare answer", "JSON\n", false, true, resetAlready, false, false, ""},
		{"ClickHouse cannot be asked", "", true, true, resetNone, false, false, "so nothing was dropped"},
		{"another type", "Object('json')\t" + eventsColumnMark + "\n", false, true, resetNone, false, false, "neither the pre-reset (String) nor the new (JSON)"},
		// A run that did not ask accepts only the new table.
		{"not asked, the new table", liveJSONRow, false, false, resetAlready, false, false, ""},
		{"not asked, the pre-reset table", liveStringRow, false, false, resetNone, false, false, "it asks before it finishes the reset"},
		{"not asked, no table", "", false, false, resetNone, false, false, "it asks before it finishes the reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			f.live, f.probeFails = tc.live, tc.probeFails
			var got resetOutcome
			var err error
			out := captureStdout(t, func() { got, err = resetDockerEvents("/dp", "/dp/.env", "0.1.20", tc.mayDrop) })
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
			var err error
			out := captureStdout(t, func() { _, err = resetDockerEvents("/dp", "/dp/.env", "0.1.20", true) })
			if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, "Events table recreated") ||
				!strings.Contains(out, "ClickHouse schema ready.") || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20`") {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
	// The check itself cannot be asked: not done either.
	f := newFakeCompose(t)
	f.live, f.checkFails = liveStringRow, true
	var err error
	out := captureStdout(t, func() { _, err = resetDockerEvents("/dp", "/dp/.env", "0.1.20", true) })
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
	// the pre-reset bundle, ClickHouse stopped. With nothing to ask, the
	// schema file, not the tag, says this crosses the reset; the reset asks
	// ClickHouse once it is up, and drops the table it finds is the old one.
	p := newUpgradeProject(t, compose248, stringSchemaSQL, v018Env, nil)
	f := newFakeCompose(t)
	f.live, f.stopped = liveStringRow, true
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Upgrading main → 0.1.20 crosses the data plane's storage reset") || callIndex(f.calls, "DROP TABLE") < 0 {
		t.Fatalf("no reset:\n%s", out)
	}
	// The confirmation says ClickHouse could not be asked about its volume.
	if !strings.Contains(out, "what its data volume (dp_clickhouse-data) holds could not be checked") || !strings.Contains(out, "drops the table only if it is the pre-reset one") {
		t.Fatalf("the confirmation does not say ClickHouse was not asked:\n%s", out)
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
// in time counts as not asked; the decision falls back, and the reset asks
// again before it drops anything.
func TestUpgradeDockerProbeTimeout(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.20"), nil)
	f := newFakeCompose(t)
	f.live, f.probeFails = liveStringRow, true
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	// The file and tags say across: no reset, nothing dropped.
	if err != nil || callIndex(f.calls, "DROP") >= 0 || !strings.Contains(out, notTouched) {
		t.Fatalf("%v\n%s", err, out)
	}
}
