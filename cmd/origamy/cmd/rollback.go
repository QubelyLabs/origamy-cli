package cmd

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Roll back the data plane to a previous version",
	Long: `Roll back to a previous Helm revision (default: the one immediately before
the current). Your data is preserved. List revisions with 'origamy status' or
'helm history odp -n origamy-dp', then target one with --to.

The revision's images must write the events table ClickHouse holds: a
rollback across the storage reset (to a revision before ` + minImageForJSONSchema + ` while
ClickHouse holds the new table with JSON payload columns, or the other way
round) is refused before anything changes, and so is one while an upgrade's
storage reset is unfinished (ConfigMap ` + resetMarkerConfigMap + `). Once rolled
back, the rollback checks that ClickHouse holds the events table the images
that run write, and fails when it does not.`,
	Example: `  origamy rollback           # to the previous revision
  origamy rollback --to 3`,
	RunE: func(cmd *cobra.Command, args []string) error {
		to, _ := cmd.Flags().GetInt("to")
		return runRollback(to)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rollbackCmd.Flags().Int("to", 0, "Target Helm revision (default: the previous one)")
}

func runRollback(to int) error {
	ui.Title("Origamy data plane — rollback")
	switch {
	case hasKubernetes() && releaseInstalled():
		return rollbackKubernetes(to)
	case hasDocker():
		return fail("Docker installs don't keep a revision history.",
			"Docker tracks image tags, not Helm releases. Move back with: origamy upgrade --version <older>")
	default:
		return fail("No existing Origamy data plane found on this machine.",
			"There is nothing to roll back.")
	}
}

func rollbackKubernetes(to int) error {
	// What the revision runs is checked against what ClickHouse holds before
	// anything changes: a rollback never resets the events table, so one to
	// images that cannot write it would leave events accepted and dropped at
	// insert (and, across the storage reset, ClickHouse on a release that
	// predates its table).
	rev, err := rollbackTarget(to)
	switch {
	case errors.Is(err, errNoHistory):
		return fail(fmt.Sprintf("The release has no revision %d.", rev.Revision),
			"See available revisions with: helm history "+release+" -n "+namespace)
	case err != nil:
		return fail("Could not read the release history.", err.Error())
	}
	label := fmt.Sprintf("revision %d (%s)", rev.Revision, rev.Chart)
	if problem, hint := rollbackProblem(rev); problem != "" {
		return fail(problem, hint+" Nothing has been changed.")
	}

	args := []string{"rollback", release}
	if to > 0 {
		args = append(args, fmt.Sprintf("%d", to))
	}
	args = append(args, "-n", namespace, "--wait")

	sp := ui.Start("Rolling back to %s", label)
	if out, err := kubeRun("helm", args...); err != nil {
		sp.Fail("Rollback failed")
		ui.Detail("See available revisions with: helm history %s -n %s", release, namespace)
		return diagnose(out)
	}
	sp.Success("Rolled back to %s", label)

	// Re-pull for the rolled-back templates (Deployments only — data is safe).
	_, _ = kubeRun("kubectl", "rollout", "restart", "deployment", "-n", namespace)

	external := false
	if vals, err := releaseValues(); err == nil {
		external = usesExternalClickHouse(vals)
	}
	cur, curErr := installedRelease()
	if c := kubeEventsCheck(external); c.why != "" {
		recheck := "origamy upgrade"
		if curErr == nil {
			recheck = kubeUpgradeCommand(chartVersion(cur.Chart), "stable")
		}
		return kubeCheckFail("Rolled back to "+label, recheck, c, external, false)
	}

	lines := []string{}
	if curErr == nil {
		lines = append(lines,
			ui.Gray("Now on    ")+ui.Bold(cur.Chart),
			ui.Gray("Revision  ")+cur.Revision,
			"")
	}
	lines = append(lines, ui.Green("Your data was preserved."))
	ui.Box("Rolled back", lines)
	return nil
}

// rollbackProblem says why rolling back to rev is refused (problem and a
// hint), "" when it is not: an unfinished storage reset, a revision on
// another ClickHouse (bundled or external) than the release now, one whose
// data-plane images do not write one known events table, or write another
// than the one ClickHouse holds (which is asked; one that cannot be is a
// refusal too).
func rollbackProblem(rev releaseRevision) (problem, hint string) {
	label := fmt.Sprintf("revision %d (%s)", rev.Revision, rev.Chart)
	m, err := readResetMarker()
	if err != nil {
		return "Could not read whether an upgrade left a storage reset unfinished.", err.Error()
	}
	if m.present {
		return "An upgrade began the storage reset and did not finish it (ConfigMap " + resetMarkerConfigMap + " in " + namespace + "), so a rollback could leave the events table half-way.",
			"Finish it first: " + m.command() + "."
	}
	nowVals, err := releaseValues()
	if err != nil {
		return "Could not read the current release values.", err.Error()
	}
	revVals, err := revisionValues(rev.Revision)
	if err != nil {
		return "Could not read the values of " + label + ".", err.Error()
	}
	external := usesExternalClickHouse(nowVals)
	if usesExternalClickHouse(revVals) != external {
		return "Rolling back to " + label + " would also move the data plane between the bundled ClickHouse and your external one, whose events tables this rollback cannot check together.",
			"Roll back with helm yourself if that is what you mean: helm rollback " + release + " " + strconv.Itoa(rev.Revision) + " -n " + namespace + "."
	}
	manifest, err := revisionManifest(rev.Revision)
	if err != nil {
		return "Could not read what " + label + " runs.", err.Error()
	}
	gen, why := manifestGeneration(manifest)
	if why != "" {
		return "Rolling back to " + label + " is refused: " + why + ".", "Pick another revision (helm history " + release + " -n " + namespace + "), or move forward with `origamy upgrade`."
	}
	live := askKubeClickHouse(external)
	switch {
	case live.gen == "":
		return "ClickHouse could not be asked which events table it holds, so this rollback cannot tell whether " + label + " can write it.",
			outputHint(live.out) + "Check: kubectl get pods -n " + namespace + "\nOnce it answers, retry."
	case live.gen == liveOther:
		return fmt.Sprintf("ClickHouse's %s has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, which no release writes.", eventsTable, live.typ),
			"Check what created it, and drop it yourself if it should be recreated with the new schema; then retry."
	case live.gen == "json" && gen == "string":
		return "ClickHouse holds the new events table (JSON payload columns), and " + label + " predates the storage reset: its images cannot write that table (events would be accepted and then dropped at insert), and its ClickHouse may not open it.",
			"Roll back to a revision on " + minImageForJSONSchema + " or newer (helm history " + release + " -n " + namespace + "), or move forward with `origamy upgrade --version <release>`."
	case live.gen == "string" && gen == "json":
		return "ClickHouse holds the pre-reset events table (String payload columns), which the images of " + label + " cannot write, and a rollback does not reset it.",
			"Move across the storage reset with `origamy upgrade --version " + chartVersion(rev.Chart) + "` instead: it resets the table (asking before it discards the event history)."
	}
	return "", ""
}
