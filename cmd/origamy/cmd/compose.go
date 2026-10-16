package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// would miss exactly that.
const composeSettle = 30 * time.Second

// waitForCompose polls the project for up to ~3 minutes. It returns healthy
// when every service has been running (and healthy, where it has a check)
// for composeSettle, or the failed services as soon as one exits or starts
// restarting.
func waitForCompose(dir, envPath string, sp *ui.Spinner) (healthy bool, failed []string) {
	deadline := time.Now().Add(3 * time.Minute)
	var stableSince time.Time
	for {
		out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "ps", "-a", "--format", "json")
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
		time.Sleep(3 * time.Second)
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

// liveSchemaGeneration is probeLiveSchema's generation, "" unless ClickHouse
// holds one of the two (not running, no events table, another type).
func liveSchemaGeneration(dir, envPath string) string {
	if l := probeLiveSchema(dir, envPath, nil); l.known() {
		return l.gen
	}
	return ""
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
	Name     string `json:"name"`
	Services map[string]struct {
		Image   string `json:"image"`
		Volumes []struct {
			Type   string `json:"type"`
			Source string `json:"source"`
		} `json:"volumes"`
	} `json:"services"`
	Volumes map[string]struct {
		Name string `json:"name"`
	} `json:"volumes"`
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

// serviceVolumes names the Docker volumes service keeps its data in, as the
// configuration resolves them.
func (c composeConfig) serviceVolumes(service string) []string {
	var out []string
	for _, v := range c.Services[service].Volumes {
		if v.Type != "volume" || v.Source == "" {
			continue
		}
		name := c.Volumes[v.Source].Name
		if name == "" {
			name = c.Name + "_" + v.Source
		}
		out = append(out, name)
	}
	return out
}

// existingVolumes keeps the volumes Docker has.
func existingVolumes(names []string) []string {
	var out []string
	for _, n := range names {
		if _, err := dockerRun(nil, "docker", "volume", "inspect", n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// composeLogs returns the last n log lines of one service.
func composeLogs(dir, envPath, service string, n int) string {
	out, _ := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath,
		"logs", "--no-color", "--tail", fmt.Sprintf("%d", n), service)
	return strings.TrimSpace(out)
}
