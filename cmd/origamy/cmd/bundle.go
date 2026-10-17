package cmd

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── Docker deploy bundle ─────────────────────────────────────────────────────
// A Docker data plane runs the compose bundle its control plane serves at
// /byod/* — the data-plane repo is private, so the control plane is the only
// source. `origamy deploy` downloads it (next to the project, swapped in once
// the deploy can go ahead, every file it replaces backed up); `origamy
// upgrade` refreshes it, but only when the control plane serves the bundle
// for exactly the release being moved to (/byod/VERSION), and only the files
// the operator has not edited.

// The known-version table is generated from the control plane's git history.
//go:generate bash ../../../scripts/gen-bundle-hashes.sh

// bundleFiles is the deploy bundle, in download order.
var bundleFiles = []string{"docker-compose.yml", "clickhouse-init.sql", "clickhouse-users.xml", "Caddyfile"}

// bundleManifestFile records in the project directory which release the
// bundle files came from and their sha256 as written, so a later refresh can
// tell a file the CLI wrote from one the operator edited since.
const bundleManifestFile = ".origamy-bundle.json"

type bundleManifest struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
	// Releases is the release each file came with, where known. An edited
	// file a refresh keeps keeps the release of the version it was made from,
	// which Version (the last refresh's) is not. Absent in manifests from
	// earlier CLIs.
	Releases map[string]string `json:"releases,omitempty"`
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readBundleManifest returns the project's manifest. A missing or unreadable
// one is empty, so every file falls back to the versions the control plane is
// known to have served.
func readBundleManifest(dir string) bundleManifest {
	var m bundleManifest
	if b, err := os.ReadFile(filepath.Join(dir, bundleManifestFile)); err == nil {
		if json.Unmarshal(b, &m) != nil {
			m = bundleManifest{}
		}
	}
	return m
}

func writeBundleManifest(dir string, m bundleManifest) error {
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, bundleManifestFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, bundleManifestFile))
}

// recordBundleFile updates one file's entry in dir's manifest after it was
// rewritten from the control plane outside a refresh (the storage reset's
// schema fetch), so the next upgrade does not take it for an edit. release
// is the release it came with, "" when unknown.
func recordBundleFile(dir, name, release string) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	m := readBundleManifest(dir)
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	m.Files[name] = sha256Hex(b)
	if release != "" {
		if m.Releases == nil {
			m.Releases = map[string]string{}
		}
		m.Releases[name] = release
	} else {
		delete(m.Releases, name)
	}
	return writeBundleManifest(dir, m)
}

// resetSchemaWrite is what writeResetSchema did to clickhouse-init.sql.
type resetSchemaWrite struct {
	// backup is where the previous file went ("" when there was none, or it
	// already held the new schema); edited, whether it was edited here.
	backup string
	edited bool
}

// resetSchemaProblem says why the storage reset cannot write its schema to
// the project's clickhouse-init.sql ("is …"), "" when it can: a symbolic link
// (the reset never writes through one) or a directory with something in it
// is the operator's own arrangement.
func resetSchemaProblem(dir string) string {
	path := filepath.Join(dir, "clickhouse-init.sql")
	cur, err := readBundleCurrent(path)
	switch {
	case err != nil:
		return "cannot be read (" + err.Error() + ")"
	case !cur.own:
		return ""
	}
	if target, err := os.Readlink(path); err == nil {
		return "is a symbolic link (to " + target + "), and the reset does not write through a link"
	}
	return "is not a regular file"
}

// writeResetSchema puts sql, the schema the storage reset applies, in the
// project's clickhouse-init.sql (the bundle's init service reads it from
// there) when the bundle refresh did not. The file there is backed up first
// as <file>.bak-<the release it came with>, edited or not, like a refresh
// does; resetSchemaProblem has ruled out a link or a directory.
func writeResetSchema(dir string, sql []byte, installedTag string) (resetSchemaWrite, error) {
	const name = "clickhouse-init.sql"
	// Not cut in half by an interrupt.
	exitGate.Lock()
	defer exitGate.Unlock()
	path := filepath.Join(dir, name)
	cur, err := readBundleCurrent(path)
	if err != nil {
		return resetSchemaWrite{}, err
	}
	if cur.own {
		return resetSchemaWrite{}, fmt.Errorf("%s is not a regular file", path)
	}
	m := readBundleManifest(dir)
	var w resetSchemaWrite
	mode := os.FileMode(0o644)
	switch {
	case cur.emptyDir:
		if err := os.Remove(path); err != nil {
			return w, err
		}
	case cur.exists && !bytes.Equal(cur.data, sql):
		w.edited = !bundlePristineSum(name, sha256Hex(cur.data), m)
		if w.backup, err = writeBackup(path, cur.data, cur.mode, backupTag(backupRelease(name, cur.data, m, installedTag))); err != nil {
			return resetSchemaWrite{}, fmt.Errorf("back up %s: %w", name, err)
		}
		mode = cur.mode
	}
	if err := os.WriteFile(path, sql, mode); err != nil {
		return w, err
	}
	// The control plane's file now, not an edit, for the next upgrade.
	_ = recordBundleFile(dir, name, "")
	return w, nil
}

