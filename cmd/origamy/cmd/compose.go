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

// liveSchemaGeneration asks the running ClickHouse of a compose project which
// generation its events table is ("json", "string"), "" when the container is
// not running or the table does not exist yet. The served init SQL says what
// the control plane would create; this says what the volume already holds.
func liveSchemaGeneration(dir, envPath string) string {
	out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "exec", "-T", "clickhouse", "sh", "-c",
		`clickhouse-client --password "$CH_DEFAULT_PASSWORD" --query "SELECT type FROM system.columns WHERE database = 'events' AND table = 'events' AND name = 'properties'"`)
	if err != nil {
		return ""
	}
	switch t := strings.TrimSpace(out); {
	case strings.HasPrefix(t, "JSON"):
		return "json"
	case strings.HasPrefix(t, "String"):
		return "string"
	}
	return ""
}

// composeLogs returns the last n log lines of one service.
func composeLogs(dir, envPath, service string, n int) string {
	out, _ := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath,
		"logs", "--no-color", "--tail", fmt.Sprintf("%d", n), service)
	return strings.TrimSpace(out)
}
