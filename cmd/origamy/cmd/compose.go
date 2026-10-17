package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── Project discovery ────────────────────────────────────────────────────────

// errNoProject means no Origamy compose project was found here.
var errNoProject = errors.New("no Origamy compose project found")

// existingProjectID returns the data-plane id of the Origamy compose project
// in dir, or "" when dir is not one. A directory counts only when it has both
// the compose file AND an .env naming a DATA_PLANE_ID — any docker-compose.yml
// alone (some unrelated project the operator happens to be in) must not be
// mistaken for ours and have its .env rewritten.
func existingProjectID(dir string) string {
	if !fileExists(filepath.Join(dir, "docker-compose.yml")) {
		return ""
	}
	return readEnvVar(filepath.Join(dir, ".env"), "DATA_PLANE_ID")
}

// composeProjectDir finds the Origamy compose project to operate on: the
// current directory when it is one, otherwise the single ./origamy-dp-*/
// project below it. Several candidates are an error rather than a silent
// first-match: the operator must say which plane they mean.
func composeProjectDir() (string, error) {
	if existingProjectID(".") != "" {
		return os.Getwd()
	}
	matches, _ := filepath.Glob("origamy-dp-*/docker-compose.yml")
	var dirs []string
	for _, m := range matches {
		if d := filepath.Dir(m); existingProjectID(d) != "" {
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	switch len(dirs) {
	case 0:
		return "", errNoProject
	case 1:
		return dirs[0], nil
	default:
		return "", fmt.Errorf("several Origamy data planes here (%s) — cd into the one you mean", strings.Join(dirs, ", "))
	}
}

// hasProfile reports whether name is in the profile list.
func hasProfile(profiles []string, name string) bool {
	for _, p := range profiles {
		if p == name {
			return true
		}
	}
	return false
}

// ── Health after `up -d` ─────────────────────────────────────────────────────

// composeService is the subset of `docker compose ps --format json` we read.
// Status is the human line ("Up 5 seconds", "Restarting (1) 3 seconds ago");
// a crash-looping container reports State "running" between restarts, so
// Status is the only field that exposes the loop.
type composeService struct {
	Service  string `json:"Service"`
	State    string `json:"State"`
	Status   string `json:"Status"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}

// parseComposePS accepts both shapes `docker compose ps --format json` has
// produced: a JSON array (Compose < 2.21) and one object per line.
func parseComposePS(out string) ([]composeService, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var list []composeService
	if strings.HasPrefix(out, "[") {
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s composeService
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	return list, nil
}

// classifyCompose sorts services into those still coming up and those that
// have failed. A one-shot init service (name ending in -init) that exited 0 is
// done, not failed; a service that exited non-zero, died or is restarting has
// failed; anything created or still passing its healthcheck is pending.
func classifyCompose(svcs []composeService) (pending, failed []string) {
	for _, s := range svcs {
		if strings.HasPrefix(strings.ToLower(s.Status), "restarting") {
			failed = append(failed, s.Service)
			continue
		}
		switch strings.ToLower(s.State) {
		case "running":
			if strings.ToLower(s.Health) == "starting" {
				pending = append(pending, s.Service)
			} else if strings.ToLower(s.Health) == "unhealthy" {
				failed = append(failed, s.Service)
			}
		case "exited":
			if s.ExitCode == 0 && strings.HasSuffix(s.Service, "-init") {
				continue
			}
			failed = append(failed, s.Service)
		case "dead", "restarting":
			failed = append(failed, s.Service)
		default: // created, paused, …
			pending = append(pending, s.Service)
		}
	}
	sort.Strings(pending)
	sort.Strings(failed)
	return pending, failed
}

// composeSettle is how long every service must stay up, with no exit or
// restart, before the stack counts as healthy. A service that cannot reach
// the control plane (wrong token, refused client cert) starts fine and dies a
// few seconds later; declaring success on the first all-running snapshot
// would miss exactly that. composeWait bounds the whole watch, and
// composePoll is how often it looks. Variables so tests need not wait.
var (
	composeSettle = 30 * time.Second
	composeWait   = 3 * time.Minute
	composePoll   = 3 * time.Second
)

// waitForCompose polls the project for up to composeWait. It returns healthy
// when every service has been running (and healthy, where it has a check)
// for composeSettle, or the failed services as soon as one exits or starts
// restarting.
func waitForCompose(dir, envPath string, sp *ui.Spinner) (healthy bool, failed []string) {
	deadline := time.Now().Add(composeWait)
	var stableSince time.Time
	for {
		out, err := dockerRun(nil, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "ps", "-a", "--format", "json")
		if err == nil {
			if svcs, perr := parseComposePS(out); perr == nil && len(svcs) > 0 {
				pending, failed := classifyCompose(svcs)
				if len(failed) > 0 {
					return false, failed
				}
				if len(pending) == 0 {
					if stableSince.IsZero() {
						stableSince = time.Now()
					}
					if time.Since(stableSince) >= composeSettle {
						return true, nil
					}
					sp.Suffix("all %d services up — watching for %ds", len(svcs), int((composeSettle-time.Since(stableSince)).Seconds())+1)
				} else {
					stableSince = time.Time{}
					sp.Suffix("%d/%d services ready", len(svcs)-len(pending), len(svcs))
				}
			}
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(composePoll)
	}
}

// What probeLiveSchema can find besides the two generations ("json",
// "string"): ClickHouse answered and has no events table (liveMissing), or a
// payload column of a type neither generation writes (liveOther).
const (
	liveMissing = "missing"
	liveOther   = "other"
)

// liveSchema is what the running ClickHouse of a compose project holds.
type liveSchema struct {
	// gen is "json", "string", liveMissing or liveOther; "" when ClickHouse
	// could not be asked (not running, no answer within clickhouseProbeTimeout).
	gen string
	// typ is the payload column's type as ClickHouse reported it; out, what
	// a failed ask printed.
	typ, out string
}

// known reports whether ClickHouse said which events table it holds.
func (l liveSchema) known() bool { return l.gen == "json" || l.gen == "string" }

// clickhouseProbeTimeout bounds every query the CLI sends into the ClickHouse
// container (the events-table probe, the readiness ping): a hung container
// must not stall the upgrade. A probe that runs out is "could not be asked".
// A variable so tests need not wait.
var clickhouseProbeTimeout = 20 * time.Second

// probeLiveSchema asks the running ClickHouse of a compose project what its
// events table is. The served init SQL says what the control plane would
// create; this says what the volume already holds. env is extra environment
// for compose (DP_IMAGE_TAG during the reset).
func probeLiveSchema(dir, envPath string, env []string) liveSchema {
	out, err := dockerRunTimeout(clickhouseProbeTimeout, env, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "exec", "-T", "clickhouse", "sh", "-c", clickhouseEventsPropertiesType)
	if err != nil {
		return liveSchema{out: out}
	}
	// The output is combined: compose's warnings about variables an older
	// .env lacks come before the answer, which is the column's type and
	// eventsColumnMark (a bare type is taken too when it is one of ours).
	for _, l := range strings.Split(out, "\n") {
		typ, mark, row := strings.Cut(strings.TrimSpace(l), "\t")
		if row && mark != eventsColumnMark {
			continue
		}
		switch {
		case strings.HasPrefix(typ, "JSON"):
			return liveSchema{gen: "json", typ: typ}
		case strings.HasPrefix(typ, "String"):
			return liveSchema{gen: "string", typ: typ}
		case row:
			return liveSchema{gen: liveOther, typ: typ}
		}
	}
	return liveSchema{gen: liveMissing}
}

// describe says what ClickHouse holds, for a message.
func (l liveSchema) describe() string {
	switch l.gen {
	case "json":
		return "the new events table (JSON payload columns)"
	case "string":
		return "the pre-reset events table (String payload columns)"
	case liveMissing:
		return "no events table"
	case liveOther:
		return "an events table with payload columns of type " + l.typ + ", neither the pre-reset (String) nor the new (JSON) schema"
	}
	return "an events table it did not say"
}

// waitClickHouse waits, up to clickhouseReadyTimeout, until the project's
// ClickHouse answers a query. out is what the last ping printed when it never
// did. env is extra environment for compose.
func waitClickHouse(dir, envPath string, env []string) (out string, ok bool) {
	deadline := time.Now().Add(clickhouseReadyTimeout)
	for {
		o, err := dockerRunTimeout(clickhouseProbeTimeout, env, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "exec", "-T", "clickhouse", "sh", "-c", clickhousePing)
		if err == nil {
			return "", true
		}
		if time.Now().After(deadline) {
			return o, false
		}
		time.Sleep(clickhouseReadyPoll)
	}
}

// askClickHouse asks the project's ClickHouse which events table it holds,
// which decides what an upgrade or an in-place deploy does with it. One that
// cannot be asked (stopped, hung, no container) is started and asked again:
// `up -d clickhouse` with the project's own compose files and .env on the
// release it runs now (tag, its DP_IMAGE_TAG), so nothing but ClickHouse
// changes, then waited for (clickhouseReadyTimeout). gen stays "" when it
// still cannot be asked; the caller refuses rather than guess from files or
// tags.
func askClickHouse(dir, envPath, tag string) liveSchema {
	var env []string
	if tag != "" {
		env = []string{"DP_IMAGE_TAG=" + tag}
	}
	if live := probeLiveSchema(dir, envPath, env); live.gen != "" {
		return live
	}
	sp := ui.Start("ClickHouse did not answer; starting it on the release this plane runs now")
	if out, err := dockerRun(env, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "up", "-d", "clickhouse"); err != nil {
		sp.Fail("Could not start ClickHouse")
		return liveSchema{out: out}
	}
	if out, ok := waitClickHouse(dir, envPath, env); !ok {
		sp.Fail("ClickHouse did not answer within %s", clickhouseReadyTimeout)
		return liveSchema{out: out}
	}
	live := probeLiveSchema(dir, envPath, env)
	if live.gen == "" {
		sp.Fail("ClickHouse is up, and did not say which events table it holds")
		return live
	}
	sp.Success("ClickHouse is up and holds %s", live.describe())
	return live
}

// initRunning reports whether the bundle's one-shot clickhouse-init service,
// which every `up -d` runs again, is still at work (created or running).
func initRunning(dir, envPath string, env []string) bool {
	out, err := dockerRun(env, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "ps", "-a", "--format", "json", "clickhouse-init")
	if err != nil {
		return false
	}
	// compose's warnings come first on the same output.
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			rows = append(rows, t)
		}
	}
	svcs, err := parseComposePS(strings.Join(rows, "\n"))
	if err != nil {
		return false
	}
	for _, s := range svcs {
		if s.Service == "clickhouse-init" && slices.Contains([]string{"created", "running", "restarting"}, strings.ToLower(s.State)) {
			return true
		}
	}
	return false
}

// eventsTableProblem is the check after the last `up -d` of an upgrade or a
// deploy: ClickHouse answers, and its events table exists and is the one the
// images of tag write (any of the two for a tag whose schema is not known:
// main, sha-…). It returns what ClickHouse holds and, when that is not it,
// why ("" when it is). A missing table is asked about again while the init
// service `up -d` started is still at work.
func eventsTableProblem(dir, envPath, tag string) (liveSchema, string) {
	env := []string{"DP_IMAGE_TAG=" + tag}
	sp := ui.Start("Checking the events table the %s images write to", tag)
	if out, ok := waitClickHouse(dir, envPath, env); !ok {
		sp.Fail("ClickHouse did not answer within %s", clickhouseReadyTimeout)
		return liveSchema{out: out}, "ClickHouse did not answer, so whether " + eventsTable + " is there could not be checked"
	}
	deadline := time.Now().Add(clickhouseReadyTimeout)
	var live liveSchema
	for {
		live = probeLiveSchema(dir, envPath, env)
		settled := live.gen != "" && (live.gen != liveMissing || !initRunning(dir, envPath, env))
		if settled || time.Now().After(deadline) {
			break
		}
		time.Sleep(clickhouseReadyPoll)
	}
	want := releaseSchemaGeneration(tag)
	why := ""
	switch {
	case live.gen == "":
		why = "ClickHouse did not say which events table it holds"
	case !live.known():
		why = "ClickHouse holds " + live.describe()
	case want != "unknown" && live.gen != want:
		why = "ClickHouse holds " + live.describe() + ", which the " + tag + " images cannot write"
	}
	if why != "" {
		sp.Fail("%s", why)
		return live, why
	}
	if want == "unknown" {
		sp.Success("ClickHouse holds %s (the :%s images are not a numbered release, so which one they write is not known)", live.describe(), tag)
	} else {
		sp.Success("ClickHouse holds %s", live.describe())
	}
	return live, ""
}

// eventsColumnMark follows the type on the probe's answer, so a type of
// neither generation is told from compose's own output.
const eventsColumnMark = "events.events.properties"

// clickhouseEventsPropertiesType prints the type of the events table's
// payload column and eventsColumnMark, tab-separated; nothing when the table
// does not exist.
const clickhouseEventsPropertiesType = `clickhouse-client --password "$CH_DEFAULT_PASSWORD" --query "SELECT type, '` + eventsColumnMark + `' FROM system.columns WHERE database = 'events' AND table = 'events' AND name = 'properties'"`

// composeConfig is what the CLI reads of `docker compose config --format
// json`: the project's effective configuration.
type composeConfig struct {
	Services map[string]struct {
		Image string `json:"image"`
	} `json:"services"`
}

// effectiveCompose renders the configuration `docker compose up` would run in
// dir with DP_IMAGE_TAG=tag: the files in fileArgs (composeFileArgs, so
// override files and COMPOSE_FILE count), interpolated with envPath.
func effectiveCompose(dir, envPath, tag string, fileArgs []string) (composeConfig, error) {
	args := append(append([]string{"compose", "--project-directory", dir}, fileArgs...), "--env-file", envPath, "config", "--format", "json")
	out, err := dockerRun([]string{"DP_IMAGE_TAG=" + tag}, "docker", args...)
	if err != nil {
		return composeConfig{}, fmt.Errorf("docker compose config failed: %s", strings.ReplaceAll(lastLines(out, 3), "\n", " "))
	}
	// compose's warnings come first on the same output; the JSON starts on a
	// line of its own.
	start, off := -1, 0
	for _, l := range strings.SplitAfter(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "{") {
			start = off
			break
		}
		off += len(l)
	}
	var cfg composeConfig
	if start < 0 {
		return cfg, errors.New("docker compose config printed no configuration")
	}
	if err := json.NewDecoder(strings.NewReader(out[start:])).Decode(&cfg); err != nil {
		return composeConfig{}, fmt.Errorf("could not read docker compose config: %w", err)
	}
	return cfg, nil
}

// composeLogs returns the last n log lines of one service.
func composeLogs(dir, envPath, service string, n int) string {
	out, _ := dockerRun(nil, "docker", "compose", "--project-directory", dir, "--env-file", envPath,
		"logs", "--no-color", "--tail", fmt.Sprintf("%d", n), service)
	return strings.TrimSpace(out)
}
