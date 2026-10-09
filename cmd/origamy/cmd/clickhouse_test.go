package cmd

import (
	"bufio"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// dsnPassword is what the data plane's driver (clickhouse-go ParseDSN, which
// is net/url underneath) recovers from the DSN the chart builds by
// substituting the password verbatim.
func dsnPassword(pw string) (string, bool) {
	u, err := url.Parse("clickhouse://default:" + pw + "@ch.example.com:9440/events?secure=true")
	if err != nil || u.User == nil || u.Hostname() != "ch.example.com" {
		return "", false
	}
	got, ok := u.User.Password()
	return got, ok
}

func TestClickHousePasswordProblem(t *testing.T) {
	ok := []string{
		"s3cret", "S3cret-Pass_word.v2~", "a!b$c&d'e(f)g*h+i,j;k=l", "with:colon", "with@at", "p@ss:w0rd!",
		"-._~!$&'()*+,;=:@", strings.Repeat("x", 64),
	}
	for _, pw := range ok {
		if msg := clickhousePasswordProblem(pw); msg != "" {
			t.Errorf("%q rejected: %s", pw, msg)
			continue
		}
		// Accepted passwords must survive the driver's URL parsing unchanged.
		if got, parsed := dsnPassword(pw); !parsed || got != pw {
			t.Errorf("%q accepted but the DSN yields %q (parsed=%v)", pw, got, parsed)
		}
	}

	bad := []string{"has space", "slash/", "q?mark", "hash#", "pct%41", "pct%zz", `quote"`, "back\\slash", "less<", "brace{", "pipe|", "caret^", "tick`", "brack[", "tab\t", "ünïcode"}
	for _, pw := range bad {
		if msg := clickhousePasswordProblem(pw); msg == "" {
			t.Errorf("%q accepted", pw)
		}
	}
	// Each rejected character really breaks (or alters) the DSN, so the
	// rule is not stricter than the driver for these.
	for _, pw := range []string{"has space", "slash/", "q?mark", "hash#", "pct%41", "pct%zz", `quote"`, "brace{"} {
		if got, parsed := dsnPassword(pw); parsed && got == pw {
			t.Errorf("%q is rejected, yet the DSN carries it intact", pw)
		}
	}
	if msg := clickhousePasswordProblem("has space"); !strings.Contains(msg, "a space") {
		t.Errorf("space should be named plainly: %s", msg)
	}
}

func TestClickHouseUserProblem(t *testing.T) {
	for _, u := range []string{"default", "origamy_rw", "svc-origamy", "team.ingest", "U1"} {
		if msg := clickhouseUserProblem(u); msg != "" {
			t.Errorf("%q rejected: %s", u, msg)
		}
	}
	for _, u := range []string{"", "a:b", "a@b", "a b", "a/b", "a%b"} {
		if msg := clickhouseUserProblem(u); msg == "" {
			t.Errorf("%q accepted", u)
		}
	}
}

func TestNativePortProblem(t *testing.T) {
	for _, p := range []int{9000, 9440, 19000, 1, 65535} {
		if msg := nativePortProblem(p); msg != "" {
			t.Errorf("%d rejected: %s", p, msg)
		}
	}
	for _, p := range []int{0, -1, 65536, 8123, 8443} {
		if msg := nativePortProblem(p); msg == "" {
			t.Errorf("%d accepted", p)
		}
	}
}

func TestParseClickHouseHost(t *testing.T) {
	cases := []struct {
		in       string
		host     string
		port     int
		problems bool
	}{
		{"clickhouse.mycompany.com", "clickhouse.mycompany.com", 0, false},
		{"  ch.example.com  ", "ch.example.com", 0, false},
		{"ch.example.com:9440", "ch.example.com", 9440, false},
		{"10.0.3.7:9000", "10.0.3.7", 9000, false},
		{"https://abc123.eu-west-1.aws.clickhouse.cloud:9440/", "abc123.eu-west-1.aws.clickhouse.cloud", 9440, false},
		{"clickhouse://ch.example.com/events?secure=true", "ch.example.com", 0, false},
		{"clickhouse.analytics.svc.cluster.local", "clickhouse.analytics.svc.cluster.local", 0, false},
		{"", "", 0, true},
		{"https://", "", 0, true},
		{"bad host", "", 0, true},
		{"ch_example.com", "", 0, true},
		{"-ch.example.com", "", 0, true},
		{"ch.example.com:abc", "", 0, true},
		{"ch.example.com:8443", "", 0, true}, // HTTPS interface, not native
		{"[::1]:9000", "", 0, true},
		{"::1", "", 0, true},
		{"user:pw@ch.example.com", "", 0, true},
	}
	for _, c := range cases {
		host, port, problem := parseClickHouseHost(c.in)
		if (problem != "") != c.problems {
			t.Errorf("%q: problem=%q, want problem=%v", c.in, problem, c.problems)
			continue
		}
		if !c.problems && (host != c.host || port != c.port) {
			t.Errorf("%q = (%q, %d), want (%q, %d)", c.in, host, port, c.host, c.port)
		}
	}
}

func TestIsInClusterHost(t *testing.T) {
	for _, h := range []string{"ch.analytics.svc", "ch.analytics.svc.cluster.local", "CH.Analytics.SVC.cluster.local."} {
		if !isInClusterHost(h) {
			t.Errorf("%q should be in-cluster", h)
		}
	}
	for _, h := range []string{"ch.example.com", "svc.example.com", "10.0.0.1"} {
		if isInClusterHost(h) {
			t.Errorf("%q should not be in-cluster", h)
		}
	}
}

func TestClickHouseSetArgs(t *testing.T) {
	if got, want := clickhouseSetArgs(nil), []string{"--set", "clickhouse.enabled=true"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bundled = %v, want %v", got, want)
	}

	ext := &externalClickHouse{host: "ch.example.com", port: 9440, secure: true, user: "origamy", password: "s3cret"}
	want := []string{
		"--set", "clickhouse.enabled=false",
		"--set-string", "clickhouse.host=ch.example.com",
		"--set", "clickhouse.port=9440",
		"--set", "clickhouse.secure=true",
		"--set-string", "clickhouse.username=origamy",
		"--set", "clickhouse.existingSecret=origamy-clickhouse",
	}
	got := clickhouseSetArgs(ext)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("external = %v, want %v", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "s3cret") {
			t.Fatalf("the password must never reach helm --set: %v", got)
		}
	}

	noPw := &externalClickHouse{host: "10.0.3.7", port: 9000, user: "default"}
	got = clickhouseSetArgs(noPw)
	if strings.Contains(strings.Join(got, " "), "existingSecret") {
		t.Errorf("no password should reference no Secret: %v", got)
	}
	if !strings.Contains(strings.Join(got, " "), "clickhouse.secure=false") {
		t.Errorf("TLS off should be explicit: %v", got)
	}
}

