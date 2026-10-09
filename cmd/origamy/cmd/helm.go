package cmd

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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

// ── Predictor (conversion scoring) enablement ────────────────────────────────

// predictorToggleArgs returns the helm --set arguments that switch the
// predictor service on or off. Same contract as aiToggleArgs: only the
// boolean is passed; the chart's defaults supply everything else (which is
// why toggles go through the exported-values path, not --reuse-values —
// a release installed before the predictor existed has none of its values).
func predictorToggleArgs(enable, disable bool) ([]string, error) {
	switch {
	case enable && disable:
		return nil, fmt.Errorf("--enable-predictor and --disable-predictor cannot be used together")
	case enable:
		return []string{"--set", "predictor.enabled=true"}, nil
	case disable:
		return []string{"--set", "predictor.enabled=false"}, nil
	default:
		return nil, nil
	}
}

// exportReleaseValues writes the release's user-supplied values to a temp file
// (JSON, which helm -f accepts) and returns its path; the caller removes it.
// Every upgrade goes through this rather than `helm upgrade --reuse-values`:
// --reuse-values hands the new chart the old release's fully coalesced values,
// so any default block the old chart lacked is missing and the templates
// nil-deref at render. Re-applying only the customer's own values (controlPlane,
// portalAgent, preset, the tunnel identity Secret, …) lets the target chart's
// defaults fill the gaps. When targetVer no longer needs the pre-0.1.18 probe
// overrides a deploy may have written, they are pruned here so the release
// returns to the chart's defaults.
func exportReleaseValues(targetVer string) (string, error) {
	vals, err := releaseValues()
	if err != nil {
		return "", err
	}
	if legacyChartSetArgs(targetVer) == nil {
		pruneLegacyValues(vals)
	}
	data, err := json.Marshal(vals)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "origamy-values-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
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

// releaseValues returns the values the operator supplied to the release
// (helm get values), as a map. An install with none yields an empty map.
func releaseValues() (map[string]any, error) {
	out, err := runCaptured("helm", "get", "values", release, "-n", namespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("%s", out)
	}
	vals := map[string]any{}
	if strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "null" {
		if err := json.Unmarshal([]byte(out), &vals); err != nil {
			return nil, fmt.Errorf("could not parse the release values: %w", err)
		}
	}
	return vals, nil
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

// ── Docker (compose) helpers ─────────────────────────────────────────────────
// deploy.go writes the compose project to ./origamy-dp-<id>/; discovery lives
// in compose.go (composeProjectDir).

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
		// Insert before the trailing "" that a newline-terminated file splits
		// into, so the file stays newline-terminated with no blank line.
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = append(lines[:n-1], key+"="+val, "")
		} else {
			lines = append(lines, key+"="+val, "")
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return err
	}
	// WriteFile only applies the mode when it creates the file; an .env an
	// operator created 0644 must not stay world-readable once it holds secrets.
	return os.Chmod(path, 0o600)
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

// ── Chart version gating ────────────────────────────────────────────────────

// Minimum chart versions that carry a feature's templates. Helm silently
// ignores values an older chart doesn't know, so toggling a feature on a chart
// that predates it would report "enabled" while nothing deployed.
const (
	minChartAI        = "0.1.16" // orchestrator-engine templates + secret
	minChartPredictor = "0.1.17" // predictor templates
	// clickhouse.existingSecret/secure/username, the schema Job and egress for
	// an external host (QubelyLabs/origamy-data-plane#389). 0.1.18 is cut
	// from staging without it, so the first release that can carry it is
	// 0.1.19. Lower this only if #389 lands in an earlier release.
	minChartExternalClickHouse = "0.1.19"
)

// featureGate returns an error when a requested feature toggle targets a chart
// too old to have it. Non-semver targets ("main", "") are never blocked.
func featureGate(chartVer string, enableAI, enablePredictor bool) error {
	if enableAI && versionLess(chartVer, minChartAI) {
		return fmt.Errorf("the AI engine needs chart %s or newer (targeting %s)", minChartAI, chartVer)
	}
	if enablePredictor && versionLess(chartVer, minChartPredictor) {
		return fmt.Errorf("the predictor needs chart %s or newer (targeting %s)", minChartPredictor, chartVer)
	}
	return nil
}

// versionLess reports whether semver a < b ("0.1.9" < "0.1.10"). Anything that
// isn't X.Y.Z compares as not-less, so edge/unknown versions pass through.
func versionLess(a, b string) bool {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ── Docker (compose) helpers ─────────────────────────────────────────────────

// orchestratorEngineURL is where portal-agent reaches the AI engine inside the
// compose network when the "agentic" profile is on (matches the bundle's
// service name + health port).
const orchestratorEngineURL = "http://orchestrator-engine:18090"

// existingOrRandomKEK is existingOrRandom for the orchestrator KEK, which the
// engine requires as STANDARD base64 decoding to exactly 32 bytes — not the
// URL-safe alphabet the datastore passwords use. Never rotated once set:
// per-workspace LLM keys encrypted under it would become unreadable.
func existingOrRandomKEK(path, key string) string {
	if v := readEnvVar(path, key); v != "" {
		return v
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// composeProfiles reads COMPOSE_PROFILES from a dotenv file as a list.
func composeProfiles(envPath string) []string {
	var out []string
	for _, p := range strings.Split(readEnvVar(envPath, "COMPOSE_PROFILES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// withProfile adds or removes one compose profile, preserving order and
// never duplicating.
func withProfile(profiles []string, name string, on bool) []string {
	var out []string
	for _, p := range profiles {
		if p != name {
			out = append(out, p)
		}
	}
	if on {
		out = append(out, name)
	}
	return out
}

// errNotServed means the control plane answered a bundle-file request with its
// SPA index.html: the file is not part of the bundle that control plane ships.
var errNotServed = errors.New("not served by the control plane")

// isLocalBase reports whether a plaintext base URL points at this machine —
// the only case where fetching over http:// is acceptable (same rule as the
// enrollment redemption in internal/token).
func isLocalBase(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// fetchBundleFile downloads one file of the BYOD deploy bundle from the
// control plane into dir. The bundle is executable configuration (compose
// file, schema), so it only travels over HTTPS (or to a local dev plane) and
// a redirect to another host or scheme is refused. The control plane's SPA
// handler answers unknown paths with index.html and HTTP 200, so a successful
// status alone can't tell "missing" from "found" — sniff the body and reject
// HTML. Plain net/http: the CLI must not depend on a curl being installed.
func fetchBundleFile(base, name, dir string) error {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(base, "https://") && !isLocalBase(base) {
		return fmt.Errorf("refusing to fetch the deploy bundle from %s: the control plane URL must be https", base)
	}
	target := base + "/byod/" + name
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !isLocalBase(req.URL.String()) {
				return fmt.Errorf("refusing a redirect to %s (not https)", req.URL)
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing a redirect to another host (%s)", req.URL.Host)
			}
			return nil
		},
	}
	resp, err := client.Get(target)
	if err != nil {
		return fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", target, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("GET %s: %w", target, err)
	}
	if looksLikeHTML(b) {
		return errNotServed
	}
	return os.WriteFile(filepath.Join(dir, name), b, 0o644)
}

// mergeEnv returns rendered (the keys the CLI owns, freshly generated) with
// every other KEY=value line of an existing .env appended, so an operator's
// own additions (CORS_ALLOWED_ORIGINS, RATE_LIMIT_RPS, LLM_API_KEY, …) survive
// a re-deploy in place. Comments and blank lines of the old file are dropped;
// a key the CLI owns is always the freshly rendered one.
func mergeEnv(existing, rendered string) string {
	owned := map[string]bool{}
	for _, l := range strings.Split(rendered, "\n") {
		if k, _, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(l, "#") {
			owned[strings.TrimSpace(k)] = true
		}
	}
	var kept []string
	for _, l := range strings.Split(existing, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		if !ok || owned[strings.TrimSpace(k)] {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		return rendered
	}
	return rendered + "# Kept from your previous .env (not managed by `origamy deploy`).\n" + strings.Join(kept, "\n") + "\n"
}

func looksLikeHTML(b []byte) bool {
	head := strings.ToLower(strings.TrimSpace(string(b[:min(len(b), 64)])))
	return len(b) == 0 || strings.HasPrefix(head, "<!doctype") || strings.HasPrefix(head, "<html")
}

// dockerEnvParams is everything `origamy deploy` needs to render the compose
// bundle's .env. Kept as a struct so the rendering is testable.
type dockerEnvParams struct {
	TunnelAddr, HTTPURL, DataPlaneID, AuthToken string
	ImageTag, Preset                            string
	Profiles                                    []string
	IngestDomain                                string
	RedisPw, NatsPw, ClickHousePw, DBPw         string
	OrchKEK, OrchToken                          string
	AIEnabled                                   bool
	MTLS                                        bool
}

// renderDockerEnv writes the .env the served compose bundle expects. The
// control plane's own "Docker / bare-metal" handoff emits the same keys, so a
// CLI install and a hand-pasted install are interchangeable.
func renderDockerEnv(p dockerEnvParams) string {
	base := strings.TrimRight(p.HTTPURL, "/")
	var b strings.Builder
	w := func(k, v string) { _, _ = fmt.Fprintf(&b, "%s=%s\n", k, v) }
	b.WriteString("# Generated by `origamy deploy`. Edit, then: docker compose --env-file .env up -d\n")
	w("CONTROL_PLANE_ADDR", p.TunnelAddr)
	w("CONTROL_PLANE_HTTP_URL", base)
	// config-sync and the telemetry reporter take FULL endpoint URLs (the Helm
	// chart derives the same two paths from controlPlane.httpUrl).
	w("CONFIG_URL", base+"/api/v1/config")
	w("TELEMETRY_URL", base+"/api/v1/telemetry")
	w("DATA_PLANE_ID", p.DataPlaneID)
	w("AUTH_TOKEN", p.AuthToken)
	w("TLS_ENABLED", "true")
	b.WriteString("# Release to run. Images and the Helm chart share a version; `origamy upgrade` moves it.\n")
	w("DP_IMAGE_TAG", p.ImageTag)
	w("LOG_LEVEL", "info")
	w("DEPLOYMENT_PRESET", p.Preset)
	b.WriteString("# Write keys sync from the control plane; this static allowlist is only a bootstrap fallback.\n")
	w("WRITE_KEYS", "")
	b.WriteString("# Compose profiles: full = journeys/broadcasts/tasks (Postgres + workflow-engine),\n# agentic = AI engine, ingress = HTTPS for SDK traffic via the bundled Caddy.\n")
	w("COMPOSE_PROFILES", strings.Join(p.Profiles, ","))
	w("INGEST_DOMAIN", p.IngestDomain)
	b.WriteString("# Datastore passwords — generated on this host, never sent to Origamy. Don't rotate them\n# casually: the datastores hold your data under these credentials.\n")
	w("DP_REDIS_PASSWORD", p.RedisPw)
	w("NATS_PASSWORD", p.NatsPw)
	w("CLICKHOUSE_PASSWORD", p.ClickHousePw)
	w("DB_PASSWORD", p.DBPw)
	if p.OrchKEK != "" || p.OrchToken != "" {
		b.WriteString("# AI engine secrets (generated here). ORCH_KEK encrypts per-workspace LLM keys at rest — NEVER rotate it.\n")
		w("ORCH_KEK", p.OrchKEK)
		w("ORCH_ENGINE_API_TOKEN", p.OrchToken)
	}
	if p.AIEnabled {
		w("ORCHESTRATOR_ENGINE_URL", orchestratorEngineURL)
	}
	if p.MTLS {
		b.WriteString("# mTLS client identity for the tunnel (written to ./certs; the agent renews it in place).\n")
		w("TUNNEL_TLS_CERT", "/certs/tls.crt")
		w("TUNNEL_TLS_KEY", "/certs/tls.key")
	}
	return b.String()
}
