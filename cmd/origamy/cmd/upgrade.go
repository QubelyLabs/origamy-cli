package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the data plane to a newer version",
	Long: `Upgrade the Origamy data plane in place, preserving all data and config.

With no flags it moves to the latest published version. Your datastores
(ClickHouse, Postgres, NATS, Redis) are never touched — only the service pods
roll. Reuses your existing configuration, so no token is needed.`,
	Example: `  origamy upgrade                 # to the latest published version
  origamy upgrade --version 0.1.12
  origamy upgrade --channel edge  # track the bleeding edge (:main)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		version, _ := cmd.Flags().GetString("version")
		channel, _ := cmd.Flags().GetString("channel")
		sets, _ := cmd.Flags().GetStringArray("set")
		return runUpgrade(version, channel, sets)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	upgradeCmd.Flags().String("version", "", "Target chart version (default: latest published)")
	upgradeCmd.Flags().String("channel", "stable", "Release channel: stable (pinned) or edge (:main)")
	upgradeCmd.Flags().StringArray("set", nil, "Set a chart value (key=value, repeatable; Kubernetes only). New values introduced by a chart version don't exist in the release yet, so --reuse-values alone can't set them.")
}

func runUpgrade(version, channel string, sets []string) error {
	ui.Title("Origamy data plane — upgrade")
	switch {
	case hasKubernetes() && releaseInstalled():
		return upgradeKubernetes(version, channel, sets)
	case hasDocker():
		if len(sets) > 0 {
			return fail("--set applies to Kubernetes installs only.",
				"Docker installs configure via .env; edit it directly and rerun without --set.")
		}
		return upgradeDocker(version, channel)
	default:
		return fail("No existing Origamy data plane found on this machine.",
			"Run `origamy deploy --token …` first, or point kubectl/Docker at the host where it's installed.")
	}
}

func upgradeKubernetes(version, channel string, sets []string) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fail("Helm is required for Kubernetes upgrades.",
			"Install it from https://helm.sh/docs/intro/install/ and retry.")
	}

	if cur, err := installedRelease(); err == nil {
		ui.KV("Installed", cur.Chart+"  (revision "+cur.Revision+")")
	}

	// Resolve the target version (explicit flag wins; otherwise ask the registry).
	target := version
	if target == "" {
		sp := ui.Start("Resolving the latest published version")
		lv, err := latestChartVersion()
		if err != nil {
			sp.Fail("Could not reach the chart registry")
			return diagnose(err.Error())
		}
		target = lv
		sp.Success("Latest published version is %s", target)
	}
	ui.KV("Target", target)

	if cur, err := installedRelease(); err == nil && chartVersion(cur.Chart) == target && channel != "edge" {
		ui.Success("Already on %s — nothing to upgrade.", target)
		return nil
	}

	args := []string{
		"upgrade", release, helmChart,
		"--namespace", namespace,
		"--version", target,
		"--reuse-values",
	}
	if channel == "edge" {
		// Track the moving :main image tag instead of the pinned appVersion.
		args = append(args, "--set", "global.imageTag=main")
	}
	// Operator-supplied values (e.g. a key a new chart version introduced,
	// which --reuse-values can't know about). Passed to helm verbatim.
	for _, s := range sets {
		args = append(args, "--set", s)
	}

	sp := ui.Start("Upgrading the chart to %s", target)
	if out, err := runCaptured("helm", args...); err != nil {
		sp.Fail("Helm upgrade failed")
		return diagnose(out)
	}
	sp.Success("Chart upgraded to %s", target)

	// Roll the service pods so they re-pull. rollout restart targets Deployments
	// only, so the datastore StatefulSets (and their PVCs) are never touched.
	sp = ui.Start("Rolling service pods")
	if out, err := runCaptured("kubectl", "rollout", "restart", "deployment", "-n", namespace); err != nil {
		sp.Warn("Chart upgraded, but the pod roll needs a manual nudge")
		ui.Detail("Run: kubectl rollout restart deployment -n %s", namespace)
		_ = out
	} else {
		sp.Success("Service pods rolling")
	}

	ui.Box("Upgraded", []string{
		ui.Gray("Version    ") + ui.Bold(target),
		ui.Gray("Namespace  ") + namespace,
		"",
		ui.Green("Your data (ClickHouse/Postgres/NATS/Redis) was not touched."),
		ui.Gray("Roll back with: ") + "origamy rollback",
	})
	return nil
}

func upgradeDocker(version, channel string) error {
	dir, ok := findComposeDir()
	if !ok {
		return fail("No Origamy compose project found here.",
			"cd into your data-plane directory (e.g. ./origamy-dp-<id>) and retry, or run `origamy deploy` first.")
	}
	envPath := filepath.Join(dir, ".env")

	// The docker install pins images via DP_IMAGE_TAG. A version pins to that
	// tag; edge (or no version) tracks :main.
	tag := strings.TrimSpace(version)
	if tag == "" || channel == "edge" {
		tag = "main"
	}
	if err := setEnvVar(envPath, "DP_IMAGE_TAG", tag); err != nil {
		return fail("Could not update .env.", err.Error())
	}
	ui.KV("Directory", dir)
	ui.KV("Image tag", tag)

	sp := ui.Start("Pulling %s images", tag)
	if out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "pull"); err != nil {
		sp.Fail("docker compose pull failed")
		return diagnose(out)
	}
	sp.Success("Images pulled")

	sp = ui.Start("Restarting services")
	if out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "up", "-d"); err != nil {
		sp.Fail("docker compose up failed")
		return diagnose(out)
	}
	sp.Success("Services restarted")

	ui.Box("Upgraded", []string{
		ui.Gray("Image tag  ") + ui.Bold(tag),
		ui.Gray("Directory  ") + dir,
		"",
		ui.Green("Named volumes (your data) were not touched."),
	})
	return nil
}
