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
// chart, the image tag of each data-plane service (tags), the ClickHouse
// image and whether it uses an external ClickHouse. `helm upgrade` adds a
// revision from its --version and --set flags (the ClickHouse image is the
// chart's: 25.3 from 0.1.18 on, 24.8 before, unless clickhouse.image is
// set), `helm rollback` one copied from the revision it names. Both run the
// chart's schema Job, which creates the events table of the chart's
// generation when there is none (unless initJobOff), and leave the pods on
// the previous images for podLag pod listings (all of them when < 0).
//
// ClickHouse holds live ("" = no events table, else the payload column's
// type); RENAME moves it to aside, a DROP of a table moved aside removes it
// (recorded in dropped as name:type), and the schema applied on stdin
// creates the table of its generation when there is none. The reset record
// (resetMarkerConfigMap) is marker (nil = none). Knobs make steps fail:
// chNotReady (no ready ClickHouse pod), probeFails, renameFails, schemaFails
// (the schema apply fails, for its first schemaFails runs), helmFails,
// markerFails (writing the record), external (the release uses an external
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

	external     bool
	externalLive string
	probeJob     map[string]any

	chNotReady, probeFails, renameFails bool
	helmFails, markerFails, initJobOff  bool
	signingSecret                       bool
	schemaFails, podLag, initJobPolls   int
	prevTags                            map[string]string
	afterHelm                           func(*fakeKube)
}

// fakeRev is one revision of the fake release.
type fakeRev struct {
	rev      int
	chart    string
	tags     map[string]string
	chImage  string
	external bool
}

// fakeServices are the data-plane services of the fake release, by their
// values key.
var fakeServices = map[string]string{"bulkerWorker": "bulker-worker", "portalAgent": "portal-agent", "ingestGateway": "ingestion-gateway"}