// controlPlaneHTTPBase returns the control plane's base URL from a Docker
// project's .env: CONTROL_PLANE_HTTP_URL, or for a project deployed by CLI
// v0.1.18 or older, which wrote no such key, CONFIG_URL (the bare base there,
// the config endpoint under it in the dashboard's snippet). fromConfigURL
// reports that fallback; base is "" when neither holds an http(s) URL.
func controlPlaneHTTPBase(envPath string) (base string, fromConfigURL bool) {
	if v := strings.TrimRight(unquoteEnv(readEnvVar(envPath, "CONTROL_PLANE_HTTP_URL")), "/"); v != "" {
		return v, false
	}
	v := strings.TrimRight(unquoteEnv(readEnvVar(envPath, "CONFIG_URL")), "/")
	v = strings.TrimRight(strings.TrimSuffix(v, configPath), "/")
	if u, err := url.Parse(v); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", false
	}
	return v, true
}

// configPath and telemetryPath are where config-sync and its telemetry
// reporter reach the control plane. Both take the FULL endpoint URL
// (CONFIG_URL, TELEMETRY_URL), which renderDockerEnv writes from the base.
const (
	configPath    = "/api/v1/config"
	telemetryPath = "/api/v1/telemetry"
)

// backfillDockerEnv brings an .env from CLI v0.1.18 or older up to what
// renderDockerEnv writes for the control plane at base, without changing a
// value set here: CONTROL_PLANE_HTTP_URL when it is empty (the CLI reads it
// for the next refresh); CONFIG_URL when it is empty and TELEMETRY_URL when
// it is absent (empty turns telemetry off), those two only when the
// project's compose file reads them. Those CLIs also wrote CONFIG_URL as the
// bare control-plane URL, which config-sync requests as it is (it never
// appends the endpoint), so such a plane has never received its config:
// that one value is corrected. It returns a line for each change.
func backfillDockerEnv(dir, envPath, base string) ([]string, error) {
	if base == "" {
		return nil, nil
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	reads := func(key string) bool {
		return regexp.MustCompile(`\$\{` + regexp.QuoteMeta(key) + `[}:?-]`).Match(compose)
	}
	var notes []string
	set := func(key, val, note string) error {
		if err := setEnvVar(envPath, key, val); err != nil {
			return err
		}
		notes = append(notes, note)
		return nil
	}
	if readEnvVar(envPath, "CONTROL_PLANE_HTTP_URL") == "" {
		if err := set("CONTROL_PLANE_HTTP_URL", base, "Recorded CONTROL_PLANE_HTTP_URL="+base+" in .env."); err != nil {
			return notes, err
		}
	}
	switch cfg := strings.TrimRight(unquoteEnv(readEnvVar(envPath, "CONFIG_URL")), "/"); {
	case !reads("CONFIG_URL"):
	case cfg == "":
		if err := set("CONFIG_URL", base+configPath, "Recorded CONFIG_URL="+base+configPath+" in .env."); err != nil {
			return notes, err
		}
	case isBareURL(cfg):
		if err := set("CONFIG_URL", cfg+configPath, "Corrected CONFIG_URL to "+cfg+configPath+" in .env: config-sync requests it as it is, and the bare control-plane URL CLI v0.1.18 and older wrote never returned this plane's config."); err != nil {
			return notes, err
		}
	}
	if reads("TELEMETRY_URL") && !envHasKey(envPath, "TELEMETRY_URL") {
		if err := set("TELEMETRY_URL", base+telemetryPath, "Recorded TELEMETRY_URL="+base+telemetryPath+" in .env, so the dashboard gets this plane's health."); err != nil {
			return notes, err
		}
	}
	return notes, nil
}

// isBareURL reports whether v is an http(s) URL with no path.
func isBareURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && strings.Trim(u.Path, "/") == "" && u.RawQuery == ""
}

