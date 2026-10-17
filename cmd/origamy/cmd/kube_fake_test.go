package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeKube stands in for helm and kubectl in a Kubernetes upgrade, rollback
// or deploy, with a release and a bundled ClickHouse that keep state.
//
// The release has revisions (history); the newest is what is installed: its
// chart, the image tag of each data-plane service (tags), the services pinned
// by digest (digests), the ClickHouse image and whether it uses an external
// ClickHouse. `helm upgrade` adds a revision from its --version and --set
// flags (the ClickHouse image is the chart's: 25.3 from 0.1.18 on, 24.8
// before, unless clickhouse.image is set), `helm rollback` one copied from
// the revision it names. Both run the chart's schema Job, which creates the
// events table of the chart's generation when there is none (unless
// initJobOff), and leave the pods on the previous images for podLag pod
// listings (all of them when < 0).
//
// ClickHouse holds live ("" = no events table, else the payload column's
// type); RENAME moves it to aside, a DROP of a table moved aside removes it
// (recorded in dropped as name:type), and the schema applied on stdin
// creates the table of its generation when there is none. The reset record
// (resetMarkerConfigMap) is marker (nil = none), the lease lease (nil =
// none). Knobs make steps fail: chNotReady (no ready ClickHouse pod, in the
// state chWaiting), probeFails, renameFails (the RENAME fails;
// renameAppliesThenFails: it takes effect and the client sees an error),
// schemaFails (the schema apply fails, for its first schemaFails runs),
// helmFails (helm fails before applying; helmAppliesThenFails: after),
// markerFails (writing the record), markerReadForbidden, leaseCreateForbidden,
// execForbidden (kubectl exec is refused), listForbidden (the release's
// objects may not be listed), external (the release uses an external
// ClickHouse, which holds externalLive and is asked through a probe Job).
type fakeKube struct {
	t     *testing.T
	calls []string

	history []fakeRev
	latest  string
	values  map[string]any

	live    string
	aside   map[string]string
	dropped []string
	marker  map[string]string
	lease   map[string]string
	leaseRV int

	external     bool
	externalLive string
	probeJob     map[string]any

	chNotReady, probeFails, renameFails, renameAppliesThenFails bool
	helmFails, helmAppliesThenFails, markerFails, initJobOff    bool
	markerReadForbidden, leaseCreateForbidden, execForbidden    bool
	listForbidden, signingSecret, hostOnly, chartDigests        bool
	chScaledDown                                                bool
	schemaFails, podLag, initJobPolls                           int
	chWaiting, serverVersion                                    string
	prevTags                                                    map[string]string
	afterHelm                                                   func(*fakeKube)
}

// fakeRev is one revision of the fake release.
type fakeRev struct {
	rev      int
	chart    string
	tags     map[string]string
	digests  map[string]string
	chImage  string
	external bool
}

// fakeServices are the data-plane services of the fake release, by their
// values key.
var fakeServices = map[string]string{"bulkerWorker": "bulker-worker", "portalAgent": "portal-agent", "ingestGateway": "ingestion-gateway"}

const fakeRepo = "ghcr.io/qubelylabs/origamy-data-plane/"

const fakeDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// chartCH is the ClickHouse image a chart version runs by default.
func chartCH(version string) string {
	if versionLess(version, minImageForJSONSchema) {
		return "clickhouse/clickhouse-server:24.8"
	}
	return "clickhouse/clickhouse-server:25.3"
}

// chartType is the payload column type a chart version's schema creates.
func chartType(version string) string {
	if versionLess(version, minImageForJSONSchema) {
		return "String"
	}
	return "JSON"
}

