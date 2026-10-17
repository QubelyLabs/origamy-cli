package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── Kubernetes storage reset ─────────────────────────────────────────────────
//
// The Kubernetes side of the storage reset (upgrade.go has the Docker one,
// and the rules both follow). What the release's ClickHouse holds decides,
// never the chart versions: an upgrade to a release that writes the new
// (JSON) events table moves a String one aside and creates the new one
// (asking first), creates a missing one (nothing to ask), and never touches
// one that already has the JSON columns. Moving the release to a chart that
// cannot write what ClickHouse holds (an upgrade --version, a rollback) is
// refused before anything changes. Every upgrade, rollback and install ends
// with a check that ClickHouse holds the table the images that run write.

// resetMarkerConfigMap records, in the release namespace, a storage reset an
// upgrade has begun and not finished: it is written once the chart is on the
// target and before the events table is moved aside, and deleted once the
// new table is in place and the check after the upgrade passed. While it is
// there, `origamy upgrade` finishes the reset whatever version the chart is
// on (with a release that writes the new table), and refuses one that does
// not, `origamy rollback` and an in-place `origamy deploy` are refused, and
// the table an earlier run moved aside is dropped once the new one is in
// (the operator agreed to that when the reset began).
const resetMarkerConfigMap = "origamy-reset-pending"

// releaseSelector selects every object of the release by the label the
// chart puts on all of them; componentSelector, one of its components.
const releaseSelector = "app.kubernetes.io/instance=" + release

func componentSelector(component string) string {
	return releaseSelector + ",app.kubernetes.io/component=" + component
}

// How long the Kubernetes reset and the check after a chart change wait:
// for ClickHouse to be back on its StatefulSet's current spec, for the
// schema Jobs to finish and the service pods to run the release's images.
// kubeRequestTimeout bounds each kubectl read, kubeStatementTimeout each
// statement that changes ClickHouse (RENAME, the schema, DROP). Variables so
// tests need not wait.
var (
	kubeClickHouseTimeout = 5 * time.Minute
	kubeCheckTimeout      = 5 * time.Minute
	kubePoll              = 5 * time.Second
	kubeRequestTimeout    = 30 * time.Second
	kubeStatementTimeout  = 5 * time.Minute
)

// kubeUpgradeCommand is the command that moves a Kubernetes plane to chart
// version again, on channel.
func kubeUpgradeCommand(version, channel string) string {
	if channel == "edge" {
		return "origamy upgrade --version " + version + " --channel edge"
	}
	return "origamy upgrade --version " + version
}

// ── The reset record ─────────────────────────────────────────────────────────

// kubeMarker is the record of an unfinished reset (resetMarkerConfigMap):
// the chart version and channel of the upgrade that began it.
type kubeMarker struct {
	present          bool
	version, channel string
}

// command is the upgrade that finishes the recorded reset.
func (m kubeMarker) command() string {
	if m.version == "" {
		return "origamy upgrade"
	}
	return kubeUpgradeCommand(m.version, m.channel)
}

// readResetMarker reads the record of an unfinished reset; err when kubectl
// could not tell whether there is one.
func readResetMarker() (kubeMarker, error) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "kubectl", "get", "configmap", resetMarkerConfigMap, "-n", namespace, "--ignore-not-found", "-o", "json")
	if err != nil {
		return kubeMarker{}, fmt.Errorf("kubectl could not read the ConfigMap %s in %s: %s", resetMarkerConfigMap, namespace, orDash(lastLines(out, 1)))
	}
	if strings.TrimSpace(out) == "" {
		return kubeMarker{}, nil
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return kubeMarker{}, fmt.Errorf("could not read the ConfigMap %s in %s: %w", resetMarkerConfigMap, namespace, err)
	}
	return kubeMarker{present: true, version: cm.Data["version"], channel: cm.Data["channel"]}, nil
}

// writeResetMarker records a reset to chart version on channel.
func writeResetMarker(version, channel string) error {
	out, err := kubectlApply(configMapManifest(namespace, resetMarkerConfigMap, map[string]string{
		"version": version,
		"channel": channel,
		"began":   time.Now().UTC().Format(time.RFC3339),
	}))
	if err != nil {
		return fmt.Errorf("kubectl apply: %s", orDash(lastLines(out, 2)))
	}
	return nil
}

// clearResetMarker deletes the record once the reset is done and checked.
func clearResetMarker() error {
	out, err := kubeRunTimeout(kubeRequestTimeout, "kubectl", "delete", "configmap", resetMarkerConfigMap, "-n", namespace, "--ignore-not-found")
	if err != nil {
		return fail("The storage reset is finished, but its record could not be deleted.",
			orDash(lastLines(out, 2))+"\nDelete it yourself: kubectl delete configmap "+resetMarkerConfigMap+" -n "+namespace+"\nOr rerun the upgrade, which finds the new table, drops nothing and deletes it.")
	}
	return nil
}

// ── Objects of the release ───────────────────────────────────────────────────

