package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
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
// events-table probe answers ("" = no table); moving it aside (RENAME) puts
// its type in aside under the new name, a DROP of a table moved aside
// removes it (recorded in dropped as name:type), and a clickhouse-init run
// that succeeds leaves afterInit there (the new table, unless a test says
// the schema silently failed). Everything else succeeds, except: ClickHouse
// answers nothing while stopped (until an `up` or a `docker start`;
// startFails makes `up -d clickhouse` fail), the first pingFails pings fail
// (all when < 0), the probe cannot be asked while probeFails (or for its
// first probeHangs answers, nor after an init while checkFails), the first
// initFails clickhouse-init runs fail, moving aside fails while renameFails,
// dropping what was moved aside while dropFails, listing it while
// asideFails, and a pull fails while failPull is set. A compose call fails
// as compose does when a compose file it loads holds brokenCompose. onUp
// runs at every `up` of the whole project (compose reruns the init then).
//
// Containers are modelled too, one per service: every `up` of the whole
// project creates (or recreates) the containers of the services its
// configuration runs (renderComposeConfig: the files, DP_IMAGE_TAG and the
// profiles that are on) and, with --remove-orphans, removes those of
// services no profile of the compose file has; the containers of a profile
// that is off stay, as compose leaves them, until `rm` removes them.
// `up -d clickhouse` (re)creates ClickHouse's alone. runTag, when a test pins
// it, is the tag the data-plane containers run whatever compose was given,
// and chImage the image ClickHouse runs. `docker compose ps` lists the
// containers (without their image when psNoImage, as Compose before 2.15
// does), `docker inspect` says what each was created from, and `docker
// image inspect` an image's ID (ids, else one of its reference) and version
// label (versions); missing images are not on this host until `docker pull`
// (pullFails fails it). psFails fails `docker compose ps`, and server is what
// ClickHouse says its version is ("" = no answer). ranTag is the DP_IMAGE_TAG of the last `up` of the
// whole project, ranCH ClickHouse's image at its last `up`. `docker ps`
// finds ClickHouse's container (container, "" = none) by its labels, and
// lists owners (rows of project and working directory) for composeOwners.
type fakeCompose struct {
	calls                              []string
	live, afterInit                    string
	aside                              map[string]string
	dropped                            []string
	pingFails, initFails               int
	probeHangs                         int
	stopped, probeFails, checkFails    bool
	startFails, failPull               bool
	renameFails, dropFails, asideFails bool
	onUp                               func(*fakeCompose)
	pings, inits, probes               int
	runTag, chImage, container         string
	ranTag, ranCH                      string
	running                            map[string]string // service → image
	removed                            []string
	psNoImage, psFails, pullFails      bool
	server                             string
	ids, versions                      map[string]string
	missing                            map[string]bool
	owners                             string
}

// brokenCompose makes a compose file that does not parse.
const brokenCompose = "\nservices:\n  gateway: [\n"

var reTestRename = regexp.MustCompile(`RENAME TABLE events\.events TO events\.(\w+)`)
var reTestDropAside = regexp.MustCompile(`DROP TABLE events\.(` + eventsAsidePrefix + `\w+)`)

// rowType is the type a probe row names.
func rowType(row string) string {
	typ, _, _ := strings.Cut(strings.TrimSpace(row), "\t")
	return typ
}

// composeFiles are the compose files a fake compose call loads: its -f
// files, else, as compose does, the project's docker-compose.yml and an
// override file next to it.
func composeFiles(args []string) []string {
	var files []string
	dir := "."
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "-f":
			files = append(files, args[i+1])
		case "--project-directory":
			dir = args[i+1]
		}
	}
	if len(files) == 0 {
		files = composeFileArgs(dir, "", filepath.Join(dir, "docker-compose.yml"))
		files = slices.DeleteFunc(files, func(a string) bool { return a == "-f" })
	}
	return files
}

// labels are a fake image's configuration as `{{json .Config}}` prints it,
// with its labels (versions; none like most images).
func (f *fakeCompose) labels(ref string) string {
	if v, ok := f.versions[ref]; ok {
		return `{"Cmd":["./portal-agent"],"Labels":{"org.opencontainers.image.version":"` + v + `"}}`
	}
	return `{"Cmd":["./portal-agent"]}`
}

// testContainerID is the ID of a fake service's container.
func testContainerID(svc string) string { return sha256Hex([]byte(svc))[:12] }

// testImageID is the ID of a fake image: ids', else one of its reference.
func (f *fakeCompose) imageID(ref string) string {
	if id, ok := f.ids[ref]; ok {
		return id
	}
	return "sha256:" + sha256Hex([]byte(ref))
}

// up creates the containers of the services the configuration of a compose
// call runs (only those named, when it names any).
func (f *fakeCompose) up(env, args []string, named ...string) {
	svcs := renderServices(env, args)
	for name, s := range svcs {
		if !s.on || (len(named) > 0 && !slices.Contains(named, name)) {
			continue
		}
		image := s.image
		switch {
		case name == "clickhouse":
			f.ranCH = image
			image = cmp.Or(f.chImage, image)
		case f.runTag != "" && strings.Contains(image, dataPlaneImageRepo):
			image = imageRepo(image) + ":" + f.runTag
		}
		f.running[name] = image
	}
	if slices.Contains(args, "--remove-orphans") {
		for name := range f.running {
			if _, known := svcs[name]; !known {
				delete(f.running, name)
			}
		}
	}
}

