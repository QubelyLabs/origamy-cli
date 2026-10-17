package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qubelylabs/origamy-cli/internal/token"
)

// composeWithLogs is compose253 with the bundle's opt-in log stack: services of
// the logs and loki profiles, which the CLI does not manage.
const composeWithLogs = compose253 +
	"  alloy:\n    image: grafana/alloy:latest\n    profiles: [\"logs\"]\n" +
	"  loki:\n    image: grafana/loki:3\n    profiles: [\"loki\"]\n"

// runAll puts a container of each of the services on f's host, the
// data-plane ones of compose on release tag.
func (f *fakeCompose) runAll(compose, tag string, services ...string) {
	for _, s := range services {
		image := "other/" + s + ":1"
		switch s {
		case "clickhouse":
			image = "clickhouse/clickhouse-server:25.3"
		case "dp-postgres":
			image = "postgres:16-alpine"
		case "alloy":
			image = "grafana/alloy:latest"
		case "loki":
			image = "grafana/loki:3"
		default:
			if strings.Contains(compose, dpImage+s+":") {
				image = dpImage + s + ":" + tag
			}
		}
		f.running[s] = image
	}
}

func TestCanTouchEventsTable(t *testing.T) {
	for _, tc := range []struct {
		installed, tag, pending string
		want                    bool
	}{
		{"0.1.19", "0.1.20", "", false},
		{"0.1.20", "0.1.20", "", false},
		{"0.1.20", "0.1.19", "", false}, // a rollback within a generation
		{"0.1.18", "0.1.20", "", false},
		{"0.1.17", "0.1.20", "", true}, // across the reset
		{"0.1.16", "0.1.17", "", false},
		{"0.1.20", "0.1.17", "", true}, // a rollback across it
		{"main", "0.1.20", "", true},   // projects of CLI v0.1.15 to v0.1.18
		{"", "0.1.20", "", true},
		{"0.1.19", "main", "", true},
		{"staging", "0.1.20", "", false}, // the edge channel writes JSON
		{"0.1.20", "staging", "", false},
		{"0.1.19", "0.1.20", "0.1.20", true}, // a reset is recorded
	} {
		if got := canTouchEventsTable(tc.installed, tc.tag, tc.pending); got != tc.want {
			t.Errorf("canTouchEventsTable(%q, %q, %q) = %v, want %v", tc.installed, tc.tag, tc.pending, got, tc.want)
		}
	}
}

// TestApplyDockerKeepsOtherProfiles: an in-place deploy turns journeys, the
// AI engine and HTTPS on and off by its answers and nothing else: the
// bundle's log stack (profiles logs and loki), which the operator turned on
// in COMPOSE_PROFILES, keeps running and stays there, however the line is
// written (quotes, a comment).
func TestApplyDockerKeepsOtherProfiles(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "")
	for _, tc := range []struct {
		name, answers string
		profiles      string // COMPOSE_PROFILES afterwards
		removed       []string
	}{
		{"every default answer", "\n\n\n", "full,logs,loki", nil},
		{"journeys off", "n\n\n", "logs,loki", []string{"dp-postgres", "workflow-engine"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, parent := deployProject(t, composeWithLogs, jsonSchemaSQL, "0.1.19")
			p.served["docker-compose.yml"] = composeWithLogs
			withKnownHashes(t, map[string][]string{"docker-compose.yml": {sha256Hex([]byte(composeWithLogs))}})
			writeFile(t, p.env, strings.Replace(readFile(t, p.env), "COMPOSE_PROFILES=full", "COMPOSE_PROFILES=\"full,logs,loki\" # the log stack", 1))
			f := newFakeCompose(t)
			f.live = liveJSONRow
			f.runAll(composeWithLogs, "0.1.19", "clickhouse", "config-sync", "portal-agent", "workflow-engine", "dp-postgres", "alloy", "loki")
			chdir(t, parent)
			withAnswers(t, false, tc.answers)
			var plan *dockerPlan
			var err error
			out := captureStdout(t, func() { plan, err = planDocker("0.1.20", false, true, "dp-1", p.base) })
			if err != nil || plan.fullProfile != (tc.answers == "\n\n\n") {
				t.Fatalf("%v\n%s", err, out)
			}
			defer plan.close()
			out = captureStdout(t, func() {
				err = applyDocker(&token.Enrollment{ID: "dp-1", Tok: "tok", Addr: "tunnel.example.com:443", URL: p.base}, nil, plan)
			})
			if err != nil || !strings.Contains(out, "Deployed") {
				t.Fatalf("%v\n%s", err, out)
			}
			if !slices.Equal(f.removed, tc.removed) || strings.Contains(out, "alloy") || strings.Contains(out, "loki profile is off") {
				t.Fatalf("removed %v, want %v\n%s", f.removed, tc.removed, out)
			}
			if got := dotenvValue(p.env, "COMPOSE_PROFILES"); got != tc.profiles {
				t.Fatalf("COMPOSE_PROFILES = %q, want %q\n%s", got, tc.profiles, readFile(t, p.env))
			}
			for _, s := range []string{"alloy", "loki"} {
				if _, ok := f.running[s]; !ok {
					t.Fatalf("%s was stopped", s)
				}
			}
		})
	}
}

