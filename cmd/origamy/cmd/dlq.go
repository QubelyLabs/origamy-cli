package cmd

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── origamy dlq ──────────────────────────────────────────────────────────────
// What is parked in the data plane's dead-letter queue, and replaying it once
// the cause is fixed. Both run eventctl in the data plane (eventctl.go); the
// CLI never reads NATS itself. Stored payloads are personal data, so they are
// only fetched with --list --show-payload.

// dlqReport is `eventctl dlq --json` (data-plane cmd/eventctl/report.go).
type dlqReport struct {
	Schema          int    `json:"schema"`
	EventctlVersion string `json:"eventctl_version"`
	Error           string `json:"error"`

	Stream         string `json:"stream"`
	StreamMessages uint64 `json:"stream_messages"`
	Partial        bool   `json:"partial"`
	Scanned        int    `json:"scanned"`
	Matched        int    `json:"matched"`
	Replayable     int    `json:"replayable"`
	PayloadLess    int    `json:"payload_less"`

	OldestFailedAt string `json:"oldest_failed_at"`
	NewestFailedAt string `json:"newest_failed_at"`
	Ages           struct {
		LastHour int `json:"last_hour"`
		LastDay  int `json:"last_day"`
		LastWeek int `json:"last_week"`
		Older    int `json:"older"`
		Unknown  int `json:"unknown"`
	} `json:"ages"`
	Groups []dlqGroup `json:"groups"`

	Entries        []dlqEntry `json:"entries"`
	EntriesOmitted int        `json:"entries_omitted"`

	Replay   *dlqReplay `json:"replay"`
	Warnings []string   `json:"warnings"`
}

type dlqGroup struct {
	ErrorCode      string `json:"error_code"`
	ProjectID      string `json:"project_id"`
	Service        string `json:"service"`
	Message        string `json:"message"`
	Count          int    `json:"count"`
	Unreplayable   string `json:"unreplayable"`
	OldestFailedAt string `json:"oldest_failed_at"`
}

type dlqEntry struct {
	Seq             uint64 `json:"seq"`
	ErrorCode       string `json:"error_code"`
	ErrorMessage    string `json:"error_message"`
	Service         string `json:"service"`
	ProjectID       string `json:"project_id"`
	OriginalSubject string `json:"original_subject"`
	RetryCount      string `json:"retry_count"`
	FailedAt        string `json:"failed_at"`
	Unreplayable    string `json:"unreplayable"`
	// Payload is the stored DLQ message, present only with --show-payload.
	Payload rawJSON `json:"payload"`
}

type dlqReplay struct {
	DryRun              bool     `json:"dry_run"`
	Replayed            int      `json:"replayed"`
	Deleted             int      `json:"deleted"`
	Kept                int      `json:"kept"`
	DeleteFailed        int      `json:"delete_failed"`
	Unreplayable        int      `json:"unreplayable"`
	Failed              int      `json:"failed"`
	StreamMessagesAfter *uint64  `json:"stream_messages_after"`
	Errors              []string `json:"errors"`
	ErrorsOmitted       int      `json:"errors_omitted"`
}

// rawJSON keeps a JSON value as it came.
type rawJSON []byte

func (r *rawJSON) UnmarshalJSON(b []byte) error { *r = append((*r)[:0], b...); return nil }

// dlqTimeout bounds one eventctl run: a summary pages through the whole DLQ
// stream, a replay re-publishes and deletes each entry. eventctl stops itself
// at its own --timeout first and reports what it did.
const (
	dlqEventctlTimeout = 10 * time.Minute
	dlqExecTimeout     = dlqEventctlTimeout + time.Minute
)