func newFakeCompose(t *testing.T) *fakeCompose {
	t.Helper()
	f := &fakeCompose{afterInit: liveJSONRow, aside: map[string]string{}, running: map[string]string{},
		ids: map[string]string{}, versions: map[string]string{}, missing: map[string]bool{}}
	prevTimeout, prevPoll := clickhouseReadyTimeout, clickhouseReadyPoll
	clickhouseReadyTimeout, clickhouseReadyPoll = 100*time.Millisecond, time.Millisecond
	prevSettle, prevWait, prevPoll2 := composeSettle, composeWait, composePoll
	composeSettle, composeWait, composePoll = 0, 100*time.Millisecond, time.Millisecond
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		f.calls = append(f.calls, strings.Join(append(append(append([]string{}, env...), name), args...), " "))
		if args[0] == "compose" {
			for _, file := range composeFiles(args) {
				if b, err := os.ReadFile(file); err == nil && strings.Contains(string(b), brokenCompose) {
					return "yaml: line 3: did not find expected node content", errors.New("exit status 15")
				}
			}
		}
		switch last := args[len(args)-1]; {
		case args[0] == "ps" && slices.Contains(args, "label=com.docker.compose.project"):
			return f.owners, nil
		case args[0] == "ps":
			if f.container == "" {
				return "", nil
			}
			return f.container + " " + map[bool]string{true: "exited", false: "running"}[f.stopped] + "\n", nil
		case args[0] == "start":
			f.stopped = false
		case args[0] == "inspect":
			var rows []string
			for _, id := range args[3:] {
				for svc, image := range f.running {
					if testContainerID(svc) == id {
						rows = append(rows, id+"\t"+image+"\t"+f.imageID(image)+"\t"+f.labels(image))
					}
				}
			}
			return strings.Join(rows, "\n"), nil
		case args[0] == "image" && args[1] == "inspect":
			if f.missing[last] {
				return "error: no such image: " + last, errors.New("exit status 1")
			}
			return f.imageID(last) + "\t" + f.labels(last) + "\n", nil
		case args[0] == "pull":
			if f.pullFails {
				return "Error response from daemon: manifest unknown", errors.New("exit status 1")
			}
			delete(f.missing, last)
		case slices.Contains(args, "up"):
			if last == "clickhouse" && f.startFails {
				return "Error response from daemon: no such image", errors.New("exit status 1")
			}
			f.stopped = false
			if last == "--remove-orphans" {
				f.up(env, args)
				for _, e := range env {
					if v, ok := strings.CutPrefix(e, "DP_IMAGE_TAG="); ok {
						f.ranTag = v
					}
				}
				if f.onUp != nil {
					f.onUp(f)
				}
			} else {
				f.up(env, args, last)
			}
		case slices.Contains(args, "rm"):
			for _, a := range args[slices.Index(args, "rm")+1:] {
				if !strings.HasPrefix(a, "-") {
					delete(f.running, a)
					f.removed = append(f.removed, a)
				}
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
		case last == clickhouseAsideTables:
			if f.asideFails {
				return "", errors.New("no answer within 20s")
			}
			var rows []string
			for _, n := range slices.Sorted(maps.Keys(f.aside)) {
				rows = append(rows, n+"\t"+f.aside[n]+"\t"+eventsAsideMark)
			}
			return strings.Join(rows, "\n"), nil
		case reTestRename.MatchString(last):
			if f.renameFails || f.live == "" {
				return "Code: 60. DB::Exception: Table `events`.`events` doesn't exist. (UNKNOWN_TABLE)", errors.New("exit status 60")
			}
			f.aside[reTestRename.FindStringSubmatch(last)[1]], f.live = rowType(f.live), ""
		case reTestDropAside.MatchString(last):
			n := reTestDropAside.FindStringSubmatch(last)[1]
			if _, ok := f.aside[n]; !ok || f.dropFails {
				return "Code: 359. DB::Exception: Table or Partition in events." + n + " was not dropped.", errors.New("exit status 359")
			}
			f.dropped = append(f.dropped, n+":"+f.aside[n])
			delete(f.aside, n)
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
		case slices.Contains(args, "config"):
			return renderComposeConfig(env, args)
		case last == clickhouseServerVersion:
			if f.server == "" {
				return "Code: 210. DB::NetException: Connection refused", errors.New("exit status 210")
			}
			return f.server, nil
		case slices.Contains(args, "ps") && last == "json" && f.psFails:
			return "yaml: line 3: did not find expected key", errors.New("exit status 15")
		case slices.Contains(args, "ps") && last == "json":
			var rows []string
			for _, svc := range slices.Sorted(maps.Keys(f.running)) {
				image := `,"Image":"` + f.running[svc] + `"`
				if f.psNoImage {
					image = ""
				}
				rows = append(rows, `{"ID":"`+testContainerID(svc)+`","Service":"`+svc+`"`+image+`,"State":"running","Status":"Up 5 seconds"}`)
			}
			return `time="2026-10-16T20:00:00Z" level=warning msg="The \"WRITE_KEYS\" variable is not set. Defaulting to a blank string."` + "\n" + strings.Join(rows, "\n") + "\n", nil
		}
		return "", nil
	})
	t.Cleanup(func() {
		clickhouseReadyTimeout, clickhouseReadyPoll = prevTimeout, prevPoll
		composeSettle, composeWait, composePoll = prevSettle, prevWait, prevPoll2
	})
	return f
}

// renderedService is a service as the fake compose renders it: its image
// and whether the profiles that are on run it.
type renderedService struct {
	image string
	on    bool
}

// envFileArg is the --env-file a compose call names.
func envFileArg(args []string) string {
	if i := slices.Index(args, "--env-file"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// renderServices reads the services of the compose files a fake compose call
// loads (a later file's image wins), with DP_IMAGE_TAG and COMPOSE_PROFILES
// from its environment, else its env file.
func renderServices(env, args []string) map[string]renderedService {
	lookup := func(k string) string {
		v, set := "", false
		for _, e := range env {
			if e == k {
				v, set = "", false
			} else if val, ok := strings.CutPrefix(e, k+"="); ok {
				v, set = val, true
			}
		}
		if !set {
			v = readEnvVar(envFileArg(args), k)
		}
		return v
	}
	tag, on := cmp.Or(lookup("DP_IMAGE_TAG"), "main"), strings.Split(lookup("COMPOSE_PROFILES"), ",")
	svcs := map[string]renderedService{}
	profiles := map[string][]string{}
	for _, file := range composeFiles(args) {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		svc := ""
		for _, l := range strings.Split(string(b), "\n") {
			switch {
			case l == "services:":
			case !strings.HasPrefix(l, " "):
				svc = ""
			case strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.HasSuffix(l, ":"):
				svc = strings.TrimSuffix(strings.TrimSpace(l), ":")
				if _, ok := svcs[svc]; !ok {
					svcs[svc] = renderedService{}
				}
			case svc != "" && strings.HasPrefix(l, "    image: "):
				image := strings.TrimPrefix(l, "    image: ")
				image = strings.NewReplacer("${DP_IMAGE_TAG:-main}", tag, "${DP_IMAGE_TAG}", tag).Replace(image)
				svcs[svc] = renderedService{image: image}
			case svc != "" && strings.HasPrefix(l, "    profiles: "):
				profiles[svc] = strings.Split(strings.Trim(strings.TrimPrefix(l, "    profiles: "), `[]" `), `", "`)
			}
		}
	}
	for name, s := range svcs {
		s.on = len(profiles[name]) == 0
		for _, p := range profiles[name] {
			s.on = s.on || slices.Contains(on, p)
		}
		svcs[name] = s
	}
	return svcs
}

// renderComposeConfig is the fake `docker compose config`: --services lists
// the services the profiles that are on run, --format json renders them with
// their images, after a compose warning, like compose does.
func renderComposeConfig(env, args []string) (string, error) {
	for _, file := range composeFiles(args) {
		if _, err := os.Stat(file); err != nil {
			return "open " + file + ": no such file or directory", errors.New("exit status 1")
		}
	}
	svcs := renderServices(env, args)
	names := slices.Sorted(maps.Keys(svcs))
	names = slices.DeleteFunc(names, func(n string) bool { return !svcs[n].on })
	if slices.Contains(args, "--services") {
		return strings.Join(names, "\n") + "\n", nil
	}
	var rendered []string
	for _, n := range names {
		rendered = append(rendered, fmt.Sprintf("%q: {\"image\": %q}", n, svcs[n].image))
	}
	return `time="2026-10-16T20:00:00Z" level=warning msg="The \"WRITE_KEYS\" variable is not set. Defaulting to a blank string."` + "\n" +
		`{"name": "dp", "services": {` + strings.Join(rendered, ", ") + `}, "volumes": {"clickhouse-data": {"name": "dp_clickhouse-data"}}}` + "\n", nil
}

// resetProject is a project directory whose clickhouse-init.sql is sql, for
// the reset alone.
func resetProject(t *testing.T, sql string) (dir, env string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), sql)
	return dir, filepath.Join(dir, ".env")
}

