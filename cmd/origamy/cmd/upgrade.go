package cmd

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
credential in your dashboard. On Docker .env takes the toggle only once the
upgrade is through: one that fails or is refused before (a failed pull, say)
leaves .env as it was.

Use --enable-predictor / --disable-predictor the same way for the predictor
(conversion scoring) service — Kubernetes only.

Moving from a release before ` + minImageForJSONSchema + ` to ` + minImageForJSONSchema + ` or later crosses the data
plane's storage reset: ClickHouse moves to 25.3 and the events table is
recreated with native JSON columns, which discards the event history
collected so far (profiles, traits, segments and journeys are kept). The
upgrade explains this and asks for confirmation; pass --yes to skip the
prompt. On Docker the running ClickHouse decides, never the tags or
clickhouse-init.sql: for a release that writes the JSON columns, an events
table with String columns is moved aside (renamed) and dropped once the new
one is in place (asking first), and a missing one created (nothing to ask);
one that already has the JSON columns is never dropped, and moving it to a
release before ` + minImageForJSONSchema + `, or an events table of any other type, is refused.
A ClickHouse that does not answer is started on the release and files the
project runs now and asked again; when it still cannot be asked, the upgrade
is refused. The reset runs only on ClickHouse 25.3 or newer (as the
project's compose configuration, overrides included, names it) with your
control plane's new schema, and one that fails half-way is recorded in .env
(` + resetPendingVar + `): the next upgrade finishes it before anything else.
Once the services are up again, the upgrade checks that ClickHouse holds
the events table the release writes, and that the containers run the
release's images (every docker compose command gets the release in
DP_IMAGE_TAG, whatever the shell exports), and fails when they do not
(recording the reset still to do). One upgrade, in-place deploy or
uninstall works on a project at a time (an operating system lock on
` + projectLockFile + ` in it, gone with its process); Ctrl-C ends an upgrade with exit
code 130 and leaves neither the lock nor a download.

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
// for an explicit yes (unless --yes). notes are said before the question.
func confirmSchemaReset(from, to string, assumeYes bool, notes ...string) bool {
	ui.Warn("Upgrading %s → %s crosses the data plane's storage reset.", from, to)
	return confirmResetDetails(assumeYes, notes...)
}

// confirmResumedReset is confirmSchemaReset for a reset an earlier run began
// and did not finish (resetPendingVar names its target).
func confirmResumedReset(pending, to string, assumeYes bool, notes ...string) bool {
	ui.Warn("An earlier upgrade to %s began the storage reset and did not finish it (%s in .env); this run finishes it before %s starts.", pending, resetPendingVar, to)
	return confirmResetDetails(assumeYes, notes...)
}

func confirmResetDetails(assumeYes bool, notes ...string) bool {
	ui.Detail("ClickHouse moves to 25.3 and the events table is recreated with native JSON payload columns.")
	ui.Detail("The event history collected so far is DISCARDED. Profiles, traits, segments and journeys are kept.")
	for _, n := range notes {
		if n != "" {
			ui.Detail("%s", n)
		}
	}
	if assumeYes {
		return true
	}
	return promptString("Type 'yes' to continue") == "yes"
}