// newFakeKube is a release installed on chart (revision 1, every service on
// that release) whose ClickHouse holds live.
func newFakeKube(t *testing.T, chart, live string) *fakeKube {
	t.Helper()
	f := &fakeKube{t: t, latest: chart, live: live, aside: map[string]string{}, values: map[string]any{}}
	f.history = []fakeRev{{rev: 1, chart: chart, tags: allTags(chart), chImage: chartCH(chart)}}
	prev := []time.Duration{kubeAskTimeout, kubeClickHouseTimeout, kubeCheckTimeout, kubeSoftTimeout, kubePoll}
	kubeAskTimeout, kubeClickHouseTimeout, kubeCheckTimeout, kubeSoftTimeout, kubePoll = 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, time.Millisecond
	prevRun, prevTimeout, prevStdin := kubeRun, kubeRunTimeout, kubeRunStdin
	kubeRun = func(name string, args ...string) (string, error) { return f.run("", name, args...) }
	kubeRunTimeout = func(_ time.Duration, name string, args ...string) (string, error) { return f.run("", name, args...) }
	kubeRunStdin = func(_ time.Duration, input, name string, args ...string) (string, error) {
		return f.run(input, name, args...)
	}
	t.Cleanup(func() {
		kubeRun, kubeRunTimeout, kubeRunStdin = prevRun, prevTimeout, prevStdin
		kubeAskTimeout, kubeClickHouseTimeout, kubeCheckTimeout, kubeSoftTimeout, kubePoll = prev[0], prev[1], prev[2], prev[3], prev[4]
	})
	// upgradeKubernetes looks helm up on PATH before anything else.
	bin := t.TempDir()
	name := "helm"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func allTags(tag string) map[string]string {
	tags := map[string]string{}
	for _, svc := range fakeServices {
		tags[svc] = tag
	}
	return tags
}

// cur is the installed revision.
func (f *fakeKube) cur() fakeRev { return f.history[len(f.history)-1] }

// withMarker records an unfinished reset to version that the operator agreed
// to (action reset), moving the pre-reset table to aside.
func (f *fakeKube) withMarker(version string, aside ...string) *fakeKube {
	f.marker = map[string]string{"version": version, "channel": "stable", "action": markerReset}
	if len(aside) > 0 {
		f.marker["aside"] = aside[0]
	}
	return f
}

// useExternal puts the release on an external ClickHouse holding live.
func (f *fakeKube) useExternal(live string) *fakeKube {
	f.external, f.externalLive = true, live
	f.values["clickhouse"] = map[string]any{"enabled": false, "host": "ch.example.com"}
	for i := range f.history {
		f.history[i].external = true
	}
	return f
}

// pinDigest pins the service with values key key by digest in the installed
// revision.
func (f *fakeKube) pinDigest(key string) *fakeKube {
	f.history[len(f.history)-1].digests = map[string]string{fakeServices[key]: fakeDigest}
	return f
}

// tagAll sets the image tag of every service of the first revision.
func (f *fakeKube) tagAll(tag string) *fakeKube {
	f.history[0].tags = allTags(tag)
	return f
}

func (f *fakeKube) fail(out string) (string, error) { return out, errors.New("exit status 1") }

func jsonOut(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var reFakeRename = regexp.MustCompile(`RENAME TABLE events\.events TO events\.(\w+)`)
var reFakeDropAside = regexp.MustCompile(`DROP TABLE events\.(` + eventsAsidePrefix + `\w+)`)

const (
	fakeExecForbidden = `Error from server (Forbidden): pods "odp-clickhouse-0" is forbidden: User "ops" cannot create resource "pods/exec" in API group "" in the namespace "origamy-dp"`
	fakeCMForbidden   = `Error from server (Forbidden): configmaps "origamy-reset-pending" is forbidden: User "ops" cannot get resource "configmaps" in API group "" in the namespace "origamy-dp"`
	fakeLeaseExists   = `Error from server (AlreadyExists): error when creating "STDIN": configmaps "origamy-reset-lock" already exists`
)

func (f *fakeKube) run(stdin, name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	switch name {
	case "helm":
		return f.helm(args)
	case "kubectl":
		return f.kubectl(stdin, args)
	}
	f.t.Errorf("unexpected command %s %v", name, args)
	return f.fail("unexpected")
}

func (f *fakeKube) helm(args []string) (string, error) {
	cur := f.cur()
	switch {
	case args[0] == "status":
		return "STATUS: deployed", nil
	case args[0] == "list":
		return jsonOut([]map[string]string{{"name": release, "revision": strconv.Itoa(cur.rev), "status": "deployed", "chart": "origamy-data-plane-" + cur.chart, "app_version": cur.chart}}), nil
	case args[0] == "show":
		return "apiVersion: v2\nname: origamy-data-plane\nversion: " + f.latest + "\n", nil
	case args[0] == "history":
		var h []map[string]any
		for _, r := range f.history {
			h = append(h, map[string]any{"revision": r.rev, "chart": "origamy-data-plane-" + r.chart, "status": "superseded"})
		}
		return jsonOut(h), nil
	case args[0] == "get" && args[1] == "values":
		rev := f.revisionArg(args)
		if slices.Contains(args, "--all") {
			all := map[string]any{}
			for key, svc := range fakeServices {
				all[key] = map[string]any{"image": map[string]any{"repository": fakeRepo + svc, "tag": rev.tags[svc]}}
			}
			return jsonOut(all), nil
		}
		vals := maps.Clone(f.values)
		if rev.external && f.hostOnly {
			vals["clickhouse"] = map[string]any{"host": "ch.example.com"}
		} else if rev.external {
			vals["clickhouse"] = map[string]any{"enabled": false, "host": "ch.example.com"}
		} else if ch, ok := vals["clickhouse"].(map[string]any); ok && ch["enabled"] == false {
			delete(vals, "clickhouse")
		}
		for key, svc := range fakeServices {
			if d := rev.digests[svc]; d != "" && !f.chartDigests {
				vals[key] = map[string]any{"image": map[string]any{"digest": d}}
			}
		}
		return jsonOut(vals), nil
	case args[0] == "get" && args[1] == "manifest":
		return f.manifest(f.revisionArg(args)), nil
	case args[0] == "upgrade":
		if f.helmFails {
			return f.fail("Error: UPGRADE FAILED: timed out")
		}
		next := fakeRev{rev: cur.rev + 1, tags: maps.Clone(cur.tags), digests: maps.Clone(cur.digests), external: cur.external}
		for i := 0; i < len(args)-1; i++ {
			switch args[i] {
			case "--version":
				next.chart = args[i+1]
			case "--set":
				for _, p := range parseHelmSets([]string{args[i+1]}) {
					k, v := strings.Join(p.path, "."), p.value
					if svc, ok := fakeServices[strings.TrimSuffix(k, ".image.tag")]; ok && strings.HasSuffix(k, ".image.tag") {
						next.tags[svc] = v
					}
					if svc, ok := fakeServices[strings.TrimSuffix(k, ".image.digest")]; ok && strings.HasSuffix(k, ".image.digest") {
						if v == "" {
							delete(next.digests, svc)
						} else {
							next.digests[svc] = v
						}
					}
					if k == "global.imageTag" {
						next.tags = allTags(v)
					}
					if k == "clickhouse.image" {
						f.values["clickhouse"] = map[string]any{"image": v}
					}
					if k == "clickhouse.host" {
						f.values["clickhouse"] = map[string]any{"host": v}
					}
				}
			}
		}
		next.chImage = chartCH(next.chart)
		if ch, ok := f.values["clickhouse"].(map[string]any); ok && ch["image"] != nil {
			next.chImage = ch["image"].(string)
		}
		f.moveTo(next)
		if f.helmAppliesThenFails {
			return f.fail("Error: UPGRADE FAILED: context deadline exceeded")
		}
		return "Release \"odp\" has been upgraded.", nil
	case args[0] == "rollback":
		to := cur.rev - 1
		if n, err := strconv.Atoi(args[2]); err == nil {
			to = n
		}
		for _, r := range f.history {
			if r.rev == to {
				r.rev = cur.rev + 1
				f.moveTo(r)
				return "Rollback was a success! Happy Helming!", nil
			}
		}
		return f.fail("Error: release: not found")
	}
	f.t.Errorf("unexpected helm %v", args)
	return f.fail("unexpected")
}

// revisionArg is the revision a helm get names (--revision), else the newest.
func (f *fakeKube) revisionArg(args []string) fakeRev {
	if i := slices.Index(args, "--revision"); i >= 0 {
		n, _ := strconv.Atoi(args[i+1])
		for _, r := range f.history {
			if r.rev == n {
				return r
			}
		}
	}
	return f.cur()
}

// moveTo installs revision r and runs its schema Job.
func (f *fakeKube) moveTo(r fakeRev) {
	f.prevTags = f.cur().tags
	f.history = append(f.history, r)
	if f.live == "" && !f.initJobOff && !r.external {
		f.live = chartType(r.chart)
	}
	if f.afterHelm != nil {
		f.afterHelm(f)
	}
}

// imageOf is the image reference of a service in revision r: by digest when
// the revision pins one, else by tag.
func imageOf(r fakeRev, svc string) string {
	if d := r.digests[svc]; d != "" {
		return fakeRepo + svc + "@" + d
	}
	return fakeRepo + svc + ":" + r.tags[svc]
}

// manifest renders revision r: its services' and ClickHouse's images and the
// chart's schema Job.
func (f *fakeKube) manifest(r fakeRev) string {
	var b strings.Builder
	for _, svc := range slices.Sorted(maps.Keys(r.tags)) {
		fmt.Fprintf(&b, "---\n# Source: origamy-data-plane/templates/%s-deployment.yaml\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - name: %s\n          image: %s\n          imagePullPolicy: IfNotPresent\n", svc, svc, imageOf(r, svc))
	}
	fmt.Fprintf(&b, "---\nkind: StatefulSet\nspec:\n  template:\n    spec:\n      containers:\n        - name: clickhouse\n          image: %s\n", r.chImage)
	fmt.Fprintf(&b, "---\n# Source: origamy-data-plane/templates/nats-init-job.yaml\napiVersion: batch/v1\nkind: Job\nmetadata:\n  name: odp-nats-init-%d\n  labels:\n    app.kubernetes.io/component: nats-init\n", r.rev)
	if ch, _ := f.values["clickhouse"].(map[string]any); ch["initSchema"] == false {
		return b.String()
	}
	fmt.Fprintf(&b, "---\n# Source: origamy-data-plane/templates/clickhouse-init-job.yaml\napiVersion: batch/v1\nkind: Job\nmetadata:\n  name: odp-clickhouse-init-%d\n  labels:\n    app.kubernetes.io/component: clickhouse-init\nspec:\n  template:\n    spec:\n      containers:\n        - name: clickhouse-init\n          image: \"clickhouse/clickhouse-server:25.3\"\n", r.rev)
	return b.String()
}

// tablesRows is what clickhouseAsideTables prints.
func (f *fakeKube) asideRows() string {
	var rows []string
	for _, n := range slices.Sorted(maps.Keys(f.aside)) {
		rows = append(rows, n+"\t"+f.aside[n]+"\t"+eventsAsideMark)
	}
	return strings.Join(rows, "\n")
}

// cmData reads the data of a ConfigMap rendered by configMapManifest(At).
func cmData(manifest string) map[string]string {
	data := map[string]string{}
	for _, l := range strings.Split(manifest, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), ": \""); ok && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && k != "resourceVersion" {
			data[k] = strings.TrimSuffix(v, "\"")
		}
	}
	return data
}