// unquoteEnv strips the quotes compose's dotenv parser would.
func unquoteEnv(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// reBundleVersion is what /byod/VERSION may hold: one release name.
var reBundleVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)

// servedBundleVersion asks the control plane at base which data-plane release
// the bundle it serves belongs to (/byod/VERSION, plain text). A control
// plane from before that file answers errNotServed.
func servedBundleVersion(base string) (string, error) {
	b, err := fetchBundleBytes(base, "VERSION")
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if !reBundleVersion.MatchString(v) {
		return "", fmt.Errorf("unexpected /byod/VERSION %q", v[:min(len(v), 40)])
	}
	return v, nil
}

// bundleVersionProblem says why the served bundle must not replace the
// project's files for a move to tag, "" when it is tag's bundle. The bundle
// follows the control plane's own release pin, so an older or newer
// --version, or the edge channel, keeps the files the project has.
func bundleVersionProblem(served string, err error, tag string) string {
	switch {
	case errors.Is(err, errNotServed):
		return "your control plane predates /byod/VERSION, so it cannot say which release its bundle is for; re-run `origamy upgrade` after your control plane is updated to refresh them"
	case err != nil:
		return "could not ask the control plane which release its bundle is for: " + err.Error()
	case served != tag:
		return fmt.Sprintf("your control plane serves the bundle for %s, not %s", served, tag)
	}
	return ""
}

// bundleState is what a refresh does with one bundle file.
type bundleState int

const (
	bundleUnchanged bundleState = iota // already the served bytes: nothing
	bundlePristine                     // as the CLI wrote it: back up, replace
	bundleModified                     // edited here: keep, write <name>.new
	bundleMissing                      // absent: write it
)

// bundleCurrent is what the project holds at a bundle file's path.
type bundleCurrent struct {
	data   []byte
	mode   os.FileMode
	exists bool
	// emptyDir: Docker creates an empty directory for a missing bind-mount
	// source, so that counts as a missing file. own: a symlink or a populated
	// directory is the operator's own arrangement and is never replaced.
	emptyDir, own bool
}

func readBundleCurrent(path string) (bundleCurrent, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return bundleCurrent{}, nil
	}
	if err != nil {
		return bundleCurrent{}, err
	}
	switch {
	case fi.Mode().IsRegular():
		data, err := os.ReadFile(path)
		return bundleCurrent{data: data, mode: fi.Mode().Perm(), exists: true}, err
	case fi.IsDir():
		entries, err := os.ReadDir(path)
		return bundleCurrent{exists: true, emptyDir: len(entries) == 0, own: len(entries) > 0}, err
	default:
		data, _ := os.ReadFile(path) // through the link; a dangling one reads as nothing
		return bundleCurrent{data: data, exists: true, own: true}, nil
	}
}

// classifyBundleFile decides what a refresh does with one bundle file. A
// file is pristine when its sha256 is the one the manifest recorded for it
// or one of the versions the control plane has ever served (all a project
// deployed by an older CLI, without a manifest, has to go on).
// replaceEdited (--replace-bundle) treats an edited regular file as
// pristine; a symlink or populated directory is never replaced.
func classifyBundleFile(name string, cur bundleCurrent, served []byte, m bundleManifest, replaceEdited bool) bundleState {
	switch {
	case !cur.exists || cur.emptyDir:
		return bundleMissing
	case bytes.Equal(cur.data, served):
		return bundleUnchanged
	case cur.own:
		return bundleModified
	case replaceEdited || bundlePristineSum(name, sha256Hex(cur.data), m):
		return bundlePristine
	}
	return bundleModified
}

// bundlePristineSum takes either record: deciding by the manifest alone where
// it has an entry would call a served version the operator put back (a .bak
// copied over the file) an edit on one run and, once commit dropped that
// entry, pristine on the next.
func bundlePristineSum(name, sum string, m bundleManifest) bool {
	return m.Files[name] == sum || slices.Contains(knownBundleHashes[name], sum)
}

// bundleMode is how `origamy upgrade` treats a Docker project's bundle.
type bundleMode int

const (
	bundleAuto    bundleMode = iota // refresh the files nobody edited
	bundleReplace                   // --replace-bundle: edited files too
	bundleKeep                      // --keep-bundle: leave it alone
)