// asideIn replaces the name of a table the reset moved aside with <aside>.
func asideIn(calls []string) []string {
	re := regexp.MustCompile(eventsAsidePrefix + `[0-9]+`)
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = re.ReplaceAllString(c, "<aside>")
	}
	return out
}

// resetIn runs the storage reset alone, on a project whose clickhouse-init.sql
// is the new schema, to 0.1.20 with what the run found and whether the
// operator agreed to discard the pre-reset table.
func resetIn(t *testing.T, found string, discard bool) (resetResult, string, error) {
	t.Helper()
	dir, env := resetProject(t, jsonSchemaSQL)
	var r resetResult
	var err error
	out := captureStdout(t, func() { r, err = resetDockerEvents(dir, env, "0.1.20", found, composeEnv("0.1.20"), discard) })
	return r, out, err
}

func TestResetDockerEventsStartsClickHouseFirst(t *testing.T) {
	f := newFakeCompose(t)
	f.live, f.pingFails = liveStringRow, 2
	dir, env := resetProject(t, jsonSchemaSQL)
	var r resetResult
	var err error
	out := captureStdout(t, func() { r, err = resetDockerEvents(dir, env, "0.1.20", "string", composeEnv("0.1.20"), true) })
	if err != nil || r.outcome != resetDropped || len(r.dropped) != 1 || len(r.kept) != 0 {
		t.Fatalf("%+v, %v\n%s", r, err, out)
	}
	// ClickHouse comes up on the compose file's image (with the target tag)
	// and answers BEFORE anything else; it is asked which table it holds, the
	// old one moved aside (RENAME, never DROP of events.events), the schema
	// applied on the new server rather than the container still running, the
	// result checked, and only then the table moved aside dropped, once it is
	// seen to be the pre-reset one.
	dc := dcCall("0.1.20", dir, env)
	want := []string{
		dc + "up -d clickhouse",
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhouseEventsPropertiesType,
		dc + "exec -T clickhouse sh -c " + clickhouseAsideTables,
		dc + "exec -T clickhouse sh -c " + clickhouseMoveEventsAside("<aside>"),
		dc + "run --rm --no-deps clickhouse-init",
		dc + "exec -T clickhouse sh -c " + clickhouseEventsPropertiesType,
		dc + "exec -T clickhouse sh -c " + clickhouseAsideTables,
		dc + "exec -T clickhouse sh -c " + clickhouseDropAside("<aside>"),
	}
	if got := asideIn(f.calls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out, "Events table recreated") || !strings.HasSuffix(f.dropped[0], ":String") || len(f.aside) != 0 {
		t.Fatalf("dropped %v, aside %v\n%s", f.dropped, f.aside, out)
	}
}

func TestResetDockerEventsClickHouseNeverReady(t *testing.T) {
	f := newFakeCompose(t)
	f.live, f.pingFails = liveStringRow, -1
	if _, _, err := resetIn(t, "string", true); err == nil {
		t.Fatal("a ClickHouse that never answers must fail the reset")
	}
	for _, c := range f.calls {
		if strings.Contains(c, "RENAME") || strings.Contains(c, "DROP TABLE") || strings.Contains(c, "clickhouse-init") {
			t.Fatalf("nothing may be moved, dropped or applied before ClickHouse answers: %q", c)
		}
	}
}

// TestResetDockerEventsDropsOnlyThePreResetTable is the cardinal rule: the
// events table is moved aside only when ClickHouse says it is the String one,
// and the run decided on that table (asked to drop it); and nothing but a
// String table is ever dropped.
func TestResetDockerEventsDropsOnlyThePreResetTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		live       string
		probeFails bool
		found      string // what the run decided on
		want       resetOutcome
		moved      bool
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
			got, out, err := resetIn(t, tc.found, true)
			if (err == nil) != (tc.failure == "") || got.outcome != tc.want || !strings.Contains(out, tc.failure) {
				t.Fatalf("got %+v, %v\n%s", got, err, out)
			}
			if (callIndex(f.calls, "RENAME TABLE") >= 0) != tc.moved || (callIndex(f.calls, "clickhouse-init") >= 0) != tc.init ||
				(len(f.dropped) == 1) != tc.moved || callIndex(f.calls, clickhouseDropEvents) >= 0 {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
			for _, d := range f.dropped {
				if !strings.HasSuffix(d, ":String") {
					t.Fatalf("dropped %s", d)
				}
			}
			if tc.failure != "" && strings.Contains(out, "Events table recreated") {
				t.Fatalf("a failed reset said the table was recreated:\n%s", out)
			}
		})
	}
}

