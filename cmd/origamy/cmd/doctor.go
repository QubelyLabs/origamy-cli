package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── origamy doctor ───────────────────────────────────────────────────────────
// One read-only pass over what the pipeline depends on, run as `eventctl
// doctor` in the data plane (eventctl.go): ClickHouse disk and system-log
// sizes, the NATS streams (DLQ and retry depth) and consumer lag, a Redis
// PING, and a one-line likely cause. It is the first thing to run when the
// dashboard shows events parked in the DLQ.

// doctorReport is `eventctl doctor --json` (data-plane cmd/eventctl/doctor.go).
type doctorReport struct {
	Schema          int    `json:"schema"`
	EventctlVersion string `json:"eventctl_version"`

	ClickHouse struct {
		Reachable bool   `json:"reachable"`
		Error     string `json:"error"`
		Version   string `json:"version"`
		Disks     []struct {
			Name       string  `json:"name"`
			FreeBytes  uint64  `json:"free_bytes"`
			TotalBytes uint64  `json:"total_bytes"`
			UsedPct    float64 `json:"used_pct"`
		} `json:"disks"`
		SystemLogs []struct {
			Table string `json:"table"`
			Bytes uint64 `json:"bytes"`
		} `json:"system_logs"`
		SystemLogBytes uint64 `json:"system_log_bytes"`
		Databases      []struct {
			Database string `json:"database"`
			Bytes    uint64 `json:"bytes"`
		} `json:"databases"`
	} `json:"clickhouse"`

	NATS struct {
		Reachable bool   `json:"reachable"`
		Error     string `json:"error"`
		Streams   []struct {
			Name      string  `json:"name"`
			Role      string  `json:"role"`
			Messages  uint64  `json:"messages"`
			Bytes     uint64  `json:"bytes"`
			FullPct   float64 `json:"full_pct"`
			Consumers []struct {
				Name       string `json:"name"`
				Pending    uint64 `json:"pending"`
				AckPending int    `json:"ack_pending"`
			} `json:"consumers"`
		} `json:"streams"`
	} `json:"nats"`

	Redis struct {
		Configured bool    `json:"configured"`
		Reachable  bool    `json:"reachable"`
		Error      string  `json:"error"`
		PingMillis float64 `json:"ping_ms"`
		UsedMemory uint64  `json:"used_memory"`
		MaxMemory  uint64  `json:"max_memory"`
	} `json:"redis"`

	Findings    []doctorFinding `json:"findings"`
	LikelyCause string          `json:"likely_cause"`
}

type doctorFinding struct {
	Severity string `json:"severity"` // critical | warning | info
	Check    string `json:"check"`
	Message  string `json:"message"`
}

