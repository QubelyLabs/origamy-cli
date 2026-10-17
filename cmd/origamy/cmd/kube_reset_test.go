package cmd

import (
	"slices"
	"strings"
	"testing"
	"time"
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
	// Recorded (and the lease taken) before the chart moves, and the table
	// renamed after it; cleared only after the check that ends the run, the
	// lease given up last.
	lease, rec, up, mv, del := f.at("kubectl create -f -"), f.at(markerWrite), f.at(helmUpgrade), f.asideCall(), f.at(markerDelete)
	check := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "kubectl get deployments,statefulsets,pods") })
	gave := f.at("kubectl delete configmap " + kubeLeaseConfigMap)
	inOrder := lease < rec && rec < up && up < mv && mv < check && check < del && del < gave
	if lease < 0 || !inOrder {
		t.Fatalf("want lease < record < helm upgrade < RENAME < check < record deleted < lease given up, got %d %d %d %d %d %d %d:\n%s", lease, rec, up, mv, check, del, gave, strings.Join(f.calls, "\n"))
	}
	if f.marker != nil || f.lease != nil {
		t.Fatalf("the reset record or the lease was left behind: %v %v", f.marker, f.lease)
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
	if f.at(helmUpgrade) >= 0 || f.marker != nil || f.lease != nil || f.live != "String" {
		t.Fatalf("a cancelled reset changed something: calls=%v marker=%v lease=%v live=%q", f.calls, f.marker, f.lease, f.live)
	}
}

func TestUpgradeKubernetesCreatesAMissingTable(t *testing.T) {
	// The installed images write the pre-reset table, so the move can touch it;
	// ClickHouse holds none, and the chart's schema Job does not get to it first.
	f := newFakeKube(t, "0.1.17", "")
	f.initJobOff = true
	out, err := upgradeK8s(t, "0.1.20")
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, out)
	}
	if f.live != "JSON" || f.asideCall() >= 0 || len(f.dropped) != 0 {
		t.Fatalf("live=%q dropped=%v, want the new table created and nothing moved or dropped:\n%s", f.live, f.dropped, strings.Join(f.calls, "\n"))
	}
	if strings.Contains(out, "Already on") || strings.Contains(out, "Type 'yes'") || !strings.Contains(out, "nothing is dropped") {
		t.Fatalf("a missing table must be created, without asking:\n%s", out)
	}
	if strings.Contains(out, "history from before the reset is gone") || !strings.Contains(out, "ClickHouse had none") {
		t.Fatalf("the summary must not speak of lost history when there was none:\n%s", out)
	}
	if f.marker != nil || f.lease != nil {
		t.Fatalf("record or lease left behind: %v %v", f.marker, f.lease)
	}
	// What was recorded before the chart moved says there was nothing to discard.
	if rec := f.at(markerWrite); rec < 0 || rec > f.at(helmUpgrade) {
		t.Fatalf("the reset was not recorded before the chart moved:\n%s", strings.Join(f.calls, "\n"))
	}
}