// kubeObject is what the CLI reads of the release's Pods, Deployments,
// StatefulSets, Jobs and ConfigMaps (kubectl get -o json).
type kubeObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		Generation        int64             `json:"generation"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		OwnerReferences   []ownerRef        `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Replicas   *int32          `json:"replicas"`
		Containers []kubeContainer `json:"containers"`
		Template   struct {
			Spec struct {
				Containers []kubeContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Phase              string `json:"phase"`
		ObservedGeneration int64  `json:"observedGeneration"`
		UpdatedReplicas    int32  `json:"updatedReplicas"`
		UpdateRevision     string `json:"updateRevision"`
		ContainerStatuses  []struct {
			Ready bool `json:"ready"`
		} `json:"containerStatuses"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
	Data map[string]string `json:"data"`
}

type kubeContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type ownerRef struct {
	Kind string `json:"kind"`
}

// containers are the containers a Pod runs, or the pod template of a
// Deployment or StatefulSet does.
func (o kubeObject) containers() []kubeContainer {
	if o.Kind == "Pod" {
		return o.Spec.Containers
	}
	return o.Spec.Template.Spec.Containers
}

// ownedByJob reports whether a pod belongs to a Job (the schema Jobs).
func (o kubeObject) ownedByJob() bool {
	return slices.ContainsFunc(o.Metadata.OwnerReferences, func(r ownerRef) bool { return r.Kind == "Job" })
}

// ready reports whether a pod runs, is not going away and every container in
// it is ready.
func (o kubeObject) ready() bool {
	if o.Status.Phase != "Running" || o.Metadata.DeletionTimestamp != "" || len(o.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, c := range o.Status.ContainerStatuses {
		if !c.Ready {
			return false
		}
	}
	return true
}

// finished reports whether a Job has completed or failed for good.
func (o kubeObject) finished() bool {
	for _, c := range o.Status.Conditions {
		if (c.Type == "Complete" || c.Type == "Failed") && c.Status == "True" {
			return true
		}
	}
	return false
}

// kubeList lists objects of the kinds given (kubectl get's first argument,
// e.g. "statefulsets,pods") in the release namespace.
func kubeList(kinds string, args ...string) ([]kubeObject, error) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "kubectl", append([]string{"get", kinds, "-n", namespace, "-o", "json"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("kubectl get %s: %s", kinds, orDash(lastLines(out, 1)))
	}
	var list struct {
		Items []kubeObject `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("could not read kubectl get %s: %w", kinds, err)
	}
	return list.Items, nil
}

// ── ClickHouse ───────────────────────────────────────────────────────────────

// kubeClickHouse is the release's bundled ClickHouse: a ready pod of its
// StatefulSet, which clickhouse-client runs in, and the image the
// StatefulSet runs.
type kubeClickHouse struct {
	pod, image string
}

// exec runs a shell snippet in the ClickHouse pod, within
// clickhouseProbeTimeout.
func (k kubeClickHouse) exec(script string) (string, error) {
	return kubeRunTimeout(clickhouseProbeTimeout, "kubectl", "exec", "-n", namespace, k.pod, "--", "sh", "-c", script)
}

// probe asks ClickHouse what its events table is.
func (k kubeClickHouse) probe() liveSchema {
	out, err := k.exec(clickhouseEventsPropertiesType)
	if err != nil {
		return liveSchema{out: out}
	}
	return parseLiveSchema(out)
}

// session is how the reset reaches this ClickHouse.
func (k kubeClickHouse) session() chSession {
	return chSession{
		query: k.exec,
		stmt: func(script string) (string, error) {
			return kubeRunTimeout(kubeStatementTimeout, "kubectl", "exec", "-n", namespace, k.pod, "--", "sh", "-c", script)
		},
		byHand: func(script string) string {
			return "kubectl exec -n " + namespace + " " + k.pod + " -- sh -c '" + script + "'"
		},
	}
}

