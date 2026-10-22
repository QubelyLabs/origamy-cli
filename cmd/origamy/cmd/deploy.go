package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/token"
	"github.com/qubelylabs/origamy-cli/internal/ui"
)

const (
	helmChart = "oci://ghcr.io/qubelylabs/charts/origamy-data-plane"
	// helmVersion is the data-plane release a fresh install gets: the Helm
	// chart version on Kubernetes AND the image tag (DP_IMAGE_TAG) on Docker —
	// chart and images share one version per release. 0.1.20 carries every
	// feature this CLI can toggle (mTLS identity 0.1.15, orchestrator engine
	// 0.1.16, predictor 0.1.17, external ClickHouse and config signing 0.1.19),
	// plus 0.1.20's at-most-once sends with a 45 s shutdown drain for the
	// workflow and orchestrator engines and the events.page_name column (the
	// bulker adds it to an existing table on start), and 0.1.21's delivery
	// status for workspace-connected Cloudflare/Envelope accounts (needs
	// ENGAGEMENT_SECRET, generated once by deploy/upgrade), and 0.1.22's
	// year-less birthday traits for the form date fields, and 0.1.23's list
	// segments and workflow property-filter operators. On Kubernetes `origamy
	// upgrade` resolves the latest published chart at run time, so there this
	// pin only governs a fresh install; on Docker it is also upgrade's default.
	helmVersion = "0.1.23"
	namespace   = "origamy-dp"
	release     = "odp"
)

type preset struct {
	name        string
	label       string
	description string
	replicas    string
}

var presets = []preset{
	{"starter", "Starter", "dev/test  — 1 replica, ~4 GB RAM", "1"},
	{"standard", "Standard", "production — 2 replicas, ~8 GB RAM", "2"},
	{"production", "Production", "high-scale — 3 replicas, ~16 GB RAM", "3"},
}

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy an Origamy data plane to this machine",
	Long: `Deploy the Origamy data plane using your enrollment token.

Auto-detects your environment (override with --target):
  • Kubernetes cluster (kubectl) → Helm install into origamy-dp namespace
  • Docker                       → Docker Compose in ./origamy-dp-<id>/

Kubernetes wins whenever kubectl reaches a cluster — including Docker Desktop's
bundled Kubernetes or a kubeconfig that still points at a shared cluster — so
the install prints the kubectl context it is about to use; pass
--target docker to install with Docker Compose on such a host.

Get your enrollment token from the Connections page in your Origamy dashboard.
Every question is asked, and every check made, BEFORE the token is redeemed:
a token is single-use, so nothing is spent until the install can proceed.

The core plane (ingestion, identity, segments, storage) is all a fresh install
needs. The optional pieces can be added later without touching it:
  • AI engine: --enable-ai now, or 'origamy upgrade --enable-ai' later. On
    Kubernetes it adds ~1 pod; on Docker it enables the "agentic" compose
    profile. The engine stays inert until you enable AI and add an LLM
    credential in your dashboard.
  • Predictor (conversion scoring, Kubernetes): 'origamy upgrade --enable-predictor'.
  • Journeys, broadcasts and tasks (Docker): the "full" compose profile,
    offered at install.

Add --datastore-auth on Kubernetes to password-protect the bundled Redis, NATS
and ClickHouse (the chart generates the passwords in-cluster). Docker installs
always get generated datastore passwords.

On Kubernetes with release ` + minChartExternalClickHouse + ` or newer, deploy also offers to connect your
own ClickHouse instead of the bundled one. Its password goes into the
origamy-clickhouse Secret, and the chart creates the events database and
tables on that server during install.

When the control plane signs config, enrollment hands out its public verify
key. Release ` + minSigningVersion + ` or newer verifies config with it; on an older release
the key is only stored, and 'origamy upgrade' applies it once the release can
verify. --require-signed-config (` + minSigningVersion + `+) makes the services refuse to
start without the key.

Re-running deploy inside an existing ./origamy-dp-<id>/ re-deploys that plane
in place, keeping its generated passwords and data. The bundle is downloaded
into a directory inside the project and swapped in only once every check
passed and the token was redeemed (each file it replaces backed up as
<file>.bak-<release>), so a refused deploy leaves the project as it was.
As in 'origamy upgrade', how much is checked depends on the move: over a
plane whose release writes another events table than the one deployed (or
that runs :main, or has a storage reset recorded), what the plane's
ClickHouse holds is asked first (it is started when it does not answer; the
deploy is refused when it still cannot be asked), and once the services are
up, deploy checks that ClickHouse holds the events table the release writes
and that every container of the compose project that runs a data-plane image
runs the release's. Over a plane of the same generation ClickHouse is only
peeked at, never started, and for a new plane it is not asked before the
services start; what is found after they are up is a warning, but for a table
of another generation or none, and a data-plane image of the other
generation, which fail the deploy. The
services only a profile the answers turn off ran (journeys, the AI engine)
are stopped and removed; every other profile in COMPOSE_PROFILES (the log
stack: logs, loki) stays as it is. A --version that is not a numbered release
(main, sha-…) is refused: which events table its images write is not known.
Every docker compose command the CLI runs pins the project's name
(COMPOSE_PROJECT_NAME in .env, else the top-level name: of
docker-compose.yml, else the directory's name) and ignores
COMPOSE_PROJECT_NAME, COMPOSE_FILE and COMPOSE_PROFILES exported in the
shell; .env is where they belong.

On Kubernetes a deploy over a release that is already installed is refused
before the token is spent when an upgrade's storage reset is unfinished, or
when the deploy takes the images across the storage reset and the release
cannot write the events table its ClickHouse holds (move it with 'origamy
upgrade', which resets the table); only such a deploy asks ClickHouse (create
on pods/exec in origamy-dp) and holds the lease on the events table. Every
Kubernetes install then asks ClickHouse once, without holding the install
up: a missing table, or one the images cannot write, fails the deploy after
the endpoint and summary are printed; a ClickHouse that cannot be asked is
reported as not verified.`,
	Example: `  origamy deploy --token dpe_xxx
  origamy deploy --token dpe_xxx --enable-ai
  origamy deploy --token dpe_xxx --datastore-auth
  origamy deploy --token dpe_xxx --target docker   # this laptop also has a kubeconfig`,
	RunE: func(cmd *cobra.Command, args []string) error {
		t, _ := cmd.Flags().GetString("token")
		target, _ := cmd.Flags().GetString("target")
		enableAI, _ := cmd.Flags().GetBool("enable-ai")
		aiSet := cmd.Flags().Changed("enable-ai")
		datastoreAuth, _ := cmd.Flags().GetBool("datastore-auth")
		requireSigned, _ := cmd.Flags().GetBool("require-signed-config")
		return runDeploy(t, target, enableAI, aiSet, datastoreAuth, requireSigned)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

// deployChartVersion optionally overrides the pinned helmVersion for the initial
// install (bound to --version). Empty means "use the CLI's pinned default".
var deployChartVersion string

func init() {
	deployCmd.Flags().StringP("token", "t", "", "Enrollment token from your Origamy dashboard (required)")
	deployCmd.Flags().StringVar(&deployChartVersion, "version", "", "Data-plane release to install: the chart version on Kubernetes, the image tag on Docker (default: the CLI's pinned version)")
	deployCmd.Flags().String("target", string(targetAuto), "Where to install: auto (Kubernetes if kubectl reaches a cluster, else Docker), kubernetes, or docker")
	deployCmd.Flags().Bool("enable-ai", false, "Enable the Origamy AI (agentic) engine at install")
	deployCmd.Flags().Bool("datastore-auth", false, "Password-protect the bundled Redis/NATS/ClickHouse (Kubernetes; passwords generated in-cluster)")
	deployCmd.Flags().Bool("require-signed-config", false, "Make the services refuse to start without the control plane's config signing key (release "+minSigningVersion+"+)")
	_ = deployCmd.MarkFlagRequired("token")
}

func runDeploy(raw, target string, enableAI, aiSet, datastoreAuth, requireSigned bool) error {
	// A successful /byod/register consumes the one-time token, so everything
	// that can fail or needs an answer — target, tooling, architecture, the
	// bundle, the questions — happens first. Only then is the token redeemed
	// and the install applied.
	//
	// The data-plane release a fresh install gets: the chart version on
	// Kubernetes and the image tag on Docker (they share one version).
	dpVersion := helmVersion
	if deployChartVersion != "" {
		dpVersion = deployChartVersion
	}
	if requireSigned && !signingSupported(dpVersion) {
		return fail(requireSigningProblem(dpVersion, true), "Pass --version "+minSigningVersion+" or newer, or drop --require-signed-config.")
	}

	want := strings.ToLower(strings.TrimSpace(target))
	// Probe only what the target needs: a stale kubeconfig can hang
	// `kubectl cluster-info` for its whole dial timeout, which --target docker
	// should never have to wait for.
	k8s := want != string(targetDocker) && hasKubernetes()
	docker := want != string(targetKubernetes) && hasDocker()
	where, err := resolveTarget(target, k8s, docker)
	if err != nil {
		if errors.Is(err, errNoTarget) {
			return fail("No Kubernetes cluster or Docker found on this machine.",
				"Install Docker (https://docs.docker.com/get-docker/) or point kubectl at a cluster, then retry.")
		}
		return fail(err.Error(), "Pass --target kubernetes or --target docker to choose explicitly, or omit it to auto-detect.")
	}

	// Read the token without spending it: a v2 handle is resolved through
	// /v1/byod/enroll/resolve, which leaves it redeemable, and yields the plane
	// id and control-plane URL the plan needs (which directory, which bundle).
	// An invalid or expired token therefore fails here, before any question.
	peek, err := token.Decode(raw)
	if err != nil {
		return fail("Could not read the enrollment token.", enrollHint(err))
	}
	if peek.Exp > 0 && time.Now().Unix() > peek.Exp {
		return fail("Your enrollment token has expired.",
			"Generate a new one from the Connections page in your dashboard.")
	}
	if requireSigned && peek.CSK == "" {
		return fail(requireSigningProblem(dpVersion, false),
			"Your control plane did not hand one out: it does not sign config, or predates handing the key to data planes. Drop --require-signed-config.")
	}

	var kp *k8sPlan
	var dp *dockerPlan
	if where == targetKubernetes {
		kp, err = planKubernetes(dpVersion, enableAI, aiSet, datastoreAuth, docker)
	} else {
		dp, err = planDocker(dpVersion, enableAI, aiSet, peek.ID, peek.URL)
		if dp != nil {
			// The download and the lock go once the deploy is over; until
			// the token is redeemed, the project's files are as they were.
			defer dp.close()
		}
	}
	if err != nil {
		return err
	}
	if kp != nil {
		kp.requireSigned = requireSigned
		if kp.close != nil {
			defer kp.close()
		}
	} else {
		dp.requireSigned = requireSigned
	}

	// Generate a keypair + CSR locally so enrollment can request an mTLS identity
	// — the private key never leaves this machine. Enroll falls back to a
	// bearer-only token if the control plane has no CA configured.
	keyPEM, csrPEM, err := token.GenerateIdentity()
	if err != nil {
		return fail("Could not generate a data-plane keypair.", err.Error())
	}
	ui.Title("Enrolling")
	sp := ui.Start("Redeeming the enrollment token")
	tok, err := token.Enroll(raw, csrPEM)
	if err != nil {
		sp.Fail("Enrollment failed")
		return fail("Could not enroll this data plane.", enrollHint(err))
	}
	if tok.ID != peek.ID {
		sp.Fail("Enrollment mismatch")
		return fail(fmt.Sprintf("The control plane enrolled %s but the token announced %s.", tok.ID, peek.ID),
			"Generate a fresh token from the Connections page in your dashboard and retry.")
	}
	sp.Success("Enrolled data plane %s", ui.Bold(tok.ID))
	ui.KV("Control", tok.Addr)
	if tok.Cert != "" {
		ui.KV("Identity", "mTLS certificate issued")
	}

	if kp != nil {
		return applyKubernetes(tok, keyPEM, kp)
	}
	return applyDocker(tok, keyPEM, dp)
}

// enrollHint turns an enrollment error into guidance. The control plane's own
// phrases (and the token decoder's) mark the cases where a new token is the
// fix; anything else — a proxy, a lapsed TLS certificate on the control
// plane, a 502 — is a connectivity problem that a new token would not solve.
func enrollHint(err error) string {
	msg := err.Error()
	for _, needle := range []string{
		"already-used enrollment token", "enrollment token expired",
		"missing required fields", "could not decode token", "could not parse token",
	} {
		if strings.Contains(msg, needle) {
			return msg + "\nTokens are single-use and expire after 72 hours — generate a new one from the Connections page in your dashboard."
		}
	}
	return msg + "\nCheck that this host can reach your control plane over HTTPS, then retry with the same token."
}

// ── Kubernetes ────────────────────────────────────────────────────────────────

func hasKubernetes() bool {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return false
	}
	return runQuiet("kubectl", "cluster-info", "--request-timeout=10s") == nil
}