func (f *fakeKube) kubectl(stdin string, args []string) (string, error) {
	cur := f.cur()
	last := args[len(args)-1]
	switch {
	case args[0] == "get" && args[1] == "configmap" && args[2] == resetMarkerConfigMap:
		if f.markerReadForbidden {
			return f.fail(fakeCMForbidden)
		}
		if f.marker == nil {
			return "", nil
		}
		return jsonOut(map[string]any{"kind": "ConfigMap", "data": f.marker}), nil
	case args[0] == "get" && args[1] == "configmap" && args[2] == kubeLeaseConfigMap:
		if f.lease == nil {
			return "", nil
		}
		return jsonOut(map[string]any{"kind": "ConfigMap", "metadata": map[string]any{"resourceVersion": strconv.Itoa(f.leaseRV)}, "data": f.lease}), nil
	case args[0] == "create" && strings.Contains(stdin, "name: "+kubeLeaseConfigMap):
		if f.leaseCreateForbidden {
			return f.fail(`Error from server (Forbidden): configmaps is forbidden: User "ops" cannot create resource "configmaps"`)
		}
		if f.lease != nil {
			return f.fail(fakeLeaseExists)
		}
		f.lease, f.leaseRV = cmData(stdin), f.leaseRV+1
		return "configmap/" + kubeLeaseConfigMap + " created", nil
	case args[0] == "replace" && strings.Contains(stdin, "name: "+kubeLeaseConfigMap):
		if f.lease == nil {
			return f.fail("Error from server (NotFound): configmaps \"" + kubeLeaseConfigMap + "\" not found")
		}
		if !strings.Contains(stdin, "resourceVersion: \""+strconv.Itoa(f.leaseRV)+"\"") {
			return f.fail("Error from server (Conflict): Operation cannot be fulfilled on configmaps \"" + kubeLeaseConfigMap + "\": the object has been modified")
		}
		f.lease, f.leaseRV = cmData(stdin), f.leaseRV+1
		return "configmap/" + kubeLeaseConfigMap + " replaced", nil
	case args[0] == "delete" && args[1] == "configmap" && args[2] == kubeLeaseConfigMap:
		f.lease = nil
		return "deleted", nil
	case args[0] == "apply":
		if strings.Contains(stdin, "name: "+resetMarkerConfigMap) {
			if f.markerFails {
				return f.fail("error: configmaps is forbidden")
			}
			f.marker = cmData(stdin)
		}
		return "configured", nil
	case args[0] == "delete" && args[1] == "configmap":
		f.marker = nil
		return "deleted", nil
	case args[0] == "delete" && args[1] == "secret":
		return "deleted", nil
	case args[0] == "get" && args[1] == "secret":
		if f.signingSecret {
			return "origamy-config-signing", nil
		}
		return f.fail("Error from server (NotFound)")
	case args[0] == "get" && args[1] == "statefulsets,pods":
		if f.listForbidden {
			return f.fail(`Error from server (Forbidden): statefulsets.apps is forbidden: User "ops" cannot list resource "statefulsets" in API group "apps" in the namespace "origamy-dp"`)
		}
		return jsonOut(map[string]any{"items": f.clickhouseObjects()}), nil
	case args[0] == "get" && args[1] == "configmaps":
		items := []any{map[string]any{"kind": "ConfigMap", "data": map[string]string{"users.xml": "<clickhouse/>"}}}
		if ch, _ := f.values["clickhouse"].(map[string]any); ch["initSchema"] != false {
			sql := stringSchemaSQL
			if chartType(cur.chart) == "JSON" {
				sql = jsonSchemaSQL
			}
			items = append(items, map[string]any{"kind": "ConfigMap", "data": map[string]string{"init.sql": sql}})
		}
		return jsonOut(map[string]any{"items": items}), nil
	case args[0] == "get" && args[1] == "deployments,statefulsets,pods":
		if f.listForbidden {
			return f.fail(`Error from server (Forbidden): deployments.apps is forbidden: User "ops" cannot list resource "deployments" in API group "apps" in the namespace "origamy-dp"`)
		}
		return jsonOut(map[string]any{"items": f.releaseObjects()}), nil
	case args[0] == "get" && args[1] == "jobs" && slices.Contains(args, "--field-selector"):
		if f.probeJob == nil {
			return jsonOut(map[string]any{"items": []any{}}), nil
		}
		return jsonOut(map[string]any{"items": []any{map[string]any{"kind": "Job", "status": map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}}}}), nil
	case args[0] == "get" && args[1] == "jobs":
		cond := "Complete"
		if f.initJobPolls > 0 {
			f.initJobPolls--
			cond = "Progressing"
		}
		return jsonOut(map[string]any{"items": []any{map[string]any{"kind": "Job", "status": map[string]any{"conditions": []any{map[string]any{"type": cond, "status": "True"}}}}}}), nil
	case args[0] == "get" && args[1] == "pods":
		return jsonOut(map[string]any{"items": []any{map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "odp-bulker-worker-7d9f", "labels": map[string]any{"app.kubernetes.io/component": "bulker-worker"}},
			"status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"ready": true}}}}}}), nil
	case args[0] == "rollout":
		return "restarted", nil
	case args[0] == "create" && slices.Contains(args, "--dry-run=client") && !strings.Contains(stdin, "component: clickhouse-init"):
		return jsonOut(map[string]any{"apiVersion": "batch/v1", "kind": "Job", "spec": map[string]any{"template": map[string]any{
			"spec": map[string]any{"containers": []any{map[string]any{"name": "nats-init", "image": "natsio/nats-box"}}}}}}), nil
	case args[0] == "create" && slices.Contains(args, "--dry-run=client"):
		return jsonOut(map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": "odp-clickhouse-init-" + strconv.Itoa(cur.rev)},
			"spec": map[string]any{"backoffLimit": 12, "template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "odp", "app.kubernetes.io/instance": "odp", "app.kubernetes.io/component": "clickhouse-init"}},
				"spec": map[string]any{"restartPolicy": "OnFailure", "containers": []any{map[string]any{
					"name": "clickhouse-init", "image": "clickhouse/clickhouse-server:25.3", "command": []any{"/bin/sh", "-c", "apply"},
					"env": []any{map[string]any{"name": "CH_HOST", "value": "ch.example.com"}},
				}}}}}}), nil
	case args[0] == "create":
		f.probeJob = map[string]any{}
		if err := json.Unmarshal([]byte(stdin), &f.probeJob); err != nil {
			f.t.Errorf("probe Job is not JSON: %v", err)
		}
		return "job.batch/created", nil
	case args[0] == "logs":
		if f.externalLive == "" {
			return "", nil
		}
		return f.externalLive + "\t" + eventsColumnMark + "\n", nil
	case args[0] == "delete" && args[1] == "job":
		return "deleted", nil
	case args[0] == "exec":
		return f.exec(stdin, last)
	}
	f.t.Errorf("unexpected kubectl %v", args)
	return f.fail("unexpected")
}

