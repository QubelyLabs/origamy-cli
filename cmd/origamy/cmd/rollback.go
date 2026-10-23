package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Roll back the data plane to a previous version",
	Long: `Roll back to a previous Helm revision (default: the one immediately before
the current). Your data is preserved. List revisions with 'origamy status' or
'helm history odp -n origamy-dp', then target one with --to.

A rollback that cannot touch the events table (the revision's images write
the same table as the installed ones) is helm's own rollback, followed by a
check of the images the release runs. One that takes the images across the
storage reset (to a revision before ` + firstJSONChart + ` while ClickHouse holds the new table
with JSON payload columns, or the other way round) asks ClickHouse first
(in its pod: it needs create on pods/exec in ` + namespace + `; an external ClickHouse is
asked through a short-lived Job) and is refused before anything changes when
the revision's images cannot write the table it holds, when it cannot be
asked, for a revision whose images are unnumbered or pinned by digest alone,
or one that switches between the bundled and an external ClickHouse. So is
any rollback while an upgrade's storage reset is unfinished (ConfigMap
` + resetMarkerConfigMap + `). A rollback across the storage reset ends with a check that
ClickHouse holds the events table the images that run write, and fails when it
does not.`,
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
	kind, err := locateInstall("origamy rollback")
	if err != nil {
		return err
	}
	switch kind {
	case installKubernetes:
		return rollbackKubernetes(to)
	case installDocker:
		return fail("Docker installs don't keep a revision history.",
			"Docker tracks image tags, not Helm releases. Move back with: origamy upgrade --version <older>")
	default:
		return fail("No existing Origamy data plane found on this machine.",
			"There is nothing to roll back.")
	}
}