// k8sPlan is everything a Kubernetes install needs to know before the token
// is spent: answers to the prompts plus the release to install.
type k8sPlan struct {
	chartVer      string
	preset        preset
	aiEnabled     bool
	datastoreAuth bool
	// exposeMode: 1 LoadBalancer, 2 Ingress, 3 Internal (ClusterIP only).
	exposeMode                                  int
	ingressHost, ingressClass, ingressTLSSecret string
	// clickhouse is the operator's own ClickHouse; nil means the bundled one.
	clickhouse *externalClickHouse
	// requireSigned turns on config-signing strict mode (already gated).
	requireSigned bool
	// close gives up the lease on the events table a deploy that can touch it
	// holds (nil: it holds none).
	close func()
}

// planKubernetes runs the preflight checks and asks every question for a
// Kubernetes install. dockerToo says Docker is also usable on this host, so
// the operator can be pointed at --target docker.
func planKubernetes(chartVer string, enableAI, aiSet, datastoreAuth, dockerToo bool) (*k8sPlan, error) {
	if _, err := exec.LookPath("helm"); err != nil {
		hint := "Install it from https://helm.sh/docs/intro/install/ and retry."
		if dockerToo {
			hint += " Or install with Docker Compose instead: --target docker."
		}
		return nil, fail("Helm is required for Kubernetes installs.", hint)
	}

	ui.Title("Target")
	ctx := kubeContext()
	if ctx != "" {
		ui.Success("Kubernetes cluster detected (kubectl context: %s)", ui.Bold(ctx))
	} else {
		ui.Success("Kubernetes cluster detected")
	}
	if dockerToo {
		ui.Step("Docker is also available here — rerun with --target docker to use Docker Compose instead.")
	}
	// The images only exist for amd64 (see preflight.go): refuse a cluster that
	// could never schedule them, warn about a mixed one. A desktop distribution
	// (Docker Desktop, kind, OrbStack, …) on Apple Silicon reports arm64 nodes
	// yet runs amd64 images under emulation, so it only gets the warning.
	if block, warn := nodeArchProblem(nodeArchs()); block != "" {
		if !isDesktopContext(ctx) {
			return nil, fail(block, "Add amd64 nodes, or deploy on an amd64 Docker host with --target docker.")
		}
		ui.Warn("%s Desktop clusters run them under emulation (slower), so continuing.", block)
	} else if warn != "" {
		ui.Warn("%s", warn)
	}
	// A release already here moves to chartVer without the storage reset
	// (kube_reset.go), so before any question: refused while an upgrade's
	// reset is unfinished, or when the deploy takes the images across the
	// storage reset and ClickHouse holds a table chartVer cannot write. Such a
	// deploy holds the lease on the events table until it is over (close).
	p := &k8sPlan{chartVer: chartVer, datastoreAuth: datastoreAuth}
	if releaseInstalled() {
		headline, hint, giveUp := kubeDeployProblem(chartVer)
		if headline != "" {
			return nil, fail(headline, hint)
		}
		if giveUp != nil {
			// Ctrl-C, SIGTERM or SIGHUP end the deploy through close too.
			stopSignals := exitOnSignal()
			p.close = onExit(func() {
				giveUp()
				stopSignals()
			})
		}
	}
	planned := false
	defer func() {
		if !planned && p.close != nil {
			p.close()
		}
	}()

	// — Deployment tier ——————————————————————————————————————————————————
	ui.Title("Deployment size")
	for i, pr := range presets {
		fmt.Printf("  %s  %s  %s\n", ui.Cyan(fmt.Sprintf("%d", i+1)), ui.Bold(fmt.Sprintf("%-11s", pr.label)), ui.Gray(pr.description))
	}
	p.preset = presets[promptChoice("Choose 1-3", 1, len(presets), 1)-1]

	// — AI engine ————————————————————————————————————————————————————————
	// AI is a package customers buy; enabling it here adds ~1 pod. The flag wins
	// when set on the command line, otherwise we ask. The engine stays inert
	// until the workspace opts into AI and adds an LLM credential in the
	// dashboard — CLI controls engine presence, the control plane controls use.
	p.aiEnabled = enableAI
	if !aiSet {
		ui.Title("AI engine")
		p.aiEnabled = promptYesNo("Enable the Origamy AI engine? Adds ~1 pod; requires the AI package in your dashboard.", false)
	}
	if p.aiEnabled && p.preset.name == "starter" {
		ui.Warn("The AI engine adds a pod; the Starter tier is sized for dev/test. Consider Standard for production use.")
	}
	// The chart must actually carry the engine's templates — helm silently
	// ignores values an older chart doesn't know, and we'd report "enabled"
	// while nothing deployed.
	if err := featureGate(chartVer, p.aiEnabled, false); err != nil {
		return nil, fail(err.Error(), "Pass --version "+minChartAI+" or newer, or drop --enable-ai.")
	}

	// — ClickHouse ————————————————————————————————————————————————————————
	// The bundled StatefulSet, or the operator's own server on a chart that
	// supports it (see clickhouse.go).
	p.clickhouse = promptClickHouse(chartVer)

	// — Event endpoint ————————————————————————————————————————————————————
	// How the gateway is exposed for SDK traffic. ClusterIP (internal) alone
	// means events can't reach the plane from outside the cluster.
	ui.Title("Event endpoint")
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("1"), ui.Bold(fmt.Sprintf("%-12s", "LoadBalancer")), ui.Gray("cloud load balancer on :8081 (EKS/GKE/AKS)"))
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("2"), ui.Bold(fmt.Sprintf("%-12s", "Ingress")), ui.Gray("your own domain through an ingress controller"))
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("3"), ui.Bold(fmt.Sprintf("%-12s", "Internal")), ui.Gray("ClusterIP only — expose later"))
	p.exposeMode = promptChoice("Choose 1-3", 1, 3, 1)
	if p.exposeMode == 2 {
		p.ingressHost = promptString("Event domain (e.g. events.mycompany.com)")
		// An Ingress with no ingressClassName is claimed by NO controller when the
		// cluster has no default class — it silently 404s. Default to nginx.
		p.ingressClass = promptString("Ingress class (nginx, alb, …) [nginx]")
		if p.ingressClass == "" {
			p.ingressClass = "nginx"
		}
		// Without a TLS section the controller serves its placeholder
		// certificate and every SDK rejects the handshake — so ask, and only
		// promise https when it was configured.
		ui.Step("HTTPS needs a TLS Secret in the %s namespace holding the certificate for %s (e.g. from cert-manager).", namespace, p.ingressHost)
		p.ingressTLSSecret = promptString("TLS Secret name (leave empty to serve plain HTTP for now)")
	}
	planned = true
	return p, nil
}