func bundleModeFlags(replace, keep bool) (bundleMode, error) {
	switch {
	case replace && keep:
		return bundleAuto, fmt.Errorf("--replace-bundle and --keep-bundle cannot be used together")
	case replace:
		return bundleReplace, nil
	case keep:
		return bundleKeep, nil
	}
	return bundleAuto, nil
}

// dockerRun runs a docker command with extra environment and returns its
// combined output; dockerRunTimeout, one that must finish within a time.
// Variables so tests can stand in for Docker.
var (
	dockerRun        = runCapturedEnv
	dockerRunTimeout = runCapturedEnvTimeout
)

type bundleFile struct {
	name  string
	state bundleState
	cur   bundleCurrent
	sum   string // sha256 of the served bytes
	// edited: replaced although edited here (--replace-bundle, or the schema
	// across the storage reset). backup: where apply saved the previous file.
	edited bool
	backup string
}

func (f bundleFile) changes() bool { return f.state == bundlePristine || f.state == bundleMissing }

// bundleRefresh swaps a Docker project's bundle files for the ones its control
// plane serves for the target release. planBundleRefresh prepares it (the new
// files wait in tmp, a directory inside the project so the swap is a rename);
// the images are pulled with the new compose file from there, then apply
// swaps the files in (rollback puts the previous ones back if that fails
// half-way) and commit finishes it.
type bundleRefresh struct {
	dir, tmp, version, installedTag string
	files                           []bundleFile
	// prev is the manifest as the refresh found it.
	prev bundleManifest
	// undo holds, in order, the steps apply took; rollback reverses them.
	undo []func() error
	// removedNew lists the stale <name>.new files commit removed.
	removedNew []string
	// remove removes tmp, once: by cleanup, or by a prompt or a signal that
	// ends the CLI first (onExit), the download included.
	remove func()
}

// controlPlaneFetchFail explains a bundle file (name) a Docker upgrade could
// not download from its control plane at base: a network or control-plane
// problem, never a cluster's. rerun says how to try again.
func controlPlaneFetchFail(base, name string, err error, rerun string) error {
	if errors.Is(err, errNotServed) {
		return fail(fmt.Sprintf("Your control plane (%s) does not serve %s.", base, name),
			"It predates this CLI's deploy bundle: update the control plane, then "+rerun+".")
	}
	return fail(fmt.Sprintf("Could not download %s from your control plane (%s).", name, base),
		err.Error()+"\nCheck CONTROL_PLANE_HTTP_URL in .env and that this host reaches the control plane over HTTPS (DNS, proxy, firewall), then "+rerun+".")
}

// bundleTempPrefix names the download directory prepareBundleRefresh makes
// in the project; reBundleTemp matches what os.MkdirTemp makes of it.
const bundleTempPrefix = ".origamy-bundle-"

var reBundleTemp = regexp.MustCompile(`^` + regexp.QuoteMeta(bundleTempPrefix) + `[0-9]+$`)

// removeStaleBundleDirs deletes download directories an interrupted upgrade
// left in dir and returns their names. Only directories named the way
// prepareBundleRefresh names them are touched.
func removeStaleBundleDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		if e.IsDir() && e.Type()&os.ModeSymlink == 0 && reBundleTemp.MatchString(e.Name()) {
			if os.RemoveAll(filepath.Join(dir, e.Name())) == nil {
				removed = append(removed, e.Name())
			}
		}
	}
	return removed
}

// bundleFileError is a bundle file prepareBundleRefresh could not download
// or read.
type bundleFileError struct {
	name string
	err  error
}

func (e *bundleFileError) Error() string { return e.name + ": " + e.err.Error() }
func (e *bundleFileError) Unwrap() error { return e.err }