const fakeRepo = "ghcr.io/qubelylabs/origamy-data-plane/"

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
	prev := []any{kubeClickHouseTimeout, kubeCheckTimeout, kubePoll}
	kubeClickHouseTimeout, kubeCheckTimeout, kubePoll = 50*time.Millisecond, 50*time.Millisecond, time.Millisecond
	prevRun, prevTimeout, prevStdin := kubeRun, kubeRunTimeout, kubeRunStdin
	kubeRun = func(name string, args ...string) (string, error) { return f.run("", name, args...) }
	kubeRunTimeout = func(_ time.Duration, name string, args ...string) (string, error) { return f.run("", name, args...) }
	kubeRunStdin = func(_ time.Duration, input, name string, args ...string) (string, error) {
		return f.run(input, name, args...)
	}
	t.Cleanup(func() {
		kubeRun, kubeRunTimeout, kubeRunStdin = prevRun, prevTimeout, prevStdin
		kubeClickHouseTimeout, kubeCheckTimeout, kubePoll = prev[0].(time.Duration), prev[1].(time.Duration), prev[2].(time.Duration)
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

// withMarker records an unfinished reset to version.
func (f *fakeKube) withMarker(version string) *fakeKube {
	f.marker = map[string]string{"version": version, "channel": "stable"}
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

func (f *fakeKube) fail(out string) (string, error) { return out, errors.New("exit status 1") }

func jsonOut(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var reFakeRename = regexp.MustCompile(`RENAME TABLE events\.events TO events\.(\w+)`)
var reFakeDropAside = regexp.MustCompile(`DROP TABLE events\.(` + eventsAsidePrefix + `\w+)`)

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
		if rev.external {
			vals["clickhouse"] = map[string]any{"enabled": false, "host": "ch.example.com"}
		} else if ch, ok := vals["clickhouse"].(map[string]any); ok && ch["enabled"] == false {
			delete(vals, "clickhouse")
		}
		return jsonOut(vals), nil
	case args[0] == "get" && args[1] == "manifest":
		return f.manifest(f.revisionArg(args)), nil
	case args[0] == "upgrade":
		if f.helmFails {
			return f.fail("Error: UPGRADE FAILED: timed out")
		}
		next := fakeRev{rev: cur.rev + 1, tags: maps.Clone(cur.tags), external: cur.external}
		for i := 0; i < len(args)-1; i++ {
			switch args[i] {
			case "--version":
				next.chart = args[i+1]
			case "--set":
				k, v, _ := strings.Cut(args[i+1], "=")
				if svc, ok := fakeServices[strings.TrimSuffix(k, ".image.tag")]; ok {
					next.tags[svc] = v
				}
				if k == "global.imageTag" {
					next.tags = allTags(v)
				}
				if k == "clickhouse.image" {
					f.values["clickhouse"] = map[string]any{"image": v}
				}
			}
		}
		next.chImage = chartCH(next.chart)
		if ch, ok := f.values["clickhouse"].(map[string]any); ok && ch["image"] != nil {
			next.chImage = ch["image"].(string)
		}
		f.moveTo(next)
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

// manifest renders revision r: its services' and ClickHouse's images and the
// chart's schema Job.
func (f *fakeKube) manifest(r fakeRev) string {
	var b strings.Builder
	for _, svc := range slices.Sorted(maps.Keys(r.tags)) {
		fmt.Fprintf(&b, "---\n# Source: origamy-data-plane/templates/%s-deployment.yaml\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - name: %s\n          image: %s%s:%s\n          imagePullPolicy: IfNotPresent\n", svc, svc, fakeRepo, svc, r.tags[svc])
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

func (f *fakeKube) kubectl(stdin string, args []string) (string, error) {
	cur := f.cur()
	last := args[len(args)-1]
	switch {
	case args[0] == "get" && args[1] == "configmap" && args[2] == resetMarkerConfigMap:
		if f.marker == nil {
			return "", nil
		}
		return jsonOut(map[string]any{"kind": "ConfigMap", "data": f.marker}), nil
	case args[0] == "apply":
		if strings.Contains(stdin, "name: "+resetMarkerConfigMap) {
			if f.markerFails {
				return f.fail("error: configmaps is forbidden")
			}
			f.marker = map[string]string{}
			for _, l := range strings.Split(stdin, "\n") {
				if k, v, ok := strings.Cut(strings.TrimSpace(l), ": \""); ok && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") {
					f.marker[k] = strings.TrimSuffix(v, "\"")
				}
			}
		}
		return "configured", nil
	case args[0] == "delete" && args[1] == "configmap":
		f.marker = nil
		return "deleted", nil
	case args[0] == "get" && args[1] == "secret":
		if f.signingSecret {
			return "origamy-config-signing", nil
		}
		return f.fail("Error from server (NotFound)")
	case args[0] == "get" && args[1] == "statefulsets,pods":
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
	if f.chNotReady {
		phase = "Pending"
	}
	return []any{
		map[string]any{"kind": "StatefulSet", "metadata": map[string]any{"name": "odp-clickhouse", "generation": 2},
			"spec":   map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "clickhouse", "image": f.cur().chImage}}}}},
			"status": map[string]any{"observedGeneration": 2, "updatedReplicas": 1, "updateRevision": "odp-clickhouse-2"}},
		map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "odp-clickhouse-0", "labels": map[string]any{"controller-revision-hash": "odp-clickhouse-2"}},
			"status": map[string]any{"phase": phase, "containerStatuses": []any{map[string]any{"ready": phase == "Running"}}}},
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
		items = append(items,
			map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "odp-" + svc},
				"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": svc, "image": fakeRepo + svc + ":" + cur.tags[svc]}}}}}},
			map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "odp-" + svc + "-7d9f", "ownerReferences": []any{map[string]any{"kind": "ReplicaSet"}}},
				"spec": map[string]any{"containers": []any{map[string]any{"name": svc, "image": fakeRepo + svc + ":" + podTags[svc]}}}, "status": map[string]any{"phase": "Running"}})
	}
	items = append(items,
		map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "odp-alloy-logs"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "alloy", "image": "grafana/alloy:v1.20.1"}}}}}},
		map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "odp-clickhouse-init-1-x", "ownerReferences": []any{map[string]any{"kind": "Job"}}},
			"spec": map[string]any{"containers": []any{map[string]any{"name": "clickhouse-init", "image": "clickhouse/clickhouse-server:25.3"}}}, "status": map[string]any{"phase": "Running"}})
	if !cur.external {
		items = append(items, f.clickhouseObjects()[0])
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

func TestUpgradeKubernetesResetsAStringTable(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "String")
	out, err := upgradeK8s(t, "0.1.20")
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, out)
	}
	if f.live != "JSON" || len(f.aside) != 0 || len(f.dropped) != 1 || !strings.HasSuffix(f.dropped[0], ":String") {
		t.Fatalf("live=%q aside=%v dropped=%v, want the new table and the String one moved aside and dropped", f.live, f.aside, f.dropped)
	}
	// Recorded after the chart moved and before the table was renamed;
	// cleared only after the check that ends the run.
	up, rec, mv, del := f.at(helmUpgrade), f.at(markerWrite), f.asideCall(), f.at(markerDelete)
	check := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "kubectl get deployments,statefulsets,pods") })
	if up < 0 || !(up < rec && rec < mv && mv < check && check < del) {
		t.Fatalf("want helm upgrade < record < RENAME < check < record deleted, got %d %d %d %d %d:\n%s", up, rec, mv, check, del, strings.Join(f.calls, "\n"))
	}
	if f.marker != nil {
		t.Fatalf("the reset record was left behind: %v", f.marker)
	}
	if !strings.Contains(out, discarded) || strings.Contains(out, untouched) {
		t.Fatalf("summary does not say the event history was discarded:\n%s", out)
	}
	if f.cur().chart != "0.1.20" || f.cur().tags["bulker-worker"] != "0.1.20" {
		t.Fatalf("release not moved: %+v", f.cur())
	}
}