// applyKubernetes installs the plane described by p for the enrolled token.
func applyKubernetes(tok *token.Enrollment, keyPEM []byte, p *k8sPlan) error {
	ui.Title("Provisioning")

	sp := ui.Start("Creating namespace %s", namespace)
	if out, err := kubectlApply(namespaceManifest(namespace)); err != nil {
		sp.Fail("Could not create namespace")
		return diagnose(out)
	}
	sp.Success("Namespace %s ready", namespace)

	// Secrets go to kubectl as manifests on stdin — never as --from-literal
	// arguments, which sit in `ps` and execve audit logs for the duration of
	// the command.
	sp = ui.Start("Storing auth token as a Kubernetes Secret")
	if out, err := kubectlApply(secretManifest(namespace, "origamy-byod-token", map[string]string{"auth-token": tok.Tok})); err != nil {
		sp.Fail("Could not store auth token")
		return diagnose(out)
	}
	sp.Success("Auth token stored securely")

	// mTLS identity: the issued client cert + private key + CA chain. The
	// portal-agent mounts this for the mTLS tunnel. Only present when the
	// control plane issued a cert (mTLS configured).
	if tok.Cert != "" {
		sp = ui.Start("Storing the mTLS identity as a Kubernetes Secret")
		if out, err := kubectlApply(secretManifest(namespace, "origamy-byod-identity", map[string]string{
			"tls.crt": tok.Cert, "tls.key": string(keyPEM), "ca.crt": tok.CAChain,
		})); err != nil {
			sp.Fail("Could not store the mTLS identity")
			return diagnose(out)
		}
		sp.Success("mTLS identity stored securely")
	}

	// External ClickHouse: the chart reads the password from this Secret, so
	// it never travels through helm --set (release history).
	if p.clickhouse != nil && p.clickhouse.password != "" {
		sp = ui.Start("Storing the ClickHouse password as a Kubernetes Secret")
		if out, err := kubectlApply(secretManifest(namespace, clickhouseSecretName, map[string]string{"clickhouse-password": p.clickhouse.password})); err != nil {
			sp.Fail("Could not store the ClickHouse password")
			return diagnose(out)
		}
		sp.Success("ClickHouse password stored securely")
	}

	// Config signing: the verify key is public, but kept in its own Secret on
	// every chart so `origamy upgrade` can wire it in once the chart verifies
	// (older charts never read it). A control plane that hands out no key gets
	// any earlier copy removed, so a later upgrade cannot apply a key it no
	// longer signs with.
	signing := tok.CSK != "" && signingSupported(p.chartVer)
	if tok.CSK != "" {
		sp = ui.Start("Storing the config signing key as a Kubernetes Secret")
		if out, err := kubectlApply(secretManifest(namespace, configSigningSecret, map[string]string{"public-key": tok.CSK})); err != nil {
			sp.Fail("Could not store the config signing key")
			return diagnose(out)
		}
		sp.Success("Config signing key stored")
	} else if out, err := kubeRun("kubectl", "delete", "secret", configSigningSecret, "-n", namespace, "--ignore-not-found"); err != nil {
		return diagnose(out)
	}

	helmArgs := []string{
		"upgrade", "--install", release, helmChart,
		"--namespace", namespace,
		"--version", p.chartVer,
		"--set", "controlPlane.url=" + tok.Addr,
		"--set", "controlPlane.httpUrl=" + tok.URL,
		"--set", "controlPlane.dataPlaneId=" + tok.ID,
		"--set", "portalAgent.enabled=true",
		"--set", "portalAgent.existingSecret=origamy-byod-token",
		"--set", "portalAgent.existingSecretAuthKey=auth-token",
		"--set", "preset=" + p.preset.name,
	}
	helmArgs = append(helmArgs, clickhouseSetArgs(p.clickhouse)...)
	// mTLS: point the portal-agent at the identity Secret we stored above so it
	// presents its client cert on the tunnel and auto-rotates it (chart >= 0.1.15).
	// Required once the control plane enforces client certs (Phase 6); without
	// it the agent connects bearer-only and is refused at the handshake.
	if tok.Cert != "" {
		helmArgs = append(helmArgs, "--set", "portalAgent.tunnelTLS.identitySecret=origamy-byod-identity")
	}
	// Telemetry push (portal-agent → control plane). The agent refuses a
	// plaintext telemetry URL when the tunnel is TLS, so only wire it for an
	// https control plane; a local dev plane keeps the tunnel-side path.
	if strings.HasPrefix(tok.URL, "https://") {
		helmArgs = append(helmArgs, "--set", "controlPlane.telemetryUrl="+strings.TrimRight(tok.URL, "/")+"/api/v1/telemetry")
	}
	// Datastore auth (Redis/NATS/ClickHouse passwords). Opt-in: the chart
	// generates the passwords in-cluster into Secrets; they never leave it.
	if p.datastoreAuth {
		helmArgs = append(helmArgs, "--set", "datastores.auth.enabled=true")
	}
	// AI engine: the chart auto-generates the engine's KEK + API token when this
	// flips true, so we pass only the boolean — never a secret (disable is unused
	// at install; that path lives in `origamy upgrade`).
	aiArgs, _ := aiToggleArgs(p.aiEnabled, false)
	helmArgs = append(helmArgs, aiArgs...)
	helmArgs = append(helmArgs, signingSetArgs(signing, p.requireSigned)...)
	// Charts before 0.1.18 kill config-sync under the default liveness probe
	// before its first telemetry push (see preflight.go); give them time.
	helmArgs = append(helmArgs, legacyChartSetArgs(p.chartVer)...)
	switch p.exposeMode {
	case 1: // LoadBalancer
		helmArgs = append(helmArgs,
			"--set", "ingestGateway.service.type=LoadBalancer",
			// Request an INTERNET-FACING LB. The AWS Load Balancer Controller
			// (common on EKS) defaults NLBs to `internal` scheme — which lands the
			// gateway on private VPC IPs, unreachable by browser/SDK traffic. This
			// annotation forces public; the in-tree/GKE/AKS providers (which are
			// internet-facing by default) ignore it harmlessly.
			"--set-string", `ingestGateway.service.annotations.service\.beta\.kubernetes\.io/aws-load-balancer-scheme=internet-facing`,
		)
	case 2: // Ingress
		helmArgs = append(helmArgs,
			"--set", "ingestGateway.ingress.enabled=true",
			"--set", "ingestGateway.ingress.host="+p.ingressHost,
			"--set", "ingestGateway.ingress.className="+p.ingressClass,
		)
		if p.ingressTLSSecret != "" {
			helmArgs = append(helmArgs,
				"--set", "ingestGateway.ingress.tls.enabled=true",
				"--set", "ingestGateway.ingress.tls.secretName="+p.ingressTLSSecret,
			)
		}
	}

	sp = ui.Start("Installing data plane (%s) via Helm", p.preset.label)
	if out, err := kubeRun("helm", helmArgs...); err != nil {
		sp.Fail("Helm install failed")
		return diagnose(out)
	}
	sp.Success("Data plane installed (%s)", p.preset.label)

	// — Wait for readiness (live) ————————————————————————————————————————
	ui.Title("Bringing services online")
	sp = ui.Start("Waiting for services to become ready")
	allReady, issues := watchReadiness(namespace, sp)
	if allReady {
		sp.Success("All services are ready")
	} else if len(issues) == 0 {
		sp.Warn("Services are still starting")
	} else {
		sp.Warn("Most services are up — some need attention")
		for comp, reason := range issues {
			ui.Detail("%s — %s", ui.Bold(comp), ui.DiagnosePod(reason))
		}
	}

	// ClickHouse is asked once, without holding the install up (a fresh
	// install touches no existing table, so it needs no permission to run
	// commands in the ClickHouse pod): a table that is missing or that the
	// images cannot write fails the install, one that could not be asked is
	// said to be unverified. Either way the endpoint and summary below are
	// printed: the install itself went through, and the token is spent.
	chk := kubeEventsCheck(checkSoft, p.clickhouse != nil, "")
	recorded := false
	if chk.why != "" && !chk.images && chk.live.gen == liveMissing && p.clickhouse == nil {
		// The rerun that creates the table is an upgrade, which only a recorded
		// reset makes ask ClickHouse. Not on an external ClickHouse: the CLI
		// never creates the table there (the failure says how to by hand), and
		// the record would only hold rollbacks and deploys up.
		recorded = writeResetMarker(kubeMarker{present: true, version: p.chartVer, channel: "stable", action: markerCreate}) == nil
	}

	// — Resolve the SDK event endpoint ————————————————————————————————————
	var eventURL, eventHint string
	switch p.exposeMode {
	case 1: // LoadBalancer — wait for the cloud LB address
		spURL := ui.Start("Waiting for the load balancer address")
		if addr := waitForGatewayLB(namespace); addr != "" {
			eventURL = fmt.Sprintf("http://%s:%d", addr, gatewayAPIPort)
			spURL.Success("Load balancer ready")
		} else {
			spURL.Warn("Load balancer still provisioning")
			eventHint = "kubectl get svc -n " + namespace + " " + release + "-ingestion-gateway"
		}
	case 2: // Ingress
		if p.ingressTLSSecret != "" {
			eventURL = "https://" + p.ingressHost
		} else {
			eventURL = "http://" + p.ingressHost
		}
	default: // Internal
		eventHint = "kubectl port-forward -n " + namespace + " svc/" + release + "-ingestion-gateway " + fmt.Sprintf("%d:%d", gatewayAPIPort, gatewayAPIPort)
	}

	// — Summary ———————————————————————————————————————————————————————————
	lines := []string{
		ui.Gray("Data plane  ") + ui.Bold(tok.ID),
		ui.Gray("Size        ") + p.preset.label,
		ui.Gray("Namespace   ") + namespace,
	}
	if p.aiEnabled {
		lines = append(lines, ui.Gray("AI engine   ")+ui.Green("enabled"))
	}
	if p.datastoreAuth {
		lines = append(lines, ui.Gray("Datastores  ")+"password-protected")
	}
	if p.clickhouse != nil {
		lines = append(lines, ui.Gray("ClickHouse  ")+p.clickhouse.describe())
	}
	if line := signingSummary(tok.CSK != "", signing, p.requireSigned); line != "" {
		lines = append(lines, ui.Gray("Config      ")+line)
	}
	if eventURL != "" {
		lines = append(lines, ui.Gray("Send events ")+ui.Bold(eventURL+"/v1/identify"))
	}
	switch {
	case chk.why != "":
		lines = append(lines, "", ui.Red("The events table check FAILED: events may not be stored. Details below."))
	case chk.unverified != "":
		lines = append(lines, "", ui.Yellow("The events table was not verified (see above)."))
	}
	if allReady && chk.why == "" {
		lines = append(lines, "", ui.Green("Your dashboard will show it as Connected shortly."))
	} else if !allReady {
		lines = append(lines,
			"",
			ui.Gray("Check progress:"),
			"  kubectl get pods -n "+namespace,
		)
		if p.clickhouse != nil {
			// Services crash-loop until the schema exists on the external
			// server; the schema Job's log names the connection problem.
			lines = append(lines, "  kubectl logs -n "+namespace+" -l app.kubernetes.io/component=clickhouse-init --tail=20")
		}
	}
	if eventURL != "" {
		lines = append(lines, "", ui.Gray("Paste the event URL into your source's Setup tab in the dashboard."))
		if p.exposeMode == 2 && p.ingressTLSSecret == "" {
			lines = append(lines, ui.Gray("Plain HTTP — add a TLS Secret and set ingestGateway.ingress.tls.{enabled,secretName} (origamy upgrade --set …) before sending production traffic."))
		}
	} else if eventHint != "" {
		lines = append(lines, "", ui.Gray("Get your event endpoint:"), "  "+eventHint)
	}
	if !p.aiEnabled {
		lines = append(lines, "", ui.Gray("Add the AI engine later:"), "  origamy upgrade --enable-ai")
	}
	if chk.unverified != "" {
		warnUnverified(chk)
	}
	ui.Box("Deployed", lines)
	if chk.why != "" {
		return kubeCheckFail("The data plane is installed", kubeUpgradeCommand(p.chartVer, "stable"), chk, p.clickhouse != nil, recorded)
	}
	return nil
}

