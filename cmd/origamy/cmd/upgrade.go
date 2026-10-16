package cmd

import (
	"cmp"
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
prompt. On Docker the project's schema decides rather than its tag (CLI
v0.1.18 and older pinned :main), the reset runs only with the target's
bundle (ClickHouse 25.3) and one that fails half-way is recorded in .env
(` + resetPendingVar + `): the next upgrade finishes it before anything else.

Config signing: a key stored at deploy (Docker: ` + signingKeyCopyVar + ` in .env;
Kubernetes: the ` + configSigningSecret + ` Secret) is applied once the target release is
` + minSigningVersion + ` or newer, and dropped again on Docker when moving below it. A plane
deployed without a key has none to apply: re-deploy with a fresh token, or set
it by hand (Docker: CONFIG_SIGNING_KEY in .env; Kubernetes:
--set controlPlane.publicKey=<key>). --require-signed-config turns on strict
mode, where the services refuse to start without the key.

Docker installs also refresh the compose bundle (docker-compose.yml,
clickhouse-init.sql, clickhouse-users.xml, Caddyfile) from the control plane,
but only when it serves the bundle for exactly the target release
(/byod/VERSION). Otherwise (an older control plane, a --version other than
the one it serves, --channel edge) the files are left as they are and only
the image tag moves. A file you have not edited is replaced and backed up as
<file>.bak-<the release it came with>; one you edited is kept, with the new
version next to it as <file>.new. The new compose file is checked against
your .env and the images are pulled with it before any file is replaced, so
a failed pull leaves the project as it was. --replace-bundle replaces edited
files too (still backed up); --keep-bundle leaves the bundle alone.`,
	Example: `  origamy upgrade                 # Kubernetes: latest published chart; Docker: this CLI's release
  origamy upgrade --version 0.1.20
  origamy upgrade --channel edge  # track the moving :staging images; not for production
  origamy upgrade --enable-ai     # switch the AI engine on
  origamy upgrade --disable-ai    # switch the AI engine off
  origamy upgrade --enable-predictor   # switch the predictor (scoring) service on
  origamy upgrade --disable-predictor  # switch the predictor off
  origamy upgrade --require-signed-config  # refuse to run on unverified config
  origamy upgrade --replace-bundle     # Docker: also replace bundle files you edited (backed up)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		version, _ := cmd.Flags().GetString("version")
		channel, _ := cmd.Flags().GetString("channel")
		sets, _ := cmd.Flags().GetStringArray("set")
		enableAI, _ := cmd.Flags().GetBool("enable-ai")
		disableAI, _ := cmd.Flags().GetBool("disable-ai")
		enablePredictor, _ := cmd.Flags().GetBool("enable-predictor")
		disablePredictor, _ := cmd.Flags().GetBool("disable-predictor")
		yes, _ := cmd.Flags().GetBool("yes")
		requireSigned, _ := cmd.Flags().GetBool("require-signed-config")
		replaceBundle, _ := cmd.Flags().GetBool("replace-bundle")
		keepBundle, _ := cmd.Flags().GetBool("keep-bundle")
		return runUpgrade(version, channel, sets, enableAI, disableAI, enablePredictor, disablePredictor, requireSigned, yes, replaceBundle, keepBundle)
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
	upgradeCmd.Flags().Bool("require-signed-config", false, "Make the services refuse to start without the control plane's config signing key (release "+minSigningVersion+"+)")
	upgradeCmd.Flags().Bool("replace-bundle", false, "Docker: also replace compose bundle files you have edited (each is backed up as <file>.bak-<release>)")
	upgradeCmd.Flags().Bool("keep-bundle", false, "Docker: leave the compose bundle files as they are; only move the image tag")
}