var dlqCmd = &cobra.Command{
	Use:   "dlq",
	Short: "Show what is parked in the data plane's dead-letter queue",
	Long: `Summarise the data plane's dead-letter queue (DLQ): entries by error code,
project and age, which can be replayed, and which hold no payload at all
(ClickHouse satellite rows, never replayable). Read-only.

--list shows one row per entry. Stored payloads hold personal data and are
left out unless you also pass --show-payload.

Fix the cause first (origamy doctor), then: origamy dlq replay.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		list, _ := cmd.Flags().GetBool("list")
		show, _ := cmd.Flags().GetBool("show-payload")
		limit, _ := cmd.Flags().GetInt("limit")
		code, _ := cmd.Flags().GetString("code")
		project, _ := cmd.Flags().GetString("project")
		if show && !list {
			return fail("--show-payload needs --list.", "Payloads are shown per entry: origamy dlq --list --show-payload [--code …] [--project …] [--limit N].")
		}
		return runDLQ(dlqOptions{list: list, showPayload: show, limit: limit, code: code, project: project})
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var dlqReplayCmd = &cobra.Command{
	Use:   "replay",
	Short: "Re-send parked events once the cause is fixed (dry run, then confirm)",
	Long: `Replay the dead-letter queue: each replayable entry's original event is
re-published to the subject it failed on, and removed from the DLQ once NATS
has stored it. Entries without a payload stay; nothing can replay them.

It always starts with a dry run that changes nothing and shows what would be
sent, then asks before replaying. --dry-run stops after that; --yes replays
without asking (needed when stdin is not a terminal).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dry, _ := cmd.Flags().GetBool("dry-run")
		yes, _ := cmd.Flags().GetBool("yes")
		code, _ := cmd.Flags().GetString("code")
		project, _ := cmd.Flags().GetString("project")
		return runDLQReplay(dlqOptions{code: code, project: project, dryRun: dry, yes: yes})
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	for _, c := range []*cobra.Command{dlqCmd, dlqReplayCmd} {
		c.Flags().String("code", "", "Only entries with these error codes (comma-separated), e.g. CLICKHOUSE_DATA_ERROR")
		c.Flags().String("project", "", "Only entries of these project IDs (comma-separated)")
	}
	dlqCmd.Flags().Bool("list", false, "Show one row per entry (up to --limit)")
	dlqCmd.Flags().Bool("show-payload", false, "With --list: include each entry's stored payload (personal data)")
	dlqCmd.Flags().Int("limit", 50, "With --list: entries to show")
	dlqReplayCmd.Flags().Bool("dry-run", false, "Only show what a replay would do; change nothing")
	dlqReplayCmd.Flags().BoolP("yes", "y", false, "Replay without asking")
	dlqCmd.AddCommand(dlqReplayCmd)
}

type dlqOptions struct {
	list, showPayload bool
	limit             int
	code, project     string
	dryRun, yes       bool
}

// eventctlArgs is the `eventctl dlq` command line for o; extra selects the
// mode (--summary, --dry-run, --replay).
func (o dlqOptions) eventctlArgs(extra ...string) []string {
	args := []string{"dlq", "--all", "--json", "--timeout", dlqEventctlTimeout.String()}
	if o.code != "" {
		args = append(args, "--code", o.code)
	}
	if o.project != "" {
		args = append(args, "--project", o.project)
	}
	return append(args, extra...)
}

func runDLQ(o dlqOptions) error {
	ui.Title("Origamy data plane — dead-letter queue")
	p, err := locateDataPlane("origamy dlq")
	if err != nil {
		return err
	}
	ui.KV("Target", p.target())

	args := o.eventctlArgs("--summary")
	if o.list {
		args = o.eventctlArgs("--max-entries", strconv.Itoa(max(o.limit, 0)))
		if o.showPayload {
			args = append(args, "--show-payload")
		}
	}
	sp := ui.Start("Reading the DLQ")
	var rep dlqReport
	err = p.runEventctl(dlqExecTimeout, &rep, args...)
	if err != nil {
		sp.Fail("Could not read the DLQ")
		return failEventctl(err)
	}
	if rep.Error != "" {
		sp.Fail("Could not read the DLQ")
		return fail("eventctl could not read the DLQ: "+rep.Error, "Check NATS with `origamy doctor`, then rerun.")
	}
	sp.Success("Read %d entries", rep.Scanned)

	printDLQSummary(rep, o)
	if o.list {
		printDLQEntries(rep, o.showPayload)
	}
	printDLQNext(rep, o)
	return nil
}