// gatewayAPIPort is the ingestion gateway's HTTP API port (matches the chart's
// ingestGateway.apiPort). SDK events POST to <endpoint>:<port>/v1/identify etc.
const gatewayAPIPort = 8081

// waitForGatewayLB polls the ingestion-gateway Service for a cloud
// load-balancer address (hostname or IP), for up to ~90s. Returns "" if the LB
// is still provisioning.
func waitForGatewayLB(ns string) string {
	svc := release + "-ingestion-gateway"
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out, err := kubeRun("kubectl", "get", "svc", svc, "-n", ns,
			"-o", "jsonpath={.status.loadBalancer.ingress[0].hostname}{.status.loadBalancer.ingress[0].ip}")
		if addr := strings.TrimSpace(out); err == nil && addr != "" {
			return addr
		}
		time.Sleep(5 * time.Second)
	}
	return ""
}

// watchReadiness polls pod status and drives the spinner with live progress.
// Returns once everything is ready, on timeout, or early when the only
// remaining problems are unrecoverable (e.g. image-pull failures that won't
// fix themselves). Job pods (the init jobs) are excluded from the service count.
func watchReadiness(ns string, sp *ui.Spinner) (bool, map[string]string) {
	deadline := time.Now().Add(5 * time.Minute)
	lastReady := -1
	stalled := 0
	for {
		pods, err := snapshotPods(ns)
		if err == nil && len(pods) > 0 {
			ready, issues := 0, map[string]string{}
			hardOnly := true
			for _, p := range pods {
				if p.ready {
					ready++
					continue
				}
				if p.issue != "" {
					issues[p.component] = p.issue
				}
				if !isUnrecoverable(p.issue) {
					hardOnly = false
				}
			}
			sp.Suffix("%d/%d services ready", ready, len(pods))

			if ready == len(pods) {
				return true, nil
			}
			if ready == lastReady {
				stalled++
			} else {
				stalled = 0
				lastReady = ready
			}
			// Don't hang once progress has plateaued: exit fast (~18s) when the
			// only thing left is an unrecoverable image-pull, or after a longer
			// plateau (~60s) when there are flagged problems that aren't fixing
			// themselves. A plateau with NO flagged issues (e.g. a slow image
			// pull still in ContainerCreating) keeps waiting until the deadline.
			if len(issues) > 0 && ((hardOnly && stalled >= 6) || stalled >= 20) {
				return false, issues
			}
		}
		if time.Now().After(deadline) {
			_, issues := summarize(ns)
			return false, issues
		}
		time.Sleep(3 * time.Second)
	}
}

type podInfo struct {
	component string
	ready     bool
	issue     string
}