// exec is a statement or query run in the ClickHouse pod.
func (f *fakeKube) exec(stdin, script string) (string, error) {
	if f.execForbidden {
		return f.fail(fakeExecForbidden)
	}
	switch {
	case script == clickhouseInitStdin:
		if f.schemaFails > 0 {
			f.schemaFails--
			return f.fail("Code: 44. DB::Exception: Cannot create table events.events")
		}
		if f.live == "" {
			f.live = map[string]string{"json": "JSON", "string": "String"}[schemaGeneration(stdin)]
		}
		return "", nil
	case script == clickhouseEventsPropertiesType:
		if f.probeFails {
			return f.fail("no answer within 20s")
		}
		if f.live == "" {
			return "", nil
		}
		return f.live + "\t" + eventsColumnMark + "\n", nil
	case script == clickhousePing:
		return "1", nil
	case script == clickhouseServerVersion:
		if f.serverVersion != "" {
			return f.serverVersion, nil
		}
		if v, ok := clickHouseImageVersion(f.cur().chImage); ok {
			return v + ".14.14", nil
		}
		return "25.3.14.14", nil
	case script == clickhouseAsideTables:
		return f.asideRows(), nil
	case reFakeRename.MatchString(script):
		if f.renameFails || f.live == "" {
			return f.fail("Code: 60. DB::Exception: Table `events`.`events` doesn't exist. (UNKNOWN_TABLE)")
		}
		f.aside[reFakeRename.FindStringSubmatch(script)[1]], f.live = f.live, ""
		if f.renameAppliesThenFails {
			return f.fail("Error from server: unexpected EOF")
		}
		return "", nil
	case reFakeDropAside.MatchString(script):
		n := reFakeDropAside.FindStringSubmatch(script)[1]
		if _, ok := f.aside[n]; !ok {
			return f.fail("Code: 60. DB::Exception: Table events." + n + " doesn't exist.")
		}
		f.dropped = append(f.dropped, n+":"+f.aside[n])
		delete(f.aside, n)
		return "", nil
	}
	f.t.Errorf("unexpected exec %q", script)
	return f.fail("unexpected")
}