func TestExternalClickHouseDescribe(t *testing.T) {
	if got := (&externalClickHouse{host: "ch.example.com", port: 9440, secure: true}).describe(); got != "external — ch.example.com:9440, TLS" {
		t.Errorf("describe = %q", got)
	}
	if got := (&externalClickHouse{host: "10.0.3.7", port: 9000}).describe(); got != "external — 10.0.3.7:9000" {
		t.Errorf("describe = %q", got)
	}
}

func TestUsesExternalClickHouse(t *testing.T) {
	cases := []struct {
		vals map[string]any
		want bool
	}{
		{map[string]any{}, false},
		{map[string]any{"clickhouse": map[string]any{"enabled": true}}, false},
		{map[string]any{"clickhouse": map[string]any{"host": "ch.example.com"}}, false},
		{map[string]any{"clickhouse": map[string]any{"enabled": false, "host": "ch.example.com"}}, true},
		{map[string]any{"clickhouse": "garbage"}, false},
	}
	for i, c := range cases {
		if got := usesExternalClickHouse(c.vals); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}

func TestExternalClickHouseGate(t *testing.T) {
	// The option is hidden on the pinned chart until a release carries the
	// chart support; edge/non-semver targets are never blocked.
	if !versionLess("0.1.17", minChartExternalClickHouse) || !versionLess("0.1.18", minChartExternalClickHouse) {
		t.Errorf("charts before %s must not offer an external ClickHouse", minChartExternalClickHouse)
	}
	if versionLess(minChartExternalClickHouse, minChartExternalClickHouse) || versionLess("main", minChartExternalClickHouse) {
		t.Errorf("%s itself and non-semver targets must offer it", minChartExternalClickHouse)
	}
}

// withAnswers feeds the shared prompt reader from a string for one test.
func withAnswers(t *testing.T, terminal bool, answers string) {
	t.Helper()
	oldIn, oldTerm := stdin, stdinIsTerminal
	stdin = bufio.NewReader(strings.NewReader(answers))
	stdinIsTerminal = terminal
	t.Cleanup(func() { stdin, stdinIsTerminal = oldIn, oldTerm })
}

func TestPromptExternalClickHouse(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		answers  string
		want     externalClickHouse
	}{
		{
			name:    "pasted managed-service URL, TLS, custom user",
			answers: "https://abc.eu-west-1.aws.clickhouse.cloud:9440/\ny\n\norigamy\np@ss:w0rd!\n",
			want:    externalClickHouse{host: "abc.eu-west-1.aws.clickhouse.cloud", port: 9440, secure: true, user: "origamy", password: "p@ss:w0rd!"},
		},
		{
			name:    "plain host, no TLS, defaults, no password",
			answers: "10.0.3.7\nn\n\n\n\ny\n",
			want:    externalClickHouse{host: "10.0.3.7", port: 9000, user: "default"},
		},
		{
			name:    "TLS default port follows the TLS answer",
			answers: "ch.example.com\n\n\n\ns3cret\n",
			want:    externalClickHouse{host: "ch.example.com", port: 9440, secure: true, user: "default", password: "s3cret"},
		},
		{
			name:     "a person corrects a bad host, port and password",
			terminal: true,
			answers:  "bad host\nch.example.com\ny\n8443\n9441\n\nhas space\ns3cret\n",
			want:     externalClickHouse{host: "ch.example.com", port: 9441, secure: true, user: "default", password: "s3cret"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withAnswers(t, c.terminal, c.answers)
			got := promptExternalClickHouse()
			if *got != c.want {
				t.Errorf("got %+v, want %+v", *got, c.want)
			}
		})
	}
}