// TestResetDockerEventsMovedAside: what the reset moved aside is dropped only
// once the new table is in place and only while it is the String one; one
// that turns out to be the new table (the events table changed between the
// check and the move) is kept, with how to put it back.
func TestResetDockerEventsMovedAside(t *testing.T) {
	// The table turned into the new one between the probe and the move.
	f := newFakeCompose(t)
	f.live = liveStringRow
	stub := dockerRun
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		if reTestRename.MatchString(args[len(args)-1]) {
			f.live = liveJSONRow
		}
		return stub(env, name, args...)
	})
	r, out, err := resetIn(t, "string", true)
	if err != nil || len(r.kept) != 1 || len(r.dropped) != 0 || len(f.dropped) != 0 || f.live != liveJSONRow {
		t.Fatalf("%+v, %v, dropped %v\n%s", r, err, f.dropped, out)
	}
	if !strings.Contains(out, "has the new (JSON) payload columns") || !strings.Contains(out, "EXCHANGE TABLES events.events AND events."+r.kept[0]) ||
		!strings.Contains(resetSummary(r), "Kept in ClickHouse, not dropped") {
		t.Fatalf("output:\n%s", out)
	}

	// Dropping what was moved aside fails (too big to drop, say): kept, and
	// said how to drop it; the reset itself is done.
	f = newFakeCompose(t)
	f.live, f.dropFails = liveStringRow, true
	r, out, err = resetIn(t, "string", true)
	if err != nil || r.outcome != resetDropped || len(r.kept) != 1 || f.aside[r.kept[0]] != "String" || !strings.Contains(out, "Could not drop events."+r.kept[0]) {
		t.Fatalf("%+v, %v\n%s", r, err, out)
	}

	// What was moved aside cannot be listed after the new table is in: kept.
	f = newFakeCompose(t)
	f.live = liveStringRow
	f.onUp = nil
	stub = dockerRun
	inits := 0
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		if args[len(args)-1] == "clickhouse-init" {
			inits++
		}
		if args[len(args)-1] == clickhouseAsideTables && inits > 0 {
			return "", errors.New("no answer within 20s")
		}
		return stub(env, name, args...)
	})
	r, out, err = resetIn(t, "string", true)
	if err != nil || len(r.kept) != 1 || len(f.dropped) != 0 || !strings.Contains(out, "so it was not dropped") {
		t.Fatalf("%+v, %v\n%s", r, err, out)
	}

	// The move itself fails: nothing changed in ClickHouse.
	f = newFakeCompose(t)
	f.live, f.renameFails = liveStringRow, true
	r, out, err = resetIn(t, "string", true)
	if err == nil || r.outcome != resetNone || f.live != liveStringRow || callIndex(f.calls, "clickhouse-init") >= 0 || !strings.Contains(out, "nothing was changed in ClickHouse") {
		t.Fatalf("%+v, %v\n%s", r, err, out)
	}
}