// printDLQSummary prints what the DLQ holds: totals, then the matched
// entries by error code, project and age, then the largest groups.
func printDLQSummary(rep dlqReport, o dlqOptions) {
	fmt.Println()
	if rep.Stream == "" {
		ui.KV("DLQ", "no DLQ stream on this data plane")
		return
	}
	ui.KV("Stream", fmt.Sprintf("%s — %s entries", rep.Stream, commas(int(rep.StreamMessages))))
	if o.code != "" || o.project != "" {
		ui.KV("Matching", fmt.Sprintf("%s of %s read", commas(rep.Matched), commas(rep.Scanned)))
	}
	if rep.Matched == 0 {
		ui.Success("Nothing parked%s.", filterSuffix(o))
		return
	}
	ui.KV("Replayable", commas(rep.Replayable))
	if rep.PayloadLess > 0 {
		ui.KV("No payload", commas(rep.PayloadLess)+ui.Gray("  — never replayable (see below)"))
	}
	if other := rep.Matched - rep.Replayable - rep.PayloadLess; other > 0 {
		ui.KV("Other", commas(other)+ui.Gray("  — not replayable (malformed payload or no subject)"))
	}
	if rep.OldestFailedAt != "" {
		ui.KV("Oldest", rep.OldestFailedAt+ui.Gray("  ("+ago(rep.OldestFailedAt)+")"))
		ui.KV("Newest", rep.NewestFailedAt+ui.Gray("  ("+ago(rep.NewestFailedAt)+")"))
	}
	a := rep.Ages
	ages := fmt.Sprintf("last hour %s · last day %s · last week %s · older %s", commas(a.LastHour), commas(a.LastDay), commas(a.LastWeek), commas(a.Older))
	if a.Unknown > 0 {
		ages += " · unknown " + commas(a.Unknown)
	}
	ui.KV("By age", ages)
	if rep.Partial {
		ui.Warn("The run stopped before reading every entry; the counts cover only what it read.")
	}
	for _, w := range rep.Warnings {
		ui.Warn("%s", w)
	}

	byCode, byProject := map[string]int{}, map[string]int{}
	for _, g := range rep.Groups {
		byCode[g.ErrorCode] += g.Count
		byProject[orDash(g.ProjectID)] += g.Count
	}
	fmt.Println()
	fmt.Println("  " + ui.Bold("By error code"))
	printCounts(byCode)
	fmt.Println("  " + ui.Bold("By project"))
	printCounts(byProject)

	fmt.Println("  " + ui.Bold("Largest groups"))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "    COUNT\tCODE\tPROJECT\tSERVICE\tREPLAY\tMESSAGE")
	for i, g := range rep.Groups {
		if i == 10 {
			_, _ = fmt.Fprintf(tw, "    …\t%d more groups\t\t\t\t\n", len(rep.Groups)-10)
			break
		}
		_, _ = fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\t%s\n", commas(g.Count), orDash(g.ErrorCode), orDash(g.ProjectID), orDash(g.Service), replayLabel(g.Unreplayable), truncate(g.Message, 70))
	}
	_ = tw.Flush()

	if rep.PayloadLess > 0 {
		fmt.Println()
		ui.Warn("%s entries hold no payload.", commas(rep.PayloadLess))
		ui.Detail("From the bulker these are user_traits rows derived from an identify; nothing can replay them.")
		ui.Detail("Replaying the identify events rebuilds them. They expire with the DLQ stream's retention (7 days).")
	}
}

