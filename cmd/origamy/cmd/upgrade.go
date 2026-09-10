package cmd

import (
	"fmt"
	"os"
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

With no flags a Kubernetes install moves to the latest published chart, and a
Docker install to the release this CLI was built for (` + helmVersion + `). Your
datastores (ClickHouse, Postgres, NATS, Redis) are never touched — only the
service containers roll. Reuses your existing configuration, so no token is
needed.

Use --enable-ai / --disable-ai (mutually exclusive) to switch the agentic AI
engine on or off on an already-running data plane — this is how you turn on the
AI package after you've deployed. On Kubernetes the toggle re-applies your
values from a clean export so the chart's own defaults for the new service fill
in; on Docker it flips the "agentic" compose profile (generating the engine's
KEK + API token locally the first time). Either way your datastores are never
touched, and the engine stays inert until you enable AI and add an LLM
credential in your dashboard.

Use --enable-predictor / --disable-predictor the same way for the predictor
(conversion scoring) service — Kubernetes only.`,
	Example: `  origamy upgrade                 # Kubernetes: latest published chart; Docker: this CLI's release
  origamy upgrade --version 0.1.17
  origamy upgrade --channel edge  # track the bleeding edge (:main)
  origamy upgrade --enable-ai     # switch the AI engine on
  origamy upgrade --disable-ai    # switch the AI engine off
  origamy upgrade --enable-predictor   # switch the predictor (scoring) service on
  origamy upgrade --disable-predictor  # switch the predictor off`,
	RunE: func(cmd *cobra.Command, args []string) error {
		version, _ := cmd.Flags().GetString("version")
		channel, _ := cmd.Flags().GetString("channel")
		sets, _ := cmd.Flags().GetStringArray("set")
		enableAI, _ := cmd.Flags().GetBool("enable-ai")
		disableAI, _ := cmd.Flags().GetBool("disable-ai")
		enablePredictor, _ := cmd.Flags().GetBool("enable-predictor")
		disablePredictor, _ := cmd.Flags().GetBool("disable-predictor")
		return runUpgrade(version, channel, sets, enableAI, disableAI, enablePredictor, disablePredictor)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	upgradeCmd.Flags().String("version", "", "Target release (default: latest published chart on Kubernetes, "+helmVersion+" on Docker)")
	upgradeCmd.Flags().String("channel", "stable", "Release channel: stable (pinned) or edge (:main)")
	upgradeCmd.Flags().StringArray("set", nil, "Set a chart value (key=value, repeatable; Kubernetes only). New values introduced by a chart version don't exist in the release yet, so --reuse-values alone can't set them.")
	upgradeCmd.Flags().Bool("enable-ai", false, "Switch the Origamy AI (agentic) engine on for the existing install")
	upgradeCmd.Flags().Bool("disable-ai", false, "Switch the Origamy AI (agentic) engine off for the existing install")
	upgradeCmd.Flags().Bool("enable-predictor", false, "Switch the predictor (conversion scoring) service on for the existing release (Kubernetes only)")
	upgradeCmd.Flags().Bool("disable-predictor", false, "Switch the predictor (conversion scoring) service off for the existing release (Kubernetes only)")
}

func runUpgrade(version, channel string, sets []string, enableAI, disableAI, enablePredictor, disablePredictor bool) error {
	// Validate the toggles up front (before any registry/cluster calls) so a
	// conflicting request fails fast with a clear message.
	if _, err := aiToggleArgs(enableAI, disableAI); err != nil {
		return fail(err.Error(), "Pass one of --enable-ai or --disable-ai, not both.")
	}
	if _, err := predictorToggleArgs(enablePredictor, disablePredictor); err != nil {
		return fail(err.Error(), "Pass one of --enable-predictor or --disable-predictor, not both.")
	}
	ui.Title("Origamy data plane — upgrade")
	switch {
	case hasKubernetes() && releaseInstalled():
		return upgradeKubernetes(version, channel, sets, enableAI, disableAI, enablePredictor, disablePredictor)
	case hasDocker():
		if len(sets) > 0 {
			return fail("--set applies to Kubernetes installs only.",
				"Docker installs configure via .env; edit it directly and rerun without --set.")
		}
		if enablePredictor || disablePredictor {
			return fail("--enable-predictor/--disable-predictor apply to Kubernetes installs only.",
				"The predictor runs on Kubernetes data planes; the Docker compose bundle doesn't include it.")
		}
		return upgradeDocker(version, channel, enableAI, disableAI)
	default:
		return fail("No existing Origamy data plane found on this machine.",
			"Run `origamy deploy --token …` first, or point kubectl/Docker at the host where it's installed.")
	}
}

func upgradeKubernetes(version, channel string, sets []string, enableAI, disableAI, enablePredictor, disablePredictor bool) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fail("Helm is required for Kubernetes upgrades.",
			"Install it from https://helm.sh/docs/intro/install/ and retry.")
	}

	// A toggle means the run's purpose includes flipping a feature boolean
	// (orchestratorEngine.enabled / predictor.enabled), which changes how we
	// preserve values (see the -f branch below) and forces the upgrade to
	// proceed even when already on the target version.
	aiToggle := enableAI || disableAI
	predictorToggle := enablePredictor || disablePredictor
	toggle := aiToggle || predictorToggle

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

	// Enabling a feature on a chart that predates it would "succeed" with
	// nothing deployed (helm ignores unknown values), so refuse up front.
	if err := featureGate(target, enableAI, enablePredictor); err != nil {
		return fail(err.Error(), "Upgrade to a newer chart first (`origamy upgrade`), or pass --version.")
	}

	// A feature toggle must run even at the same version (that's how "buy AI
	// later" flips the value on a release already on latest), so skip the
	// no-op shortcut.
	if !toggle {
		if cur, err := installedRelease(); err == nil && chartVersion(cur.Chart) == target && channel != "edge" {
			ui.Success("Already on %s — nothing to upgrade.", target)
			return nil
		}
	}

	args := []string{
		"upgrade", release, helmChart,
		"--namespace", namespace,
		"--version", target,
	}
	// Value preservation: the normal version bump reuses the release's values in
	// place. But toggling a feature boolean (orchestratorEngine.enabled /
	// predictor.enabled) must NOT use --reuse-values — when the target chart has
	// default structure the old release never set (the predictor block on a
	// pre-predictor install, say), --reuse-values ignores the new chart's
	// defaults and the render nil-derefs. For a toggle we export the user's
	// values to a file and re-apply them, so the new chart's defaults fill the
	// gaps cleanly while every customer value (controlPlane, portalAgent,
	// preset, clickhouse, tunnel identity secret) is preserved.
	if toggle {
		valsFile, err := exportReleaseValues()
		if err != nil {
			return fail("Could not read the current release values.", err.Error())
		}
		defer func() { _ = os.Remove(valsFile) }()
		args = append(args, "-f", valsFile)
	} else {
		args = append(args, "--reuse-values")
	}

	// Resolve the image tag every data-plane service should run, then pin it
	// both globally AND per-service. Per-service is not redundant: --reuse-values
	// carries each service's frozen image.tag forward, and the chart's image
	// helper (.image.tag | default .global.imageTag | default .appVersion) lets
	// a non-empty per-service tag SHADOW global.imageTag — so a release first
	// installed on the pre-pinning 0.1.12 chart (which froze tag: main) would
	// otherwise stay on :main no matter the target version.
	imageTag := target // pinned appVersion of the target chart
	if channel == "edge" {
		imageTag = "main" // track the moving edge tag instead
	}
	args = append(args, "--set", "global.imageTag="+imageTag)
	for _, svc := range serviceImageKeys() {
		args = append(args, "--set", svc+".image.tag="+imageTag)
	}

	// Feature toggles (validated in runUpgrade, so these never error here).
	// Only booleans are set — the chart owns secret generation (AI KEK/token).
	if aiArgs, _ := aiToggleArgs(enableAI, disableAI); len(aiArgs) > 0 {
		args = append(args, aiArgs...)
	}
	if predArgs, _ := predictorToggleArgs(enablePredictor, disablePredictor); len(predArgs) > 0 {
		args = append(args, predArgs...)
	}

	// Operator-supplied values LAST so they win over the pins above (e.g. a key
	// a new chart version introduced, which --reuse-values can't know about, or
	// a deliberate per-service tag override). Passed to helm verbatim.
	for _, s := range sets {
		args = append(args, "--set", s)
	}

	action := fmt.Sprintf("Upgrading the chart to %s", target)
	switch {
	case aiToggle && predictorToggle:
		action = fmt.Sprintf("Applying feature toggles (chart %s)", target)
	case aiToggle:
		verb := "Enabling"
		if disableAI {
			verb = "Disabling"
		}
		action = fmt.Sprintf("%s the AI engine (chart %s)", verb, target)
	case predictorToggle:
		verb := "Enabling"
		if disablePredictor {
			verb = "Disabling"
		}
		action = fmt.Sprintf("%s the predictor (chart %s)", verb, target)
	}
	sp := ui.Start("%s", action)
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

	boxLines := []string{
		ui.Gray("Version    ") + ui.Bold(target),
		ui.Gray("Namespace  ") + namespace,
	}
	if enablePredictor {
		boxLines = append(boxLines, ui.Gray("Predictor  ")+ui.Green("enabled"))
	} else if disablePredictor {
		boxLines = append(boxLines, ui.Gray("Predictor  ")+"disabled")
	}
	if enableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+ui.Green("enabled"))
	} else if disableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+"disabled")
	}
	boxLines = append(
		boxLines,
		"",
		ui.Green("Your data (ClickHouse/Postgres/NATS/Redis) was not touched."),
		ui.Gray("Roll back with: ")+"origamy rollback",
	)
	ui.Box("Upgraded", boxLines)
	return nil
}