// findKubeClickHouse finds the release's bundled ClickHouse by the chart's
// labels (componentSelector("clickhouse")): its StatefulSet, and a ready pod
// of it that is not going away. current asks for the pod of the
// StatefulSet's current spec, once its rollout is through (after a chart
// change, the pod still running may be the one that is about to be
// replaced). why says what is missing, "" when it was found.
func findKubeClickHouse(current bool) (kubeClickHouse, string) {
	objs, err := kubeList("statefulsets,pods", "-l", componentSelector("clickhouse"))
	if err != nil {
		return kubeClickHouse{}, err.Error()
	}
	var sts []kubeObject
	var pods []kubeObject
	for _, o := range objs {
		switch o.Kind {
		case "StatefulSet":
			sts = append(sts, o)
		case "Pod":
			pods = append(pods, o)
		}
	}
	if len(sts) != 1 {
		return kubeClickHouse{}, fmt.Sprintf("the release has %d ClickHouse StatefulSets (%s), not one", len(sts), componentSelector("clickhouse"))
	}
	s := sts[0]
	ch := kubeClickHouse{image: clickhouseContainerImage(s.containers())}
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	if current && (s.Status.ObservedGeneration < s.Metadata.Generation || s.Status.UpdatedReplicas < replicas) {
		return ch, "ClickHouse is still moving to its new spec (StatefulSet " + s.Metadata.Name + ")"
	}
	slices.SortFunc(pods, func(a, b kubeObject) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	for _, p := range pods {
		if !p.ready() {
			continue
		}
		if current && p.Metadata.Labels["controller-revision-hash"] != s.Status.UpdateRevision {
			continue
		}
		ch.pod = p.Metadata.Name
		return ch, ""
	}
	return ch, "no ClickHouse pod is ready (kubectl get pods -n " + namespace + " -l " + componentSelector("clickhouse") + ")"
}

// clickhouseContainerImage is the image of the container named clickhouse,
// else of the first.
func clickhouseContainerImage(cs []kubeContainer) string {
	for _, c := range cs {
		if c.Name == "clickhouse" {
			return c.Image
		}
	}
	if len(cs) > 0 {
		return cs[0].Image
	}
	return ""
}

// waitKubeClickHouse waits, up to kubeClickHouseTimeout, until the bundled
// ClickHouse is there (findKubeClickHouse, current as there) and answers a
// query. why says what it is not, "" once it is.
func waitKubeClickHouse(current bool) (kubeClickHouse, string) {
	deadline := time.Now().Add(kubeClickHouseTimeout)
	for {
		ch, why := findKubeClickHouse(current)
		if why == "" {
			out, err := ch.exec(clickhousePing)
			if err == nil {
				return ch, ""
			}
			why = "ClickHouse (" + ch.pod + ") does not answer: " + orDash(lastLines(out, 1))
		}
		if time.Now().After(deadline) {
			return ch, why
		}
		time.Sleep(kubePoll)
	}
}

// askKubeClickHouse asks the release's ClickHouse which events table it
// holds: the bundled one in its pod (waited for while it is not ready), the
// operator's own (external) through a probe Job. gen stays "" when it could
// not be asked, with why in out; the caller refuses rather than guess from
// the chart versions.
func askKubeClickHouse(external bool) liveSchema {
	if external {
		sp := ui.Start("Asking your ClickHouse which events table it holds")
		live := probeExternalClickHouse()
		if live.gen == "" {
			sp.Fail("Your ClickHouse could not be asked")
			return live
		}
		sp.Success("Your ClickHouse holds %s", live.describe())
		return live
	}
	sp := ui.Start("Asking ClickHouse which events table it holds")
	ch, why := waitKubeClickHouse(false)
	if why != "" {
		sp.Fail("ClickHouse could not be asked")
		return liveSchema{out: why}
	}
	live := ch.probe()
	if live.gen == "" {
		sp.Fail("ClickHouse did not say which events table it holds")
		return live
	}
	sp.Success("ClickHouse holds %s", live.describe())
	return live
}

// clickhouseServerProblem says why the bundled ClickHouse ch cannot hold the
// new events table, "" when it can: its image's tag, or the server when the
// tag names no version, is minClickHouseForJSON or newer.
func clickhouseServerProblem(ch kubeClickHouse) string {
	if v, ok := clickHouseImageVersion(ch.image); ok {
		if versionLess(v+".0", minClickHouseForJSON+".0") {
			return "ClickHouse runs " + ch.image
		}
		return ""
	}
	out, err := ch.exec(clickhouseServerVersion)
	sv, ok := serverVersion(out)
	switch {
	case err != nil || !ok:
		return "ClickHouse runs " + ch.image + ", whose version neither its tag nor the server (" + orDash(lastLines(out, 1)) + ") tells"
	case versionLess(sv+".0", minClickHouseForJSON+".0"):
		return "ClickHouse runs " + ch.image + " (ClickHouse " + sv + ")"
	}
	return ""
}

// ── External ClickHouse ──────────────────────────────────────────────────────

// externalProbeScript prints the payload column's type of the events table
// on the operator's ClickHouse, connecting the way the chart's schema Job
// does (its environment: CH_HOST, CH_PORT, CH_USER, CH_SECURE, CH_PASSWORD).
const externalProbeScript = `set -e
set -- --host "$CH_HOST" --port "$CH_PORT" --user "$CH_USER"
if [ -n "${CH_SECURE:-}" ]; then set -- "$@" --secure; fi
if [ -n "${CH_PASSWORD:-}" ]; then set -- "$@" --password "$CH_PASSWORD"; fi
clickhouse-client "$@" --query "` + eventsPropertiesQuery + `"
`

// reManifestJob finds the chart's schema Job in a release manifest.
var (
	reManifestKindJob = regexp.MustCompile(`(?m)^kind:\s*Job\s*$`)
	reManifestInitJob = regexp.MustCompile(`(?m)^\s+app\.kubernetes\.io/component:\s*"?clickhouse-init"?\s*$`)
)

// externalProbeJob renders the Job that asks an external ClickHouse what its
// events table is (externalProbeScript), from the release's own schema Job
// as `helm get manifest` has it: the same image, environment (the password
// from its Secret), security context, pull secrets and the selector labels
// the chart's NetworkPolicy opens egress to that server for. name is the
// Job's. why says why there is none to make ("" when there is).
func externalProbeJob(name string) (manifest []byte, why string) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "helm", "get", "manifest", release, "-n", namespace)
	if err != nil {
		return nil, "helm could not read the release manifest: " + orDash(lastLines(out, 1))
	}
	doc := ""
	for _, d := range strings.Split(out, "\n---") {
		if reManifestKindJob.MatchString(d) && reManifestInitJob.MatchString(d) {
			doc = d
			break
		}
	}
	if doc == "" {
		return nil, "the release has no ClickHouse schema Job to ask it with (clickhouse.initSchema=false)"
	}
	// kubectl turns the YAML into JSON; nothing is sent to the cluster.
	js, err := kubeRunStdin(kubeRequestTimeout, doc, "kubectl", "create", "--dry-run=client", "-o", "json", "-f", "-")
	if err != nil {
		return nil, "kubectl could not read the schema Job: " + orDash(lastLines(js, 1))
	}
	var job map[string]any
	if err := json.Unmarshal([]byte(js), &job); err != nil {
		return nil, "could not read the schema Job: " + err.Error()
	}
	spec, _ := job["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) == 0 {
		return nil, "the schema Job has no container"
	}
	c, _ := containers[0].(map[string]any)
	if c == nil {
		return nil, "the schema Job has no container"
	}
	c["command"] = []any{"/bin/sh", "-c", externalProbeScript}
	delete(c, "args")
	podSpec["restartPolicy"] = "Never"
	if meta, _ := tmpl["metadata"].(map[string]any); meta != nil {
		if labels, _ := meta["labels"].(map[string]any); labels != nil {
			labels["app.kubernetes.io/component"] = "clickhouse-probe"
		}
	}
	probe := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/instance":   release,
				"app.kubernetes.io/component":  "clickhouse-probe",
				"app.kubernetes.io/managed-by": "origamy-cli",
			},
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   300,
			"ttlSecondsAfterFinished": 600,
			"template":                tmpl,
		},
	}
	b, err := json.Marshal(probe)
	if err != nil {
		return nil, err.Error()
	}
	return b, ""
}

