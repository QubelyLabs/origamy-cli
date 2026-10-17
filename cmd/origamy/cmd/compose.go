package cmd

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// Status is the only field that exposes the loop. Image is what the
// container was created from (its ID once that tag names another image).
type composeService struct {
	ID       string `json:"ID"`
	Service  string `json:"Service"`
	Image    string `json:"Image"`
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

// composeRows keeps the JSON lines of compose's output: its warnings about
// variables an older .env lacks come first on the same output.
func composeRows(out string) string {
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
			rows = append(rows, t)
		}
	}
	return strings.Join(rows, "\n")
}

// waitForCompose polls the project for up to composeWait. It returns healthy
// when every service has been running (and healthy, where it has a check)
// for composeSettle, or the failed services as soon as one exits or starts
// restarting. env is the compose environment (tagEnv).
func waitForCompose(dir, envPath string, env []string, sp *ui.Spinner) (healthy bool, failed []string) {
	deadline := time.Now().Add(composeWait)
	var stableSince time.Time
	for {
		out, err := dockerRun(env, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "ps", "-a", "--format", "json")
		if err == nil {
			if svcs, perr := parseComposePS(composeRows(out)); perr == nil && len(svcs) > 0 {
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

// tagEnv is the environment of every docker compose command the CLI runs in
// a project: DP_IMAGE_TAG set to the release the command is for. compose
// takes the process environment over .env, so a DP_IMAGE_TAG exported in the
// operator's shell would otherwise start another release. With none ("": the
// project's .env names no release) the shell's is left out (a bare name, see
// commandEnv), so compose takes the compose file's default.
func tagEnv(tag string) []string {
	if tag == "" {
		return []string{"DP_IMAGE_TAG"}
	}
	return []string{"DP_IMAGE_TAG=" + tag}
}

// clickhouseConn is how the CLI reaches a project's ClickHouse: through
// compose (exec in its clickhouse service, with env), or, when compose cannot
// load the project (a compose file or .env that does not parse), through
// the container itself (id), found by the labels compose put on it.
type clickhouseConn struct {
	dir, envPath string
	env          []string
	id           string
}

// exec runs a shell snippet in the ClickHouse container, within
// clickhouseProbeTimeout.
func (c clickhouseConn) exec(script string) (string, error) {
	if c.id != "" {
		return dockerRunTimeout(clickhouseProbeTimeout, nil, "docker", "exec", c.id, "sh", "-c", script)
	}
	return dockerRunTimeout(clickhouseProbeTimeout, c.env, "docker", "compose", "--project-directory", c.dir, "--env-file", c.envPath, "exec", "-T", "clickhouse", "sh", "-c", script)
}

// probe asks ClickHouse what its events table is.
func (c clickhouseConn) probe() liveSchema {
	out, err := c.exec(clickhouseEventsPropertiesType)
	if err != nil {
		return liveSchema{out: out}
	}
	return parseLiveSchema(out)
}

// parseLiveSchema reads the answer of clickhouseEventsPropertiesType (or of
// the same query run elsewhere) that ClickHouse gave.
func parseLiveSchema(out string) liveSchema {
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

// wait waits, up to clickhouseReadyTimeout, until ClickHouse answers a
// query. out is what the last ping printed when it never did.
func (c clickhouseConn) wait() (out string, ok bool) {
	deadline := time.Now().Add(clickhouseReadyTimeout)
	for {
		o, err := c.exec(clickhousePing)
		if err == nil {
			return "", true
		}
		if time.Now().After(deadline) {
			return o, false
		}
		time.Sleep(clickhouseReadyPoll)
	}
}

// probeLiveSchema asks the running ClickHouse of a compose project what its
// events table is. The served init SQL says what the control plane would
// create; this says what the volume already holds. env is the compose
// environment (tagEnv).
func probeLiveSchema(dir, envPath string, env []string) liveSchema {
	return clickhouseConn{dir: dir, envPath: envPath, env: env}.probe()
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
// did. env is the compose environment (tagEnv).
func waitClickHouse(dir, envPath string, env []string) (out string, ok bool) {
	return clickhouseConn{dir: dir, envPath: envPath, env: env}.wait()
}

var (
	// reComposeName is a compose file's top-level name.
	reComposeName = regexp.MustCompile(`(?m)^name:[ \t]*["']?([^"'\s#]+)`)
	// reContainerID is a container's (short or full) ID.
	reContainerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
)

// composeProjectName is the name compose gives the project in dir, which its
// com.docker.compose.project label carries: COMPOSE_PROJECT_NAME (the process
// environment over .env), else the top-level name of docker-compose.yml, else
// the directory's name, lowercased to the characters compose keeps.
func composeProjectName(dir, envPath string) string {
	name := cmp.Or(os.Getenv("COMPOSE_PROJECT_NAME"), unquoteEnv(readEnvVar(envPath, "COMPOSE_PROJECT_NAME")))
	if b, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml")); name == "" && err == nil {
		if m := reComposeName.FindSubmatch(b); m != nil {
			name = string(m[1])
		}
	}
	if name == "" {
		name = filepath.Base(dir)
	}
	keep := strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(name))
	return strings.TrimLeft(keep, "-_")
}

// clickhouseContainer finds the project's ClickHouse container by the labels
// compose put on it (project and service), for when compose cannot load the
// project: its id and whether it runs, "" unless there is exactly one.
func clickhouseContainer(dir, envPath string) (id string, running bool) {
	out, err := dockerRun(nil, "docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project="+composeProjectName(dir, envPath),
		"--filter", "label=com.docker.compose.service=clickhouse",
		"--format", "{{.ID}} {{.State}}")
	if err != nil {
		return "", false
	}
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 && reContainerID.MatchString(f[0]) {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	f := strings.Fields(found[0])
	return f[0], f[1] == "running"
}

// askClickHouse asks the project's ClickHouse which events table it holds,
// which decides what an upgrade or an in-place deploy does with it. One that
// compose cannot ask is asked through its container (clickhouseContainer)
// when that runs: the project's compose file or .env may not parse (an edit
// gone wrong), which the bundle the run installs then repairs. One that still
// cannot be asked (stopped, hung, no container) is started and asked again:
// `up -d clickhouse` with the project's own compose files and .env on the
// release it runs now (tag, its DP_IMAGE_TAG), or, when compose cannot load
// the project, `docker start` of that container, so nothing but ClickHouse
// changes; then waited for (clickhouseReadyTimeout). gen stays "" when it
// still cannot be asked; the caller refuses rather than guess from files or
// tags.
func askClickHouse(dir, envPath, tag string) liveSchema {
	c := clickhouseConn{dir: dir, envPath: envPath, env: tagEnv(tag)}
	live := c.probe()
	if live.gen != "" {
		return live
	}
	id, running := clickhouseContainer(dir, envPath)
	direct := clickhouseConn{id: id}
	if running {
		if l := direct.probe(); l.gen != "" {
			ui.Step("Asked ClickHouse through its container (%s), which docker compose could not reach in this project (%s).", id, orDash(lastLines(live.out, 1)))
			return l
		}
	}
	sp := ui.Start("ClickHouse did not answer; starting it on the release this plane runs now")
	if out, err := dockerRun(c.env, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "up", "-d", "clickhouse"); err != nil {
		if id == "" {
			sp.Fail("Could not start ClickHouse")
			return liveSchema{out: out}
		}
		// compose cannot start it: the container it made starts as it was.
		if o, err := dockerRun(nil, "docker", "start", id); err != nil {
			sp.Fail("Could not start ClickHouse")
			return liveSchema{out: strings.TrimSpace(out + "\n" + o)}
		}
		c = direct
	}
	if out, ok := c.wait(); !ok {
		sp.Fail("ClickHouse did not answer within %s", clickhouseReadyTimeout)
		return liveSchema{out: out}
	}
	live = c.probe()
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
	svcs, err := parseComposePS(composeRows(out))
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
// service `up -d` started is still at work. runningImagesProblem checks that
// the images of tag are the ones that run.
func eventsTableProblem(dir, envPath, tag string) (liveSchema, string) {
	env := tagEnv(tag)
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

// reImageID is an image's ID, which Docker shows for a container in place of
// the reference it was created from once that tag names another image.
var reImageID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{12,64}$`)

// imageTag is the tag of an image reference ("" when it names none).
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// runningImagesProblem is the rest of the check after the last `up -d`
// (eventsTableProblem): the containers that run are the release's. Every
// data-plane service (an image of dataPlaneImageRepo) runs tag, and while
// ClickHouse holds the new events table (gen), ClickHouse runs
// minClickHouseForJSON or newer (as its image's tag says, or the server when
// the tag names no version). What runs is what `docker compose ps` lists,
// and a container listed by its image's ID (the tag names another image now)
// is asked what it was created from. It returns why the images are not the
// release's, "" when they are.
func runningImagesProblem(dir, envPath, tag, gen string) string {
	sp := ui.Start("Checking the images that run")
	out, err := dockerRun(tagEnv(tag), "docker", "compose", "--project-directory", dir, "--env-file", envPath, "ps", "--format", "json")
	svcs, perr := parseComposePS(composeRows(out))
	if err != nil || perr != nil {
		why := "docker compose could not list the containers that run (" + orDash(lastLines(out, 1)) + ")"
		sp.Fail("%s", why)
		return why
	}
	var wrong []string
	dataPlane, clickhouse := 0, ""
	for _, s := range svcs {
		image := s.Image
		if reImageID.MatchString(image) && s.ID != "" {
			if o, err := dockerRun(nil, "docker", "inspect", "--format", "{{.Config.Image}}", s.ID); err == nil && strings.TrimSpace(o) != "" {
				image = strings.TrimSpace(o)
			}
		}
		switch {
		case s.Service == "clickhouse":
			clickhouse = image
		case strings.Contains(image, dataPlaneImageRepo):
			if dataPlane++; imageTag(image) != tag {
				wrong = append(wrong, s.Service+" runs "+image)
			}
		case reImageID.MatchString(image):
			wrong = append(wrong, s.Service+" runs image "+image+", whose tag could not be read")
		}
	}
	if dataPlane == 0 {
		wrong = append(wrong, "no data-plane service is running")
	}
	if gen == "json" {
		needs := ", and the new events table needs ClickHouse " + minClickHouseForJSON + " or newer"
		switch v, ok := clickHouseImageVersion(clickhouse); {
		case clickhouse == "":
			wrong = append(wrong, "ClickHouse is not among the containers that run")
		case !ok:
			// A tag that names no version (latest, a registry's own name):
			// the server itself says which it is.
			out, err := clickhouseConn{dir: dir, envPath: envPath, env: tagEnv(tag)}.exec(clickhouseServerVersion)
			if sv, ok := serverVersion(out); err != nil || !ok {
				wrong = append(wrong, "clickhouse runs "+clickhouse+", whose version neither its tag nor the server ("+orDash(lastLines(out, 1))+") tells"+needs)
			} else if versionLess(sv+".0", minClickHouseForJSON+".0") {
				wrong = append(wrong, "clickhouse runs "+clickhouse+" (ClickHouse "+sv+")"+needs)
			}
		case versionLess(v+".0", minClickHouseForJSON+".0"):
			wrong = append(wrong, "clickhouse runs "+clickhouse+needs)
		}
	}
	if len(wrong) > 0 {
		why := "not every container runs what the " + tag + " release does: " + strings.Join(wrong, "; ")
		sp.Fail("Not every container runs the %s images", tag)
		return why
	}
	sp.Success("Every data-plane service runs the %s images", tag)
	return ""
}

// reServerVersion is the major and minor version at the start of what
// `SELECT version()` prints.
var reServerVersion = regexp.MustCompile(`^(\d+)\.(\d+)\.`)

// serverVersion reads "major.minor" from clickhouseServerVersion's answer
// (compose's warnings can come first).
func serverVersion(out string) (string, bool) {
	for _, l := range strings.Split(out, "\n") {
		if m := reServerVersion.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			return m[1] + "." + m[2], true
		}
	}
	return "", false
}

// runningImagesCauses says what can make a project's containers run other
// images than tag's, for the failure runningImagesProblem reports.
func runningImagesCauses(dir, envPath, tag string) []string {
	var causes []string
	if v := os.Getenv("DP_IMAGE_TAG"); v != "" && v != tag {
		causes = append(causes, "DP_IMAGE_TAG="+v+" is exported in your shell, and docker compose takes it over .env: a `docker compose up` run from this shell starts "+v+". Unset it (unset DP_IMAGE_TAG).")
	}
	if n := envKeyCount(envPath, "DP_IMAGE_TAG"); n > 1 {
		causes = append(causes, fmt.Sprintf(".env has %d DP_IMAGE_TAG lines, and docker compose takes the last (%s): keep one.", n, readEnvVar(envPath, "DP_IMAGE_TAG")))
	}
	return append(causes, "A compose file can pin another image: docker-compose.yml, an override file or one COMPOSE_FILE names (`docker compose --project-directory "+dir+" config --images` lists what compose starts).")
}

// eventsColumnMark follows the type on the probe's answer, so a type of
// neither generation is told from compose's own output.
const eventsColumnMark = "events.events.properties"

// eventsPropertiesQuery selects the type of the events table's payload
// column and eventsColumnMark; no row when the table does not exist.
const eventsPropertiesQuery = `SELECT type, '` + eventsColumnMark + `' FROM system.columns WHERE database = 'events' AND table = 'events' AND name = 'properties'`

// clickhouseEventsPropertiesType prints the type of the events table's
// payload column and eventsColumnMark, tab-separated; nothing when the table
// does not exist.
const clickhouseEventsPropertiesType = `clickhouse-client --password "$CH_DEFAULT_PASSWORD" --query "` + eventsPropertiesQuery + `"`

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
	out, err := dockerRun(tagEnv(tag), "docker", args...)
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

// composeLogs returns the last n log lines of one service; env is the
// compose environment (tagEnv).
func composeLogs(dir, envPath string, env []string, service string, n int) string {
	out, _ := dockerRun(env, "docker", "compose", "--project-directory", dir, "--env-file", envPath,
		"logs", "--no-color", "--tail", fmt.Sprintf("%d", n), service)
	return strings.TrimSpace(out)
}