func TestUpgradeKubernetesAsksBeforeTheReset(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "String")
	withAnswers(t, false, "no\n")
	var err error
	out := captureStdout(t, func() {
		err = upgradeKubernetes("0.1.20", "stable", nil, false, false, false, false, false, false)
	})
	if err == nil || !strings.Contains(out, "Cancelled.") || !strings.Contains(out, "Type 'yes' to continue") {
		t.Fatalf("want the reset asked about and cancelled, got err=%v:\n%s", err, out)
	}
	if f.at(helmUpgrade) >= 0 || f.marker != nil || f.live != "String" {
		t.Fatalf("a cancelled reset changed something: calls=%v marker=%v live=%q", f.calls, f.marker, f.live)
	}
}

func TestUpgradeKubernetesCreatesAMissingTable(t *testing.T) {
	f := newFakeKube(t, "0.1.20", "")
	f.initJobOff = true // the chart's schema Job does not get to it first
	out, err := upgradeK8s(t, "0.1.20")
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, out)
	}
	if f.live != "JSON" || f.asideCall() >= 0 || len(f.dropped) != 0 {
		t.Fatalf("live=%q dropped=%v, want the new table created and nothing moved or dropped:\n%s", f.live, f.dropped, strings.Join(f.calls, "\n"))
	}
	if strings.Contains(out, "Already on") || strings.Contains(out, "Type 'yes'") || !strings.Contains(out, "nothing is dropped") {
		t.Fatalf("a missing table on the target chart must be created, without asking:\n%s", out)
	}
	if f.marker != nil {
		t.Fatalf("record left behind: %v", f.marker)
	}
}

func TestUpgradeKubernetesKeepsWhatItWasNotToldToDrop(t *testing.T) {
	// No record: a table moved aside by someone else is not this run's to
	// drop, even though the run creates the new table.
	f := newFakeKube(t, "0.1.20", "")
	f.initJobOff = true
	f.aside["events_pre_reset_100"] = "String"
	out, err := upgradeK8s(t, "0.1.20")
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, out)
	}
	if f.live != "JSON" || len(f.dropped) != 0 || f.aside["events_pre_reset_100"] != "String" || !strings.Contains(out, "it is kept") {
		t.Fatalf("live=%q dropped=%v aside=%v:\n%s", f.live, f.dropped, f.aside, out)
	}
}

func TestUpgradeKubernetesTableChangedDuringTheRun(t *testing.T) {
	// No events table when the run decided (nothing asked), a String one by
	// the time it resets: it was not agreed to, so it is not moved.
	f := newFakeKube(t, "0.1.20", "")
	f.initJobOff = true
	f.afterHelm = func(f *fakeKube) { f.live = "String" }
	out, err := upgradeK8s(t, "0.1.20")
	if err == nil || !strings.Contains(out, "changed nothing in it") {
		t.Fatalf("want the run to stop, got err=%v:\n%s", err, out)
	}
	if f.asideCall() >= 0 || f.live != "String" {
		t.Fatalf("a table nobody agreed to discard was moved:\n%s", strings.Join(f.calls, "\n"))
	}
}