// clickhouseObjects is the bundled ClickHouse's StatefulSet and pod.
func (f *fakeKube) clickhouseObjects() []any {
	if f.cur().external {
		return []any{}
	}
	phase := "Running"
	status := map[string]any{"phase": phase, "containerStatuses": []any{map[string]any{"ready": true}}}
	if f.chNotReady {
		cs := map[string]any{"ready": false}
		if f.chWaiting != "" {
			cs["state"] = map[string]any{"waiting": map[string]any{"reason": f.chWaiting}}
		}
		status = map[string]any{"phase": "Pending", "containerStatuses": []any{cs}}
	}
	if f.chScaledDown {
		return []any{map[string]any{"kind": "StatefulSet", "metadata": map[string]any{"name": "odp-clickhouse", "generation": 2},
			"spec":   map[string]any{"replicas": 0, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "clickhouse", "image": f.cur().chImage}}}}},
			"status": map[string]any{"observedGeneration": 2, "updatedReplicas": 0, "updateRevision": "odp-clickhouse-2"}}}
	}
	return []any{
		map[string]any{"kind": "StatefulSet", "metadata": map[string]any{"name": "odp-clickhouse", "generation": 2},
			"spec":   map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "clickhouse", "image": f.cur().chImage}}}}},
			"status": map[string]any{"observedGeneration": 2, "updatedReplicas": 1, "updateRevision": "odp-clickhouse-2"}},
		map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "odp-clickhouse-0", "labels": map[string]any{"controller-revision-hash": "odp-clickhouse-2"}},
			"status": status},
	}
}