// TestUpgradeDockerDisableAIKeepsOtherProfiles: --disable-ai takes agentic
// out of COMPOSE_PROFILES, read as compose reads it (quotes and a comment
// are not part of it), and stops the engine only.
func TestUpgradeDockerDisableAIKeepsOtherProfiles(t *testing.T) {
	p := newUpgradeProject(t, composeWithLogs, jsonSchemaSQL, func(base string) string {
		return strings.Replace(envAt("0.1.19")(base), "COMPOSE_PROFILES=full", "COMPOSE_PROFILES=\"full,agentic,logs\" # all", 1)
	}, map[string]string{"docker-compose.yml": composeWithLogs})
	withKnownHashes(t, map[string][]string{"docker-compose.yml": {sha256Hex([]byte(composeWithLogs))}})
	f := newFakeCompose(t)
	f.live = liveJSONRow
	f.runAll(composeWithLogs, "0.1.19", "clickhouse", "config-sync", "portal-agent", "workflow-engine", "dp-postgres", "orchestrator-engine", "alloy")
	chdir(t, p.dir)
	var err error
	out := captureStdout(t, func() { err = upgradeDocker("0.1.20", "stable", false, true, false, true, bundleAuto) })
	if err != nil || !strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	if !slices.Equal(f.removed, []string{"orchestrator-engine"}) {
		t.Fatalf("removed %v\n%s", f.removed, out)
	}
	if got := dotenvValue(p.env, "COMPOSE_PROFILES"); got != "full,logs" {
		t.Fatalf("COMPOSE_PROFILES = %q\n%s", got, readFile(t, p.env))
	}
	if _, ok := f.running["alloy"]; !ok {
		t.Fatal("alloy was stopped")
	}
}