func upgradeDocker(version, channel string, enableAI, disableAI bool) error {
	dir, ok := findComposeDir()
	if !ok {
		return fail("No Origamy compose project found here.",
			"cd into your data-plane directory (e.g. ./origamy-dp-<id>) and retry, or run `origamy deploy` first.")
	}
	envPath := filepath.Join(dir, ".env")

	// Docker pins images via DP_IMAGE_TAG. An explicit version wins; "edge"
	// tracks the moving :main tag; otherwise the release this CLI ships with
	// (images and chart share a version, so this matches the Kubernetes pin).
	tag := strings.TrimSpace(version)
	switch {
	case tag != "":
	case channel == "edge":
		tag = "main"
	default:
		tag = helmVersion
	}
	if err := setEnvVar(envPath, "DP_IMAGE_TAG", tag); err != nil {
		return fail("Could not update .env.", err.Error())
	}

	// AI engine = the "agentic" compose profile. Enabling generates the engine's
	// secrets locally on first use (the KEK is never regenerated afterwards —
	// per-workspace LLM keys are encrypted under it) and points portal-agent
	// at the engine; disabling drops the profile and that pointer, keeping the
	// secrets so a later re-enable finds the same KEK.
	profiles := composeProfiles(envPath)
	switch {
	case enableAI:
		for k, v := range map[string]string{
			"ORCH_KEK":                existingOrRandomKEK(envPath, "ORCH_KEK"),
			"ORCH_ENGINE_API_TOKEN":   existingOrRandom(envPath, "ORCH_ENGINE_API_TOKEN"),
			"ORCHESTRATOR_ENGINE_URL": orchestratorEngineURL,
		} {
			if err := setEnvVar(envPath, k, v); err != nil {
				return fail("Could not update .env.", err.Error())
			}
		}
		profiles = withProfile(profiles, "agentic", true)
	case disableAI:
		if err := setEnvVar(envPath, "ORCHESTRATOR_ENGINE_URL", ""); err != nil {
			return fail("Could not update .env.", err.Error())
		}
		profiles = withProfile(profiles, "agentic", false)
	}
	if enableAI || disableAI {
		if err := setEnvVar(envPath, "COMPOSE_PROFILES", strings.Join(profiles, ",")); err != nil {
			return fail("Could not update .env.", err.Error())
		}
	}
	ui.KV("Directory", dir)
	ui.KV("Image tag", tag)
	ui.KV("Profiles", orDash(strings.Join(profiles, ",")))

	sp := ui.Start("Pulling %s images", tag)
	if out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "pull"); err != nil {
		sp.Fail("docker compose pull failed")
		return diagnose(out)
	}
	sp.Success("Images pulled")

	// --remove-orphans stops containers of profiles that were just turned off
	// (compose otherwise leaves them running); named volumes are untouched.
	sp = ui.Start("Restarting services")
	if out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "up", "-d", "--remove-orphans"); err != nil {
		sp.Fail("docker compose up failed")
		return diagnose(out)
	}
	sp.Success("Services restarted")

	boxLines := []string{
		ui.Gray("Image tag  ") + ui.Bold(tag),
		ui.Gray("Directory  ") + dir,
	}
	if enableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+ui.Green("enabled"))
	} else if disableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+"disabled")
	}
	boxLines = append(boxLines, "", ui.Green("Named volumes (your data) were not touched."))
	ui.Box("Upgraded", boxLines)
	return nil
}