// probeExternalClickHouse asks the operator's own ClickHouse, which only the
// cluster may be able to reach, what its events table is: through a Job made
// from the release's schema Job (externalProbeJob), waited for up to
// kubeClickHouseTimeout and deleted again. gen is "" when it could not be
// asked, with why in out.
func probeExternalClickHouse() liveSchema {
	name := fmt.Sprintf("%s-clickhouse-probe-%d", release, time.Now().Unix())
	manifest, why := externalProbeJob(name)
	if why != "" {
		return liveSchema{out: why}
	}
	if out, err := kubeRunStdin(kubeRequestTimeout, string(manifest), "kubectl", "create", "-f", "-"); err != nil {
		return liveSchema{out: "kubectl could not create the probe Job: " + orDash(lastLines(out, 1))}
	}
	defer func() {
		_, _ = kubeRunTimeout(kubeRequestTimeout, "kubectl", "delete", "job", name, "-n", namespace, "--ignore-not-found", "--wait=false")
	}()
	deadline := time.Now().Add(kubeClickHouseTimeout)
	for {
		objs, err := kubeList("jobs", "--field-selector", "metadata.name="+name)
		if err == nil && len(objs) == 1 && objs[0].finished() {
			break
		}
		if time.Now().After(deadline) {
			return liveSchema{out: "the probe Job " + name + " did not finish within " + kubeClickHouseTimeout.String()}
		}
		time.Sleep(kubePoll)
	}
	out, err := kubeRunTimeout(kubeRequestTimeout, "kubectl", "logs", "-n", namespace, "job/"+name)
	if err != nil {
		return liveSchema{out: "kubectl could not read the probe Job's output: " + orDash(lastLines(out, 1))}
	}
	if objs, err := kubeList("jobs", "--field-selector", "metadata.name="+name); err != nil || len(objs) != 1 || !jobSucceeded(objs[0]) {
		return liveSchema{out: "the probe Job failed: " + orDash(lastLines(out, 2))}
	}
	return parseLiveSchema(out)
}

// jobSucceeded reports whether a Job completed.
func jobSucceeded(o kubeObject) bool {
	for _, c := range o.Status.Conditions {
		if c.Type == "Complete" && c.Status == "True" {
			return true
		}
	}
	return false
}

// ── What the release runs ────────────────────────────────────────────────────

// dataPlaneServices are the chart's data-plane services: their component
// labels, container names and image names.
var dataPlaneServices = []string{
	"bulker-worker", "config-sync", "destination-worker", "identity-resolver",
	"ingestion-gateway", "orchestrator-engine", "portal-agent", "predictor",
	"segment-evaluator", "source-worker", "transformer-engine", "workflow-engine",
}

// imageName is the last path element of an image reference, without tag or
// digest (ghcr.io/…/bulker-worker:0.1.20 → bulker-worker).
func imageName(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref[strings.LastIndex(ref, "/")+1:]
}

// isDataPlaneContainer reports whether a container runs a data-plane
// service: an image of dataPlaneImageRepo, or one of dataPlaneServices by
// its container or image name (a mirror in another registry).
func isDataPlaneContainer(c kubeContainer) bool {
	return strings.Contains(c.Image, dataPlaneImageRepo) || slices.Contains(dataPlaneServices, c.Name) || slices.Contains(dataPlaneServices, imageName(c.Image))
}

// imageGeneration is the events table the image of a data-plane service
// writes (releaseSchemaGeneration of its tag): "unknown" for one whose tag
// is not a numbered release nor the edge channel's, or that is pinned by
// digest alone.
func imageGeneration(ref string) string {
	return releaseSchemaGeneration(imageTag(ref))
}

// reManifestImage is an image line of a rendered manifest.
var reManifestImage = regexp.MustCompile(`(?m)^\s*-?\s*image:\s*["']?([^"'\s#]+)`)

// manifestGeneration says which events table the data-plane images of a
// rendered release manifest (helm get manifest) write: gen, or why that is
// not one generation (none, several, or one not known).
func manifestGeneration(manifest string) (gen, why string) {
	var images []string
	for _, m := range reManifestImage.FindAllStringSubmatch(manifest, -1) {
		if img := m[1]; isDataPlaneContainer(kubeContainer{Image: img}) && !slices.Contains(images, img) {
			images = append(images, img)
		}
	}
	return imagesGeneration(images)
}

