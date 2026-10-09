package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
(conversion scoring) service — Kubernetes only.

Moving from a release before ` + minImageForJSONSchema + ` to ` + minImageForJSONSchema + ` or later crosses the data
plane's storage reset: ClickHouse moves to 25.3 and the events table is
recreated with native JSON columns, which discards the event history
collected so far (profiles, traits, segments and journeys are kept). The
upgrade explains this and asks for confirmation; pass --yes to skip the
prompt.`,
	Example: `  origamy upgrade                 # Kubernetes: latest published chart; Docker: this CLI's release
  origamy upgrade --version 0.1.17
  origamy upgrade --channel edge  # track the moving :staging images; not for production
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
		yes, _ := cmd.Flags().GetBool("yes")
		return runUpgrade(version, channel, sets, enableAI, disableAI, enablePredictor, disablePredictor, yes)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	upgradeCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt when the upgrade discards event history (storage reset)")
	upgradeCmd.Flags().String("version", "", "Target release (default: latest published chart on Kubernetes, "+helmVersion+" on Docker)")
	upgradeCmd.Flags().String("channel", "stable", "Release channel: stable (pinned) or edge (the moving :staging images; Kubernetes needs chart "+minImageForJSONSchema+"+)")
	upgradeCmd.Flags().StringArray("set", nil, "Set a chart value (key=value, repeatable; Kubernetes only), e.g. a key a newer chart introduced.")
	upgradeCmd.Flags().Bool("enable-ai", false, "Switch the Origamy AI (agentic) engine on for the existing install")
	upgradeCmd.Flags().Bool("disable-ai", false, "Switch the Origamy AI (agentic) engine off for the existing install")
	upgradeCmd.Flags().Bool("enable-predictor", false, "Switch the predictor (conversion scoring) service on for the existing release (Kubernetes only)")
	upgradeCmd.Flags().Bool("disable-predictor", false, "Switch the predictor (conversion scoring) service off for the existing release (Kubernetes only)")
}

func runUpgrade(version, channel string, sets []string, enableAI, disableAI, enablePredictor, disablePredictor, assumeYes bool) error {
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
		return upgradeKubernetes(version, channel, sets, enableAI, disableAI, enablePredictor, disablePredictor, assumeYes)
	case hasDocker():
		if len(sets) > 0 {
			return fail("--set applies to Kubernetes installs only.",
				"Docker installs configure via .env; edit it directly and rerun without --set.")
		}
		if enablePredictor || disablePredictor {
			return fail("--enable-predictor/--disable-predictor apply to Kubernetes installs only.",
				"The predictor runs on Kubernetes data planes; the Docker compose bundle doesn't include it.")
		}
		return upgradeDocker(version, channel, enableAI, disableAI, assumeYes)
	default:
		return fail("No existing Origamy data plane found on this machine.",
			"Run `origamy deploy --token …` first, or point kubectl/Docker at the host where it's installed.")
	}
}

