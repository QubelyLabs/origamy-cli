package cmd

import (
	"cmp"
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
// refused before anything changes.
//
// All of that, and the permissions it needs (pods/exec on ClickHouse, a
// lease, the reset record), applies only to a move that can touch the events
// table (kube_move.go): images that cross the storage reset or cannot be
// told, or a reset recorded as unfinished. Any other upgrade, rollback or
// deploy only reads the images the release runs afterwards.

// resetMarkerConfigMap records, in the release namespace, a storage reset an
// upgrade is about to run or has begun and not finished: it is written
// before the chart moves (so an upgrade that cannot record it does not
// start), and deleted once the new table is in place and the check after the
// upgrade passed. While it is there, `origamy upgrade` finishes the reset
// whatever version the chart is on (with a release that writes the new
// table), and refuses one that does not, `origamy rollback` and an in-place
// `origamy deploy` are refused, and the table the reset moved aside (its
// aside field) is dropped once the new one is in place when the operator
// agreed to that (its action is markerReset).
const resetMarkerConfigMap = "origamy-reset-pending"

// A reset record's action: the operator agreed to discard the pre-reset
// table (markerReset), or there was none to discard and the new one was to
// be created (markerCreate). A create never turns into consent to drop.
const (
	markerReset  = "reset"
	markerCreate = "create"
)

// releaseSelector selects every object of the release by the label the
// chart puts on all of them; componentSelector, one of its components.
const releaseSelector = "app.kubernetes.io/instance=" + release

func componentSelector(component string) string {
	return releaseSelector + ",app.kubernetes.io/component=" + component
}

// How long the Kubernetes reset and the check after a chart change wait:
// for ClickHouse to answer before a move (kubeAskTimeout), to be back on its
// StatefulSet's current spec once the chart moved (kubeClickHouseTimeout),
// for the schema Jobs to finish and the service pods to run the release's
// images (kubeCheckTimeout), or, in a check that must not hold a run up, for
// the same (kubeSoftTimeout). kubeRequestTimeout bounds each kubectl read,
// kubeStatementTimeout each statement that changes ClickHouse (RENAME, the
// schema, DROP). Variables so tests need not wait.
var (
	kubeAskTimeout        = 30 * time.Second
	kubeClickHouseTimeout = 5 * time.Minute
	kubeCheckTimeout      = 5 * time.Minute
	kubeSoftTimeout       = 30 * time.Second
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

// isForbidden reports whether kubectl's output says the Kubernetes API
// refused the request for want of a permission: no amount of waiting helps.
func isForbidden(out string) bool {
	return strings.Contains(strings.ToLower(out), "forbidden")
}

// ── The reset record ─────────────────────────────────────────────────────────

// kubeMarker is the record of an unfinished reset (resetMarkerConfigMap):
// the chart version and channel of the upgrade that began it, whether the
// operator agreed to discard the pre-reset table (action), and the name that
// table is moved to (aside, events.<aside>).
type kubeMarker struct {
	present                  bool
	version, channel, action string
	aside                    string
}

// command is the upgrade that finishes the recorded reset.
func (m kubeMarker) command() string {
	if m.version == "" {
		return "origamy upgrade"
	}
	return kubeUpgradeCommand(m.version, m.channel)
}

// consented reports whether the operator agreed, when the reset was recorded,
// to discard the pre-reset table it moves aside. A record without an action
// (not one this CLI wrote) never counts.
func (m kubeMarker) consented() bool { return m.present && m.action == markerReset }

// mayDrop says whether the reset may drop the table named name, which it
// moved aside: only the one its own record names, and only with consent.
func (m kubeMarker) mayDrop(name string) bool {
	return m.consented() && m.aside != "" && name == m.aside
}

// kubeReadError is a kubectl read that failed; forbidden says the cause is a
// missing permission.
type kubeReadError struct {
	msg       string
	forbidden bool
}

func (e *kubeReadError) Error() string { return e.msg }

// readResetMarker reads the record of an unfinished reset; err when kubectl
// could not tell whether there is one (a *kubeReadError).
func readResetMarker() (kubeMarker, error) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "kubectl", "get", "configmap", resetMarkerConfigMap, "-n", namespace, "--ignore-not-found", "-o", "json")
	if err != nil {
		return kubeMarker{}, &kubeReadError{msg: fmt.Sprintf("kubectl could not read the ConfigMap %s in %s: %s", resetMarkerConfigMap, namespace, orDash(lastLines(out, 1))), forbidden: isForbidden(out)}
	}
	if strings.TrimSpace(out) == "" {
		return kubeMarker{}, nil
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return kubeMarker{}, &kubeReadError{msg: fmt.Sprintf("could not read the ConfigMap %s in %s: %v", resetMarkerConfigMap, namespace, err)}
	}
	return kubeMarker{present: true, version: cm.Data["version"], channel: cm.Data["channel"], action: cm.Data["action"], aside: cm.Data["aside"]}, nil
}

// writeResetMarker records a reset (kubectl apply of the ConfigMap).
func writeResetMarker(m kubeMarker) error {
	data := map[string]string{
		"version": m.version,
		"channel": m.channel,
		"action":  m.action,
		"began":   time.Now().UTC().Format(time.RFC3339),
	}
	if m.aside != "" {
		data["aside"] = m.aside
	}
	out, err := kubectlApply(configMapManifest(namespace, resetMarkerConfigMap, data))
	if err != nil {
		return fmt.Errorf("kubectl apply: %s", orDash(lastLines(out, 2)))
	}
	return nil
}