// imagesGeneration is the one events table generation the data-plane images
// write, or why there is not one.
func imagesGeneration(images []string) (gen, why string) {
	if len(images) == 0 {
		return "", "it runs no data-plane service"
	}
	byGen := map[string][]string{}
	for _, img := range images {
		g := imageGeneration(img)
		byGen[g] = append(byGen[g], img)
	}
	if u := byGen["unknown"]; len(u) > 0 {
		return "", "it runs " + strings.Join(u, ", ") + ", whose tag is not a numbered release, so which events table it writes is not known"
	}
	if len(byGen) > 1 {
		return "", "its images write different events tables (pre-reset: " + strings.Join(byGen["string"], ", ") + "; new: " + strings.Join(byGen["json"], ", ") + ")"
	}
	for g := range byGen {
		gen = g
	}
	return gen, ""
}

// releaseImagesProblem is the second half of the check after a chart change
// (kubeEventsCheck): every data-plane container of the release writes gen
// (the events table ClickHouse holds), in the Deployments' and
// StatefulSets' specs (what runs once their rollouts are through) and in
// the pods that run now, Job pods aside. While ClickHouse holds the new
// table, the bundled ClickHouse runs minClickHouseForJSON or newer (ch,
// when external is false). A pod of the old images is waited for, up to
// deadline, while a rollout replaces it; anything else fails at once. It
// returns why the release's images are not right, "" when they are.
func releaseImagesProblem(gen string, external bool, ch kubeClickHouse, deadline time.Time) string {
	for {
		objs, err := kubeList("deployments,statefulsets,pods", "-l", releaseSelector)
		if err != nil {
			return err.Error()
		}
		var wrong, waiting []string
		services := 0
		for _, o := range objs {
			if o.Kind == "Pod" && (o.ownedByJob() || o.Status.Phase == "Succeeded" || o.Status.Phase == "Failed") {
				continue
			}
			for _, c := range o.containers() {
				if !isDataPlaneContainer(c) {
					continue
				}
				what := strings.ToLower(o.Kind) + " " + o.Metadata.Name + " runs " + c.Image
				switch g := imageGeneration(c.Image); {
				case g == "unknown":
					wrong = append(wrong, what+", whose tag is not a numbered release, so which events table it writes is not known")
				case g != gen && o.Kind == "Pod":
					waiting = append(waiting, what)
				case g != gen:
					wrong = append(wrong, what)
				case o.Kind != "Pod":
					services++
				}
			}
		}
		if services == 0 && len(wrong) == 0 {
			wrong = append(wrong, "no data-plane service is deployed")
		}
		if gen == "json" && !external {
			if why := clickhouseServerProblem(ch); why != "" {
				wrong = append(wrong, why+", and the new events table needs ClickHouse "+minClickHouseForJSON+" or newer")
			}
		}
		if len(wrong) > 0 {
			return listSome(wrong)
		}
		if len(waiting) == 0 {
			return ""
		}
		if time.Now().After(deadline) {
			return "pods still run images that write the other events table after " + kubeCheckTimeout.String() + ": " + listSome(waiting)
		}
		time.Sleep(kubePoll)
	}
}

// listSome joins the first three of items, and says how many more there are.
func listSome(items []string) string {
	if len(items) <= 3 {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(items[:3], "; "), len(items)-3)
}

// waitSchemaJobs waits, up to deadline, until the release's schema Jobs
// (the chart runs one per revision, against whichever ClickHouse it uses)
// have finished. It reports whether they did.
func waitSchemaJobs(deadline time.Time) bool {
	for {
		objs, err := kubeList("jobs", "-l", componentSelector("clickhouse-init"))
		if err == nil && !slices.ContainsFunc(objs, func(o kubeObject) bool { return !o.finished() }) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(kubePoll)
	}
}

// kubeCheck is what the check after a chart change found.
type kubeCheck struct {
	live liveSchema
	// why the release fails it, "" when it passes.
	why string
	// images: the failure is in what runs, not in the table.
	images bool
}

// kubeEventsCheck is the check every Kubernetes upgrade, rollback and
// install ends with, and the one an upgrade with nothing to do makes: once
// the schema Jobs are through (waited for up to kubeCheckTimeout),
// ClickHouse answers (the bundled one on its StatefulSet's current spec,
// an external one through a probe Job), its events table exists with
// payload columns of one generation, and every data-plane image the release
// runs, or will run once its rollouts are through, writes that generation
// (releaseImagesProblem).
func kubeEventsCheck(external bool) kubeCheck {
	deadline := time.Now().Add(kubeCheckTimeout)
	sp := ui.Start("Checking the events table the release's images write to")
	if !waitSchemaJobs(deadline) {
		sp.Suffix("the schema Job is still at work")
	}
	var ch kubeClickHouse
	var live liveSchema
	if external {
		live = probeExternalClickHouse()
	} else {
		var why string
		if ch, why = waitKubeClickHouse(true); why != "" {
			live = liveSchema{out: why}
		} else {
			live = ch.probe()
		}
	}
	switch {
	case live.gen == "":
		sp.Fail("ClickHouse could not be asked")
		return kubeCheck{live: live, why: "ClickHouse could not be asked whether " + eventsTable + " is there (" + orDash(lastLines(live.out, 1)) + ")"}
	case !live.known():
		sp.Fail("ClickHouse holds %s", live.describe())
		return kubeCheck{live: live, why: "ClickHouse holds " + live.describe()}
	}
	if why := releaseImagesProblem(live.gen, external, ch, deadline); why != "" {
		sp.Fail("What runs does not write %s", live.describe())
		return kubeCheck{live: live, why: "ClickHouse holds " + live.describe() + ", and " + why, images: true}
	}
	sp.Success("ClickHouse holds %s, which every data-plane service writes", live.describe())
	return kubeCheck{live: live}
}