func runUpgrade(version, channel string, sets []string, enableAI, disableAI, enablePredictor, disablePredictor, requireSigned, assumeYes, replaceBundle, keepBundle bool) error {
	// Validate the toggles up front (before any registry/cluster calls) so a
	// conflicting request fails fast with a clear message.
	if _, err := aiToggleArgs(enableAI, disableAI); err != nil {
		return fail(err.Error(), "Pass one of --enable-ai or --disable-ai, not both.")
	}
	if _, err := predictorToggleArgs(enablePredictor, disablePredictor); err != nil {
		return fail(err.Error(), "Pass one of --enable-predictor or --disable-predictor, not both.")
	}
	bundle, err := bundleModeFlags(replaceBundle, keepBundle)
	if err != nil {
		return fail(err.Error(), "Pass one of --replace-bundle or --keep-bundle, not both.")
	}
	ui.Title("Origamy data plane — upgrade")
	switch {
	case hasKubernetes() && releaseInstalled():
		if bundle != bundleAuto {
			return fail("--replace-bundle/--keep-bundle apply to Docker installs only.",
				"A Kubernetes install's configuration comes with the chart, which the upgrade replaces as a whole.")
		}
		return upgradeKubernetes(version, channel, sets, enableAI, disableAI, enablePredictor, disablePredictor, requireSigned, assumeYes)
	case hasDocker():
		if len(sets) > 0 {
			return fail("--set applies to Kubernetes installs only.",
				"Docker installs configure via .env; edit it directly and rerun without --set.")
		}
		if enablePredictor || disablePredictor {
			return fail("--enable-predictor/--disable-predictor apply to Kubernetes installs only.",
				"The predictor runs on Kubernetes data planes; the Docker compose bundle doesn't include it.")
		}
		return upgradeDocker(version, channel, enableAI, disableAI, requireSigned, assumeYes, bundle)
	default:
		return fail("No existing Origamy data plane found on this machine.",
			"Run `origamy deploy --token …` first, or point kubectl/Docker at the host where it's installed.")
	}
}

