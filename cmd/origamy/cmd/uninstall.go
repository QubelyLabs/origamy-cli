package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall [data-plane-id]",
	Short: "Tear down an Origamy data plane from this machine",
	Long: `Remove the Origamy data plane this CLI deployed.

Auto-detects your environment:
  • Kubernetes → helm uninstall odp + delete the origamy-dp namespace
  • Docker     → docker compose down -v in ./origamy-dp-<id>/

Run this BEFORE deactivating the workspace in your dashboard so the plane
stops trying to reconnect.`,
	Example: `  origamy uninstall dp-13-qaglj0ye`,
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := ""
		if len(args) > 0 {
			id = args[0]
		}
		yes, _ := cmd.Flags().GetBool("yes")
		return runUninstall(id, yes)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	uninstallCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}

func runUninstall(id string, assumeYes bool) error {
	ui.Title("Uninstall Origamy data plane")
	if id != "" {
		ui.KV("Data plane", ui.Bold(id))
	}

	switch {
	case hasKubernetes():
		return uninstallKubernetes(assumeYes)
	case hasDocker() && id != "":
		return uninstallDocker(id, assumeYes)
	case hasDocker():
		return fail("Docker detected, but no data plane id was given.",
			"Run: origamy uninstall <data-plane-id>  (the id is on your dashboard's Connections page).")
	default:
		return fail("No Kubernetes cluster or Docker found on this machine.",
			"Nothing to uninstall here — run this command where the data plane is deployed.")
	}
}

// confirmTeardown asks for an explicit "yes" before destroying anything, unless
// --yes was passed.
func confirmTeardown(what string, assumeYes bool) bool {
	if assumeYes {
		return true
	}
	ui.Warn("This permanently removes %s.", what)
	ans := promptString("Type 'yes' to continue")
	return ans == "yes" || ans == "y"
}

// ── Kubernetes ────────────────────────────────────────────────────────────────

func uninstallKubernetes(assumeYes bool) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fail("Helm is required to uninstall a Kubernetes data plane.",
			"Install it from https://helm.sh/docs/intro/install/ and retry.")
	}
	ui.Title("Target")
	ui.Success("Kubernetes cluster detected")

	if !confirmTeardown(fmt.Sprintf("the '%s' release and the '%s' namespace", release, namespace), assumeYes) {
		return fail("Cancelled.", "")
	}

	// Uninstall the release. A missing release isn't fatal — we still want to
	// drop the namespace — so we don't bail here.
	sp := ui.Start("Uninstalling Helm release %s", release)
	if out, err := runCaptured("helm", "uninstall", release, "--namespace", namespace); err != nil {
		sp.Fail("Helm release not removed (it may already be gone)")
		if tail := lastLines(out, 3); tail != "" {
			ui.Detail("%s", ui.Dim(tail))
		}
	} else {
		sp.Success("Helm release %s removed", release)
	}

	sp = ui.Start("Deleting namespace %s", namespace)
	if _, err := runCaptured("kubectl", "delete", "namespace", namespace, "--ignore-not-found"); err != nil {
		sp.Fail("Could not delete namespace %s", namespace)
		return fail("Namespace deletion failed.",
			fmt.Sprintf("Remove it manually: kubectl delete namespace %s", namespace))
	}
	sp.Success("Namespace %s deleted", namespace)

	ui.Title("Done")
	ui.Success("Data plane uninstalled. You can now deactivate the workspace in your dashboard.")
	return nil
}

// ── Docker ────────────────────────────────────────────────────────────────────

func uninstallDocker(id string, assumeYes bool) error {
	dir := "origamy-dp-" + id
	compose := filepath.Join(dir, "docker-compose.yml")
	if _, err := os.Stat(compose); err != nil {
		return fail(fmt.Sprintf("No data plane found at ./%s", dir),
			"Run this from the directory where you deployed, or pass the right data plane id.")
	}
	ui.Title("Target")
	ui.Success("Docker deployment detected (./%s)", dir)

	if !confirmTeardown(fmt.Sprintf("the containers and volumes in ./%s", dir), assumeYes) {
		return fail("Cancelled.", "")
	}

	sp := ui.Start("Stopping and removing containers + volumes")
	if out, err := runCaptured("docker", "compose", "-f", compose, "down", "-v", "--remove-orphans"); err != nil {
		sp.Fail("docker compose down failed")
		return diagnose(out)
	}
	sp.Success("Containers and volumes removed")

	// Best-effort: remove the generated deploy directory (.env, compose, etc.).
	if err := os.RemoveAll(dir); err == nil {
		ui.Success("Removed ./%s", dir)
	}

	ui.Title("Done")
	ui.Success("Data plane uninstalled. You can now deactivate the workspace in your dashboard.")
	return nil
}