func snapshotPods(ns string) ([]podInfo, error) {
	out, err := kubeRun("kubectl", "get", "pods", "-n", ns, "-o", "json")
	if err != nil {
		return nil, err
	}
	var pl struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				Labels          map[string]string `json:"labels"`
				OwnerReferences []struct {
					Kind string `json:"kind"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Ready bool `json:"ready"`
					State struct {
						Waiting *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pl); err != nil {
		return nil, err
	}

	var pods []podInfo
	for _, it := range pl.Items {
		if it.Status.Phase == "Succeeded" {
			continue // completed init job
		}
		isJob := false
		for _, o := range it.Metadata.OwnerReferences {
			if o.Kind == "Job" {
				isJob = true
			}
		}
		if isJob {
			continue
		}
		comp := it.Metadata.Labels["app.kubernetes.io/component"]
		if comp == "" {
			comp = it.Metadata.Name
		}
		ready := len(it.Status.ContainerStatuses) > 0
		issue := ""
		for _, cs := range it.Status.ContainerStatuses {
			if !cs.Ready {
				ready = false
			}
			if cs.State.Waiting != nil && isProblem(cs.State.Waiting.Reason) {
				issue = cs.State.Waiting.Reason
			}
		}
		if it.Status.Phase == "Pending" && issue == "" {
			issue = "Pending"
		}
		pods = append(pods, podInfo{component: comp, ready: ready, issue: issue})
	}
	return pods, nil
}

// summarize returns ready count and distinct issues for a final report.
func summarize(ns string) (int, map[string]string) {
	pods, err := snapshotPods(ns)
	if err != nil {
		return 0, nil
	}
	ready, issues := 0, map[string]string{}
	for _, p := range pods {
		if p.ready {
			ready++
		} else if p.issue != "" {
			issues[p.component] = p.issue
		}
	}
	return ready, issues
}

func isProblem(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName",
		"CrashLoopBackOff", "CreateContainerConfigError",
		"CreateContainerError", "RunContainerError":
		return true
	}
	return false
}

func isUnrecoverable(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
		return true
	}
	return false
}

// ── Docker ────────────────────────────────────────────────────────────────────

func hasDocker() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return runQuiet("docker", "info") == nil
}

// dockerPlan is everything a Docker install needs to know before the token is
// spent: the answers, the project directory, and a bundle already downloaded
// and checked against the release.
type dockerPlan struct {
	imageTag string
	// dir is the project directory (absolute; created by the plan).
	// existingID is set when it already held this plane: the deploy then
	// happens in place, keeping the generated passwords (the datastores hold
	// data under them) and the operator's own .env additions.
	dir           string
	existingID    string
	fullProfile   bool
	aiEnabled     bool
	ingestDomain  string
	requireSigned bool
	// bundle is the control plane's bundle, downloaded into a directory in
	// the project and swapped in by applyDocker only once every check passed
	// and the token was redeemed: a refused or failed plan leaves the
	// project's files as they were.
	bundle *bundleRefresh
	// resetPending is the release a storage reset is recorded under that this
	// deploy finishes before the services start: one an earlier upgrade began
	// here and did not finish (resetPendingVar), or, for a plane whose
	// ClickHouse has no events table, imageTag (the table is created).
	// resetFound is what ClickHouse held when the plan was made, and
	// resetDiscard whether the operator agreed to discard the pre-reset
	// table, here or in the upgrade that began the reset (resetDockerEvents).
	resetPending, resetFound string
	resetDiscard             bool
	// strict is a deploy over a plane that can touch the events table
	// (canTouchEventsTable, or ClickHouse held another table than the
	// release writes): it gets the heavy checks. A deploy of a new plane, or
	// over one that stays on its generation, does not.
	strict bool
	// close removes what the plan put in the project — the download, the
	// lock, a directory it created and nothing filled — once the deploy is
	// over, whatever its outcome (or when a prompt ends the CLI).
	close func()
}

// planDocker runs the preflight checks, downloads the bundle and asks every
// question for a Docker install of plane id, whose control plane is at base.
// The current directory is used when it is already this plane's project;
// otherwise ./origamy-dp-<id>/, reused when it exists from an earlier deploy.
// A plane deployed there already is asked what its ClickHouse holds first.
func planDocker(imageTag string, enableAI, aiSet bool, id, base string) (*dockerPlan, error) {
	ui.Title("Target")
	ui.Success("Docker detected")
	// Like the upgrade, a deploy reports success only once the images that
	// run write the events table ClickHouse holds, which a moving tag other
	// than :staging does not say.
	if releaseSchemaGeneration(imageTag) == "unknown" {
		return nil, unknownRelease(imageTag, "Nothing has been enrolled or installed.")
	}
	// The images only exist for amd64 (see preflight.go). Emulation works on
	// Docker Desktop, so this is a warning, not a stop.
	if warn := dockerArchWarning(dockerArch()); warn != "" {
		ui.Warn("%s", warn)
	}

	p := &dockerPlan{imageTag: imageTag}

	// — Directory ————————————————————————————————————————————————————————
	switch cwdID := existingProjectID("."); {
	case cwdID == id:
		p.dir, p.existingID = ".", id
	case cwdID != "":
		return nil, fail(fmt.Sprintf("This directory holds data plane %s, but the token enrolls %s.", cwdID, id),
			"Run deploy from the parent directory to create ./origamy-dp-"+id+"/ for the new plane, or uninstall the old one first.")
	default:
		p.dir = "origamy-dp-" + id
		if existingProjectID(p.dir) == id {
			p.existingID = id
		}
	}
	abs, err := filepath.Abs(p.dir)
	if err != nil {
		return nil, fail("Could not read the working directory.", err.Error())
	}
	p.dir = abs
	envPath := filepath.Join(p.dir, ".env")
	if p.existingID != "" {
		ui.Step("Data plane %s is already deployed in ./%s — it will be re-deployed in place, keeping its passwords, data and extra .env settings.", ui.Bold(id), filepath.Base(p.dir))
	}

	// One run at a time on a project (lock.go). Everything the plan puts in
	// it until the token is redeemed is removed again on the way out.
	_, statErr := os.Stat(p.dir)
	created := errors.Is(statErr, os.ErrNotExist)
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return nil, fail("Could not create the working directory.", err.Error())
	}
	unlock, err := lockProject(p.dir, "origamy deploy")
	if err != nil {
		if created {
			_ = os.Remove(p.dir)
		}
		return nil, lockFail(err, "Nothing has been enrolled or installed.")
	}
	// Ctrl-C, SIGTERM or SIGHUP end the deploy through close too; once it is
	// over, after its cleanup, they are the CLI's own again.
	stopSignals := exitOnSignal()
	p.close = onExit(func() {
		if p.bundle != nil {
			p.bundle.cleanup()
		}
		unlock()
		if created {
			_ = os.Remove(p.dir) // only while nothing was put in it
		}
		stopSignals()
	})
	refuse := func(err error) (*dockerPlan, error) {
		p.close()
		return nil, err
	}
	nothing := "Nothing has been enrolled or installed."
	if p.existingID != "" {
		// The plane must run under the compose project name every command
		// pins here, or `up -d` would start a second copy of it.
		if err := otherProjectName(p.dir, envPath, nothing); err != nil {
			return refuse(err)
		}
	}

	// — Bundle ——————————————————————————————————————————————————————————
	// The whole bundle (compose + ClickHouse schema/users config + the
	// Caddyfile) is served by the control plane at /byod/*. The Caddyfile is
	// fetched even without a domain: it is inert until the "ingress" profile
	// is on, and a later `up` with the profile added would otherwise turn the
	// missing bind-mount source into an empty directory and crash Caddy. It
	// waits in a directory in the project until applyDocker swaps it in, with
	// the release the control plane says it is for recorded so `origamy
	// upgrade` can tell these files from later edits here.
	if p.existingID != "" {
		if stale := removeStaleBundleDirs(p.dir); len(stale) > 0 {
			ui.Step("Removed the bundle download an interrupted run left behind (%s).", strings.Join(stale, ", "))
		}
	}
	sp := ui.Start("Downloading the deploy bundle")
	served, _ := servedBundleVersion(base)
	r, err := prepareBundleRefresh(p.dir, base, served, dotenvValue(envPath, "DP_IMAGE_TAG"), true, nil)
	if err != nil {
		name := "the deploy bundle"
		if fe := (*bundleFileError)(nil); errors.As(err, &fe) {
			name = fe.name
		}
		sp.Fail("Could not download %s", name)
		if errors.Is(err, errNotServed) {
			return refuse(fail(fmt.Sprintf("Your control plane does not serve %s.", name),
				"It predates this CLI's deploy bundle — upgrade the control plane, or deploy with an older CLI. "+nothing))
		}
		return refuse(fail(fmt.Sprintf("Could not download %s from your control plane (%s).", name, base),
			err.Error()+"\nCheck that this host reaches the control plane over HTTPS (DNS, proxy, firewall), then retry with the same token: nothing has been enrolled."))
	}
	p.bundle = r
	sp.Success("Bundle downloaded (%s)", strings.Join(bundleFiles, ", "))

	// — What ClickHouse holds ———————————————————————————————————————————
	// For a plane that already runs, what its volume holds decides, as in the
	// upgrade: ClickHouse is started on the release it runs now when it does
	// not answer, and the deploy is refused when it still cannot be asked. A
	// storage reset an earlier upgrade left unfinished is finished, and a
	// missing table created, before the services start (deployResetPlan).
	// First, what would make a plain `docker compose` here start another
	// release than this deploy does (a shell export, duplicate .env lines).
	dockerTagNotes(envPath, imageTag)
	live := liveSchema{}
	if p.existingID != "" {
		// As in the upgrade, only a move that can touch the events table gets
		// the heavy checks: ClickHouse asked, started if need be, and the
		// images compared with the release's. Any other (the release the plane
		// runs, or one of its generation) only peeks at ClickHouse, and
		// becomes a strict move when it holds another table than the release
		// writes.
		installed := dotenvValue(envPath, "DP_IMAGE_TAG")
		p.strict = canTouchEventsTable(installed, imageTag, readEnvVar(envPath, resetPendingVar))
		if p.strict {
			live = askClickHouse(p.dir, envPath, installed)
		} else if live = peekClickHouse(p.dir, envPath, installed); live.gen != "" && live.gen != releaseSchemaGeneration(imageTag) {
			p.strict = true
		}
		switch {
		case live.gen == "" && !p.strict:
			ui.Warn("ClickHouse did not answer, so which events table it holds was not checked: deploying %s over %s does not change which table the images write.", imageTag, orDash(installed))
		case live.gen == "":
			return refuse(fail("ClickHouse could not be asked which events table this plane holds, so what deploying over it does to the table cannot be checked.",
				outputHint(live.out)+"Check: "+composeHint(p.dir, envPath)+" logs clickhouse\nOnce it answers, rerun deploy with the same token. "+nothing))
		case live.gen == liveOther:
			return refuse(fail(fmt.Sprintf("ClickHouse's %s here has payload columns of type %s, neither the pre-reset (String) nor the new (JSON) schema, which no release writes.", eventsTable, live.typ),
				"Check what created it, and drop it yourself if it should be recreated with the new schema; then rerun deploy. "+nothing))
		}
		if p.resetPending, p.resetFound, p.resetDiscard, err = deployResetPlan(p.dir, imageTag, live, r); err != nil {
			return refuse(err)
		}
	}

	// — Schema guard ————————————————————————————————————————————————————
	// The bundle's ClickHouse schema follows the control plane's current build;
	// the images follow imageTag. A mismatch comes up looking healthy and then
	// drops every event at insert — refuse it here rather than find out later.
	// For a plane that already runs, what its volume holds is checked against
	// the release, unless the reset above is about to replace it; and the
	// schema the deploy installs (the served clickhouse-init.sql, which a new
	// volume, or a reset, would get) must be one the release writes too.
	if live.known() && p.resetPending == "" {
		if why := liveSchemaMismatch(live.gen, imageTag); why != "" {
			return refuse(fail(why, nothing))
		}
	}
	if !live.known() || p.resetPending != "" || r.served("clickhouse-init.sql") {
		if sql, err := os.ReadFile(bundlePath(r, "clickhouse-init.sql")); err == nil {
			if why := bundleSchemaMismatch(string(sql), imageTag); why != "" {
				return refuse(fail(why, "Pass --version <release> to install a matching data-plane release, or ask Origamy which release your control plane expects. "+nothing))
			}
		}
	}

	// — Questions ———————————————————————————————————————————————————————
	// A re-deploy offers the plane's current answers as the defaults.
	fullDefault, ingestDefault := true, ""
	if p.existingID != "" {
		profiles := composeProfiles(envPath)
		fullDefault = hasProfile(profiles, "full")
		ingestDefault = dotenvValue(envPath, "INGEST_DOMAIN")
		if !aiSet {
			enableAI = hasProfile(profiles, "agentic")
		}
	}

	// No "deployment size" question here: the compose bundle has a single
	// replica of everything, so there is nothing a size could change.

	// Journeys, broadcasts and human tasks run in workflow-engine, which needs
	// the bundled Postgres (compose profile "full"). Default on, matching the
	// Kubernetes install where workflowEngine.enabled is true.
	ui.Title("Engagement services")
	p.fullProfile = promptYesNo("Enable journeys, broadcasts and human tasks? Adds Postgres + workflow-engine.", fullDefault)

	// Same contract as Kubernetes: the CLI controls engine presence (compose
	// profile "agentic"), the control plane controls use. The KEK and internal
	// API token are generated on this host and never leave it.
	p.aiEnabled = enableAI
	if !aiSet {
		ui.Title("AI engine")
		p.aiEnabled = promptYesNo("Enable the Origamy AI engine? Adds the orchestrator engine; requires the AI package in your dashboard.", enableAI)
	}

	// The gateway listens on :8081 over plain HTTP. With a domain, the bundled
	// Caddy (profile "ingress") terminates HTTPS with a Let's Encrypt cert.
	ui.Title("Event endpoint")
	ui.Step("Plain HTTP on :8081 by default. Give it a domain to serve HTTPS via the bundled Caddy")
	ui.Step("(needs ports 80/443 reachable and the domain's DNS A record pointing at this host).")
	label := "Event domain (e.g. events.mycompany.com) [none]"
	if ingestDefault != "" {
		label = "Event domain [" + ingestDefault + "]"
	}
	p.ingestDomain = promptString(label)
	if p.ingestDomain == "" {
		p.ingestDomain = ingestDefault
	}
	return p, nil
}

// bundlePath is where bundle file name is once refresh r is swapped in: the
// served version (from the download directory) unless r keeps the
// project's own.
func bundlePath(r *bundleRefresh, name string) string {
	if r.served(name) {
		return filepath.Join(r.tmp, name)
	}
	return filepath.Join(r.dir, name)
}

// deployResetPlan decides what an in-place deploy of imageTag does with the
// events table before the services start, from what the project's ClickHouse
// holds (live) and the bundle downloaded for the deploy (r). It finishes a
// storage reset an earlier upgrade began here and did not finish
// (resetPendingVar), and creates a missing table for a release that writes
// the new schema. It returns the release the reset is recorded under ("" for
// nothing to do), what ClickHouse held (found) and whether the operator
// agreed to discard the pre-reset table (discard: in the upgrade that began
// the reset, and asked again here before it is moved aside), for
// resetDockerEvents. When ClickHouse holds the new table, a recorded reset is
// finished without asking anything; the pre-reset table is moved aside and
// dropped only for a recorded reset, once the operator agreed like the
// upgrade asks (without a record, liveSchemaMismatch points at the upgrade).
// The bundle must be able to create the table: ClickHouse 25.3 and the new
// schema. Nothing has been enrolled when it refuses.
func deployResetPlan(dir, imageTag string, live liveSchema, r *bundleRefresh) (target, found string, discard bool, err error) {
	envPath := filepath.Join(dir, ".env")
	pending := readEnvVar(envPath, resetPendingVar)
	_, create := dockerResetAction(live.gen, imageTag)
	if pending == "" && !create {
		return "", "", false, nil
	}
	nothing := "Nothing has been enrolled or installed."
	what := fmt.Sprintf("ClickHouse here has no events table, which this deploy would create with the %s schema", imageTag)
	finish := upgradeCommand(imageTag) + " (in ./" + filepath.Base(dir) + ")"
	if pending != "" {
		what = fmt.Sprintf("An earlier upgrade to %s began the storage reset here and did not finish it (%s in .env)", pending, resetPendingVar)
		finish = upgradeCommand(pending) + " (in ./" + filepath.Base(dir) + ")"
		if releaseSchemaGeneration(imageTag) != "json" {
			return "", "", false, fail(what+", and "+cannotFinishReset(imageTag)+".", "Finish it first: "+finish+". "+nothing)
		}
		if live.gen == "json" {
			ui.Step("An earlier upgrade to %s recorded an unfinished storage reset here (%s in .env), but ClickHouse already holds the new events table (%s payload columns): it is kept, and the record is cleared once the services are up.", pending, resetPendingVar, live.typ)
			return pending, live.gen, true, nil
		}
	}
	why, culprit, cerr := resetComposeProblem(dir, envPath, imageTag, r)
	initSQL, _ := os.ReadFile(bundlePath(r, "clickhouse-init.sql"))
	switch {
	case cerr != nil:
		return "", "", false, fail(what+", and docker compose could not render this project's configuration to tell which ClickHouse it runs.",
			cerr.Error()+"\nFix the compose files or .env, then rerun deploy. "+nothing)
	case len(culprit) > 0:
		headline, hint := resetComposeRefusal(dir, imageTag, nil, "", why, culprit, nil)
		return "", "", false, fail(headline, strings.Replace(hint, "rerun `"+upgradeCommand(imageTag)+"`. Nothing has been changed.", "rerun deploy. "+nothing, 1))
	case why != "" || schemaGeneration(string(initSQL)) != "json":
		return "", "", false, fail(what+", and your control plane's bundle cannot do it: it needs ClickHouse "+minClickHouseForJSON+" and the new schema (native JSON payload columns).",
			"Update the control plane, then rerun deploy, or run "+finish+". "+nothing)
	}
	switch {
	case live.gen == "string":
		if !confirmResumedReset(pending, imageTag, false) {
			return "", "", false, fail("Cancelled.", nothing)
		}
	case pending != "":
		ui.Step("An earlier upgrade to %s began the storage reset here and did not finish it (%s in .env). ClickHouse has no events table now: this deploy creates it with the new schema before the services start (a pre-reset table the earlier run moved aside is dropped once it is in place).", pending, resetPendingVar)
	default:
		ui.Step("ClickHouse here has no events table: this deploy creates it with the new schema before the services start; nothing is dropped.")
		return imageTag, live.gen, false, nil
	}
	return pending, live.gen, true, nil
}

// applyDocker installs the plane described by p for the enrolled token.
func applyDocker(tok *token.Enrollment, keyPEM []byte, p *dockerPlan) error {
	ui.Title("Provisioning")
	if err := os.Chdir(p.dir); err != nil {
		return fail("Could not enter the working directory.", err.Error())
	}
	dir := filepath.Base(p.dir)
	if p.existingID != "" {
		ui.Success("Re-deploying in ./%s", dir)
	} else {
		ui.Success("Working directory ./%s", dir)
	}

	// The bundle downloaded with the plan goes in now that every check passed
	// and the token was redeemed. A file it replaces is backed up as
	// <file>.bak-<release> (an edited one too); a symbolic link or a
	// directory with files in it is the operator's own and is kept, the
	// served version next to it as <file>.new.
	b := p.bundle
	if err, rerr := b.swapIn(); err != nil {
		if rerr != nil {
			return fail("Could not put the deploy bundle in place, nor the previous files back.",
				err.Error()+"\n"+rerr.Error()+"\nThe backups are next to them in ./"+dir+" (*.bak-*).")
		}
		return fail("Could not put the deploy bundle in place; the previous files were put back.",
			err.Error()+"\nFix the cause, then rerun deploy here with a fresh token.")
	}
	if err := b.commit(); err != nil {
		ui.Warn("Could not record the bundle in %s (%v); `origamy upgrade` falls back to the bundle versions it knows.", bundleManifestFile, err)
	}
	var backups []string
	for _, f := range b.files {
		switch {
		case f.edited:
			ui.Warn("Replaced your edited %s with your control plane's version; yours is backed up as %s", f.name, filepath.Base(f.backup))
		case f.backup != "":
			backups = append(backups, filepath.Base(f.backup))
		}
	}
	if len(backups) > 0 {
		ui.Step("Replaced %s with your control plane's version; the previous ones are backed up (%s).", strings.Join(b.changed(), ", "), strings.Join(backups, ", "))
	}
	for _, name := range b.kept() {
		ui.Warn("Kept %s (not a regular file, so the operator's own); your control plane's version is next to it as %s.new", name, name)
	}

	// Secrets are generated HERE, in the customer's environment, and written to
	// the local .env — they never reach Origamy. Reuse any already in .env from
	// a prior deploy so a re-deploy doesn't rotate them out from under the
	// running datastores (which hold data). The AI secrets are carried forward
	// even when AI is off: the KEK must survive a disable/enable cycle or the
	// per-workspace LLM keys encrypted under it become unreadable.
	existingEnv, _ := os.ReadFile(".env")
	// The profiles the plane ran with: the services of one the answers turn
	// off are stopped before `up -d` (stopTurnedOff).
	before := composeProfiles(".env")
	orchKEK := readEnvVar(".env", "ORCH_KEK")
	orchToken := readEnvVar(".env", "ORCH_ENGINE_API_TOKEN")
	if p.aiEnabled {
		orchKEK = existingOrRandomKEK(".env", "ORCH_KEK")
		orchToken = existingOrRandom(".env", "ORCH_ENGINE_API_TOKEN")
	}
	// Postgres bakes its password into the volume at first start. A project
	// whose .env never set DB_PASSWORD (dashboard snippet, older CLI) runs on
	// the bundle's default, and a freshly generated one would lock
	// workflow-engine out of its own data.
	dbPw := existingOrRandom(".env", "DB_PASSWORD")
	if p.existingID != "" && readEnvVar(".env", "DB_PASSWORD") == "" {
		dbPw = "origamy"
		ui.Warn("Keeping the bundled Postgres on its original default password: it was initialised without one and cannot be rotated from here.")
	}
	// Only full, agentic and ingress follow the answers; any other profile the
	// plane runs (logs, loki, the operator's own) stays in COMPOSE_PROFILES,
	// so a re-deploy never stops its services.
	profiles := deployProfiles(before, p.fullProfile, p.aiEnabled, p.ingestDomain != "")
	env := renderDockerEnv(dockerEnvParams{
		TunnelAddr:    tok.Addr,
		HTTPURL:       tok.URL,
		DataPlaneID:   tok.ID,
		AuthToken:     tok.Tok,
		ImageTag:      p.imageTag,
		Preset:        "starter",
		Profiles:      profiles,
		IngestDomain:  p.ingestDomain,
		RedisPw:       existingOrRandom(".env", "DP_REDIS_PASSWORD"),
		NatsPw:        existingOrRandom(".env", "NATS_PASSWORD"),
		ClickHousePw:  existingOrRandom(".env", "CLICKHOUSE_PASSWORD"),
		DBPw:          dbPw,
		OrchKEK:       orchKEK,
		OrchToken:     orchToken,
		AIEnabled:     p.aiEnabled,
		MTLS:          tok.Cert != "",
		SigningKey:    tok.CSK,
		RequireSigned: p.requireSigned,

		// Delivery-event signing key: generated once, then kept (a new one
		// would stop the callbacks and webhooks already issued from verifying).
		EngagementSecret: existingOrRandomHex(".env", "ENGAGEMENT_SECRET"),
	})
	// Keep whatever else the operator put in the old .env (rate limits, CORS,
	// an opt-in LLM key): only the keys the CLI owns are rewritten.
	env = mergeEnv(string(existingEnv), env)
	// 0600, also over an operator-created 0644 .env: it holds secrets now.
	if err := writeEnvFile(".env", []byte(env), 0o600); err != nil {
		return fail("Could not write .env.", err.Error())
	}
	ui.Success("Wrote .env (secrets generated locally — never sent to Origamy)")

	// mTLS identity files for the portal-agent (only when the control plane
	// issued a cert). Written into ./certs — the dir the compose bundle
	// bind-mounts read-write, so the renew loop's rotations persist across
	// restarts. The private key stays on disk here, never transmitted.
	if tok.Cert != "" {
		if err := os.MkdirAll("certs", 0o700); err != nil {
			return fail("Could not create certs directory.", err.Error())
		}
		for name, content := range map[string]string{"tls.crt": tok.Cert, "tls.key": string(keyPEM), "ca.crt": tok.CAChain} {
			if err := os.WriteFile("certs/"+name, []byte(content), 0o600); err != nil {
				return fail("Could not write certs/"+name+".", err.Error())
			}
			_ = os.Chmod("certs/"+name, 0o600)
		}
		ui.Success("Wrote mTLS identity (certs/tls.crt, tls.key, ca.crt)")
	}

	// The storage reset an earlier upgrade left unfinished, or the missing
	// events table, on the bundle just swapped in, before anything runs on
	// the new release. Recorded in .env first (mergeEnv kept an earlier
	// record), so a failure is finished by the next run.
	envPath := filepath.Join(p.dir, ".env")
	// Every docker compose command runs the release in DP_IMAGE_TAG itself,
	// whatever the shell exports.
	tagged := composeEnv(p.imageTag)
	var outcome resetResult
	if p.resetPending != "" {
		ui.Title("Storage reset")
		if readEnvVar(envPath, resetPendingVar) == "" {
			if err := setEnvVar(envPath, resetPendingVar, p.resetPending); err != nil {
				return fail("Could not update .env.", err.Error())
			}
		}
		var err error
		if outcome, err = resetDockerEvents(p.dir, envPath, p.imageTag, p.resetFound, tagged, p.resetDiscard); err != nil {
			return err
		}
	}

	ui.Title("Bringing services online")
	// Profiles come from COMPOSE_PROFILES in .env, so every later compose
	// invocation (upgrade/status) sees the same service set. The services
	// only a profile a re-deploy just turned off ran are stopped (compose
	// keeps them, and `up -d` leaves them running on the old release), and
	// --remove-orphans removes those of services the bundle no longer has.
	if p.existingID != "" {
		stopTurnedOff(p.dir, envPath, tagged, before, profiles)
		if p.strict {
			removeInactiveDataPlane(p.dir, envPath, tagged, profiles)
		}
	}
	sp := ui.Start("Starting services")
	if out, err := dockerRun(tagged, "docker", composeCmd(p.dir, envPath, "up", "-d", "--remove-orphans")...); err != nil {
		sp.Fail("docker compose failed")
		return diagnoseDocker(out, "rerun `"+composeHint(p.dir, envPath)+" up -d`")
	}
	sp.Success("Services started")
	// Compose recreates a container when its definition changes, not when a
	// file it bind-mounts does: the services whose files the deploy replaced
	// are restarted. The upgrade that began a reset may have swapped bundle
	// files under them and stopped before restarting them too; the reset is
	// finished once they were.
	var restarts []string
	if p.existingID != "" {
		restarts = b.restarts(profiles)
	}
	if p.resetPending != "" {
		for _, svc := range bundleMountServices(profiles) {
			if !slices.Contains(restarts, svc) {
				restarts = append(restarts, svc)
			}
		}
	}
	restarted := restartMounts(p.dir, envPath, tagged, restarts)
	// The deploy succeeds only when the events table the services now run on
	// is the one the release writes, and the images that run are the
	// release's.
	if err := checkAfterUp(p.dir, envPath, p.imageTag, p.strict); err != nil {
		return err
	}
	if p.resetPending != "" {
		if restarted {
			if err := clearResetRecord(envPath, p.imageTag); err != nil {
				return err
			}
		} else {
			ui.Warn("%s stays in .env until those services restart: the next `%s` restarts them and clears it, dropping nothing.", resetPendingVar, upgradeCommand(p.imageTag))
		}
	}

	// `up -d` returning 0 only means the containers were created. Watch them
	// stay up for a while so a wrong token, a refused mTLS handshake or a
	// crashing service is reported here, with its logs, instead of as a plane
	// that never shows up Connected.
	sp = ui.Start("Waiting for services to become healthy")
	healthy, failed := waitForCompose(p.dir, envPath, tagged, sp)
	switch {
	case healthy:
		sp.Success("All services are up and stable")
	case len(failed) > 0:
		sp.Fail("Some services exited or keep restarting")
		for _, svc := range failed {
			ui.Detail("%s — last log lines:", ui.Bold(svc))
			for _, l := range strings.Split(composeLogs(p.dir, envPath, tagged, svc, 15), "\n") {
				if l != "" {
					fmt.Printf("      %s\n", ui.Gray(l))
				}
			}
		}
		return fail("The data plane did not come up cleanly.",
			"Fix the cause above, then rerun `"+composeHint(p.dir, envPath)+" up -d` — or rerun deploy here with a fresh token to re-enroll.")
	default:
		sp.Warn("Services are still starting")
	}

	lines := []string{
		ui.Gray("Data plane  ") + ui.Bold(tok.ID),
		ui.Gray("Location    ") + "./" + dir,
		ui.Gray("Release     ") + p.imageTag,
	}
	if p.fullProfile {
		lines = append(lines, ui.Gray("Engagement  ")+ui.Green("enabled"))
	}
	if p.aiEnabled {
		lines = append(lines, ui.Gray("AI engine   ")+ui.Green("enabled"))
	}
	if line := signingSummary(tok.CSK != "", signingSupported(p.imageTag), p.requireSigned); line != "" {
		lines = append(lines, ui.Gray("Config      ")+line)
	}
	if len(backups) > 0 {
		lines = append(lines, ui.Gray("Bundle      ")+"replaced "+strings.Join(b.changed(), ", ")+"; previous versions in *.bak-*")
	}
	switch {
	case outcome.outcome == resetNone:
	case len(outcome.kept) > 0:
		lines = append(lines, ui.Gray("Storage     ")+ui.Yellow("reset finished: the events table is the new one; kept in ClickHouse, not dropped (see above): events."+strings.Join(outcome.kept, ", events.")))
	case outcome.outcome == resetDropped:
		lines = append(lines, ui.Gray("Storage     ")+ui.Yellow("reset finished: event history was discarded; profiles, traits, segments and journeys were kept"))
	case outcome.outcome == resetCreated && len(outcome.dropped) > 0:
		lines = append(lines, ui.Gray("Storage     ")+ui.Yellow("the events table was created with the new schema, and the pre-reset one an earlier upgrade had moved aside was dropped; profiles, traits, segments and journeys were kept"))
	case outcome.outcome == resetCreated:
		lines = append(lines, ui.Gray("Storage     ")+ui.Yellow("the events table was created with the new schema (nothing was dropped); profiles, traits, segments and journeys were kept"))
	case len(outcome.dropped) > 0:
		lines = append(lines, ui.Gray("Storage     ")+ui.Yellow("reset finished: ClickHouse already held the new events table; the pre-reset one an earlier upgrade had moved aside was dropped"))
	default:
		lines = append(lines, ui.Gray("Storage     ")+"reset finished: ClickHouse already held the new events table; nothing was dropped")
	}
	if p.ingestDomain != "" {
		lines = append(lines,
			ui.Gray("Send events ")+ui.Bold("https://"+p.ingestDomain+"/v1/identify"),
			"",
			ui.Gray("Point "+p.ingestDomain+"'s DNS A record at this host; Caddy provisions the certificate on first request."))
	} else {
		lines = append(lines,
			ui.Gray("Send events ")+ui.Bold("http://<this host>:"+fmt.Sprintf("%d", gatewayAPIPort)+"/v1/identify"),
			"",
			ui.Gray("Plain HTTP — for production SDK traffic set INGEST_DOMAIN in .env, add \"ingress\" to COMPOSE_PROFILES and rerun `docker compose --env-file .env up -d`."))
	}
	if healthy {
		lines = append(lines, "", ui.Green("Your dashboard will show it as Connected shortly."))
	} else {
		lines = append(lines, "", ui.Gray("Check progress:"), "  docker compose --env-file .env ps   (in ./"+dir+")")
	}
	lines = append(lines, ui.Gray("Logs: ")+"docker compose -f ./"+dir+"/docker-compose.yml logs -f portal-agent")
	if !p.aiEnabled {
		lines = append(lines, "", ui.Gray("Add the AI engine later:"), "  origamy upgrade --enable-ai")
	}
	ui.Box("Deployed", lines)
	return nil
}

// ── Prompt helpers ────────────────────────────────────────────────────────────

// stdin is shared by every prompt. A fresh bufio.Reader per question would
// swallow whatever the first one read ahead — with answers piped in
// (`printf '1\nn\n' | origamy deploy …`) only the first would be honoured.
var stdin = bufio.NewReader(os.Stdin)

// stdinIsTerminal says whether a person is answering. Answers from a pipe or
// file cannot be corrected interactively, so a rejected one must stop the
// run — the prompts run before anything is enrolled or installed, so that is
// always safe — instead of looping on EOF and silently taking defaults.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

// rejectAnswer stops a non-interactive run on an unusable answer.
func rejectAnswer(answer string) {
	if stdinIsTerminal {
		return
	}
	fmt.Println()
	ui.Fail("Unexpected answer on a non-interactive stdin: %q", answer)
	ui.Detail("The prompts changed in this release; check the piped answers. Nothing has been enrolled or installed.")
	fmt.Println()
	exitNow(1)
}

func promptChoice(label string, min, max, defaultVal int) int {
	for {
		fmt.Printf("\n  %s %s ", label, ui.Gray(fmt.Sprintf("[%d]", defaultVal)))
		line, _ := stdin.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return defaultVal
		}
		var n int
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= min && n <= max {
			return n
		}
		rejectAnswer(line)
		ui.Warn("Please enter a number between %d and %d.", min, max)
	}
}