// TestUpgradeDockerOrdinaryMove: a move that cannot touch the events table
// behaves as CLI v0.1.20 did, but for cheap checks. ClickHouse is only
// peeked at (never started, never a reason to refuse); nothing is pulled to
// compare, so a registry mirror a host cannot compare, a digest pin, a
// service scaled to 0 and a container left from a profile that is off are
// not reasons to refuse or to fail after the commit point; what is found is
// a warning. Only an image that writes the other events table is refused.
func TestUpgradeDockerOrdinaryMove(t *testing.T) {
	const (
		mirror = "mirror.example.com/origamy/portal-agent:"
		ai     = dpImage + "orchestrator-engine:"
	)
	for _, tc := range []struct {
		name, override string
		setup          func(*fakeCompose)
		says           []string
	}{
		{"ClickHouse does not answer", "", func(f *fakeCompose) { f.probeFails = true }, []string{"which events table it holds was not checked: moving 0.1.19 → 0.1.20 does not change which table the images write"}},
		{"ClickHouse is stopped", "", func(f *fakeCompose) { f.stopped = true }, []string{"was not checked"}},
		{"a registry mirror that cannot be compared", "services:\n  portal-agent:\n    image: " + mirror + "${DP_IMAGE_TAG:-main}\n", func(f *fakeCompose) {
			f.missing[dpImage+"portal-agent:0.1.20"], f.pullFails = true, true
		}, nil},
		{"a digest pin", "services:\n  portal-agent:\n    image: " + dpImage + "portal-agent:0.1.20@sha256:00\n", nil, nil},
		{"a digest pin on the old tag", "services:\n  portal-agent:\n    image: " + dpImage + "portal-agent:0.1.19@sha256:00\n", nil, nil},
		{"a service scaled to 0", "services:\n  portal-agent:\n    scale: 0\n", nil, nil},
		{"replicas 0", "services:\n  portal-agent:\n    deploy:\n      replicas: 0\n", nil, nil},
		{"another release of the generation pinned", "services:\n  portal-agent:\n    image: " + dpImage + "portal-agent:0.1.18\n", nil,
			[]string{"portal-agent runs " + dpImage + "portal-agent:0.1.18, not the 0.1.20 release"}},
		{"a writer an override adds, on the same generation", "services:\n  bulker-worker-2:\n    image: " + dpImage + "bulker-worker:0.1.19\n", nil,
			[]string{"bulker-worker-2 runs " + dpImage + "bulker-worker:0.1.19, not the 0.1.20 release"}},
		{"a container left from a profile that is off", "", func(f *fakeCompose) { f.running["orchestrator-engine"] = ai + "0.1.19" },
			[]string{"orchestrator-engine runs " + ai + "0.1.19, not the 0.1.20 release"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
			writeFile(t, filepath.Join(p.dir, "docker-compose.override.yml"), tc.override)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			if tc.setup != nil {
				tc.setup(f)
			}
			left := f.running["orchestrator-engine"]
			out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
			if err != nil || !strings.Contains(out, "Upgraded") || !strings.Contains(out, notTouched) {
				t.Fatalf("%v\n%s", err, out)
			}
			for _, want := range tc.says {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			// Nothing started to ask, nothing pulled to compare, nothing
			// listed to judge, nothing removed.
			for _, c := range f.calls {
				for _, heavy := range []string{"up -d clickhouse", "docker pull", "image ls", "RENAME", "DROP", "COMPOSE_PROFILES=* "} {
					if strings.Contains(c, heavy) {
						t.Fatalf("a move that cannot touch the table ran %q:\n%s", c, strings.Join(f.calls, "\n"))
					}
				}
			}
			if len(f.removed) != 0 || f.running["orchestrator-engine"] != left {
				t.Fatalf("removed %v\n%s", f.removed, out)
			}
			if readEnvVar(p.env, "DP_IMAGE_TAG") != "0.1.20" {
				t.Fatalf(".env:\n%s", readFile(t, p.env))
			}
		})
	}
}

// TestUpgradeDockerOrdinaryMoveWrongGeneration: what an ordinary move cannot
// afford is a writer of the other events table: the effective configuration
// is read before the commit point (one `docker compose config`), and one
// that would start it is refused with the project as it was; a running
// container of that kind, found after `up -d`, fails the run.
func TestUpgradeDockerOrdinaryMoveWrongGeneration(t *testing.T) {
	override := "services:\n  bulker-worker-2:\n    image: " + dpImage + "bulker-worker:0.1.16\n"
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	writeFile(t, filepath.Join(p.dir, "docker-compose.override.yml"), override)
	f := newFakeCompose(t)
	f.live = liveJSONRow
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || !strings.Contains(out, "bulker-worker-2 is set to "+dpImage+"bulker-worker:0.1.16") || !strings.Contains(out, "write another events table than the 0.1.20 release does") ||
		!strings.Contains(out, "Nothing has been changed.") || strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) || callIndex(f.calls, "--remove-orphans") >= 0 || callIndex(f.calls, " pull") < 0 {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v\ncalls %v", before, after, f.calls)
	}

	// After `up -d`: whatever a container runs, found by its reference.
	p = newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	f = newFakeCompose(t)
	f.live, f.runTag = liveJSONRow, "0.1.17"
	out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || strings.Contains(out, "Upgraded") || !strings.Contains(out, "portal-agent runs "+dpImage+"portal-agent:0.1.17") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestUpgradeDockerOrdinaryMoveEscalates: a plane whose .env says 0.1.19 but
// whose ClickHouse holds the pre-reset table is not an ordinary move once
// ClickHouse is peeked at: it gets the storage reset, asking first.
func TestUpgradeDockerOrdinaryMoveEscalates(t *testing.T) {
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	f := newFakeCompose(t)
	f.live = liveStringRow
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || !strings.Contains(out, "still holds the pre-reset events table") || len(f.dropped) != 1 || !strings.HasSuffix(f.dropped[0], ":String") ||
		!strings.Contains(out, "Every data-plane service runs the 0.1.20 images") {
		t.Fatalf("%v, dropped %v\n%s", err, f.dropped, out)
	}
	// And no table at all is created, not left to the images.
	p = newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	f = newFakeCompose(t)
	out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || !strings.Contains(out, "ClickHouse has no events table") || callIndex(f.calls, "run --rm --no-deps clickhouse-init") < 0 || len(f.dropped) != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestUpgradeDockerLeftoverContainers: on a move that can touch the events
// table, the containers of data-plane services whose profile is off are
// removed before `up -d`, whoever turned the profile off: compose keeps them,
// on whatever release they run, beside the new one. A removal that fails is
// warned about, and the run fails on the container, never reporting every
// service on the release while one is not. Those of services that are not
// data-plane, and of the profiles that are on, are not touched.
func TestUpgradeDockerLeftoverContainers(t *testing.T) {
	const ai = dpImage + "orchestrator-engine:"
	setup := func(t *testing.T) (bundleProject, *fakeCompose) {
		p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("main"), nil)
		f := newFakeCompose(t)
		f.live = liveJSONRow
		f.runAll(compose253, "0.1.17", "clickhouse", "config-sync", "portal-agent", "workflow-engine", "dp-postgres")
		f.running["orchestrator-engine"] = ai + "0.1.17"
		return p, f
	}

	p, f := setup(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || !slices.Equal(f.removed, []string{"orchestrator-engine"}) || !strings.Contains(out, "Removed orchestrator-engine") ||
		!strings.Contains(out, "Every data-plane service runs the 0.1.20 images") {
		t.Fatalf("%v, removed %v\n%s", err, f.removed, out)
	}
	if rm, up := callIndex(f.calls, " rm --stop --force orchestrator-engine"), callIndex(f.calls, "up -d --remove-orphans"); rm < 0 || up < rm {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	if _, ok := f.running["dp-postgres"]; !ok {
		t.Fatal("a service that is not data-plane was removed")
	}

	// It cannot be removed: warned about, with the command, and the run fails.
	p, f = setup(t)
	prev := dockerRun
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		if slices.Contains(args, "rm") {
			return "Error response from daemon: cannot stop container", errors.New("exit status 1")
		}
		return prev(env, name, args...)
	})
	n := len(f.calls)
	out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err == nil || strings.Contains(out, "Every data-plane service runs") || strings.Contains(out, "Upgraded") ||
		!strings.Contains(out, "Could not remove orchestrator-engine (Error response from daemon: cannot stop container)") ||
		!strings.Contains(out, "orchestrator-engine (not run by the configuration) runs "+ai+"0.1.17") || len(f.since(n)) == 0 {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestRemoveInactiveDataPlane: only containers of data-plane services whose
// profiles are all off go, whatever the profiles are called.
func TestRemoveInactiveDataPlane(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	compose := composeWithLogs + "  bulker-debug:\n    image: " + dpImage + "bulker-worker:0.1.16\n    profiles: [\"debug\"]\n"
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), compose)
	writeFile(t, env, "COMPOSE_PROFILES=full\n")
	f := newFakeCompose(t)
	f.runAll(compose, "0.1.17", "clickhouse", "config-sync", "workflow-engine", "dp-postgres", "orchestrator-engine", "alloy", "loki", "bulker-debug")
	f.running["bulker-debug"] = dpImage + "bulker-worker:0.1.16"
	out := captureStdout(t, func() { removeInactiveDataPlane(dir, env, composeEnv("0.1.20"), []string{"full"}) })
	slices.Sort(f.removed)
	if want := []string{"bulker-debug", "orchestrator-engine"}; !slices.Equal(f.removed, want) {
		t.Fatalf("removed %v, want %v\n%s", f.removed, want, out)
	}
	// With the profile on, its container is no leftover.
	f = newFakeCompose(t)
	f.runAll(compose, "0.1.17", "orchestrator-engine")
	captureStdout(t, func() { removeInactiveDataPlane(dir, env, composeEnv("0.1.20"), []string{"full", "agentic"}) })
	if len(f.removed) != 0 {
		t.Fatalf("removed %v", f.removed)
	}
	// Docker cannot say: a warning, no removal.
	f = newFakeCompose(t)
	f.runAll(compose, "0.1.17", "orchestrator-engine")
	f.psFails = true
	out = captureStdout(t, func() { removeInactiveDataPlane(dir, env, composeEnv("0.1.20"), []string{"full"}) })
	if len(f.removed) != 0 || !strings.Contains(out, "Could not tell whether containers of data-plane services whose profile is off are left over") {
		t.Fatalf("removed %v\n%s", f.removed, out)
	}
}

// TestUpgradeDockerPlannedImagesCovers: the planned check (a move that can
// touch the events table) judges every data-plane service the configuration
// starts, also ones an override only adds, and an image pinned by digest by
// that digest, not the tag next to it; a service scaled to 0 starts nowhere.
func TestUpgradeDockerPlannedImagesCovers(t *testing.T) {
	const (
		pa     = dpImage + "portal-agent:"
		digest = "sha256:ab" // the digest of the release's portal-agent
	)
	for _, tc := range []struct {
		name, override string
		setup          func(*fakeCompose)
		says           string // "" = the upgrade goes through
	}{
		{"an old writer an override adds", "services:\n  bulker-worker-2:\n    image: " + dpImage + "bulker-worker:0.1.16\n", nil, "bulker-worker-2 is set to " + dpImage + "bulker-worker:0.1.16"},
		{"a writer an override adds, on the release", "services:\n  bulker-worker-2:\n    image: " + dpImage + "bulker-worker:0.1.20\n", nil, ""},
		{"the release's digest", "services:\n  portal-agent:\n    image: " + pa + "0.1.20@" + digest + "\n", func(f *fakeCompose) {
			f.digests[pa+"0.1.20"] = []string{dpImage + "portal-agent@" + digest}
			f.ids[pa+"0.1.20@"+digest] = f.imageID(pa + "0.1.20")
		}, ""},
		{"the release's digest under an old tag", "services:\n  portal-agent:\n    image: " + pa + "0.1.17@" + digest + "\n", func(f *fakeCompose) {
			f.digests[pa+"0.1.20"] = []string{dpImage + "portal-agent@" + digest}
			f.ids[pa+"0.1.17@"+digest] = f.imageID(pa + "0.1.20")
		}, ""},
		// The tag text changed, the digest is the old image's.
		{"the old digest under the release's tag", "services:\n  portal-agent:\n    image: " + pa + "0.1.20@sha256:cd\n", func(f *fakeCompose) {
			f.digests[pa+"0.1.20"] = []string{dpImage + "portal-agent@" + digest}
		}, "portal-agent is pinned by digest (" + pa + "0.1.20@sha256:cd), which is not the digest of the release's image " + pa + "0.1.20"},
		{"a digest that cannot be compared", "services:\n  portal-agent:\n    image: " + pa + "0.1.20@" + digest + "\n", func(f *fakeCompose) {
			f.missing[pa+"0.1.20"], f.pullFails = true, true
		}, "could not be pulled here to compare them"},
		{"scaled to 0 on an old tag", "services:\n  portal-agent:\n    image: " + pa + "0.1.17\n    scale: 0\n", nil, ""},
		{"a mirror under a service name nobody knows", "services:\n  shadow:\n    image: mirror.example.com/x/bw:0.1.20\n", func(f *fakeCompose) {
			f.ids["mirror.example.com/x/bw:0.1.20"] = f.imageID(dpImage + "bulker-worker:0.1.16")
			f.images = []string{dpImage + "bulker-worker:0.1.16"}
		}, "shadow runs mirror.example.com/x/bw:0.1.20, which is not the release's image " + dpImage + "bulker-worker:0.1.20"},
		{"a mirror under a service name nobody knows, the release's image", "services:\n  shadow:\n    image: mirror.example.com/x/bw:0.1.20\n", func(f *fakeCompose) {
			f.ids["mirror.example.com/x/bw:0.1.20"] = f.imageID(dpImage + "bulker-worker:0.1.20")
			f.images = []string{dpImage + "bulker-worker:0.1.20"}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("main"), nil)
			writeFile(t, filepath.Join(p.dir, "docker-compose.override.yml"), tc.override)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			if tc.setup != nil {
				tc.setup(f)
			}
			before := p.snapshot(t)
			out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
			if tc.says == "" {
				if err != nil || !strings.Contains(out, "Upgraded") {
					t.Fatalf("%v\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(out, tc.says) || !strings.Contains(out, "Nothing has been changed.") || strings.Contains(out, "Upgraded") {
				t.Fatalf("%v\n%s", err, out)
			}
			if after := p.snapshot(t); !mapsEqual(before, after) || callIndex(f.calls, "--remove-orphans") >= 0 {
				t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestUpgradeDockerMirrorWithoutGHCR: a host that cannot reach ghcr.io cannot
// compare a registry mirror's image with the release's, so a move that can
// touch the events table is refused before anything is changed, with advice
// that works there: the comparison needs the release's image on the host
// under its ghcr.io reference, which `docker tag` or `docker load` gives
// (the refusal does not tell the operator to pull it). An ordinary move is
// not refused (TestUpgradeDockerOrdinaryMove).
func TestUpgradeDockerMirrorWithoutGHCR(t *testing.T) {
	const mirror = "mirror.example.com/origamy/portal-agent:"
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("main"), nil)
	writeFile(t, filepath.Join(p.dir, "docker-compose.override.yml"), "services:\n  portal-agent:\n    image: "+mirror+"${DP_IMAGE_TAG:-main}\n")
	f := newFakeCompose(t)
	f.live = liveJSONRow
	f.missing[dpImage+"portal-agent:0.1.20"], f.pullFails = true, true
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	for _, want := range []string{
		"portal-agent runs " + mirror + "0.1.20, and the release's image " + dpImage + "portal-agent:0.1.20 could not be pulled here to compare them",
		"A host that cannot reach ghcr.io cannot make the comparison, and the move is refused before anything is changed",
		"`docker tag <the mirror's image> " + dpImage + "<service>:0.1.20`", "docker save",
		"Nothing has been changed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if err == nil || strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatal("the project was touched")
	}

	// The advice works: the release's image on the host under its ghcr.io
	// reference (tagged from the mirror's, or loaded), the same image.
	f.missing = map[string]bool{}
	f.ids[mirror+"0.1.20"] = f.imageID(dpImage + "portal-agent:0.1.20")
	out, err = upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	if err != nil || !strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestUpgradeDockerStrandedPlane (cardinal rule 3): a plane run under a
// COMPOSE_PROJECT_NAME exported in the shell and then taken down (no
// container) has its data volumes under that name. The CLI pins the
// directory's, so `up -d clickhouse` would create a new, empty plane beside
// them and the upgrade would report it done. It is refused before anything is
// created, saying how to go on; with its volumes under the pinned name, or the
// name in .env, it is not.
func TestUpgradeDockerStrandedPlane(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "sk36foo")
	foo := [][3]string{{"sk36foo", "clickhouse-data", "sk36foo_clickhouse-data"}, {"sk36foo", "nats-data", "sk36foo_nats-data"}}
	p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
	name := composeProjectName(p.dir, p.env)
	f := newFakeCompose(t)
	f.live = liveJSONRow
	f.volumes = foo
	before := p.snapshot(t)
	out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto)
	for _, want := range []string{
		"Nothing of this data plane exists under the compose project name " + name,
		"sk36foo_clickhouse-data, sk36foo_nats-data (compose project sk36foo)",
		"Your shell exports COMPOSE_PROJECT_NAME=sk36foo, which this CLI ignores.",
		"a new, empty plane", "(COMPOSE_PROJECT_NAME=sk36foo), then rerun", "Nothing has been changed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if err == nil || strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	// Nothing was created: no compose command at all, the project as it was.
	if n := len(composeCalls(f.calls)); n != 0 {
		t.Fatalf("%d compose calls:\n%s", n, strings.Join(f.calls, "\n"))
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched:\nbefore %v\nafter  %v", before, after)
	}

	for _, tc := range []struct {
		name  string
		setup func(*fakeCompose)
	}{
		{"the plane has volumes under its own name", func(f *fakeCompose) {
			f.volumes = append(slices.Clone(foo), [3]string{name, "clickhouse-data", name + "_clickhouse-data"})
		}},
		{"the other project runs from another directory", func(f *fakeCompose) { f.owners = "sk36foo\t" + t.TempDir() + "\n" }},
		{"the volumes are not a plane's", func(f *fakeCompose) { f.volumes = [][3]string{{"sk36foo", "data", "sk36foo_data"}} }},
		{"Docker cannot list the volumes", func(f *fakeCompose) { f.volumesFail = true }},
		{"there are none", func(f *fakeCompose) { f.volumes = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newUpgradeProject(t, compose253, jsonSchemaSQL, envAt("0.1.19"), nil)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			f.volumes = foo
			tc.setup(f)
			if out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto); err != nil || !strings.Contains(out, "Upgraded") {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}

	// The name in .env: pinned, and the plane found under it.
	p = newUpgradeProject(t, compose253, jsonSchemaSQL, func(base string) string { return envAt("0.1.19")(base) + "COMPOSE_PROJECT_NAME=sk36foo\n" }, nil)
	f = newFakeCompose(t)
	f.live, f.volumes = liveJSONRow, foo
	if out, err := upgradeIn(t, p.dir, "0.1.20", bundleAuto); err != nil || !strings.Contains(out, "Upgraded") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range composeCalls(f.calls) {
		if !strings.Contains(c, " docker compose -p sk36foo ") {
			t.Fatalf("not pinned to sk36foo: %q", c)
		}
	}
}

// TestPlanDockerStrandedPlane: the same for an in-place deploy, before the
// token is redeemed.
func TestPlanDockerStrandedPlane(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "sk36foo")
	p, parent := deployProject(t, compose253, jsonSchemaSQL, "0.1.19")
	f := newFakeCompose(t)
	f.live = liveJSONRow
	f.volumes = [][3]string{{"sk36foo", "clickhouse-data", "sk36foo_clickhouse-data"}}
	before := p.snapshot(t)
	_, out, err := planIn(t, parent, "0.1.20", p.base)
	if err == nil || !strings.Contains(out, "Nothing of this data plane exists under the compose project name origamy-dp-dp-1") || !strings.Contains(out, "sk36foo_clickhouse-data") ||
		!strings.Contains(out, "Nothing has been enrolled or installed.") || len(composeCalls(f.calls)) != 0 {
		t.Fatalf("%v, calls %v\n%s", err, f.calls, out)
	}
	if after := p.snapshot(t); !mapsEqual(before, after) {
		t.Fatalf("the project was touched")
	}
}

// TestUninstallDockerStrandedPlane: uninstalling a plane whose data volumes
// are under another name than the pinned one says so before it asks, removes
// what it can (the directory) and does not report the data as removed: it
// lists the volumes that stay.
func TestUninstallDockerStrandedPlane(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "sk36foo")
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chdir(t, parent)
	dir := filepath.Join(parent, "origamy-dp-dp-1")
	writeProject(t, dir, "dp-1")
	f := newFakeCompose(t)
	f.volumes = [][3]string{{"sk36foo", "clickhouse-data", "sk36foo_clickhouse-data"}, {"sk36foo", "dp-redis-data", "sk36foo_dp-redis-data"}}
	out := captureStdout(t, func() { err = uninstallDocker("dp-1", true) })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Nothing of this data plane exists under the compose project name origamy-dp-dp-1, but the data volumes of a plane like it do under another: sk36foo_clickhouse-data, sk36foo_dp-redis-data (compose project sk36foo)",
		"put COMPOSE_PROJECT_NAME=sk36foo in " + filepath.Join(dir, ".env"),
		"Compose project origamy-dp-dp-1 had no containers or volumes to remove",
		"The data plane's directory is removed, but its data was NOT",
		"docker volume rm sk36foo_clickhouse-data sk36foo_dp-redis-data",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, nope := range []string{"Containers and volumes of", "Data plane uninstalled"} {
		if strings.Contains(out, nope) {
			t.Errorf("output claims %q:\n%s", nope, out)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the directory is still there: %v", err)
	}
	// With the name in .env, `down -v` reaches them and the usual messages stand.
	writeProject(t, dir, "dp-1")
	writeFile(t, filepath.Join(dir, ".env"), "DATA_PLANE_ID=dp-1\nCOMPOSE_PROJECT_NAME=sk36foo\n")
	f.owners = "sk36foo\t" + dir + "\n"
	out = captureStdout(t, func() { err = uninstallDocker("dp-1", true) })
	if err != nil || !strings.Contains(out, "Containers and volumes of sk36foo removed") || !strings.Contains(out, "Data plane uninstalled") || strings.Contains(out, "NOT") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// TestAdvisoryImageNotes: the cheap look a move that cannot touch the
// events table takes after `up -d`: running data-plane containers by the
// reference they were made from, no pull and no comparison.
func TestAdvisoryImageNotes(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	f := newFakeCompose(t)
	f.running = map[string]string{
		"portal-agent":        dpImage + "portal-agent:0.1.20",
		"config-sync":         dpImage + "config-sync:0.1.19",
		"bulker-worker":       dpImage + "bulker-worker:0.1.16",
		"workflow-engine":     dpImage + "workflow-engine:0.1.20@sha256:00",
		"clickhouse":          "clickhouse/clickhouse-server:25.3",
		"orchestrator-engine": dpImage + "orchestrator-engine:0.1.17",
	}
	f.states["orchestrator-engine"] = "exited"
	notes, wrong := advisoryImageNotes(dir, env, "0.1.20")
	if want := []string{"config-sync runs " + dpImage + "config-sync:0.1.19"}; !slices.Equal(notes, want) {
		t.Errorf("notes %v, want %v", notes, want)
	}
	if want := []string{"bulker-worker runs " + dpImage + "bulker-worker:0.1.16"}; !slices.Equal(wrong, want) {
		t.Errorf("wrong generation %v, want %v", wrong, want)
	}
	f.psFails = true
	if notes, wrong := advisoryImageNotes(dir, env, "0.1.20"); notes != nil || wrong != nil {
		t.Errorf("Docker cannot list them: %v %v", notes, wrong)
	}
}

// TestPeekClickHouse: it asks and starts nothing.
func TestPeekClickHouse(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	for _, tc := range []struct {
		name  string
		setup func(*fakeCompose)
		gen   string
	}{
		{"answers", func(f *fakeCompose) { f.live = liveJSONRow }, "json"},
		{"stopped", func(f *fakeCompose) { f.live, f.stopped = liveJSONRow, true }, ""},
		{"hangs", func(f *fakeCompose) { f.live, f.probeFails = liveJSONRow, true }, ""},
		{"no table", func(f *fakeCompose) {}, liveMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			tc.setup(f)
			var got liveSchema
			captureStdout(t, func() { got = peekClickHouse(dir, env, "0.1.19") })
			if got.gen != tc.gen {
				t.Fatalf("gen %q, want %q", got.gen, tc.gen)
			}
			for _, c := range f.calls {
				if strings.Contains(c, " up ") || strings.Contains(c, " start ") || strings.HasPrefix(c, "start") {
					t.Fatalf("started something: %q", c)
				}
			}
		})
	}
}

// TestApplyDockerLeftoverContainers: an in-place deploy over a plane that
// can touch the events table (here one that runs :main) removes the
// containers of data-plane services whose profile is off before `up -d`;
// over one that cannot, they are left as they were.
func TestApplyDockerLeftoverContainers(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "")
	const ai = dpImage + "orchestrator-engine:"
	for _, tc := range []struct {
		name, from string
		removed    []string
	}{
		{"over a plane on main", "main", []string{"orchestrator-engine"}},
		{"over the same generation", "0.1.19", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, parent := deployProject(t, compose253, jsonSchemaSQL, tc.from)
			f := newFakeCompose(t)
			f.live = liveJSONRow
			f.runAll(compose253, "0.1.19", "clickhouse", "config-sync", "portal-agent", "workflow-engine", "dp-postgres")
			f.running["orchestrator-engine"] = ai + "0.1.19"
			plan, out, err := planIn(t, parent, "0.1.20", p.base)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			defer plan.close()
			out = captureStdout(t, func() {
				err = applyDocker(&token.Enrollment{ID: "dp-1", Tok: "tok", Addr: "tunnel.example.com:443", URL: p.base}, nil, plan)
			})
			if err != nil || !strings.Contains(out, "Deployed") || !slices.Equal(f.removed, tc.removed) {
				t.Fatalf("%v, removed %v\n%s", err, f.removed, out)
			}
		})
	}
}