func upgradeKubernetes(version, channel string, sets []string, enableAI, disableAI, enablePredictor, disablePredictor, requireSigned, assumeYes bool) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fail("Helm is required for Kubernetes upgrades.",
			"Install it from https://helm.sh/docs/intro/install/ and retry.")
	}

	// A toggle means the run's purpose includes flipping a feature boolean
	// (orchestratorEngine.enabled / predictor.enabled / strict signing) or
	// setting a value (--set, e.g. controlPlane.publicKey), which forces the
	// upgrade to proceed even when already on the target version.
	aiToggle := enableAI || disableAI
	predictorToggle := enablePredictor || disablePredictor
	toggle := aiToggle || predictorToggle || requireSigned || len(sets) > 0

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

	// A release on an external ClickHouse needs a chart that can still reach
	// it: an older one renders a password-less DSN, never creates the schema
	// on that server and blocks egress to it.
	external, signing := false, false
	if vals, err := releaseValues(); err == nil {
		external = usesExternalClickHouse(vals)
		signing = signingConfigured(vals)
	}
	if external && versionLess(target, minChartExternalClickHouse) {
		return fail(fmt.Sprintf("This data plane uses an external ClickHouse, which needs chart %s or newer (targeting %s).", minChartExternalClickHouse, target),
			"Pass --version "+minChartExternalClickHouse+" or newer.")
	}
	// Likewise a release that verifies signed config: an older chart given the
	// key rejects all config.
	if signing && !signingSupported(target) {
		return fail(fmt.Sprintf("This data plane verifies signed config, which needs chart %s or newer (targeting %s).", minSigningVersion, target),
			"Pass --version "+minSigningVersion+" or newer, or return to an earlier revision with `origamy rollback`.")
	}
	// A key stored at deploy on a chart that could not verify yet is wired in
	// now that the target can. There is no control-plane endpoint to fetch a
	// missing key from with the plane's credentials, so without one the
	// operator re-deploys with a fresh token or sets it by hand.
	stored := !signing && signingSupported(target) && runQuiet("kubectl", "get", "secret", configSigningSecret, "-n", namespace) == nil
	if requireSigned {
		if why := requireSigningProblem(target, signing || stored); why != "" {
			return fail(why, "Upgrade to "+minSigningVersion+" or newer with the key set first: re-deploy with a fresh token, or pass --set controlPlane.publicKey=<key from your dashboard's Kubernetes install snippet>.")
		}
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
	args = append(args, signingSetArgs(stored, requireSigned)...)

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
	// An external ClickHouse has no pod here to run that in; the operator owns
	// its tables.
	if reset && external {
		ui.Warn("External ClickHouse: if %s on your server predates the storage reset, drop it there and apply the chart's schema with your own client:", eventsTable)
		ui.Detail("kubectl get configmap %s-clickhouse-init -n %s -o jsonpath='{.data.init\\.sql}' | clickhouse-client --host … --port … --user … --password … [--secure] --multiquery", release, namespace)
	} else if reset {
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
	if line := signingSummary(signing || stored, signingSupported(target), requireSigned); line != "" {
		boxLines = append(boxLines, ui.Gray("Config     ")+line)
	}
	if reset {
		boxLines = append(boxLines, "", ui.Yellow("Event history was discarded (storage reset); profiles, traits, segments and journeys were kept."))
	} else {
		boxLines = append(boxLines, "", ui.Green("Your data (ClickHouse/Postgres/NATS/Redis) was not touched."))
	}
	boxLines = append(boxLines, ui.Gray("Roll back with: ")+"origamy rollback")
	if !signing && !stored && signingSupported(target) {
		boxLines = append(boxLines, "", ui.Gray("Config signing is not set up (no verify key stored). If your control plane signs config:"),
			"  origamy upgrade --set controlPlane.publicKey=<key from your dashboard's Kubernetes install snippet>")
	}
	ui.Box("Upgraded", boxLines)
	return nil
}

// confirmSchemaReset explains what crossing the storage reset means and asks
// for an explicit yes (unless --yes).
func confirmSchemaReset(from, to string, assumeYes bool) bool {
	ui.Warn("Upgrading %s → %s crosses the data plane's storage reset.", from, to)
	return confirmResetDetails(assumeYes)
}

// confirmResumedReset is confirmSchemaReset for a reset an earlier run began
// and did not finish (resetPendingVar names its target).
func confirmResumedReset(pending, to string, assumeYes bool) bool {
	ui.Warn("An earlier upgrade to %s began the storage reset and did not finish it (%s in .env); this run finishes it before %s starts.", pending, resetPendingVar, to)
	return confirmResetDetails(assumeYes)
}

func confirmResetDetails(assumeYes bool) bool {
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

// clickhouseReadyTimeout bounds the wait for a Docker project's ClickHouse to
// answer before the storage reset; clickhouseReadyPoll is how often it asks.
// Variables so tests need not wait.
var (
	clickhouseReadyTimeout = 3 * time.Minute
	clickhouseReadyPoll    = 2 * time.Second
)

// resetPendingVar records in a Docker project's .env a storage reset that has
// begun and not finished, as the release it moves to. It is set before
// anything is replaced or dropped and removed once the schema is applied;
// .env names the new release (DP_IMAGE_TAG) only after that. While it is set,
// `origamy upgrade` and an in-place `origamy deploy` redo the reset (asking
// again) before the services start, so a reset that failed half-way is never
// taken for a finished one. Every step can run again: the drop is IF EXISTS
// and the bundle's schema creates everything IF NOT EXISTS.
const resetPendingVar = "ORIGAMY_RESET_PENDING"

// upgradeCommand is the command that moves a Docker plane to tag again.
func upgradeCommand(tag string) string {
	if tag == dockerEdgeTag {
		return "origamy upgrade --channel edge"
	}
	return "origamy upgrade --version " + tag
}

// resetDockerEvents recreates a Docker project's events table with the schema
// now in clickhouse-init.sql. ClickHouse is first brought up on the image the
// project's compose file names now and waited for: a refreshed bundle moves it
// to a newer server (24.8 → 25.3 for a project from before the reset), whose
// JSON columns the new schema needs, and the old container would otherwise
// still be the one running until the `up -d` after this.
func resetDockerEvents(dir, envPath, tag string) error {
	compose := func(args ...string) (string, error) {
		return dockerRun([]string{"DP_IMAGE_TAG=" + tag}, "docker", append([]string{"compose", "--project-directory", dir, "--env-file", envPath}, args...)...)
	}
	// Every failure leaves the reset recorded in .env, and a rerun finishes it.
	rerun := "rerun `" + upgradeCommand(tag) + "`: the unfinished reset is recorded in .env (" + resetPendingVar + ") and is finished before anything else."
	sp := ui.Start("Starting ClickHouse")
	if out, err := compose("up", "-d", "clickhouse"); err != nil {
		sp.Fail("Could not start ClickHouse")
		return fail("ClickHouse did not start, so the events table was not reset yet.", outputHint(out)+"Fix the cause, then "+rerun)
	}
	deadline := time.Now().Add(clickhouseReadyTimeout)
	for {
		if _, err := compose("exec", "-T", "clickhouse", "sh", "-c", clickhousePing); err == nil {
			break
		}
		if time.Now().After(deadline) {
			sp.Fail("ClickHouse did not answer within %s", clickhouseReadyTimeout)
			return fail("ClickHouse is not accepting queries, so the events table was not reset yet.",
				"Check: docker compose --project-directory "+dir+" --env-file "+envPath+" logs clickhouse\nOnce it answers, "+rerun)
		}
		time.Sleep(clickhouseReadyPoll)
	}
	sp.Success("ClickHouse is up")

	sp = ui.Start("Recreating the events table with the new schema")
	if out, err := compose("exec", "-T", "clickhouse", "sh", "-c", clickhouseDropEvents); err != nil {
		sp.Fail("Could not drop the old events table")
		return fail("The old events table could not be dropped.", outputHint(out)+"Fix the cause, then "+rerun)
	}
	if out, err := compose("run", "--rm", "--no-deps", "clickhouse-init"); err != nil {
		sp.Fail("Schema init failed")
		return fail("The old events table was dropped, but the new schema could not be applied.", outputHint(out)+"Fix the cause, then "+rerun)
	}
	sp.Success("Events table recreated")
	return nil
}

// runDockerReset is the storage reset of a project whose .env records it
// (resetPendingVar): resetDockerEvents, then the record is cleared.
func runDockerReset(dir, envPath, tag string) error {
	if err := resetDockerEvents(dir, envPath, tag); err != nil {
		return err
	}
	if err := unsetEnvVar(envPath, resetPendingVar); err != nil {
		return fail("The events table was recreated, but .env could not be updated.",
			err.Error()+"\nRemove the "+resetPendingVar+" line from "+envPath+" by hand, then rerun `"+upgradeCommand(tag)+"`.")
	}
	return nil
}

// outputHint is the tail of a failed command's output as the first lines of a
// fail hint ("" when there was none).
func outputHint(out string) string {
	if tail := lastLines(out, 6); tail != "" {
		return tail + "\n"
	}
	return ""
}

// dockerEdgeTag is what `--channel edge` runs on Docker. The :staging tag is
// rebuilt for every service on each staging deploy; :main is not (the
// data-plane's main branch is development-only), so it lags for months and
// is missing entirely for the AI engine and predictor images.
const dockerEdgeTag = "staging"

func upgradeDocker(version, channel string, enableAI, disableAI, requireSigned, assumeYes bool, bundle bundleMode) error {
	dir, err := composeProjectDir()
	if err != nil {
		return fail("No Origamy compose project found here.",
			err.Error()+"\ncd into your data-plane directory (e.g. ./origamy-dp-<id>) and retry, or run `origamy deploy` first.")
	}
	envPath := filepath.Join(dir, ".env")
	installedTag := readEnvVar(envPath, "DP_IMAGE_TAG")
	// The bundle download of an upgrade interrupted before its swap.
	if stale := removeStaleBundleDirs(dir); len(stale) > 0 {
		ui.Step("Removed the bundle download an interrupted upgrade left behind (%s).", strings.Join(stale, ", "))
	}
	// Where the bundle and the schema come from. Projects deployed by CLI
	// v0.1.18 or older only have it as CONFIG_URL; it is recorded as
	// CONTROL_PLANE_HTTP_URL once the upgrade goes through (backfillDockerEnv).
	base, _ := controlPlaneHTTPBase(envPath)

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
	// (:main, which CLI v0.1.18 and older pinned, is months older than any
	// release this moves to, so only :staging warrants the warning.)
	_, targetSemver := parseVersion(tag)
	switch {
	case installedTag != "" && versionLess(tag, installedTag):
		ui.Warn("Moving from %s back to %s — an older release may not understand data written by the newer one.", installedTag, tag)
	case installedTag == dockerEdgeTag && targetSemver:
		ui.Warn("Moving from the moving %s images to release %s may be a downgrade; pass --channel edge to stay on %s.", installedTag, tag, installedTag)
	}

	// Storage reset. One an earlier run began and did not finish
	// (resetPendingVar) is finished first, whatever the tags say. Otherwise
	// the schema generations decide (dockerSchemaResetNeeded): the one this
	// project's ClickHouse was created from and holds, against the one tag
	// writes.
	pending := readEnvVar(envPath, resetPendingVar)
	installedSQL, _ := os.ReadFile(filepath.Join(dir, "clickhouse-init.sql"))
	reset, live := pending != "", ""
	if !reset && releaseSchemaGeneration(tag) == "json" {
		live = liveSchemaGeneration(dir, envPath)
		reset = dockerSchemaResetNeeded(installedTag, schemaGeneration(string(installedSQL)), live, tag)
	}
	if pending != "" && releaseSchemaGeneration(tag) == "string" {
		return fail(fmt.Sprintf("An earlier upgrade to %s began the storage reset and did not finish it (%s in .env), and release %s cannot write the schema it moves to.", pending, resetPendingVar, tag),
			"Finish it first: "+upgradeCommand(pending))
	}
	// The reset recreates the events table from the control plane's current
	// schema, so it cannot start without knowing where that is.
	if reset && base == "" {
		return fail("The control plane URL is missing from .env (no CONTROL_PLANE_HTTP_URL or CONFIG_URL), and crossing the storage reset needs its current ClickHouse schema.",
			"Re-deploy with `origamy deploy --token …`, or add CONTROL_PLANE_HTTP_URL=https://<your control plane> to "+envPath+" and retry.")
	}
	if reset {
		from := cmp.Or(installedTag, "the installed release")
		var yes bool
		switch {
		case pending != "":
			yes = confirmResumedReset(pending, tag, assumeYes)
		case live == "string":
			// Say why when the tags alone would not: an earlier upgrade can
			// have moved them on and left the old table behind.
			ui.Warn("ClickHouse here still holds the pre-reset events table (String payload columns), which %s cannot write: upgrading %s → %s crosses the data plane's storage reset.", tag, from, tag)
			yes = confirmResetDetails(assumeYes)
		default:
			yes = confirmSchemaReset(from, tag, assumeYes)
		}
		if !yes {
			return fail("Cancelled.", "")
		}
	}

	// Config signing: the CLI's copy of the verify key, or a CONFIG_SIGNING_KEY
	// set by hand or by the dashboard's snippet. Applied, or dropped again,
	// below together with the tag. There is no control-plane endpoint to fetch
	// a missing key from with the plane's credentials.
	signingKey := readEnvVar(envPath, signingKeyCopyVar)
	if signingKey == "" {
		signingKey = readEnvVar(envPath, "CONFIG_SIGNING_KEY")
	}
	if requireSigned {
		if why := requireSigningProblem(tag, signingKey != ""); why != "" {
			return fail(why, "Pass --version "+minSigningVersion+" or newer, and set the key first: re-deploy with a fresh token, or put the key from your dashboard's Docker install snippet in CONFIG_SIGNING_KEY in "+envPath+".")
		}
	}

	// AI engine = the "agentic" compose profile. Enabling generates the engine's
	// secrets locally on first use (the KEK is never regenerated afterwards —
	// per-workspace LLM keys are encrypted under it) and points portal-agent
	// at the engine; disabling drops the profile and that pointer, keeping the
	// secrets so a later re-enable finds the same KEK. Written to .env once the
	// checks below pass, before the pull (which pulls the profiles' images).
	profiles := composeProfiles(envPath)
	aiVars := map[string]string{}
	switch {
	case enableAI:
		aiVars = map[string]string{
			"ORCH_KEK":                existingOrRandomKEK(envPath, "ORCH_KEK"),
			"ORCH_ENGINE_API_TOKEN":   existingOrRandom(envPath, "ORCH_ENGINE_API_TOKEN"),
			"ORCHESTRATOR_ENGINE_URL": orchestratorEngineURL,
		}
		profiles = withProfile(profiles, "agentic", true)
	case disableAI:
		aiVars = map[string]string{"ORCHESTRATOR_ENGINE_URL": ""}
		profiles = withProfile(profiles, "agentic", false)
	}
	if enableAI || disableAI {
		aiVars["COMPOSE_PROFILES"] = strings.Join(profiles, ",")
	}
	ui.KV("Directory", dir)
	ui.KV("Image tag", tag)
	ui.KV("Profiles", orDash(strings.Join(profiles, ",")))

	// The compose file, ClickHouse config and Caddyfile follow the release
	// too, and only the control plane has them (bundle.go). They are
	// refreshed when it serves the bundle for exactly this tag, and only the
	// files nobody edited here.
	refresh, bundleSkipped := planBundleRefresh(dir, envPath, base, tag, installedTag, bundle, reset)
	// abort drops the prepared refresh (its download directory) on the way out.
	abort := func(err error) error {
		if refresh != nil {
			refresh.cleanup()
		}
		return err
	}

	// What the project will run once the refresh is in is checked before
	// anything is touched. Across the reset: the ClickHouse its compose file
	// starts must hold the new schema, and the schema the reset applies (the
	// refreshed one, else the control plane's current one, fetched now) must
	// be the new one.
	var fetchedSQL []byte
	if reset {
		if why := resetComposeProblem(dir, refresh); why != "" {
			return abort(fail(resetComposeRefusal(tag, refresh, bundleSkipped, why)))
		}
		var resetSQL []byte
		if refresh != nil && refresh.served("clickhouse-init.sql") {
			resetSQL, _ = os.ReadFile(filepath.Join(refresh.tmp, "clickhouse-init.sql"))
		} else {
			sp := ui.Start("Fetching the current ClickHouse schema")
			b, err := fetchBundleBytes(base, "clickhouse-init.sql")
			if err != nil {
				sp.Fail("Could not download clickhouse-init.sql")
				return abort(diagnose(err.Error()))
			}
			sp.Success("Schema fetched from %s", base)
			resetSQL, fetchedSQL = b, b
		}
		if schemaGeneration(string(resetSQL)) == "string" {
			return abort(fail("Your control plane serves the pre-reset ClickHouse schema (String payload columns), so the storage reset cannot recreate the events table from it.",
				"Update the control plane first, then rerun `"+upgradeCommand(tag)+"`."))
		}
	}
	// Same guard as deploy, on the schema the project keeps when the refresh
	// does not replace it (a refreshed one was checked with the bundle). A
	// pre-reset String schema cannot trip it: moving that to a release that
	// writes JSON is the storage reset above.
	if !reset && (refresh == nil || !refresh.served("clickhouse-init.sql")) &&
		schemaGeneration(string(installedSQL)) == "json" && releaseSchemaGeneration(tag) == "string" {
		return abort(fail(fmt.Sprintf("This data plane's ClickHouse schema is the post-reset one (native JSON payload columns), which release %s cannot write.", tag),
			"Pass --version "+minImageForJSONSchema+" or newer; plain `origamy upgrade` moves to "+helmVersion+"."))
	}

	for k, v := range aiVars {
		if err := setEnvVar(envPath, k, v); err != nil {
			return abort(fail("Could not update .env.", err.Error()))
		}
	}

	// Pull BEFORE touching the project: a tag that does not exist for one of
	// the services (or a registry outage) must leave .env and the bundle files
	// — and therefore every later `docker compose up` — on the release that is
	// actually running. The process environment wins over the env file in
	// compose interpolation, so the pull sees the candidate tag without
	// writing it, and a refreshed compose file is pulled with from where it
	// was downloaded, so images of services it adds come too.
	sp := ui.Start("Pulling %s images", tag)
	if out, err := pullImages(dir, envPath, tag, refresh); err != nil {
		sp.Fail("docker compose pull failed")
		if refresh != nil {
			refresh.cleanup()
			ui.Step("Bundle files left as they were.")
		}
		return diagnose(out)
	}
	sp.Success("Images pulled")
	// From here on the reset is under way. It is recorded before any file is
	// replaced or anything dropped, so whatever fails next, the next run
	// finishes it.
	if reset {
		if err := setEnvVar(envPath, resetPendingVar, tag); err != nil {
			return abort(fail("Could not update .env.", err.Error()))
		}
	}
	if refresh != nil {
		if err := refresh.apply(); err != nil {
			if rerr := refresh.rollback(); rerr != nil {
				return fail("Could not swap in the new bundle files, nor put the previous ones back.",
					err.Error()+"\n"+rerr.Error()+"\nThe backups are next to them in "+dir+" (*.bak-*).")
			}
			if reset {
				// Nothing was dropped yet, and the reset needs these files.
				if pending == "" {
					_ = unsetEnvVar(envPath, resetPendingVar)
				}
				return fail("Could not swap in the new bundle files the storage reset needs; they were put back as they were.",
					err.Error()+"\nFix the cause and rerun `"+upgradeCommand(tag)+"`.")
			}
			ui.Warn("Bundle files left as-is: could not swap them in (%v)", err)
			refresh, bundleSkipped = nil, "swap failed"
		}
	}
	if refresh != nil {
		if err := refresh.commit(); err != nil {
			ui.Warn("Could not record the refreshed bundle in %s (%v); the next upgrade may take these files for edits.", bundleManifestFile, err)
		}
		for _, f := range refresh.overwritten() {
			ui.Warn("Replaced your edited %s with the %s version; yours is backed up as %s", f.name, tag, filepath.Base(f.backup))
		}
		for _, name := range refresh.kept() {
			ui.Warn("Kept your edited %s; the %s version is next to it as %s.new", name, tag, name)
			ui.Detail("Compare: diff -u %s %s.new", filepath.Join(dir, name), filepath.Join(dir, name))
		}
		for _, name := range refresh.removedNew {
			ui.Step("Removed %s, left by an earlier upgrade: %s is the %s version now.", name, strings.TrimSuffix(name, ".new"), tag)
		}
	}

	// Storage reset on Docker: drop the old events table and re-run the
	// bundle's one-shot init service with the new schema (the refreshed one,
	// forced in across the reset, or the one fetched above) before the
	// services roll.
	if reset {
		if fetchedSQL != nil {
			if err := os.WriteFile(filepath.Join(dir, "clickhouse-init.sql"), fetchedSQL, 0o644); err != nil {
				return fail("Could not write clickhouse-init.sql.", err.Error()+"\nFix the cause and rerun `"+upgradeCommand(tag)+"`.")
			}
			// The control plane's file now, not an edit, for the next upgrade.
			_ = recordBundleFile(dir, "clickhouse-init.sql")
		}
		if err := runDockerReset(dir, envPath, tag); err != nil {
			return err
		}
	}

	// Only now does .env name the new release: until here every `docker
	// compose up`, and the next upgrade, still see the one that runs.
	if err := setEnvVar(envPath, "DP_IMAGE_TAG", tag); err != nil {
		return fail("Could not update .env.", err.Error())
	}
	notes, err := backfillDockerEnv(dir, envPath, base)
	for _, n := range notes {
		ui.Step("%s", n)
	}
	if err != nil {
		return fail("Could not update .env.", err.Error())
	}
	// The key follows the tag: only a release that verifies gets
	// CONFIG_SIGNING_KEY, so a move below minSigningVersion blanks it.
	signingVars := map[string]string{}
	if signingKey != "" {
		signingVars = dockerSigningEnv(signingKey, tag)
	}
	if requireSigned {
		signingVars["CONFIG_SIGNING_REQUIRED"] = "true"
	}
	for k, v := range signingVars {
		if err := setEnvVar(envPath, k, v); err != nil {
			return fail("Could not update .env.", err.Error())
		}
	}

	// --remove-orphans stops containers of profiles that were just turned off
	// (compose otherwise leaves them running); named volumes are untouched.
	sp = ui.Start("Restarting services")
	if out, err := dockerRun(nil, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "up", "-d", "--remove-orphans"); err != nil {
		sp.Fail("docker compose up failed")
		return diagnose(out)
	}
	sp.Success("Services restarted")
	if refresh != nil {
		for _, svc := range refresh.restarts(profiles) {
			sp = ui.Start("Restarting %s for its new config file", svc)
			if _, err := dockerRun(nil, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "restart", svc); err != nil {
				sp.Warn("Could not restart %s; run: docker compose --project-directory %s --env-file %s restart %s", svc, dir, envPath, svc)
			} else {
				sp.Success("Restarted %s", svc)
			}
		}
	}

	boxLines := []string{
		ui.Gray("Image tag  ") + ui.Bold(tag),
		ui.Gray("Directory  ") + dir,
	}
	boxLines = append(boxLines, bundleBoxLines(refresh, bundleSkipped)...)
	if enableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+ui.Green("enabled"))
	} else if disableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+"disabled")
	}
	if line := signingSummary(signingKey != "", signingSupported(tag), requireSigned); line != "" {
		boxLines = append(boxLines, ui.Gray("Config     ")+line)
	}
	if reset {
		boxLines = append(boxLines, "", ui.Yellow("Event history was discarded (storage reset); profiles, traits, segments and journeys were kept."))
	} else {
		boxLines = append(boxLines, "", ui.Green("Named volumes (your data) were not touched."))
	}
	if signingKey == "" && signingSupported(tag) {
		boxLines = append(boxLines, "", ui.Gray("Config signing is not set up (no verify key stored). If your control plane signs config,"),
			ui.Gray("set CONFIG_SIGNING_KEY in .env to the key from your dashboard's Docker install snippet and rerun `origamy upgrade`."))
	}
	ui.Box("Upgraded", boxLines)
	return nil
}