func TestUpgradeKubernetesKeepsWhatItWasNotToldToDrop(t *testing.T) {
	// No record: a table moved aside by someone else is not this run's to
	// drop, even though the run creates the new table.
	f := newFakeKube(t, "0.1.17", "")
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
	f := newFakeKube(t, "0.1.17", "")
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

func TestUpgradeKubernetesACreateNeverBecomesConsentToDrop(t *testing.T) {
	// A run that found no table recorded a create. If it then fails, and the
	// next run finds a pre-reset table, that is a reset like any other: the
	// operator is asked, and only the table this reset moves aside is dropped.
	f := newFakeKube(t, "0.1.17", "")
	f.initJobOff = true
	f.schemaFails = 1
	out, err := upgradeK8s(t, "0.1.20")
	if err == nil || f.marker["action"] != markerCreate || f.marker["aside"] != "" {
		t.Fatalf("want a failed create recorded as a create, got err=%v record=%v:\n%s", err, f.marker, out)
	}
	f.live = "String"
	f.aside["events_pre_reset_100"] = "String" // an operator's own
	withAnswers(t, false, "no\n")
	f.calls = nil
	var rerr error
	out = captureStdout(t, func() {
		rerr = upgradeKubernetes("0.1.20", "stable", nil, false, false, false, false, false, false)
	})
	if rerr == nil || !strings.Contains(out, "Type 'yes' to continue") || !strings.Contains(out, "Cancelled.") {
		t.Fatalf("a String table found by a run that recorded a create must be asked about, got err=%v:\n%s", rerr, out)
	}
	if f.live != "String" || len(f.dropped) != 0 || f.marker["action"] != markerCreate || f.lease != nil {
		t.Fatalf("a refused run changed something: live=%q dropped=%v record=%v lease=%v", f.live, f.dropped, f.marker, f.lease)
	}
	// Agreed to this time: the table this reset moves aside goes, the operator's own stays.
	out, err = upgradeK8s(t, "0.1.20")
	if err != nil || f.live != "JSON" || len(f.dropped) != 1 || f.aside["events_pre_reset_100"] != "String" {
		t.Fatalf("got err=%v live=%q dropped=%v aside=%v:\n%s", err, f.live, f.dropped, f.aside, out)
	}
	if f.marker != nil {
		t.Fatalf("record left behind: %v", f.marker)
	}
}

func TestUpgradeKubernetesNeverDropsTheNewTable(t *testing.T) {
	t.Run("another release", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "JSON")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if f.asideCall() >= 0 || f.live != "JSON" || !f.untouched() {
			t.Fatalf("the new table was touched, or ClickHouse asked, for a move that cannot touch it:\n%s", strings.Join(f.calls, "\n"))
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
		// Even with nothing to do, what runs is checked, by its images.
		if f.at("kubectl get deployments,statefulsets,pods") < 0 || !f.untouched() {
			t.Fatalf("no check of what runs, or ClickHouse asked:\n%s", strings.Join(f.calls, "\n"))
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
		f := newFakeKube(t, "0.1.20", "JSON").withMarker("0.1.20", "events_pre_reset_100")
		f.aside["events_pre_reset_100"] = "String"
		f.aside["events_pre_reset_7"] = "String" // somebody else's: not this reset's
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if f.asideCall() >= 0 || f.count("kubectl exec -i") != 0 {
			t.Fatalf("the new table was touched:\n%s", strings.Join(f.calls, "\n"))
		}
		if !slices.Equal(f.dropped, []string{"events_pre_reset_100:String"}) || f.aside["events_pre_reset_7"] != "String" || f.marker != nil {
			t.Fatalf("dropped=%v aside=%v marker=%v, want only the table the record names dropped and the record cleared", f.dropped, f.aside, f.marker)
		}
	})
	t.Run("recorded without the operator's agreement, the new table already there", func(t *testing.T) {
		f := newFakeKube(t, "0.1.20", "JSON").withMarker("0.1.20", "events_pre_reset_100")
		f.marker["action"] = markerCreate
		f.aside["events_pre_reset_100"] = "String"
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil || len(f.dropped) != 0 || f.aside["events_pre_reset_100"] != "String" || !strings.Contains(out, "it is kept") {
			t.Fatalf("err=%v dropped=%v aside=%v: a create record must not drop anything:\n%s", err, f.dropped, f.aside, out)
		}
	})
	t.Run("not recorded, the old table under the new chart", func(t *testing.T) {
		// Nothing in helm's state says this move touches the events table (the
		// chart is on the target, its images write JSON): it behaves as an
		// upgrade did before the reset existed, and asks ClickHouse nothing.
		// A reset an earlier run left unfinished is recorded, which is what
		// the other cases here are.
		f := newFakeKube(t, "0.1.20", "String")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil || !strings.Contains(out, "Already on 0.1.20") || f.live != "String" || !f.untouched() {
			t.Fatalf("got err=%v live=%q:\n%s", err, f.live, out)
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
		{"old ClickHouse set", "0.1.17", "String", "0.1.20", nil, []string{"clickhouse.image=clickhouse/clickhouse-server:24.8"}, "needs ClickHouse 25.3 or newer"},
		// A --set is split on its commas as helm does: these are not the
		// start of the string, and are still gates of the reset.
		{"old ClickHouse set among others", "0.1.17", "String", "0.1.20", nil, []string{"preset=starter,clickhouse.image=clickhouse/clickhouse-server:24.8"}, "needs ClickHouse 25.3 or newer"},
		{"schema Job set off among others", "0.1.17", "String", "0.1.20", nil, []string{"preset=starter,clickhouse.initSchema=false"}, "clickhouse.initSchema=false"},
		{"schema Job set off, capitalised", "0.1.17", "String", "0.1.20", nil, []string{"clickhouse.initSchema=False"}, "clickhouse.initSchema=false"},
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
			if f.at(helmUpgrade) >= 0 || f.at(markerWrite) >= 0 || f.asideCall() >= 0 || f.live != tc.live || f.marker != nil || f.lease != nil {
				t.Fatalf("a refused run changed something (live %q, record %v, lease %v):\n%s", f.live, f.marker, f.lease, strings.Join(f.calls, "\n"))
			}
		})
	}
}

func TestUpgradeKubernetesChecksWhatRuns(t *testing.T) {
	t.Run("a service pinned to a pre-reset release", func(t *testing.T) {
		// Services of both sides of the storage reset cannot write one table:
		// refused before the chart moves, whatever the table holds.
		f := newFakeKube(t, "0.1.19", "JSON")
		out, err := upgradeK8s(t, "0.1.20", "bulkerWorker.image.tag=0.1.17")
		if err == nil || !strings.Contains(out, "write different events tables (tags 0.1.17, 0.1.20)") || !strings.Contains(out, "Nothing has been changed.") {
			t.Fatalf("want the request refused, got err=%v:\n%s", err, out)
		}
		if f.at(helmUpgrade) >= 0 || !f.untouched() {
			t.Fatalf("a refused run changed something:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("a tag that is not a release", func(t *testing.T) {
		// Which table :main writes is not known: a move that cannot be checked
		// is refused before it begins.
		f := newFakeKube(t, "0.1.19", "JSON")
		out, err := upgradeK8s(t, "0.1.20", "portalAgent.image.tag=main")
		if err == nil || !strings.Contains(out, "whose tag is not a numbered release (main)") || f.at(helmUpgrade) >= 0 || f.lease != nil {
			t.Fatalf("want the move refused, got err=%v:\n%s", err, out)
		}
	})
	t.Run("a tag that is not a release, on a move within one side", func(t *testing.T) {
		// The same tags on both sides move nothing across the reset: as before.
		f := newFakeKube(t, "0.1.19", "JSON")
		f.history[0].tags["portal-agent"] = "main"
		out, err := upgradeK8s(t, "0.1.19", "portalAgent.image.tag=main")
		if err != nil || !f.untouched() {
			t.Fatalf("err=%v:\n%s\n%s", err, out, strings.Join(f.calls, "\n"))
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
		if f.live != "JSON" || f.marker == nil || !strings.Contains(out, "finishes the work on the events table left recorded in ConfigMap "+resetMarkerConfigMap) {
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
		f := newFakeKube(t, "0.1.17", "String")
		f.initJobPolls = 3
		if out, err := upgradeK8s(t, "0.1.20"); err != nil {
			t.Fatalf("upgrade failed: %v\n%s", err, out)
		}
		if f.count("kubectl get jobs") < 4 {
			t.Fatalf("the check did not wait for the schema Job:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("already on the target, a table it cannot write", func(t *testing.T) {
		// One service of the installed release is on the other side of the
		// storage reset: the move is checked against the table.
		f := newFakeKube(t, "0.1.20", "JSON")
		f.history[0].tags["bulker-worker"] = "0.1.17"
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || strings.Contains(out, "Already on") || !strings.Contains(out, "deployment odp-bulker-worker runs") {
			t.Fatalf("want the check to fail with nothing to do, got err=%v:\n%s", err, out)
		}
	})
	t.Run("a move within one side, drifted after the upgrade", func(t *testing.T) {
		// The check that moves of this kind make reads the images the objects
		// run, and asks ClickHouse nothing.
		f := newFakeKube(t, "0.1.19", "JSON")
		f.afterHelm = func(f *fakeKube) { f.cur().tags["bulker-worker"] = "0.1.17" }
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "deployment odp-bulker-worker runs") || strings.Contains(out, "Upgraded") {
			t.Fatalf("want the images check to fail, got err=%v:\n%s", err, out)
		}
		if f.count("kubectl exec") != 0 || f.lease != nil || f.marker != nil {
			t.Fatalf("ClickHouse was asked, or something recorded:\n%s", strings.Join(f.calls, "\n"))
		}
	})
}

func TestUpgradeKubernetesResetSteps(t *testing.T) {
	t.Run("the record cannot be written", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.markerFails = true
		out, err := upgradeK8s(t, "0.1.20")
		// The record is made before the chart moves, so a user who cannot
		// make it moves nothing: not the chart, not the table.
		if err == nil || f.at(helmUpgrade) >= 0 || f.asideCall() >= 0 || f.live != "String" || f.lease != nil ||
			!strings.Contains(out, "could not be recorded") || !strings.Contains(out, "Nothing has been changed.") {
			t.Fatalf("want nothing moved without the record, got err=%v live=%q:\n%s", err, f.live, out)
		}
	})
	t.Run("the move fails", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.renameFails = true
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "ClickHouse still holds it as it was") || strings.Contains(out, "nothing was changed") || f.live != "String" || f.marker == nil {
			t.Fatalf("got err=%v live=%q marker=%v:\n%s", err, f.live, f.marker, out)
		}
	})
	t.Run("the move took effect although the client saw an error", func(t *testing.T) {
		f := newFakeKube(t, "0.1.17", "String")
		f.renameAppliesThenFails = true
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil || f.live != "JSON" || len(f.dropped) != 1 || f.marker != nil || !strings.Contains(out, "it was moved to events.") {
			t.Fatalf("got err=%v live=%q dropped=%v marker=%v:\n%s", err, f.live, f.dropped, f.marker, out)
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
		if err == nil || !strings.Contains(out, "ClickHouse is not ready, so the events table was not reset yet") || f.live != "String" || f.marker == nil {
			t.Fatalf("got err=%v live=%q marker=%v:\n%s", err, f.live, f.marker, out)
		}
	})
}

func TestUpgradeKubernetesExternalClickHouse(t *testing.T) {
	t.Run("a move within one side asks nothing of it", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "").useExternal("String")
		out, err := upgradeK8s(t, "0.1.20")
		if err != nil || !f.untouched() {
			t.Fatalf("err=%v:\n%s\n%s", err, out, strings.Join(f.calls, "\n"))
		}
	})
	t.Run("a move that cannot be told is asked, with a Job copied from the schema Job", func(t *testing.T) {
		// The installed images' tag is not a release: the move can touch the
		// table, which the CLI reads on the operator's server and never resets.
		f := newFakeKube(t, "0.1.19", "").useExternal("JSON").tagAll("main")
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
		f := newFakeKube(t, "0.1.19", "").useExternal("String").tagAll("main")
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "the CLI never resets an external ClickHouse") || !strings.Contains(out, "Nothing has been changed.") {
			t.Fatalf("want the move refused with how to fix it on the server, got err=%v:\n%s", err, out)
		}
		if f.count("kubectl exec") != 0 || f.at(helmUpgrade) >= 0 || f.lease != nil {
			t.Fatalf("exec into an external ClickHouse, or something changed:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("no schema Job to ask it with, on a move that must ask", func(t *testing.T) {
		// Only a move that must ask needs the Job; this one is refused before
		// anything changes, with a message that says what to do.
		f := newFakeKube(t, "0.1.19", "").useExternal("JSON").tagAll("main")
		f.values["clickhouse"].(map[string]any)["initSchema"] = false
		out, err := upgradeK8s(t, "0.1.20")
		if err == nil || !strings.Contains(out, "clickhouse.initSchema=false") || strings.Contains(out, "Check: kubectl") || strings.Contains(out, "Check the Job") || strings.Contains(out, "Once it answers") ||
			!strings.Contains(out, "Check events.events on your server yourself") || f.at(helmUpgrade) >= 0 || f.lease != nil {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
	t.Run("no schema Job to ask it with, on a move that need not", func(t *testing.T) {
		f := newFakeKube(t, "0.1.19", "").useExternal("JSON")
		f.values["clickhouse"].(map[string]any)["initSchema"] = false
		if out, err := upgradeK8s(t, "0.1.20"); err != nil || !f.untouched() {
			t.Fatalf("a move that does not ask must not need the schema Job: err=%v\n%s", err, out)
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
			if f.at("helm rollback") >= 0 || f.lease != nil {
				t.Fatalf("rolled back, or the lease was left:\n%s", strings.Join(f.calls, "\n"))
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
		// A rollback that cannot touch the events table is helm's own, and
		// asks ClickHouse nothing, takes no lease.
		if !f.untouched() {
			t.Fatalf("ClickHouse was asked, or a lease taken:\n%s", strings.Join(f.calls, "\n"))
		}
	})
	t.Run("to a revision whose ClickHouse cannot hold what it writes", func(t *testing.T) {
		// Refused from the revision's own manifest, before anything changes.
		f := twoRevs(t, "0.1.19", "0.1.20", "JSON")
		f.history[0].chImage = "clickhouse/clickhouse-server:24.8"
		out, err := rollbackRun(t, 0)
		if err == nil || !strings.Contains(out, "would run ClickHouse 24.8") || !strings.Contains(out, "Nothing has been changed.") || f.at("helm rollback") >= 0 {
			t.Fatalf("want the rollback refused up front, got err=%v:\n%s", err, out)
		}
	})
	t.Run("the check after it fails", func(t *testing.T) {
		f := twoRevs(t, "0.1.19", "0.1.20", "JSON")
		f.afterHelm = func(f *fakeKube) { f.cur().tags["bulker-worker"] = "0.1.17" }
		out, err := rollbackRun(t, 0)
		if err == nil || !strings.Contains(out, "Rolled back to revision 1") || !strings.Contains(out, "deployment odp-bulker-worker runs") || strings.Contains(out, "Your data was preserved.") {
			t.Fatalf("want the check to fail after the rollback, got err=%v:\n%s", err, out)
		}
	})
	t.Run("within the old side", func(t *testing.T) {
		f := twoRevs(t, "0.1.16", "0.1.17", "String")
		out, err := rollbackRun(t, 0)
		if err != nil || !f.untouched() || f.cur().chart != "0.1.16" {
			t.Fatalf("got err=%v:\n%s", err, out)
		}
	})
}

func TestKubeEventsCheck(t *testing.T) {
	check := func(t *testing.T, external bool) (kubeCheck, string) {
		t.Helper()
		var c kubeCheck
		var err error
		out := captureStdout(t, func() {
			if c = kubeEventsCheck(checkFull, external, ""); c.why != "" {
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
		lease                     bool // the deploy holds the lease on the events table
	}{
		{"pre-reset table, new release", "0.1.17", "String", "0.1.20", nil, "origamy upgrade --version 0.1.20", false},
		{"new table, old release", "0.1.20", "JSON", "0.1.17", nil, "Deploy " + firstJSONChart + " or newer", false},
		{"unfinished reset", "0.1.20", "", "0.1.20", func(f *fakeKube) { f.withMarker("0.1.20") }, "Finish it first: origamy upgrade --version 0.1.20", false},
		{"crosses, cannot be asked", "0.1.17", "String", "0.1.20", func(f *fakeKube) { f.chNotReady = true }, "could not be asked", false},
		{"crosses, forbidden to ask", "0.1.17", "String", "0.1.20", func(f *fakeKube) { f.execForbidden = true }, "create on pods/exec", false},
		{"crosses, table matches the release", "0.1.17", "JSON", "0.1.20", nil, "", true},
		{"another type", "0.1.17", "Map(String, String)", "0.1.20", nil, "neither the pre-reset (String) nor the new (JSON)", false},
		{"a digest pin is replaced by the chart's tag", "0.1.19", "JSON", "0.1.20", func(f *fakeKube) { f.pinDigest("bulkerWorker") }, "", true},
		{"a digest pin is replaced, the table cannot be written", "0.1.19", "String", "0.1.20", func(f *fakeKube) { f.pinDigest("bulkerWorker") }, "origamy upgrade --version 0.1.20", false},
		{"same generation, ClickHouse down", "0.1.20", "JSON", "0.1.20", func(f *fakeKube) { f.chNotReady = true; f.execForbidden = true }, "", false},
		{"same generation, the record unreadable", "0.1.20", "JSON", "0.1.20", func(f *fakeKube) { f.markerReadForbidden = true }, "", false},
		{"crosses, the record unreadable", "0.1.17", "String", "0.1.20", func(f *fakeKube) { f.markerReadForbidden = true }, "may not read ConfigMaps", false},
		{"same generation", "0.1.19", "JSON", "0.1.20", nil, "", false},
		{"no table", "0.1.19", "", "0.1.20", nil, "", false},
		{"external", "0.1.19", "", "0.1.20", func(f *fakeKube) { f.useExternal("String") }, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKube(t, tc.chart, tc.live)
			if tc.setup != nil {
				tc.setup(f)
			}
			var headline, hint string
			var giveUp func()
			captureStdout(t, func() { headline, hint, giveUp = kubeDeployProblem(tc.deploy) })
			if got := headline + " " + hint; (tc.want == "") != (headline == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
			if f.at(markerWrite) >= 0 || f.asideCall() >= 0 {
				t.Fatalf("the check changed something:\n%s", strings.Join(f.calls, "\n"))
			}
			// A deploy that can touch the table keeps the lease until it is over;
			// a refused one gives it back; any other takes none.
			if (giveUp != nil) != tc.lease || (f.lease != nil) != tc.lease {
				t.Fatalf("lease held by the caller %v, in the cluster %v; want %v", giveUp != nil, f.lease != nil, tc.lease)
			}
			if giveUp != nil {
				giveUp()
				if f.lease != nil {
					t.Fatalf("giving up left the lease")
				}
			}
			if tc.lease == false && tc.want == "" && !f.untouched() {
				t.Fatalf("a deploy that cannot touch the events table asked ClickHouse or took a lease:\n%s", strings.Join(f.calls, "\n"))
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
	if gen, why := manifestGeneration(m + "    image: registry.example.com/mirror/portal-agent:main\n"); gen != "" || !strings.Contains(why, "not a numbered release (main)") {
		t.Fatalf("unnumbered: got %q %q", gen, why)
	}
	// A service pinned by digest names no release: it is left out of what the
	// tagged ones write, and alone it says nothing.
	if gen, why := manifestGeneration(m + "    image: " + fakeRepo + "portal-agent@sha256:0123456789abcdef\n"); gen != "string" || why != "" {
		t.Fatalf("a digest beside a tag: got %q %q", gen, why)
	}
	if gen, why := manifestGeneration("image: " + fakeRepo + "portal-agent@sha256:0123456789abcdef\n"); gen != "" || !strings.Contains(why, "digest alone") {
		t.Fatalf("digest alone: got %q %q", gen, why)
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
	if strings.Contains(m, "resourceVersion") {
		t.Fatalf("a manifest to create carries a resourceVersion:\n%s", m)
	}
	// A replace names the version it replaces.
	if m := configMapManifestAt("origamy-dp", kubeLeaseConfigMap, map[string]string{"holder": "a"}, "42"); !strings.Contains(m, "  resourceVersion: \"42\"\n") {
		t.Fatalf("manifest lacks the resourceVersion:\n%s", m)
	}
}
