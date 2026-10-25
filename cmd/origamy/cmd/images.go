package cmd

import (
	"cmp"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// What a Docker project runs, held against the release it was moved to. These
// checks are the heavy part of a Docker upgrade or deploy: they run only on
// a move that can touch the events table (canTouchEventsTable), because a
// data-plane image of the wrong generation running against it drops every
// event at insert. Any other move gets advisoryImageNotes.

// reImageID is an image's ID, which Docker shows for a container in place of
// the reference it was created from once that tag names another image.
var reImageID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{12,64}$`)

// imageTag is the tag of an image reference ("" when it names none).
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// imageRepo is the repository of an image reference: without its tag and
// digest.
func imageRepo(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// imageDigest is the digest an image reference pins ("sha256:…"), "" when it
// pins none. (compose runs the digest and ignores the tag next to it.)
func imageDigest(ref string) string {
	_, digest, _ := strings.Cut(ref, "@")
	return digest
}

// imageName is the last path element of an image reference's repository
// (bulker-worker for ghcr.io/…/origamy-data-plane/bulker-worker:0.1.20).
func imageName(ref string) string { return path.Base(imageRepo(ref)) }

// dataPlaneRegistry is where the release's data-plane images come from.
const dataPlaneRegistry = "ghcr.io/qubelylabs/" + dataPlaneImageRepo

// bundleDataPlaneServices are the data-plane services of every compose bundle
// so far (0.1.16 to 0.1.20, plus 0.1.25's source-worker and
// destination-worker), each named like its image in dataPlaneRegistry: one of
// them is a data-plane service whatever image a compose file the operator
// edited gives it. engagement-public is left out: it runs the onboarding-public
// image, so its name is not its image's, and the bundle's own image for it
// already identifies it.
var bundleDataPlaneServices = []string{
	"ingestion-gateway", "transformer-engine", "identity-resolver", "segment-evaluator",
	"bulker-worker", "config-sync", "portal-agent", "workflow-engine", "orchestrator-engine",
	"source-worker", "destination-worker",
}

// dpService is a data-plane service as a project's compose configuration
// runs it: image is the image compose starts it from (override files and
// COMPOSE_FILE included), release the release's own image for it (an image
// of dataPlaneImageRepo), scaledDown whether the configuration runs it
// nowhere (scale: 0, deploy.replicas: 0).
type dpService struct {
	name, image, release string
	scaledDown           bool
}

// dataPlaneImageName is the name, in the release, of the data-plane image a
// compose service runs, "" when it is not a data-plane service by what its
// configuration says: an image of dataPlaneImageRepo (whichever service
// name an override file gives it: bulker-worker-2), the image the bundle's
// own compose file gives the service when an override took another (a
// registry mirror), or one of bundleDataPlaneServices by name.
func dataPlaneImageName(service, image, bundleImage string) string {
	switch {
	case strings.Contains(image, dataPlaneImageRepo):
		return imageName(image)
	case strings.Contains(bundleImage, dataPlaneImageRepo):
		return imageName(bundleImage)
	case slices.Contains(bundleDataPlaneServices, service):
		return service
	}
	return ""
}

// dpImageIDs maps the ID of every image on this host that carries a
// data-plane reference (dataPlaneImageRepo; any release, any tag) to its
// name: an image of the same ID is a data-plane image whatever registry's
// name it was retagged with. nil when Docker cannot list the images.
func dpImageIDs() map[string]string {
	out, err := dockerRun(nil, "docker", "image", "ls", "--no-trunc", "--format", `{{.ID}}`+"\t"+`{{.Repository}}`)
	if err != nil {
		return nil
	}
	lines := strings.Split(out, "\n")
	slices.Sort(lines)
	ids := map[string]string{}
	for _, l := range lines {
		id, repo, ok := strings.Cut(strings.TrimRight(l, "\r"), "\t")
		if _, seen := ids[id]; ok && strings.HasPrefix(id, "sha256:") && strings.Contains(repo, dataPlaneImageRepo) && !seen {
			ids[id] = path.Base(repo)
		}
	}
	return ids
}

// dataPlaneServices are the data-plane services the compose configuration of
// the project in dir runs with env (the release, tag, in DP_IMAGE_TAG; the
// profiles that are on), main in place of its docker-compose.yml (the
// refreshed one, before it is swapped in). They are told by what the
// configuration says (dataPlaneImageName), and by image: a service whose
// image is, by ID, a data-plane image of this host (a mirror's retag of
// one, under a service name the CLI does not know). That covers the
// services an override file only adds (bulker-worker-2) and the ones it
// takes from a registry mirror. The services of a profile that is off are
// not among them (see removeInactiveDataPlane).
func dataPlaneServices(dir, envPath, tag string, env []string, main string) ([]dpService, error) {
	files := composeFileArgs(dir, envPath, main)
	full, err := effectiveCompose(dir, envPath, env, files)
	if err != nil {
		return nil, err
	}
	base := full
	if !slices.Equal(files, []string{"-f", main}) {
		if base, err = effectiveCompose(dir, envPath, env, []string{"-f", main}); err != nil {
			return nil, err
		}
	}
	var ids map[string]string
	var svcs []dpService
	for _, name := range slices.Sorted(maps.Keys(full.Services)) {
		s := full.Services[name]
		bundleImage := base.Services[name].Image
		img := dataPlaneImageName(name, s.Image, bundleImage)
		if img == "" {
			if ids == nil {
				ids = dpImageIDs()
			}
			if i, ok := imageInfo(s.Image); ok {
				img = ids[i.id]
			}
		}
		if img == "" {
			continue
		}
		release := dataPlaneRegistry + img + ":" + tag
		if strings.Contains(bundleImage, dataPlaneImageRepo) {
			release = bundleImage
		}
		svcs = append(svcs, dpService{name: name, image: s.Image, release: release, scaledDown: s.scaledDown()})
	}
	return svcs, nil
}

// versionLabel is the org.opencontainers.image.version label in an image's
// or a container's configuration, as `{{json .Config}}` prints it. (A
// template that reads .Config.Labels itself fails on an image without
// labels: docker inspect's templates refuse a missing key.)
func versionLabel(config string) string {
	var c struct {
		Labels map[string]string `json:"Labels"`
	}
	_ = json.Unmarshal([]byte(config), &c)
	return c.Labels["org.opencontainers.image.version"]
}

// imageIdent is what Docker says an image is: its ID, the digests of the
// manifests it is known by (RepoDigests) and its
// org.opencontainers.image.version label ("" for none; the official images
// carry none).
type imageIdent struct {
	id, version string
	digests     []string
}

// names are the digests that name this image: its ID and its RepoDigests.
func (i imageIdent) names() []string {
	if i.id == "" {
		return i.digests
	}
	return append([]string{i.id}, i.digests...)
}

// has reports whether digest names this image (as its ID, or as a manifest
// digest it was pulled by).
func (i imageIdent) has(digest string) bool {
	return digest != "" && slices.Contains(i.names(), digest)
}

// same reports whether o is this very image.
func (i imageIdent) same(o imageIdent) bool { return slices.ContainsFunc(o.names(), i.has) }

// imageInfo is a local image's identity; ok is false when this host does not
// have it.
func imageInfo(ref string) (imageIdent, bool) {
	out, err := dockerRun(nil, "docker", "image", "inspect", "--format", `{{.Id}}`+"\t"+`{{json .RepoDigests}}`+"\t"+`{{json .Config}}`, ref)
	f := strings.SplitN(strings.TrimSpace(out), "\t", 3)
	if err != nil || len(f) != 3 || !strings.HasPrefix(f[0], "sha256:") {
		return imageIdent{}, false
	}
	img := imageIdent{id: f[0], version: versionLabel(f[2])}
	var repoDigests []string
	_ = json.Unmarshal([]byte(f[1]), &repoDigests)
	for _, d := range repoDigests {
		if _, digest := imageSplit(d); digest != "" {
			img.digests = append(img.digests, digest)
		}
	}
	return img, true
}

// imageSplit cuts repo@digest.
func imageSplit(ref string) (repo, digest string) {
	repo, digest, _ = strings.Cut(ref, "@")
	return repo, digest
}

// releaseImages finds the identity of the release's own images, which
// anything else is compared with (a data-plane image from another registry,
// one pinned by digest, a container's): inspected, and pulled first when this
// host does not have one. Each is looked up once per check.
type releaseImages map[string]struct {
	img imageIdent
	err error
}

func (r releaseImages) ident(ref string) (imageIdent, error) {
	if got, ok := r[ref]; ok {
		return got.img, got.err
	}
	img, ok := imageInfo(ref)
	var err error
	if !ok {
		if out, perr := dockerRun(nil, "docker", "pull", ref); perr != nil {
			err = errors.New(orDash(lastLines(out, 1)))
		} else if img, ok = imageInfo(ref); !ok {
			err = errors.New("docker pull " + ref + " left no image")
		}
	}
	r[ref] = struct {
		img imageIdent
		err error
	}{img, err}
	return img, err
}

// problem says why img, the image a data-plane service runs (or is to
// start, ref: how the service names it) is not the image the release
// names for it (release), "" when it is: the same image, by ID or digest, or
// one whose version label names the release. who says whose image it is.
func (r releaseImages) problem(who, ref string, img imageIdent, release, tag string) string {
	if img.version == tag || img.version == "v"+tag {
		return ""
	}
	rid, err := r.ident(release)
	switch {
	case err != nil:
		return who + " runs " + ref + ", and the release's image " + release + " could not be pulled here to compare them (" + err.Error() + ")"
	case img.id == "":
		return who + " runs " + ref + ", and which image it is could not be read"
	case !img.same(rid):
		return who + " runs " + ref + ", which is not the release's image " + release + " (another image, by ID)"
	}
	return ""
}

// digestProblem says why the data-plane service s, whose image is pinned by
// digest (s.image), does not start the release's image: compose runs the
// digest and ignores the tag next to it, so the tag proves nothing. "" when
// the digest names the release's image (its ID, or a digest it was pulled by).
func (s dpService) digestProblem(rel releaseImages) string {
	pinned := imageDigest(s.image)
	rid, err := rel.ident(s.release)
	switch {
	case err != nil:
		return s.name + " is pinned by digest (" + s.image + "), and the release's image " + s.release + " could not be pulled here to compare them (" + err.Error() + ")"
	case !rid.has(pinned):
		return s.name + " is pinned by digest (" + s.image + "), which is not the digest of the release's image " + s.release + " (" + strings.Join(rid.names(), ", ") + "): compose runs the digest, whatever tag stands next to it"
	}
	return ""
}

// projectContainer is a container of the project and what it runs: the
// image reference it was created from, the image it is (by ID) and that
// image's org.opencontainers.image.version label.
type projectContainer struct{ service, state, ref, image, version string }

// running reports whether the container is up (or coming up again): one that
// has exited or was only created writes nothing.
func (c projectContainer) running() bool {
	return slices.Contains([]string{"running", "restarting", "paused"}, c.state)
}

// projectContainers lists every container, in any state, that carries the
// compose project label name: the plane's services, replicas an override
// scales up and services it adds, and the one-off runs; whatever its compose
// configuration says now. out is what docker printed when it failed.
func projectContainers(name string) (list []composeService, out string, err error) {
	out, err = dockerRun(nil, "docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+name, "--format",
		`{{.ID}}`+"\t"+`{{.Label "com.docker.compose.service"}}`+"\t"+`{{.State}}`+"\t"+`{{.Image}}`)
	if err != nil {
		return nil, out, err
	}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Split(strings.TrimRight(l, "\r"), "\t"); len(f) == 4 && reContainerID.MatchString(f[0]) {
			list = append(list, composeService{ID: f[0], Service: f[1], State: f[2], Image: f[3]})
		}
	}
	return list, out, nil
}

// inspectContainers asks Docker what each container listed was created from:
// Compose before 2.15 lists no image, and one whose tag names another image
// now is listed by the image's ID.
func inspectContainers(listed []composeService) []projectContainer {
	var ids []string
	for _, s := range listed {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	inspected := map[string][]string{}
	if len(ids) > 0 {
		// A container removed since the listing fails the command but not
		// the others' lines.
		out, _ := dockerRun(nil, "docker", append([]string{"inspect", "--format",
			`{{.Id}}` + "\t" + `{{.Config.Image}}` + "\t" + `{{.Image}}` + "\t" + `{{json .Config}}`}, ids...)...)
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Split(strings.TrimRight(l, "\r"), "\t"); len(f) == 4 && f[0] != "" {
				inspected[f[0]] = f[1:]
			}
		}
	}
	var cs []projectContainer
	for _, s := range listed {
		c := projectContainer{service: s.Service, state: strings.ToLower(s.State), ref: s.Image}
		for id, f := range inspected {
			if s.ID != "" && strings.HasPrefix(id, s.ID) {
				c.ref, c.image, c.version = cmp.Or(f[0], s.Image), f[1], versionLabel(f[2])
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// containerImageName is the name, in the release, of the data-plane image a
// container runs, "" when it runs none: by the reference it was created from
// (an image of dataPlaneImageRepo), its service's name (bundleDataPlaneServices),
// or its image's ID (ids: dpImageIDs).
func containerImageName(c projectContainer, ids map[string]string) string {
	switch {
	case strings.Contains(c.ref, dataPlaneImageRepo):
		return imageName(c.ref)
	case slices.Contains(bundleDataPlaneServices, c.service):
		return c.service
	}
	return ids[c.image]
}

// runningImagesProblem is the rest of the check after the last `up -d`
// (eventsTableProblem): the containers that run are the release's. EVERY
// container of the compose project (by its project label, under the name
// the CLI pins) that runs a data-plane image has to run the release's image
// for it: the same image, by ID, as the release's own reference names (or a
// version label naming the release), whatever the reference it was created
// from says (a tag can name another image than it did) and whichever
// service it belongs to. A data-plane image is told by the reference it was
// created from (dataPlaneImageRepo), by its service's name, or by its ID,
// when it is one of the data-plane images on this host (dpImageIDs). So an
// old writer an override file adds (bulker-worker-2 on the old image), one
// left over from a profile that is off, or one from a registry mirror under a
// name the CLI does not know, fails the run. Every data-plane service the
// configuration runs (dataPlaneServices) has a container, except one the
// configuration scales down to 0 on purpose (reported). While ClickHouse
// holds the new events table (gen), ClickHouse runs minClickHouseForJSON or
// newer (as its image's tag says, or the server when the tag names no
// version). What a container runs is what docker inspect says it was created
// from (inspectContainers). It returns why the images are not the release's,
// "" when they are.
func runningImagesProblem(dir, envPath, tag, gen string) string {
	sp := ui.Start("Checking the images that run")
	failed := func(why string) string {
		sp.Fail("%s", why)
		return why
	}
	env := composeEnv(tag)
	svcs, err := dataPlaneServices(dir, envPath, tag, env, filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return failed("the configuration the project runs could not be read (" + err.Error() + ")")
	}
	listed, out, err := projectContainers(composeProjectName(dir, envPath))
	if err != nil {
		return failed("docker could not list the containers of the compose project (" + orDash(lastLines(out, 1)) + ")")
	}
	byService := map[string][]projectContainer{}
	for _, c := range inspectContainers(listed) {
		byService[c.service] = append(byService[c.service], c)
	}
	configured := map[string]dpService{}
	var wrong, scaled []string
	for _, s := range svcs {
		configured[s.name] = s
		switch {
		case s.scaledDown:
			scaled = append(scaled, s.name)
		case len(byService[s.name]) == 0:
			wrong = append(wrong, s.name+" has no container")
		}
	}
	if len(svcs) == 0 {
		wrong = append(wrong, "the project's configuration runs no data-plane service")
	}
	rel, ids := releaseImages{}, dpImageIDs()
	for _, service := range slices.Sorted(maps.Keys(byService)) {
		for _, c := range byService[service] {
			s, isConfigured := configured[service]
			name := containerImageName(c, ids)
			if name == "" && !isConfigured {
				continue // not a data-plane container
			}
			if !c.running() && !isConfigured {
				continue // exited, or only created: it writes nothing
			}
			if s.scaledDown && !c.running() {
				continue
			}
			release := s.release
			if !isConfigured || release == "" {
				release = dataPlaneRegistry + name + ":" + tag
			}
			who := service
			if !isConfigured {
				who += " (not run by the configuration)"
			}
			ref := c.ref
			if ref == "" || reImageID.MatchString(ref) {
				ref = "image " + orDash(ref) + " (the reference it was created from could not be read)"
			}
			if why := rel.problem(who, ref, imageIdent{id: c.image, version: c.version}, release, tag); why != "" {
				if strings.Contains(why, "another image, by ID") && imageDigest(c.ref) == "" && imageTag(c.ref) == tag && imageRepo(c.ref) == imageRepo(release) {
					// The reference is the release's own: the tag named another image
					// when the container was made.
					why = who + " runs " + c.image + ", created from " + c.ref + " when that tag named another image than the release's " + release
				}
				wrong = append(wrong, why)
			}
		}
	}
	clickhouse := ""
	for _, c := range byService["clickhouse"] {
		if c.running() && c.state == "running" {
			clickhouse = c.ref
		}
	}
	if gen == "json" {
		needs := ", and the new events table needs ClickHouse " + minClickHouseForJSON + " or newer"
		switch v, ok := clickHouseImageVersion(clickhouse); {
		case clickhouse == "":
			wrong = append(wrong, "ClickHouse is not among the containers that run")
		case !ok:
			// A tag that names no version (latest, a registry's own name):
			// the server itself says which it is.
			out, err := clickhouseConn{dir: dir, envPath: envPath, env: env}.exec(clickhouseServerVersion)
			if sv, ok := serverVersion(out); err != nil || !ok {
				wrong = append(wrong, "clickhouse runs "+clickhouse+", whose version neither its tag nor the server ("+orDash(lastLines(out, 1))+") tells"+needs)
			} else if versionLess(sv+".0", minClickHouseForJSON+".0") {
				wrong = append(wrong, "clickhouse runs "+clickhouse+" (ClickHouse "+sv+")"+needs)
			}
		case versionLess(v+".0", minClickHouseForJSON+".0"):
			wrong = append(wrong, "clickhouse runs "+clickhouse+needs)
		}
	}
	if len(wrong) > 0 {
		why := "not every container runs what the " + tag + " release does: " + strings.Join(wrong, "; ")
		sp.Fail("Not every container runs the %s images", tag)
		return why
	}
	sp.Success("Every data-plane service runs the %s images", tag)
	if len(scaled) > 0 {
		ui.Step("Scaled down to 0 by the configuration, so running nowhere: %s.", strings.Join(scaled, ", "))
	}
	return ""
}

// plannedImagesProblem is the first half of runningImagesProblem, made before
// an upgrade's commit point: the data-plane services the project's
// configuration starts once refresh r is in (its compose file in place of
// docker-compose.yml when it replaces it), with the run's environment (env:
// the release, the AI toggle's profiles), start the release's images. An
// image named by tag must be on tag; one pinned by digest is judged by the
// digest, which must name the release's image (digestProblem); and one from
// another registry than the release's (a mirror), which pullImages just
// pulled, must be the release's image itself (releaseImages.problem). The
// release's own image is looked at on this host, and pulled when the host does
// not have it, so a host that cannot reach ghcr.io cannot compare: such a
// run is refused here, before anything is changed. It returns why not, ""
// when they are.
func plannedImagesProblem(dir, envPath, tag string, env []string, r *bundleRefresh) string {
	main := filepath.Join(dir, "docker-compose.yml")
	if r != nil && r.changes("docker-compose.yml") {
		main = filepath.Join(r.tmp, "docker-compose.yml")
	}
	sp := ui.Start("Checking the images the %s services start from", tag)
	svcs, err := dataPlaneServices(dir, envPath, tag, env, main)
	if err != nil {
		why := "the configuration it would start could not be read (" + err.Error() + ")"
		sp.Fail("%s", why)
		return why
	}
	var wrong []string
	rel := releaseImages{}
	for _, s := range svcs {
		switch {
		case s.scaledDown:
			// Runs nowhere: its image is not judged (runningImagesProblem reports it).
		case imageDigest(s.image) != "":
			if why := s.digestProblem(rel); why != "" {
				wrong = append(wrong, why)
			}
		case imageTag(s.image) != tag:
			wrong = append(wrong, s.name+" is set to "+s.image)
		case imageRepo(s.image) != imageRepo(s.release):
			img, _ := imageInfo(s.image)
			if why := rel.problem(s.name, s.image, img, s.release, tag); why != "" {
				wrong = append(wrong, why)
			}
		}
	}
	if len(svcs) == 0 {
		wrong = append(wrong, "it runs no data-plane service")
	}
	if len(wrong) > 0 {
		sp.Fail("Not every data-plane service would run the %s images", tag)
		return strings.Join(wrong, "; ")
	}
	sp.Success("Every data-plane service starts from the %s images", tag)
	return ""
}

// plannedGenerationProblem is the cheap check an upgrade makes before its
// commit point on a move that cannot touch the events table: one `docker
// compose config`, and no data-plane service (an image of
// dataPlaneImageRepo) set to a tag of a numbered release that writes the
// other events table than tag's does (an override pinning 0.1.17 under
// 0.1.20): it would write String payloads to a JSON table, and every event
// would be dropped at insert. It pulls and compares nothing, passes what it
// cannot read (a digest pin, a mirror, a compose file that does not render:
// those are the strict move's to judge), and returns why not, "" when none.
func plannedGenerationProblem(dir, envPath, tag string, env []string, r *bundleRefresh) string {
	main := filepath.Join(dir, "docker-compose.yml")
	if r != nil && r.changes("docker-compose.yml") {
		main = filepath.Join(r.tmp, "docker-compose.yml")
	}
	cfg, err := effectiveCompose(dir, envPath, env, composeFileArgs(dir, envPath, main))
	if err != nil {
		return ""
	}
	want := releaseSchemaGeneration(tag)
	var wrong []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		s := cfg.Services[name]
		if got := releaseSchemaGeneration(imageTag(s.Image)); strings.Contains(s.Image, dataPlaneImageRepo) && imageDigest(s.Image) == "" && got != "unknown" && got != want && !s.scaledDown() {
			wrong = append(wrong, name+" is set to "+s.Image)
		}
	}
	return strings.Join(wrong, "; ")
}

