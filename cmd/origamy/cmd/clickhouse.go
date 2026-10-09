package cmd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── External ClickHouse (Kubernetes) ─────────────────────────────────────────
//
// By default the chart runs ClickHouse as a bundled StatefulSet. From
// minChartExternalClickHouse on it can instead connect to the operator's own
// server: it reads the password from a Secret the CLI pre-creates, builds the
// services' DSN from it, runs the schema Job against that host and opens
// egress to it. Older charts build a password-less DSN, never create the
// schema there and block the port, so the option is only offered from there.

// clickhouseSecretName is the Secret holding the external ClickHouse password
// (key clickhouse-password), applied over kubectl's stdin like the token.
const clickhouseSecretName = "origamy-clickhouse"

// externalClickHouse is the operator's own ClickHouse server.
type externalClickHouse struct {
	host     string
	port     int
	secure   bool // TLS on the native port
	user     string
	password string // empty: connect without one
}

// Native-protocol defaults; 8123/8443 are the HTTP(S) interface, which the
// data plane's driver cannot speak.
const (
	clickhouseNativePort    = 9000
	clickhouseNativeTLSPort = 9440
)

// dsnSafePunct is the punctuation a password may contain. The chart embeds
// the password verbatim in the services' connection URL (through a $(VAR)
// reference, so it never lands in a pod spec), and Go's URL parser accepts
// exactly ASCII letters, digits and these in the userinfo part. '%' is out:
// the parser would decode it while the schema Job uses the raw value.
const dsnSafePunct = "-._~!$&'()*+,;=:@"

// clickhousePasswordProblem explains why a password cannot be used, or
// returns "" when it can.
func clickhousePasswordProblem(pw string) string {
	for _, r := range pw {
		if !isASCIIAlnum(r) && !strings.ContainsRune(dsnSafePunct, r) {
			what := fmt.Sprintf("%q", r)
			if r == ' ' {
				what = "a space"
			}
			return fmt.Sprintf("The password contains %s, which the data plane cannot carry in its ClickHouse connection URL. Use letters, digits and %s only (for example, set a new password for this user).", what, dsnSafePunct)
		}
	}
	return ""
}

// clickhouseUserProblem explains why a user name cannot be used, or "".
// The name lands in the same URL, unescaped, so keep it to a plain charset.
func clickhouseUserProblem(user string) string {
	if user == "" {
		return "Enter a user name."
	}
	for _, r := range user {
		if !isASCIIAlnum(r) && r != '-' && r != '_' && r != '.' {
			return fmt.Sprintf("The user name contains %q. Use letters, digits, '-', '_' and '.' only.", r)
		}
	}
	return ""
}

// nativePortProblem explains why a port cannot be the native-protocol port,
// or returns "".
func nativePortProblem(port int) string {
	switch {
	case port < 1 || port > 65535:
		return "Enter a port between 1 and 65535."
	case port == 8123 || port == 8443:
		return fmt.Sprintf("%d is ClickHouse's HTTP interface; the data plane needs the native protocol port (usually %d, or %d with TLS).", port, clickhouseNativePort, clickhouseNativeTLSPort)
	}
	return ""
}

// parseClickHouseHost accepts a bare host, host:port, or a pasted URL such as
// https://abc.clickhouse.cloud:9440, and returns the host plus the port it
// named (0 when none). problem is "" on success.
func parseClickHouseHost(in string) (host string, port int, problem string) {
	s := strings.TrimSpace(in)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "", 0, "Enter the ClickHouse host name, e.g. clickhouse.mycompany.com."
	}
	if strings.Contains(s, ":") {
		h, p, err := net.SplitHostPort(s)
		if err != nil || strings.Contains(h, ":") {
			return "", 0, "Enter a host name or IPv4 address, optionally with :port (IPv6 addresses are not supported)."
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, fmt.Sprintf("%q is not a port number.", p)
		}
		if msg := nativePortProblem(n); msg != "" {
			return "", 0, msg
		}
		s, port = h, n
	}
	for _, r := range s {
		if !isASCIIAlnum(r) && r != '.' && r != '-' {
			return "", 0, fmt.Sprintf("The host contains %q. Enter a host name or IPv4 address, e.g. clickhouse.mycompany.com.", r)
		}
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") || strings.HasSuffix(s, "-") {
		return "", 0, "Enter a host name or IPv4 address, e.g. clickhouse.mycompany.com."
	}
	return s, port, ""
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isInClusterHost reports whether host is a Service DNS name of this cluster.
// The chart's egress rule is an ipBlock, which some CNIs (e.g. Cilium) never
// match for in-cluster pods.
func isInClusterHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	return strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local")
}

// clickhouseSetArgs returns the helm flags selecting the bundled ClickHouse
// (ch == nil) or the operator's own. Host and user go through --set-string so
// a numeric-looking value stays a string; the password never travels here —
// only the name of the Secret holding it.
func clickhouseSetArgs(ch *externalClickHouse) []string {
	if ch == nil {
		return []string{"--set", "clickhouse.enabled=true"}
	}
	args := []string{
		"--set", "clickhouse.enabled=false",
		"--set-string", "clickhouse.host=" + ch.host,
		"--set", "clickhouse.port=" + strconv.Itoa(ch.port),
		"--set", "clickhouse.secure=" + strconv.FormatBool(ch.secure),
		"--set-string", "clickhouse.username=" + ch.user,
	}
	if ch.password != "" {
		args = append(args, "--set", "clickhouse.existingSecret="+clickhouseSecretName)
	}
	return args
}