func TestPromptClickHouseGate(t *testing.T) {
	// A chart without external support never asks, and consumes no answer.
	withAnswers(t, false, "2\n")
	if ch := promptClickHouse("0.1.17"); ch != nil {
		t.Fatalf("0.1.17 offered an external ClickHouse: %+v", ch)
	}
	if rest, _ := stdin.ReadString('\n'); rest != "2\n" {
		t.Errorf("the gated prompt consumed an answer; left %q", rest)
	}

	withAnswers(t, false, "1\n")
	if ch := promptClickHouse(minChartExternalClickHouse); ch != nil {
		t.Errorf("choosing Embedded returned %+v", ch)
	}

	withAnswers(t, false, "2\nch.example.com\ny\n\n\ns3cret\n")
	if ch := promptClickHouse(minChartExternalClickHouse); ch == nil || ch.host != "ch.example.com" {
		t.Errorf("choosing External returned %+v", ch)
	}
}

// A piped answer that cannot be used stops the run; for the password, the
// value must not be echoed (rejectAnswer prints the answer, rejectSecret does
// not). The prompt exits the process, so it runs in a child.
func TestRejectedPasswordIsNotPrinted(t *testing.T) {
	const secret = "leaky pass#1"
	if os.Getenv("ORIGAMY_TEST_REJECT_SECRET") == "1" {
		withAnswers(t, false, "ch.example.com\ny\n\n\n"+secret+"\n")
		promptExternalClickHouse()
		t.Fatal("an unusable piped password must stop the run")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRejectedPasswordIsNotPrinted$")
	cmd.Env = append(os.Environ(), "ORIGAMY_TEST_REJECT_SECRET=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("want exit status 1, got %v\n%s", err, out)
	}
	if strings.Contains(string(out), "leaky") {
		t.Errorf("the rejected password was printed:\n%s", out)
	}
	if !strings.Contains(string(out), "the ClickHouse password") {
		t.Errorf("the failure should name the question:\n%s", out)
	}
}

// Input that ends before a required answer stops the run instead of
// re-asking forever.
func TestPromptStopsAtEndOfInput(t *testing.T) {
	if os.Getenv("ORIGAMY_TEST_EOF") == "1" {
		withAnswers(t, true, "") // a person pressed Ctrl-D at the host prompt
		promptExternalClickHouse()
		t.Fatal("end of input must stop the run")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPromptStopsAtEndOfInput$")
	cmd.Env = append(os.Environ(), "ORIGAMY_TEST_EOF=1")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the prompt kept re-asking after end of input")
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "input ended") {
		t.Fatalf("want exit 1 with 'input ended', got %v\n%s", err, out)
	}
}
