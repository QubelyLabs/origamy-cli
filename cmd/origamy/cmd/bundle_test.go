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

// fakeDocker stands in for dockerRun: it records every call and fails those
// whose arguments contain a word in failOn.
func fakeDocker(t *testing.T, failOn ...string) *[]string {
	t.Helper()
	var calls []string
	prev := dockerRun
	dockerRun = func(env []string, name string, args ...string) (string, error) {
		call := strings.Join(append(append(env, name), args...), " ")
		calls = append(calls, call)
		for _, w := range failOn {
			if slices.Contains(args, w) {
				return "boom: " + w + " failed", errors.New("exit status 1")
			}
		}
		return "", nil
	}
	t.Cleanup(func() { dockerRun = prev })
	return &calls
}

// withKnownHashes replaces the generated table for one test.
func withKnownHashes(t *testing.T, table map[string][]string) {
	t.Helper()
	prev := knownBundleHashes
	knownBundleHashes = table
	t.Cleanup(func() { knownBundleHashes = prev })
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
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
		// The manifest is the record for a file it covers: a known served
		// version that differs from it is still an edit.
		{"manifest wins over the known table", reg(old), recorded, false, bundleModified},
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
		{"older control plane", "", errNotServed, "0.1.20", "predates /byod/VERSION"},
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
			r, skipped := planBundleRefresh(p.dir, p.env, tc.tag, "0.1.19", tc.mode, false)
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
	r, skipped := planBundleRefresh(p.dir, p.env, "0.1.20", "0.1.19", bundleAuto, false)
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
	if r, skipped := planBundleRefresh(p.dir, p.env, "0.1.17", "0.1.16", bundleAuto, true); r != nil || skipped != "new bundle failed validation" {
		t.Fatalf("got %v, %q", r, skipped)
	}
}

