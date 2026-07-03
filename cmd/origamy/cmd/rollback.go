package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Roll back the data plane to a previous version",
	Long: `Roll back to a previous Helm revision (default: the one immediately before
the current). Your data is preserved. List revisions with 'origamy status' or
'helm history odp -n origamy-dp', then target one with --to.`,
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
	args := []string{"rollback", release}
	label := "the previous revision"
	if to > 0 {
		args = append(args, fmt.Sprintf("%d", to))
		label = fmt.Sprintf("revision %d", to)
	}
	args = append(args, "-n", namespace, "--wait")

	sp := ui.Start("Rolling back to %s", label)
	if out, err := runCaptured("helm", args...); err != nil {
		sp.Fail("Rollback failed")
		ui.Detail("See available revisions with: helm history %s -n %s", release, namespace)
		return diagnose(out)
	}
	sp.Success("Rolled back to %s", label)

	// Re-pull for the rolled-back templates (Deployments only — data is safe).
	_, _ = runCaptured("kubectl", "rollout", "restart", "deployment", "-n", namespace)

	lines := []string{}
	if cur, err := installedRelease(); err == nil {
		lines = append(lines,
			ui.Gray("Now on    ")+ui.Bold(cur.Chart),
			ui.Gray("Revision  ")+cur.Revision,
			"")
	}
	lines = append(lines, ui.Green("Your data was preserved."))
	ui.Box("Rolled back", lines)
	return nil
}