func TestUpgradeKubernetesNeverDropsTheNewTable(t *testing.T) {
	t.Run("another release", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "JSON")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if f.asideCall() >= 0 || f.at(markerWrite) >= 0 || f.count("kubectl exec -i") != 0 || f.live != "JSON" {
			t.Fatalf("the new table was touched:\n%s", strings.Join(f.calls, "\n"))
		}
		if !strings.Contains(out, untouched) {
			t.Fatalf("summary:\n%s", out)
		}
	})
	t.Run("already on the target", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "JSON")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil || !strings.Contains(out, "Already on 0.1.20") {
			t.Fatalf("want nothing to do, got err=%v:\n%s", err, out)
		}
		if f.at(helmUpgrade) >= 0 || f.asideCall() >= 0 {
			t.Fatalf("nothing to do, yet:\n%s", strings.Join(f.calls, "\n"))
		}
		// Even with nothing to do, what runs is checked.
		if f.at("kubectl get deployments,statefulsets,pods") < 0 {
			t.Fatalf("no check of what runs:\n%s", strings.Join(f.calls, "\n"))
		}
	})
}

func TestUpgradeKubernetesFailureAfterTheMoveIsFinishedByTheNextRun(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "String")
	f.schemaFails = 1
	f.initJobOff = true
	out, err := upgradeK8s(t, "0.1.20")
	if err == nil {
		t.Fatalf("want the failed schema to fail the run:\n%s", out)
	}
	if f.live != "" || len(f.aside) != 1 || len(f.dropped) != 0 {
		t.Fatalf("after the failure: live=%q aside=%v dropped=%v, want the old table kept aside", f.live, f.aside, f.dropped)
	}
	if f.marker["version"] != "0.1.20" {
		t.Fatalf("the reset is not recorded: %v", f.marker)
	}
	for _, want := range []string{"is events." + eventsAsidePrefix, "its data still there", "recorded in the cluster"} {
		if !strings.Contains(out, want) {
			t.Fatalf("failure does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Upgraded") {
		t.Fatalf("a failed reset printed success:\n%s", out)
	}

	// The chart is on 0.1.20 now: the rerun must not take that for done.
	f.calls = nil
	out, err = upgradeK8s(t, "0.1.20")
	if err != nil {
		t.Fatalf("rerun failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "Already on") || f.at(helmUpgrade) < 0 {
		t.Fatalf("the rerun skipped the unfinished reset:\n%s", out)
	}
	if f.live != "JSON" || len(f.aside) != 0 || len(f.dropped) != 1 || f.asideCall() >= 0 {
		t.Fatalf("rerun: live=%q aside=%v dropped=%v (moved again: %v), want the new table and the old one dropped", f.live, f.aside, f.dropped, f.asideCall() >= 0)
	}
	if f.marker != nil {
		t.Fatalf("record left after the reset finished: %v", f.marker)
	}
}

func TestUpgradeKubernetesUnfinishedResetOnTheTargetChart(t *testing.T) {
	t.Run("recorded, the old table still there", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "String").withMarker("0.1.20")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "Already on") || !strings.Contains(out, "did not finish it") {
			t.Fatalf("want the recorded reset finished:\n%s", out)
		}
		if f.live != "JSON" || len(f.dropped) != 1 || f.marker != nil {
			t.Fatalf("live=%q dropped=%v marker=%v", f.live, f.dropped, f.marker)
		}
	})
	t.Run("recorded, the new table already there", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "JSON").withMarker("0.1.20")
		f.aside["events_pre_reset_100"] = "String"
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if f.asideCall() >= 0 || f.count("kubectl exec -i") != 0 {
			t.Fatalf("the new table was touched:\n%s", strings.Join(f.calls, "\n"))
		}
		if !slices.Equal(f.dropped, []string{"events_pre_reset_100:String"}) || f.marker != nil {
			t.Fatalf("dropped=%v marker=%v, want what the earlier run moved aside dropped and the record cleared", f.dropped, f.marker)
		}
	})
	t.Run("not recorded, the old table under the new chart", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "String")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "Already on") || f.live != "JSON" || len(f.dropped) != 1 {
			t.Fatalf("want the String table under the 0.1.20 chart reset:\n%s", out)
		}
	})
	t.Run("recorded, a release that cannot finish it", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "").withMarker("0.1.20")
		f.aside["events_pre_reset_100"] = "String"
		out, err := upgradeK8s(t, "0.1.17")
		if err == nil || !strings.Contains(out, "Finish it first: origamy upgrade --version 0.1.20") {
			t.Fatalf("want a refusal naming the upgrade that finishes it, got err=%v:\n%s", err, out)
		}
		if f.at(helmUpgrade) >= 0 || len(f.dropped) != 0 || f.marker == nil {
			t.Fatalf("a refused run changed something:\n%s", strings.Join(f.calls, "\n"))
		}
	})
}