func TestPlanBundleRefreshResetForcesSchema(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	writeFile(t, filepath.Join(p.dir, "clickhouse-init.sql"), "-- my edit\n")
	fakeDocker(t)
	r, _ := planBundleRefresh(p.dir, p.env, "0.1.20", "0.1.17", bundleAuto, false)
	if r == nil || r.served("clickhouse-init.sql") {
		t.Fatalf("without the reset an edited schema is kept: %+v", r)
	}
	r.cleanup()
	r, _ = planBundleRefresh(p.dir, p.env, "0.1.20", "0.1.17", bundleAuto, true)
	if r == nil || !r.changes("clickhouse-init.sql") || !r.served("clickhouse-init.sql") {
		t.Fatalf("across the reset the new schema is forced in: %+v", r)
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

	r, _ := planBundleRefresh(p.dir, p.env, "0.1.20", "0.1.19", bundleAuto, false)
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
	r2, _ := planBundleRefresh(p.dir, p.env, "0.1.20", "0.1.20", bundleAuto, false)
	if r2 == nil || len(r2.changed()) != 0 || !slices.Equal(r2.kept(), []string{"Caddyfile"}) {
		t.Fatalf("second refresh: changed %v kept %v", r2.changed(), r2.kept())
	}
	r2.cleanup()

	// --replace-bundle takes the edit out too, backing it up.
	r3, _ := planBundleRefresh(p.dir, p.env, "0.1.20", "0.1.20", bundleReplace, false)
	if r3 == nil || !slices.Equal(r3.changed(), []string{"Caddyfile"}) {
		t.Fatalf("--replace-bundle: changed %v", r3.changed())
	}
	if err := r3.apply(); err != nil {
		t.Fatal(err)
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

func TestPullImagesRollsBackOnFailure(t *testing.T) {
	p := newBundleProject(t, "0.1.20")
	if err := os.Mkdir(filepath.Join(p.dir, "clickhouse-users.xml"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := fakeDocker(t, "pull")
	before := p.snapshot(t)
	r, _ := planBundleRefresh(p.dir, p.env, "0.1.20", "0.1.19", bundleAuto, false)
	if r == nil {
		t.Fatal("no refresh planned")
	}
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	if p.read(t, "docker-compose.yml") == oldCompose {
		t.Fatal("apply did not swap the compose file")
	}
	out, err, restoreErr := pullImages(p.dir, p.env, "0.1.20", r)
	if err == nil || !strings.Contains(out, "pull failed") {
		t.Fatalf("the pull must fail: %q, %v", out, err)
	}
	if restoreErr != nil {
		t.Fatalf("rollback: %v", restoreErr)
	}
	// The pull ran with the NEW compose (so new services' images come too)
	// and the candidate tag.
	if last := (*calls)[len(*calls)-1]; !strings.HasPrefix(last, "DP_IMAGE_TAG=0.1.20 docker compose --project-directory "+p.dir) || !strings.HasSuffix(last, " pull") {
		t.Fatalf("pull call = %q", last)
	}
	// Exactly the project as it was: old files back (the empty directory
	// too), backups and the added file gone, no download directory.
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("rollback did not restore the project:\nbefore %v\nafter  %v", before, after)
	}
	// Nothing to commit after a rollback; a successful pull with no refresh
	// is just the pull.
	fakeDocker(t)
	if _, err, _ := pullImages(p.dir, p.env, "0.1.20", nil); err != nil {
		t.Fatal(err)
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

	for _, name := range bundleFiles {
		writeFile(t, filepath.Join(dir, name), name+" body\n")
	}
	if err := recordDeployedBundle(dir, ""); err != nil {
		t.Fatal(err)
	}
	m := readBundleManifest(dir)
	if m.Version != "" || len(m.Files) != len(bundleFiles) || m.Files["Caddyfile"] != sha256Hex([]byte("Caddyfile body\n")) {
		t.Fatalf("deploy manifest = %+v", m)
	}
	raw := readFile(t, filepath.Join(dir, bundleManifestFile))
	if !strings.Contains(raw, `"version": ""`) || !strings.Contains(raw, `"files": {`) {
		t.Fatalf("manifest format: %s", raw)
	}

	// The storage reset rewrites the schema outside a refresh.
	writeFile(t, filepath.Join(dir, "clickhouse-init.sql"), "new schema\n")
	if err := recordBundleFile(dir, "clickhouse-init.sql"); err != nil {
		t.Fatal(err)
	}
	m2 := readBundleManifest(dir)
	if m2.Files["clickhouse-init.sql"] != sha256Hex([]byte("new schema\n")) || m2.Files["Caddyfile"] != m.Files["Caddyfile"] {
		t.Fatalf("after recordBundleFile: %+v", m2)
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
	if got := strings.Join(bundleBoxLines(nil, "--keep-bundle"), "\n"); !strings.Contains(got, "left as-is (--keep-bundle)") {
		t.Fatalf("got %q", got)
	}
	r := &bundleRefresh{version: "0.1.20", files: []bundleFile{
		{name: "docker-compose.yml", state: bundlePristine},
		{name: "Caddyfile", state: bundleModified},
		{name: "clickhouse-users.xml", state: bundleMissing},
	}}
	got := strings.Join(bundleBoxLines(r, ""), "\n")
	if !strings.Contains(got, "0.1.20") || !strings.Contains(got, "(docker-compose.yml, clickhouse-users.xml)") || !strings.Contains(got, "see Caddyfile.new") {
		t.Fatalf("got %q", got)
	}
	r.files = r.files[1:2]
	if got := strings.Join(bundleBoxLines(r, ""), "\n"); !strings.Contains(got, "0.1.20: nothing replaced") || !strings.Contains(got, "see Caddyfile.new") {
		t.Fatalf("got %q", got)
	}
	r.files[0].state = bundleUnchanged
	if got := strings.Join(bundleBoxLines(r, ""), "\n"); !strings.Contains(got, "already 0.1.20") {
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
