package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseComposePS(t *testing.T) {
	// Compose ≥ 2.21: one object per line.
	ndjson := `{"Service":"nats","State":"running","Health":"healthy","ExitCode":0}
{"Service":"nats-init","State":"exited","Health":"","ExitCode":0}
`
	got, err := parseComposePS(ndjson)
	if err != nil || len(got) != 2 || got[0].Service != "nats" || got[1].State != "exited" {
		t.Fatalf("ndjson: got %+v, err %v", got, err)
	}
	// Compose < 2.21: a JSON array.
	arr := `[{"Service":"portal-agent","State":"running","Health":"","ExitCode":0}]`
	got, err = parseComposePS(arr)
	if err != nil || len(got) != 1 || got[0].Service != "portal-agent" {
		t.Fatalf("array: got %+v, err %v", got, err)
	}
	if got, err := parseComposePS("  \n"); err != nil || got != nil {
		t.Fatalf("empty output must be (nil, nil), got %+v, %v", got, err)
	}
	if _, err := parseComposePS("{not json"); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestClassifyCompose(t *testing.T) {
	svcs := []composeService{
		{Service: "nats", State: "running", Health: "healthy"},
		{Service: "portal-agent", State: "running"},                   // no healthcheck → ready
		{Service: "clickhouse", State: "running", Health: "starting"}, // still probing
		{Service: "nats-init", State: "exited", ExitCode: 0},          // one-shot, done
		{Service: "clickhouse-init", State: "exited", ExitCode: 1},    // one-shot that failed
		{Service: "bulker-worker", State: "exited", ExitCode: 2},      // exec format error, bad token, …
		{Service: "workflow-engine", State: "restarting"},
		{Service: "caddy", State: "created"},
		{Service: "redis", State: "running", Health: "unhealthy"},
	}
	pending, failed := classifyCompose(svcs)
	if want := []string{"caddy", "clickhouse"}; !reflect.DeepEqual(pending, want) {
		t.Fatalf("pending = %v, want %v", pending, want)
	}
	if want := []string{"bulker-worker", "clickhouse-init", "redis", "workflow-engine"}; !reflect.DeepEqual(failed, want) {
		t.Fatalf("failed = %v, want %v", failed, want)
	}
	// Everything up: nothing pending, nothing failed.
	pending, failed = classifyCompose([]composeService{
		{Service: "nats", State: "running", Health: "healthy"},
		{Service: "nats-init", State: "exited", ExitCode: 0},
	})
	if len(pending) != 0 || len(failed) != 0 {
		t.Fatalf("healthy stack reported pending=%v failed=%v", pending, failed)
	}
}

// chdir moves into dir for the test and restores the cwd afterwards.
func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

func writeProject(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := ""
	if id != "" {
		env = "DATA_PLANE_ID=" + id + "\nCOMPOSE_PROFILES=full\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestComposeProjectDir(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)

	// Nothing here.
	if _, err := composeProjectDir(); err != errNoProject {
		t.Fatalf("empty dir: want errNoProject, got %v", err)
	}

	// A docker-compose.yml that is NOT ours (no DATA_PLANE_ID) must be ignored,
	// not adopted — `origamy upgrade` would otherwise rewrite its .env.
	writeProject(t, root, "")
	if _, err := composeProjectDir(); err != errNoProject {
		t.Fatalf("foreign compose project: want errNoProject, got %v", err)
	}
	if id := existingProjectID("."); id != "" {
		t.Fatalf("foreign project must have no id, got %q", id)
	}

	// Exactly one ./origamy-dp-<id>/ below the cwd is found.
	writeProject(t, filepath.Join(root, "origamy-dp-a"), "dp-a")
	dir, err := composeProjectDir()
	if err != nil || filepath.Base(dir) != "origamy-dp-a" {
		t.Fatalf("single project: got %q, %v", dir, err)
	}

	// Two candidates: refuse to guess, name both.
	writeProject(t, filepath.Join(root, "origamy-dp-b"), "dp-b")
	if _, err := composeProjectDir(); err == nil || !strings.Contains(err.Error(), "origamy-dp-a") || !strings.Contains(err.Error(), "origamy-dp-b") {
		t.Fatalf("two projects: want an error naming both, got %v", err)
	}

	// Inside a project, the cwd wins regardless of siblings.
	chdir(t, filepath.Join(root, "origamy-dp-b"))
	dir, err = composeProjectDir()
	if err != nil || filepath.Base(dir) != "origamy-dp-b" {
		t.Fatalf("inside project: got %q, %v", dir, err)
	}
	if id := existingProjectID("."); id != "dp-b" {
		t.Fatalf("existingProjectID = %q, want dp-b", id)
	}
}

func TestHasProfile(t *testing.T) {
	if !hasProfile([]string{"full", "agentic"}, "agentic") || hasProfile([]string{"full"}, "agentic") || hasProfile(nil, "full") {
		t.Fatal("hasProfile wrong")
	}
}

func TestClassifyComposeRestarting(t *testing.T) {
	// A crash-looping container is "running" between restarts; only Status
	// shows the loop.
	_, failed := classifyCompose([]composeService{
		{Service: "portal-agent", State: "running", Status: "Restarting (1) 3 seconds ago"},
		{Service: "nats", State: "running", Status: "Up 40 seconds (healthy)", Health: "healthy"},
	})
	if len(failed) != 1 || failed[0] != "portal-agent" {
		t.Fatalf("restarting service must be failed, got %v", failed)
	}
}

func TestProbeLiveSchema(t *testing.T) {
	warn := "time=\"2026-10-16T20:00:00Z\" level=warning msg=\"The \\\"WRITE_KEYS\\\" variable is not set. Defaulting to a blank string.\"\n"
	for _, tc := range []struct {
		name, out string
		err       error
		want      string
		typ       string
	}{
		{"JSON", liveJSONRow, nil, "json", "JSON"},
		{"JSON with parameters", "JSON(max_dynamic_paths=1024)\t" + eventsColumnMark + "\n", nil, "json", "JSON(max_dynamic_paths=1024)"},
		{"String", liveStringRow, nil, "string", "String"},
		// A bare answer (an older probe, a stand-in) is taken when it is one
		// of the two.
		{"bare JSON", "JSON\n", nil, "json", "JSON"},
		{"bare String", "String\n", nil, "string", "String"},
		// compose warns about variables an older .env lacks, on the same output.
		{"String after compose warnings", warn + warn + liveStringRow, nil, "string", "String"},
		// ClickHouse answered, and there is no such table.
		{"no events table", "", nil, liveMissing, ""},
		{"no events table, compose warnings", warn, nil, liveMissing, ""},
		// Neither generation: told apart from the table missing.
		{"another type", "Object('json')\t" + eventsColumnMark + "\n", nil, liveOther, "Object('json')"},
		{"a row of something else is not ours", "Nullable(String)\tsomething.else\n", nil, liveMissing, ""},
		// Not asked: not running, or no answer in time.
		{"ClickHouse not running", "service \"clickhouse\" is not running", errors.New("exit status 1"), "", ""},
		{"no answer in time", "", errors.New("no answer within 20s"), "", ""},
	} {
		var call string
		var limit time.Duration
		prev := dockerRunTimeout
		dockerRunTimeout = func(d time.Duration, env []string, name string, args ...string) (string, error) {
			call, limit = strings.Join(append(append([]string{}, env...), args...), " "), d
			return tc.out, tc.err
		}
		got := probeLiveSchema("/dp", "/dp/.env", []string{"DP_IMAGE_TAG=0.1.20"})
		dockerRunTimeout = prev
		if got.gen != tc.want || got.typ != tc.typ {
			t.Errorf("%s: got %+v, want %q %q", tc.name, got, tc.want, tc.typ)
		}
		if !strings.HasPrefix(call, "DP_IMAGE_TAG=0.1.20 compose --project-directory /dp --env-file /dp/.env exec -T clickhouse sh -c ") || limit != clickhouseProbeTimeout {
			t.Errorf("%s: call %q within %s", tc.name, call, limit)
		}
	}
}

func TestRunCapturedEnvTimeout(t *testing.T) {
	// A command that hangs is killed once its time is up.
	start := time.Now()
	out, err := runCapturedEnvTimeout(200*time.Millisecond, nil, "sh", "-c", "echo started; sleep 30")
	if err == nil || !strings.Contains(err.Error(), "no answer within 200ms") || !strings.Contains(out, "started") {
		t.Fatalf("%q, %v", out, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %s", took)
	}
	if out, err := runCapturedEnvTimeout(5*time.Second, []string{"X=1"}, "sh", "-c", "echo $X"); err != nil || strings.TrimSpace(out) != "1" {
		t.Fatalf("%q, %v", out, err)
	}
	if _, err := runCapturedEnvTimeout(5*time.Second, nil, "sh", "-c", "exit 3"); err == nil || strings.Contains(err.Error(), "no answer") {
		t.Fatalf("a failing command is not a timeout: %v", err)
	}
}

func TestEffectiveCompose(t *testing.T) {
	var call string
	stubDocker(t, func(env []string, name string, args ...string) (string, error) {
		call = strings.Join(append(append([]string{}, env...), args...), " ")
		return renderComposeConfig(args)
	})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), compose253)
	writeFile(t, filepath.Join(dir, "override.yml"), "services:\n  clickhouse:\n    image: clickhouse/clickhouse-server:24.8\n")
	files := []string{"-f", filepath.Join(dir, "docker-compose.yml"), "-f", filepath.Join(dir, "override.yml")}
	cfg, err := effectiveCompose(dir, filepath.Join(dir, ".env"), "0.1.20", files)
	if err != nil {
		t.Fatal(err)
	}
	// The files are rendered together, with the .env and the target tag, and
	// the JSON is read past compose's warnings.
	if want := "DP_IMAGE_TAG=0.1.20 compose --project-directory " + dir + " " + strings.Join(files, " ") + " --env-file " + filepath.Join(dir, ".env") + " config --format json"; call != want {
		t.Fatalf("call %q\nwant %q", call, want)
	}
	if cfg.Services["clickhouse"].Image != "clickhouse/clickhouse-server:24.8" {
		t.Fatalf("cfg = %+v", cfg)
	}
	// compose failing, or printing no configuration, is an error.
	stubDocker(t, func([]string, string, ...string) (string, error) {
		return "yaml: line 3: did not find expected key", errors.New("exit status 15")
	})
	if _, err := effectiveCompose(dir, filepath.Join(dir, ".env"), "0.1.20", files); err == nil || !strings.Contains(err.Error(), "did not find expected key") {
		t.Fatalf("err = %v", err)
	}
	stubDocker(t, func([]string, string, ...string) (string, error) { return "level=warning msg=x\n", nil })
	if _, err := effectiveCompose(dir, filepath.Join(dir, ".env"), "0.1.20", files); err == nil {
		t.Fatal("no configuration must be an error")
	}
}

// TestAskClickHouse: a ClickHouse that does not answer is started on what the
// project runs now (its own files, the release in .env), never the target,
// and asked again; gen stays "" when it still cannot be asked.
func TestAskClickHouse(t *testing.T) {
	dc := "DP_IMAGE_TAG=0.1.16 docker compose --project-directory /dp --env-file /dp/.env "
	for _, tc := range []struct {
		name  string
		setup func(*fakeCompose)
		want  string
		start bool
	}{
		{"answers", func(f *fakeCompose) { f.live = liveStringRow }, "string", false},
		{"stopped", func(f *fakeCompose) { f.live, f.stopped = liveJSONRow, true }, "json", true},
		{"stopped, no events table", func(f *fakeCompose) { f.stopped = true }, liveMissing, true},
		{"hangs once", func(f *fakeCompose) { f.live, f.probeHangs = liveStringRow, 1 }, "string", true},
		{"never answers the probe", func(f *fakeCompose) { f.live, f.probeFails = liveStringRow, true }, "", true},
		{"never answers at all", func(f *fakeCompose) { f.stopped, f.pingFails = true, -1 }, "", true},
		{"cannot be started", func(f *fakeCompose) { f.stopped, f.startFails = true, true }, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCompose(t)
			tc.setup(f)
			var got liveSchema
			out := captureStdout(t, func() { got = askClickHouse("/dp", "/dp/.env", "0.1.16") })
			if got.gen != tc.want {
				t.Fatalf("gen %q\n%s", got.gen, out)
			}
			up := callIndex(f.calls, "up -d clickhouse")
			if (up >= 0) != tc.start || (up >= 0 && f.calls[up] != dc+"up -d clickhouse") {
				t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
			}
			for _, c := range f.calls {
				if !strings.HasPrefix(c, dc) || strings.Contains(c, " -f ") {
					t.Fatalf("not on what the project runs now: %q", c)
				}
			}
		})
	}
}