// kubeCheckFail fails a run whose check (kubeEventsCheck) found c: done says
// what the run did ("The chart is on 0.1.20", …), recheck is the command
// that checks again (and does what is left to do), recorded says the run
// left a storage reset recorded as unfinished (resetMarkerConfigMap).
func kubeCheckFail(done, recheck string, c kubeCheck, external, recorded bool) error {
	headline := done + ", but " + c.why + "."
	again := "Then `" + recheck + "` checks again."
	if recorded {
		again = "The storage reset stays recorded as unfinished (ConfigMap " + resetMarkerConfigMap + " in " + namespace + "): `" + recheck + "` finishes it and checks again."
	}
	switch {
	case c.live.gen == "":
		return fail(headline, "Check: kubectl get pods -n "+namespace+" (and the schema Job: kubectl logs -n "+namespace+" -l "+componentSelector("clickhouse-init")+" --tail=20)\nOnce it answers: "+again)
	case c.live.gen == liveOther:
		return fail(headline, "No release writes it, and the storage reset does not drop it. Check what created it, and drop it yourself if it should be recreated with the new schema. "+again)
	case external && c.live.gen != "json":
		return fail(headline, "The CLI does not reset or create the events table on an external ClickHouse beyond the chart's schema Job. On your server, rename (or drop) "+eventsTable+" if it is the pre-reset one, and apply the chart's schema:\n  kubectl get configmap "+release+"-clickhouse-init -n "+namespace+" -o jsonpath='{.data.init\\.sql}' | clickhouse-client --host … --port … --user … --password … [--secure] --multiquery\n"+again)
	case c.live.gen == liveMissing:
		return fail(headline, "The chart's schema Job creates it: check its output (kubectl logs -n "+namespace+" -l "+componentSelector("clickhouse-init")+" --tail=20). For a release that writes the new table, `origamy upgrade` creates it itself (nothing is dropped). "+again)
	}
	which := "Releases before " + minImageForJSONSchema + " cannot write it."
	if c.live.gen == "string" {
		which = "Releases from " + minImageForJSONSchema + " on cannot write it; `origamy upgrade --version " + minImageForJSONSchema + "` or newer moves it across the storage reset (asking before it drops anything)."
	}
	return fail(headline, "Until every data-plane service writes it, their events are accepted and then dropped at insert. "+which+"\n"+
		"Check what pins those images: a per-service image.tag or digest in the release values (helm get values "+release+" -n "+namespace+") stays there until you replace it, e.g. `origamy upgrade --set bulkerWorker.image.tag=<release>`; and the rollouts (kubectl get pods -n "+namespace+").\n"+again)
}

// ── The reset ────────────────────────────────────────────────────────────────