// doctorTimeout bounds each of eventctl's checks; the exec gets all three.
const doctorTimeout = 30 * time.Second

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check the data plane's pipeline: ClickHouse disk, NATS, Redis, DLQ",
	Long: `Run a read-only health check inside the data plane: ClickHouse disk use and
the size of its system.*_log tables, the depth of every NATS stream (the DLQ
and retry streams included) and consumer lag, a Redis PING, and a one-line
likely cause.

--json prints the report as eventctl returned it, for scripts.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		return runDoctor(asJSON)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	doctorCmd.Flags().Bool("json", false, "Print the raw JSON report")
}

func runDoctor(asJSON bool) error {
	if asJSON {
		// Nothing but the report on stdout, so it can be piped.
		p, err := locateDataPlaneQuiet("origamy doctor")
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := p.runEventctl(4*doctorTimeout, &raw, "doctor", "--json", "--timeout", doctorTimeout.String()); err != nil {
			return failEventctl(err)
		}
		fmt.Println(string(raw))
		return nil
	}

	ui.Title("Origamy data plane — doctor")
	p, err := locateDataPlane("origamy doctor")
	if err != nil {
		return err
	}
	ui.KV("Target", p.target())
	sp := ui.Start("Checking ClickHouse, NATS and Redis")
	var rep doctorReport
	if err := p.runEventctl(4*doctorTimeout, &rep, "doctor", "--json", "--timeout", doctorTimeout.String()); err != nil {
		sp.Fail("Could not run the checks")
		return failEventctl(err)
	}
	sp.Success("Checked")
	printDoctor(rep)
	return nil
}

// locateDataPlaneQuiet is locateDataPlane with its messages on stderr, so
// --json keeps stdout to the report.
func locateDataPlaneQuiet(command string) (dataPlane, error) {
	prev := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = prev }()
	return locateDataPlane(command)
}

func printDoctor(rep doctorReport) {
	fmt.Println()
	ch := rep.ClickHouse
	if !ch.Reachable {
		ui.KV("ClickHouse", ui.Red("unreachable")+ui.Gray("  "+firstLine(ch.Error)))
	} else {
		ui.KV("ClickHouse", ch.Version)
		for _, d := range ch.Disks {
			if d.TotalBytes == 0 {
				continue
			}
			used := fmt.Sprintf("%.0f%% used", d.UsedPct)
			switch {
			case d.UsedPct >= 95:
				used = ui.Red(used)
			case d.UsedPct >= 85:
				used = ui.Yellow(used)
			}
			ui.KV("", fmt.Sprintf("disk %s %s (%s free of %s)", d.Name, used, humanSize(d.FreeBytes), humanSize(d.TotalBytes)))
		}
		logs := fmt.Sprintf("system logs %s", humanSize(ch.SystemLogBytes))
		var top []string
		for i, t := range ch.SystemLogs {
			if i == 3 {
				break
			}
			top = append(top, fmt.Sprintf("%s %s", t.Table, humanSize(t.Bytes)))
		}
		if len(top) > 0 {
			logs += ui.Gray("  (" + strings.Join(top, ", ") + ")")
		}
		ui.KV("", logs)
	}

	n := rep.NATS
	if !n.Reachable {
		ui.KV("NATS", ui.Red("unreachable")+ui.Gray("  "+firstLine(n.Error)))
	} else {
		var dlq, retry uint64
		for _, s := range n.Streams {
			switch s.Role {
			case "dlq":
				dlq += s.Messages
			case "retry":
				retry += s.Messages
			}
		}
		dlqText := commas(int(dlq))
		if dlq > 0 {
			dlqText = ui.Yellow(dlqText)
		}
		ui.KV("NATS", fmt.Sprintf("%d streams · DLQ %s · retry %s", len(n.Streams), dlqText, commas(int(retry))))
		for _, s := range n.Streams {
			var lag []string
			for _, c := range s.Consumers {
				if c.Pending > 0 || c.AckPending > 0 {
					lag = append(lag, fmt.Sprintf("%s %s pending", c.Name, commas(int(c.Pending))))
				}
			}
			if len(lag) > 0 {
				ui.KV("", fmt.Sprintf("%s: %s", s.Name, strings.Join(lag, ", ")))
			}
		}
	}

	r := rep.Redis
	switch {
	case !r.Configured:
		ui.KV("Redis", ui.Gray("not configured for the portal-agent"))
	case !r.Reachable:
		ui.KV("Redis", ui.Red("unreachable")+ui.Gray("  "+firstLine(r.Error)))
	default:
		mem := ""
		if r.UsedMemory > 0 {
			mem = " · " + humanSize(r.UsedMemory)
			if r.MaxMemory > 0 {
				mem += " of " + humanSize(r.MaxMemory)
			}
		}
		ui.KV("Redis", fmt.Sprintf("PONG in %.1f ms%s", r.PingMillis, mem))
	}

	cause := rep.LikelyCause
	if cause == "" {
		cause = "No problems found."
	}
	// The likely cause is the first critical or warning finding; it is
	// printed once, last, and the others above it.
	causeSeverity := ""
	var others []doctorFinding
	for _, f := range rep.Findings {
		if causeSeverity == "" && f.Message == cause {
			causeSeverity = f.Severity
			continue
		}
		others = append(others, f)
	}
	if len(others) > 0 {
		fmt.Println()
		for _, f := range others {
			switch f.Severity {
			case "critical":
				ui.Fail("%s", f.Message)
			case "warning":
				ui.Warn("%s", f.Message)
			default:
				ui.Step("%s", f.Message)
			}
		}
	}
	fmt.Println()
	switch causeSeverity {
	case "":
		ui.Success("Likely cause: %s", cause)
	case "critical":
		ui.Fail("%s %s", ui.Bold("Likely cause:"), cause)
	default:
		ui.Warn("%s %s", ui.Bold("Likely cause:"), cause)
	}
	fmt.Println()
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// humanSize formats bytes in binary units.
func humanSize(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
