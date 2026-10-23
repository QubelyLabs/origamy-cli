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