// prepareBundleRefresh downloads the bundle from base into a temporary
// directory inside dir and classifies every file. force names files replaced
// even when edited (the storage reset's schema). Backups are named after the
// release each replaced file belongs to (backupRelease). An in-place deploy
// takes the bundle the same way, with replaceEdited.
func prepareBundleRefresh(dir, base, version, installedTag string, replaceEdited bool, force map[string]bool) (*bundleRefresh, error) {
	tmp, err := os.MkdirTemp(dir, bundleTempPrefix)
	if err != nil {
		return nil, err
	}
	m := readBundleManifest(dir)
	r := &bundleRefresh{dir: dir, tmp: tmp, version: version, installedTag: installedTag, prev: m}
	// An interrupt during the download removes it too.
	r.remove = onExit(func() { _ = os.RemoveAll(tmp) })
	for _, name := range bundleFiles {
		served, err := fetchBundleBytes(base, name)
		if err == nil {
			err = os.WriteFile(filepath.Join(tmp, name), served, 0o644)
		}
		var cur bundleCurrent
		if err == nil {
			cur, err = readBundleCurrent(filepath.Join(dir, name))
		}
		if err != nil {
			r.cleanup()
			return nil, &bundleFileError{name: name, err: err}
		}
		state := classifyBundleFile(name, cur, served, m, replaceEdited || force[name])
		r.files = append(r.files, bundleFile{
			name:   name,
			state:  state,
			cur:    cur,
			sum:    sha256Hex(served),
			edited: state == bundlePristine && !bundlePristineSum(name, sha256Hex(cur.data), m),
		})
	}
	return r, nil
}

// backupRelease is the release the backup of bundle file name (data, as it
// is now) is named after: one the control plane handed out with exactly
// these bytes (knownBundleReleases). Otherwise the file was edited here, or
// holds bytes the table does not know, and it is the release the file itself
// came with: the manifest's record for it (Releases, else the release of the
// version recorded in Files, which the edit was made from). Only then the
// release the manifest recorded the bundle as, which a kept edit can
// predate, else the installed tag.
func backupRelease(name string, data []byte, m bundleManifest, installedTag string) string {
	if rels := knownBundleReleases[sha256Hex(data)]; len(rels) > 0 {
		return preferRelease(rels, installedTag, m.Version)
	}
	if v := m.Releases[name]; v != "" {
		return v
	}
	if rels := knownBundleReleases[m.Files[name]]; len(rels) > 0 {
		return preferRelease(rels, installedTag, m.Version)
	}
	return cmp.Or(m.Version, installedTag)
}

// preferRelease picks one of rels (oldest first): the first of prefer that is
// one of them, else the newest.
func preferRelease(rels []string, prefer ...string) string {
	for _, v := range prefer {
		if v != "" && slices.Contains(rels, v) {
			return v
		}
	}
	return rels[len(rels)-1]
}

// backupTag makes a release name a backup suffix ("previous" for none).
func backupTag(installedTag string) string {
	t := strings.Map(func(c rune) rune {
		if c == '.' || c == '-' || c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			return c
		}
		return '_'
	}, installedTag)
	if t == "" {
		return "previous"
	}
	return t
}

func (r *bundleRefresh) file(name string) (bundleFile, bool) {
	for _, f := range r.files {
		if f.name == name {
			return f, true
		}
	}
	return bundleFile{}, false
}

// changes reports whether the refresh writes name.
func (r *bundleRefresh) changes(name string) bool {
	f, ok := r.file(name)
	return ok && f.changes()
}

// served reports whether name ends up as the served version (written or
// already identical) rather than kept as the operator's edit.
func (r *bundleRefresh) served(name string) bool {
	f, ok := r.file(name)
	return ok && f.state != bundleModified
}

func (r *bundleRefresh) names(want func(bundleFile) bool) []string {
	var out []string
	for _, f := range r.files {
		if want(f) {
			out = append(out, f.name)
		}
	}
	return out
}

// changed lists the files the refresh writes; kept, the edited ones it leaves.
func (r *bundleRefresh) changed() []string { return r.names(bundleFile.changes) }
func (r *bundleRefresh) kept() []string {
	return r.names(func(f bundleFile) bool { return f.state == bundleModified })
}

// overwritten lists the edited files the refresh replaces.
func (r *bundleRefresh) overwritten() []bundleFile {
	var out []bundleFile
	for _, f := range r.files {
		if f.edited {
			out = append(out, f)
		}
	}
	return out
}

// composeArgs is what a docker compose command in the project runs with
// when it must see the new compose file before it is swapped in: the -f
// files the project's own invocations load (composeFileArgs) with the new
// file in place of docker-compose.yml. nil when the compose file stays.
func (r *bundleRefresh) composeArgs(envPath string) []string {
	if r == nil || !r.changes("docker-compose.yml") {
		return nil
	}
	return composeFileArgs(r.dir, envPath, filepath.Join(r.tmp, "docker-compose.yml"))
}