func TestUpgradeKubernetesRefusesBeforeAnythingChanges(t *testing.T) {
	cases := []struct {
		name, chart, live, target string
		setup                     func(*fakeKube)
		sets                      []string
		want                      string
	}{
		{"downgrade below the reset", "0.1.20", "JSON", "0.1.17", nil, nil, "which chart 0.1.17 cannot write"},
		{"ClickHouse cannot be asked", "0.1.17", "String", "0.1.20", func(f *fakeKube) { f.chNotReady = true }, nil, "could not be asked"},
		{"ClickHouse does not answer", "0.1.17", "String", "0.1.20", func(f *fakeKube) { f.probeFails = true }, nil, "could not be asked"},
		{"another type", "0.1.17", "Map(String, String)", "0.1.20", nil, nil, "neither the pre-reset (String) nor the new (JSON)"},
		{"schema Job off", "0.1.17", "String", "0.1.20", func(f *fakeKube) { f.values["clickhouse"] = map[string]any{"initSchema": false} }, nil, "clickhouse.initSchema=false"},
		{"old ClickHouse pinned", "0.1.17", "String", "0.1.20", func(f *fakeKube) {
			f.values["clickhouse"] = map[string]any{"image": "clickhouse/clickhouse-server:24.8"}
		}, nil, "needs ClickHouse 25.3 or newer"},
		{"old ClickHouse set", "0.1.17", "String", "0.1.20", nil, []string{"clickhouse.image=clickhouse/clickhouse-server:latest"}, "needs ClickHouse 25.3 or newer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKube(t, tc.chart, tc.live)
			if tc.setup != nil {
				tc.setup(f)
			}
			out, err := upgradeK8s(t, tc.target, tc.sets...)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("want a refusal saying %q, got err=%v:\n%s", tc.want, err, out)
			}
			if f.at(helmUpgrade) >= 0 || f.at(markerWrite) >= 0 || f.asideCall() >= 0 || f.live != tc.live {
				t.Fatalf("a refused run changed something (live %q):\n%s", f.live, strings.Join(f.calls, "\n"))
			}
		})
	}
}

func TestUpgradeKubernetesChecksWhatRuns(t *testing.T) {
	t.Run("a service pinned to a pre-reset release", func(t *testing.T) {
		newFakeKube(t, "0.1.19", "JSON")
		out, err := upgradeK8s(t, "0.1.20", "bulkerWorker.image.tag=0.1.17")
		if err == nil || !strings.Contains(out, "deployment odp-bulker-worker runs "+fakeRepo+"bulker-worker:0.1.17") {
			t.Fatalf("want the check to fail on the 0.1.17 bulker, got err=%v:\n%s", err, out)
		}
		if strings.Contains(out, "Upgraded") {
			t.Fatalf("success printed:\n%s", out)
		}
	})
	t.Run("a tag that is not a release", func(t *testing.T) {
		newFakeKube(t, "0.1.19", "JSON")
		out, err := upgradeK8s(t, "0.1.20", "portalAgent.image.tag=main")
		if err == nil || !strings.Contains(out, "portal-agent:main, whose tag is not a numbered release") {
			t.Fatalf("want the check to fail on :main, got err=%v:\n%s", err, out)
		}
	})
	t.Run("pods of the old release that never go", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.podLag = -1
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "pods still run images that write the other events table") {
			t.Fatalf("want the check to fail on the 0.1.17 pods, got err=%v:\n%s", err, out)
		}
		// The reset itself went through; it stays recorded until the check passes.
		if f.live != "JSON" || f.marker == nil || !strings.Contains(out, "finishes it and checks again") {
			t.Fatalf("live=%q marker=%v:\n%s", f.live, f.marker, out)
		}
	})
	t.Run("pods of the old release that go", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.podLag = 2
		if out, err := upgradeK8s(t, "0.1.20"); err != nil {
			t.Fatalf("a rollout that finishes in time failed the check: %v\n%s", err, out)
		}
	})
	t.Run("the schema Job still at work", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "JSON")
		f.initJobPolls = 3
		if out, err := upgradeK8s(t, "0.1.20"); err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if f.count("kubectl get jobs") < 4 {
			t.Fatalf("the check did not wait for the schema Job:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("already on the target, a table it cannot write", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "JSON")
		f.history[0].tags["bulker-worker"] = "0.1.17"
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || strings.Contains(out, "Already on") {
			t.Fatalf("want the check to fail with nothing to do, got err=%v:\n%s", err, out)
		}
	})
}

