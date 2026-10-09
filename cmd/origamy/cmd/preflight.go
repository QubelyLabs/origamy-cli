package cmd

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ── Install target ───────────────────────────────────────────────────────────

// installTarget is where `origamy deploy` puts the data plane.
type installTarget string

const (
	targetAuto       installTarget = "auto"
	targetKubernetes installTarget = "kubernetes"
	targetDocker     installTarget = "docker"
)

// errNoTarget means neither a Kubernetes cluster nor Docker is usable here.
var errNoTarget = errors.New("no Kubernetes cluster or Docker found on this machine")

// resolveTarget picks the install target. "auto" prefers Kubernetes whenever
// kubectl reaches a cluster — which is also true on a laptop running Docker
// Desktop with its bundled Kubernetes switched on, or with a kubeconfig that
// still points at some shared cluster — so callers of auto should say which
// target won and how to override it. An explicit target is verified and never
// silently falls back to the other one.
func resolveTarget(want string, k8s, docker bool) (installTarget, error) {
	switch strings.ToLower(strings.TrimSpace(want)) {
	case string(targetKubernetes), "k8s":
		if !k8s {
			return "", errors.New("no Kubernetes cluster is reachable (kubectl cluster-info failed)")
		}
		return targetKubernetes, nil
	case string(targetDocker):
		if !docker {
			return "", errors.New("Docker is not available here (docker info failed)")
		}
		return targetDocker, nil
	case string(targetAuto), "":
		switch {
		case k8s:
			return targetKubernetes, nil
		case docker:
			return targetDocker, nil
		}
		return "", errNoTarget
	default:
		return "", fmt.Errorf("unknown --target %q (use auto, kubernetes or docker)", want)
	}
}

// kubeContext names the kubectl context an install would land in, "" if unknown.
func kubeContext() string {
	out, err := runCaptured("kubectl", "config", "current-context")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// namespaceExists reports whether the data-plane namespace is present in the
// current cluster — the signal that an install lives there, even when the
// Helm release itself is gone.
func namespaceExists(ns string) bool {
	return runQuiet("kubectl", "get", "namespace", ns) == nil
}

// ── CPU architecture ─────────────────────────────────────────────────────────

// imageArch is the only CPU architecture the data-plane images are published
// for (the release workflow builds on amd64 runners without a platforms list).
const imageArch = "amd64"

// normalizeArch maps the spellings uname, Docker and Kubernetes use onto Go's
// GOARCH names so they can be compared with imageArch.
func normalizeArch(a string) string {
	switch s := strings.ToLower(strings.TrimSpace(a)); s {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return s
	}
}

// dockerArch returns the Docker engine's CPU architecture, "" if unknown.
func dockerArch() string {
	out, err := runCaptured("docker", "info", "--format", "{{.Architecture}}")
	if err != nil {
		return ""
	}
	return normalizeArch(out)
}

// dockerArchWarning explains what a non-amd64 Docker host means for a compose
// install; "" when the host can run the images natively or is unknown.
func dockerArchWarning(arch string) string {
	if arch == "" || arch == imageArch {
		return ""
	}
	return fmt.Sprintf("Origamy data-plane images are published for linux/%s only and this Docker host is %s. "+
		"Docker Desktop runs them under emulation (works, slower); a Linux %s host needs QEMU binfmt first "+
		"(docker run --privileged --rm tonistiigi/binfmt --install %s) or every service exits with 'exec format error'.",
		imageArch, arch, arch, imageArch)
}

// nodeArchs returns the distinct CPU architectures of the cluster's nodes,
// sorted; nil when they cannot be listed (RBAC, no nodes yet).
func nodeArchs() []string {
	out, err := runCaptured("kubectl", "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.status.nodeInfo.architecture}{"\n"}{end}`)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var archs []string
	for _, l := range strings.Split(out, "\n") {
		if a := normalizeArch(l); a != "" && !seen[a] {
			seen[a] = true
			archs = append(archs, a)
		}
	}
	sort.Strings(archs)
	return archs
}

// nodeArchProblem turns the node architectures into either a blocking message
// (no node can run the images: the pods would never schedule) or a warning
// (some nodes can; the pods will only land on those). Both empty when every
// node is amd64 or the architectures are unknown.
func nodeArchProblem(archs []string) (block, warn string) {
	if len(archs) == 0 {
		return "", ""
	}
	hasAMD64 := false
	for _, a := range archs {
		if a == imageArch {
			hasAMD64 = true
		}
	}
	if !hasAMD64 {
		return fmt.Sprintf("This cluster's nodes are %s, but Origamy data-plane images are published for linux/%s only — the pods would never schedule.",
			strings.Join(archs, "/"), imageArch), ""
	}
	if len(archs) > 1 {
		return "", fmt.Sprintf("This cluster mixes %s nodes; Origamy data-plane images are linux/%s only, so the pods will schedule on the %s nodes.",
			strings.Join(archs, " and "), imageArch, imageArch)
	}
	return "", ""
}

// ── ClickHouse schema generation of the served Docker bundle ─────────────────

// The control plane serves ONE clickhouse-init.sql at /byod/ — the schema of
// its current data-plane build — while the Docker install pins the images to
// a release. Those two moved apart in the 2026-08 "storage reset": releases
// up to 0.1.17 write the events payload columns as String (and the bulker
// widens the table with new columns), later builds write native JSON columns
// into a closed schema. Mixing them deploys cleanly, the dashboard shows the
// plane as Connected, and every event insert fails — so check before `up`.
const minImageForJSONSchema = "0.1.18"

var (
	reJSONSchema   = regexp.MustCompile(`(?m)^\s*properties\s+JSON\b`)
	reStringSchema = regexp.MustCompile(`(?m)^\s*properties\s+String\b`)
)

// schemaGeneration classifies a clickhouse-init.sql as "json" (closed schema,
// native JSON payload columns), "string" (the earlier String columns) or
// "unknown" when neither marker is present.
func schemaGeneration(initSQL string) string {
	switch {
	case reJSONSchema.MatchString(initSQL):
		return "json"
	case reStringSchema.MatchString(initSQL):
		return "string"
	default:
		return "unknown"
	}
}

// bundleSchemaMismatch returns a non-empty explanation when images tagged
// imageTag cannot write the schema initSQL creates. Non-semver tags
// (main, staging, sha-…) and unknown schemas are never blocked.
func bundleSchemaMismatch(initSQL, imageTag string) string {
	if _, semver := parseVersion(imageTag); !semver {
		return ""
	}
	switch schemaGeneration(initSQL) {
	case "json":
		if versionLess(imageTag, minImageForJSONSchema) {
			return fmt.Sprintf("Your control plane serves the current ClickHouse schema (native JSON payload columns), which data-plane release %s cannot write to — events would be accepted and then dropped at insert. Releases from %s on match it.",
				imageTag, minImageForJSONSchema)
		}
	case "string":
		if !versionLess(imageTag, minImageForJSONSchema) {
			return fmt.Sprintf("Your control plane serves the pre-%s ClickHouse schema (String payload columns), which data-plane release %s no longer writes. Upgrade the control plane, or install a release before %s.",
				minImageForJSONSchema, imageTag, minImageForJSONSchema)
		}
	}
	return ""
}