// printDLQEntries lists the entries the report carries.
func printDLQEntries(rep dlqReport, showPayload bool) {
	if len(rep.Entries) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("  " + ui.Bold("Entries"))
	if showPayload {
		ui.Warn("Payloads below are personal data from your data plane. Do not paste them into tickets or chats.")
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "    SEQ\tFAILED AT\tCODE\tPROJECT\tSERVICE\tREPLAY\tMESSAGE")
	for _, e := range rep.Entries {
		_, _ = fmt.Fprintf(tw, "    %d\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Seq, orDash(shortTime(e.FailedAt)), orDash(e.ErrorCode), orDash(e.ProjectID), orDash(e.Service), replayLabel(e.Unreplayable), truncate(e.ErrorMessage, 60))
		if showPayload && len(e.Payload) > 0 {
			_ = tw.Flush()
			fmt.Printf("      %s\n", ui.Gray(truncate(string(e.Payload), 2000)))
		}
	}
	_ = tw.Flush()
	if rep.EntriesOmitted > 0 {
		ui.Detail("%s more not shown (--limit).", commas(rep.EntriesOmitted))
	}
}

func printDLQNext(rep dlqReport, o dlqOptions) {
	if rep.Replayable == 0 {
		fmt.Println()
		return
	}
	fmt.Println()
	ui.Step("Fix the cause first: %s", ui.Cyan("origamy doctor"))
	ui.Step("Then replay (dry run first, asks before sending): %s", ui.Cyan("origamy dlq replay"+filterFlags(o)))
	fmt.Println()
}

func runDLQReplay(o dlqOptions) error {
	ui.Title("Origamy data plane — replay the dead-letter queue")
	p, err := locateDataPlane("origamy dlq replay")
	if err != nil {
		return err
	}
	ui.KV("Target", p.target())

	sp := ui.Start("Dry run: checking what would be replayed")
	var plan dlqReport
	if err := p.runEventctl(dlqExecTimeout, &plan, o.eventctlArgs("--summary", "--dry-run")...); err != nil {
		sp.Fail("Dry run failed")
		return failEventctl(err)
	}
	if plan.Error != "" || plan.Replay == nil {
		sp.Fail("Dry run failed")
		return fail("eventctl could not read the DLQ: "+orDash(plan.Error), "Check NATS with `origamy doctor`, then rerun.")
	}
	sp.Success("Dry run done — nothing was changed")

	fmt.Println()
	ui.KV("Matching", commas(plan.Matched)+filterSuffix(o))
	ui.KV("Would send", commas(plan.Replay.Replayed))
	ui.KV("Stay parked", fmt.Sprintf("%s (no payload %s)", commas(plan.Replay.Unreplayable), commas(plan.PayloadLess)))
	for _, g := range plan.Groups {
		if g.Unreplayable == "" {
			ui.Detail("%s × %s %s (%s)", commas(g.Count), orDash(g.ErrorCode), orDash(g.ProjectID), truncate(g.Message, 60))
		}
	}
	if plan.Replay.Replayed == 0 {
		fmt.Println()
		ui.Success("Nothing to replay.")
		fmt.Println()
		return nil
	}
	if o.dryRun {
		fmt.Println()
		ui.Step("Replay for real: %s", ui.Cyan("origamy dlq replay"+filterFlags(o)))
		fmt.Println()
		return nil
	}

	fmt.Println()
	ui.Warn("Fix what parked these events first (origamy doctor): a replay into the same failure parks them again.")
	if !o.yes {
		if !stdinIsTerminal {
			return fail("Not replaying: stdin is not a terminal, so there is no one to confirm.",
				"Rerun with --yes to replay without asking.")
		}
		if !promptYesNo(fmt.Sprintf("Replay %s events now?", commas(plan.Replay.Replayed)), false) {
			fmt.Println()
			ui.Step("Nothing replayed.")
			fmt.Println()
			return nil
		}
	}

	sp = ui.Start("Replaying %s events", commas(plan.Replay.Replayed))
	var rep dlqReport
	err = p.runEventctl(dlqExecTimeout, &rep, o.eventctlArgs("--summary", "--replay")...)
	if err != nil {
		sp.Fail("Replay did not report back")
		_ = failEventctl(err)
		return fail("The replay may have partly run.", "Run `origamy dlq` to see what is still parked before replaying again.")
	}
	if rep.Replay == nil {
		sp.Fail("Replay failed")
		return fail("eventctl could not replay: "+orDash(rep.Error), "Run `origamy dlq` to see what is still parked.")
	}
	r := rep.Replay
	if r.Failed > 0 || rep.Error != "" {
		sp.Warn("Replayed %s events, %s failed", commas(r.Replayed), commas(r.Failed))
	} else {
		sp.Success("Replayed %s events", commas(r.Replayed))
	}
	printReplayOutcome(rep)
	if r.Failed > 0 || rep.Error != "" {
		return errSilent
	}
	return nil
}

// printReplayOutcome says what was sent, what left the DLQ and what is still
// parked, and why.
func printReplayOutcome(rep dlqReport) {
	r := rep.Replay
	fmt.Println()
	ui.KV("Replayed", commas(r.Replayed))
	ui.KV("Removed", commas(r.Deleted)+ui.Gray("  from the DLQ"))
	parked := r.DeleteFailed + r.Unreplayable + r.Failed + r.Kept
	ui.KV("Still parked", fmt.Sprintf("%s  %s", commas(parked),
		ui.Gray(fmt.Sprintf("(delete failed %s · unreplayable %s · send failed %s)", commas(r.DeleteFailed), commas(r.Unreplayable), commas(r.Failed)))))
	if r.StreamMessagesAfter != nil {
		ui.KV("DLQ now", commas(int(*r.StreamMessagesAfter))+" entries")
	}
	if rep.Partial {
		ui.Warn("The run stopped before reading every entry (time limit). Rerun `origamy dlq replay` for the rest.")
	}
	if rep.Error != "" {
		ui.Fail("%s", rep.Error)
	}
	if r.DeleteFailed > 0 {
		fmt.Println()
		ui.Warn("%s events were re-sent but their DLQ copies could not be removed.", commas(r.DeleteFailed))
		ui.Detail("A rerun within 2 minutes removes them without sending twice (NATS deduplicates the replay);")
		ui.Detail("after that, a rerun sends them again.")
	}
	if r.Failed > 0 {
		fmt.Println()
		ui.Warn("%s events could not be sent and stay parked. Check `origamy doctor`.", commas(r.Failed))
	}
	for _, e := range r.Errors {
		ui.Detail("%s", e)
	}
	if r.ErrorsOmitted > 0 {
		ui.Detail("… and %d more", r.ErrorsOmitted)
	}
	fmt.Println()
}

// replayLabel is a group's or entry's replayability in a table.
func replayLabel(blocker string) string {
	switch blocker {
	case "":
		return "yes"
	case "no_payload":
		return "no (no payload)"
	case "non_json_payload":
		return "no (malformed)"
	case "no_subject", "wildcard_subject":
		return "no (no subject)"
	}
	return "no (" + blocker + ")"
}

func printCounts(m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, k := range keys {
		_, _ = fmt.Fprintf(tw, "    %s\t%s\n", k, commas(m[k]))
	}
	_ = tw.Flush()
}

func filterFlags(o dlqOptions) string {
	s := ""
	if o.code != "" {
		s += " --code " + o.code
	}
	if o.project != "" {
		s += " --project " + o.project
	}
	return s
}

func filterSuffix(o dlqOptions) string {
	var parts []string
	if o.code != "" {
		parts = append(parts, "code "+o.code)
	}
	if o.project != "" {
		parts = append(parts, "project "+o.project)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// commas formats n with thousands separators.
func commas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// shortTime is an RFC 3339 time to the second, in UTC.
func shortTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

// nowFunc is the clock ago reads; tests pin it.
var nowFunc = time.Now

// ago says how long before now an RFC 3339 time was.
func ago(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "?"
	}
	d := nowFunc().Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