func TestUpgradeKubernetesResetSteps(t *testing.T) {
	t.Run("the record cannot be written", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.markerFails = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || f.asideCall() >= 0 || f.live != "String" {
			t.Fatalf("want nothing moved without the record, got err=%v live=%q:\n%s", err, f.live, out)
		}
	})
	t.Run("the move fails", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.renameFails = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "nothing was changed in ClickHouse") || f.live != "String" || f.marker == nil {
			t.Fatalf("got err=%v live=%q marker=%v:\n%s", err, f.live, f.marker, out)
		}
	})
	t.Run("ClickHouse not back", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		// Ready when asked before the upgrade, not after.
		f.chNotReady = false
		prev := kubeRunTimeout
		kubeRunTimeout = func(d time.Duration, name string, args ...string) (string, error) {
			if f.at(helmUpgrade) >= 0 {
				f.chNotReady = true
			}
			return prev(d, name, args...)
		}
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "not ready after the upgrade") || f.live != "String" || f.marker == nil {
			t.Fatalf("got err=%v live=%q marker=%v:\n%s", err, f.live, f.marker, out)
		}
	})
}

func TestUpgradeKubernetesExternalClickHouse(t *testing.T) {
	t.Run("the new table", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "").useExternal("JSON")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if f.count("kubectl exec") != 0 || f.at(markerWrite) >= 0 {
			t.Fatalf("an external ClickHouse was reset or exec'd into:\n%s", strings.Join(f.calls, "\n"))
		}
		// The probe Job is the chart's schema Job with another command.
		tmpl := f.probeJob["spec"].(map[string]any)["template"].(map[string]any)
		c := tmpl["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		labels := tmpl["metadata"].(map[string]any)["labels"].(map[string]any)
		if cmd := c["command"].([]any); !strings.Contains(cmd[2].(string), eventsPropertiesQuery) || c["env"] == nil || c["name"] != "clickhouse-init" {
			t.Fatalf("probe container: %v", c)
		}
		if labels["app.kubernetes.io/instance"] != "odp" || labels["app.kubernetes.io/name"] != "odp" || labels["app.kubernetes.io/component"] != "clickhouse-probe" {
			t.Fatalf("probe labels: %v", labels)
		}
		if tmpl["spec"].(map[string]any)["restartPolicy"] != "Never" || f.at("kubectl delete job") < 0 {
			t.Fatalf("probe Job not one-shot or not deleted: %v", f.probeJob)
		}
	})
	t.Run("the pre-reset table", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "").useExternal("String")
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "does not reset or create the events table on an external ClickHouse") {
			t.Fatalf("want the check to fail with how to fix it on the server, got err=%v:\n%s", err, out)
		}
		if f.count("kubectl exec") != 0 {
			t.Fatalf("exec into an external ClickHouse:\n%s", strings.Join(f.calls, "\n"))
		}
	})
}

