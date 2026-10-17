package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeControlPlane serves files at /byod/<name> and answers anything else
// the way the control plane's SPA handler does: index.html with HTTP 200.
func fakeControlPlane(t *testing.T, files map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := files[strings.TrimPrefix(r.URL.Path, "/byod/")]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte("<!doctype html><html><body>dashboard</body></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// stubDocker stands fn in for both Docker runners (dockerRun, and
// dockerRunTimeout, whose time limit fn does not see) for one test.
func stubDocker(t *testing.T, fn func(env []string, name string, args ...string) (string, error)) {
	t.Helper()
	prevRun, prevTimeout := dockerRun, dockerRunTimeout
	dockerRun = fn
	dockerRunTimeout = func(_ time.Duration, env []string, name string, args ...string) (string, error) {
		return fn(env, name, args...)
	}
	t.Cleanup(func() { dockerRun, dockerRunTimeout = prevRun, prevTimeout })
}

// fakeDocker stands in for Docker: it records every call and fails those
// whose arguments contain a word in failOn.
func fakeDocker(t *testing.T, failOn ...string) *[]string {
	t.Helper()
	var calls []string
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		call := strings.Join(append(append(env, name), args...), " ")
		calls = append(calls, call)
		for _, w := range failOn {
			if slices.Contains(args, w) {
				return "boom: " + w + " failed", errors.New("exit status 1")
			}
		}
		return "", nil
	})
	return &calls
}

// withKnownHashes replaces the generated table for one test.
func withKnownHashes(t *testing.T, table map[string][]string) {
	t.Helper()
	prev := knownBundleHashes
	knownBundleHashes = table
	t.Cleanup(func() { knownBundleHashes = prev })
}