// cannotFinishReset says why the images of tag cannot finish a storage
// reset: only a numbered release from minImageForJSONSchema on, or the edge
// channel's :staging, writes the schema it moves to.
func cannotFinishReset(tag string) string {
	if _, semver := parseVersion(tag); semver {
		return "release " + tag + " cannot write the schema it moves to"
	}
	return "the :" + tag + " images are not a numbered release, so it is not known that they write the schema it moves to"
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

// clickhouseReadyTimeout bounds each wait for a Docker project's ClickHouse to
// answer (once started to be asked, before the storage reset, after the last
// `up -d`); clickhouseReadyPoll is how often it asks. Variables so tests need
// not wait.
var (
	clickhouseReadyTimeout = 3 * time.Minute
	clickhouseReadyPoll    = 2 * time.Second
)

// resetPendingVar records in a Docker project's .env a storage reset that has
// begun and not finished, as the release it moves to. It is set before
// anything is replaced or dropped, and by the check after `up -d` when that
// finds the events table missing or pre-reset under a release that writes the
// new one; it is removed once the reset is done, the services that bind-mount
// bundle files were restarted and the check passed. .env names the new
// release (DP_IMAGE_TAG) only after the schema is applied. While it is set,
// `origamy upgrade` and an in-place `origamy deploy` finish the reset before
// the services start, and refuse a release that does not write the new
// schema, so a reset that failed half-way is never taken for a finished one.
// What ClickHouse holds decides how: the pre-reset table is dropped (asking
// again), a missing one created, and the new one left as it is (nothing is
// asked for either). Every step can run again: the bundle's schema creates
// everything IF NOT EXISTS.
const resetPendingVar = "ORIGAMY_RESET_PENDING"

// upgradeCommand is the command that moves a Docker plane to tag again.
func upgradeCommand(tag string) string {
	if tag == dockerEdgeTag {
		return "origamy upgrade --channel edge"
	}
	return "origamy upgrade --version " + tag
}

// resetOutcome is what a Docker storage reset found and did.
type resetOutcome int

const (
	resetNone    resetOutcome = iota // no reset ran
	resetDropped                     // the pre-reset table was moved aside and the new one created
	resetCreated                     // there was no events table (an earlier run moved it aside or dropped it): it was created
	resetAlready                     // ClickHouse already held the new table: nothing moved or created
)

// resetResult is what a Docker storage reset did: its outcome, and the tables
// a reset moved aside (events.events_pre_reset_…) that it dropped once the
// new one was in place, or kept in ClickHouse (see settleAside).
type resetResult struct {
	outcome       resetOutcome
	dropped, kept []string
}

// resetDockerEvents recreates a Docker project's events table with the schema
// now in clickhouse-init.sql. ClickHouse is first brought up on the image the
// project's compose file names now and waited for: a refreshed bundle moves it
// to a newer server (24.8 → 25.3 for a project from before the reset), whose
// JSON columns the new schema needs, and the old container would otherwise
// still be the one running until the `up -d` after this. env is the compose
// environment of the run (tagEnv(tag), and what it sets before .env does).
//
// What ClickHouse holds then decides, held against what it held when the run
// made its decision (found, a probeLiveSchema generation): the table is moved
// aside only when ClickHouse says it is the pre-reset one (String payload
// columns) and the run asked to drop it (found "string"); the schema is
// applied to that or to no table, unless the run found the new one (it then
// only checks); the new table is left alone. Nothing is moved or applied
// unless clickhouse-init.sql is the new schema. Moved aside is RENAME TABLE
// events.events TO events.events_pre_reset_<unix time>, not DROP: the reset
// counts as done only once ClickHouse reports the new table (the bundle's
// init service exits 0 even when a statement fails), and only then is the
// table moved aside dropped, while it is the pre-reset one (settleAside).
// discard says the operator agreed to discard the pre-reset table (this run
// asked, or the run that began the reset did), which drops what an earlier
// run moved aside too.
func resetDockerEvents(dir, envPath, tag, found string, env []string, discard bool) (resetResult, error) {
	compose := func(args ...string) (string, error) {
		return dockerRun(env, "docker", append([]string{"compose", "--project-directory", dir, "--env-file", envPath}, args...)...)
	}
	ch := clickhouseConn{dir: dir, envPath: envPath, env: env}
	none := resetResult{}
	// Every failure leaves the reset recorded in .env, and a rerun finishes it.
	rerun := "rerun `" + upgradeCommand(tag) + "`: the unfinished reset is recorded in .env (" + resetPendingVar + ") and is finished before anything else."
	sp := ui.Start("Starting ClickHouse")
	if out, err := compose("up", "-d", "clickhouse"); err != nil {
		sp.Fail("Could not start ClickHouse")
		return none, fail("ClickHouse did not start, so the events table was not reset yet.", outputHint(out)+"Fix the cause, then "+rerun)
	}
	if out, ok := ch.wait(); !ok {
		sp.Fail("ClickHouse did not answer within %s", clickhouseReadyTimeout)
		return none, fail("ClickHouse is not accepting queries, so the events table was not reset yet.",
			outputHint(out)+"Check: docker compose --project-directory "+dir+" --env-file "+envPath+" logs clickhouse\nOnce it answers, "+rerun)
	}
	sp.Success("ClickHouse is up")

	sp = ui.Start("Checking the events table")
	live := ch.probe()
	switch {
	case live.gen == "json":
		sp.Success("ClickHouse already holds the new events table (%s payload columns): nothing to move aside", live.typ)
		return settleAside(dir, envPath, compose, ch, resetResult{outcome: resetAlready}, "", discard), nil
	case live.gen == "":
		sp.Fail("ClickHouse did not say which events table it holds")
		return none, fail("Could not read which events table ClickHouse holds, so nothing was dropped.", outputHint(live.out)+"Once it answers, "+rerun)
	case live.gen == liveOther:
		sp.Fail("Unexpected events table")
		return none, fail(fmt.Sprintf("ClickHouse's %s has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, so the reset did not drop it.", eventsTable, live.typ),
			"Check what created it. To recreate it with the new schema, drop it yourself:\n"+
				"  docker compose --project-directory "+dir+" --env-file "+envPath+" exec clickhouse sh -c '"+strings.ReplaceAll(clickhouseDropEvents, " IF EXISTS", "")+"'\nthen "+rerun)
	case (live.gen == "string" && found != "string") || (live.gen == liveMissing && found == "json"):
		sp.Fail("ClickHouse holds %s", live.describe())
		return none, fail("ClickHouse holds "+live.describe()+", although it held "+liveSchema{gen: found}.describe()+" when this run began, so this run changed nothing in it.",
			"Rerun `"+upgradeCommand(tag)+"`: it asks again, before it drops anything.")
	case live.gen == "string":
		sp.Success("ClickHouse holds the pre-reset events table (String payload columns)")
	default:
		sp.Success("ClickHouse has no events table")
	}
	// The init service applies the project's clickhouse-init.sql: it must be
	// the schema the reset moves to before anything is moved or created.
	if sql, err := os.ReadFile(filepath.Join(dir, "clickhouse-init.sql")); err != nil || schemaGeneration(string(sql)) != "json" {
		sp.Fail("clickhouse-init.sql is not the new schema")
		return none, fail("clickhouse-init.sql in "+dir+" is not the new ClickHouse schema (native JSON payload columns), so nothing was dropped or created.",
			"Put the "+tag+" schema there (`"+upgradeCommand(tag)+"` takes your control plane's current one), then "+rerun)
	}

	sp = ui.Start("Creating the events table with the new schema")
	outcome, did, done, aside, kept := resetCreated, "The new schema", "Events table created", "", ""
	if live.gen == "string" {
		aside = asideName(ch)
		sp.Suffix("the pre-reset one is moved aside first, as events.%s", aside)
		if out, err := compose("exec", "-T", "clickhouse", "sh", "-c", clickhouseMoveEventsAside(aside)); err != nil {
			sp.Fail("Could not move the old events table aside")
			return none, fail("The old events table could not be moved aside (renamed to events."+aside+"), so nothing was changed in ClickHouse.", outputHint(out)+"Fix the cause, then "+rerun)
		}
		outcome, did, done = resetDropped, "The old events table was moved aside, but the new schema", "Events table recreated"
		kept = "The pre-reset events table is events." + aside + " now, its data still there: the next run drops it once the new table is in place.\n"
	}
	out, err := compose("run", "--rm", "--no-deps", "clickhouse-init")
	if err != nil {
		sp.Fail("Schema init failed")
		return none, fail(did+" could not be applied.", kept+outputHint(out)+"Fix the cause, then "+rerun)
	}
	switch got := ch.probe(); got.gen {
	case "json":
	case "":
		sp.Fail("Could not confirm the new events table")
		return none, fail("The schema was applied, but ClickHouse could not be asked whether "+eventsTable+" is the new table now.", kept+outputHint(got.out)+"Once it answers, "+rerun)
	default:
		sp.Fail("The events table is not the new one")
		have := map[string]string{"string": "still the pre-reset one (String payload columns)", liveMissing: "missing", liveOther: "of type " + got.typ}[got.gen]
		return none, fail("The schema was applied, but ClickHouse's "+eventsTable+" is "+have+" afterwards.",
			kept+outputHint(out)+"The schema's output is above. Fix the cause (docker compose --project-directory "+dir+" --env-file "+envPath+" logs clickhouse), then "+rerun)
	}
	sp.Success("%s", done)
	return settleAside(dir, envPath, compose, ch, resetResult{outcome: outcome}, aside, discard || aside != ""), nil
}

// asideName is the name the reset moves the pre-reset events table to,
// events_pre_reset_<unix time>, one no table moved aside before has.
func asideName(ch clickhouseConn) string {
	taken := map[string]bool{}
	if out, err := ch.exec(clickhouseAsideTables); err == nil {
		for name := range asideTables(out) {
			taken[name] = true
		}
	}
	for n := time.Now().Unix(); ; n++ {
		if name := fmt.Sprintf("%s%d", eventsAsidePrefix, n); !taken[name] {
			return name
		}
	}
}

// asideTables reads clickhouseAsideTables' answer: name → payload type.
func asideTables(out string) map[string]string {
	tables := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(l), "\t")
		if len(f) == 3 && f[2] == eventsAsideMark && reAsideTable.MatchString(f[0]) {
			tables[f[0]] = f[1]
		}
	}
	return tables
}