// composeFileArgs is the -f list for the compose files a plain `docker
// compose --project-directory dir --env-file envPath` loads, with dir's
// docker-compose.yml replaced by compose: the files COMPOSE_FILE names (the
// process environment over the .env), else docker-compose.yml and the first
// override file compose picks up next to it.
func composeFileArgs(dir, envPath, compose string) []string {
	own := filepath.Join(dir, "docker-compose.yml")
	var files []string
	if list := cmp.Or(os.Getenv("COMPOSE_FILE"), unquoteEnv(readEnvVar(envPath, "COMPOSE_FILE"))); list != "" {
		sep := cmp.Or(os.Getenv("COMPOSE_PATH_SEPARATOR"), unquoteEnv(readEnvVar(envPath, "COMPOSE_PATH_SEPARATOR")), string(os.PathListSeparator))
		for _, f := range strings.Split(list, sep) {
			if f = strings.TrimSpace(f); f != "" {
				if !filepath.IsAbs(f) {
					f = filepath.Join(dir, f)
				}
				files = append(files, filepath.Clean(f))
			}
		}
	} else {
		files = []string{own}
		for _, o := range []string{"compose.override.yml", "compose.override.yaml", "docker-compose.override.yml", "docker-compose.override.yaml"} {
			if fileExists(filepath.Join(dir, o)) {
				files = append(files, filepath.Join(dir, o))
				break
			}
		}
	}
	var args []string
	for _, f := range files {
		if f == own {
			f = compose
		}
		args = append(args, "-f", f)
	}
	return args
}

// validate checks the new bundle before anything is touched. The new compose
// file must render against the project's .env with the target tag (and what
// else the run gives compose in env before .env holds it): one that needs a
// variable the .env lacks (${X:?}) or is otherwise invalid would take the
// plane down at `up`. And the new schema must be one tag can write.
func (r *bundleRefresh) validate(envPath string, env []string, tag string) error {
	if args := r.composeArgs(envPath); args != nil {
		args = append(append([]string{"compose", "--project-directory", r.dir}, args...), "--env-file", envPath, "config", "-q")
		if out, err := dockerRun(env, "docker", args...); err != nil {
			return fmt.Errorf("the new docker-compose.yml does not validate against your .env: %s", strings.ReplaceAll(lastLines(out, 3), "\n", " "))
		}
	}
	if r.changes("clickhouse-init.sql") {
		sql, err := os.ReadFile(filepath.Join(r.tmp, "clickhouse-init.sql"))
		if err != nil {
			return err
		}
		if why := bundleSchemaMismatch(string(sql), tag); why != "" {
			return errors.New(why)
		}
	}
	return nil
}

// apply backs up every file it replaces and renames the new ones into place.
// On an error the caller rolls back what it did.
func (r *bundleRefresh) apply() error {
	for i := range r.files {
		f := &r.files[i]
		if !f.changes() {
			continue
		}
		dst, src := filepath.Join(r.dir, f.name), filepath.Join(r.tmp, f.name)
		switch {
		case f.state == bundlePristine:
			bak, err := writeBackup(dst, f.cur.data, f.cur.mode, backupTag(backupRelease(f.name, f.cur.data, r.prev, r.installedTag)))
			if err != nil {
				return fmt.Errorf("back up %s: %w", f.name, err)
			}
			f.backup = bak
			// Renaming the backup over the file restores it, whether or not
			// the new one got in.
			r.undo = append(r.undo, func() error { return os.Rename(bak, dst) })
			if f.cur.mode != 0 {
				_ = os.Chmod(src, f.cur.mode)
			}
		case f.cur.emptyDir:
			if err := os.Remove(dst); err != nil {
				return err
			}
			r.undo = append(r.undo, func() error { return os.Mkdir(dst, 0o755) })
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		if f.state == bundleMissing {
			r.undo = append(r.undo, func() error { return os.Remove(dst) })
		}
	}
	return nil
}

// swapIn applies the refresh and, when that fails half-way, rolls it back, as
// one step an interrupt waits for (exitGate). err is apply's error, rerr
// rollback's.
func (r *bundleRefresh) swapIn() (err, rerr error) {
	exitGate.Lock()
	defer exitGate.Unlock()
	if err = r.apply(); err != nil {
		rerr = r.rollback()
	}
	return err, rerr
}

// writeBackup copies data to <path>.bak-<tag>, never over an existing backup
// (another upgrade from the same release gets .bak-<tag>.1, …).
func writeBackup(path string, data []byte, mode os.FileMode, tag string) (string, error) {
	if mode == 0 {
		mode = 0o644
	}
	base := path + ".bak-" + tag
	for i := 0; i < 100; i++ {
		bak := base
		if i > 0 {
			bak = fmt.Sprintf("%s.%d", base, i)
		}
		f, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			_ = os.Remove(bak)
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(bak)
			return "", err
		}
		return bak, nil
	}
	return "", fmt.Errorf("too many backups of %s", path)
}