// releaseObjects is every Deployment, StatefulSet and pod of the release:
// the services' (pods on the previous images while podLag lasts), the
// ClickHouse StatefulSet, an alloy Deployment and a schema Job pod.
func (f *fakeKube) releaseObjects() []any {
	cur := f.cur()
	podTags := cur.tags
	if f.podLag != 0 && f.prevTags != nil {
		podTags = f.prevTags
		if f.podLag > 0 {
			f.podLag--
		}
	}
	var items []any
	for _, svc := range slices.Sorted(maps.Keys(cur.tags)) {
		podImage := fakeRepo + svc + ":" + podTags[svc]
		if cur.digests[svc] != "" {
			podImage = imageOf(cur, svc)
		}
		items = append(items,
			map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "odp-" + svc},
				"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": svc, "image": imageOf(cur, svc)}}}}}},
			map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "odp-" + svc + "-7d9f", "ownerReferences": []any{map[string]any{"kind": "ReplicaSet"}}},
				"spec": map[string]any{"containers": []any{map[string]any{"name": svc, "image": podImage}}}, "status": map[string]any{"phase": "Running"}})
	}
	items = append(items,
		map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "odp-alloy-logs"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "alloy", "image": "grafana/alloy:v1.20.1"}}}}}},
		map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "odp-clickhouse-init-1-x", "ownerReferences": []any{map[string]any{"kind": "Job"}}},
			"spec": map[string]any{"containers": []any{map[string]any{"name": "clickhouse-init", "image": "clickhouse/clickhouse-server:25.3"}}}, "status": map[string]any{"phase": "Running"}})
	if !cur.external {
		sts := f.clickhouseObjects()[0].(map[string]any)
		sts["metadata"].(map[string]any)["labels"] = map[string]any{"app.kubernetes.io/component": "clickhouse"}
		items = append(items, sts)
	}
	return items
}