func rollbackKubernetes(to int) error {
	// What the revision runs is checked against the release before anything
	// changes. A rollback never resets the events table, so one that takes the
	// images across the storage reset would leave events accepted and dropped
	// at insert (and ClickHouse on a release that predates its table): that is
	// the only kind of rollback that asks ClickHouse; any other is helm's own.
	rev, err := rollbackTarget(to)
	switch {
	case errors.Is(err, errNoHistory):
		return fail(fmt.Sprintf("The release has no revision %d.", rev.Revision),
			"See available revisions with: helm history "+release+" -n "+namespace)
	case err != nil:
		return fail("Could not read the release history.", err.Error())
	}
	label := fmt.Sprintf("revision %d (%s)", rev.Revision, rev.Chart)
	plan, problem, hint := planRollback(rev)
	if problem != "" {
		return fail(problem, hint+" Nothing has been changed.")
	}
	if plan.relevant {
		// One run at a time on the events table; given up on every way out.
		giveUp, err := acquireKubeLease("rollback")
		if err != nil {
			return leaseFail(err)
		}
		defer onExit(giveUp)()
		defer exitOnSignal()()
		if problem, hint := rollbackAskProblem(rev, plan); problem != "" {
			return fail(problem, hint+" Nothing has been changed.")
		}
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

	cur, curErr := installedRelease()
	mode, want := checkImages, plan.gen
	if plan.relevant {
		mode, want = checkFull, ""
	}
	if c := kubeEventsCheck(mode, plan.external, want); c.why != "" {
		recheck := "origamy upgrade"
		if curErr == nil {
			recheck = kubeUpgradeCommand(chartVersion(cur.Chart), "stable")
		}
		return kubeCheckFail("Rolled back to "+label, recheck, c, plan.external, false)
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

// rollbackPlan is what planRollback found out about a rollback from helm's
// own state.
type rollbackPlan struct {
	// relevant: the rollback takes the images across the storage reset, or
	// cannot be told not to, so it is checked against ClickHouse.
	relevant bool
	// external: the release the rollback leaves is on an external ClickHouse.
	external bool
	// gen is the events table the revision's images write ("string", "json";
	// "" when they do not write one that is told).
	gen string
	// revVals are the revision's values; nowVals the installed ones.
	revVals, nowVals map[string]any
	revSet           imageSet
}

// planRollback decides, from helm's state alone (no contact with ClickHouse),
// whether rolling back to rev can touch the events table, and refuses
// (problem and a hint) what is wrong however ClickHouse answers: an
// unfinished storage reset, a revision whose images and ClickHouse cannot go
// together, and, for a rollback across the storage reset, a revision that
// moves between the bundled and an external ClickHouse, whose images do not
// write one known events table, or that has images pinned by digest.
func planRollback(rev releaseRevision) (plan rollbackPlan, problem, hint string) {
	label := fmt.Sprintf("revision %d (%s)", rev.Revision, rev.Chart)
	m, err := readResetMarker()
	markerUnread := false
	if err != nil {
		if re, ok := err.(*kubeReadError); !ok || !re.forbidden {
			return plan, "Could not read whether an upgrade left a storage reset unfinished.", err.Error()
		}
		markerUnread = true
	}
	if m.present {
		return plan, "An upgrade began the storage reset and did not finish it (ConfigMap " + resetMarkerConfigMap + " in " + namespace + "), so a rollback could leave the events table half-way.",
			"Finish it first: " + m.command() + "."
	}
	nowVals, err := releaseValues()
	if err != nil {
		return plan, "Could not read the current release values.", err.Error()
	}
	revVals, err := revisionValues(rev.Revision)
	if err != nil {
		return plan, "Could not read the values of " + label + ".", err.Error()
	}
	manifest, err := revisionManifest(rev.Revision)
	if err != nil {
		return plan, "Could not read what " + label + " runs.", err.Error()
	}
	plan = rollbackPlan{external: usesExternalClickHouse(revVals), revVals: revVals, nowVals: nowVals}
	var none bool
	plan.revSet, none = manifestImageSet(manifest)
	if g := plan.revSet.gen(); g == "string" || g == "json" {
		plan.gen = g
	}
	// The new events table needs ClickHouse 25.3 or newer: a revision whose
	// images write it and whose ClickHouse is older cannot have worked.
	if plan.gen == "json" && !plan.external {
		for _, im := range reManifestImage.FindAllStringSubmatch(manifest, -1) {
			if below, v := clickhouseImageBelow(im[1]); below {
				return plan, "Rolling back to " + label + " would run ClickHouse " + v + " (" + im[1] + ") under images that write the new events table, which needs ClickHouse " + minClickHouseForJSON + " or newer.",
					"Pick another revision (helm history " + release + " -n " + namespace + ")."
			}
		}
	}
	installed, noneInstalled := installedImageSet()
	crosses, _ := movesAcrossReset(installed, noneInstalled, plan.revSet.tags, false)
	// A rollback replaces what the release pins, too: images by digest that
	// the revision does not have the same of name no release, before or after.
	if none || !slices.Equal(installed.digests, plan.revSet.digests) {
		crosses = true
	}
	plan.relevant = crosses
	if markerUnread {
		if crosses {
			return plan, "Could not read whether an upgrade left a storage reset unfinished (your user may not read ConfigMaps in " + namespace + "), and this rollback can touch the events table.",
				"Grant get on configmaps in " + namespace + ", or roll back with helm yourself."
		}
		ui.Warn("Could not check whether an upgrade left a storage reset unfinished (your user may not read ConfigMaps in %s).", namespace)
	}
	if !crosses {
		return plan, "", ""
	}
	if usesExternalClickHouse(nowVals) != plan.external {
		return plan, "Rolling back to " + label + " would also move the data plane between the bundled ClickHouse and your external one, whose events tables this rollback cannot check together.",
			"Roll back with helm yourself if that is what you mean: helm rollback " + release + " " + strconv.Itoa(rev.Revision) + " -n " + namespace + "."
	}
	if _, why := manifestGeneration(manifest); why != "" {
		return plan, "Rolling back to " + label + " is refused: " + why + ".", "Pick another revision (helm history " + release + " -n " + namespace + "), or move forward with `origamy upgrade`."
	}
	if len(plan.revSet.digests) > 0 {
		return plan, "Rolling back to " + label + " is refused: some of its images are pinned by digest alone (" + strings.Join(plan.revSet.digests, ", ") + "), which names no release, so which events table they write is not known.",
			"Pick another revision (helm history " + release + " -n " + namespace + "), or roll back with helm yourself."
	}
	return plan, "", ""
}

// rollbackAskProblem is the other half of the check of a rollback across the
// storage reset, made under the lease: what rolling back to rev would do to
// the events table ClickHouse holds, which is asked (a ClickHouse that cannot
// be is a refusal too). It says why the rollback is refused, "" when it is
// not.
func rollbackAskProblem(rev releaseRevision, plan rollbackPlan) (problem, hint string) {
	label := fmt.Sprintf("revision %d (%s)", rev.Revision, rev.Chart)
	if m, err := readResetMarker(); err != nil {
		return "Could not read whether an upgrade left a storage reset unfinished.", err.Error()
	} else if m.present {
		return "An upgrade began the storage reset and did not finish it (ConfigMap " + resetMarkerConfigMap + " in " + namespace + "), so a rollback could leave the events table half-way.",
			"Finish it first: " + m.command() + "."
	}
	ask := askKubeClickHouse(plan.external)
	live := ask.live
	switch {
	case live.gen == "":
		return "ClickHouse could not be asked which events table it holds, so this rollback cannot tell whether " + label + " can write it.",
			ask.hint() + "\n" + strings.TrimSpace(ask.retry("origamy rollback"))
	case live.gen == liveOther:
		return fmt.Sprintf("ClickHouse's %s has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, which no release writes.", eventsTable, live.typ),
			"Check what created it, and drop it yourself if it should be recreated with the new schema; then retry."
	case live.gen == "json" && plan.gen == "string":
		return "ClickHouse holds the new events table (JSON payload columns), and " + label + " predates the storage reset: its images cannot write that table (events would be accepted and then dropped at insert), and its ClickHouse may not open it.",
			"Roll back to a revision on " + firstJSONChart + " or newer (helm history " + release + " -n " + namespace + "), or move forward with `origamy upgrade --version <release>`."
	case live.gen == "string" && plan.gen == "json" && plan.external:
		return "Your external ClickHouse holds the pre-reset events table (String payload columns), which the images of " + label + " cannot write, and the CLI never resets an external ClickHouse.",
			externalResetHint(plan.nowVals)
	case live.gen == "string" && plan.gen == "json":
		return "ClickHouse holds the pre-reset events table (String payload columns), which the images of " + label + " cannot write, and a rollback does not reset it.",
			"Move across the storage reset with `origamy upgrade --version " + chartVersion(rev.Chart) + "` instead: it resets the table (asking before it discards the event history)."
	}
	return "", ""
}
