package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeEventctl stands in for eventctl in the data plane: each run is
// recorded and answered by reply, keyed on the eventctl subcommand mode.
type fakeEventctl struct {
	calls [][]string
	reply func(args []string) (stdout, stderr string, err error)
}

// eventctlArgsOf is what follows "eventctl" in a recorded command line.
func eventctlArgsOf(argv []string) []string {
	if i := slices.Index(argv, "eventctl"); i >= 0 {
		return argv[i+1:]
	}
	return nil
}

// useFakeEventctl points the commands at a Docker data plane in a temp
// dir whose eventctl is f. HOME is a temp dir too: nothing a test runs
// may read or write the real one.
func useFakeEventctl(t *testing.T, f *fakeEventctl) dataPlane {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=0.1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := dataPlane{kind: installDocker, dir: dir, envPath: filepath.Join(dir, ".env")}
	oldLocate, oldRun := locateDataPlane, eventctlRun
	locateDataPlane = func(string) (dataPlane, error) { return p, nil }
	eventctlRun = func(_ time.Duration, _ []string, name string, args ...string) (string, string, error) {
		argv := append([]string{name}, args...)
		f.calls = append(f.calls, argv)
		return f.reply(eventctlArgsOf(argv))
	}
	t.Cleanup(func() { locateDataPlane, eventctlRun = oldLocate, oldRun })
	return p
}

const satelliteReport = `{"schema":1,"eventctl_version":"0.1.24","stream":"DLQ","stream_messages":14672,"last_seq":14672,
"partial":false,"scanned":14672,"matched":14672,"replayable":931,"payload_less":13741,
"oldest_failed_at":"2026-10-20T09:00:00Z","newest_failed_at":"2026-10-23T08:00:00Z",
"ages":{"last_hour":0,"last_day":200,"last_week":14472,"older":0,"unknown":0},
"groups":[{"error_code":"CLICKHOUSE_DATA_ERROR","project_id":"","service":"bulker-worker","message":"code: 243 NOT_ENOUGH_SPACE","count":13741,"unreplayable":"no_payload"},
{"error_code":"CLICKHOUSE_DATA_ERROR","project_id":"p1","service":"bulker-worker","message":"code: 243 NOT_ENOUGH_SPACE","count":931}]%s}`

func reportWith(extra string) string {
	return strings.ReplaceAll(strings.ReplaceAll(satelliteReport, "%s", extra), "\n", "") + "\n"
}

func TestDLQSummary_FlagsPayloadLessRows(t *testing.T) {
	f := &fakeEventctl{reply: func([]string) (string, string, error) {
		return "WARN[0000] The \"X\" variable is not set.\n" + reportWith(""), "", nil
	}}
	useFakeEventctl(t, f)
	nowFunc = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = time.Now })

	var err error
	out := captureStdout(t, func() { err = runDLQ(dlqOptions{}) })
	if err != nil {
		t.Fatalf("runDLQ: %v\n%s", err, out)
	}
	for _, want := range []string{"14,672 entries", "Replayable    931", "13,741", "never replayable", "no (no payload)",
		"CLICKHOUSE_DATA_ERROR  14,672", "user_traits", "3d ago", "origamy dlq replay"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	args := eventctlArgsOf(f.calls[0])
	if !slices.Contains(args, "--summary") || !slices.Contains(args, "--all") || !slices.Contains(args, "--json") || slices.Contains(args, "--show-payload") {
		t.Errorf("eventctl args = %v", args)
	}
	if !slices.Equal(f.calls[0][:2], []string{"docker", "compose"}) || !slices.Contains(f.calls[0], "portal-agent") {
		t.Errorf("command = %v, want docker compose exec in portal-agent", f.calls[0])
	}
}

// Payloads hold personal data: --list alone never asks eventctl for them.
func TestDLQList_PayloadOnlyOnRequest(t *testing.T) {
	entries := `,"entries":[{"seq":7,"error_code":"CLICKHOUSE_DATA_ERROR","project_id":"p1","failed_at":"2026-10-23T08:00:00Z","error_message":"boom"%s}],"entries_omitted":4`
	f := &fakeEventctl{reply: func(args []string) (string, string, error) {
		payload := ""
		if slices.Contains(args, "--show-payload") {
			payload = `,"payload":{"traits":{"email":"ada@example.com"}}`
		}
		return reportWith(strings.Replace(entries, "%s", payload, 1)), "", nil
	}}
	useFakeEventctl(t, f)

	out := captureStdout(t, func() { _ = runDLQ(dlqOptions{list: true, limit: 5, code: "CLICKHOUSE_DATA_ERROR"}) })
	if strings.Contains(out, "ada@example.com") {
		t.Fatalf("payload printed without --show-payload:\n%s", out)
	}
	args := eventctlArgsOf(f.calls[0])
	if slices.Contains(args, "--summary") || !slices.Contains(args, "--max-entries") || args[slices.Index(args, "--max-entries")+1] != "5" ||
		args[slices.Index(args, "--code")+1] != "CLICKHOUSE_DATA_ERROR" {
		t.Errorf("eventctl args = %v", args)
	}
	if !strings.Contains(out, "2026-10-23 08:00:00Z") || !strings.Contains(out, "4 more not shown") {
		t.Errorf("listing:\n%s", out)
	}

	out = captureStdout(t, func() { _ = runDLQ(dlqOptions{list: true, limit: 5, showPayload: true}) })
	if !strings.Contains(out, "ada@example.com") || !strings.Contains(out, "personal data") {
		t.Errorf("--show-payload listing:\n%s", out)
	}
	if !slices.Contains(eventctlArgsOf(f.calls[1]), "--show-payload") {
		t.Errorf("eventctl args = %v", f.calls[1])
	}
}