// settleAside ends a storage reset once ClickHouse holds the new events
// table. Each table a reset moved aside, this run (moved) or an earlier one
// that stopped before it got this far, is dropped when it is the pre-reset
// one (String payload columns) and the operator agreed to discard it
// (discard), and kept otherwise: one with the new (JSON) columns means the
// events table changed between the check and the move, and is kept with how
// to put it back. A table that cannot be checked or dropped is kept too.
func settleAside(dir, envPath string, compose func(...string) (string, error), ch clickhouseConn, r resetResult, moved string, discard bool) resetResult {
	run := "docker compose --project-directory " + dir + " --env-file " + envPath + " exec clickhouse sh -c '"
	out, err := ch.exec(clickhouseAsideTables)
	if err != nil {
		if moved != "" {
			r.kept = append(r.kept, moved)
			ui.Warn("Could not check events.%s, where the pre-reset events table was moved, so it was not dropped (%s). Drop it yourself once you no longer need its data:", moved, orDash(lastLines(out, 1)))
			ui.Detail("%s", run+clickhouseDropAside(moved)+"'")
		}
		return r
	}
	tables := asideTables(out)
	if _, ok := tables[moved]; moved != "" && !ok {
		r.kept = append(r.kept, moved)
		ui.Warn("ClickHouse no longer lists events.%s, where the pre-reset events table was moved: nothing was dropped.", moved)
	}
	names := slices.Sorted(maps.Keys(tables))
	for _, name := range names {
		typ := tables[name]
		switch {
		case strings.HasPrefix(typ, "String") && discard:
			sp := ui.Start("Dropping events.%s, the pre-reset events table moved aside", name)
			if out, err := compose("exec", "-T", "clickhouse", "sh", "-c", clickhouseDropAside(name)); err != nil {
				r.kept = append(r.kept, name)
				sp.Warn("Could not drop events.%s (%s); it is kept, with the pre-reset event history. Drop it yourself once you no longer need it:", name, orDash(lastLines(out, 1)))
				ui.Detail("%s", run+clickhouseDropAside(name)+"'")
				continue
			}
			r.dropped = append(r.dropped, name)
			sp.Success("Dropped events.%s, the pre-reset events table", name)
		case strings.HasPrefix(typ, "String"):
			r.kept = append(r.kept, name)
			ui.Warn("events.%s holds a pre-reset events table an earlier upgrade moved aside; it is kept. Drop it yourself once you no longer need its data:", name)
			ui.Detail("%s", run+clickhouseDropAside(name)+"'")
		case strings.HasPrefix(typ, "JSON"):
			r.kept = append(r.kept, name)
			ui.Warn("events.%s, which a storage reset moved aside, has the new (JSON) payload columns: %s changed between the check and the move. It is kept, with its data; the %s in its place is new. To put it back (the new one then goes by events.%s, to drop):", name, eventsTable, eventsTable, name)
			ui.Detail("%s", run+clickhouseRestoreAside(name)+"'")
			ui.Detail("%s", run+clickhouseDropAside(name)+"'")
		default:
			r.kept = append(r.kept, name)
			ui.Warn("events.%s, which a storage reset moved aside, has payload columns of type %s; it is kept. Drop it yourself once you no longer need it:", name, typ)
			ui.Detail("%s", run+clickhouseDropAside(name)+"'")
		}
	}
	return r
}

