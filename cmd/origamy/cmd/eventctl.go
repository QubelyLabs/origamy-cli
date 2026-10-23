package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── eventctl inside the data plane ───────────────────────────────────────────
// `origamy dlq` and `origamy doctor` do not reach NATS, ClickHouse or Redis
// themselves: the data plane's datastores stay inside the customer's network
// and their passwords stay in its Secrets and .env. Instead they exec eventctl
// in the portal-agent container (shipped there from data-plane 0.1.24), which
// reads the connection settings from that container's own env and prints one
// JSON report. Only that report crosses the exec, and it carries no payload
// unless the operator asks for one.

// eventctlService is the compose service, and eventctlDeployment the
// Kubernetes Deployment, whose container carries eventctl.
const (
	eventctlService    = "portal-agent"
	eventctlDeployment = release + "-portal-agent"
)

// eventctlSchema is the newest report schema this CLI reads.
const eventctlSchema = 1

// eventctlRun runs a command and returns its stdout and stderr apart: the
// report is stdout, and kubectl's or compose's warnings land on stderr. A
// variable so tests can stand in for the data plane.
var eventctlRun = runSplitTimeout

// runSplitTimeout runs a command with extra in its environment (see
// commandEnv), within d, keeping stdout and stderr apart.
func runSplitTimeout(d time.Duration, extraEnv []string, name string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var out, errb bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Env = commandEnv(extraEnv)
	c.Stdout = &out
	c.Stderr = &errb
	c.WaitDelay = 2 * time.Second
	err = c.Run()
	if ctx.Err() != nil {
		return out.String(), errb.String(), fmt.Errorf("no answer within %s", d)
	}
	return out.String(), errb.String(), err
}

// dataPlane is the install a diagnostic command runs eventctl in.
type dataPlane struct {
	kind installKind
	// Docker: the compose project.
	dir, envPath string
}

// target names the install for the intro block.
func (p dataPlane) target() string {
	if p.kind == installKubernetes {
		return "Kubernetes (namespace " + namespace + ")"
	}
	return "Docker (" + p.dir + ")"
}

// locateDataPlane finds the install command acts on, as upgrade and status
// do. The error is already printed. A variable so tests can name the install.
var locateDataPlane = findDataPlane

func findDataPlane(command string) (dataPlane, error) {
	kind, err := locateInstall(command)
	if err != nil {
		return dataPlane{}, err
	}
	switch kind {
	case installKubernetes:
		return dataPlane{kind: kind}, nil
	case installDocker:
		dir, err := composeProjectDir()
		if err != nil {
			return dataPlane{}, fail("No Origamy compose project found here.",
				err.Error()+"\ncd into your data-plane directory (e.g. ./origamy-dp-<id>) and retry.")
		}
		return dataPlane{kind: kind, dir: dir, envPath: filepath.Join(dir, ".env")}, nil
	}
	return dataPlane{}, fail("No Origamy data plane found on this machine.",
		"Point kubectl at the cluster the data plane runs on, or cd into its Docker directory (e.g. ./origamy-dp-<id>), then rerun `"+command+"`.")
}

// eventctlCommand is the command line that runs eventctl with args in p.
func (p dataPlane) eventctlCommand(args ...string) (env []string, name string, argv []string) {
	if p.kind == installKubernetes {
		return nil, "kubectl", append([]string{"exec", "-n", namespace, "deploy/" + eventctlDeployment, "-c", eventctlService, "--", "eventctl"}, args...)
	}
	env = composeEnv(dotenvValue(p.envPath, "DP_IMAGE_TAG"))
	return env, "docker", composeCmd(p.dir, p.envPath, append([]string{"exec", "-T", eventctlService, "eventctl"}, args...)...)
}

// eventctlError is why eventctl could not produce a report; headline and
// hint are for fail.
type eventctlError struct{ headline, hint string }

func (e *eventctlError) Error() string { return e.headline }

