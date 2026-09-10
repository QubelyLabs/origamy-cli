package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the data plane's version and health",
	Long:  `Report the installed version, whether a newer one is available, and service health.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStatus()
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func runStatus() error {
	ui.Title("Origamy data plane — status")
	switch {
	case hasKubernetes() && releaseInstalled():
		return statusKubernetes()
	case hasDocker():
		return statusDocker()
	default:
		return fail("No Origamy data plane found on this machine.",
			"Run `origamy deploy --token …` to install one, or point kubectl/Docker at the host where it runs.")
	}
}

func statusKubernetes() error {
	cur, err := installedRelease()
	if err != nil {
		return diagnose(err.Error())
	}
	ui.KV("Target", "Kubernetes")
	ui.KV("Chart", cur.Chart)
	ui.KV("App version", orDash(cur.AppVersion))
	ui.KV("Revision", cur.Revision)
	ui.KV("Status", cur.Status)

	// Is a newer version published?
	installed := chartVersion(cur.Chart)
	if latest, err := latestChartVersion(); err == nil {
		if latest != installed {
			ui.KV("Latest", ui.Cyan(latest)+ui.Gray("  — run: origamy upgrade"))
		} else {
			ui.KV("Latest", latest+ui.Gray("  (up to date)"))
		}
	}

	// Service health.
	ready, total, issues := podSummary(namespace)
	ui.KV("Services", fmt.Sprintf("%d/%d ready", ready, total))
	for comp, reason := range issues {
		ui.Detail("%s — %s", ui.Bold(comp), ui.DiagnosePod(reason))
	}
	return nil
}

func statusDocker() error {
	dir, ok := findComposeDir()
	if !ok {
		return fail("No Origamy compose project found here.",
			"cd into your data-plane directory (e.g. ./origamy-dp-<id>) and retry.")
	}
	ui.KV("Target", "Docker")
	ui.KV("Directory", dir)
	ui.KV("Image tag", orDash(readEnvVar(filepath.Join(dir, ".env"), "DP_IMAGE_TAG")))
	ui.KV("Profiles", orDash(readEnvVar(filepath.Join(dir, ".env"), "COMPOSE_PROFILES")))

	out, err := runCaptured("docker", "compose", "--project-directory", dir,
		"--env-file", filepath.Join(dir, ".env"), "ps")
	if err != nil {
		return diagnose(out)
	}
	fmt.Println()
	fmt.Println(out)
	return nil
}
