package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ── Which install a command acts on ──────────────────────────────────────────
// upgrade, status and rollback act on the Kubernetes release when kubectl can
// reach a cluster that has it, else on the Docker compose project here. A
// cluster kubectl is configured for but cannot reach is reported as such,
// unless there is a compose project here to act on instead: falling through
// to Docker then would only say "no compose project found", which hides why
// the Kubernetes install was not found (an expired cloud login, a VPN that
// is down, the wrong context).

// installKind is what locateInstall found.
type installKind int

const (
	installNone installKind = iota
	installKubernetes
	installDocker
)

// kubeProbeTimeout bounds each question locateInstall asks the cluster. An
// exec credential plugin (aws eks get-token, gke-gcloud-auth-plugin) that
// cannot sign in can keep kubectl busy well past its --request-timeout.
var kubeProbeTimeout = 30 * time.Second

// dockerAvailable reports whether Docker answers here; a variable so tests
// can stand in for it.
var dockerAvailable = hasDocker

// kubeState is what kubectl and helm said about the cluster.
type kubeState struct {
	// configured: kubectl is installed and has a current context (or runs in
	// a pod). Without one kubectl falls back to localhost:8080, which says
	// nothing about where a data plane might be.
	configured bool
	context    string
	// reachable: the cluster answered; installed: it has the release.
	reachable, installed bool
	// problem is why the cluster could not be asked ("" when it answered).
	problem string
}

// probeKube asks kubectl and helm whether the release is installed, keeping
// why when they could not tell.
func probeKube() kubeState {
	var k kubeState
	if _, err := exec.LookPath("kubectl"); err != nil {
		return k
	}
	if out, err := kubeRunTimeout(kubeProbeTimeout, "kubectl", "config", "current-context"); err == nil {
		k.context = strings.TrimSpace(out)
	}
	k.configured = k.context != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != ""
	if !k.configured {
		return k
	}
	if out, err := kubeRunTimeout(kubeProbeTimeout, "kubectl", "cluster-info", "--request-timeout=10s"); err != nil {
		k.problem = probeProblem(out, err)
		return k
	}
	k.reachable = true
	out, err := kubeRunTimeout(kubeProbeTimeout, "helm", "status", release, "-n", namespace)
	switch {
	case err == nil:
		k.installed = true
	case !strings.Contains(strings.ToLower(out), "release: not found"):
		k.problem = probeProblem(out, err)
	}
	return k
}

// probeProblem is the tail of a failed probe's output, or its error when it
// printed nothing. kubectl's own log lines (E1023 12:00:00 … memcache.go)
// and its "cluster-info dump" advice are left out: they repeat or bury the
// error it ends with.
func probeProblem(out string, err error) string {
	var kept []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" || reKlogLine.MatchString(l) || strings.Contains(l, "kubectl cluster-info dump") {
			continue
		}
		kept = append(kept, l)
	}
	if tail := lastLines(strings.Join(kept, "\n"), 3); tail != "" {
		return tail
	}
	return err.Error()
}

var reKlogLine = regexp.MustCompile(`^[IWEF]\d{4} \d`)

// locateInstall picks the install command (e.g. "origamy upgrade") acts on.
// It returns an error (already printed) when it can tell there is one but
// not reach it, and installNone when there is none to tell of: the caller
// says that in its own words.
func locateInstall(command string) (installKind, error) {
	k := probeKube()
	if k.installed {
		return installKubernetes, nil
	}
	docker := dockerAvailable()
	_, projectErr := composeProjectDir()
	// A compose project here (or several, which the Docker path explains)
	// is what to act on whatever the cluster said.
	if docker && !errors.Is(projectErr, errNoProject) {
		return installDocker, nil
	}
	where := "kubectl context " + quoteContext(k.context)
	switch {
	case k.problem != "" && !k.reachable:
		return installNone, fail(fmt.Sprintf("Could not reach your Kubernetes cluster (%s).", where),
			k.problem+"\n"+
				"If kubectl signs in through a cloud CLI (EKS, GKE, AKS), refresh that login (e.g. `aws sso login`) or your VPN, check `kubectl cluster-info`, then rerun `"+command+"`.\n"+
				"If this data plane runs on Docker instead, cd into its directory (e.g. ./origamy-dp-<id>) and retry.")
	case k.problem != "":
		return installNone, fail(fmt.Sprintf("Could not tell whether the cluster (%s) has the Origamy data plane.", where),
			"`helm status "+release+" -n "+namespace+"` failed:\n"+k.problem+"\n"+
				"Fix that (helm installed, the right context, read access to "+namespace+"), then rerun `"+command+"`.")
	case k.reachable && docker:
		return installNone, fail("No Origamy data plane found.",
			fmt.Sprintf("The cluster of %s has no %s release in namespace %s, and there is no compose project here.\n", where, release, namespace)+
				"Switch kubectl to the cluster the data plane runs on, or cd into its Docker directory (e.g. ./origamy-dp-<id>), then rerun `"+command+"`.")
	case docker:
		return installDocker, nil
	}
	return installNone, nil
}

// quoteContext names a kubectl context for a message ("in-cluster" when
// kubectl runs in a pod without one).
func quoteContext(ctx string) string {
	if ctx == "" {
		return "in-cluster"
	}
	return fmt.Sprintf("%q", ctx)
}