// rollback undoes apply, newest step first, so the project describes the
// release that is still running.
func (r *bundleRefresh) rollback() error {
	var errs []error
	for i := len(r.undo) - 1; i >= 0; i-- {
		if err := r.undo[i](); err != nil {
			errs = append(errs, err)
		}
	}
	r.undo = nil
	r.cleanup()
	return errors.Join(errs...)
}

// commit finishes a refresh once its files are in place. The served version
// of every file kept because it was edited goes next to it as <name>.new, and
// the manifest records the files now on disk that are the bundle's. An edited
// file keeps the entry it had (none for a project from an older CLI), so the
// next upgrade decides about it exactly as this one did. A <name>.new an
// earlier upgrade left next to a file that is now the served version is
// stale, and removed.
func (r *bundleRefresh) commit() error {
	defer r.cleanup()
	r.undo = nil
	m := bundleManifest{Version: r.version, Files: map[string]string{}, Releases: map[string]string{}}
	var errs []error
	for _, f := range r.files {
		if f.state == bundleModified {
			if err := os.Rename(filepath.Join(r.tmp, f.name), filepath.Join(r.dir, f.name+".new")); err != nil {
				errs = append(errs, err)
			}
			if prev := r.prev.Files[f.name]; prev != "" {
				m.Files[f.name] = prev
			}
			if rel := r.prev.Releases[f.name]; rel != "" {
				m.Releases[f.name] = rel
			}
			continue
		}
		m.Files[f.name] = f.sum
		if r.version != "" {
			m.Releases[f.name] = r.version
		}
		stale := filepath.Join(r.dir, f.name+".new")
		if fi, err := os.Lstat(stale); err == nil && fi.Mode().IsRegular() {
			if err := os.Remove(stale); err != nil {
				errs = append(errs, err)
			} else {
				r.removedNew = append(r.removedNew, f.name+".new")
			}
		}
	}
	if err := writeBundleManifest(r.dir, m); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// cleanup removes the download directory.
func (r *bundleRefresh) cleanup() {
	switch {
	case r.remove != nil:
		r.remove()
	case r.tmp != "":
		_ = os.RemoveAll(r.tmp)
	}
}

// restarts names the services to restart after `up -d` because a file they
// bind-mount was replaced (none for a nil refresh). Compose recreates a
// container when its service definition changes, not when a mounted file
// does, and the replaced file is a new inode the running container never
// sees.
func (r *bundleRefresh) restarts(profiles []string) []string {
	if r == nil {
		return nil
	}
	var out []string
	if r.changes("clickhouse-users.xml") {
		out = append(out, "clickhouse")
	}
	if r.changes("Caddyfile") && hasProfile(profiles, "ingress") {
		out = append(out, "caddy")
	}
	return out
}

// bundleMountServices are all the services that bind-mount bundle files
// (restarts): ClickHouse, and Caddy when the ingress profile runs it. A run
// that finishes a storage reset restarts them whatever it replaced itself,
// since the run that began the reset may have swapped files under them and
// stopped before restarting them.
func bundleMountServices(profiles []string) []string {
	out := []string{"clickhouse"}
	if hasProfile(profiles, "ingress") {
		out = append(out, "caddy")
	}
	return out
}

// planBundleRefresh decides whether upgrading the project in dir to tag
// refreshes its bundle from the control plane at base (controlPlaneHTTPBase),
// and prepares and validates the refresh (with env, the run's compose
// environment). Without one it returns the reason, short enough for the
// summary box, and the upgrade goes on with the files the project has. reset
// forces the new schema in: the storage reset recreates the events table
// from it.
func planBundleRefresh(dir, envPath, base, tag, installedTag string, env []string, mode bundleMode, reset bool) (*bundleRefresh, string) {
	// Across the reset a skipped refresh does not leave the files as they
	// are: the reset writes its schema to clickhouse-init.sql all the same.
	asIs, resetNote := "left as-is", ""
	if reset {
		asIs, resetNote = "not refreshed", "; the storage reset still writes its schema to clickhouse-init.sql"
	}
	if mode == bundleKeep {
		ui.Step("Bundle files %s (--keep-bundle)%s.", asIs, resetNote)
		return nil, "--keep-bundle"
	}
	if base == "" {
		ui.Warn("Bundle files (%s) left as-is: the control plane URL is missing from .env (no CONTROL_PLANE_HTTP_URL or CONFIG_URL); re-deploy with `origamy deploy --token …` to restore it", strings.Join(bundleFiles, ", "))
		return nil, "control-plane URL missing from .env; re-deploy"
	}
	sp := ui.Start("Checking the control plane's deploy bundle")
	served, err := servedBundleVersion(base)
	if why := bundleVersionProblem(served, err, tag); why != "" {
		sp.Warn("Bundle files (%s) %s: %s%s", strings.Join(bundleFiles, ", "), asIs, why, resetNote)
		switch {
		case errors.Is(err, errNotServed):
			return nil, "control plane has no /byod/VERSION"
		case err != nil:
			return nil, "control plane unreachable"
		}
		return nil, "control plane serves " + served
	}
	r, err := prepareBundleRefresh(dir, base, tag, installedTag, mode == bundleReplace, map[string]bool{"clickhouse-init.sql": reset})
	if err != nil {
		sp.Warn("Bundle files %s: could not download the %s bundle (%v)%s", asIs, tag, err, resetNote)
		return nil, "download failed"
	}
	if err := r.validate(envPath, env, tag); err != nil {
		r.cleanup()
		sp.Warn("Bundle files %s: %v%s", asIs, err, resetNote)
		return nil, "new bundle failed validation"
	}
	switch changed := r.changed(); {
	case len(changed) > 0:
		var edited []string
		for _, f := range r.overwritten() {
			edited = append(edited, f.name)
		}
		if len(edited) > 0 {
			sp.Success("Bundle %s: updating %s (replacing your edited %s)", tag, strings.Join(changed, ", "), strings.Join(edited, ", "))
		} else {
			sp.Success("Bundle %s: updating %s", tag, strings.Join(changed, ", "))
		}
	case len(r.kept()) > 0:
		sp.Success("Bundle %s: only files edited here differ", tag)
	default:
		sp.Success("Bundle files already match %s", tag)
	}
	return r, ""
}

// pullImages pulls the images of the release env names (tagEnv, and what
// else the run gives compose before .env holds it: the AI toggle's profile).
// With a refresh that replaces the compose file it pulls with the new one
// from the download directory, before anything in the project is touched,
// so the images of services the new bundle adds come too and a failed or
// interrupted pull leaves the project as it was.
func pullImages(dir, envPath string, env []string, r *bundleRefresh) (string, error) {
	args := append(append([]string{"compose", "--project-directory", dir}, r.composeArgs(envPath)...), "--env-file", envPath, "pull")
	return dockerRun(env, "docker", args...)
}

// bundleBoxLines summarises the refresh for the upgrade's final box, and
// the storage reset's own write of clickhouse-init.sql (schema; nil when the
// reset wrote none).
func bundleBoxLines(r *bundleRefresh, skipped string, schema *resetSchemaWrite) []string {
	label := ui.Gray("Bundle     ")
	indent := "           "
	if schema != nil {
		what := "clickhouse-init.sql replaced with your control plane's current schema for the storage reset"
		switch {
		case schema.edited:
			what = ui.Yellow("replaced your edited clickhouse-init.sql") + " with your control plane's current schema for the storage reset — yours is in " + filepath.Base(schema.backup)
		case schema.backup != "":
			what += " — the previous one is in " + filepath.Base(schema.backup)
		}
		if r == nil {
			return []string{label + "not refreshed (" + skipped + ")", indent + what}
		}
		return append(bundleBoxLines(r, skipped, nil), indent+what)
	}
	if r == nil {
		return []string{label + "left as-is (" + skipped + ")"}
	}
	var lines []string
	switch changed := r.changed(); {
	case len(changed) > 0:
		lines = append(lines, label+"refreshed to "+ui.Bold(r.version)+" ("+strings.Join(changed, ", ")+")")
	case len(r.kept()) > 0:
		lines = append(lines, label+r.version+": nothing replaced")
	default:
		lines = append(lines, label+"already "+r.version)
	}
	for _, f := range r.overwritten() {
		lines = append(lines, "           "+ui.Yellow("replaced your edited "+f.name)+" — yours is in "+filepath.Base(f.backup))
	}
	for _, name := range r.kept() {
		lines = append(lines, "           "+ui.Yellow("kept your edited "+name)+" — see "+name+".new")
	}
	return lines
}