func dryRunThenReplay(dryRun, replay string) *fakeEventctl {
	return &fakeEventctl{reply: func(args []string) (string, string, error) {
		switch {
		case slices.Contains(args, "--dry-run"):
			return reportWith(dryRun), "", nil
		case slices.Contains(args, "--replay"):
			return reportWith(replay), "", nil
		}
		return "", "unexpected eventctl call", errors.New("exit status 2")
	}}
}

const (
	plannedReplay = `,"replay":{"dry_run":true,"replayed":931,"unreplayable":13741}`
	doneReplay    = `,"replay":{"replayed":931,"deleted":734,"delete_failed":197,"unreplayable":13741,"stream_messages_after":13938,"errors":["seq 12: replayed; warning: failed to delete from DLQ: timeout"]}`
)

func TestDLQReplay_DryRunChangesNothing(t *testing.T) {
	f := dryRunThenReplay(plannedReplay, doneReplay)
	useFakeEventctl(t, f)
	out := captureStdout(t, func() {
		if err := runDLQReplay(dlqOptions{dryRun: true, code: "CLICKHOUSE_DATA_ERROR"}); err != nil {
			t.Errorf("runDLQReplay: %v", err)
		}
	})
	if len(f.calls) != 1 || !slices.Contains(eventctlArgsOf(f.calls[0]), "--dry-run") {
		t.Fatalf("calls = %v, want only the dry run", f.calls)
	}
	if !strings.Contains(out, "Would send    931") || !strings.Contains(out, "origamy dlq replay --code CLICKHOUSE_DATA_ERROR") {
		t.Errorf("output:\n%s", out)
	}
}

func TestDLQReplay_AsksAndHonoursNo(t *testing.T) {
	f := dryRunThenReplay(plannedReplay, doneReplay)
	useFakeEventctl(t, f)
	withAnswers(t, true, "n\n")
	out := captureStdout(t, func() { _ = runDLQReplay(dlqOptions{}) })
	if len(f.calls) != 1 {
		t.Fatalf("replayed after a no: calls = %v", f.calls)
	}
	if !strings.Contains(out, "Replay 931 events now?") || !strings.Contains(out, "Nothing replayed") {
		t.Errorf("output:\n%s", out)
	}
}

func TestDLQReplay_RefusesWithoutATerminal(t *testing.T) {
	f := dryRunThenReplay(plannedReplay, doneReplay)
	useFakeEventctl(t, f)
	withAnswers(t, false, "y\n")
	var err error
	out := captureStdout(t, func() { err = runDLQReplay(dlqOptions{}) })
	if err == nil || len(f.calls) != 1 || !strings.Contains(out, "--yes") {
		t.Fatalf("err=%v calls=%v\n%s", err, f.calls, out)
	}
}