func upgradeKubernetes(version, channel string, sets []string, enableAI, disableAI, enablePredictor, disablePredictor, assumeYes bool) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fail("Helm is required for Kubernetes upgrades.",
			"Install it from https://helm.sh/docs/intro/install/ and retry.")
	}

	// A toggle means the run's purpose includes flipping a feature boolean
	// (orchestratorEngine.enabled / predictor.enabled), which forces the
	// upgrade to proceed even when already on the target version.
	aiToggle := enableAI || disableAI
	predictorToggle := enablePredictor || disablePredictor
	toggle := aiToggle || predictorToggle

	installedVer := ""
	if cur, err := installedRelease(); err == nil {
		ui.KV("Installed", cur.Chart+"  (revision "+cur.Revision+")")
		installedVer = chartVersion(cur.Chart)
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

	// Crossing the storage reset discards the event history: say so and get an
	// explicit yes before anything is touched.
	reset := crossesSchemaReset(installedVer, target)
	if reset && !confirmSchemaReset(installedVer, target, assumeYes) {
		return fail("Cancelled.", "")
	}

	args := []string{
		"upgrade", release, helmChart,
		"--namespace", namespace,
		"--version", target,
	}
	// Value preservation: the customer's own values are exported and re-applied
	// from a file — never `--reuse-values`. --reuse-values hands the NEW chart
	// the OLD release's fully coalesced values, so every default block the old
	// chart lacked (the predictor on a pre-0.1.17 install, the orchestrator on
	// a pre-0.1.16 one) is simply missing and the templates nil-deref at
	// render. Every release the shipped v0.1.18 CLI installed is on chart
	// 0.1.15 and hit exactly that. Re-applying only what the customer set
	// (controlPlane, portalAgent, preset, clickhouse, the tunnel identity
	// Secret) lets the target chart's defaults fill the gaps.
	valsFile, err := exportReleaseValues(target)
	if err != nil {
		return fail("Could not read the current release values.", err.Error())
	}
	defer func() { _ = os.Remove(valsFile) }()
	args = append(args, "-f", valsFile)
	// A target that still predates the config-sync liveness fix keeps the
	// slow probe a deploy would have given it.
	args = append(args, legacyChartSetArgs(target)...)

	// Resolve the image tag every data-plane service should run, then pin it
	// both globally AND per-service. Per-service is not redundant: the exported
	// values carry each service's frozen image.tag forward, and the chart's
	// image helper (.image.tag | default .global.imageTag | default .appVersion)
	// lets a non-empty per-service tag SHADOW global.imageTag — so a release
	// first installed on the pre-pinning 0.1.12 chart (which froze tag: main)
	// would otherwise stay on :main no matter the target version.
	imageTag := target // pinned appVersion of the target chart
	if channel == "edge" {
		// :staging is the only moving tag every service is rebuilt under
		// (:main is months old and missing for the AI engine and predictor).
		// Those images write the post-reset ClickHouse schema, so they need a
		// chart that ships it.
		if versionLess(target, minImageForJSONSchema) {
			return fail(fmt.Sprintf("The edge images need chart %s or newer (targeting %s).", minImageForJSONSchema, target),
				"Pass --version "+minImageForJSONSchema+" or newer together with --channel edge, or drop --channel edge.")
		}
		imageTag = dockerEdgeTag
		ui.Warn("The edge channel runs the moving :%s images; not for production.", imageTag)
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

	// Storage reset: the chart's init job only runs CREATE TABLE IF NOT EXISTS,
	// so the pre-reset events table survives the upgrade and the new images
	// cannot query it. Once ClickHouse is back on the new image, drop it and
	// re-apply the chart's schema (from its own ConfigMap) so the table comes
	// back with the native JSON columns.
	if reset {
		if err := reinitKubernetesClickHouse(); err != nil {
			return err
		}
	}

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
	if reset {
		boxLines = append(boxLines, "", ui.Yellow("Event history was discarded (storage reset); profiles, traits, segments and journeys were kept."))
	} else {
		boxLines = append(boxLines, "", ui.Green("Your data (ClickHouse/Postgres/NATS/Redis) was not touched."))
	}
	boxLines = append(boxLines, ui.Gray("Roll back with: ")+"origamy rollback")
	ui.Box("Upgraded", boxLines)
	return nil
}

// confirmSchemaReset explains what crossing the storage reset means and asks
// for an explicit yes (unless --yes).
func confirmSchemaReset(from, to string, assumeYes bool) bool {
	ui.Warn("Upgrading %s → %s crosses the data plane's storage reset.", from, to)
	ui.Detail("ClickHouse moves to 25.3 and the events table is recreated with native JSON payload columns.")
	ui.Detail("The event history collected so far is DISCARDED. Profiles, traits, segments and journeys are kept.")
	if assumeYes {
		return true
	}
	return promptString("Type 'yes' to continue") == "yes"
}

// reinitKubernetesClickHouse waits for the ClickHouse pod to be back after the
// chart upgrade, drops the pre-reset events table and re-applies the chart's
// schema from its ConfigMap, inside the pod.
func reinitKubernetesClickHouse() error {
	pod := release + "-clickhouse-0"
	sp := ui.Start("Waiting for ClickHouse to come back on the new image")
	deadline := time.Now().Add(5 * time.Minute)
	for {
		out, err := runCaptured("kubectl", "get", "pod", pod, "-n", namespace, "-o", "jsonpath={.status.containerStatuses[0].ready}")
		if err == nil && strings.TrimSpace(out) == "true" {
			break
		}
		if time.Now().After(deadline) {
			sp.Fail("ClickHouse did not become ready")
			return fail("ClickHouse is not ready after the upgrade.",
				"Check: kubectl get pods -n "+namespace+"  — then finish the reset by hand: drop "+eventsTable+" and re-run the chart's init SQL.")
		}
		time.Sleep(5 * time.Second)
	}
	sp.Success("ClickHouse is ready")

	sp = ui.Start("Recreating the events table with the new schema")
	if out, err := runCaptured("kubectl", "exec", "-n", namespace, pod, "--", "sh", "-c", clickhouseDropEvents); err != nil {
		sp.Fail("Could not drop the old events table")
		return diagnose(out)
	}
	schema, err := runCaptured("kubectl", "get", "configmap", release+"-clickhouse-init", "-n", namespace, "-o", `jsonpath={.data.init\.sql}`)
	if err != nil || strings.TrimSpace(schema) == "" {
		sp.Fail("Could not read the chart's schema")
		return fail("The old events table was dropped but the chart's init SQL could not be read.",
			"Re-run the chart's schema by hand: kubectl get configmap "+release+"-clickhouse-init -n "+namespace+" -o jsonpath='{.data.init\\.sql}' | kubectl exec -i -n "+namespace+" "+pod+" -- sh -c '"+clickhouseInitStdin+"'")
	}
	if out, err := runWithStdin(schema, "kubectl", "exec", "-i", "-n", namespace, pod, "--", "sh", "-c", clickhouseInitStdin); err != nil {
		sp.Fail("Schema init failed")
		return diagnose(out)
	}
	sp.Success("Events table recreated")
	return nil
}

// dockerEdgeTag is what `--channel edge` runs on Docker. The :staging tag is
// rebuilt for every service on each staging deploy; :main is not (the
// data-plane's main branch is development-only), so it lags for months and
// is missing entirely for the AI engine and predictor images.
const dockerEdgeTag = "staging"

func upgradeDocker(version, channel string, enableAI, disableAI, assumeYes bool) error {
	dir, err := composeProjectDir()
	if err != nil {
		return fail("No Origamy compose project found here.",
			err.Error()+"\ncd into your data-plane directory (e.g. ./origamy-dp-<id>) and retry, or run `origamy deploy` first.")
	}
	envPath := filepath.Join(dir, ".env")
	installedTag := readEnvVar(envPath, "DP_IMAGE_TAG")

	// Docker pins images via DP_IMAGE_TAG. An explicit version wins; "edge"
	// tracks the moving :staging tag; otherwise the release this CLI ships with
	// (images and chart share a version, so this matches the Kubernetes pin).
	tag := strings.TrimSpace(version)
	switch {
	case tag != "":
	case channel == "edge":
		tag = dockerEdgeTag
		ui.Warn("The edge channel runs the moving :%s images; not for production.", tag)
	default:
		tag = helmVersion
	}
	_, installedSemver := parseVersion(installedTag)
	_, targetSemver := parseVersion(tag)
	switch {
	case installedTag != "" && versionLess(tag, installedTag):
		ui.Warn("Moving from %s back to %s — an older release may not understand data written by the newer one.", installedTag, tag)
	case installedTag != "" && !installedSemver && targetSemver:
		ui.Warn("Moving from the moving %s images to release %s may be a downgrade; pass --channel edge to stay on %s.", installedTag, tag, installedTag)
	}
	reset := crossesSchemaReset(installedTag, tag)
	if reset && !confirmSchemaReset(installedTag, tag, assumeYes) {
		return fail("Cancelled.", "")
	}
	// Same guard as deploy: the schema in this project is what the control
	// plane served, and a release that cannot write it must not be started
	// against it. Crossing the reset re-fetches the schema below instead.
	if !reset {
		if initSQL, err := os.ReadFile(filepath.Join(dir, "clickhouse-init.sql")); err == nil {
			if why := bundleSchemaMismatch(string(initSQL), tag); why != "" {
				return fail(why, "Pass --version <release> that matches your control plane's schema, or --channel edge to stay on the moving images.")
			}
		}
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

	// Pull BEFORE recording the tag: a tag that does not exist for one of the
	// services (or a registry outage) must leave .env — and therefore every
	// later `docker compose up` — on the release that is actually running.
	// The process environment wins over the env file in compose
	// interpolation, so the pull sees the candidate tag without writing it.
	sp := ui.Start("Pulling %s images", tag)
	if out, err := runCapturedEnv([]string{"DP_IMAGE_TAG=" + tag}, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "pull"); err != nil {
		sp.Fail("docker compose pull failed")
		return diagnose(out)
	}
	sp.Success("Images pulled")
	if err := setEnvVar(envPath, "DP_IMAGE_TAG", tag); err != nil {
		return fail("Could not update .env.", err.Error())
	}

	// Storage reset on Docker: fetch the control plane's current schema (the
	// one the new images write), drop the old events table, and re-run the
	// bundle's one-shot init service against it before the services roll.
	if reset {
		base := readEnvVar(envPath, "CONTROL_PLANE_HTTP_URL")
		sp = ui.Start("Fetching the current ClickHouse schema")
		wd, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			sp.Fail("Could not enter %s", dir)
			return fail("Could not enter the project directory.", err.Error())
		}
		ferr := fetchBundleFile(base, "clickhouse-init.sql", ".")
		_ = os.Chdir(wd)
		if ferr != nil {
			sp.Fail("Could not download clickhouse-init.sql")
			return diagnose(ferr.Error())
		}
		sp.Success("Schema fetched from %s", base)

		sp = ui.Start("Recreating the events table with the new schema")
		if out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "exec", "-T", "clickhouse", "sh", "-c", clickhouseDropEvents); err != nil {
			sp.Fail("Could not drop the old events table")
			return diagnose(out)
		}
		if out, err := runCaptured("docker", "compose", "--project-directory", dir, "--env-file", envPath, "run", "--rm", "--no-deps", "clickhouse-init"); err != nil {
			sp.Fail("Schema init failed")
			return diagnose(out)
		}
		sp.Success("Events table recreated")
	}

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
	if reset {
		boxLines = append(boxLines, "", ui.Yellow("Event history was discarded (storage reset); profiles, traits, segments and journeys were kept."))
	} else {
		boxLines = append(boxLines, "", ui.Green("Named volumes (your data) were not touched."))
	}
	ui.Box("Upgraded", boxLines)
	return nil
}