// resetKubernetesEvents recreates the release's events table with the
// chart's schema once the chart is on version (channel) and the release
// writes the new table. The reset is recorded first (resetMarkerConfigMap),
// then ClickHouse is waited for on its StatefulSet's current spec, which
// must run minClickHouseForJSON or newer.
//
// What ClickHouse holds then decides, held against what it held when the run
// made its decision (found): the table is moved aside only when ClickHouse
// says it is the pre-reset one (String payload columns) and the run found
// that; the chart's schema (from its ConfigMap, which must be the new one) is
// applied to that or to no table; the new table is left alone. Moved aside is
// RENAME TABLE events.events TO events.events_pre_reset_<unix time>, never a
// DROP: the reset counts as done only once ClickHouse reports the new table,
// and only then is what was moved aside dropped, while it is the pre-reset
// one (settleAside). discard says the operator agreed to discard the
// pre-reset table (this run asked, or the run that began the reset did).
func resetKubernetesEvents(version, channel, found string, discard bool) (resetResult, error) {
	none := resetResult{}
	rerun := "rerun `" + kubeUpgradeCommand(version, channel) + "`: the unfinished reset is recorded in the cluster (ConfigMap " + resetMarkerConfigMap + " in " + namespace + ") and is finished before anything else."
	if err := writeResetMarker(version, channel); err != nil {
		return none, fail("The chart is on "+version+", but the storage reset could not be recorded in the cluster, so the events table was not reset yet.",
			err.Error()+"\nFix the cause, then rerun `"+kubeUpgradeCommand(version, channel)+"`: it asks ClickHouse again and finishes the reset.")
	}
	sp := ui.Start("Waiting for ClickHouse to come back on the new image")
	ch, why := waitKubeClickHouse(true)
	if why != "" {
		sp.Fail("ClickHouse is not ready after the upgrade")
		return none, fail("ClickHouse is not ready after the upgrade, so the events table was not reset yet.",
			why+"\nCheck: kubectl get pods -n "+namespace+"\nOnce it answers, "+rerun)
	}
	if why := clickhouseServerProblem(ch); why != "" {
		sp.Fail("ClickHouse is too old for the new events table")
		return none, fail("The new events table needs ClickHouse "+minClickHouseForJSON+" or newer, and "+why+", so nothing was changed in it.",
			"Set clickhouse.image to clickhouse/clickhouse-server:"+minClickHouseForJSON+" or newer (or take it out of your values), then "+rerun)
	}
	sp.Success("ClickHouse is up (%s)", ch.pod)

	sess := ch.session()
	sp = ui.Start("Checking the events table")
	live := ch.probe()
	switch {
	case live.gen == "json":
		sp.Success("ClickHouse already holds the new events table (%s payload columns): nothing to move aside", live.typ)
		return settleAside(sess, resetResult{outcome: resetAlready}, "", discard), nil
	case live.gen == "":
		sp.Fail("ClickHouse did not say which events table it holds")
		return none, fail("Could not read which events table ClickHouse holds, so nothing was dropped.", outputHint(live.out)+"Once it answers, "+rerun)
	case live.gen == liveOther:
		sp.Fail("Unexpected events table")
		return none, fail(fmt.Sprintf("ClickHouse's %s has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, so the reset did not drop it.", eventsTable, live.typ),
			"Check what created it. To recreate it with the new schema, drop it yourself:\n  "+sess.byHand(strings.ReplaceAll(clickhouseDropEvents, " IF EXISTS", ""))+"\nthen "+rerun)
	case (live.gen == "string" && found != "string") || (live.gen == liveMissing && found == "json"):
		sp.Fail("ClickHouse holds %s", live.describe())
		return none, fail("ClickHouse holds "+live.describe()+", although it held "+liveSchema{gen: found}.describe()+" when this run began, so this run changed nothing in it.",
			"Rerun `"+kubeUpgradeCommand(version, channel)+"`: it asks again, before it drops anything.")
	case live.gen == "string":
		sp.Success("ClickHouse holds the pre-reset events table (String payload columns)")
	default:
		sp.Success("ClickHouse has no events table")
	}
	// The chart's schema Job applies the SQL in its ConfigMap; the reset
	// applies the same, which must be the new schema before anything moves.
	schema, why := chartSchema()
	if why == "" && schemaGeneration(schema) != "json" {
		why = "it is not the new schema (native JSON payload columns)"
	}
	if why != "" {
		sp.Fail("The chart's ClickHouse schema cannot be applied")
		return none, fail("The chart's ClickHouse schema (the ConfigMap its schema Job applies): "+why+", so nothing was dropped or created.",
			"Check: kubectl get configmaps -n "+namespace+" -l "+componentSelector("clickhouse")+"\nThen "+rerun)
	}

	sp = ui.Start("Creating the events table with the new schema")
	outcome, did, done, aside, kept := resetCreated, "The new schema", "Events table created", "", ""
	if live.gen == "string" {
		aside = asideName(sess.query)
		sp.Suffix("the pre-reset one is moved aside first, as events.%s", aside)
		if out, err := sess.stmt(clickhouseMoveEventsAside(aside)); err != nil {
			sp.Fail("Could not move the old events table aside")
			return none, fail("The old events table could not be moved aside (renamed to events."+aside+"), so nothing was changed in ClickHouse.", outputHint(out)+"Fix the cause, then "+rerun)
		}
		outcome, did, done = resetDropped, "The old events table was moved aside, but the new schema", "Events table recreated"
		kept = "The pre-reset events table is events." + aside + " now, its data still there: the next run drops it once the new table is in place.\n"
	}
	out, err := kubeRunStdin(kubeStatementTimeout, schema, "kubectl", "exec", "-i", "-n", namespace, ch.pod, "--", "sh", "-c", clickhouseInitStdin)
	if err != nil {
		sp.Fail("Schema init failed")
		return none, fail(did+" could not be applied.", kept+outputHint(out)+"Fix the cause, then "+rerun)
	}
	switch got := ch.probe(); got.gen {
	case "json":
	case "":
		sp.Fail("Could not confirm the new events table")
		return none, fail("The schema was applied, but ClickHouse could not be asked whether "+eventsTable+" is the new table now.", kept+outputHint(got.out)+"Once it answers, "+rerun)
	default:
		sp.Fail("The events table is not the new one")
		have := map[string]string{"string": "still the pre-reset one (String payload columns)", liveMissing: "missing", liveOther: "of type " + got.typ}[got.gen]
		return none, fail("The schema was applied, but ClickHouse's "+eventsTable+" is "+have+" afterwards.",
			kept+outputHint(out)+"The schema's output is above. Fix the cause (kubectl logs -n "+namespace+" "+ch.pod+"), then "+rerun)
	}
	sp.Success("%s", done)
	return settleAside(sess, resetResult{outcome: outcome}, aside, discard || aside != ""), nil
}

// chartSchema is the ClickHouse schema the release's chart applies: init.sql
// of its ConfigMap (labelled with the clickhouse component). why says why it
// could not be read, "" when it was.
func chartSchema() (sql, why string) {
	objs, err := kubeList("configmaps", "-l", componentSelector("clickhouse"))
	if err != nil {
		return "", err.Error()
	}
	for _, o := range objs {
		if s := o.Data["init.sql"]; strings.TrimSpace(s) != "" {
			return s, ""
		}
	}
	return "", "the release has none (clickhouse.initSchema=false?)"
}

// ── Before the chart changes ─────────────────────────────────────────────────