// clearResetRecord removes resetPendingVar once the reset is done and the
// services that mount bundle files were restarted.
func clearResetRecord(envPath, tag string) error {
	if err := unsetEnvVar(envPath, resetPendingVar); err != nil {
		return fail("The storage reset is finished, but .env could not be updated.",
			err.Error()+"\nRemove the "+resetPendingVar+" line from "+envPath+" by hand, or rerun `"+upgradeCommand(tag)+"`, which finds the new table and drops nothing.")
	}
	return nil
}

// restartMounts restarts the services that bind-mount bundle files (svcs)
// after `up -d`, run in the project at dir (compose's own arguments, e.g. the
// project directory and env file, in args; its environment in env), and
// reports whether every one restarted.
func restartMounts(env, svcs []string, args ...string) bool {
	ok := true
	for _, svc := range svcs {
		sp := ui.Start("Restarting %s for its new config file", svc)
		if _, err := dockerRun(env, "docker", append(append([]string{"compose"}, args...), "restart", svc)...); err != nil {
			sp.Warn("Could not restart %s; run: docker compose %s restart %s", svc, strings.Join(args, " "), svc)
			ok = false
		} else {
			sp.Success("Restarted %s", svc)
		}
	}
	return ok
}

// resetSummary is the summary box's account of a Docker storage reset.
func resetSummary(r resetResult) string {
	switch {
	case r.outcome == resetNone:
		return ui.Green("Named volumes (your data) were not touched.")
	case len(r.kept) > 0:
		return ui.Yellow("Storage reset finished: the events table is the new one. Kept in ClickHouse, not dropped (see above): events." + strings.Join(r.kept, ", events.") + ".")
	case r.outcome == resetDropped:
		return ui.Yellow("Event history was discarded (storage reset); profiles, traits, segments and journeys were kept.")
	case r.outcome == resetCreated:
		return ui.Yellow("Storage reset finished: the events table was created with the new schema (the event history from before the reset is gone); profiles, traits, segments and journeys were kept.")
	case len(r.dropped) > 0:
		return ui.Yellow("Storage reset finished: ClickHouse already held the new events table; the pre-reset one an earlier run had moved aside was dropped.")
	}
	return ui.Green("Storage reset finished: ClickHouse already held the new events table, so nothing was dropped.")
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
	// One run at a time on a project (lock.go), so two cannot interleave a
	// storage reset's drop with each other's checks.
	unlock, err := lockProject(dir, "origamy upgrade")
	if err != nil {
		return lockFail(err, "Nothing has been changed.")
	}
	defer unlock()
	// Ctrl-C, SIGTERM or SIGHUP end the run without leaving the lock or a
	// bundle download behind.
	defer exitOnSignal()()
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
	dockerTagNotes(envPath, tag)

	// Storage reset. What the running ClickHouse holds decides, never the
	// project's clickhouse-init.sql or the tags (dockerResetAction): the
	// pre-reset table is dropped (asking first) and a missing one created for
	// a release that writes the new schema, and the new table is never
	// dropped. A reset an earlier run began and did not finish
	// (resetPendingVar) is finished first: by the reset when ClickHouse holds
	// the old table or none, and when it holds the new one, by the steps that
	// follow it (finish), with nothing dropped and nothing asked.
	pending := readEnvVar(envPath, resetPendingVar)
	targetGen := releaseSchemaGeneration(tag)
	// Only a release that writes the schema the reset moves to can finish it;
	// settled before Docker is asked anything.
	if pending != "" && targetGen != "json" {
		return fail(fmt.Sprintf("An earlier upgrade to %s began the storage reset and did not finish it (%s in .env), and %s.", pending, resetPendingVar, cannotFinishReset(tag)),
			"Finish it first: "+upgradeCommand(pending)+" (any numbered release from "+minImageForJSONSchema+" on, or --channel edge, can).")
	}
	rerun := "rerun `" + upgradeCommand(tag) + "`. Nothing has been changed."
	live := askClickHouse(dir, envPath, installedTag)
	switch {
	case live.gen == "":
		return fail("ClickHouse could not be asked which events table it holds, so this upgrade cannot tell what it would do with it.",
			outputHint(live.out)+"Check: docker compose --project-directory "+dir+" --env-file "+envPath+" logs clickhouse\nOnce it answers, rerun `"+upgradeCommand(tag)+"`. No file of the project has been changed.")
	case live.gen == liveOther:
		return fail(fmt.Sprintf("ClickHouse's %s has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, which no release writes and the storage reset does not drop.", eventsTable, live.typ),
			"Check what created it, and drop it yourself if it should be recreated with the new schema; then "+rerun)
	case live.gen == "json" && targetGen == "string":
		return fail(fmt.Sprintf("ClickHouse here holds the new events table (JSON payload columns), which release %s cannot write: events would be accepted and then dropped at insert.", tag),
			"Pass --version "+minImageForJSONSchema+" or newer; plain `origamy upgrade` moves to "+helmVersion+". Nothing has been changed.")
	}
	drop, create := dockerResetAction(live.gen, tag)
	// reset: the events table is (re)created with the new schema before the
	// release moves, by the reset's own steps and checks.
	reset := drop || create
	finish := pending != "" && live.gen == "json"
	// The reset recreates the events table from the control plane's current
	// schema, so it cannot start without knowing where that is.
	if reset && base == "" {
		return fail("The control plane URL is missing from .env (no CONTROL_PLANE_HTTP_URL or CONFIG_URL), and the storage reset needs its current ClickHouse schema to create the events table.",
			"Re-deploy with `origamy deploy --token …`, or add CONTROL_PLANE_HTTP_URL=https://<your control plane> to "+envPath+" and retry.")
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
	// secrets so a later re-enable finds the same KEK. Until the commit point
	// below they are given to docker compose in its environment (runEnv),
	// over .env, so the new compose file is validated and the profiles'
	// images pulled with them; .env takes them only once the run is through.
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
	// Every docker compose command before the commit point runs with this,
	// and so does the `up -d` after it: .env holds the same values by then,
	// and a COMPOSE_PROFILES the shell exports cannot undo the toggle.
	runEnv := tagEnv(tag)
	for _, k := range slices.Sorted(maps.Keys(aiVars)) {
		runEnv = append(runEnv, k+"="+aiVars[k])
	}
	ui.KV("Directory", dir)
	ui.KV("Image tag", tag)
	ui.KV("Profiles", orDash(strings.Join(profiles, ",")))

	// The compose file, ClickHouse config and Caddyfile follow the release
	// too, and only the control plane has them (bundle.go). They are
	// refreshed when it serves the bundle for exactly this tag, and only the
	// files nobody edited here.
	refresh, bundleSkipped := planBundleRefresh(dir, envPath, base, tag, installedTag, runEnv, bundle, reset)
	// The download goes with the run, however it ends (a prompt or a signal
	// that ends the CLI removes it too: prepareBundleRefresh).
	defer func() {
		if refresh != nil {
			refresh.cleanup()
		}
	}()
	// abort drops the prepared refresh (its download directory) on the way out.
	abort := func(err error) error {
		if refresh != nil {
			refresh.cleanup()
		}
		return err
	}

	// The schema the project keeps: the served one when the refresh takes it
	// (written, or already identical), else its own.
	keptSQL, _ := os.ReadFile(filepath.Join(dir, "clickhouse-init.sql"))
	servedSchema := refresh != nil && refresh.served("clickhouse-init.sql")
	if servedSchema {
		keptSQL, _ = os.ReadFile(filepath.Join(refresh.tmp, "clickhouse-init.sql"))
	}

	// What the project will run once the refresh is in is checked before
	// anything is touched or asked. Across the reset: the ClickHouse its
	// effective compose configuration starts must hold the new schema, and
	// the schema the reset applies (the refreshed one, else the control
	// plane's current one, fetched now and written over clickhouse-init.sql)
	// must be the new one: anything else is refused before any drop.
	var fetchedSQL []byte
	if reset {
		why, culprit, cerr := resetComposeProblem(dir, envPath, tag, refresh)
		if why != "" || cerr != nil {
			return abort(fail(resetComposeRefusal(dir, tag, refresh, bundleSkipped, why, culprit, cerr)))
		}
		resetSQL := keptSQL
		if !servedSchema {
			if why := resetSchemaProblem(dir); why != "" {
				return abort(fail("The storage reset writes your control plane's current schema to clickhouse-init.sql, which here "+why+".",
					"Replace it with a regular file (a copy of what it points to is fine: the reset backs it up before writing), then "+rerun))
			}
			sp := ui.Start("Fetching the current ClickHouse schema")
			b, err := fetchBundleBytes(base, "clickhouse-init.sql")
			if err != nil {
				sp.Fail("Could not download clickhouse-init.sql")
				return abort(controlPlaneFetchFail(base, "clickhouse-init.sql", err, "rerun `"+upgradeCommand(tag)+"` (nothing has been changed)"))
			}
			sp.Success("Schema fetched from %s", base)
			resetSQL, fetchedSQL = b, b
		}
		switch schemaGeneration(string(resetSQL)) {
		case "json":
		case "string":
			return abort(fail("Your control plane serves the pre-reset ClickHouse schema (String payload columns), so the storage reset cannot recreate the events table from it.",
				"Update the control plane first, then "+rerun))
		default:
			return abort(fail("The ClickHouse schema your control plane serves has neither the pre-reset (String) nor the new (JSON) payload columns, so the storage reset cannot tell that it creates the events table the "+tag+" images write.",
				"Check "+base+"/byod/clickhouse-init.sql and update the control plane, then "+rerun))
		}
	} else {
		switch gen := schemaGeneration(string(keptSQL)); {
		// Same guard as deploy: a release that cannot write the schema the
		// project keeps must not be started against it.
		case gen == "json" && targetGen == "string":
			return abort(fail(fmt.Sprintf("This data plane's ClickHouse schema is the post-reset one (native JSON payload columns), which release %s cannot write.", tag),
				"Pass --version "+minImageForJSONSchema+" or newer; plain `origamy upgrade` moves to "+helmVersion+"."))
		// The control plane's bundle for exactly this release carries the
		// pre-reset schema: refused as before the refresh took the schema
		// (the operator's own file is only warned about below).
		case gen == "string" && targetGen == "json" && servedSchema:
			return abort(fail(bundleSchemaMismatch(string(keptSQL), tag), "Update the control plane first, then "+rerun))
		}
	}

	// Asked last, once nothing can refuse any more.
	switch {
	case drop:
		var yes bool
		if pending != "" {
			yes = confirmResumedReset(pending, tag, assumeYes)
		} else {
			// Say why when the tags alone would not: an earlier upgrade can
			// have moved them on and left the old table behind.
			ui.Warn("ClickHouse here still holds the pre-reset events table (String payload columns), which %s cannot write: upgrading %s → %s crosses the data plane's storage reset.", tag, cmp.Or(installedTag, "the installed release"), tag)
			yes = confirmResetDetails(assumeYes)
		}
		if !yes {
			return abort(fail("Cancelled.", "Nothing has been changed."))
		}
	case create && pending != "":
		ui.Step("An earlier upgrade to %s began the storage reset and did not finish it (%s in .env). ClickHouse has no events table now: this run creates it with the new schema before %s starts (a pre-reset table the earlier run moved aside is dropped once it is in place).", pending, resetPendingVar, tag)
	case create:
		ui.Step("ClickHouse has no events table (an earlier upgrade can have dropped it without creating the new one): this run creates it with the new schema before %s starts; nothing is dropped.", tag)
	case finish:
		ui.Step("An earlier upgrade to %s recorded an unfinished storage reset (%s in .env), but ClickHouse already holds the new events table (%s payload columns): it is kept, and the record is cleared once the services are up.", pending, resetPendingVar, live.typ)
	case live.gen == "json" && schemaGeneration(string(keptSQL)) == "string":
		// The file here is the pre-reset schema while ClickHouse is across.
		// Harmless on this volume (its tables exist), wrong on a new one.
		ui.Warn("clickhouse-init.sql here is the pre-reset schema (String payload columns), while ClickHouse already holds the new events table: nothing is dropped. A new, empty volume would get the wrong schema from it; replace it with the %s version before ever recreating that volume.", tag)
	}

	// Pull BEFORE touching the project: a tag that does not exist for one of
	// the services (or a registry outage) must leave .env and the bundle files
	// — and therefore every later `docker compose up` — on the release that is
	// actually running. The process environment wins over the env file in
	// compose interpolation, so the pull sees the candidate tag and AI toggle
	// (runEnv) without writing them, and a refreshed compose file is pulled
	// with from where it was downloaded, so images of services it adds come
	// too. Nothing in the project has been written up to here.
	sp := ui.Start("Pulling %s images", tag)
	if out, err := pullImages(dir, envPath, runEnv, refresh); err != nil {
		sp.Fail("docker compose pull failed")
		if refresh != nil {
			if slices.Contains(refresh.kept(), "docker-compose.yml") {
				ui.Step("Your edited docker-compose.yml is what was pulled with: when it is what fails, --replace-bundle takes the %s one (yours is backed up next to it).", tag)
			}
			refresh.cleanup()
			ui.Step("Bundle files left as they were.")
		}
		return diagnoseDocker(out, "rerun `"+upgradeCommand(tag)+"`")
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
		if err, rerr := refresh.swapIn(); err != nil {
			if rerr != nil {
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
			refresh.cleanup()
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

	// Storage reset on Docker: the bundle's one-shot init service applies the
	// new schema (the refreshed one, forced in across the reset, or the one
	// fetched above, written now) to a ClickHouse that holds the old table or
	// none, before the services roll. A recorded reset ClickHouse had already
	// finished is checked the same way, and nothing is dropped or applied.
	var outcome resetResult
	var schemaWrite *resetSchemaWrite
	if reset || finish {
		if fetchedSQL != nil {
			w, err := writeResetSchema(dir, fetchedSQL, installedTag)
			if err != nil {
				return fail("Could not write clickhouse-init.sql.", err.Error()+"\nFix the cause and rerun `"+upgradeCommand(tag)+"`.")
			}
			schemaWrite = &w
			switch {
			case w.edited:
				ui.Warn("Replaced your edited clickhouse-init.sql with your control plane's current schema for the storage reset; yours is backed up as %s", filepath.Base(w.backup))
			case w.backup != "":
				ui.Step("Replaced clickhouse-init.sql with your control plane's current schema for the storage reset; the previous one is backed up as %s.", filepath.Base(w.backup))
			}
		}
		var err error
		// The operator agreed to discard the pre-reset table in this run
		// (drop), or in the one that began the reset.
		if outcome, err = resetDockerEvents(dir, envPath, tag, live.gen, runEnv, drop || pending != ""); err != nil {
			return err
		}
	}

	// The commit point: the pull and, across the storage reset, the reset
	// went through. Only now does .env name the new release, and take the AI
	// toggle: until here every `docker compose up`, and the next upgrade,
	// still see what runs, and a run that failed or was refused before here
	// left .env byte for byte as it was, but for the reset record.
	commit := map[string]string{"DP_IMAGE_TAG": tag}
	maps.Copy(commit, aiVars)
	for _, k := range slices.Sorted(maps.Keys(commit)) {
		if err := setEnvVar(envPath, k, commit[k]); err != nil {
			return fail("Could not update .env.", err.Error())
		}
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
	if out, err := dockerRun(runEnv, "docker", "compose", "--project-directory", dir, "--env-file", envPath, "up", "-d", "--remove-orphans"); err != nil {
		sp.Fail("docker compose up failed")
		return diagnoseDocker(out, "rerun `"+upgradeCommand(tag)+"`")
	}
	sp.Success("Services restarted")
	// A recorded reset is finished only once the services that bind-mount
	// bundle files run with the files that are there now: the run that began
	// it may have swapped them and stopped before restarting anything, and
	// this one may have nothing left to replace.
	restarts := refresh.restarts(profiles)
	if pending != "" {
		for _, svc := range bundleMountServices(profiles) {
			if !slices.Contains(restarts, svc) {
				restarts = append(restarts, svc)
			}
		}
	}
	restarted := restartMounts(tagEnv(tag), restarts, "--project-directory", dir, "--env-file", envPath)
	// Whatever this run did or did not do to it, it succeeds only when the
	// events table the services now run on is the one the release writes,
	// and the images that run are the release's.
	got, why := eventsTableProblem(dir, envPath, tag)
	if why != "" {
		return eventsTableFail(dir, envPath, tag, got, why)
	}
	if why := runningImagesProblem(dir, envPath, tag, got.gen); why != "" {
		return runningImagesFail(dir, envPath, tag, why)
	}
	if reset || finish {
		if !restarted {
			ui.Warn("%s stays in .env until those services restart: the next `%s` restarts them and clears it, dropping nothing.", resetPendingVar, upgradeCommand(tag))
		} else if err := clearResetRecord(envPath, tag); err != nil {
			return err
		}
	}

	boxLines := []string{
		ui.Gray("Image tag  ") + ui.Bold(tag),
		ui.Gray("Directory  ") + dir,
	}
	boxLines = append(boxLines, bundleBoxLines(refresh, bundleSkipped, schemaWrite)...)
	if enableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+ui.Green("enabled"))
	} else if disableAI {
		boxLines = append(boxLines, ui.Gray("AI engine  ")+"disabled")
	}
	if line := signingSummary(signingKey != "", signingSupported(tag), requireSigned); line != "" {
		boxLines = append(boxLines, ui.Gray("Config     ")+line)
	}
	boxLines = append(boxLines, "", resetSummary(outcome))
	if signingKey == "" && signingSupported(tag) {
		boxLines = append(boxLines, "", ui.Gray("Config signing is not set up (no verify key stored). If your control plane signs config,"),
			ui.Gray("set CONFIG_SIGNING_KEY in .env to the key from your dashboard's Docker install snippet and rerun `origamy upgrade`."))
	}
	ui.Box("Upgraded", boxLines)
	return nil
}

// eventsTableFail fails an upgrade or a deploy whose check after the last
// `up -d` (eventsTableProblem) found ClickHouse without the events table the
// images of tag write: got is what it holds, why says so. A missing or
// pre-reset table under a release that writes the new schema is a storage
// reset still to do, recorded in .env (resetPendingVar) so the next
// `origamy upgrade` (or in-place deploy) finishes it before anything else.
func eventsTableFail(dir, envPath, tag string, got liveSchema, why string) error {
	headline := "The " + tag + " services are up, but " + why + "."
	rerun := "rerun `" + upgradeCommand(tag) + "`"
	if drop, create := dockerResetAction(got.gen, tag); drop || create {
		hint := "Until it is, events are accepted and then dropped at insert. The storage reset is recorded in .env (" + resetPendingVar + "): " + rerun + ", which finishes it (asking before it drops anything)."
		if err := setEnvVar(envPath, resetPendingVar, tag); err != nil {
			hint = "Until it is, events are accepted and then dropped at insert. Recording the storage reset in .env failed too (" + err.Error() + "): " + rerun + ", which finds it all the same."
		}
		return fail(headline, hint)
	}
	switch got.gen {
	case "":
		return fail(headline, outputHint(got.out)+"Check: docker compose --project-directory "+dir+" --env-file "+envPath+" logs clickhouse\nOnce it answers, "+rerun+", which checks again.")
	case "json":
		return fail(headline, "Events are accepted and then dropped at insert. Move to a release that writes it: `origamy upgrade --version "+minImageForJSONSchema+"` or newer.")
	case "string":
		return fail(headline, "Events are accepted and then dropped at insert. Move to a release that writes it (before "+minImageForJSONSchema+"), or to "+minImageForJSONSchema+" or newer, which resets the table (asking first).")
	case liveMissing:
		// Not a release that writes the new schema (dockerResetAction):
		// nothing this CLI creates is known to be the table its images write.
		if releaseSchemaGeneration(tag) == "unknown" {
			return fail(headline, "The :"+tag+" images are not a numbered release, so which schema generation they write is not known, and this run cannot create the table for them. Move to a numbered release: `origamy upgrade --version "+helmVersion+"` creates the new events table (nothing is dropped).")
		}
		return fail(headline, "Release "+tag+" writes the pre-reset (String) table, which the bundle's clickhouse-init service creates at every `up -d`: check its output (docker compose --project-directory "+dir+" --env-file "+envPath+" logs clickhouse-init) and clickhouse-init.sql, then "+rerun+". Or move to "+minImageForJSONSchema+" or newer (`origamy upgrade --version "+helmVersion+"`), which creates the new table.")
	}
	return fail(headline, "Check what created it, and drop it yourself if it should be recreated with the new schema; then "+rerun+".")
}

// runningImagesFail fails an upgrade or a deploy whose check after the last
// `up -d` found containers that do not run what release tag does (why, from
// runningImagesProblem), with what can cause it.
func runningImagesFail(dir, envPath, tag, why string) error {
	return fail("The "+tag+" services are up, but "+why+".",
		"Until they do, what runs may not write the events table ClickHouse holds. What can cause it:\n"+
			strings.Join(runningImagesCauses(dir, envPath, tag), "\n")+
			"\nFix that, then rerun `"+upgradeCommand(tag)+"`, which starts the "+tag+" images and checks again.")
}

// dockerTagNotes says what makes a plain `docker compose` in the project start
// another release than tag, which the CLI's own commands are not subject to
// (tagEnv): a DP_IMAGE_TAG exported in the shell, which compose takes over
// .env, and several DP_IMAGE_TAG lines in .env, of which compose takes the
// last (and so does the CLI).
func dockerTagNotes(envPath, tag string) {
	if v := os.Getenv("DP_IMAGE_TAG"); v != "" && v != tag {
		ui.Warn("DP_IMAGE_TAG=%s is exported in your shell, and docker compose takes it over .env. This run gives docker compose %s itself, but a `docker compose up` from this shell would start %s: unset it (unset DP_IMAGE_TAG).", v, tag, v)
	}
	if n := envKeyCount(envPath, "DP_IMAGE_TAG"); n > 1 {
		ui.Step(".env has %d DP_IMAGE_TAG lines; docker compose takes the last (%s), and so does this run, which leaves one when it writes the release.", n, readEnvVar(envPath, "DP_IMAGE_TAG"))
	}
}

// resetComposeProblem says why the storage reset cannot run on the ClickHouse
// the project starts once refresh r (nil: none) is in, "" when it can. That
// is the effective configuration (effectiveCompose) of the files the
// project's own `docker compose` loads, with the served compose file in place
// of docker-compose.yml when r takes it: an override file or COMPOSE_FILE
// can pin another server. culprit lists the files besides the compose file
// that cause it, when the compose file alone would do. err is why compose
// could not render the configuration (the reset is refused then too).
func resetComposeProblem(dir, envPath, tag string, r *bundleRefresh) (why string, culprit []string, err error) {
	main := filepath.Join(dir, "docker-compose.yml")
	if r != nil && r.served("docker-compose.yml") {
		main = filepath.Join(r.tmp, "docker-compose.yml")
	}
	files := composeFileArgs(dir, envPath, main)
	cfg, err := effectiveCompose(dir, envPath, tag, files)
	if err != nil {
		return "", nil, err
	}
	if why = clickHouseImageProblem(cfg.Services["clickhouse"].Image); why == "" {
		return "", nil, nil
	}
	var others []string
	for i := 1; i < len(files); i += 2 {
		if files[i] != main {
			others = append(others, files[i])
		}
	}
	if len(others) > 0 {
		if alone, err := effectiveCompose(dir, envPath, tag, []string{"-f", main}); err == nil && clickHouseImageProblem(alone.Services["clickhouse"].Image) == "" {
			culprit = others
		}
	}
	return why, culprit, nil
}

// resetComposeRefusal explains a refusal by resetComposeProblem (why,
// culprit, cfgErr): the files compose loads over the compose file when they
// are at fault, else the compose file: the served one when refresh r takes
// it, the operator's edited one when r keeps it, else the project's own,
// which planBundleRefresh did not refresh for the reason skipped.
func resetComposeRefusal(dir, tag string, r *bundleRefresh, skipped, why string, culprit []string, cfgErr error) (headline, hint string) {
	rerun := "rerun `" + upgradeCommand(tag) + "`. Nothing has been changed."
	switch {
	case cfgErr != nil && r != nil && slices.Contains(r.kept(), "docker-compose.yml"):
		// The edited compose file the refresh keeps is what does not render.
		return "The storage reset needs to know which ClickHouse this project runs, and docker compose could not render its configuration with your edited docker-compose.yml, which the refresh keeps.",
			cfgErr.Error() + "\nPass --replace-bundle to take the " + tag + " compose file (yours is backed up next to it) and re-apply your edits afterwards,\nor fix the compose files or .env (`docker compose --project-directory " + dir + " config` shows the error), then " + rerun
	case cfgErr != nil:
		return "The storage reset needs to know which ClickHouse this project runs, and docker compose could not render its configuration.",
			cfgErr.Error() + "\nFix the compose files or .env (`docker compose --project-directory " + dir + " config` shows the error), then " + rerun
	case len(culprit) > 0:
		var names []string
		for _, f := range culprit {
			if rel, err := filepath.Rel(dir, f); err == nil && !strings.HasPrefix(rel, "..") {
				f = rel
			}
			names = append(names, f)
		}
		return fmt.Sprintf("The storage reset needs ClickHouse %s or newer, and %s, which compose loads with docker-compose.yml here, changes the clickhouse service: %s.", minClickHouseForJSON, strings.Join(names, ", "), why),
			"Set the clickhouse image there to clickhouse/clickhouse-server:" + minClickHouseForJSON + " or newer (or take it out of that file), then " + rerun
	case r != nil && r.served("docker-compose.yml"):
		return fmt.Sprintf("The storage reset needs ClickHouse %s or newer, and the docker-compose.yml your control plane serves for %s does not run it (%s).", minClickHouseForJSON, tag, why),
			"Update the control plane, then " + rerun
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