func TestRollbackKubernetes(t *testing.T) {
	rollbackRun := func(t *testing.T, to int) (string, error) {
		t.Helper()
		var err error
		out := captureStdout(t, func() { err = rollbackKubernetes(to) })
		return out, err
	}
	twoRevs := func(t *testing.T, from, to, live string) *fakeKube {
		f := newFakeKube(t, from, live)
		f.history = append(f.history, fakeRev{rev: 2, chart: to, tags: allTags(to), chImage: chartCH(to)})
		return f
	}
	t.Run("across the reset with the new table", func(t *testing.T) {
		for _, to := range []int{0, 1} {
			f := twoRevs(t, "0.1.17", "0.1.20", "JSON")
			out, err := rollbackRun(t, to)
			if err == nil || !strings.Contains(out, "predates the storage reset") {
				t.Fatalf("want the rollback refused, got err=%v:\n%s", err, out)
			}
			if f.at("helm rollback") >= 0 {
				t.Fatalf("rolled back:\n%s", strings.Join(f.calls, "\n"))
			}
		}
	})
	t.Run("to the new release over the old table", func(t *testing.T) {
		f := twoRevs(t, "0.1.20", "0.1.17", "String")
		out, err := rollbackRun(t, 0)
		if err == nil || !strings.Contains(out, "origamy upgrade --version 0.1.20") || f.at("helm rollback") >= 0 {
			t.Fatalf("want the rollback refused, got err=%v:\n%s", err, out)
		}
	})
	t.Run("with a reset unfinished", func(t *testing.T) {
		f := twoRevs(t, "0.1.19", "0.1.20", "JSON").withMarker("0.1.20")
		out, err := rollbackRun(t, 0)
		if err == nil || !strings.Contains(out, "Finish it first") || f.at("helm rollback") >= 0 {
			t.Fatalf("want the rollback refused, got err=%v:\n%s", err, out)
		}
	})
	t.Run("a revision that is not there", func(t *testing.T) {
		f := twoRevs(t, "0.1.19", "0.1.20", "JSON")
		out, err := rollbackRun(t, 7)
		if err == nil || !strings.Contains(out, "no revision 7") || f.at("helm rollback") >= 0 {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("on the same side of the reset", func(t *testing.T) {
		f := twoRevs(t, "0.1.19", "0.1.20", "JSON")
		out, err := rollbackRun(t, 0)
		if err != nil || !strings.Contains(out, "Rolled back to revision 1 (origamy-data-plane-0.1.19)") || f.cur().chart != "0.1.19" {
			t.Fatalf("got err=%v chart=%s:\n%s", err, f.cur().chart, out)
		}
		if f.at("kubectl get deployments,statefulsets,pods") < f.at("helm rollback") {
			t.Fatalf("no check after the rollback:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("the check after it fails", func(t *testing.T) {
		f := twoRevs(t, "0.1.19", "0.1.20", "JSON")
		f.history[0].chImage = "clickhouse/clickhouse-server:24.8"
		out, err := rollbackRun(t, 0)
		if err == nil || !strings.Contains(out, "Rolled back to revision 1") || !strings.Contains(out, "needs ClickHouse 25.3 or newer") || strings.Contains(out, "Your data was preserved.") {
			t.Fatalf("want the check to fail after the rollback, got err=%v:\n%s", err, out)
		}
	})
}

func TestKubeEventsCheck(t *testing.T) {
	check := func(t *testing.T, external bool) (kubeCheck, string) {
		t.Helper()
		var c kubeCheck
		var err error
		out := captureStdout(t, func() {
			if c = kubeEventsCheck(external); c.why != "" {
				err = kubeCheckFail("The data plane is installed", "origamy upgrade --version 0.1.20", c, external, false)
			}
		})
		if (c.why == "") != (err == nil) {
			t.Fatalf("check %+v, fail %v", c, err)
		}
		return c, out
	}
	t.Run("passes", func(t *testing.T) {
		newFakeKube(t, "0.1.20", "JSON")
		if c, out := check(t, false); c.why != "" {
			t.Fatalf("got %q:\n%s", c.why, out)
		}
	})
	t.Run("no events table", func(t *testing.T) {
		newFakeKube(t, "0.1.20", "")
		c, out := check(t, false)
		if c.live.gen != liveMissing || !strings.Contains(out, "The chart's schema Job creates it") {
			t.Fatalf("got %+v:\n%s", c, out)
		}
	})
	t.Run("ClickHouse 24.8 under the new table", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "JSON")
		f.history[0].chImage = "clickhouse/clickhouse-server:24.8"
		if c, out := check(t, false); !c.images || !strings.Contains(c.why, "needs ClickHouse 25.3 or newer") {
			t.Fatalf("got %+v:\n%s", c, out)
		}
	})
	t.Run("external, no schema Job to ask with", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "").useExternal("JSON")
		f.values["clickhouse"].(map[string]any)["initSchema"] = false
		c, out := check(t, true)
		if c.live.gen != "" || !strings.Contains(c.why, "clickhouse.initSchema=false") || f.probeJob != nil {
			t.Fatalf("got %+v (probe %v):\n%s", c, f.probeJob, out)
		}
	})
}

func TestKubeDeployProblem(t *testing.T) {
	for _, tc := range []struct {
		name, chart, live, deploy string
		setup                     func(*fakeKube)
		want                      string
	}{
		{"pre-reset table, new release", "0.1.17", "String", "0.1.20", nil, "origamy upgrade --version 0.1.20"},
		{"new table, old release", "0.1.20", "JSON", "0.1.17", nil, "Deploy 0.1.18 or newer"},
		{"unfinished reset", "0.1.20", "", "0.1.20", func(f *fakeKube) { f.withMarker("0.1.20") }, "Finish it first: origamy upgrade --version 0.1.20"},
		{"cannot be asked", "0.1.20", "JSON", "0.1.20", func(f *fakeKube) { f.chNotReady = true }, "could not be asked"},
		{"same generation", "0.1.19", "JSON", "0.1.20", nil, ""},
		{"no table", "0.1.19", "", "0.1.20", nil, ""},
		{"external", "0.1.19", "", "0.1.20", func(f *fakeKube) { f.useExternal("String") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKube(t, tc.chart, tc.live)
			if tc.setup != nil {
				tc.setup(f)
			}
			var headline, hint string
			captureStdout(t, func() { headline, hint = kubeDeployProblem(tc.deploy) })
			if got := headline + " " + hint; (tc.want == "") != (headline == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
			if f.at(markerWrite) >= 0 || f.asideCall() >= 0 {
				t.Fatalf("the check changed something:\n%s", strings.Join(f.calls, "\n"))
			}
		})
	}
}

func TestManifestGeneration(t *testing.T) {
	m := "kind: Deployment\n        - name: bulker-worker\n          image: " + fakeRepo + "bulker-worker:0.1.17\n" +
		"          imagePullPolicy: IfNotPresent\n      - image: \"grafana/alloy:v1.20.1\"\n          image: clickhouse/clickhouse-server:24.8\n"
	if gen, why := manifestGeneration(m); gen != "string" || why != "" {
		t.Fatalf("got %q %q", gen, why)
	}
	if gen, why := manifestGeneration(m + "    image: registry.example.com/mirror/portal-agent:0.1.20\n"); gen != "" || !strings.Contains(why, "different events tables") {
		t.Fatalf("mixed: got %q %q", gen, why)
	}
	if gen, why := manifestGeneration(m + "    image: " + fakeRepo + "portal-agent@sha256:0123456789abcdef\n"); gen != "" || !strings.Contains(why, "not a numbered release") {
		t.Fatalf("digest: got %q %q", gen, why)
	}
	if gen, why := manifestGeneration("image: clickhouse/clickhouse-server:25.3\n"); gen != "" || why == "" {
		t.Fatalf("no services: got %q %q", gen, why)
	}
}

func TestIsDataPlaneContainer(t *testing.T) {
	for c, want := range map[kubeContainer]bool{
		{Name: "bulker-worker", Image: fakeRepo + "bulker-worker:0.1.20"}:            true,
		{Name: "x", Image: "registry.example.com/mirror/bulker-worker:0.1.20"}:       true,
		{Name: "portal-agent", Image: "registry.example.com/odp/agent:0.1.20"}:       true,
		{Name: "alloy", Image: "grafana/alloy:v1.20.1"}:                              false,
		{Name: "clickhouse", Image: "clickhouse/clickhouse-server:25.3"}:             false,
		{Name: "istio-proxy", Image: "docker.io/istio/proxyv2:1.20.0"}:               false,
		{Name: "seed", Image: fakeRepo + "portal-agent@sha256:0123456789abcdef0123"}: true,
	} {
		if got := isDataPlaneContainer(c); got != want {
			t.Errorf("isDataPlaneContainer(%+v) = %v, want %v", c, got, want)
		}
	}
}

func TestConfigMapManifest(t *testing.T) {
	m := configMapManifest("origamy-dp", resetMarkerConfigMap, map[string]string{"version": "0.1.20", "channel": "stable"})
	for _, want := range []string{"kind: ConfigMap", "name: " + resetMarkerConfigMap, "namespace: origamy-dp", "  channel: \"stable\"\n  version: \"0.1.20\"\n"} {
		if !strings.Contains(m, want) {
			t.Fatalf("manifest lacks %q:\n%s", want, m)
		}
	}
}