func TestDLQReplay_ReportsWhatStaysParked(t *testing.T) {
	f := dryRunThenReplay(plannedReplay, doneReplay)
	useFakeEventctl(t, f)
	withAnswers(t, true, "y\n")
	var err error
	out := captureStdout(t, func() { err = runDLQReplay(dlqOptions{project: "p1"}) })
	if err != nil {
		t.Fatalf("runDLQReplay: %v\n%s", err, out)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	replay := eventctlArgsOf(f.calls[1])
	if !slices.Contains(replay, "--replay") || slices.Contains(replay, "--dry-run") || replay[slices.Index(replay, "--project")+1] != "p1" {
		t.Errorf("replay args = %v", replay)
	}
	for _, want := range []string{"Replayed      931", "Removed       734", "Still parked  13,938", "delete failed 197",
		"DLQ now       13,938 entries", "within 2 minutes", "failed to delete from DLQ: timeout"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDLQReplay_FailedSendsFailTheCommand(t *testing.T) {
	f := dryRunThenReplay(plannedReplay, `,"replay":{"replayed":900,"deleted":900,"failed":31,"unreplayable":13741}`)
	useFakeEventctl(t, f)
	var err error
	out := captureStdout(t, func() { err = runDLQReplay(dlqOptions{yes: true}) })
	if err == nil || !strings.Contains(out, "31 events could not be sent") {
		t.Errorf("err=%v\n%s", err, out)
	}
}

func TestDLQ_ShowPayloadNeedsList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dlqCmd.SetArgs(nil)
	_ = dlqCmd.Flags().Set("show-payload", "true")
	t.Cleanup(func() { _ = dlqCmd.Flags().Set("show-payload", "false") })
	var err error
	out := captureStdout(t, func() { err = dlqCmd.RunE(dlqCmd, nil) })
	if err == nil || !strings.Contains(out, "--show-payload needs --list") {
		t.Errorf("err=%v\n%s", err, out)
	}
}

func TestEventctlCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, name, argv := dataPlane{kind: installKubernetes}.eventctlCommand("doctor", "--json")
	want := []string{"exec", "-n", "origamy-dp", "deploy/odp-portal-agent", "-c", "portal-agent", "--", "eventctl", "doctor", "--json"}
	if name != "kubectl" || !slices.Equal(argv, want) {
		t.Errorf("kubernetes: %s %v", name, argv)
	}

	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("DATA_PLANE_ID=dp-1\nDP_IMAGE_TAG=0.1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra, name, argv := dataPlane{kind: installDocker, dir: dir, envPath: env}.eventctlCommand("dlq", "--json")
	if name != "docker" || !slices.Contains(extra, "DP_IMAGE_TAG=0.1.24") {
		t.Errorf("docker: %s env=%v", name, extra)
	}
	if !slices.Equal(argv[len(argv)-6:], []string{"exec", "-T", "portal-agent", "eventctl", "dlq", "--json"}) {
		t.Errorf("docker argv = %v", argv)
	}
	if !slices.Contains(argv, "--env-file") || !slices.Contains(argv, "--project-directory") {
		t.Errorf("docker argv = %v, want the project pinned", argv)
	}
}

// What a data plane without eventctl, or one that cannot run it, says, and
// what the operator is told to do.
func TestRunEventctl_ExplainsMissingReport(t *testing.T) {
	cases := []struct {
		name   string
		kind   installKind
		stderr string
		err    error
		want   string
	}{
		{"docker, old image", installDocker, `OCI runtime exec failed: exec failed: unable to start container process: exec: "eventctl": executable file not found in $PATH: unknown`, errors.New("exit status 126"), "origamy upgrade"},
		{"kubernetes, old image", installKubernetes, `error: Internal error occurred: exec: "eventctl": executable file not found in $PATH`, errors.New("exit status 1"), "origamy upgrade"},
		{"busybox sh", installKubernetes, `sh: eventctl: not found`, errors.New("exit status 127"), "origamy upgrade"},
		{"no portal-agent", installKubernetes, `Error from server (NotFound): deployments.apps "odp-portal-agent" not found`, errors.New("exit status 1"), "portalAgent.enabled=true"},
		{"no exec permission", installKubernetes, `Error from server (Forbidden): pods "odp-portal-agent-x" is forbidden: User "u" cannot create resource "pods/exec"`, errors.New("exit status 1"), "pods/exec"},
		{"container down", installDocker, `service "portal-agent" is not running`, errors.New("exit status 1"), "up -d portal-agent"},
		{"anything else", installDocker, "boom", errors.New("exit status 3"), "exit status 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEventctl{reply: func([]string) (string, string, error) { return "", tc.stderr, tc.err }}
			p := useFakeEventctl(t, f)
			p.kind = tc.kind
			err := p.runEventctl(time.Second, &dlqReport{}, "dlq")
			var e *eventctlError
			if !errors.As(err, &e) || !strings.Contains(e.headline+"\n"+e.hint, tc.want) {
				t.Errorf("runEventctl() = %v, want a hint containing %q", err, tc.want)
			}
		})
	}
}

func TestRunEventctl_NewerSchema(t *testing.T) {
	f := &fakeEventctl{reply: func([]string) (string, string, error) { return `{"schema":2}` + "\n", "", nil }}
	p := useFakeEventctl(t, f)
	err := p.runEventctl(time.Second, &dlqReport{}, "dlq")
	var e *eventctlError
	if !errors.As(err, &e) || !strings.Contains(e.headline, "newer than the CLI") {
		t.Errorf("runEventctl() = %v", err)
	}
}

// eventctl ran but could not reach NATS: the error is in the report, and
// the command says so instead of printing an empty summary.
func TestDLQ_ReportError(t *testing.T) {
	f := &fakeEventctl{reply: func([]string) (string, string, error) {
		return `{"schema":1,"error":"connecting to NATS: nats: no servers available for connection"}` + "\n", "", errors.New("exit status 1")
	}}
	useFakeEventctl(t, f)
	var err error
	out := captureStdout(t, func() { err = runDLQ(dlqOptions{}) })
	if err == nil || !strings.Contains(out, "no servers available") || !strings.Contains(out, "origamy doctor") {
		t.Errorf("err=%v\n%s", err, out)
	}
}

func TestCommasAndAgo(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 14672: "14,672", 1234567: "1,234,567", -1500: "-1,500"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
	nowFunc = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = time.Now })
	for in, want := range map[string]string{"2026-10-23T11:59:30Z": "just now", "2026-10-23T11:15:00Z": "45m ago", "2026-10-22T12:00:00Z": "24h ago", "2026-10-13T12:00:00Z": "10d ago", "bad": "?"} {
		if got := ago(in); got != want {
			t.Errorf("ago(%q) = %q, want %q", in, got, want)
		}
	}
}