// deleteResetMarker deletes the record.
func deleteResetMarker() (string, error) {
	return kubeRunTimeout(kubeRequestTimeout, "kubectl", "delete", "configmap", resetMarkerConfigMap, "-n", namespace, "--ignore-not-found")
}

// clearResetMarker deletes the record once the reset is done and checked.
func clearResetMarker() error {
	out, err := deleteResetMarker()
	if err != nil {
		return fail("The storage reset is finished, but its record could not be deleted.",
			orDash(lastLines(out, 2))+"\nDelete it yourself: kubectl delete configmap "+resetMarkerConfigMap+" -n "+namespace+"\nOr rerun the upgrade, which finds the new table, drops nothing and deletes it.")
	}
	return nil
}

// restoreResetMarker puts the record back as it was before a run rewrote it
// (prev), or removes the one the run wrote when there was none, for a run
// whose chart did not move.
func restoreResetMarker(prev kubeMarker) error {
	if prev.present {
		return writeResetMarker(prev)
	}
	out, err := deleteResetMarker()
	if err != nil {
		return fmt.Errorf("kubectl delete: %s", orDash(lastLines(out, 2)))
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
		ResourceVersion   string            `json:"resourceVersion"`
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
			State struct {
				Waiting *struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
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

// stateWords says in a few words what state a pod is in: its phase, why a
// container waits (CrashLoopBackOff, ImagePullBackOff, …) or is not ready, and
// that it cannot be scheduled.
func (o kubeObject) stateWords() string {
	words := []string{cmp.Or(o.Status.Phase, "Unknown")}
	add := func(w string) {
		if w != "" && !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	for _, c := range o.Status.ContainerStatuses {
		switch {
		case c.State.Waiting != nil && c.State.Waiting.Reason != "":
			add(c.State.Waiting.Reason)
		case !c.Ready:
			add("not ready")
		}
	}
	for _, c := range o.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			add(cmp.Or(c.Reason, "not scheduled"))
		}
	}
	if o.Metadata.DeletionTimestamp != "" {
		add("terminating")
	}
	return strings.Join(words, ", ")
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

// execForbiddenWhy says that the user may not run commands in the ClickHouse
// pod (the permission is create on pods/exec); out is what kubectl said.
func execForbiddenWhy(out string) string {
	return "your Kubernetes user is not allowed to run commands in the ClickHouse pod (create on pods/exec in namespace " + namespace + "): " + orDash(lastLines(out, 1))
}

// chWait says whether it is worth waiting longer for ClickHouse.
type chWait int

const (
	// chMaybe: it is not up, but may be soon.
	chMaybe chWait = iota
	// chStuck: a pod state that waiting does not mend (a pod that cannot be
	// scheduled, an image that cannot be pulled, a container that crashes).
	chStuck
	// chDenied: a permission is missing.
	chDenied
)

// stuckPodStates are the waiting reasons (and PodScheduled reasons) of a pod
// that a wait of seconds does not mend.
var stuckPodStates = []string{"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CreateContainerConfigError", "CrashLoopBackOff", "Unschedulable"}

// findKubeClickHouse finds the release's bundled ClickHouse by the chart's
// labels (componentSelector("clickhouse")): its StatefulSet, and a ready pod
// of it that is not going away. current asks for the pod of the
// StatefulSet's current spec, once its rollout is through (after a chart
// change, the pod still running may be the one that is about to be
// replaced); without it, a pod in a state that does not mend by waiting
// (stuckPodStates) is reported as stuck. why says what is missing, "" when it
// was found.
func findKubeClickHouse(current bool) (ch kubeClickHouse, why string, wait chWait) {
	objs, err := kubeList("statefulsets,pods", "-l", componentSelector("clickhouse"))
	if err != nil {
		if isForbidden(err.Error()) {
			return kubeClickHouse{}, "your Kubernetes user is not allowed to list StatefulSets and pods in namespace " + namespace + ": " + err.Error(), chDenied
		}
		return kubeClickHouse{}, err.Error(), chMaybe
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
		return kubeClickHouse{}, fmt.Sprintf("the release has %d ClickHouse StatefulSets (%s), not one", len(sts), componentSelector("clickhouse")), chMaybe
	}
	s := sts[0]
	ch = kubeClickHouse{image: clickhouseContainerImage(s.containers())}
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	if replicas == 0 {
		// Nothing is going to start: waiting does not mend a StatefulSet that
		// is scaled down.
		return ch, "the ClickHouse StatefulSet " + s.Metadata.Name + " is scaled to 0, so there is no ClickHouse to ask (kubectl scale statefulset " + s.Metadata.Name + " -n " + namespace + " --replicas=1)", chStuck
	}
	if current && (s.Status.ObservedGeneration < s.Metadata.Generation || s.Status.UpdatedReplicas < replicas) {
		return ch, "ClickHouse is still moving to its new spec (StatefulSet " + s.Metadata.Name + ")", chMaybe
	}
	slices.SortFunc(pods, func(a, b kubeObject) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	var states []string
	stuck := len(pods) > 0
	for _, p := range pods {
		if !p.ready() {
			state := p.stateWords()
			states = append(states, p.Metadata.Name+" is "+state)
			stuck = stuck && slices.ContainsFunc(stuckPodStates, func(w string) bool { return strings.Contains(state, w) })
			continue
		}
		if current && p.Metadata.Labels["controller-revision-hash"] != s.Status.UpdateRevision {
			states = append(states, p.Metadata.Name+" is still the previous revision")
			stuck = false
			continue
		}
		ch.pod = p.Metadata.Name
		return ch, "", chMaybe
	}
	if len(states) == 0 {
		states = []string{"the StatefulSet has no pod"}
	}
	why = "no ClickHouse pod is ready (" + strings.Join(states, "; ") + ")"
	if stuck && !current {
		return ch, why, chStuck
	}
	return ch, why, chMaybe
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

// waitKubeClickHouse waits, up to timeout, until the bundled ClickHouse is
// there (findKubeClickHouse, current as there) and answers a query. why says
// what it is not, "" once it is. A permission that is missing, or a pod that
// is stuck, ends the wait at once: it does not pass.
func waitKubeClickHouse(current bool, timeout time.Duration) (ch kubeClickHouse, why string, wait chWait) {
	deadline := time.Now().Add(timeout)
	for {
		ch, why, wait = findKubeClickHouse(current)
		if why == "" {
			out, err := ch.exec(clickhousePing)
			if err == nil {
				return ch, "", chMaybe
			}
			if isForbidden(out) {
				return ch, execForbiddenWhy(out), chDenied
			}
			why = "ClickHouse (" + ch.pod + ") does not answer: " + orDash(lastLines(out, 1))
		}
		if wait != chMaybe || time.Now().After(deadline) {
			return ch, why, wait
		}
		time.Sleep(kubePoll)
	}
}

// kubeAsk is the answer to asking the release's ClickHouse which events table
// it holds: live (gen stays "" when it could not be asked, with why in out;
// the caller refuses rather than guess from the chart versions), the bundled
// ClickHouse that answered, and whether the failure is a missing permission.
type kubeAsk struct {
	live     liveSchema
	ch       kubeClickHouse
	external bool
	denied   bool
}

// noJob reports that an external ClickHouse could not be asked because the
// release has no schema Job to make the probe Job from.
func (a kubeAsk) noJob() bool {
	return a.external && strings.Contains(a.live.out, "clickhouse.initSchema=false")
}

// hint says what to check when ClickHouse could not be asked.
func (a kubeAsk) hint() string {
	h := outputHint(a.live.out)
	switch {
	case a.noJob():
		return h + "The CLI asks an external ClickHouse through a Job copied from the chart's schema Job, and this release has none. Check " + eventsTable + " on your server yourself (its properties column must be JSON for release " + firstJSONChart + " on), or move the release with helm yourself."
	case a.denied && a.external:
		return h + "The CLI asks an external ClickHouse through a short-lived Job copied from the chart's schema Job, which needs create and delete on jobs and get on pods/log in namespace " + namespace + " (kubectl auth can-i create jobs -n " + namespace + ")."
	case a.denied:
		return h + "Only a move that can touch the events table asks ClickHouse, and that needs create on pods/exec in namespace " + namespace + " (kubectl auth can-i create pods/exec -n " + namespace + "). Grant it, or move the release with helm yourself."
	case a.external:
		return h + "Check the Job and its pod: kubectl get jobs,pods -n " + namespace + " -l " + componentSelector("clickhouse-probe")
	}
	return h + "Check: kubectl get pods -n " + namespace + " -l " + componentSelector("clickhouse")
}

// retry is the sentence that tells to rerun cmd once ClickHouse answers; none
// when a missing permission, not the state of ClickHouse, is the cause.
func (a kubeAsk) retry(cmd string) string {
	if a.denied || a.noJob() {
		return ""
	}
	return " Once it answers, rerun `" + cmd + "`."
}

// askKubeClickHouse asks the release's ClickHouse which events table it
// holds: the bundled one in its pod (waited for up to kubeAskTimeout while
// it is not ready), the operator's own (external) through a probe Job.
func askKubeClickHouse(external bool) kubeAsk {
	if external {
		sp := ui.Start("Asking your ClickHouse which events table it holds")
		live, denied := probeExternalClickHouse(kubeClickHouseTimeout)
		if live.gen == "" {
			sp.Fail("Your ClickHouse could not be asked")
			return kubeAsk{live: live, external: true, denied: denied}
		}
		sp.Success("Your ClickHouse holds %s", live.describe())
		return kubeAsk{live: live, external: true}
	}
	sp := ui.Start("Asking ClickHouse which events table it holds")
	ch, why, wait := waitKubeClickHouse(false, kubeAskTimeout)
	if why != "" {
		sp.Fail("ClickHouse could not be asked")
		return kubeAsk{live: liveSchema{out: why}, ch: ch, denied: wait == chDenied}
	}
	live := ch.probe()
	if live.gen == "" {
		sp.Fail("ClickHouse did not say which events table it holds")
		return kubeAsk{live: live, ch: ch, denied: isForbidden(live.out)}
	}
	sp.Success("ClickHouse holds %s", live.describe())
	return kubeAsk{live: live, ch: ch}
}

// clickhouseServerProblem says why the bundled ClickHouse ch cannot hold the
// new events table, "" when it can: its image's tag, or the server when the
// tag names no version (a mirror's image, latest), is minClickHouseForJSON or
// newer.
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
// from the release's schema Job (externalProbeJob), waited for up to wait and
// deleted again. gen is "" when it could not be asked, with why in out;
// denied says a missing permission is the cause.
func probeExternalClickHouse(wait time.Duration) (live liveSchema, denied bool) {
	name := fmt.Sprintf("%s-clickhouse-probe-%d", release, time.Now().Unix())
	manifest, why := externalProbeJob(name)
	if why != "" {
		return liveSchema{out: why}, isForbidden(why)
	}
	if out, err := kubeRunStdin(kubeRequestTimeout, string(manifest), "kubectl", "create", "-f", "-"); err != nil {
		if isForbidden(out) {
			return liveSchema{out: "your Kubernetes user is not allowed to create Jobs in namespace " + namespace + " (create on jobs): " + orDash(lastLines(out, 1))}, true
		}
		return liveSchema{out: "kubectl could not create the probe Job: " + orDash(lastLines(out, 1))}, false
	}
	defer func() {
		_, _ = kubeRunTimeout(kubeRequestTimeout, "kubectl", "delete", "job", name, "-n", namespace, "--ignore-not-found", "--wait=false")
	}()
	deadline := time.Now().Add(wait)
	for {
		objs, err := kubeList("jobs", "--field-selector", "metadata.name="+name)
		if err == nil && len(objs) == 1 && objs[0].finished() {
			break
		}
		if err != nil && isForbidden(err.Error()) {
			return liveSchema{out: "your Kubernetes user is not allowed to read Jobs in namespace " + namespace + " (get on jobs): " + err.Error()}, true
		}
		if time.Now().After(deadline) {
			return liveSchema{out: "the probe Job " + name + " did not finish within " + wait.String()}, false
		}
		time.Sleep(kubePoll)
	}
	out, err := kubeRunTimeout(kubeRequestTimeout, "kubectl", "logs", "-n", namespace, "job/"+name)
	if err != nil {
		return liveSchema{out: "kubectl could not read the probe Job's output (get on pods/log): " + orDash(lastLines(out, 1))}, isForbidden(out)
	}
	if objs, err := kubeList("jobs", "--field-selector", "metadata.name="+name); err != nil || len(objs) != 1 || !jobSucceeded(objs[0]) {
		return liveSchema{out: "the probe Job failed: " + orDash(lastLines(out, 2))}, false
	}
	return parseLiveSchema(out), false
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

// imageVerdict is what judging the images the release's objects run found.
type imageVerdict struct {
	// gen is the events table the services must write: the one asked for, or
	// (none asked for) the one their Deployments and StatefulSets agree on.
	gen string
	// wrong lists images that fail the check now; waiting, pods that run
	// images of the other generation while a rollout replaces them.
	wrong, waiting []string
	// services counts the Deployments and StatefulSets that pass; chImage is
	// the image of the release's ClickHouse StatefulSet.
	services int
	chImage  string
}

// unknownImageWhy says why an image is not judged to write one table.
func unknownImageWhy(image string) string {
	if imageTag(image) == "" && strings.Contains(image, "@") {
		return ", which is pinned by digest alone, so which events table it writes is not known (the digest is a <service>.image.digest in the release values: helm get values " + release + " -n " + namespace + ")"
	}
	return ", whose tag is not a numbered release, so which events table it writes is not known"
}

// judgeImages judges the data-plane containers of the release's objects
// (Deployments, StatefulSets and pods, the pods of Jobs aside) against want,
// the events table they must write ("" to ask only that their specs agree).
// A strict judgement fails an image that names no numbered release (a tag
// like main, a digest alone); a lenient one leaves those alone, since what
// they write cannot be told.
func judgeImages(objs []kubeObject, want string, strict bool) imageVerdict {
	type ref struct{ what, image, gen string }
	var specs, pods []ref
	var v imageVerdict
	for _, o := range objs {
		if o.Kind == "StatefulSet" && o.Metadata.Labels["app.kubernetes.io/component"] == "clickhouse" {
			v.chImage = clickhouseContainerImage(o.containers())
		}
		if o.Kind == "Pod" && (o.ownedByJob() || o.Status.Phase == "Succeeded" || o.Status.Phase == "Failed") {
			continue
		}
		for _, c := range o.containers() {
			if !isDataPlaneContainer(c) {
				continue
			}
			r := ref{what: strings.ToLower(o.Kind) + " " + o.Metadata.Name + " runs " + c.Image, image: c.Image, gen: imageGeneration(c.Image)}
			if o.Kind == "Pod" {
				pods = append(pods, r)
			} else {
				specs = append(specs, r)
			}
		}
	}
	v.gen = want
	if v.gen == "" {
		seen := map[string]bool{}
		for _, r := range specs {
			if r.gen != "unknown" {
				seen[r.gen] = true
			}
		}
		if len(seen) == 1 {
			for g := range seen {
				v.gen = g
			}
		} else if len(seen) > 1 {
			v.wrong = append(v.wrong, "the services run images that write different events tables (pre-reset and new)")
		}
	}
	for _, r := range specs {
		switch {
		case r.gen == "unknown":
			if strict {
				v.wrong = append(v.wrong, r.what+unknownImageWhy(r.image))
			}
		case v.gen != "" && r.gen != v.gen:
			v.wrong = append(v.wrong, r.what)
		default:
			v.services++
		}
	}
	for _, r := range pods {
		switch {
		case r.gen == "unknown":
			if strict {
				v.wrong = append(v.wrong, r.what+unknownImageWhy(r.image))
			}
		case v.gen != "" && r.gen != v.gen:
			v.waiting = append(v.waiting, r.what)
		}
	}
	return v
}

// releaseImagesProblem is the images half of the check after a chart change
// (kubeEventsCheck): every data-plane container of the release writes want
// (the events table ClickHouse holds), in the Deployments' and StatefulSets'
// specs (what runs once their rollouts are through) and in the pods that run
// now. While the new table is the one, the bundled ClickHouse runs
// minClickHouseForJSON or newer: as strict has it, by asking the server when
// its image's tag does not say (ch); otherwise by the tag alone. A pod of
// the old images is waited for, up to deadline, while a rollout replaces it;
// anything else fails at once. It returns why the release's images are not
// right, "" when they are; unread says the objects could not be listed.
func releaseImagesProblem(want string, strict, external bool, ch kubeClickHouse, deadline time.Time) (why string, unread bool) {
	for {
		objs, err := kubeList("deployments,statefulsets,pods", "-l", releaseSelector)
		if err != nil {
			return err.Error(), true
		}
		v := judgeImages(objs, want, strict)
		wrong := v.wrong
		if strict && v.services == 0 && len(wrong) == 0 {
			wrong = append(wrong, "no data-plane service is deployed")
		}
		if v.gen == "json" && !external {
			if strict {
				if why := clickhouseServerProblem(ch); why != "" {
					wrong = append(wrong, why+", and the new events table needs ClickHouse "+minClickHouseForJSON+" or newer")
				}
			} else if below, _ := clickhouseImageBelow(v.chImage); below {
				wrong = append(wrong, "ClickHouse runs "+v.chImage+", and the new events table needs ClickHouse "+minClickHouseForJSON+" or newer")
			}
		}
		if len(wrong) > 0 {
			return listSome(wrong), false
		}
		if len(v.waiting) == 0 {
			return "", false
		}
		if time.Now().After(deadline) {
			return "pods still run images that write the other events table after " + kubeCheckTimeout.String() + ": " + listSome(v.waiting), false
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
// have finished. It reports whether they did; Jobs it may not read are not
// waited for.
func waitSchemaJobs(deadline time.Time) bool {
	for {
		objs, err := kubeList("jobs", "-l", componentSelector("clickhouse-init"))
		if err == nil && !slices.ContainsFunc(objs, func(o kubeObject) bool { return !o.finished() }) {
			return true
		}
		if err != nil && isForbidden(err.Error()) {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(kubePoll)
	}
}

// ── The check after a chart change ───────────────────────────────────────────

// checkMode is how a check after a chart change reaches ClickHouse.
type checkMode int

const (
	// checkFull is the check of a move that could touch the events table:
	// ClickHouse is waited for and asked, and not being able to ask fails it.
	checkFull checkMode = iota
	// checkSoft asks ClickHouse once it is up but does not hold the run up for
	// it, and leaves a run unverified rather than failed when it cannot be
	// asked (a fresh install, which touches no existing table).
	checkSoft
	// checkImages never contacts ClickHouse: it reads the Kubernetes objects of
	// the release, as every other upgrade and rollback does.
	checkImages
)

// kubeCheck is what the check after a chart change found.
type kubeCheck struct {
	live liveSchema
	// why the release fails it, "" when it passes.
	why string
	// images: the failure is in what runs, not in the table.
	images bool
	// denied: why is a missing permission, not a state that passes.
	denied bool
	// unverified says why a check that did not fail could not be made.
	unverified string
}

// kubeEventsCheck is the check an upgrade, rollback or install ends with.
// In checkFull, once the schema Jobs are through (waited for up to
// kubeCheckTimeout), ClickHouse answers (the bundled one on its StatefulSet's
// current spec, an external one through a probe Job), its events table exists
// with payload columns of one generation, and every data-plane image the
// release runs, or will run once its rollouts are through, writes that
// generation (releaseImagesProblem). checkSoft is the same without waiting
// longer than kubeSoftTimeout for any of it, and an answer it cannot get is
// not a failure. checkImages is only the images half, against want ("" to ask
// only that they agree), and never contacts ClickHouse.
func kubeEventsCheck(mode checkMode, external bool, want string) kubeCheck {
	wait := kubeCheckTimeout
	if mode != checkFull {
		wait = kubeSoftTimeout
	}
	deadline := time.Now().Add(wait)
	if mode == checkImages {
		sp := ui.Start("Checking the images the release runs")
		why, unread := releaseImagesProblem(want, false, external, kubeClickHouse{}, deadline)
		switch {
		case unread:
			sp.Warn("Could not read the release's objects, so the images it runs were not checked")
			return kubeCheck{unverified: why}
		case why != "":
			sp.Fail("What runs does not write one events table")
			return kubeCheck{why: why, images: true}
		}
		sp.Success("Every data-plane service runs images that write the same events table")
		return kubeCheck{}
	}
	sp := ui.Start("Checking the events table the release's images write to")
	jobsDone := waitSchemaJobs(deadline)
	if !jobsDone {
		sp.Suffix("the schema Job is still at work")
	}
	var ch kubeClickHouse
	var live liveSchema
	var denied bool
	if external {
		live, denied = probeExternalClickHouse(wait)
	} else {
		var why string
		var w chWait
		if ch, why, w = waitKubeClickHouse(true, wait); why != "" {
			live, denied = liveSchema{out: why}, w == chDenied
		} else {
			live = ch.probe()
			denied = isForbidden(live.out)
		}
	}
	switch {
	case live.gen == "" && mode == checkSoft:
		sp.Warn("ClickHouse could not be asked")
		return kubeCheck{live: live, unverified: "ClickHouse could not be asked (" + orDash(lastLines(live.out, 1)) + ")"}
	case live.gen == liveMissing && mode == checkSoft && !jobsDone:
		// The chart's schema Job has not finished: the table is not due yet.
		sp.Warn("The schema Job is still at work")
		return kubeCheck{live: live, unverified: "ClickHouse has no events table yet, and the chart's schema Job is still at work"}
	case live.gen == "":
		sp.Fail("ClickHouse could not be asked")
		return kubeCheck{live: live, denied: denied, why: "ClickHouse could not be asked whether " + eventsTable + " is there (" + orDash(lastLines(live.out, 1)) + ")"}
	case !live.known():
		sp.Fail("ClickHouse holds %s", live.describe())
		return kubeCheck{live: live, why: "ClickHouse holds " + live.describe()}
	}
	why, unread := releaseImagesProblem(live.gen, mode == checkFull, external, ch, deadline)
	switch {
	case unread && mode == checkSoft:
		sp.Warn("ClickHouse holds %s; the images the release runs could not be read", live.describe())
		return kubeCheck{live: live, unverified: "ClickHouse holds " + live.describe() + ", but the release's objects could not be read, so the images it runs were not checked (" + why + ")"}
	case why != "":
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
	again := "`" + recheck + "` checks again."
	if recorded {
		again = "The unfinished work on the events table stays recorded (ConfigMap " + resetMarkerConfigMap + " in " + namespace + "): `" + recheck + "` finishes it and checks again."
	}
	pins := "Check what pins those images: a per-service image.tag in the release values (helm get values " + release + " -n " + namespace + ") stays until you replace it (`origamy upgrade --set <service>.image.tag=<release>`), an image.digest wins over any tag (clear it with `--set <service>.image.digest=`), and the rollouts (kubectl get pods -n " + namespace + ")."
	switch {
	case c.images && c.live.gen == "":
		return fail(headline, "Until every data-plane service writes the same events table, the events of the others are accepted and then dropped at insert.\n"+pins+"\nThen "+again)
	case c.live.gen == "" && c.denied:
		return fail(headline, "Only a check that must ask ClickHouse needs this permission: create on pods/exec in namespace "+namespace+" for the bundled ClickHouse (kubectl auth can-i create pods/exec -n "+namespace+"), create on jobs and get on pods/log for an external one. Grant it, then "+again)
	case c.live.gen == "" && strings.Contains(c.live.out, "clickhouse.initSchema=false"):
		return fail(headline, "The CLI asks an external ClickHouse through a Job copied from the chart's schema Job, and this release has none (clickhouse.initSchema=false). Check "+eventsTable+" on your server yourself: its properties column must be JSON for the new data-plane releases ("+firstJSONChart+" on).")
	case c.live.gen == "":
		return fail(headline, "Check: kubectl get pods -n "+namespace+" (and the schema Job: kubectl logs -n "+namespace+" -l "+componentSelector("clickhouse-init")+" --tail=20)\nOnce it answers, "+again)
	case c.live.gen == liveOther:
		return fail(headline, "No release writes it, and the storage reset does not drop it. Check what created it, and drop it yourself if it should be recreated with the new schema. Then "+again)
	case external && c.live.gen != "json":
		return fail(headline, "The CLI does not reset or create the events table on an external ClickHouse beyond the chart's schema Job. On your server, rename (or drop) "+eventsTable+" if it is the pre-reset one, and apply the chart's schema:\n  kubectl get configmap "+release+"-clickhouse-init -n "+namespace+" -o jsonpath='{.data.init\\.sql}' | clickhouse-client --host … --port … --user … --password … [--secure] --multiquery\nThen "+again)
	case c.live.gen == liveMissing:
		return fail(headline, "The chart's schema Job creates it: check its output (kubectl logs -n "+namespace+" -l "+componentSelector("clickhouse-init")+" --tail=20). "+again)
	}
	which := "Pre-reset releases (0.1.17 and earlier) cannot write it."
	if c.live.gen == "string" {
		which = "Releases from " + firstJSONChart + " on cannot write it; `origamy upgrade --version " + firstJSONChart + "` or newer moves it across the storage reset (asking before it drops anything)."
	}
	return fail(headline, "Until every data-plane service writes it, their events are accepted and then dropped at insert. "+which+"\n"+pins+"\nThen "+again)
}

// warnUnverified says that the check after a fresh install could not ask
// ClickHouse, and how to ask it by hand.
func warnUnverified(c kubeCheck) {
	ui.Warn("The events table was not verified: %s.", c.unverified)
	by := kubeClickHouse{pod: release + "-clickhouse-0"}.session().byHand(clickhouseEventsPropertiesType)
	ui.Detail("The install itself went through. Its payload column must be JSON: %s", by)
}

// ── The reset ────────────────────────────────────────────────────────────────

// resetKubernetesEvents recreates the release's events table with the
// chart's schema once the chart is on the target and the release writes the
// new table. The reset is already recorded (m, written before the chart
// moved: resetMarkerConfigMap). ClickHouse is waited for on its StatefulSet's
// current spec, which must run minClickHouseForJSON or newer.
//
// What ClickHouse holds then decides, held against what it held when the run
// made its decision (found): the table is moved aside only when ClickHouse
// says it is the pre-reset one (String payload columns) and the run found
// that; the chart's schema (from its ConfigMap, which must be the new one) is
// applied to that or to no table; the new table is left alone. Moved aside is
// RENAME TABLE events.events TO events.<m.aside>, never a DROP: the reset
// counts as done only once ClickHouse reports the new table, and only then is
// what was moved aside dropped, while it is the pre-reset one (settleAsideOf),
// and only the table the record names, and only with the operator's agreement
// (this run asked, or the record says the run that began the reset did).
func resetKubernetesEvents(m kubeMarker, found string) (resetResult, error) {
	none := resetResult{}
	rerun := "rerun `" + m.command() + "`: the unfinished reset is recorded in the cluster (ConfigMap " + resetMarkerConfigMap + " in " + namespace + ") and is finished before anything else."
	sp := ui.Start("Waiting for ClickHouse to come back on the new image")
	ch, why, wait := waitKubeClickHouse(true, kubeClickHouseTimeout)
	if why != "" {
		sp.Fail("ClickHouse is not ready after the upgrade")
		if wait == chDenied {
			return none, fail("The chart is on "+m.version+", but ClickHouse could not be asked, so the events table was not reset yet.",
				why+"\nGrant the permission, then "+rerun)
		}
		return none, fail("The chart is on "+m.version+", but ClickHouse is not ready, so the events table was not reset yet.",
			why+"\nCheck: kubectl get pods -n "+namespace+"\nOnce it answers, "+rerun)
	}
	if why := clickhouseServerProblem(ch); why != "" {
		sp.Fail("ClickHouse is too old for the new events table")
		return none, fail("The chart is on "+m.version+", but the new events table needs ClickHouse "+minClickHouseForJSON+" or newer and "+why+", so the events table was not touched.",
			"Set clickhouse.image to clickhouse/clickhouse-server:"+minClickHouseForJSON+" or newer (or take it out of your values), then "+rerun)
	}
	sp.Success("ClickHouse is up (%s)", ch.pod)

	sess := ch.session()
	may := m.mayDrop
	sp = ui.Start("Checking the events table")
	live := ch.probe()
	switch {
	case live.gen == "json":
		sp.Success("ClickHouse already holds the new events table (%s payload columns): nothing to move aside", live.typ)
		return settleAsideOf(sess, resetResult{outcome: resetAlready}, "", may), nil
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
			"Rerun `"+m.command()+"`: it asks again, before it drops anything.")
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
		aside = m.aside
		if aside == "" || asideTaken(sess.query, aside) {
			aside = asideName(sess.query)
		}
		sp.Suffix("the pre-reset one is moved aside first, as events.%s", aside)
		if out, err := sess.stmt(clickhouseMoveEventsAside(aside)); err != nil {
			// The statement can have taken effect although the client saw an
			// error (a timeout, a dropped connection): ask what ClickHouse
			// holds before saying anything about it.
			switch got := ch.probe(); got.gen {
			case liveMissing:
				ui.Warn("ClickHouse reported an error for the RENAME, but %s is gone: it was moved to events.%s. Continuing.", eventsTable, aside)
			case "string":
				sp.Fail("Could not move the old events table aside")
				return none, fail("The old events table could not be moved aside (RENAME to events."+aside+" failed), and ClickHouse still holds it as it was.", outputHint(out)+"Fix the cause, then "+rerun)
			default:
				sp.Fail("Could not move the old events table aside")
				return none, fail("The RENAME of the old events table to events."+aside+" reported an error, and ClickHouse could not say afterwards whether it took effect, so it is not known whether the table moved.",
					outputHint(out)+"Check events.events and events."+aside+": "+sess.byHand(clickhouseAsideTables)+"\nThen "+rerun)
			}
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
	return settleAsideOf(sess, resetResult{outcome: outcome}, aside, func(name string) bool { return name == aside || may(name) }), nil
}

// asideTaken reports whether a table named events.<name> is already one the
// reset moved aside.
func asideTaken(query func(script string) (string, error), name string) bool {
	out, err := query(clickhouseAsideTables)
	if err != nil {
		return false
	}
	_, ok := asideTables(out)[name]
	return ok
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

// kubeResetAction says what moving the release to images that write targetGen
// does with the events table ClickHouse holds (liveGen) and nothing else, as
// dockerResetAction does: a target that writes the new (JSON) table drops
// the pre-reset String one (drop, asked first) and creates a missing one
// (create, nothing to ask); the new one is never dropped, and a target that
// does not write JSON never resets.
func kubeResetAction(liveGen, targetGen string) (drop, create bool) {
	if targetGen != "json" {
		return false, false
	}
	return liveGen == "string", liveGen == liveMissing
}

// ── Before the chart changes ─────────────────────────────────────────────────

// kubeClickHouseImageProblem says why the ClickHouse image the values set
// (clickhouse.image, in vals as helm will have them) cannot hold the new
// events table, "" when it can or when the tag does not say (a mirror's
// image, latest: the reset asks the server, see clickhouseServerProblem).
func kubeClickHouseImageProblem(vals map[string]any) string {
	image := stringAt(vals, "clickhouse", "image")
	if below, v := clickhouseImageBelow(image); below {
		return "your values set the ClickHouse image to " + image + " (ClickHouse " + v + "), and the new events table needs ClickHouse " + minClickHouseForJSON + " or newer"
	}
	return ""
}

// kubeResetValuesProblem says why the reset cannot run with the values the
// release will have (vals, with the --set flags applied), "" when it can: the
// chart's schema Job (and the ConfigMap the reset applies) must be on, and a
// ClickHouse image set there must be minClickHouseForJSON or newer.
func kubeResetValuesProblem(vals map[string]any) string {
	if on, set := valueAt(vals, "clickhouse", "initSchema").(bool); set && !on {
		return "your values turn the chart's schema off (clickhouse.initSchema=false), so there is no schema for the reset to apply"
	}
	return kubeClickHouseImageProblem(vals)
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
// manifest --revision); rev 0 is the installed one.
func revisionManifest(rev int) (string, error) {
	args := []string{"get", "manifest", release, "-n", namespace}
	if rev > 0 {
		args = append(args, "--revision", strconv.Itoa(rev))
	}
	out, err := kubeRunTimeout(kubeRequestTimeout, "helm", args...)
	if err != nil {
		return "", fmt.Errorf("%s", orDash(lastLines(out, 1)))
	}
	return out, nil
}

// installedImageSet reads the data-plane images the installed revision of the
// release renders; none when they could not be read.
func installedImageSet() (set imageSet, none bool) {
	manifest, err := revisionManifest(0)
	if err != nil {
		return imageSet{}, true
	}
	return manifestImageSet(manifest)
}

// kubeDeployProblem is the check an `origamy deploy` makes, before the token
// is spent, over a release that is already installed: a deploy moves the
// chart to chartVer without the reset, so a reset an upgrade left
// unfinished is refused, and so is a ClickHouse holding an events table the
// images of chartVer cannot write, but the latter only when the deploy takes
// the images across the storage reset: any other deploy asks nothing of
// ClickHouse and takes no lease. One that does holds the lease on the events
// table (giveUp, which the caller calls once the deploy is over) and is
// asked under it. A release on an external ClickHouse is checked after the
// install.
func kubeDeployProblem(chartVer string) (headline, hint string, giveUp func()) {
	nothing := "Nothing has been enrolled or installed."
	vals, err := releaseValues()
	if err != nil {
		return "Could not read the values of the release already installed here.", err.Error() + "\n" + nothing, nil
	}
	// markerProblem refuses a deploy over an unfinished reset. unreadOK: a
	// user who may not read ConfigMaps is let through (with a warning), as a
	// deploy that cannot touch the events table needs nothing but helm.
	markerProblem := func(unreadOK bool) (string, string) {
		m, err := readResetMarker()
		var re *kubeReadError
		switch {
		case errors.As(err, &re) && re.forbidden && unreadOK:
			ui.Warn("Could not check whether an earlier upgrade left a storage reset unfinished (your user may not read ConfigMaps in %s).", namespace)
		case errors.As(err, &re) && re.forbidden:
			return "Could not read whether an earlier upgrade left a storage reset unfinished (your user may not read ConfigMaps in " + namespace + "), and this deploy can touch the events table.",
				"Grant get on configmaps in " + namespace + ", or move the release with helm yourself. " + nothing
		case err != nil:
			return "Could not read whether an earlier upgrade left a storage reset unfinished.", err.Error() + "\n" + nothing
		case m.present:
			return "An earlier upgrade began the storage reset here and did not finish it (ConfigMap " + resetMarkerConfigMap + " in " + namespace + ").",
				"Finish it first: " + m.command() + ". " + nothing
		}
		return "", ""
	}
	// A record that can be read is refused at once; one that cannot is only a
	// problem for a deploy that can touch the events table (below).
	if h, hh := markerProblem(true); h != "" {
		return h, hh, nil
	}
	installed, none := installedImageSet()
	// A deploy sets the release's values afresh: images the installed release
	// pins by digest, which name no release, are replaced by the chart's tag.
	crosses, _ := movesAcrossReset(installed, none, []string{chartVer}, len(installed.digests) > 0)
	if !crosses || usesExternalClickHouse(vals) {
		return "", "", nil
	}
	lease, err := acquireKubeLease("deploy")
	if err != nil {
		h, hh := leaseProblem(err, nothing)
		return h, hh, nil
	}
	refuse := func(h, hh string) (string, string, func()) {
		lease()
		return h, hh, nil
	}
	// What another run did before the lease was ours is read now.
	if h, hh := markerProblem(false); h != "" {
		return refuse(h, hh)
	}
	ask := askKubeClickHouse(false)
	live := ask.live
	want := releaseSchemaGeneration(chartVer)
	switch {
	case live.gen == "":
		return refuse("ClickHouse could not be asked which events table it holds, so this deploy cannot tell whether release "+chartVer+" can write it.",
			ask.hint()+"\n"+strings.TrimSpace(ask.retry("origamy deploy")+" "+nothing))
	case live.gen == liveOther:
		return refuse(fmt.Sprintf("ClickHouse's %s has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, which no release writes.", eventsTable, live.typ),
			"Check what created it, and drop it yourself if it should be recreated with the new schema; then retry. "+nothing)
	case live.gen == "string" && want == "json":
		return refuse("ClickHouse here holds the pre-reset events table (String payload columns), which release "+chartVer+" cannot write, and a deploy does not reset it.",
			"Move the installed release across the storage reset first: origamy upgrade --version "+chartVer+" (it asks before it discards the event history). "+nothing)
	case live.gen == "json" && want == "string":
		return refuse("ClickHouse here holds the new events table (JSON payload columns), which release "+chartVer+" cannot write: events would be accepted and then dropped at insert.",
			"Deploy "+firstJSONChart+" or newer (--version). "+nothing)
	}
	return "", "", lease
}