// TestEventsTableProblem: a missing table is asked about again while the
// init service `up -d` started is still at work, and the answer is held
// against what the release writes.
func TestEventsTableProblem(t *testing.T) {
	prevTimeout, prevPoll := clickhouseReadyTimeout, clickhouseReadyPoll
	clickhouseReadyTimeout, clickhouseReadyPoll = 2*time.Second, time.Millisecond
	t.Cleanup(func() { clickhouseReadyTimeout, clickhouseReadyPoll = prevTimeout, prevPoll })
	for _, tc := range []struct {
		name         string
		running      int    // ps lists clickhouse-init as running this many times
		answer       string // the probe's answer once it has stopped
		tag, problem string
	}{
		{"created while the init runs", 3, liveJSONRow, "0.1.20", ""},
		{"never created", 2, "", "0.1.20", "ClickHouse holds no events table"},
		{"the wrong generation", 0, liveStringRow, "0.1.20", "the pre-reset events table (String payload columns), which the 0.1.20 images cannot write"},
		{"a release before the reset", 0, liveStringRow, "0.1.17", ""},
		{"a moving tag, either generation", 0, liveStringRow, "main", ""},
		{"a moving tag, another type", 0, "Object('json')\t" + eventsColumnMark + "\n", "main", "neither the pre-reset (String) nor the new (JSON) schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := 0
			stubDocker(t, func(env []string, name string, args ...string) (string, error) {
				switch last := args[len(args)-1]; {
				case last == "clickhouse-init" && slices.Contains(args, "ps"):
					if ps++; ps <= tc.running {
						return "time=x level=warning msg=y\n" + `{"Service":"clickhouse-init","State":"running"}` + "\n", nil
					}
					return `{"Service":"clickhouse-init","State":"exited","ExitCode":0}` + "\n", nil
				case last == clickhouseEventsPropertiesType:
					if ps < tc.running {
						return "", nil
					}
					return tc.answer, nil
				}
				return "", nil
			})
			var why string
			out := captureStdout(t, func() { _, why = eventsTableProblem("/dp", "/dp/.env", tc.tag) })
			if (why == "") != (tc.problem == "") || !strings.Contains(why, tc.problem) {
				t.Fatalf("why %q\n%s", why, out)
			}
			if tc.running > 0 && ps < tc.running {
				t.Fatalf("stopped asking while the init ran (%d)", ps)
			}
		})
	}
}