// kubeResetValuesProblem says why the reset cannot run with the release's
// values (vals, as helm get values has them) and the --set flags of the run,
// "" when it can: the chart's schema Job (and the ConfigMap the reset applies)
// must be on, and a ClickHouse image set there must be minClickHouseForJSON
// or newer.
func kubeResetValuesProblem(vals map[string]any, sets []string) string {
	ch, _ := vals["clickhouse"].(map[string]any)
	if on, set := ch["initSchema"].(bool); set && !on {
		return "your values turn the chart's schema off (clickhouse.initSchema=false), so there is no schema for the reset to apply"
	}
	image, _ := ch["image"].(string)
	for _, s := range sets {
		if v, ok := strings.CutPrefix(s, "clickhouse.image="); ok {
			image = v
		}
		if v, ok := strings.CutPrefix(s, "clickhouse.initSchema="); ok && v == "false" {
			return "--set clickhouse.initSchema=false turns the chart's schema off, so there is no schema for the reset to apply"
		}
	}
	if image == "" {
		return ""
	}
	if v, ok := clickHouseImageVersion(image); !ok || versionLess(v+".0", minClickHouseForJSON+".0") {
		return "your values set the ClickHouse image to " + image + ", and the new events table needs ClickHouse " + minClickHouseForJSON + " or newer, named by the image's tag"
	}
	return ""
}

// errNoHistory means helm has no such revision of the release.
var errNoHistory = errors.New("no such revision")

// releaseRevision is one revision of the release (helm history).
type releaseRevision struct {
	Revision int    `json:"revision"`
	Chart    string `json:"chart"`
	Status   string `json:"status"`
}

// rollbackTarget is the revision `helm rollback` moves to: to, or with none
// (0) the one before the newest, as helm takes it.
func rollbackTarget(to int) (releaseRevision, error) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "helm", "history", release, "-n", namespace, "-o", "json")
	if err != nil {
		return releaseRevision{}, fmt.Errorf("%s", orDash(lastLines(out, 1)))
	}
	var hist []releaseRevision
	if err := json.Unmarshal([]byte(out), &hist); err != nil {
		return releaseRevision{}, fmt.Errorf("could not read helm history: %w", err)
	}
	if to == 0 {
		for _, r := range hist {
			to = max(to, r.Revision)
		}
		to--
	}
	for _, r := range hist {
		if r.Revision == to {
			return r, nil
		}
	}
	return releaseRevision{Revision: to}, errNoHistory
}

// revisionValues is what the operator supplied to one revision of the release
// (helm get values --revision).
func revisionValues(rev int) (map[string]any, error) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "helm", "get", "values", release, "-n", namespace, "--revision", strconv.Itoa(rev), "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("%s", orDash(lastLines(out, 1)))
	}
	vals := map[string]any{}
	if t := strings.TrimSpace(out); t != "" && t != "null" {
		if err := json.Unmarshal([]byte(out), &vals); err != nil {
			return nil, fmt.Errorf("could not read the values of revision %d: %w", rev, err)
		}
	}
	return vals, nil
}

// revisionManifest is what one revision of the release renders (helm get
// manifest --revision).
func revisionManifest(rev int) (string, error) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "helm", "get", "manifest", release, "-n", namespace, "--revision", strconv.Itoa(rev))
	if err != nil {
		return "", fmt.Errorf("%s", orDash(lastLines(out, 1)))
	}
	return out, nil
}

// kubeDeployProblem is the check an `origamy deploy` makes, before the token
// is spent, over a release that is already installed: a deploy moves the
// chart to chartVer without the reset, so a reset an upgrade left
// unfinished, or a ClickHouse holding an events table the images of chartVer
// cannot write, is refused (headline and hint), and so is one that cannot be
// asked. A release on an external ClickHouse is checked after the install.
func kubeDeployProblem(chartVer string) (headline, hint string) {
	nothing := "Nothing has been enrolled or installed."
	vals, err := releaseValues()
	if err != nil {
		return "Could not read the values of the release already installed here.", err.Error() + "\n" + nothing
	}
	m, err := readResetMarker()
	if err != nil {
		return "Could not read whether an earlier upgrade left a storage reset unfinished.", err.Error() + "\n" + nothing
	}
	if m.present {
		return "An earlier upgrade began the storage reset here and did not finish it (ConfigMap " + resetMarkerConfigMap + " in " + namespace + ").",
			"Finish it first: " + m.command() + ". " + nothing
	}
	if usesExternalClickHouse(vals) {
		return "", ""
	}
	live := askKubeClickHouse(false)
	want := releaseSchemaGeneration(chartVer)
	switch {
	case live.gen == "":
		return "ClickHouse could not be asked which events table it holds, so this deploy cannot tell whether release " + chartVer + " can write it.",
			outputHint(live.out) + "Check: kubectl get pods -n " + namespace + "\nOnce it answers, retry. " + nothing
	case live.gen == liveOther:
		return fmt.Sprintf("ClickHouse's %s has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, which no release writes.", eventsTable, live.typ),
			"Check what created it, and drop it yourself if it should be recreated with the new schema; then retry. " + nothing
	case live.gen == "string" && want == "json":
		return "ClickHouse here holds the pre-reset events table (String payload columns), which release " + chartVer + " cannot write, and a deploy does not reset it.",
			"Move the installed release across the storage reset first: origamy upgrade --version " + chartVer + " (it asks before it discards the event history). " + nothing
	case live.gen == "json" && want == "string":
		return "ClickHouse here holds the new events table (JSON payload columns), which release " + chartVer + " cannot write: events would be accepted and then dropped at insert.",
			"Deploy " + minImageForJSONSchema + " or newer (--version). " + nothing
	}
	return "", ""
}