// TestResetDockerEventsFailureAfterTheMove: a reset that fails once the old
// table is moved aside says where its data is, and the rerun creates the new
// table without moving anything again, then drops what the first run moved
// aside (the operator agreed then) once it is in place.
func TestResetDockerEventsFailureAfterTheMove(t *testing.T) {
	f := newFakeCompose(t)
	f.live, f.initFails = liveStringRow, 1
	dir, env := resetProject(t, jsonSchemaSQL)
	var err error
	out := captureStdout(t, func() { _, err = resetDockerEvents(dir, env, "0.1.20", "string", composeEnv("0.1.20"), true) })
	if err == nil || len(f.aside) != 1 || f.live != "" || len(f.dropped) != 0 {
		t.Fatalf("%v, aside %v\n%s", err, f.aside, out)
	}
	moved := slices.Collect(maps.Keys(f.aside))[0]
	if !strings.Contains(out, "The pre-reset events table is events."+moved+" now, its data still there") || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20`") {
		t.Fatalf("output:\n%s", out)
	}

	// The rerun (the record in .env says the operator agreed): no table, so
	// none to move; created, then the one moved aside dropped.
	n := len(f.calls)
	var r resetResult
	out = captureStdout(t, func() { r, err = resetDockerEvents(dir, env, "0.1.20", liveMissing, composeEnv("0.1.20"), true) })
	if err != nil || r.outcome != resetCreated || callIndex(f.since(n), "RENAME") >= 0 || len(f.dropped) != 1 || f.dropped[0] != moved+":String" || f.live != liveJSONRow {
		t.Fatalf("%+v, %v, calls %v\n%s", r, err, f.since(n), out)
	}
	if callIndex(f.since(n), "DROP TABLE") < callIndex(f.since(n), "clickhouse-init") {
		t.Fatalf("dropped before the new table was in place:\n%s", strings.Join(f.since(n), "\n"))
	}

	// One left behind with no agreement in this run or a record: kept.
	f = newFakeCompose(t)
	f.aside = map[string]string{eventsAsidePrefix + "1700000000": "String", eventsAsidePrefix + "1700000001": "JSON"}
	r, out, err = resetIn(t, liveMissing, false)
	if err != nil || len(f.dropped) != 0 || len(r.kept) != 2 || !strings.Contains(out, "Drop it yourself once you no longer need its data") || !strings.Contains(out, "To put it back") {
		t.Fatalf("%+v, %v\n%s", r, err, out)
	}
	// With the agreement, only the String one goes, never the JSON one.
	f = newFakeCompose(t)
	f.live = liveJSONRow
	f.aside = map[string]string{eventsAsidePrefix + "1700000000": "String", eventsAsidePrefix + "1700000001": "JSON"}
	r, out, err = resetIn(t, "json", true)
	if err != nil || r.outcome != resetAlready || strings.Join(f.dropped, ",") != eventsAsidePrefix+"1700000000:String" || len(r.kept) != 1 {
		t.Fatalf("%+v, %v, dropped %v\n%s", r, err, f.dropped, out)
	}

	// A name a table moved aside before has is not taken again.
	f = newFakeCompose(t)
	f.live = liveStringRow
	taken := fmt.Sprintf("%s%d", eventsAsidePrefix, time.Now().Unix())
	f.aside = map[string]string{taken: "String"}
	if _, out, err = resetIn(t, "string", true); err != nil || callIndex(f.calls, "TO events."+taken+"\"") >= 0 || len(f.dropped) != 2 {
		t.Fatalf("%v, dropped %v\n%s", err, f.dropped, out)
	}
}

// TestResetDockerEventsNeedsTheNewSchemaFile: the init service applies the
// project's clickhouse-init.sql, so nothing is moved aside or created unless
// it is the new schema.
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
			out := captureStdout(t, func() { _, err = resetDockerEvents(dir, env, "0.1.20", "string", composeEnv("0.1.20"), true) })
			if err == nil || !strings.Contains(out, "is not the new ClickHouse schema") || !strings.Contains(out, "nothing was dropped or created") {
				t.Fatalf("%v\n%s", err, out)
			}
			if callIndex(f.calls, "RENAME") >= 0 || callIndex(f.calls, "DROP") >= 0 || callIndex(f.calls, "clickhouse-init") >= 0 || f.live != liveStringRow {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
		})
	}
}

// TestResetDockerEventsChecksTheTable: the bundle's init service exits 0 even
// when a statement fails, so the reset is done only once ClickHouse reports
// the new table, and nothing moved aside is dropped before.
func TestResetDockerEventsChecksTheTable(t *testing.T) {
	for _, tc := range []struct{ name, afterInit, want string }{
		{"no table after the schema", "", "is missing afterwards"},
		{"still the old table", liveStringRow, "still the pre-reset one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			f.live, f.afterInit = liveStringRow, tc.afterInit
			_, out, err := resetIn(t, "string", true)
			if err == nil || !strings.Contains(out, tc.want) || strings.Contains(out, "Events table recreated") ||
				!strings.Contains(out, "ClickHouse schema ready.") || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20`") ||
				!strings.Contains(out, "its data still there") || len(f.dropped) != 0 {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
	// The check itself cannot be asked: not done either.
	f := newFakeCompose(t)
	f.live, f.checkFails = liveStringRow, true
	_, out, err := resetIn(t, "string", true)
	if err == nil || !strings.Contains(out, "could not be asked whether") || strings.Contains(out, "Events table recreated") || len(f.dropped) != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
}

// ── The Docker upgrade, end to end against a fake Docker and control plane ──

// Compose files of the two ClickHouse generations (the image version is all
// the reset checks), reading what the 0.1.20 bundle's services read.
const (
	compose248 = "services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:24.8\n  clickhouse-init:\n    image: clickhouse/clickhouse-server:24.8\n" +
		"  config-sync:\n    image: " + dpImage + "config-sync:${DP_IMAGE_TAG:-main}\n    environment:\n      CONTROL_PLANE_URL: \"${CONFIG_URL}\"\n      TELEMETRY_URL: \"${TELEMETRY_URL}\"\n" +
		"  portal-agent:\n    image: " + dpImage + "portal-agent:${DP_IMAGE_TAG:-main}\n"
	compose253 = "services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:25.3\n  clickhouse-init:\n    image: clickhouse/clickhouse-server:25.3\n" +
		"  config-sync:\n    image: " + dpImage + "config-sync:${DP_IMAGE_TAG:-main}\n    environment:\n      CONTROL_PLANE_URL: \"${CONFIG_URL}\"\n      TELEMETRY_URL: \"${TELEMETRY_URL}\"\n" +
		"  portal-agent:\n    image: " + dpImage + "portal-agent:${DP_IMAGE_TAG:-main}\n" +
		"  dp-postgres:\n    image: postgres:16-alpine\n    profiles: [\"full\", \"agentic\"]\n" +
		"  workflow-engine:\n    image: " + dpImage + "workflow-engine:${DP_IMAGE_TAG:-main}\n    profiles: [\"full\"]\n    environment:\n      CONTROL_PLANE_URL: \"${CONTROL_PLANE_HTTP_URL}\"\n" +
		"  orchestrator-engine:\n    image: " + dpImage + "orchestrator-engine:${DP_IMAGE_TAG:-main}\n    profiles: [\"agentic\"]\n"
)

// dpImage is where the bundle's data-plane images come from.
const dpImage = "ghcr.io/qubelylabs/" + dataPlaneImageRepo

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
		moved     bool   // the failed run got as far as moving the old table aside
		failedOut string // what the failed run says
		rerunSays string // the rerun's summary
	}{
		// The rerun finds no table (the failed run moved it aside): it
		// creates one, moves nothing, and drops the one moved aside.
		{"schema init fails after the move", func(f *fakeCompose) { f.initFails = 1 }, true, "the new schema could not be applied", "the events table was created with the new schema"},
		// The rerun finds the old table still there: it moves it aside, and
		// drops it once the new one is in.
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
			// Moved aside, never dropped, by the failed run; its data is
			// still there, and the output says where.
			if (callIndex(f.calls, "RENAME TABLE") >= 0) != tc.moved || len(f.aside) == 0 == tc.moved || callIndex(f.calls, "DROP TABLE") >= 0 || callIndex(f.calls, "--remove-orphans") >= 0 {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
			if !strings.Contains(out, tc.failedOut) || !strings.Contains(out, "rerun `origamy upgrade --version 0.1.20`") ||
				strings.Contains(out, notTouched) || strings.Contains(out, "Events table recreated") || tc.moved != strings.Contains(out, "its data still there") {
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
			up, move, init, drop, all := callIndex(calls, "up -d clickhouse"), callIndex(calls, "RENAME TABLE"), callIndex(calls, "run --rm --no-deps clickhouse-init"), callIndex(calls, "DROP TABLE"), callIndex(calls, "up -d --remove-orphans")
			if up < 0 || init < up || all < init || (move >= 0) == tc.moved || (move >= 0 && (move < up || init < move)) || drop < init || all < drop {
				t.Fatalf("rerun calls:\n%s", strings.Join(calls, "\n"))
			}
			if len(f.dropped) != 1 || !strings.HasSuffix(f.dropped[0], ":String") || len(f.aside) != 0 {
				t.Fatalf("dropped %v, aside %v", f.dropped, f.aside)
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
		f.calls[first] != dcCall("main", p.dir, p.env)+"up -d clickhouse" {
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
	fake := dockerRun
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		if slices.Contains(args, "restart") {
			return "boom", errors.New("exit status 1")
		}
		return fake(env, name, args...)
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
				if callIndex(f.calls, dcCall("0.1.20", p.dir, p.env)+"up -d clickhouse") < 0 {
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

// TestUpgradeDockerRefusesAMovingTag is cardinal rule 2 for a tag whose
// images do not say which events table they write (main, production,
// sha-…): no run could check the plane works on it, so it is refused before
// Docker is asked anything, the project untouched. The edge channel's
// :staging writes the new schema and is not refused.
func TestUpgradeDockerRefusesAMovingTag(t *testing.T) {
	for _, tag := range []string{"main", "production", "sha-881359f"} {
		p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.20"), nil)
		f := newFakeCompose(t)
		f.live = liveJSONRow
		before := p.snapshot(t)
		out, err := upgradeIn(t, p.dir, tag, bundleAuto)
		if err == nil || !strings.Contains(out, tag+" is not a numbered release, so which events table its images write") || len(f.calls) != 0 || strings.Contains(out, "Upgraded") {
			t.Fatalf("%s: %v, calls %v\n%s", tag, err, f.calls, out)
		}
		if after := p.snapshot(t); !mapsEqual(before, after) {
			t.Fatalf("%s: the project was touched", tag)
		}
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
		// The edge channel writes the new schema (a tag no run can check is
		// refused before it starts: TestUpgradeDockerRefusesAMovingTag).
		{"edge, the new table", liveJSONRow, dockerEdgeTag, nil, true, notTouched, ""},
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
// project changes nothing and asks Docker nothing; once that run is over,
// however it ended, the next one goes ahead and leaves no lock behind.
func TestUpgradeDockerLocked(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	unlock, err := lockProject(p.dir, "origamy deploy")
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeCompose(t)
	f.live = liveJSONRow
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, fmt.Sprintf("Another origamy run is working on this data plane (origamy deploy, pid %d on ", os.Getpid())) || len(f.calls) != 0 {
		t.Fatalf("%v, calls %v\n%s", err, f.calls, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}
	unlock()
	if out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(p.dir, projectLockFile)); !os.IsNotExist(err) {
		t.Fatalf("the upgrade left its lock behind: %v", err)
	}
}

// composeCalls are the docker compose calls among calls.
func composeCalls(calls []string) []string {
	return slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.Contains(c, "docker compose ") })
}

// TestUpgradeDockerPinsTheRelease (cardinal rule 2): a DP_IMAGE_TAG exported
// in the operator's shell, which compose takes over .env, never decides what
// a run starts. Every docker compose command is given the release it is for:
// the one the project runs while ClickHouse is asked, the target from then
// on (the reset, `up`, the restarts and the checks after it).
func TestUpgradeDockerPinsTheRelease(t *testing.T) {
	t.Setenv("DP_IMAGE_TAG", "0.1.17")
	// Nor do the project's name, files or profiles the shell exports (another
	// project's name would point compose at that project).
	t.Setenv("COMPOSE_PROJECT_NAME", "victim")
	t.Setenv("COMPOSE_FILE", "/elsewhere/docker-compose.yml")
	t.Setenv("COMPOSE_PROFILES", "agentic")
	p := newUpgradeProject(t, compose248, stringSchemaSQL, func(base string) string {
		return strings.Replace(envAt("0.1.16")(base), "COMPOSE_PROFILES=full", "COMPOSE_PROFILES=full,ingress", 1)
	}, nil)
	f := newFakeCompose(t)
	f.live = liveStringRow
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || !strings.Contains(out, "DP_IMAGE_TAG=0.1.17 is exported in your shell") || !strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, k := range []string{"COMPOSE_PROJECT_NAME=victim", "COMPOSE_FILE=/elsewhere/docker-compose.yml", "COMPOSE_PROFILES=agentic"} {
		if !strings.Contains(out, k+" is exported in your shell, and docker compose takes it over .env. This run ignores it") {
			t.Fatalf("no warning about %s:\n%s", k, out)
		}
	}
	calls := composeCalls(f.calls)
	pull := callIndex(calls, " pull")
	for i, c := range calls {
		want := dcCall("0.1.20", p.dir, p.env)
		if i < pull && strings.HasPrefix(c, dcCall("0.1.16", p.dir, p.env)) && strings.Contains(c, " exec ") {
			continue // ClickHouse asked on the release it runs now
		}
		if !strings.HasPrefix(c, want) {
			t.Fatalf("call %d not given the target release: %q", i, c)
		}
	}
	for _, step := range []string{"up -d clickhouse", "run --rm --no-deps clickhouse-init", "up -d --remove-orphans", "restart clickhouse", "restart caddy", "ps -a --format json"} {
		if callIndex(calls, step) < 0 {
			t.Fatalf("no %q:\n%s", step, strings.Join(calls, "\n"))
		}
	}
	if f.ranTag != "0.1.20" {
		t.Fatalf("the services were started on %q", f.ranTag)
	}
}

// TestUpgradeDockerChecksTheRunningImages (cardinal rule 2): the run fails,
// without its success text, when what runs once it is through is not the
// release's: a data-plane service on another tag, or a ClickHouse older than
// 25.3 under the new events table.
func TestUpgradeDockerChecksTheRunningImages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeCompose)
		says  []string
	}{
		{"a data-plane service on another tag", func(f *fakeCompose) { f.runTag = "0.1.17" },
			[]string{"portal-agent runs ghcr.io/qubelylabs/origamy-data-plane/portal-agent:0.1.17", "DP_IMAGE_TAG=0.1.17 is exported in your shell", "config --images"}},
		{"an older ClickHouse under the new table", func(f *fakeCompose) { f.chImage = "clickhouse/clickhouse-server:24.8" },
			[]string{"clickhouse runs clickhouse/clickhouse-server:24.8, and the new events table needs ClickHouse 25.3 or newer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DP_IMAGE_TAG", "0.1.17")
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			tc.setup(f)
			out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
			if err == nil || strings.Contains(out, "Upgraded") || strings.Contains(out, notTouched) {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, want := range tc.says {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestUpgradeDockerDuplicateTagLines: an .env naming DP_IMAGE_TAG twice (the
// operator pinned it again at the bottom) is read as compose reads it, the
// last line winning, and left with one line naming the new release, the
// rest as it was.
func TestUpgradeDockerDuplicateTagLines(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, func(base string) string {
		return "# pinned\n" + envAt("0.1.19")(base) + "# pinned again\nDP_IMAGE_TAG=0.1.17\n"
	}, nil)
	before := readFile(t, p.env)
	f := newFakeCompose(t)
	f.live = liveJSONRow
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || !strings.Contains(out, ".env has 2 DP_IMAGE_TAG lines; docker compose takes the last (0.1.17)") {
		t.Fatalf("%v\n%s", err, out)
	}
	// ClickHouse was asked on the release compose runs (the last line).
	if callIndex(composeCalls(f.calls), dcCall("0.1.17", p.dir, p.env)+"exec") != 0 {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	after := readFile(t, p.env)
	if envKeyCount(p.env, "DP_IMAGE_TAG") != 1 || readEnvVar(p.env, "DP_IMAGE_TAG") != "0.1.20" ||
		!strings.HasPrefix(after, strings.Replace(strings.Split(before, "# pinned again")[0], "DP_IMAGE_TAG=0.1.19\n", "", 1)+"# pinned again\nDP_IMAGE_TAG=0.1.20\n") {
		t.Fatalf(".env before:\n%s\nafter:\n%s", before, after)
	}
}

// TestUpgradeDockerAIToggleCommitPoint (cardinal rule 3): --enable-ai and
// --disable-ai reach docker compose through its environment until the
// commit point; a run that fails before it (a pull, the storage reset)
// leaves .env byte for byte as it was, but for the reset record.
func TestUpgradeDockerAIToggleCommitPoint(t *testing.T) {
	run := func(t *testing.T, dir string, enable, disable bool) (string, error) {
		t.Helper()
		chdir(t, dir)
		var err error
		out := captureStdout(t, func() { err = upgradeDocker("0.1.20", "stable", enable, disable, false, true, bundleAuto) })
		return out, err
	}
	for _, tc := range []struct {
		name            string
		profiles        string
		enable, disable bool
		pullEnv         []string
	}{
		{"--enable-ai", "full", true, false, []string{"COMPOSE_PROFILES=full,agentic", "ORCHESTRATOR_ENGINE_URL=" + orchestratorEngineURL, "ORCH_KEK="}},
		{"--disable-ai", "full,agentic", false, true, []string{"COMPOSE_PROFILES=full", "ORCHESTRATOR_ENGINE_URL= "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, func(base string) string {
				return strings.Replace(envAt("0.1.19")(base), "COMPOSE_PROFILES=full", "COMPOSE_PROFILES="+tc.profiles, 1)
			}, nil)
			f := newFakeCompose(t)
			f.live, f.failPull = liveJSONRow, true
			before := p.snapshot(t)
			if out, err := run(t, p.dir, tc.enable, tc.disable); err == nil {
				t.Fatalf("the pull must fail:\n%s", out)
			}
			if after := p.snapshot(t); !mapsEqual(before, after) {
				t.Fatalf("a failed pull touched the project:\nbefore %v\nafter  %v", before, after)
			}
			pull := f.calls[callIndex(f.calls, " pull")]
			for _, want := range tc.pullEnv {
				if !strings.Contains(pull, want) {
					t.Fatalf("the pull was not given %q: %q", want, pull)
				}
			}

			// Through: .env takes the toggle with the release, and the `up -d`
			// is given it too, over a COMPOSE_PROFILES the shell exports.
			t.Setenv("COMPOSE_PROFILES", "full")
			f.failPull = false
			if out, err := run(t, p.dir, tc.enable, tc.disable); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if up := f.calls[callIndex(f.calls, "up -d --remove-orphans")]; !strings.Contains(up, tc.pullEnv[0]+" ") {
				t.Fatalf("the up was not given %q: %q", tc.pullEnv[0], up)
			}
			if got := readEnvVar(p.env, "COMPOSE_PROFILES"); !strings.HasPrefix(tc.pullEnv[0], "COMPOSE_PROFILES="+got) || readEnvVar(p.env, "DP_IMAGE_TAG") != "0.1.20" ||
				(tc.enable && (readEnvVar(p.env, "ORCH_KEK") == "" || readEnvVar(p.env, "ORCHESTRATOR_ENGINE_URL") != orchestratorEngineURL)) {
				t.Fatalf(".env:\n%s", readFile(t, p.env))
			}
		})
	}

	// A storage reset that fails: the record is all .env gains.
	p := newUpgradeProject(t, compose248, stringSchemaSQL, envAt("0.1.16"), nil)
	f := newFakeCompose(t)
	f.live, f.initFails = liveStringRow, 1
	before := readFile(t, p.env)
	if out, err := run(t, p.dir, true, false); err == nil {
		t.Fatalf("the reset must fail:\n%s", out)
	}
	if got := readFile(t, p.env); got != before+resetPendingVar+"=0.1.20\n" {
		t.Fatalf(".env before:\n%s\nafter:\n%s", before, got)
	}
}

// TestUpgradeDockerRepairsABrokenComposeFile: a project whose
// docker-compose.yml does not parse (an edit gone wrong) has its ClickHouse
// asked through the container compose made, and --replace-bundle repairs it
// with the release's validated file. Without it the edit is kept, the pull
// fails on it, and the project is left as it was.
func TestUpgradeDockerRepairsABrokenComposeFile(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	p := newUpgradeProject(t, compose253+brokenCompose, jsonSchemaSQL, envAt("0.1.19"), nil)
	f := newFakeCompose(t)
	f.live, f.container = liveJSONRow, "0123456789ab"
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "Asked ClickHouse through its container") || !strings.Contains(out, "--replace-bundle takes the 0.1.20 one") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}

	out, err = upgradeIn(t, p.dir, "0.1.20", bundleReplace)
	if err != nil || !strings.Contains(out, "Upgraded") || p.read(t, "docker-compose.yml") != compose253 || readEnvVar(p.env, "DP_IMAGE_TAG") != "0.1.20" {
		t.Fatalf("%v\n%s", err, out)
	}
	if bak := p.read(t, "docker-compose.yml.bak-0.1.19"); !strings.Contains(bak, brokenCompose) {
		t.Fatalf("the broken file was not backed up: %q", bak)
	}

	// Across the storage reset (the pre-reset table): the reset is refused
	// while the broken file is kept, pointing at --replace-bundle, and runs
	// once that takes the release's file.
	p = newUpgradeProject(t, compose248+brokenCompose, stringSchemaSQL, envAt("0.1.16"), nil)
	f = newFakeCompose(t)
	f.live, f.container = liveStringRow, "0123456789ab"
	before = p.snapshot(t)
	out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "Asked ClickHouse through its container") || !strings.Contains(out, "with your edited docker-compose.yml, which the refresh keeps") ||
		!strings.Contains(out, "Pass --replace-bundle to take the 0.1.20 compose file") || callIndex(f.calls, "RENAME") >= 0 || callIndex(f.calls, " pull") >= 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}
	out, err = upgradeIn(t, p.dir, "0.1.20", bundleReplace)
	if err != nil || !strings.Contains(out, "Upgraded") || !strings.Contains(out, "Event history was discarded") || p.read(t, "docker-compose.yml") != compose253 ||
		len(f.dropped) != 1 || !strings.HasSuffix(f.dropped[0], ":String") || f.live != liveJSONRow || envHasKey(p.env, resetPendingVar) {
		t.Fatalf("%v, dropped %v\n%s", err, f.dropped, out)
	}
}

// TestEventsTableFailHints: what a failed check after `up -d` tells the
// operator to do depends on the release: a missing table under a moving tag
// (main, sha-…) is not known to be one this CLI can create for it, and one
// under a release before the reset is the init service's to create.
func TestEventsTableFailHints(t *testing.T) {
	missing := liveSchema{gen: liveMissing}
	for _, tc := range []struct {
		tag     string
		says    []string
		pending string
	}{
		{"main", []string{"The :main images are not a numbered release, so which schema generation they write is not known", "`origamy upgrade --version " + helmVersion + "` creates the new events table"}, ""},
		{"sha-881359f", []string{"The :sha-881359f images are not a numbered release"}, ""},
		{"0.1.17", []string{"Release 0.1.17 writes the pre-reset (String) table", "logs clickhouse-init"}, ""},
		{"0.1.20", []string{"The storage reset is recorded in .env"}, "0.1.20"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			dir := t.TempDir()
			env := filepath.Join(dir, ".env")
			writeFile(t, env, "DP_IMAGE_TAG="+tc.tag+"\n")
			out := captureStdout(t, func() { _ = eventsTableFail(dir, env, tc.tag, missing, "ClickHouse holds no events table") })
			for _, want := range tc.says {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "drop it yourself") || readEnvVar(env, resetPendingVar) != tc.pending {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

// TestUpgradeDockerDisableAIStopsTheEngine (the check after `up -d` against a
// profile turned off): `--disable-ai` together with a release move stops and
// removes the AI engine, which compose keeps and `up -d --remove-orphans`
// leaves running on the old release, and the run succeeds. When it cannot be
// stopped, the run still succeeds (only the services of the profiles that
// are on are checked) and says how to stop it.
func TestUpgradeDockerDisableAIStopsTheEngine(t *testing.T) {
	for _, rmFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("rm fails %v", rmFails), func(t *testing.T) {
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, func(base string) string {
				return strings.Replace(envAt("0.1.19")(base), "COMPOSE_PROFILES=full", "COMPOSE_PROFILES=full,agentic", 1)
			}, nil)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			// What the plane ran with the AI engine on, on 0.1.19.
			for _, s := range []string{"config-sync", "portal-agent", "workflow-engine", "orchestrator-engine"} {
				f.running[s] = dpImage + s + ":0.1.19"
			}
			f.running["dp-postgres"], f.running["clickhouse"] = "postgres:16-alpine", "clickhouse/clickhouse-server:25.3"
			if rmFails {
				prev := dockerRun
				stubDocker(t, func(env []string, name string, args ...string) (string, error) {
					if slices.Contains(args, "rm") {
						return "Error response from daemon: cannot stop container", errors.New("exit status 1")
					}
					return prev(env, name, args...)
				})
			}
			chdir(t, p.dir)
			var err error
			out := captureStdout(t, func() { err = upgradeDocker("0.1.20", "stable", false, true, false, true, bundleAuto) })
			if err != nil || !strings.Contains(out, "Upgraded") || readEnvVar(p.env, "COMPOSE_PROFILES") != "full" {
				t.Fatalf("%v\n%s", err, out)
			}
			// Stopped before `up -d`, only the engine (dp-postgres runs for
			// journeys too).
			if rm, up := callIndex(f.calls, " rm --stop --force orchestrator-engine"), callIndex(f.calls, "up -d --remove-orphans"); !rmFails && (rm < 0 || up < rm) {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
			if _, kept := f.running["orchestrator-engine"]; kept != rmFails || !slices.Contains(slices.Collect(maps.Keys(f.running)), "dp-postgres") {
				t.Fatalf("running %v", f.running)
			}
			if rmFails != strings.Contains(out, "Could not stop orchestrator-engine") {
				t.Fatalf("%s", out)
			}
		})
	}
}

// TestUpgradeDockerOtherProjectName: a plane whose containers run under
// another compose project name than the one the CLI pins (a
// COMPOSE_PROJECT_NAME exported when it was deployed) is refused before
// anything, since `up -d` would start a second copy of it; with that name in
// .env, every command pins it.
func TestUpgradeDockerOtherProjectName(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	f := newFakeCompose(t)
	f.live = liveJSONRow
	f.owners = "custom\t" + p.dir + "\nvictim\t/somewhere/else\n"
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "run under the compose project name custom, not "+composeProjectName(p.dir, p.env)) ||
		!strings.Contains(out, "COMPOSE_PROJECT_NAME=custom") || len(composeCalls(f.calls)) != 0 {
		t.Fatalf("%v, calls %v\n%s", err, f.calls, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched")
	}
	writeFile(t, p.env, readFile(t, p.env)+"COMPOSE_PROJECT_NAME=custom\n")
	out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || !strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range composeCalls(f.calls) {
		if !strings.Contains(c, " docker compose -p custom ") {
			t.Fatalf("not pinned to custom: %q", c)
		}
	}
}

// TestUpgradeDockerPlannedImages (cardinal rules 2 and 3): what the data-plane
// services would start from is checked before the commit point. An override
// pinning another tag, or a registry mirror whose image is not the
// release's, is refused with the project as it was; a mirror of the
// release's very image goes through.
func TestUpgradeDockerPlannedImages(t *testing.T) {
	const mirror = "mirror.example.com/origamy/portal-agent:"
	for _, tc := range []struct {
		name, override string
		setup          func(*fakeCompose)
		says           string
	}{
		{"an override pins another tag", "services:\n  portal-agent:\n    image: " + dpImage + "portal-agent:0.1.19\n", nil, "portal-agent is set to " + dpImage + "portal-agent:0.1.19"},
		{"a mirror of another image", "services:\n  portal-agent:\n    image: " + mirror + "${DP_IMAGE_TAG:-main}\n", nil, "which is not the release's image " + dpImage + "portal-agent:0.1.20"},
		{"a mirror that cannot be compared", "services:\n  portal-agent:\n    image: " + mirror + "${DP_IMAGE_TAG:-main}\n", func(f *fakeCompose) {
			f.missing[dpImage+"portal-agent:0.1.20"], f.pullFails = true, true
		}, "could not be pulled here to compare them"},
		{"a mirror of the release", "services:\n  portal-agent:\n    image: " + mirror + "${DP_IMAGE_TAG:-main}\n", func(f *fakeCompose) {
			f.ids[mirror+"0.1.20"] = f.imageID(dpImage + "portal-agent:0.1.20")
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
			writeFile(t, filepath.Join(p.dir, "docker-compose.override.yml"), tc.override)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			if tc.setup != nil {
				tc.setup(f)
			}
			before := p.snapshot(t)
			out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
			if tc.says == "" {
				if err != nil || !strings.Contains(out, "Upgraded") {
					t.Fatalf("%v\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(out, tc.says) || !strings.Contains(out, "Nothing has been changed.") || strings.Contains(out, "Upgraded") {
				t.Fatalf("%v\n%s", err, out)
			}
			if after := p.snapshot(t); !mapsEqual(before, after) || callIndex(f.calls, "--remove-orphans") >= 0 {
				t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}
