package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ── Helm release introspection ──────────────────────────────────────────────
// Shared by upgrade/rollback/status. The chart coordinates (helmChart,
// namespace, release) live in deploy.go.

// releaseInstalled reports whether the odp Helm release exists in the namespace.
func releaseInstalled() bool {
	return runQuiet("helm", "status", release, "-n", namespace) == nil
}

type releaseInfo struct {
	Chart      string // e.g. "origamy-data-plane-0.1.12"
	AppVersion string
	Revision   string
	Status     string
}

// installedRelease returns the currently-deployed chart, app version and revision.
func installedRelease() (*releaseInfo, error) {
	out, err := runCaptured("helm", "list", "-n", namespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("%s", out)
	}
	var list []struct {
		Name       string `json:"name"`
		Revision   string `json:"revision"`
		Status     string `json:"status"`
		Chart      string `json:"chart"`
		AppVersion string `json:"app_version"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	for _, r := range list {
		if r.Name == release {
			return &releaseInfo{Chart: r.Chart, AppVersion: r.AppVersion, Revision: r.Revision, Status: r.Status}, nil
		}
	}
	return nil, fmt.Errorf("release %q not found in namespace %q", release, namespace)
}

// latestChartVersion queries the OCI registry for the newest published chart version.
func latestChartVersion() (string, error) {
	out, err := runCaptured("helm", "show", "chart", helmChart)
	if err != nil {
		return "", fmt.Errorf("%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "version:"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("could not read chart version from the registry")
}

// chartVersion extracts the version suffix from a Helm chart string
// ("origamy-data-plane-0.1.12" → "0.1.12").
func chartVersion(chart string) string {
	if i := strings.LastIndex(chart, "-"); i >= 0 {
		return chart[i+1:]
	}
	return chart
}

// podSummary counts ready pods and surfaces per-component problems.
func podSummary(ns string) (ready, total int, issues map[string]string) {
	pods, err := snapshotPods(ns)
	if err != nil {
		return 0, 0, nil
	}
	issues = map[string]string{}
	total = len(pods)
	for _, p := range pods {
		if p.ready {
			ready++
		} else if p.issue != "" {
			issues[p.component] = p.issue
		}
	}
	return ready, total, issues
}

// ── Docker (compose) install discovery ──────────────────────────────────────
// deploy.go writes the compose project to ./origamy-dp-<id>/.

func findComposeDir() (string, bool) {
	if fileExists("docker-compose.yml") {
		if wd, err := os.Getwd(); err == nil {
			return wd, true
		}
	}
	if matches, _ := filepath.Glob("origamy-dp-*/docker-compose.yml"); len(matches) > 0 {
		return filepath.Dir(matches[0]), true
	}
	return "", false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// setEnvVar replaces (or appends) KEY=value in a dotenv file, preserving the rest.
func setEnvVar(path, key, val string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			lines[i] = key + "=" + val
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, key+"="+val)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

func readEnvVar(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