// advisoryImageNotes is the cheap check that is all a move that cannot touch
// the events table gets after its `up -d` (see canTouchEventsTable): the
// data-plane containers that run another tag than the release's, by the
// reference they were created from, listed by `docker ps`, with nothing
// pulled or compared. notes are for a warning (an override pins another
// release of the same generation); wrongGen are the ones whose tag is a
// numbered release that writes the other events table than tag's does, which
// fail the run like any container of the wrong generation.
func advisoryImageNotes(dir, envPath, tag string) (notes, wrongGen []string) {
	listed, _, err := projectContainers(composeProjectName(dir, envPath))
	if err != nil {
		return nil, nil
	}
	want := releaseSchemaGeneration(tag)
	for _, c := range listed {
		running := slices.Contains([]string{"running", "restarting", "paused"}, strings.ToLower(c.State))
		if !running || !strings.Contains(c.Image, dataPlaneImageRepo) || imageTag(c.Image) == tag || imageDigest(c.Image) != "" {
			continue
		}
		note := c.Service + " runs " + c.Image
		if got := releaseSchemaGeneration(imageTag(c.Image)); got != "unknown" && got != want {
			wrongGen = append(wrongGen, note)
		} else {
			notes = append(notes, note)
		}
	}
	slices.Sort(notes)
	slices.Sort(wrongGen)
	return notes, wrongGen
}