// withKnownReleases replaces the generated hash → releases table for one test.
func withKnownReleases(t *testing.T, table map[string][]string) {
	t.Helper()
	prev := knownBundleReleases
	knownBundleReleases = table
	t.Cleanup(func() { knownBundleReleases = prev })
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// recordDeployedBundle writes the manifest a deploy leaves for the bundle
// files now in dir, from the release the control plane said they are for
// ("" when it predates /byod/VERSION).
func recordDeployedBundle(dir, version string) error {
	m := bundleManifest{Version: version, Files: map[string]string{}, Releases: map[string]string{}}
	for _, name := range bundleFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		m.Files[name] = sha256Hex(b)
		if version != "" {
			m.Releases[name] = version
		}
	}
	return writeBundleManifest(dir, m)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClassifyBundleFile(t *testing.T) {
	served := []byte("services: {new: {}}\n")
	old := []byte("services: {old: {}}\n")
	edited := []byte("services: {old: {}, mine: {}}\n")
	withKnownHashes(t, map[string][]string{"docker-compose.yml": {sha256Hex(old)}})
	reg := func(b []byte) bundleCurrent { return bundleCurrent{data: b, mode: 0o644, exists: true} }
	none := bundleManifest{}
	recorded := bundleManifest{Files: map[string]string{"docker-compose.yml": sha256Hex(edited)}}
	recordedOld := bundleManifest{Files: map[string]string{"docker-compose.yml": sha256Hex(old)}}

	for _, tc := range []struct {
		name    string
		cur     bundleCurrent
		m       bundleManifest
		replace bool
		want    bundleState
	}{
		{"already the served bytes", reg(served), none, false, bundleUnchanged},
		{"pristine via the manifest", reg(edited), recorded, false, bundlePristine},
		{"pristine via the known table (no manifest)", reg(old), none, false, bundlePristine},
		{"pristine via the known table (file not in the manifest)", reg(old), bundleManifest{Files: map[string]string{"Caddyfile": "x"}}, false, bundlePristine},
		{"edited, unknown hash", reg(edited), none, false, bundleModified},
		{"edited since the manifest was written", reg(edited), recordedOld, false, bundleModified},
		// A served version put back (a .bak copied over the file) is pristine
		// whatever the manifest recorded, so every run decides alike.
		{"known version though the manifest records another", reg(old), recorded, false, bundlePristine},
		{"--replace-bundle replaces an edit", reg(edited), none, true, bundlePristine},
		{"missing", bundleCurrent{}, none, false, bundleMissing},
		{"empty directory Docker left for the bind mount", bundleCurrent{exists: true, emptyDir: true}, none, false, bundleMissing},
		{"symlink is never replaced", bundleCurrent{data: old, exists: true, own: true}, none, true, bundleModified},
		{"symlink to the served bytes", bundleCurrent{data: served, exists: true, own: true}, none, false, bundleUnchanged},
	} {
		if got := classifyBundleFile("docker-compose.yml", tc.cur, served, tc.m, tc.replace); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestReadBundleCurrent(t *testing.T) {
	dir := t.TempDir()
	if cur, err := readBundleCurrent(filepath.Join(dir, "nope")); err != nil || cur.exists {
		t.Fatalf("missing: %+v, %v", cur, err)
	}
	writeFile(t, filepath.Join(dir, "f"), "x")
	if cur, err := readBundleCurrent(filepath.Join(dir, "f")); err != nil || !cur.exists || cur.own || string(cur.data) != "x" || cur.mode != 0o644 {
		t.Fatalf("regular: %+v, %v", cur, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if cur, _ := readBundleCurrent(filepath.Join(dir, "empty")); !cur.emptyDir || cur.own {
		t.Fatalf("empty dir: %+v", cur)
	}
	if err := os.Symlink(filepath.Join(dir, "f"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if cur, _ := readBundleCurrent(filepath.Join(dir, "link")); !cur.own || string(cur.data) != "x" {
		t.Fatalf("symlink: %+v", cur)
	}
}

func TestBundleVersionProblem(t *testing.T) {
	for _, tc := range []struct {
		name, served string
		err          error
		tag          string
		want         string // substring; "" = refresh
	}{
		{"match", "0.1.20", nil, "0.1.20", ""},
		{"older control plane", "", errNotServed, "0.1.20", "predates /byod/VERSION, so it cannot say which release its bundle is for; re-run `origamy upgrade` after your control plane is updated"},
		{"404", "", fmt.Errorf("GET x: HTTP 404: %w", errNotServed), "0.1.20", "predates /byod/VERSION"},
		{"unreachable", "", errors.New("dial tcp: refused"), "0.1.20", "dial tcp: refused"},
		{"--version older than the pin", "0.1.20", nil, "0.1.19", "serves the bundle for 0.1.20, not 0.1.19"},
		{"--version newer than the pin", "0.1.20", nil, "0.1.21", "not 0.1.21"},
		{"--channel edge", "0.1.20", nil, dockerEdgeTag, "not staging"},
		{"no v-prefix leniency", "0.1.20", nil, "v0.1.20", "not v0.1.20"},
	} {
		got := bundleVersionProblem(tc.served, tc.err, tc.tag)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestServedBundleVersion(t *testing.T) {
	base := fakeControlPlane(t, map[string]string{"VERSION": "0.1.20\n"})
	if v, err := servedBundleVersion(base); err != nil || v != "0.1.20" {
		t.Fatalf("got %q, %v", v, err)
	}
	// An older control plane answers with the SPA's index.html.
	if _, err := servedBundleVersion(fakeControlPlane(t, nil)); !errors.Is(err, errNotServed) {
		t.Fatalf("SPA fallback: want errNotServed, got %v", err)
	}
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	if _, err := servedBundleVersion(notFound.URL); !errors.Is(err, errNotServed) {
		t.Fatalf("404: want errNotServed, got %v", err)
	}
	if _, err := servedBundleVersion(fakeControlPlane(t, map[string]string{"VERSION": "0.1.20\nrm -rf /\n"})); err == nil {
		t.Fatal("a VERSION that is not one release name must be refused")
	}
	if _, err := servedBundleVersion("http://cp.example.com"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http to a remote host must be refused, got %v", err)
	}
}

// v018Env is the .env CLI v0.1.18 and older wrote (cmd/origamy/cmd/deploy.go
// at v0.1.18): no CONTROL_PLANE_HTTP_URL, and CONFIG_URL the bare
// control-plane URL the enrollment token carried.
func v018Env(base string) string {
	return fmt.Sprintf("CONTROL_PLANE_ADDR=%s\nCONFIG_URL=%s\nDATA_PLANE_ID=%s\nAUTH_TOKEN=%s\nTLS_ENABLED=true\nDP_IMAGE_TAG=main\nLOG_LEVEL=info\nDEPLOYMENT_PRESET=%s\n"+
		"DP_REDIS_PASSWORD=%s\nNATS_PASSWORD=%s\nCLICKHOUSE_PASSWORD=%s\n",
		"tunnel.example.com:443", base, "dp-1", "tok", "starter", "r", "n", "c") +
		"TUNNEL_TLS_CERT=/certs/tls.crt\nTUNNEL_TLS_KEY=/certs/tls.key\n"
}

func TestControlPlaneHTTPBase(t *testing.T) {
	for _, tc := range []struct {
		name, env    string
		want         string
		fromFallback bool
	}{
		{"current CLI", "CONTROL_PLANE_HTTP_URL=https://cp.example.com\nCONFIG_URL=https://cp.example.com/api/v1/config\n", "https://cp.example.com", false},
		{"v0.1.18: bare CONFIG_URL", v018Env("https://cp.example.com"), "https://cp.example.com", true},
		{"v0.1.18: trailing slash", v018Env("https://cp.example.com/"), "https://cp.example.com", true},
		{"dashboard snippet: config endpoint", "CONFIG_URL=https://cp.example.com/api/v1/config\n", "https://cp.example.com", true},
		{"config endpoint with a trailing slash", "CONFIG_URL=https://cp.example.com/api/v1/config/\n", "https://cp.example.com", true},
		{"quoted", "CONFIG_URL=\"https://cp.example.com/api/v1/config\"\n", "https://cp.example.com", true},
		{"control plane under a path", "CONFIG_URL=https://example.com/origamy/api/v1/config\n", "https://example.com/origamy", true},
		{"neither", "DATA_PLANE_ID=dp-1\n", "", false},
		{"CONFIG_URL not a URL", "CONFIG_URL=cp.example.com\n", "", false},
	} {
		env := filepath.Join(t.TempDir(), ".env")
		writeFile(t, env, tc.env)
		if got, fb := controlPlaneHTTPBase(env); got != tc.want || fb != tc.fromFallback {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, fb, tc.want, tc.fromFallback)
		}
	}
}

func TestPlanBundleRefreshV018Env(t *testing.T) {
	// A project deployed by CLI v0.1.18 (a bare CONFIG_URL, DP_IMAGE_TAG=main)
	// refreshes from the URL in CONFIG_URL.
	p := newBundleProject(t, "0.1.20")
	writeFile(t, p.env, v018Env(p.base+"/"))
	fakeDocker(t)
	base, fromConfigURL := controlPlaneHTTPBase(p.env)
	if base != p.base || !fromConfigURL {
		t.Fatalf("base = %q, %v", base, fromConfigURL)
	}
	r, skipped := planBundleRefresh(p.dir, p.env, base, "0.1.20", readEnvVar(p.env, "DP_IMAGE_TAG"), bundleAuto, false)
	if r == nil {
		t.Fatalf("no refresh: %q", skipped)
	}
	defer r.cleanup()
	if bak := backupRelease("docker-compose.yml", []byte(oldCompose), r.prev, r.installedTag); !slices.Equal(r.changed(), []string{"docker-compose.yml", "clickhouse-users.xml"}) || bak != "main" {
		t.Fatalf("changed %v, backups .bak-%s", r.changed(), bak)
	}
}

func TestPlanBundleRefreshWithoutControlPlaneURL(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	writeFile(t, p.env, "DATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=0.1.19\n")
	calls := fakeDocker(t)
	before := p.snapshot(t)
	base, _ := controlPlaneHTTPBase(p.env)
	r, skipped := planBundleRefresh(p.dir, p.env, base, "0.1.20", "0.1.19", bundleAuto, false)
	if r != nil || !strings.Contains(skipped, "URL missing from .env") || strings.Contains(skipped, "unreachable") {
		t.Fatalf("got %v, %q", r, skipped)
	}
	if len(*calls) != 0 {
		t.Fatalf("docker ran: %v", *calls)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}
}

// bundleProject is a Docker project dir as an older CLI left it: an unedited
// compose of an earlier release, an edited Caddyfile, no clickhouse-users.xml
// and a schema identical to the served one. version is what the control plane
// serves as /byod/VERSION ("" for one that predates it).
type bundleProject struct {
	dir, env, base string
	served         map[string]string
}

const oldCompose = "services: {old: {}}\n"

func newBundleProject(t *testing.T, version string) bundleProject {
	t.Helper()
	dir := t.TempDir()
	served := map[string]string{
		"docker-compose.yml":   "services: {new: {}}\n",
		"clickhouse-init.sql":  "CREATE TABLE events.events (\n    properties JSON\n) ENGINE = MergeTree ORDER BY tuple();\n",
		"clickhouse-users.xml": "<clickhouse/>\n",
		"Caddyfile":            "{$INGEST_DOMAIN} {\n\treverse_proxy ingestion-gateway:8081\n}\n",
	}
	if version != "" {
		served["VERSION"] = version + "\n"
	}
	base := fakeControlPlane(t, served)
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), oldCompose)
	writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), served["clickhouse-init.sql"])
	writeFile(t, filepath.Join(dir, "Caddyfile"), "my.domain {\n\treverse_proxy ingestion-gateway:8081\n}\n")
	env := filepath.Join(dir, ".env")
	writeFile(t, env, "CONTROL_PLANE_HTTP_URL="+base+"\nDATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=0.1.19\nCOMPOSE_PROFILES=full,ingress\n")
	withKnownHashes(t, map[string][]string{"docker-compose.yml": {sha256Hex([]byte(oldCompose))}})
	return bundleProject{dir: dir, env: env, base: base, served: served}
}

func (p bundleProject) read(t *testing.T, name string) string {
	return readFile(t, filepath.Join(p.dir, name))
}

// snapshot is every file in the project dir, for "nothing was touched".
func (p bundleProject) snapshot(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		out[e.Name()] = "dir"
		if !e.IsDir() {
			out[e.Name()] = p.read(t, e.Name())
		}
	}
	return out
}

func TestPlanBundleRefreshGating(t *testing.T) {
	for _, tc := range []struct {
		name, served, tag string
		mode              bundleMode
		wantSkip          string
	}{
		{"served version matches", "0.1.20", "0.1.20", bundleAuto, ""},
		{"older control plane without VERSION", "", "0.1.20", bundleAuto, "control plane has no /byod/VERSION"},
		{"control plane pinned elsewhere", "0.1.21", "0.1.20", bundleAuto, "control plane serves 0.1.21"},
		{"edge channel", "0.1.20", dockerEdgeTag, bundleAuto, "control plane serves 0.1.20"},
		{"--keep-bundle", "0.1.20", "0.1.20", bundleKeep, "--keep-bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newBundleProject(t, tc.served)
			calls := fakeDocker(t)
			before := p.snapshot(t)
			r, skipped := planBundleRefresh(p.dir, p.env, p.base, tc.tag, "0.1.19", tc.mode, false)
			if skipped != tc.wantSkip {
				t.Fatalf("skipped = %q, want %q", skipped, tc.wantSkip)
			}
			if tc.wantSkip != "" {
				if r != nil {
					t.Fatal("a skipped refresh must return no plan")
				}
				if len(*calls) != 0 {
					t.Fatalf("a skipped refresh must not run docker: %v", *calls)
				}
				if after := p.snapshot(t); !mapsEqual(before, after) {
					t.Fatalf("a skipped refresh touched the project:\nbefore %v\nafter  %v", before, after)
				}
				return
			}
			defer r.cleanup()
			if got := r.changed(); !slices.Equal(got, []string{"docker-compose.yml", "clickhouse-users.xml"}) {
				t.Fatalf("changed = %v", got)
			}
			if got := r.kept(); !slices.Equal(got, []string{"Caddyfile"}) {
				t.Fatalf("kept = %v", got)
			}
			// Planning validates the new compose with the target tag, and
			// touches nothing in the project yet.
			if len(*calls) != 1 || !strings.Contains((*calls)[0], "DP_IMAGE_TAG=0.1.20 docker compose") || !strings.Contains((*calls)[0], "config -q") {
				t.Fatalf("docker calls = %v", *calls)
			}
			if p.read(t, "docker-compose.yml") != oldCompose {
				t.Fatal("planning must not touch the project's files")
			}
		})
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestPlanBundleRefreshValidationFailureKeepsFiles(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	fakeDocker(t, "config")
	before := p.snapshot(t)
	r, skipped := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.19", bundleAuto, false)
	if r != nil || skipped != "new bundle failed validation" {
		t.Fatalf("got %v, %q", r, skipped)
	}
	// Nothing changed, and the download directory is gone.
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("a failed validation touched the project:\nbefore %v\nafter  %v", before, after)
	}
}

func TestPlanBundleRefreshSchemaGuard(t *testing.T) {
	// A bundle whose schema the target release cannot write is refused even
	// when the control plane claims it is that release's.
	p := newBundleProject(t, "0.1.17")
	writeFile(t, filepath.Join(p.dir, "clickhouse-init.sql"), "CREATE TABLE events.events (\n    properties String\n);\n")
	fakeDocker(t)
	if r, skipped := planBundleRefresh(p.dir, p.env, p.base, "0.1.17", "0.1.16", bundleAuto, true); r != nil || skipped != "new bundle failed validation" {
		t.Fatalf("got %v, %q", r, skipped)
	}
}

func TestPlanBundleRefreshResetForcesSchema(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	writeFile(t, filepath.Join(p.dir, "clickhouse-init.sql"), "-- my edit\n")
	fakeDocker(t)
	r, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.17", bundleAuto, false)
	if r == nil || r.served("clickhouse-init.sql") {
		t.Fatalf("without the reset an edited schema is kept: %+v", r)
	}
	r.cleanup()
	r, _ = planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.17", bundleAuto, true)
	if r == nil || !r.changes("clickhouse-init.sql") || !r.served("clickhouse-init.sql") {
		t.Fatalf("across the reset the new schema is forced in: %+v", r)
	}
	if ow := r.overwritten(); len(ow) != 1 || ow[0].name != "clickhouse-init.sql" {
		t.Fatalf("the replaced edit must be reported: %+v", ow)
	}
	r.cleanup()
}

func TestBundleRefreshApplyCommit(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	fakeDocker(t)
	// Docker left an empty directory where the missing users file belongs.
	if err := os.Mkdir(filepath.Join(p.dir, "clickhouse-users.xml"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An earlier upgrade from the same release left a backup: keep it.
	writeFile(t, filepath.Join(p.dir, "docker-compose.yml.bak-0.1.19"), "older backup\n")

	r, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.19", bundleAuto, false)
	if r == nil {
		t.Fatal("no refresh planned")
	}
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	if got := p.read(t, "docker-compose.yml"); got != p.served["docker-compose.yml"] {
		t.Fatalf("compose not replaced: %q", got)
	}
	if got := p.read(t, "docker-compose.yml.bak-0.1.19.1"); got != oldCompose {
		t.Fatalf("backup = %q", got)
	}
	if got := p.read(t, "docker-compose.yml.bak-0.1.19"); got != "older backup\n" {
		t.Fatalf("an existing backup was overwritten: %q", got)
	}
	if got := p.read(t, "clickhouse-users.xml"); got != p.served["clickhouse-users.xml"] {
		t.Fatalf("missing file not written: %q", got)
	}
	if strings.Contains(p.read(t, "Caddyfile"), "{$INGEST_DOMAIN}") {
		t.Fatal("an edited file must be kept")
	}
	if got := r.restarts([]string{"full", "ingress"}); !slices.Equal(got, []string{"clickhouse"}) {
		t.Fatalf("restarts = %v (the kept Caddyfile needs none)", got)
	}

	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	if got := p.read(t, "Caddyfile.new"); got != p.served["Caddyfile"] {
		t.Fatalf("Caddyfile.new = %q", got)
	}
	m := readBundleManifest(p.dir)
	if m.Version != "0.1.20" {
		t.Fatalf("manifest version = %q", m.Version)
	}
	for _, name := range []string{"docker-compose.yml", "clickhouse-init.sql", "clickhouse-users.xml"} {
		if m.Files[name] != sha256Hex([]byte(p.served[name])) {
			t.Errorf("manifest %s = %q", name, m.Files[name])
		}
	}
	if _, ok := m.Files["Caddyfile"]; ok {
		t.Error("an edited file must stay out of the manifest")
	}
	if _, err := os.Stat(r.tmp); !os.IsNotExist(err) {
		t.Fatalf("download directory left behind: %v", err)
	}

	// The next upgrade, with the manifest, still sees the Caddyfile as edited
	// and everything else as current.
	r2, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.20", bundleAuto, false)
	if r2 == nil || len(r2.changed()) != 0 || !slices.Equal(r2.kept(), []string{"Caddyfile"}) {
		t.Fatalf("second refresh: changed %v kept %v", r2.changed(), r2.kept())
	}
	r2.cleanup()

	// --replace-bundle takes the edit out too, backing it up under the
	// release the manifest names, and says so.
	r3, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.20", bundleReplace, false)
	if r3 == nil || !slices.Equal(r3.changed(), []string{"Caddyfile"}) {
		t.Fatalf("--replace-bundle: changed %v", r3.changed())
	}
	if err := r3.apply(); err != nil {
		t.Fatal(err)
	}
	if ow := r3.overwritten(); len(ow) != 1 || ow[0].name != "Caddyfile" || filepath.Base(ow[0].backup) != "Caddyfile.bak-0.1.20" {
		t.Fatalf("overwritten = %+v", ow)
	}
	if box := strings.Join(bundleBoxLines(r3, "", nil), "\n"); !strings.Contains(box, "replaced your edited Caddyfile") || !strings.Contains(box, "Caddyfile.bak-0.1.20") {
		t.Fatalf("box = %q", box)
	}
	if err := r3.commit(); err != nil {
		t.Fatal(err)
	}
	if got := p.read(t, "Caddyfile"); got != p.served["Caddyfile"] {
		t.Fatalf("Caddyfile not replaced: %q", got)
	}
	if !strings.HasPrefix(p.read(t, "Caddyfile.bak-0.1.20"), "my.domain") {
		t.Fatal("the edited Caddyfile was not backed up")
	}
	if readBundleManifest(p.dir).Files["Caddyfile"] == "" {
		t.Fatal("a replaced file belongs in the manifest")
	}
}

// refreshOnce plans, applies and commits one refresh to 0.1.20 and returns
// the files it replaced and kept.
func refreshOnce(t *testing.T, p bundleProject, installed string, mode bundleMode) (changed, kept []string) {
	t.Helper()
	r, skipped := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", installed, mode, false)
	if r == nil {
		t.Fatalf("no refresh: %q", skipped)
	}
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	return r.changed(), r.kept()
}

func TestBundleRefreshStableAcrossRuns(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	fakeDocker(t)
	// Deployed with the current bundle (manifest and all), then the Caddyfile
	// edited and an older compose put back from a backup.
	for name, body := range p.served {
		if name != "VERSION" {
			writeFile(t, filepath.Join(p.dir, name), body)
		}
	}
	if err := recordDeployedBundle(p.dir, "0.1.20"); err != nil {
		t.Fatal(err)
	}
	deployed := readBundleManifest(p.dir)
	writeFile(t, filepath.Join(p.dir, "Caddyfile"), "my.domain {\n}\n")
	writeFile(t, filepath.Join(p.dir, "docker-compose.yml"), oldCompose)

	// First run: the restored compose is a served version, so it is
	// replaced, not taken for an edit; the edited Caddyfile is kept and its
	// manifest entry stays the one deploy recorded.
	changed, kept := refreshOnce(t, p, "0.1.20", bundleAuto)
	if !slices.Equal(changed, []string{"docker-compose.yml"}) || !slices.Equal(kept, []string{"Caddyfile"}) {
		t.Fatalf("run 1: changed %v kept %v", changed, kept)
	}
	m1 := readBundleManifest(p.dir)
	if m1.Files["Caddyfile"] != deployed.Files["Caddyfile"] {
		t.Fatalf("run 1 dropped the kept file's entry: %+v", m1)
	}
	// Second run: the same decisions, the same manifest.
	changed, kept = refreshOnce(t, p, "0.1.20", bundleAuto)
	if len(changed) != 0 || !slices.Equal(kept, []string{"Caddyfile"}) {
		t.Fatalf("run 2: changed %v kept %v", changed, kept)
	}
	if m2 := readBundleManifest(p.dir); !mapsEqual(m1.Files, m2.Files) || m2.Version != m1.Version {
		t.Fatalf("manifest moved between runs:\n%+v\n%+v", m1, m2)
	}
}

func TestBackupRelease(t *testing.T) {
	served017, served019 := []byte("compose 0.1.17\n"), []byte("compose 0.1.17 and 0.1.19\n")
	preRelease, edited := []byte("compose main\n"), []byte("compose, edited\n")
	withKnownReleases(t, map[string][]string{
		sha256Hex(served017):  {"0.1.17"},
		sha256Hex(served019):  {"0.1.17", "0.1.19"},
		sha256Hex(preRelease): {"main"},
	})
	m := func(v string) bundleManifest { return bundleManifest{Version: v} }
	for _, tc := range []struct {
		name      string
		data      []byte
		m         bundleManifest
		installed string
		want      string
	}{
		// The release the control plane handed out with these exact bytes,
		// whatever the manifest and the tag say.
		{"known version, newer manifest and tag", served017, m("0.1.20"), "0.1.20", "0.1.17"},
		{"known version of a pre-release bundle", preRelease, m(""), "0.1.16", "main"},
		// Served under several releases: the one installed, else the
		// manifest's, else the newest.
		{"several releases, installed is one", served019, m("0.1.20"), "0.1.17", "0.1.17"},
		{"several releases, manifest is one", served019, m("0.1.17"), "0.1.20", "0.1.17"},
		{"several releases, neither", served019, m("0.1.20"), "0.1.20", "0.1.19"},
		// An edit: the release the manifest says the bundle came from, else
		// the installed tag, else nothing ("previous").
		{"edited, manifest", edited, m("0.1.19"), "0.1.20", "0.1.19"},
		{"edited, no manifest", edited, m(""), "0.1.18", "0.1.18"},
		{"edited, nothing", edited, m(""), "", ""},
		// An edit a refresh kept: the release the file itself came with, not
		// the bundle's newer one. Recorded per file by this CLI…
		{"edited, the file's own release", edited, bundleManifest{Version: "0.1.20", Releases: map[string]string{"docker-compose.yml": "0.1.17"}}, "0.1.20", "0.1.17"},
		// …and for a manifest from an earlier CLI, the release of the version
		// it recorded for the file, which the edit was made from.
		{"edited, the release of the version recorded for it", edited, bundleManifest{Version: "0.1.20", Files: map[string]string{"docker-compose.yml": sha256Hex(served017)}}, "0.1.20", "0.1.17"},
		{"edited, recorded version of several releases", edited, bundleManifest{Version: "0.1.20", Files: map[string]string{"docker-compose.yml": sha256Hex(served019)}}, "0.1.20", "0.1.19"},
		{"edited, recorded version unknown", edited, bundleManifest{Version: "0.1.20", Files: map[string]string{"docker-compose.yml": sha256Hex(edited)}}, "0.1.19", "0.1.20"},
		// Another file's record says nothing about this one.
		{"edited, only another file's release", edited, bundleManifest{Version: "0.1.20", Releases: map[string]string{"Caddyfile": "0.1.17"}}, "0.1.20", "0.1.20"},
		// Known bytes still go by the table.
		{"known version, the file's release says otherwise", served017, bundleManifest{Version: "0.1.20", Releases: map[string]string{"docker-compose.yml": "0.1.19"}}, "0.1.20", "0.1.17"},
	} {
		if got := backupRelease("docker-compose.yml", tc.data, tc.m, tc.installed); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := backupTag(backupRelease("docker-compose.yml", edited, m(""), "")); got != "previous" {
		t.Errorf("no release at all: backupTag = %q", got)
	}
}

func TestKeptEditBackedUpUnderItsOwnRelease(t *testing.T) {
	// Deployed with the 0.1.17 bundle; the Caddyfile edited here and kept
	// through a refresh to 0.1.20, which moved the manifest's version on.
	p := newBundleProject(t, "0.1.20")
	fakeDocker(t)
	withKnownReleases(t, map[string][]string{})
	writeFile(t, filepath.Join(p.dir, "clickhouse-users.xml"), p.served["clickhouse-users.xml"])
	writeFile(t, filepath.Join(p.dir, "Caddyfile"), p.served["Caddyfile"]+"# 0.1.17's\n")
	if err := recordDeployedBundle(p.dir, "0.1.17"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(p.dir, "Caddyfile"), "my.domain {\n}\n")
	if _, kept := refreshOnce(t, p, "0.1.17", bundleAuto); !slices.Equal(kept, []string{"Caddyfile"}) {
		t.Fatalf("kept %v", kept)
	}
	m := readBundleManifest(p.dir)
	if m.Version != "0.1.20" || m.Releases["Caddyfile"] != "0.1.17" || m.Releases["docker-compose.yml"] != "0.1.20" {
		t.Fatalf("manifest after the refresh: %+v", m)
	}
	// --replace-bundle later backs the edit up under 0.1.17, the release it
	// was made from, not 0.1.20.
	r, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.20", bundleReplace, false)
	if r == nil {
		t.Fatal("no refresh planned")
	}
	defer r.cleanup()
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	if f, _ := r.file("Caddyfile"); filepath.Base(f.backup) != "Caddyfile.bak-0.1.17" {
		t.Fatalf("backup %q", f.backup)
	}
}

func TestBundleRefreshRemovesStaleNew(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	fakeDocker(t)
	// Run 1 keeps the edited Caddyfile and writes Caddyfile.new next to it.
	if _, kept := refreshOnce(t, p, "0.1.19", bundleAuto); !slices.Equal(kept, []string{"Caddyfile"}) {
		t.Fatalf("kept %v", kept)
	}
	if p.read(t, "Caddyfile.new") != p.served["Caddyfile"] {
		t.Fatal("no Caddyfile.new")
	}
	// A compose.new from some earlier refresh, and a directory that only
	// looks like one: not a file the CLI wrote, never touched.
	writeFile(t, filepath.Join(p.dir, "docker-compose.yml.new"), "services: {older: {}}\n")
	if err := os.Mkdir(filepath.Join(p.dir, "clickhouse-users.xml.new"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Run 2 replaces the Caddyfile (--replace-bundle): its .new is stale now,
	// and so is the compose file's (that one is the served version already).
	r, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.20", bundleReplace, false)
	if r == nil {
		t.Fatal("no refresh planned")
	}
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(r.removedNew)
	if !slices.Equal(r.removedNew, []string{"Caddyfile.new", "docker-compose.yml.new"}) {
		t.Fatalf("removed %v", r.removedNew)
	}
	for _, gone := range []string{"Caddyfile.new", "docker-compose.yml.new"} {
		if _, err := os.Lstat(filepath.Join(p.dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", gone, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(p.dir, "clickhouse-users.xml.new")); err != nil || !fi.IsDir() {
		t.Errorf("a directory was removed: %v", err)
	}
}

func TestBundleBackupTag(t *testing.T) {
	// Backups are named after the release the replaced file belongs to: a
	// version the control plane served with a known release is named after
	// it; otherwise the manifest's release when it has one, else the
	// installed tag, else "previous".
	for _, tc := range []struct {
		manifest, installed, want string
		release                   []string
	}{
		{"0.1.19", "staging", "docker-compose.yml.bak-0.1.19", nil},
		{"", "0.1.18", "docker-compose.yml.bak-0.1.18", nil},
		{"", "", "docker-compose.yml.bak-previous", nil},
		{"0.1.20", "0.1.20", "docker-compose.yml.bak-main", []string{"main"}},
		{"", "main", "docker-compose.yml.bak-0.1.17", []string{"0.1.17"}},
	} {
		p := newBundleProject(t, "0.1.20")
		fakeDocker(t)
		withKnownReleases(t, map[string][]string{})
		if tc.release != nil {
			withKnownReleases(t, map[string][]string{sha256Hex([]byte(oldCompose)): tc.release})
		}
		if err := writeBundleManifest(p.dir, bundleManifest{Version: tc.manifest, Files: map[string]string{"docker-compose.yml": sha256Hex([]byte(oldCompose))}}); err != nil {
			t.Fatal(err)
		}
		r, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", tc.installed, bundleAuto, false)
		if r == nil {
			t.Fatal("no refresh planned")
		}
		if err := r.apply(); err != nil {
			t.Fatal(err)
		}
		f, _ := r.file("docker-compose.yml")
		if filepath.Base(f.backup) != tc.want || f.edited {
			t.Errorf("manifest %q installed %q releases %v: backup %q (edited %v), want %s", tc.manifest, tc.installed, tc.release, filepath.Base(f.backup), f.edited, tc.want)
		}
		r.cleanup()
	}
}

func TestPullImagesBeforeSwap(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	if err := os.Mkdir(filepath.Join(p.dir, "clickhouse-users.xml"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := fakeDocker(t, "pull")
	before := p.snapshot(t)
	r, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.19", bundleAuto, false)
	if r == nil {
		t.Fatal("no refresh planned")
	}
	out, err := pullImages(p.dir, p.env, "0.1.20", r)
	if err == nil || !strings.Contains(out, "pull failed") {
		t.Fatalf("the pull must fail: %q, %v", out, err)
	}
	// The pull ran with the NEW compose from the download directory (so new
	// services' images come too), the project as the directory and the
	// candidate tag.
	want := "DP_IMAGE_TAG=0.1.20 docker compose --project-directory " + p.dir + " -f " + filepath.Join(r.tmp, "docker-compose.yml") + " --env-file " + p.env + " pull"
	if last := (*calls)[len(*calls)-1]; last != want {
		t.Fatalf("pull call = %q\nwant        %q", last, want)
	}
	// Nothing in the project was touched; dropping the plan removes the
	// download directory too.
	r.cleanup()
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("a failed pull touched the project:\nbefore %v\nafter  %v", before, after)
	}

	// Without a new compose file the pull is the project's own.
	calls = fakeDocker(t)
	if _, err := pullImages(p.dir, p.env, "0.1.20", nil); err != nil {
		t.Fatal(err)
	}
	if got := (*calls)[0]; got != "DP_IMAGE_TAG=0.1.20 docker compose --project-directory "+p.dir+" --env-file "+p.env+" pull" {
		t.Fatalf("pull call = %q", got)
	}
}

func TestBundleRefreshRollback(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	if err := os.Mkdir(filepath.Join(p.dir, "clickhouse-users.xml"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDocker(t)
	before := p.snapshot(t)
	r, _ := planBundleRefresh(p.dir, p.env, p.base, "0.1.20", "0.1.19", bundleAuto, false)
	if r == nil {
		t.Fatal("no refresh planned")
	}
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	if p.read(t, "docker-compose.yml") == oldCompose {
		t.Fatal("apply did not swap the compose file")
	}
	if err := r.rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// Exactly the project as it was: old files back (the empty directory
	// too), backups and the added file gone, no download directory.
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("rollback did not restore the project:\nbefore %v\nafter  %v", before, after)
	}
}

func TestComposeFileArgs(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	writeFile(t, env, "DATA_PLANE_ID=dp-1\n")
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("COMPOSE_PATH_SEPARATOR", "")
	tmp := filepath.Join(dir, bundleTempPrefix+"1", "docker-compose.yml")
	if got := composeFileArgs(dir, env, tmp); !slices.Equal(got, []string{"-f", tmp}) {
		t.Fatalf("plain project: %v", got)
	}
	// compose loads an override file next to docker-compose.yml on its own;
	// the new file must be tried with it.
	writeFile(t, filepath.Join(dir, "docker-compose.override.yml"), "services: {}\n")
	if got := composeFileArgs(dir, env, tmp); !slices.Equal(got, []string{"-f", tmp, "-f", filepath.Join(dir, "docker-compose.override.yml")}) {
		t.Fatalf("override: %v", got)
	}
	// COMPOSE_FILE in .env names the files instead, relative to the project.
	writeFile(t, env, "DATA_PLANE_ID=dp-1\nCOMPOSE_FILE=docker-compose.yml:extra.yml\n")
	if got := composeFileArgs(dir, env, tmp); !slices.Equal(got, []string{"-f", tmp, "-f", filepath.Join(dir, "extra.yml")}) {
		t.Fatalf("COMPOSE_FILE in .env: %v", got)
	}
	// The process environment wins over .env, as in compose.
	t.Setenv("COMPOSE_FILE", "/elsewhere/base.yml;./docker-compose.yml")
	t.Setenv("COMPOSE_PATH_SEPARATOR", ";")
	if got := composeFileArgs(dir, env, tmp); !slices.Equal(got, []string{"-f", "/elsewhere/base.yml", "-f", tmp}) {
		t.Fatalf("COMPOSE_FILE in the environment: %v", got)
	}

	// validate and the pull use it.
	t.Setenv("COMPOSE_FILE", "")
	writeFile(t, env, "DATA_PLANE_ID=dp-1\n")
	r := &bundleRefresh{dir: dir, tmp: filepath.Dir(tmp), files: []bundleFile{{name: "docker-compose.yml", state: bundlePristine}}}
	calls := fakeDocker(t)
	if err := r.validate(env, "0.1.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := pullImages(dir, env, "0.1.20", r); err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if !strings.Contains(c, " -f "+tmp+" -f "+filepath.Join(dir, "docker-compose.override.yml")+" ") {
			t.Errorf("call without the override: %q", c)
		}
	}
}

func TestRemoveStaleBundleDirs(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{".origamy-bundle-123", ".origamy-bundle-4294967295", ".origamy-bundle-x", "origamy-bundle-1", ".origamy-bundle-"} {
		if err := os.MkdirAll(filepath.Join(dir, d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Not directories, or not ours: never touched.
	writeFile(t, filepath.Join(dir, bundleManifestFile), "{}\n")
	writeFile(t, filepath.Join(dir, ".origamy-bundle-77"), "a file\n")
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, ".origamy-bundle-88")); err != nil {
		t.Fatal(err)
	}
	got := removeStaleBundleDirs(dir)
	slices.Sort(got)
	if !slices.Equal(got, []string{".origamy-bundle-123", ".origamy-bundle-4294967295"}) {
		t.Fatalf("removed %v", got)
	}
	for _, keep := range []string{".origamy-bundle-x", "origamy-bundle-1", ".origamy-bundle-", bundleManifestFile, ".origamy-bundle-77", ".origamy-bundle-88"} {
		if _, err := os.Lstat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("a symlink's target must survive")
	}
}

func TestBundleRestarts(t *testing.T) {
	r := &bundleRefresh{files: []bundleFile{
		{name: "clickhouse-users.xml", state: bundleUnchanged},
		{name: "Caddyfile", state: bundlePristine},
	}}
	if got := r.restarts([]string{"full"}); len(got) != 0 {
		t.Fatalf("Caddy is not running without the ingress profile: %v", got)
	}
	if got := r.restarts([]string{"full", "ingress"}); !slices.Equal(got, []string{"caddy"}) {
		t.Fatalf("got %v", got)
	}
	r.files[0].state = bundleMissing
	if got := r.restarts(nil); !slices.Equal(got, []string{"clickhouse"}) {
		t.Fatalf("got %v", got)
	}
}

func TestBundleManifest(t *testing.T) {
	dir := t.TempDir()
	if m := readBundleManifest(dir); m.Version != "" || len(m.Files) != 0 {
		t.Fatalf("no manifest must read as empty: %+v", m)
	}
	writeFile(t, filepath.Join(dir, bundleManifestFile), "{not json")
	if m := readBundleManifest(dir); m.Version != "" || len(m.Files) != 0 {
		t.Fatalf("a corrupt manifest must read as empty: %+v", m)
	}

	// A deploy takes the bundle as a refresh that replaces everything; the
	// manifest records every file, and their release only when the control
	// plane says which it is.
	served := map[string]string{}
	for _, name := range bundleFiles {
		served[name] = name + " body\n"
	}
	base := fakeControlPlane(t, served)
	fakeDocker(t)
	for _, version := range []string{"", "0.1.20"} {
		r, err := prepareBundleRefresh(dir, base, version, "", true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.apply(); err != nil {
			t.Fatal(err)
		}
		if err := r.commit(); err != nil {
			t.Fatal(err)
		}
	}
	m := readBundleManifest(dir)
	if m.Version != "0.1.20" || len(m.Files) != len(bundleFiles) || m.Files["Caddyfile"] != sha256Hex([]byte("Caddyfile body\n")) {
		t.Fatalf("deploy manifest = %+v", m)
	}
	raw := readFile(t, filepath.Join(dir, bundleManifestFile))
	if !strings.Contains(raw, `"version": "0.1.20"`) || !strings.Contains(raw, `"files": {`) {
		t.Fatalf("manifest format: %s", raw)
	}
	if len(m.Releases) != len(bundleFiles) || m.Releases["Caddyfile"] != "0.1.20" {
		t.Fatalf("deploy manifest releases = %+v", m.Releases)
	}
	unknown := t.TempDir()
	r, err := prepareBundleRefresh(unknown, base, "", "", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	if u := readBundleManifest(unknown); u.Version != "" || len(u.Files) != len(bundleFiles) || len(u.Releases) != 0 {
		t.Fatalf("a bundle of no known release records none: %+v", u)
	}

	// The storage reset rewrites the schema outside a refresh, from a control
	// plane that does not say which release it is.
	writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), "new schema\n")
	if err := recordBundleFile(dir, "clickhouse-init.sql", ""); err != nil {
		t.Fatal(err)
	}
	m2 := readBundleManifest(dir)
	if m2.Files["clickhouse-init.sql"] != sha256Hex([]byte("new schema\n")) || m2.Files["Caddyfile"] != m.Files["Caddyfile"] {
		t.Fatalf("after recordBundleFile: %+v", m2)
	}
	if _, ok := m2.Releases["clickhouse-init.sql"]; ok || m2.Releases["Caddyfile"] != "0.1.20" {
		t.Fatalf("after recordBundleFile, releases: %+v", m2.Releases)
	}
	if _, err := os.Stat(filepath.Join(dir, bundleManifestFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary manifest left behind")
	}
}

func TestWriteBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Caddyfile")
	for i, want := range []string{"Caddyfile.bak-previous", "Caddyfile.bak-previous.1", "Caddyfile.bak-previous.2"} {
		bak, err := writeBackup(path, []byte(fmt.Sprint(i)), 0o600, backupTag(""))
		if err != nil || filepath.Base(bak) != want {
			t.Fatalf("backup %d = %q, %v; want %s", i, bak, err, want)
		}
		if fi, _ := os.Stat(bak); fi.Mode().Perm() != 0o600 {
			t.Fatalf("backup mode = %v", fi.Mode().Perm())
		}
	}
	if got := backupTag("sha-abc/../x"); got != "sha-abc_.._x" {
		t.Fatalf("backupTag = %q", got)
	}
}

func TestBundleModeFlags(t *testing.T) {
	if m, err := bundleModeFlags(false, false); err != nil || m != bundleAuto {
		t.Fatal("default must be auto")
	}
	if m, _ := bundleModeFlags(true, false); m != bundleReplace {
		t.Fatal("--replace-bundle")
	}
	if m, _ := bundleModeFlags(false, true); m != bundleKeep {
		t.Fatal("--keep-bundle")
	}
	if _, err := bundleModeFlags(true, true); err == nil {
		t.Fatal("both flags must be refused")
	}
}

func TestBundleBoxLines(t *testing.T) {
	if got := strings.Join(bundleBoxLines(nil, "--keep-bundle", nil), "\n"); !strings.Contains(got, "left as-is (--keep-bundle)") {
		t.Fatalf("got %q", got)
	}
	// The storage reset wrote clickhouse-init.sql although the bundle was not
	// refreshed: the box says exactly that, never "left as-is".
	w := &resetSchemaWrite{backup: "/dp/clickhouse-init.sql.bak-main", edited: true}
	got := strings.Join(bundleBoxLines(nil, "--keep-bundle", w), "\n")
	if strings.Contains(got, "left as-is") || !strings.Contains(got, "not refreshed (--keep-bundle)") ||
		!strings.Contains(got, "replaced your edited clickhouse-init.sql") || !strings.Contains(got, "clickhouse-init.sql.bak-main") {
		t.Fatalf("got %q", got)
	}
	if got := strings.Join(bundleBoxLines(nil, "control plane serves 0.1.19", &resetSchemaWrite{}), "\n"); strings.Contains(got, "left as-is") || !strings.Contains(got, "clickhouse-init.sql replaced with your control plane's current schema") {
		t.Fatalf("got %q", got)
	}
	r := &bundleRefresh{version: "0.1.20", files: []bundleFile{
		{name: "docker-compose.yml", state: bundlePristine},
		{name: "Caddyfile", state: bundleModified},
		{name: "clickhouse-users.xml", state: bundleMissing},
	}}
	got = strings.Join(bundleBoxLines(r, "", nil), "\n")
	if !strings.Contains(got, "0.1.20") || !strings.Contains(got, "(docker-compose.yml, clickhouse-users.xml)") || !strings.Contains(got, "see Caddyfile.new") {
		t.Fatalf("got %q", got)
	}
	r.files = r.files[1:2]
	if got := strings.Join(bundleBoxLines(r, "", nil), "\n"); !strings.Contains(got, "0.1.20: nothing replaced") || !strings.Contains(got, "see Caddyfile.new") {
		t.Fatalf("got %q", got)
	}
	r.files[0].state = bundleUnchanged
	if got := strings.Join(bundleBoxLines(r, "", nil), "\n"); !strings.Contains(got, "already 0.1.20") {
		t.Fatalf("got %q", got)
	}
}

func TestKnownBundleHashes(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, name := range bundleFiles {
		sums := knownBundleHashes[name]
		if len(sums) == 0 {
			t.Errorf("%s: no known versions", name)
		}
		seen := map[string]bool{}
		for _, s := range sums {
			if !hex64.MatchString(s) || seen[s] {
				t.Errorf("%s: bad or duplicate hash %q", name, s)
			}
			seen[s] = true
		}
	}
	if len(knownBundleHashes) != len(bundleFiles) {
		t.Errorf("table covers %d files, the bundle has %d", len(knownBundleHashes), len(bundleFiles))
	}
	// Every release entry is for a known version, and names releases.
	known := map[string]bool{}
	for _, sums := range knownBundleHashes {
		for _, s := range sums {
			known[s] = true
		}
	}
	for sum, rels := range knownBundleReleases {
		if !known[sum] {
			t.Errorf("releases for unknown version %s", sum)
		}
		if len(rels) == 0 || slices.ContainsFunc(rels, func(r string) bool { return !reBundleVersion.MatchString(r) }) {
			t.Errorf("%s: bad releases %q", sum, rels)
		}
	}
	// The current bundle is the pinned release's: a project deployed by this
	// CLI backs it up under that name.
	compose := knownBundleHashes["docker-compose.yml"]
	if rels := knownBundleReleases[compose[len(compose)-1]]; !slices.Contains(rels, helmVersion) {
		t.Errorf("the newest docker-compose.yml is for %v, not %s", rels, helmVersion)
	}
}

// TestKnownBundleHashesUpToDate regenerates the table from a control-plane
// checkout (ORIGAMY_CONTROL_PLANE=/path/to/origamy-control-plane) and fails
// when the committed one is stale: run scripts/gen-bundle-hashes.sh.
func TestKnownBundleHashesUpToDate(t *testing.T) {
	cp := os.Getenv("ORIGAMY_CONTROL_PLANE")
	if cp == "" {
		t.Skip("set ORIGAMY_CONTROL_PLANE to a control-plane checkout to check the generated table")
	}
	out, err := exec.Command("bash", "../../../scripts/gen-bundle-hashes.sh", cp, "-").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if string(out) != readFile(t, "bundle_hashes.go") {
		t.Fatal("bundle_hashes.go is stale: run scripts/gen-bundle-hashes.sh " + cp)
	}
}

func TestBackfillDockerEnv(t *testing.T) {
	const base = "https://cp.example.com"
	// What the 0.1.20 bundle reads (deploy/byod/docker-compose.yml there).
	compose := "services:\n  config-sync:\n    environment:\n      CONTROL_PLANE_URL: \"${CONFIG_URL}\"\n      TELEMETRY_URL: \"${TELEMETRY_URL}\"\n" +
		"  workflow-engine:\n    environment:\n      CONTROL_PLANE_URL: \"${CONTROL_PLANE_HTTP_URL}\"\n"
	current := "CONTROL_PLANE_HTTP_URL=" + base + "\nCONFIG_URL=" + base + "/api/v1/config\nTELEMETRY_URL=" + base + "/api/v1/telemetry\n"
	for _, tc := range []struct {
		name, env, compose string
		want               map[string]string // values after; "-" = absent
		notes              int
	}{
		{"v0.1.18 .env", v018Env(base), compose, map[string]string{
			"CONTROL_PLANE_HTTP_URL": base, "CONFIG_URL": base + "/api/v1/config", "TELEMETRY_URL": base + "/api/v1/telemetry"}, 3},
		{"v0.1.18 .env, quoted with a trailing slash", strings.Replace(v018Env(base), "CONFIG_URL="+base, `CONFIG_URL="`+base+`/"`, 1), compose, map[string]string{
			"CONFIG_URL": base + "/api/v1/config"}, 3},
		{"dashboard snippet: the endpoint is kept", "CONFIG_URL=" + base + "/api/v1/config\n", compose, map[string]string{
			"CONTROL_PLANE_HTTP_URL": base, "CONFIG_URL": base + "/api/v1/config", "TELEMETRY_URL": base + "/api/v1/telemetry"}, 2},
		{"current CLI: nothing to do", current, compose, nil, 0},
		{"telemetry turned off stays off", "CONTROL_PLANE_HTTP_URL=" + base + "\nCONFIG_URL=" + base + "/api/v1/config\nTELEMETRY_URL=\n", compose, map[string]string{"TELEMETRY_URL": ""}, 0},
		{"empty CONFIG_URL", "CONTROL_PLANE_HTTP_URL=" + base + "\nCONFIG_URL=\n", compose, map[string]string{"CONFIG_URL": base + "/api/v1/config"}, 2},
		{"a CONFIG_URL with its own path is kept", "CONTROL_PLANE_HTTP_URL=" + base + "\nCONFIG_URL=https://proxy.example.com/origamy/config\n", compose, map[string]string{
			"CONFIG_URL": "https://proxy.example.com/origamy/config"}, 1},
		{"compose without telemetry", v018Env(base), "services:\n  config-sync:\n    environment:\n      CONTROL_PLANE_URL: ${CONFIG_URL:?}\n", map[string]string{
			"TELEMETRY_URL": "-", "CONFIG_URL": base + "/api/v1/config"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env := filepath.Join(dir, ".env")
			writeFile(t, env, tc.env)
			writeFile(t, filepath.Join(dir, "docker-compose.yml"), tc.compose)
			b, _ := controlPlaneHTTPBase(env)
			notes, err := backfillDockerEnv(dir, env, b)
			if err != nil {
				t.Fatal(err)
			}
			if len(notes) != tc.notes {
				t.Errorf("notes %q, want %d", notes, tc.notes)
			}
			if tc.notes == 0 && readFile(t, env) != tc.env {
				t.Errorf("an .env with nothing to add was rewritten:\n%s", readFile(t, env))
			}
			for k, v := range tc.want {
				if v == "-" {
					if envHasKey(env, k) {
						t.Errorf("%s added", k)
					}
				} else if got := readEnvVar(env, k); got != v || !envHasKey(env, k) {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			// Nothing else moves.
			for _, l := range strings.Split(tc.env, "\n") {
				k, _, ok := strings.Cut(l, "=")
				if !ok || k == "CONFIG_URL" || k == "CONTROL_PLANE_HTTP_URL" {
					continue
				}
				if !strings.Contains(readFile(t, env), l+"\n") {
					t.Errorf("line %q changed", l)
				}
			}
			// A second run has nothing left to do.
			if again, _ := backfillDockerEnv(dir, env, b); len(again) != 0 {
				t.Errorf("second run: %q", again)
			}
		})
	}
	if notes, err := backfillDockerEnv(t.TempDir(), filepath.Join(t.TempDir(), ".env"), ""); notes != nil || err != nil {
		t.Fatalf("no control plane URL: %q, %v", notes, err)
	}
}

func TestWriteResetSchema(t *testing.T) {
	newSQL := []byte("CREATE TABLE events (properties JSON);\n")
	withKnownHashes(t, map[string][]string{"clickhouse-init.sql": {sha256Hex([]byte("served old\n"))}})
	withKnownReleases(t, map[string][]string{sha256Hex([]byte("served old\n")): {"0.1.19"}})
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T, path string)
		backup     string // "" = none
		edited     bool
		backupBody string
	}{
		{"a served version", func(t *testing.T, p string) { writeFile(t, p, "served old\n") }, "clickhouse-init.sql.bak-0.1.19", false, "served old\n"},
		{"edited here", func(t *testing.T, p string) { writeFile(t, p, "mine\n") }, "clickhouse-init.sql.bak-0.1.18", true, "mine\n"},
		{"already the new schema", func(t *testing.T, p string) { writeFile(t, p, string(newSQL)) }, "", false, ""},
		{"missing", func(*testing.T, string) {}, "", false, ""},
		{"the empty directory Docker leaves", func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "clickhouse-init.sql")
			tc.setup(t, path)
			if why := resetSchemaProblem(dir); why != "" {
				t.Fatalf("refused: %s", why)
			}
			w, err := writeResetSchema(dir, newSQL, "0.1.18")
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(w.backup) != tc.backup && !(tc.backup == "" && w.backup == "") || w.edited != tc.edited {
				t.Fatalf("got %+v", w)
			}
			if tc.backup != "" && readFile(t, w.backup) != tc.backupBody {
				t.Fatalf("backup holds %q", readFile(t, w.backup))
			}
			if readFile(t, path) != string(newSQL) || readBundleManifest(dir).Files["clickhouse-init.sql"] != sha256Hex(newSQL) {
				t.Fatal("not written, or not recorded")
			}
		})
	}
	// A link or a populated directory: refused, and never written through.
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.sql")
	writeFile(t, target, "theirs\n")
	if err := os.Symlink(target, filepath.Join(dir, "clickhouse-init.sql")); err != nil {
		t.Fatal(err)
	}
	if why := resetSchemaProblem(dir); !strings.Contains(why, "symbolic link (to "+target+")") {
		t.Fatalf("why = %q", why)
	}
	if _, err := writeResetSchema(dir, newSQL, "0.1.18"); err == nil || readFile(t, target) != "theirs\n" {
		t.Fatalf("wrote through the link: %v", err)
	}
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "clickhouse-init.sql", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if why := resetSchemaProblem(dir); !strings.Contains(why, "not a regular file") {
		t.Fatalf("why = %q", why)
	}
}