// resetComposeProblem says why the storage reset cannot run on the compose
// file the project has once refresh r (nil: none) is in — the served one when
// r takes it, else the project's own — "" when it can.
func resetComposeProblem(dir string, r *bundleRefresh) string {
	path := filepath.Join(dir, "docker-compose.yml")
	if r != nil && r.served("docker-compose.yml") {
		path = filepath.Join(r.tmp, "docker-compose.yml")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return composeClickHouseProblem(b)
}

// resetComposeRefusal explains a refusal by resetComposeProblem (why): the
// compose file at fault is the served one when refresh r takes it, the
// operator's edited one when r keeps it, else the project's own, which
// planBundleRefresh did not refresh for the reason skipped.
func resetComposeRefusal(tag string, r *bundleRefresh, skipped, why string) (headline, hint string) {
	switch {
	case r != nil && r.served("docker-compose.yml"):
		return fmt.Sprintf("The storage reset needs ClickHouse %s or newer, and the docker-compose.yml your control plane serves for %s does not run it (%s).", minClickHouseForJSON, tag, why),
			"Update the control plane, then rerun `" + upgradeCommand(tag) + "`."
	case r != nil:
		return fmt.Sprintf("The storage reset needs ClickHouse %s or newer, and your edited docker-compose.yml, which the refresh keeps, does not run it (%s).", minClickHouseForJSON, why),
			"Pass --replace-bundle to take the " + tag + " compose file (yours is backed up next to it) and re-apply your edits afterwards,\nor move its clickhouse and clickhouse-init services to clickhouse/clickhouse-server:" + minClickHouseForJSON + " yourself and rerun."
	}
	headline = fmt.Sprintf("The storage reset needs ClickHouse %s or newer, which comes with the %s bundle, but the bundle files cannot be refreshed (%s) and this project's docker-compose.yml does not run it (%s).", minClickHouseForJSON, tag, skipped, why)
	switch {
	case skipped == "--keep-bundle":
		hint = "Rerun without --keep-bundle."
	case skipped == "control plane has no /byod/VERSION":
		hint = "Your control plane must serve /byod/VERSION for " + tag + " before the CLI takes its bundle: update the control plane, then rerun `origamy upgrade`.\nOr re-deploy here with a fresh token (`origamy deploy --token …`), which downloads its current bundle."
	case strings.HasPrefix(skipped, "control plane serves "):
		hint = "Pass --version " + strings.TrimPrefix(skipped, "control plane serves ") + " (the release your control plane's bundle is for), or update the control plane to serve " + tag + "."
	default:
		hint = "Fix the cause above, then rerun `origamy upgrade`."
	}
	return headline, hint
}
