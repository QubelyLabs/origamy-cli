package cmd

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// restarting. env is the compose environment (composeEnv).
func waitForCompose(dir, envPath string, env []string, sp *ui.Spinner) (healthy bool, failed []string) {
	deadline := time.Now().Add(composeWait)
	var stableSince time.Time
	for {
		out, err := dockerRun(env, "docker", composeCmd(dir, envPath, "ps", "-a", "--format", "json")...)
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

// composeShellVars are the variables docker compose takes from its process
// environment over the project's .env and its flags' defaults: which project
// it is (its name), which compose files and profiles it loads, and what it
// does with orphans. No docker compose command the CLI runs in a project
// inherits them from the operator's shell (composeEnv): a
// COMPOSE_PROJECT_NAME exported for another project would point `down -v`
// at that project's containers and volumes, and a COMPOSE_FILE or
// COMPOSE_PROFILES left there would start other services than .env names.
// Without them compose reads them from .env, as in a plain `docker compose
// --env-file .env` in a shell that exports none.
var composeShellVars = []string{
	"COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_PROFILES",
	"COMPOSE_ENV_FILES", "COMPOSE_REMOVE_ORPHANS", "COMPOSE_IGNORE_ORPHANS",
}

// composeEnv is the environment of every docker compose command the CLI runs
// in a project: none of composeShellVars from the shell (bare names, see
// commandEnv), and DP_IMAGE_TAG set to the release the command is for.
// compose takes the process environment over .env, so a DP_IMAGE_TAG
// exported in the operator's shell would otherwise start another release.
// With none ("": the project's .env names no release) the shell's is left
// out too, so compose takes the compose file's default. What a run gives
// compose before .env holds it (the AI toggle's COMPOSE_PROFILES) goes after
// these, and wins.
func composeEnv(tag string) []string {
	env := slices.Clone(composeShellVars)
	if tag == "" {
		return append(env, "DP_IMAGE_TAG")
	}
	return append(env, "DP_IMAGE_TAG="+tag)
}

// composeCmd is the argument list of a docker compose command the CLI runs in
// the project at dir: the project's name pinned (-p, composeProjectName,
// never the shell's), its directory and its .env, then args. The env file is
// left out when there is none (an uninstall of a project that lost it).
func composeCmd(dir, envPath string, args ...string) []string {
	cmd := []string{"compose", "-p", composeProjectName(dir, envPath), "--project-directory", dir}
	if envPath != "" && fileExists(envPath) {
		cmd = append(cmd, "--env-file", envPath)
	}
	return append(cmd, args...)
}

// composeHint is how a message tells the operator to run docker compose in
// the project at dir: with the name and the files every command of the CLI
// pins there (composeCmd).
func composeHint(dir, envPath string) string {
	return "docker " + strings.Join(composeCmd(dir, envPath), " ")
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
	return dockerRunTimeout(clickhouseProbeTimeout, c.env, "docker", composeCmd(c.dir, c.envPath, "exec", "-T", "clickhouse", "sh", "-c", script)...)
}

// probe asks ClickHouse what its events table is.
func (c clickhouseConn) probe() liveSchema {
	out, err := c.exec(clickhouseEventsPropertiesType)
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
// environment (composeEnv).
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
// did. env is the compose environment (composeEnv).
func waitClickHouse(dir, envPath string, env []string) (out string, ok bool) {
	return clickhouseConn{dir: dir, envPath: envPath, env: env}.wait()
}

var (
	// reComposeName is a compose file's top-level name.
	reComposeName = regexp.MustCompile(`(?m)^name:[ \t]*["']?([^"'\s#]+)`)
	// reContainerID is a container's (short or full) ID.
	reContainerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
)

// composeProjectName is the name of the compose project in dir, which every
// docker compose command the CLI runs there pins (composeCmd) and its
// containers' com.docker.compose.project label carries: COMPOSE_PROJECT_NAME
// in .env, else the top-level name of docker-compose.yml, else the
// directory's name, lowercased to the characters compose keeps. That is the
// name `origamy deploy` gave the project. A COMPOSE_PROJECT_NAME exported in
// the shell is not taken (composeShellVars): it can name another project.
func composeProjectName(dir, envPath string) string {
	name := unquoteEnv(readEnvVar(envPath, "COMPOSE_PROJECT_NAME"))
	if b, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml")); name == "" && err == nil {
		if m := reComposeName.FindSubmatch(b); m != nil {
			name = string(m[1])
		}
	}
	if name == "" {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
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

// projectOwners is what Docker's containers say about the compose project
// the CLI pins for a project directory (composeProjectName).
type projectOwners struct {
	// pinned: a container started from this directory carries the name.
	pinned bool
	// here: the other names containers started from this directory carry (a
	// COMPOSE_PROJECT_NAME exported when they were started).
	here []string
	// elsewhere: the other directories, still there, that containers
	// carrying the name were started from (a project there has the same
	// name), "" for one whose container does not say.
	elsewhere []string
}

// composeOwners sorts the containers compose made on this Docker host, by
// their project and working-directory labels, against the project in dir
// and the name the CLI pins for it. A working directory that is gone (the
// project moved since its containers were made) is not held against it:
// compose itself goes by the name. ok is false, with what docker printed,
// when Docker could not be asked.
func composeOwners(dir, name string) (o projectOwners, out string, ok bool) {
	out, err := dockerRun(nil, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project",
		"--format", `{{.Label "com.docker.compose.project"}}`+"\t"+`{{.Label "com.docker.compose.project.working_dir"}}`)
	if err != nil {
		return o, out, false
	}
	this, _ := os.Stat(dir)
	here, elsewhere := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		project, wd, row := strings.Cut(strings.TrimRight(l, "\r"), "\t")
		if !row || project == "" {
			continue
		}
		fi, err := os.Stat(wd)
		switch fromHere := this != nil && err == nil && os.SameFile(this, fi); {
		case fromHere && project == name:
			o.pinned = true
		case fromHere:
			here[project] = true
		case project == name && (wd == "" || err == nil):
			elsewhere[wd] = true
		}
	}
	o.here, o.elsewhere = slices.Sorted(maps.Keys(here)), slices.Sorted(maps.Keys(elsewhere))
	return o, out, true
}

// otherProjectName refuses an upgrade or a deploy of the project in dir whose
// containers all run under another compose project name than the one the
// CLI pins (composeOwners): `up -d` would start a second copy of the plane
// next to the one that runs. It was started with a COMPOSE_PROJECT_NAME
// exported in the shell, which the CLI no longer takes from there; .env is
// where it belongs. Containers under both names are only warned about.
// nil when the plane runs under the pinned name, or none of it runs.
func otherProjectName(dir, envPath, nothing string) error {
	name := composeProjectName(dir, envPath)
	o, _, ok := composeOwners(dir, name)
	switch {
	case !ok || len(o.here) == 0:
		return nil
	case o.pinned:
		ui.Warn("Containers from %s also run under the compose project name %s; this run works on %s, the project's own name, and leaves them as they are.", dir, strings.Join(o.here, ", "), name)
		return nil
	}
	return fail(fmt.Sprintf("This data plane's containers run under the compose project name %s, not %s, the name every docker compose command of this CLI pins for %s (a COMPOSE_PROJECT_NAME exported in the shell when they were started; the CLI no longer takes it from there).", strings.Join(o.here, ", "), name, dir),
		"Put the name they run under in "+envPath+" (COMPOSE_PROJECT_NAME="+o.here[0]+"), then rerun. "+nothing)
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
	c := clickhouseConn{dir: dir, envPath: envPath, env: composeEnv(tag)}
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
	if out, err := dockerRun(c.env, "docker", composeCmd(dir, envPath, "up", "-d", "clickhouse")...); err != nil {
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
	out, err := dockerRun(env, "docker", composeCmd(dir, envPath, "ps", "-a", "--format", "json", "clickhouse-init")...)
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
// images of tag write. It returns what ClickHouse holds and, when that is not
// it, why ("" when it is). A tag whose schema generation is not known (main,
// production, sha-…; see unknownRelease) never passes: a run whose images
// may not write the table must not report success. A missing table is asked
// about again while the init service `up -d` started is still at work.
// runningImagesProblem checks that the images of tag are the ones that run.
func eventsTableProblem(dir, envPath, tag string) (liveSchema, string) {
	env := composeEnv(tag)
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
	case want == "unknown":
		why = "ClickHouse holds " + live.describe() + ", and whether the :" + tag + " images write it is not known: they are not a numbered release"
	case live.gen != want:
		why = "ClickHouse holds " + live.describe() + ", which the " + tag + " images cannot write"
	}
	if why != "" {
		sp.Fail("%s", why)
		return live, why
	}
	sp.Success("ClickHouse holds %s", live.describe())
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

// imageRepo is the repository of an image reference: without its tag and
// digest.
func imageRepo(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// dpService is a data-plane service as a project's compose configuration
// runs it: image is the image compose starts it from (override files and
// COMPOSE_FILE included), release the one the bundle's own compose file
// names for it, an image of dataPlaneImageRepo.
type dpService struct{ name, image, release string }

// dataPlaneRegistry is where the release's data-plane images come from.
const dataPlaneRegistry = "ghcr.io/qubelylabs/" + dataPlaneImageRepo

// bundleDataPlaneServices are the data-plane services of every compose bundle
// so far (0.1.16 to 0.1.20), each named like its image in dataPlaneRegistry:
// one of them is a data-plane service whatever image a compose file the
// operator edited gives it.
var bundleDataPlaneServices = []string{
	"ingestion-gateway", "transformer-engine", "identity-resolver", "segment-evaluator",
	"bulker-worker", "config-sync", "portal-agent", "workflow-engine", "orchestrator-engine",
}

// dataPlaneServices are the data-plane services the compose configuration of
// the project in dir runs with env (the release, tag, in DP_IMAGE_TAG; the
// profiles that are on), main in place of its docker-compose.yml (the
// refreshed one, before it is swapped in). They are told by name: every
// service whose image the compose file itself (main alone) takes from
// dataPlaneImageRepo, and every one of bundleDataPlaneServices. An override
// file, COMPOSE_FILE or an edit can take that image from elsewhere (a
// registry mirror) or pin another tag, and image is then what compose runs.
// The services of a profile that is off are not among them.
func dataPlaneServices(dir, envPath, tag string, env []string, main string) ([]dpService, error) {
	files := composeFileArgs(dir, envPath, main)
	full, err := effectiveCompose(dir, envPath, env, files)
	if err != nil {
		return nil, err
	}
	base := full
	if !slices.Equal(files, []string{"-f", main}) {
		if base, err = effectiveCompose(dir, envPath, env, []string{"-f", main}); err != nil {
			return nil, err
		}
	}
	var svcs []dpService
	for _, name := range slices.Sorted(maps.Keys(base.Services)) {
		s, on := full.Services[name]
		release := base.Services[name].Image
		if !strings.Contains(release, dataPlaneImageRepo) && slices.Contains(bundleDataPlaneServices, name) {
			release = dataPlaneRegistry + name + ":" + tag
		}
		if on && strings.Contains(release, dataPlaneImageRepo) {
			svcs = append(svcs, dpService{name: name, image: s.Image, release: release})
		}
	}
	return svcs, nil
}

// versionLabel is the org.opencontainers.image.version label in an image's
// or a container's configuration, as `{{json .Config}}` prints it. (A
// template that reads .Config.Labels itself fails on an image without
// labels: docker inspect's templates refuse a missing key.)
func versionLabel(config string) string {
	var c struct {
		Labels map[string]string `json:"Labels"`
	}
	_ = json.Unmarshal([]byte(config), &c)
	return c.Labels["org.opencontainers.image.version"]
}

// imageInfo is a local image's ID and its org.opencontainers.image.version
// label; ok is false when this host does not have it.
func imageInfo(ref string) (id, version string, ok bool) {
	out, err := dockerRun(nil, "docker", "image", "inspect", "--format", `{{.Id}}`+"\t"+`{{json .Config}}`, ref)
	id, config, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if err != nil || !strings.HasPrefix(id, "sha256:") {
		return "", "", false
	}
	return id, versionLabel(config), true
}

// releaseImages finds the image IDs of the release's own images, which a
// data-plane image from another registry (a mirror) is compared with:
// inspected, and pulled first when this host does not have one. Each is
// looked up once per check.
type releaseImages map[string]struct {
	id  string
	err error
}

func (r releaseImages) id(ref string) (string, error) {
	if got, ok := r[ref]; ok {
		return got.id, got.err
	}
	id, _, ok := imageInfo(ref)
	var err error
	if !ok {
		if out, perr := dockerRun(nil, "docker", "pull", ref); perr != nil {
			err = errors.New(orDash(lastLines(out, 1)))
		} else if id, _, ok = imageInfo(ref); !ok {
			err = errors.New("docker pull " + ref + " left no image")
		}
	}
	r[ref] = struct {
		id  string
		err error
	}{id, err}
	return id, err
}

// mirrorProblem says why ref, an image of data-plane service s that comes
// from another repository than the release's own (a registry mirror), with
// image ID id and version label version ("" for none), is not the release's
// image, "" when it is: its version label names tag, or it is the very image
// (same ID) the release's own reference names.
func (s dpService) mirrorProblem(tag, ref, id, version string, rel releaseImages) string {
	if version == tag || version == "v"+tag {
		return ""
	}
	rid, err := rel.id(s.release)
	switch {
	case err != nil:
		return s.name + " runs " + ref + ", from another registry than the release's image " + s.release + ", which could not be pulled here to compare them (" + err.Error() + ")"
	case id == "":
		return s.name + " runs " + ref + ", from another registry than the release's image " + s.release + ", and which image it is could not be read"
	case id != rid:
		return s.name + " runs " + ref + ", which is not the release's image " + s.release + " (another image, by ID)"
	}
	return ""
}

// projectContainer is a container of the project and what it runs: the
// image reference it was created from, the image it is (by ID) and that
// image's org.opencontainers.image.version label.
type projectContainer struct{ service, state, ref, image, version string }

// inspectContainers asks Docker what each container `docker compose ps`
// listed was created from: Compose before 2.15 lists no image, and one whose
// tag names another image now is listed by the image's ID.
func inspectContainers(listed []composeService) []projectContainer {
	var ids []string
	for _, s := range listed {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	inspected := map[string][]string{}
	if len(ids) > 0 {
		// A container removed since the listing fails the command but not
		// the others' lines.
		out, _ := dockerRun(nil, "docker", append([]string{"inspect", "--format",
			`{{.Id}}` + "\t" + `{{.Config.Image}}` + "\t" + `{{.Image}}` + "\t" + `{{json .Config}}`}, ids...)...)
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Split(strings.TrimRight(l, "\r"), "\t"); len(f) == 4 && f[0] != "" {
				inspected[f[0]] = f[1:]
			}
		}
	}
	var cs []projectContainer
	for _, s := range listed {
		c := projectContainer{service: s.Service, state: strings.ToLower(s.State), ref: s.Image}
		for id, f := range inspected {
			if s.ID != "" && strings.HasPrefix(id, s.ID) {
				c.ref, c.image, c.version = cmp.Or(f[0], s.Image), f[1], versionLabel(f[2])
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// runningImagesProblem is the rest of the check after the last `up -d`
// (eventsTableProblem): the containers that run are the release's. Every
// data-plane service the project's configuration runs (dataPlaneServices:
// told by name, whatever registry an override takes its image from) has a
// container, and each of its containers runs the image the configuration
// names for it, on tag; one from another registry than the release's (a
// mirror) must be the release's image itself (mirrorProblem). While
// ClickHouse holds the new events table (gen), ClickHouse runs
// minClickHouseForJSON or newer (as its image's tag says, or the server when
// the tag names no version). The services of a profile that is off are not
// checked: compose keeps their containers, which the run that turns the
// profile off stops (stopTurnedOff). What a container runs is what docker
// inspect says it was created from (inspectContainers). It returns why the
// images are not the release's, "" when they are.
func runningImagesProblem(dir, envPath, tag, gen string) string {
	sp := ui.Start("Checking the images that run")
	failed := func(why string) string {
		sp.Fail("%s", why)
		return why
	}
	env := composeEnv(tag)
	svcs, err := dataPlaneServices(dir, envPath, tag, env, filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return failed("the configuration the project runs could not be read (" + err.Error() + ")")
	}
	out, err := dockerRun(env, "docker", composeCmd(dir, envPath, "ps", "-a", "--format", "json")...)
	listed, perr := parseComposePS(composeRows(out))
	if err != nil || perr != nil {
		return failed("docker compose could not list the containers that run (" + orDash(lastLines(out, 1)) + ")")
	}
	// Only what is checked: the data-plane services and ClickHouse.
	listed = slices.DeleteFunc(listed, func(c composeService) bool {
		return c.Service != "clickhouse" && !slices.ContainsFunc(svcs, func(s dpService) bool { return s.name == c.Service })
	})
	byService := map[string][]projectContainer{}
	for _, c := range inspectContainers(listed) {
		byService[c.service] = append(byService[c.service], c)
	}
	var wrong []string
	if len(svcs) == 0 {
		wrong = append(wrong, "the project's configuration runs no data-plane service")
	}
	rel := releaseImages{}
	for _, s := range svcs {
		if len(byService[s.name]) == 0 {
			wrong = append(wrong, s.name+" has no container")
		}
		for _, c := range byService[s.name] {
			switch {
			case c.ref == "" || reImageID.MatchString(c.ref):
				wrong = append(wrong, s.name+" runs image "+orDash(c.ref)+", whose reference could not be read")
			case imageTag(c.ref) != tag:
				wrong = append(wrong, s.name+" runs "+c.ref)
			case imageRepo(c.ref) != imageRepo(s.image):
				// Not recreated from what the configuration names now.
				wrong = append(wrong, s.name+" runs "+c.ref+", and its configuration names "+s.image)
			case imageRepo(c.ref) != imageRepo(s.release):
				if why := s.mirrorProblem(tag, c.ref, c.image, c.version, rel); why != "" {
					wrong = append(wrong, why)
				}
			}
		}
	}
	clickhouse := ""
	for _, c := range byService["clickhouse"] {
		if c.state == "running" {
			clickhouse = c.ref
		}
	}
	if gen == "json" {
		needs := ", and the new events table needs ClickHouse " + minClickHouseForJSON + " or newer"
		switch v, ok := clickHouseImageVersion(clickhouse); {
		case clickhouse == "":
			wrong = append(wrong, "ClickHouse is not among the containers that run")
		case !ok:
			// A tag that names no version (latest, a registry's own name):
			// the server itself says which it is.
			out, err := clickhouseConn{dir: dir, envPath: envPath, env: env}.exec(clickhouseServerVersion)
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

// plannedImagesProblem is the first half of runningImagesProblem, made before
// an upgrade's commit point: the data-plane services the project's
// configuration starts once refresh r is in (its compose file in place of
// docker-compose.yml when it replaces it), with the run's environment (env:
// the release, the AI toggle's profiles), are on tag, and an image from
// another registry than the release's (a mirror), which pullImages just
// pulled, is the release's image itself (mirrorProblem). It returns why not,
// "" when they are.
func plannedImagesProblem(dir, envPath, tag string, env []string, r *bundleRefresh) string {
	main := filepath.Join(dir, "docker-compose.yml")
	if r != nil && r.changes("docker-compose.yml") {
		main = filepath.Join(r.tmp, "docker-compose.yml")
	}
	sp := ui.Start("Checking the images the %s services start from", tag)
	svcs, err := dataPlaneServices(dir, envPath, tag, env, main)
	if err != nil {
		why := "the configuration it would start could not be read (" + err.Error() + ")"
		sp.Fail("%s", why)
		return why
	}
	var wrong []string
	rel := releaseImages{}
	for _, s := range svcs {
		switch {
		case imageTag(s.image) != tag:
			wrong = append(wrong, s.name+" is set to "+s.image)
		case imageRepo(s.image) != imageRepo(s.release):
			id, version, _ := imageInfo(s.image)
			if why := s.mirrorProblem(tag, s.image, id, version, rel); why != "" {
				wrong = append(wrong, why)
			}
		}
	}
	if len(svcs) == 0 {
		wrong = append(wrong, "it runs no data-plane service")
	}
	if len(wrong) > 0 {
		sp.Fail("Not every data-plane service would run the %s images", tag)
		return strings.Join(wrong, "; ")
	}
	sp.Success("Every data-plane service starts from the %s images", tag)
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
	return append(causes, "A compose file can pin another image: docker-compose.yml, an override file or one COMPOSE_FILE in .env names (`"+composeHint(dir, envPath)+" config --images` lists what compose starts). An image from a registry mirror must be the release's very image: `docker pull` its "+dataPlaneRegistry+" reference on this host so the two can be compared.")
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
// dir with env (composeEnv: the release in DP_IMAGE_TAG, and what else the
// run gives compose): the files in fileArgs (composeFileArgs, so override
// files and COMPOSE_FILE count), interpolated with envPath. Only the
// services of the profiles that are on are in it.
func effectiveCompose(dir, envPath string, env []string, fileArgs []string) (composeConfig, error) {
	out, err := dockerRun(env, "docker", composeCmd(dir, envPath, append(slices.Clone(fileArgs), "config", "--format", "json")...)...)
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
// compose environment (composeEnv).
func composeLogs(dir, envPath string, env []string, service string, n int) string {
	out, _ := dockerRun(env, "docker", composeCmd(dir, envPath, "logs", "--no-color", "--tail", fmt.Sprintf("%d", n), service)...)
	return strings.TrimSpace(out)
}

// reServiceName is a line of `docker compose config --services` (compose's
// warnings, on the same output, have spaces).
var reServiceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// profileServices lists the services the compose configuration of the
// project in dir runs with profiles on (COMPOSE_PROFILES over env's).
func profileServices(dir, envPath string, env, profiles []string) ([]string, error) {
	out, err := dockerRun(append(slices.Clone(env), "COMPOSE_PROFILES="+strings.Join(profiles, ",")), "docker", composeCmd(dir, envPath, "config", "--services")...)
	if err != nil {
		return nil, errors.New(orDash(lastLines(out, 1)))
	}
	var svcs []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); reServiceName.MatchString(l) {
			svcs = append(svcs, l)
		}
	}
	return svcs, nil
}

// stopTurnedOff stops and removes the containers of the services only the
// compose profiles a run turned off ran (in before, not in after:
// COMPOSE_PROFILES before and after the run). compose keeps the services of
// a profile that is off in the project, so `up -d --remove-orphans` leaves
// their containers running on whatever release they were started with. A
// service a profile still on runs (dp-postgres, with full or agentic) is
// kept. It runs once .env holds after (the commit point), before `up -d`;
// env is the run's compose environment (composeEnv). What could not be
// stopped is warned about, with the command that does it: the check after
// `up -d` covers only the services of the profiles that are on, so it does
// not fail the run.
func stopTurnedOff(dir, envPath string, env, before, after []string) {
	var off []string
	for _, p := range before {
		if !hasProfile(after, p) {
			off = append(off, p)
		}
	}
	if len(off) == 0 {
		return
	}
	stop := func(svcs ...string) string {
		return "COMPOSE_PROFILES=" + strings.Join(before, ",") + " " + composeHint(dir, envPath) + " rm --stop --force " + strings.Join(svcs, " ")
	}
	was, err := profileServices(dir, envPath, env, before)
	var now []string
	if err == nil {
		now, err = profileServices(dir, envPath, env, after)
	}
	if err != nil {
		ui.Warn("Could not tell which services the %s profile ran (%v); their containers keep running on the release they were started with. Stop them yourself: %s <service…>", strings.Join(off, ", "), err, stop())
		return
	}
	var gone []string
	for _, s := range was {
		if !slices.Contains(now, s) {
			gone = append(gone, s)
		}
	}
	if len(gone) == 0 {
		return
	}
	sp := ui.Start("Stopping %s (the %s profile is off now)", strings.Join(gone, ", "), strings.Join(off, ", "))
	// Named, with the profiles that ran them on, so every compose version
	// knows them.
	args := append([]string{"rm", "--stop", "--force"}, gone...)
	if out, err := dockerRun(append(slices.Clone(env), "COMPOSE_PROFILES="+strings.Join(before, ",")), "docker", composeCmd(dir, envPath, args...)...); err != nil {
		sp.Warn("Could not stop %s (%s); they keep running on the release they were started with. Stop them yourself: %s", strings.Join(gone, ", "), orDash(lastLines(out, 1)), stop(gone...))
		return
	}
	sp.Success("Stopped and removed %s", strings.Join(gone, ", "))
}
