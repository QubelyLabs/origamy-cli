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
	return dotenvValue(filepath.Join(dir, ".env"), "DATA_PLANE_ID")
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
func (c clickhouseConn) wait() (out string, ok bool) { return c.waitFor(clickhouseReadyTimeout) }

// waitFor is wait for up to d.
func (c clickhouseConn) waitFor(d time.Duration) (out string, ok bool) {
	deadline := time.Now().Add(d)
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
	name := dotenvValue(envPath, "COMPOSE_PROJECT_NAME")
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
	return normalizeProjectName(name)
}

// normalizeProjectName is a compose project name as compose spells it:
// lowercased to the characters it keeps.
func normalizeProjectName(name string) string {
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
// where it belongs. Containers under both names are only warned about. A
// plane none of whose containers exist (a `docker compose down`) while its
// data volumes do, under another name, is refused too (strandedPlane):
// going on would start a new, empty plane beside them.
// nil when the plane runs under the pinned name, or it exists under no other.
func otherProjectName(dir, envPath, nothing string) error {
	name := composeProjectName(dir, envPath)
	o, rows, ok := composeOwners(dir, name)
	switch {
	case !ok:
		return nil
	case len(o.here) == 0 && !o.pinned && len(o.elsewhere) == 0:
		return strandedPlane(dir, name, rows, nothing)
	case len(o.here) == 0:
		return nil
	case o.pinned:
		ui.Warn("Containers from %s also run under the compose project name %s; this run works on %s, the project's own name, and leaves them as they are.", dir, strings.Join(o.here, ", "), name)
		return nil
	}
	return fail(fmt.Sprintf("This data plane's containers run under the compose project name %s, not %s, the name every docker compose command of this CLI pins for %s (a COMPOSE_PROJECT_NAME exported in the shell when they were started; the CLI no longer takes it from there).", strings.Join(o.here, ", "), name, dir),
		"Put the name they run under in "+envPath+" (COMPOSE_PROJECT_NAME="+o.here[0]+"), then rerun. "+nothing)
}

// composeVolume is a named volume compose made: its Docker name, the
// compose project (com.docker.compose.project) and the key of the compose
// file's volumes: (com.docker.compose.volume) it carries.
type composeVolume struct{ name, project, key string }

// planeVolumeKeys are the volumes: of the bundle that hold the plane's data.
var planeVolumeKeys = []string{"clickhouse-data", "dp-redis-data", "nats-data", "dp-postgres-data"}

// composeVolumes lists the volumes compose made on this Docker host; out is
// what docker printed when ok is false.
func composeVolumes() (vols []composeVolume, out string, ok bool) {
	out, err := dockerRun(nil, "docker", "volume", "ls", "--filter", "label=com.docker.compose.project", "--format",
		`{{.Name}}`+"\t"+`{{.Label "com.docker.compose.project"}}`+"\t"+`{{.Label "com.docker.compose.volume"}}`)
	if err != nil {
		return nil, out, false
	}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Split(strings.TrimRight(l, "\r"), "\t"); len(f) == 3 && f[0] != "" {
			vols = append(vols, composeVolume{name: f[0], project: f[1], key: f[2]})
		}
	}
	return vols, out, true
}

// planeVolumes sorts the data volumes compose made by project against the
// plane in dir, pinned as name: mine are the ones under name, others the
// ones of a plane like it under other project names, by name, that is not
// running from another directory that still exists (rows: composeOwners'
// containers). ok is false when Docker could not list them.
func planeVolumes(dir, name, rows string) (mine []string, others map[string][]string, ok bool) {
	vols, _, ok := composeVolumes()
	if !ok {
		return nil, nil, false
	}
	this, _ := os.Stat(dir)
	alive := map[string]bool{}
	for _, l := range strings.Split(rows, "\n") {
		project, wd, row := strings.Cut(strings.TrimRight(l, "\r"), "\t")
		if fi, err := os.Stat(wd); row && wd != "" && err == nil && !(this != nil && os.SameFile(this, fi)) {
			alive[project] = true
		}
	}
	others = map[string][]string{}
	for _, v := range vols {
		switch {
		case v.project == name:
			mine = append(mine, v.name)
		case v.project != "" && slices.Contains(planeVolumeKeys, v.key) && !alive[v.project]:
			others[v.project] = append(others[v.project], v.name)
		}
	}
	return mine, others, true
}