// describe is the one-line summary shown after install.
func (ch *externalClickHouse) describe() string {
	s := fmt.Sprintf("external — %s:%d", ch.host, ch.port)
	if ch.secure {
		s += ", TLS"
	}
	return s
}

// usesExternalClickHouse reports whether a release's user-supplied values
// (helm get values) point it at an external ClickHouse.
func usesExternalClickHouse(vals map[string]any) bool {
	ch, _ := vals["clickhouse"].(map[string]any)
	if ch == nil {
		return false
	}
	enabled, set := ch["enabled"].(bool)
	return set && !enabled
}

// promptClickHouse asks where ClickHouse runs. It returns nil for the bundled
// StatefulSet, which is all a chart older than minChartExternalClickHouse
// can deploy.
func promptClickHouse(chartVer string) *externalClickHouse {
	ui.Title("ClickHouse")
	if versionLess(chartVer, minChartExternalClickHouse) {
		ui.Step("Runs inside the cluster as part of the data plane. Connecting your own ClickHouse needs release %s or newer (--version).", minChartExternalClickHouse)
		return nil
	}
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("1"), ui.Bold(fmt.Sprintf("%-9s", "Embedded")), ui.Gray("runs inside the cluster (easiest)"))
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("2"), ui.Bold(fmt.Sprintf("%-9s", "External")), ui.Gray("connect to your own ClickHouse"))
	if promptChoice("Choose 1-2", 1, 2, 1) == 1 {
		return nil
	}
	return promptExternalClickHouse()
}

// promptExternalClickHouse collects the connection details of the operator's
// ClickHouse. Nothing is contacted from here: the server may only be reachable
// from inside the cluster, so the chart's schema Job is the first connection.
func promptExternalClickHouse() *externalClickHouse {
	ch := &externalClickHouse{}
	ui.Step("The data plane uses ClickHouse's native protocol and creates the events database and tables there during install, so the user needs CREATE DATABASE/TABLE and INSERT/SELECT on it.")

	var namedPort int
	for {
		ans := promptRequired("ClickHouse host (e.g. clickhouse.mycompany.com)", "the ClickHouse host")
		host, port, problem := parseClickHouseHost(ans)
		if problem == "" {
			ch.host, namedPort = host, port
			break
		}
		rejectAnswer(ans)
		ui.Warn("%s", problem)
	}
	if isInClusterHost(ch.host) {
		ui.Warn("%s is a Service in this cluster. If its namespace has its own NetworkPolicy or your CNI is Cilium, allow egress with networkPolicy.extraEgress after install (origamy upgrade --set …).", ch.host)
	}

	ch.secure = promptYesNo("Does it require TLS on the native port? ClickHouse Cloud and most managed services do.", true)

	defPort := clickhouseNativePort
	if ch.secure {
		defPort = clickhouseNativeTLSPort
	}
	if namedPort != 0 {
		defPort = namedPort
	}
	for {
		ans := promptString(fmt.Sprintf("Native protocol port [%d]", defPort))
		if ans == "" {
			ch.port = defPort
			break
		}
		n, err := strconv.Atoi(ans)
		problem := "Enter a port between 1 and 65535."
		if err == nil {
			problem = nativePortProblem(n)
		}
		if problem == "" {
			ch.port = n
			break
		}
		rejectAnswer(ans)
		ui.Warn("%s", problem)
	}

	for {
		ans := promptString("ClickHouse user [default]")
		if ans == "" {
			ans = "default"
		}
		if problem := clickhouseUserProblem(ans); problem != "" {
			rejectAnswer(ans)
			ui.Warn("%s", problem)
			continue
		}
		ch.user = ans
		break
	}

	for {
		pw := promptSecret("ClickHouse password (input hidden)", "the ClickHouse password")
		if pw == "" {
			if promptYesNo("Connect without a password?", false) {
				break
			}
			rejectSecret("the ClickHouse password")
			continue
		}
		if problem := clickhousePasswordProblem(pw); problem != "" {
			rejectSecret("the ClickHouse password")
			ui.Warn("%s", problem)
			continue
		}
		ch.password = pw
		break
	}
	return ch
}

// promptRequired is promptString for a question that must be answered: end
// of input (Ctrl-D, or piped answers running out) stops the run instead of
// re-asking forever.
func promptRequired(label, what string) string {
	fmt.Printf("  %s: ", label)
	line, err := stdin.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		exitNoAnswer(what)
	}
	return strings.TrimSpace(line)
}

// promptSecret reads one answer without echoing it when a person is typing.
// Piped answers (and anything already typed ahead into the shared reader) are
// read as plain lines. End of input stops the run.
func promptSecret(label, what string) string {
	fmt.Printf("  %s: ", label)
	if stdinIsTerminal && stdin.Buffered() == 0 {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			exitNoAnswer(what)
		}
		return strings.TrimRight(string(b), "\r\n")
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		exitNoAnswer(what)
	}
	return strings.TrimRight(line, "\r\n")
}

// exitNoAnswer stops the run when input ended before a required answer. The
// prompts run before the token is redeemed, so nothing has been changed.
func exitNoAnswer(what string) {
	fmt.Println()
	ui.Fail("No answer for %s (input ended).", what)
	ui.Detail("Nothing has been enrolled or installed.")
	fmt.Println()
	os.Exit(1)
}

// rejectSecret is rejectAnswer for a hidden answer: a non-interactive run
// stops without printing the value.
func rejectSecret(what string) {
	if stdinIsTerminal {
		return
	}
	fmt.Println()
	ui.Fail("Unusable answer on a non-interactive stdin for %s.", what)
	ui.Detail("Check the piped answers. Nothing has been enrolled or installed.")
	fmt.Println()
	os.Exit(1)
}