func promptString(label string) string {
	fmt.Printf("  %s: ", label)
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

// promptYesNo asks a yes/no question, returning defaultYes on an empty answer.
func promptYesNo(label string, defaultYes bool) bool {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	for {
		fmt.Printf("\n  %s %s ", label, ui.Gray(suffix))
		line, _ := stdin.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return defaultYes
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		rejectAnswer(strings.TrimSpace(line))
		ui.Warn("Please answer y or n.")
	}
}

// ── error helpers ─────────────────────────────────────────────────────────────

// fail builds a styled, actionable error to return from a command. A hint may
// span several lines.
func fail(headline, hint string) error {
	fmt.Println()
	ui.Fail("%s", headline)
	for _, l := range strings.Split(hint, "\n") {
		if l != "" {
			ui.Detail("%s", l)
		}
	}
	fmt.Println()
	return errSilent
}

// diagnose inspects captured command output, prints the diagnosis, and returns
// a silent error so cobra doesn't re-print a raw message.
func diagnose(output string) error {
	return printDiagnosis(ui.DiagnoseHelm(output), output)
}

// diagnoseDocker is diagnose for a Docker command of a compose install: its
// output read as Docker's (ui.DiagnoseDocker), and rerun the command that
// tries again.
func diagnoseDocker(output, rerun string) error {
	return printDiagnosis(ui.DiagnoseDocker(output, rerun), output)
}

func printDiagnosis(d ui.Diagnosis, output string) error {
	fmt.Println()
	ui.Fail("%s", d.Headline)
	if d.Hint != "" {
		ui.Detail("%s", d.Hint)
	}
	if tail := lastLines(output, 6); tail != "" {
		fmt.Println()
		ui.Detail("%s", ui.Dim("— output —"))
		for _, l := range strings.Split(tail, "\n") {
			fmt.Printf("      %s\n", ui.Gray(l))
		}
	}
	fmt.Println()
	return errSilent
}

// errSilent is returned after we've already printed a friendly error, so the
// root command exits non-zero without printing anything else.
var errSilent = fmt.Errorf("")

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ── shell helpers ─────────────────────────────────────────────────────────────

func runQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// runCaptured runs a command and returns combined stdout+stderr.
func runCaptured(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	c := exec.Command(name, args...)
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.String(), err
}

// commandEnv is the CLI's own environment with extra on top: KEY=value sets
// KEY (over the CLI's), a bare KEY leaves it out. Names compare as the
// system does (without case on Windows).
func commandEnv(extra []string) []string {
	env := os.Environ()
	for _, e := range extra {
		key, _, set := strings.Cut(e, "=")
		env = slices.DeleteFunc(env, func(v string) bool {
			name, _, _ := strings.Cut(v, "=")
			return name == key || (runtime.GOOS == "windows" && strings.EqualFold(name, key))
		})
		if set {
			env = append(env, e)
		}
	}
	return env
}

// runCapturedEnv is runCaptured with extra in the command's environment (on
// top of the CLI's own, see commandEnv).
func runCapturedEnv(extraEnv []string, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	c := exec.Command(name, args...)
	c.Env = commandEnv(extraEnv)
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.String(), err
}

// runCapturedEnvTimeout is runCapturedEnv that gives up after d: the command
// is killed and an error returned with whatever it printed so far.
func runCapturedEnvTimeout(d time.Duration, extraEnv []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var buf bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Env = commandEnv(extraEnv)
	c.Stdout = &buf
	c.Stderr = &buf
	// `docker compose` runs the compose plugin as a child process, which can
	// keep the output open after docker itself is killed.
	c.WaitDelay = 2 * time.Second
	err := c.Run()
	if ctx.Err() != nil {
		return buf.String(), fmt.Errorf("no answer within %s", d)
	}
	return buf.String(), err
}

// runWithStdin runs a command with the given stdin and returns combined
// stdout+stderr.
func runWithStdin(input string, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	c := exec.Command(name, args...)
	c.Stdin = strings.NewReader(input)
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.String(), err
}