// otherVolumesList writes the volumes of planeVolumes' others as
// "volume, volume (project X)" for a message, the project the shell exports
// first, and the project to suggest.
func otherVolumesList(others map[string][]string) (list, suggest string) {
	projects := slices.Sorted(maps.Keys(others))
	shell := normalizeProjectName(os.Getenv("COMPOSE_PROJECT_NAME"))
	slices.SortStableFunc(projects, func(a, b string) int {
		return cmp.Compare(map[bool]int{true: 0, false: 1}[a == shell], map[bool]int{true: 0, false: 1}[b == shell])
	})
	var parts []string
	for _, p := range projects {
		slices.Sort(others[p])
		parts = append(parts, strings.Join(others[p], ", ")+" (compose project "+p+")")
	}
	if len(projects) > 0 {
		suggest = projects[0]
	}
	return strings.Join(parts, "; "), suggest
}

// strandedPlane refuses to go on with a plane that has no container and no
// volume under the compose project name the CLI pins for it (name) while
// the data volumes of a plane like it exist under another name: it ran with
// a COMPOSE_PROJECT_NAME exported in the shell (or a -p), and was taken down.
// `up -d` under the pinned name would create a new, empty plane (network,
// volumes, a ClickHouse without the history) and report the upgrade as done.
// Nothing has been created when it refuses. rows are composeOwners'
// containers. nil when the plane has volumes of its own, or there are none to
// mistake for them.
func strandedPlane(dir, name, rows, nothing string) error {
	mine, others, ok := planeVolumes(dir, name, rows)
	if !ok || len(mine) > 0 || len(others) == 0 {
		return nil
	}
	list, suggest := otherVolumesList(others)
	shell := ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" && normalizeProjectName(v) != name {
		shell = " Your shell exports COMPOSE_PROJECT_NAME=" + v + ", which this CLI ignores."
	}
	return fail(fmt.Sprintf("Nothing of this data plane exists under the compose project name %s, the name every docker compose command of this CLI pins for %s, but the data volumes of a plane like it do under another: %s.%s Going on would start a new, empty plane (a ClickHouse without the history) next to them.", name, dir, list, shell),
		"If they are this plane's (it ran under that name, with COMPOSE_PROJECT_NAME exported in your shell or a -p), put the name in "+filepath.Join(dir, ".env")+" (COMPOSE_PROJECT_NAME="+suggest+"), then rerun. If the shell's variable names another project, unset it (unset COMPOSE_PROJECT_NAME), and if this plane has no data to keep, start it under its own name first (`docker compose -p "+name+" --project-directory "+dir+" up -d`), then rerun. "+nothing)
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
// strict is a move that can touch the events table (canTouchEventsTable),
// which waits clickhouseReadyTimeout for ClickHouse; any other waits
// advisoryWait, and a ClickHouse that does not answer (gen "") is a warning
// in the output rather than a failure of the spinner: the caller goes on.
func eventsTableProblem(dir, envPath, tag string, strict bool) (liveSchema, string) {
	env := composeEnv(tag)
	wait := clickhouseReadyTimeout
	if !strict {
		wait = advisoryWait
	}
	sp := ui.Start("Checking the events table the %s images write to", tag)
	unasked := func(live liveSchema, why string) (liveSchema, string) {
		if strict {
			sp.Fail("%s", why)
		} else {
			sp.Warn("%s", why)
		}
		return live, why
	}
	if out, ok := (clickhouseConn{dir: dir, envPath: envPath, env: env}).waitFor(wait); !ok {
		return unasked(liveSchema{out: out}, "ClickHouse did not answer within "+wait.String()+", so whether "+eventsTable+" is there could not be checked")
	}
	deadline := time.Now().Add(wait)
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
		return unasked(live, "ClickHouse did not say which events table it holds")
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

// advisoryWait is how long the check after `up -d` of a move that cannot
// touch the events table waits for ClickHouse: the move changes nothing in
// it, so the check is a courtesy, not a gate. A variable so tests need not
// wait.
var advisoryWait = 30 * time.Second

// canTouchEventsTable reports whether moving a Docker project from the
// release installed to tag can touch the events table: a storage reset is
// recorded (pending), or the two releases write different events tables (a
// move across the reset, up or down), or either is no numbered release, so
// which table its images write is not known. Only such a move gets the heavy
// checks (what ClickHouse holds, asked and started; the images that start and
// the ones that run, every container of the project, compared with the
// release's by ID). Any other move (0.1.19 → 0.1.20, a rollback within a
// generation, an in-place deploy of the release a plane runs) behaves like
// CLI v0.1.20 did, but for cheap checks that never refuse and never start
// anything: it escalates to the heavy path when ClickHouse, peeked at,
// turns out to hold another generation than the release writes.
func canTouchEventsTable(installed, tag, pending string) bool {
	if pending != "" {
		return true
	}
	from, to := releaseSchemaGeneration(installed), releaseSchemaGeneration(tag)
	return from == "unknown" || to == "unknown" || from != to
}

// peekClickHouse is askClickHouse without a side effect: it asks the
// project's ClickHouse which events table it holds, through compose or, when
// compose cannot load the project, through its container if that runs, and
// starts nothing. gen stays "" when it could not be asked.
func peekClickHouse(dir, envPath, tag string) liveSchema {
	live := clickhouseConn{dir: dir, envPath: envPath, env: composeEnv(tag)}.probe()
	if live.gen != "" {
		return live
	}
	if id, running := clickhouseContainer(dir, envPath); running {
		if l := (clickhouseConn{id: id}).probe(); l.gen != "" {
			ui.Step("Asked ClickHouse through its container (%s), which docker compose could not reach in this project (%s).", id, orDash(lastLines(live.out, 1)))
			return l
		}
	}
	return live
}

// checkAfterUp is the check after the last `up -d` of an upgrade or an
// in-place deploy to tag: the run succeeds only when the events table the
// services now run on is the one the release writes, and, on a move that can
// touch it (strict), the images that run are the release's. A table of
// another generation, or none, fails any move (and is recorded as a storage
// reset still to do: eventsTableFail). A ClickHouse that cannot be asked, and
// the images, fail a strict move only; any other move, which cannot have
// changed either, is told of it in a warning.
func checkAfterUp(dir, envPath, tag string, strict bool) error {
	got, why := eventsTableProblem(dir, envPath, tag, strict)
	switch {
	case why == "":
	case strict || got.gen != "":
		return eventsTableFail(dir, envPath, tag, got, why)
	default:
		ui.Warn("Whether %s holds the table the %s images write was not checked: this move does not change which table they write.", eventsTable, tag)
		ui.Detail("Check: %s", composeHint(dir, envPath)+" exec clickhouse sh -c '"+clickhouseEventsPropertiesType+"'")
	}
	if strict {
		if why := runningImagesProblem(dir, envPath, tag, got.gen); why != "" {
			return runningImagesFail(dir, envPath, tag, why)
		}
		return nil
	}
	notes, wrongGen := advisoryImageNotes(dir, envPath, tag)
	if len(wrongGen) > 0 {
		return runningImagesFail(dir, envPath, tag, "not every container runs what the "+tag+" release does: "+strings.Join(wrongGen, "; ")+" (a release that writes the other events table than ClickHouse holds)")
	}
	for _, n := range notes {
		ui.Warn("%s, not the %s release; an override file or an edit of docker-compose.yml pins it. This move does not change which events table the images write, so the run goes on.", n, tag)
	}
	return nil
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
// images than tag's, for the failure runningImagesProblem (or the planned
// check) reports, why. The advice on registry mirrors and digests comes only
// with a failure that is about comparing an image with the release's.
func runningImagesCauses(dir, envPath, tag, why string) []string {
	var causes []string
	if v := os.Getenv("DP_IMAGE_TAG"); v != "" && v != tag {
		causes = append(causes, "DP_IMAGE_TAG="+v+" is exported in your shell, and docker compose takes it over .env: a `docker compose up` run from this shell starts "+v+". Unset it (unset DP_IMAGE_TAG).")
	}
	if n := envKeyCount(envPath, "DP_IMAGE_TAG"); n > 1 {
		causes = append(causes, fmt.Sprintf(".env has %d DP_IMAGE_TAG lines, and docker compose takes the last (%s): keep one.", n, dotenvValue(envPath, "DP_IMAGE_TAG")))
	}
	causes = append(causes, "A compose file can pin another image, or add a service that runs one: docker-compose.yml, an override file or one COMPOSE_FILE in .env names (`"+composeHint(dir, envPath)+" config --images` lists what compose starts, `docker ps -a --filter label=com.docker.compose.project="+composeProjectName(dir, envPath)+"` the containers of the project). Every data-plane image must be the release's: on its tag, or pinned by the digest of the release's image.")
	if !strings.Contains(why, "release's image") && !strings.Contains(why, "pinned by digest") {
		return causes
	}
	return append(causes,
		"An image from a registry mirror is accepted only when it is the release's very image, which the CLI checks against "+dataPlaneRegistry+"<service>:"+tag+" on this host (pulling that reference when the host does not have it). A host that cannot reach ghcr.io cannot make the comparison, and the move is refused before anything is changed. Put the release's image on this host under its ghcr.io reference: `docker pull` it where ghcr.io is reachable and carry it here (`docker save` | `docker load`), or, once you have checked that the mirror's image is the release's (its digest is the one ghcr.io has for "+tag+"), `docker tag <the mirror's image> "+dataPlaneRegistry+"<service>:"+tag+"`. Then rerun.")
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
	Services map[string]composeServiceConfig `json:"services"`
	Volumes  map[string]json.RawMessage      `json:"volumes"`
}

// composeServiceConfig is a service of composeConfig: its image and how many
// containers the configuration runs (scale, deploy.replicas; nil for the
// default, one).
type composeServiceConfig struct {
	Image  string `json:"image"`
	Scale  *int   `json:"scale"`
	Deploy struct {
		Replicas *int `json:"replicas"`
	} `json:"deploy"`
}

// scaledDown reports whether the configuration runs the service nowhere
// (scale: 0, or deploy.replicas: 0): it has no container, on purpose.
func (s composeServiceConfig) scaledDown() bool {
	n := cmp.Or(s.Scale, s.Deploy.Replicas)
	return n != nil && *n == 0
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

// removeInactiveDataPlane removes the containers of the data-plane services
// whose compose profiles are all off (active: the profiles that are on),
// before the `up -d` of a move that can touch the events table. compose
// leaves them be: `up -d --remove-orphans` keeps the containers of a
// profile that is off, on whatever release they were started with, whoever
// turned the profile off (a hand edit of COMPOSE_PROFILES, an earlier run
// whose own removal failed, a fresh deploy over an old plane's containers).
// A leftover writer on an older release would run beside the new one and
// against the new events table; the check after `up -d` would fail the run
// on it, and a run must not report every service on the release while one is
// not. The services of the profiles that are on are untouched, and so are
// the volumes. A failure is a warning, with the command: the check after
// `up -d` finds what is left. env is the run's compose environment
// (composeEnv).
func removeInactiveDataPlane(dir, envPath string, env, active []string) {
	all := append(slices.Clone(env), "COMPOSE_PROFILES=*")
	every, err := effectiveCompose(dir, envPath, all, nil)
	var on composeConfig
	if err == nil {
		on, err = effectiveCompose(dir, envPath, append(slices.Clone(env), "COMPOSE_PROFILES="+strings.Join(active, ",")), nil)
	}
	var listed []composeService
	if err == nil {
		listed, _, err = projectContainers(composeProjectName(dir, envPath))
	}
	if err != nil {
		ui.Warn("Could not tell whether containers of data-plane services whose profile is off are left over (%v); the check after the restart finds any that run.", err)
		return
	}
	var left []string
	for _, c := range listed {
		s, known := every.Services[c.Service]
		_, isOn := on.Services[c.Service]
		if known && !isOn && dataPlaneImageName(c.Service, s.Image, "") != "" && !slices.Contains(left, c.Service) {
			left = append(left, c.Service)
		}
	}
	if len(left) == 0 {
		return
	}
	slices.Sort(left)
	sp := ui.Start("Removing %s (left over from a profile that is off)", strings.Join(left, ", "))
	args := append([]string{"rm", "--stop", "--force"}, left...)
	if out, err := dockerRun(all, "docker", composeCmd(dir, envPath, args...)...); err != nil {
		sp.Warn("Could not remove %s (%s); they keep running on the release they were started with. Remove them yourself: COMPOSE_PROFILES='*' %s rm --stop --force %s", strings.Join(left, ", "), orDash(lastLines(out, 1)), composeHint(dir, envPath), strings.Join(left, " "))
		return
	}
	sp.Success("Removed %s (their profile is off; they would have kept running on the release they were started with)", strings.Join(left, ", "))
}