// called reports whether a call starts with prefix; at is its index (-1).
func (f *fakeKube) at(prefix string) int {
	return slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// count is how many calls start with prefix.
func (f *fakeKube) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// asideCall is the index of the RENAME into an aside table (-1: none).
func (f *fakeKube) asideCall() int {
	return slices.IndexFunc(f.calls, func(c string) bool { return reFakeRename.MatchString(c) })
}

// untouched reports whether the run never contacted ClickHouse (no exec, no
// look at its pod or StatefulSet, no probe Job) and took neither the lease
// nor recorded a reset: how an upgrade, rollback or deploy that cannot touch
// the events table behaves.
func (f *fakeKube) untouched() bool {
	return f.count("kubectl exec") == 0 && f.count("kubectl get statefulsets,pods") == 0 && f.count("kubectl create") == 0 &&
		f.at(markerWrite) < 0 && f.lease == nil && f.leaseRV == 0 && f.probeJob == nil
}

// upgradeK8s runs `origamy upgrade --version version --yes` on Kubernetes,
// with --set flags sets.
func upgradeK8s(t *testing.T, version string, sets ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		err = upgradeKubernetes(version, "stable", sets, false, false, false, false, false, true)
	})
	return out, err
}

const (
	helmUpgrade  = "helm upgrade " + release
	markerWrite  = "kubectl apply -f -"
	markerDelete = "kubectl delete configmap " + resetMarkerConfigMap
	discarded    = "Event history was discarded"
	untouched    = "Your data (ClickHouse/Postgres/NATS/Redis) was not touched."
)
