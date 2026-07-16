package cmd

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// existingOrRandom returns the value of key from the dotenv at path if it is
// already set (so a re-deploy keeps a datastore password stable — rotating it
// would lock out the datastore that holds data), otherwise a fresh 32-byte
// URL-safe random secret. Generated locally; never transmitted.
func existingOrRandom(path, key string) string {
	if v := readEnvVar(path, key); v != "" {
		return v
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively impossible on a real host; if it
		// ever does, "" leaves that datastore unauthenticated (no auth = the
		// backward-compatible default), never a weak/predictable password.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── AI (orchestrator engine) enablement ─────────────────────────────────────

// aiToggleArgs returns the helm --set arguments that switch the agentic
// orchestrator engine on or off. The chart owns every secret: when
// orchestratorEngine.enabled flips true it auto-generates the engine's KEK and
// API token, so the CLI passes ONLY the boolean and never a credential. enable
// and disable are mutually exclusive; passing both is a user error.
func aiToggleArgs(enable, disable bool) ([]string, error) {
	switch {
	case enable && disable:
		return nil, fmt.Errorf("--enable-ai and --disable-ai cannot be used together")
	case enable:
		return []string{"--set", "orchestratorEngine.enabled=true"}, nil
	case disable:
		return []string{"--set", "orchestratorEngine.enabled=false"}, nil
	default:
		return nil, nil
	}
}

// exportReleaseValues writes the release's user-supplied values to a temp file
// and returns its path (the caller removes it). Toggling a chart-default value
// like orchestratorEngine.enabled must NOT go through `helm upgrade
// --reuse-values`: when the target chart has gained new default structure the
// old release never set, --reuse-values fails to parse the stale merged values.
// Re-applying the user's own values from a file instead lets the new chart's
// defaults fill the gaps cleanly while preserving every value the customer
// supplied (controlPlane, portalAgent, preset, clickhouse, and the tunnel
// identity Secret references).
func exportReleaseValues() (string, error) {
	out, err := runCaptured("helm", "get", "values", release, "-n", namespace, "-o", "yaml")
	if err != nil {
		return "", fmt.Errorf("%s", out)
	}
	f, err := os.CreateTemp("", "origamy-values-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(out); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

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

// dataPlaneImageRepo identifies an Origamy data-plane service image (as opposed
// to a bundled datastore like ClickHouse/Postgres, whose tags are upstream
// versions and must never be moved to a chart version).
const dataPlaneImageRepo = "origamy-data-plane/"

// serviceImageKeys returns the top-level value keys whose image is an Origamy
// data-plane service (bulkerWorker, portalAgent, …) in the current release's
// coalesced values. Used to re-pin every service on a version bump: helm
// --reuse-values carries forward each service's frozen image.tag, and the
// chart's image helper lets a per-service tag SHADOW global.imageTag/appVersion
// — so without clearing them a pinned upgrade silently keeps the old images
// (the pre-pinning 0.1.12 chart froze tag: main, stranding upgrades on :main).
// Best-effort: on any error it returns nil and the caller falls back to
// global.imageTag alone. Datastore images are excluded by repository prefix.
func serviceImageKeys() []string {
	out, err := runCaptured("helm", "get", "values", release, "-n", namespace, "--all", "-o", "json")
	if err != nil {
		return nil
	}
	var vals map[string]any
	if err := json.Unmarshal([]byte(out), &vals); err != nil {
		return nil
	}
	var keys []string
	for k, v := range vals {
		svc, ok := v.(map[string]any)
		if !ok {
			continue
		}
		img, ok := svc["image"].(map[string]any)
		if !ok {
			continue
		}
		repo, _ := img["repository"].(string)
		if !strings.Contains(repo, dataPlaneImageRepo) {
			continue // datastore or unrelated image — leave its tag alone
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