// runEventctl runs eventctl with args in p and decodes its JSON report
// into v. A report that carries an error (eventctl ran, but could not reach
// NATS) still decodes: the caller reads the error field. err is an
// *eventctlError when no report came back.
func (p dataPlane) runEventctl(d time.Duration, v any, args ...string) error {
	env, name, argv := p.eventctlCommand(args...)
	stdout, stderr, runErr := eventctlRun(d, env, name, argv...)
	line := lastJSONLine(stdout)
	if line == "" {
		return p.explainNoReport(stdout, stderr, runErr)
	}
	var head struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		return &eventctlError{"The data plane's eventctl printed a report this CLI cannot read.", err.Error()}
	}
	if head.Schema > eventctlSchema {
		return &eventctlError{"This data plane is newer than the CLI.",
			fmt.Sprintf("Its eventctl reports schema %d; this CLI reads up to %d. Update the CLI (curl -fsSL https://origamy.io/install.sh | sh) and rerun.", head.Schema, eventctlSchema)}
	}
	if err := json.Unmarshal([]byte(line), v); err != nil {
		return &eventctlError{"The data plane's eventctl printed a report this CLI cannot read.", err.Error()}
	}
	return nil
}

// lastJSONLine is the last stdout line that is a JSON object.
func lastJSONLine(stdout string) string {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

// explainNoReport turns a run that printed no report into what to do.
func (p dataPlane) explainNoReport(stdout, stderr string, runErr error) error {
	out := strings.TrimSpace(stderr + "\n" + stdout)
	low := strings.ToLower(out)
	upgrade := "Update this data plane (`origamy upgrade`) to a release that ships eventctl in its portal-agent image (data plane 0.1.24 or later), then rerun."
	switch {
	case strings.Contains(low, "executable file not found") || strings.Contains(low, "eventctl: not found") ||
		strings.Contains(low, "unknown command: doctor") || strings.Contains(low, "flag provided but not defined: -json"):
		return &eventctlError{"This data plane is too old for this command: its portal-agent has no (or an older) eventctl.", upgrade}
	case p.kind == installKubernetes && strings.Contains(low, "not found") && strings.Contains(low, eventctlDeployment):
		return &eventctlError{"The data plane has no portal-agent Deployment (" + eventctlDeployment + ").",
			"eventctl runs in the portal-agent. Turn it back on (`origamy upgrade --set portalAgent.enabled=true`), then rerun."}
	case p.kind == installKubernetes && (strings.Contains(low, "forbidden") && strings.Contains(low, "pods/exec")):
		return &eventctlError{"Your Kubernetes user may not exec into the data plane's pods.",
			"This command runs `kubectl exec -n " + namespace + " deploy/" + eventctlDeployment + " -- eventctl …`; it needs the pods/exec permission in " + namespace + ".\n" + lastLines(out, 3)}
	case p.kind == installDocker && (strings.Contains(low, "is not running") || strings.Contains(low, "no such service") || strings.Contains(low, "no container found")):
		return &eventctlError{"The data plane's portal-agent container is not running.",
			"Start it (" + composeHint(p.dir, p.envPath) + " up -d portal-agent) or check `origamy status`, then rerun."}
	case runErr != nil && errors.Is(runErr, exec.ErrNotFound):
		tool := "kubectl"
		if p.kind == installDocker {
			tool = "docker"
		}
		return &eventctlError{tool + " is not installed or not on PATH.", "Install it, then rerun."}
	}
	hint := "eventctl printed no report."
	if runErr != nil {
		hint = "It failed: " + runErr.Error() + "."
	}
	if tail := lastLines(out, 6); tail != "" {
		hint += "\n" + tail
	}
	return &eventctlError{"Could not run eventctl in the data plane.", hint}
}

// failEventctl prints err (from runEventctl) and returns errSilent.
func failEventctl(err error) error {
	var e *eventctlError
	if errors.As(err, &e) {
		return fail(e.headline, e.hint)
	}
	return fail("Could not run eventctl in the data plane.", err.Error())
}
