package cmd

import (
	"slices"
	"strings"
	"testing"
)

const diskFullDoctor = `{"schema":1,"eventctl_version":"0.1.24","clickhouse":{"reachable":true,"version":"25.3.14.14",
"disks":[{"name":"default","free_bytes":107374182,"total_bytes":10737418240,"used_pct":99}],
"system_logs":[{"table":"trace_log","bytes":8912896000},{"table":"text_log","bytes":1073741824}],"system_log_bytes":9986637824},
"nats":{"reachable":true,"streams":[{"name":"DLQ","role":"dlq","messages":14672},{"name":"Ingest","messages":3,"consumers":[{"name":"bulker","pending":3}]},{"name":"Retry","role":"retry","messages":0}]},
"redis":{"configured":true,"reachable":true,"ping_ms":0.4},
"findings":[{"severity":"critical","check":"clickhouse","message":"ClickHouse disk \"default\" is 99% full (102.4 MB free of 10.0 GB); system logs hold 9.3 GB"},
{"severity":"warning","check":"nats","message":"14672 events are parked in the DLQ"}],
"likely_cause":"ClickHouse disk \"default\" is 99% full (102.4 MB free of 10.0 GB); system logs hold 9.3 GB"}`

func TestDoctor_RendersReport(t *testing.T) {
	f := &fakeEventctl{reply: func([]string) (string, string, error) {
		return strings.ReplaceAll(diskFullDoctor, "\n", "") + "\n", "", nil
	}}
	useFakeEventctl(t, f)
	var err error
	out := captureStdout(t, func() { err = runDoctor(false) })
	if err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, out)
	}
	for _, want := range []string{"25.3.14.14", "disk default 99% used", "system logs 9.3 GiB", "trace_log 8.3 GiB",
		"DLQ 14,672", "Ingest: bulker 3 pending", "PONG in 0.4 ms", "14672 events are parked", "Likely cause: ClickHouse disk"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "99% full") != 1 {
		t.Errorf("the likely cause should be printed once:\n%s", out)
	}
	if args := eventctlArgsOf(f.calls[0]); !slices.Equal(args[:2], []string{"doctor", "--json"}) {
		t.Errorf("eventctl args = %v", args)
	}
}

func TestDoctor_Unreachable(t *testing.T) {
	f := &fakeEventctl{reply: func([]string) (string, string, error) {
		return `{"schema":1,"clickhouse":{"error":"dial tcp: connection refused"},"nats":{"error":"nats: authorization violation"},` +
			`"redis":{"configured":false},"findings":[{"severity":"critical","check":"clickhouse","message":"ClickHouse is unreachable"}],"likely_cause":"ClickHouse is unreachable"}` + "\n", "", nil
	}}
	useFakeEventctl(t, f)
	out := captureStdout(t, func() { _ = runDoctor(false) })
	for _, want := range []string{"unreachable  dial tcp: connection refused", "nats: authorization violation", "not configured", "Likely cause: ClickHouse is unreachable"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// The 2026-11-10 report from a 0.1.28 plane: its retry stream held 22
// messages, every one already retried, and doctor said "retry 22" and
// "22 events are waiting in the retry stream". The stream keeps a week of
// retries, so only what its consumers have yet to settle is waiting.
func TestDoctor_RetryCountsWhatStillWaits(t *testing.T) {
	report := func(consumers string) string {
		return `{"schema":1,"eventctl_version":"0.1.28","clickhouse":{"reachable":true,"version":"25.3.14.14"},` +
			`"nats":{"reachable":true,"streams":[{"name":"DLQ","role":"dlq","messages":0},` +
			`{"name":"Retry","role":"retry","messages":22` + consumers + `}]},` +
			`"redis":{"configured":true,"reachable":true,"ping_ms":8.9},` +
			`"findings":[{"severity":"warning","check":"clickhouse","message":"ClickHouse returned MEMORY_LIMIT_EXCEEDED (241) 23 times since it started"},` +
			`{"severity":"info","check":"nats","message":"22 events are waiting in the retry stream"}],` +
			`"likely_cause":"ClickHouse returned MEMORY_LIMIT_EXCEEDED (241) 23 times since it started"}` + "\n"
	}
	render := func(consumers string) string {
		t.Helper()
		useFakeEventctl(t, &fakeEventctl{reply: func([]string) (string, string, error) { return report(consumers), "", nil }})
		var err error
		out := captureStdout(t, func() { err = runDoctor(false) })
		if err != nil {
			t.Fatalf("runDoctor: %v\n%s", err, out)
		}
		return out
	}

	settled := render(`,"consumers":[{"name":"bulker-retry","pending":0,"ack_pending":0},{"name":"identity-retry","pending":0,"ack_pending":0}]`)
	if !strings.Contains(settled, "retry 0") || strings.Contains(settled, "waiting") {
		t.Errorf("every retry was settled, want retry 0 and no waiting line:\n%s", settled)
	}
	if !strings.Contains(settled, "Likely cause: ClickHouse returned MEMORY_LIMIT_EXCEEDED") {
		t.Errorf("the likely cause is gone:\n%s", settled)
	}

	// 3 not handed out yet, 2 held until they are due.
	due := render(`,"consumers":[{"name":"bulker-retry","pending":3,"ack_pending":0},{"name":"identity-retry","pending":0,"ack_pending":2}]`)
	if !strings.Contains(due, "retry 5") || !strings.Contains(due, "5 events are waiting to be retried") || strings.Contains(due, "22 events") {
		t.Errorf("want the 5 unsettled retries, not the stream's 22 messages:\n%s", due)
	}

	// Nobody consumes the stream: all of it waits.
	if orphaned := render(""); !strings.Contains(orphaned, "retry 22") || !strings.Contains(orphaned, "22 events are waiting to be retried") {
		t.Errorf("want all 22 waiting when the stream has no consumer:\n%s", orphaned)
	}
}

func TestDoctor_JSONIsTheReportAlone(t *testing.T) {
	f := &fakeEventctl{reply: func([]string) (string, string, error) {
		return "compose warning\n" + strings.ReplaceAll(diskFullDoctor, "\n", "") + "\n", "", nil
	}}
	useFakeEventctl(t, f)
	out := captureStdout(t, func() { _ = runDoctor(true) })
	if !strings.HasPrefix(out, `{"schema":1`) || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Errorf("--json output = %q", out)
	}
}

func TestHumanSize(t *testing.T) {
	for b, want := range map[uint64]string{512: "512 B", 2048: "2.0 KiB", 10737418240: "10.0 GiB"} {
		if got := humanSize(b); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", b, got, want)
		}
	}
}
