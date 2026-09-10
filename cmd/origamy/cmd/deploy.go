package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	// chart and images share one version per release. 0.1.17 is the first chart
	// carrying every feature this CLI can toggle (mTLS identity 0.1.15,
	// orchestrator engine 0.1.16, predictor 0.1.17); older charts silently
	// ignore those values. `origamy upgrade` resolves the latest published
	// chart at run time, so this pin only governs a fresh install.
	helmVersion = "0.1.17"
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

Auto-detects your environment:
  • Kubernetes cluster (kubectl) → Helm install into origamy-dp namespace
  • Docker                       → Docker Compose in ./origamy-dp-<id>/

Get your enrollment token from the Connections page in your Origamy dashboard.

Add --enable-ai (or answer the prompt) to also switch on the agentic AI engine
during install. On Kubernetes it adds ~1 pod; on Docker it enables the
"agentic" compose profile. The engine stays inert until you enable AI and add an
LLM credential in your dashboard — you can also add it later with
'origamy upgrade --enable-ai'.

Add --datastore-auth on Kubernetes to password-protect the bundled Redis, NATS
and ClickHouse (the chart generates the passwords in-cluster). Docker installs
always get generated datastore passwords.`,
	Example: `  origamy deploy --token dpe_xxx
  origamy deploy --token dpe_xxx --enable-ai
  origamy deploy --token dpe_xxx --datastore-auth`,
	RunE: func(cmd *cobra.Command, args []string) error {
		t, _ := cmd.Flags().GetString("token")
		enableAI, _ := cmd.Flags().GetBool("enable-ai")
		aiSet := cmd.Flags().Changed("enable-ai")
		datastoreAuth, _ := cmd.Flags().GetBool("datastore-auth")
		return runDeploy(t, enableAI, aiSet, datastoreAuth)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

// deployChartVersion optionally overrides the pinned helmVersion for the initial
// install (bound to --version). Empty means "use the CLI's pinned default".
var deployChartVersion string

func init() {
	deployCmd.Flags().StringP("token", "t", "", "Enrollment token from your Origamy dashboard (required)")
	deployCmd.Flags().StringVar(&deployChartVersion, "version", "", "Chart version to install (default: the CLI's pinned version)")
	deployCmd.Flags().Bool("enable-ai", false, "Enable the Origamy AI (agentic) engine at install")
	deployCmd.Flags().Bool("datastore-auth", false, "Password-protect the bundled Redis/NATS/ClickHouse (Kubernetes; passwords generated in-cluster)")
	_ = deployCmd.MarkFlagRequired("token")
}

func runDeploy(raw string, enableAI, aiSet, datastoreAuth bool) error {
	// Generate a keypair + CSR locally so enrollment can request an mTLS identity
	// — the private key never leaves this machine. Enroll falls back to a
	// bearer-only token if the control plane has no CA configured.
	keyPEM, csrPEM, err := token.GenerateIdentity()
	if err != nil {
		return fail("Could not generate a data-plane keypair.", err.Error())
	}
	tok, err := token.Enroll(raw, csrPEM)
	if err != nil {
		return fail("Invalid or unusable enrollment token.",
			"Get a fresh token from the Connections page in your dashboard — tokens expire after 72 hours.")
	}

	if tok.Exp > 0 && time.Now().Unix() > tok.Exp {
		return fail("Your enrollment token has expired.",
			"Generate a new one from the Connections page in your dashboard.")
	}

	ui.Title("Origamy data plane")
	ui.KV("Data plane", ui.Bold(tok.ID))
	ui.KV("Control", tok.Addr)
	if tok.Cert != "" {
		ui.KV("Identity", "mTLS certificate issued")
	}

	switch {
	case hasKubernetes():
		return deployKubernetes(tok, keyPEM, enableAI, aiSet, datastoreAuth)
	case hasDocker():
		return deployDocker(tok, keyPEM, enableAI, aiSet)
	default:
		return fail("No Kubernetes cluster or Docker found on this machine.",
			"Install Docker (https://docs.docker.com/get-docker/) or point kubectl at a cluster, then retry.")
	}
}

// ── Kubernetes ────────────────────────────────────────────────────────────────

func hasKubernetes() bool {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return false
	}
	return runQuiet("kubectl", "cluster-info") == nil
}

func deployKubernetes(tok *token.Enrollment, keyPEM []byte, enableAI, aiSet, datastoreAuth bool) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fail("Helm is required for Kubernetes installs.",
			"Install it from https://helm.sh/docs/intro/install/ and retry.")
	}

	ui.Title("Target")
	ui.Success("Kubernetes cluster detected")

	// — Deployment tier ——————————————————————————————————————————————————
	ui.Title("Deployment size")
	for i, p := range presets {
		fmt.Printf("  %s  %s  %s\n", ui.Cyan(fmt.Sprintf("%d", i+1)), ui.Bold(fmt.Sprintf("%-11s", p.label)), ui.Gray(p.description))
	}
	tierIdx := promptChoice("Choose 1-3", 1, len(presets), 1)
	selected := presets[tierIdx-1]

	// — AI engine ————————————————————————————————————————————————————————
	// AI is a package customers buy; enabling it here adds ~1 pod. The flag wins
	// when set on the command line, otherwise we ask. The engine stays inert
	// until the workspace opts into AI and adds an LLM credential in the
	// dashboard — CLI controls engine presence, the control plane controls use.
	aiEnabled := enableAI
	if !aiSet {
		ui.Title("AI engine")
		aiEnabled = promptYesNo("Enable the Origamy AI engine? Adds ~1 pod; requires the AI package in your dashboard.", false)
	}
	if aiEnabled && selected.name == "starter" {
		ui.Warn("The AI engine adds a pod; the Starter tier is sized for dev/test. Consider Standard for production use.")
	}
	// The chart must actually carry the engine's templates — helm silently
	// ignores values an older chart doesn't know, and we'd report "enabled"
	// while nothing deployed.
	chartVer := helmVersion
	if deployChartVersion != "" {
		chartVer = deployChartVersion
	}
	if err := featureGate(chartVer, aiEnabled, false); err != nil {
		return fail(err.Error(), "Pass --version "+minChartAI+" or newer, or drop --enable-ai.")
	}

	// — ClickHouse ————————————————————————————————————————————————————————
	ui.Title("ClickHouse")
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("1"), ui.Bold("Embedded"), ui.Gray("deploy inside the cluster (easiest)"))
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("2"), ui.Bold("External"), ui.Gray("connect to your own ClickHouse"))
	chMode := promptChoice("Choose 1-2", 1, 2, 1)

	var chHost, chPassword string
	if chMode == 2 {
		chHost = promptString("ClickHouse host (e.g. clickhouse.mycompany.com)")
		chPassword = promptString("ClickHouse password")
	}

	// — Event endpoint ————————————————————————————————————————————————————
	// How the gateway is exposed for SDK traffic. ClusterIP (internal) alone
	// means events can't reach the plane from outside the cluster.
	ui.Title("Event endpoint")
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("1"), ui.Bold(fmt.Sprintf("%-12s", "LoadBalancer")), ui.Gray("cloud load balancer on :8081 (EKS/GKE/AKS)"))
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("2"), ui.Bold(fmt.Sprintf("%-12s", "Ingress")), ui.Gray("HTTPS on your own domain (needs an ingress controller)"))
	fmt.Printf("  %s  %s  %s\n", ui.Cyan("3"), ui.Bold(fmt.Sprintf("%-12s", "Internal")), ui.Gray("ClusterIP only — expose later"))
	exposeMode := promptChoice("Choose 1-3", 1, 3, 1)
	var ingressHost, ingressClass string
	if exposeMode == 2 {
		ingressHost = promptString("Event domain (e.g. events.mycompany.com)")
		// An Ingress with no ingressClassName is claimed by NO controller when the
		// cluster has no default class — it silently 404s. Default to nginx.
		ingressClass = promptString("Ingress class (nginx, alb, …) [nginx]")
		if ingressClass == "" {
			ingressClass = "nginx"
		}
	}

	// — Provision ——————————————————————————————————————————————————————————
	ui.Title("Provisioning")

	sp := ui.Start("Creating namespace %s", namespace)
	if out, err := runPipedCaptured(
		[]string{"kubectl", "create", "namespace", namespace, "--dry-run=client", "-o", "yaml"},
		[]string{"kubectl", "apply", "-f", "-"},
	); err != nil {
		sp.Fail("Could not create namespace")
		return diagnose(out)
	}
	sp.Success("Namespace %s ready", namespace)

	sp = ui.Start("Storing auth token as a Kubernetes Secret")
	if out, err := runPipedCaptured(
		[]string{
			"kubectl", "create", "secret", "generic", "origamy-byod-token",
			"--namespace", namespace,
			"--from-literal=auth-token=" + tok.Tok,
			"--dry-run=client", "-o", "yaml",
		},
		[]string{"kubectl", "apply", "-f", "-"},
	); err != nil {
		sp.Fail("Could not store auth token")
		return diagnose(out)
	}
	sp.Success("Auth token stored securely")

	// mTLS identity: store the issued client cert + private key + CA chain as a
	// Secret (piped via stdin, so the key never hits shell history). The
	// portal-agent mounts this for the mTLS tunnel. Only present when the control
	// plane issued a cert (mTLS configured).
	if tok.Cert != "" {
		sp = ui.Start("Storing the mTLS identity as a Kubernetes Secret")
		if out, err := runPipedCaptured(
			[]string{
				"kubectl", "create", "secret", "generic", "origamy-byod-identity",
				"--namespace", namespace,
				"--from-literal=tls.crt=" + tok.Cert,
				"--from-literal=tls.key=" + string(keyPEM),
				"--from-literal=ca.crt=" + tok.CAChain,
				"--dry-run=client", "-o", "yaml",
			},
			[]string{"kubectl", "apply", "-f", "-"},
		); err != nil {
			sp.Fail("Could not store the mTLS identity")
			return diagnose(out)
		}
		sp.Success("mTLS identity stored securely")
	}

	// External ClickHouse: store the password in a Secret (piped via stdin, so
	// it never appears in shell history) and reference it from the chart — NOT
	// via --set, which would persist the password in helm release history.
	if chMode == 2 {
		sp = ui.Start("Storing ClickHouse password as a Kubernetes Secret")
		if out, err := runPipedCaptured(
			[]string{
				"kubectl", "create", "secret", "generic", "origamy-clickhouse",
				"--namespace", namespace,
				"--from-literal=clickhouse-password=" + chPassword,
				"--dry-run=client", "-o", "yaml",
			},
			[]string{"kubectl", "apply", "-f", "-"},
		); err != nil {
			sp.Fail("Could not store ClickHouse password")
			return diagnose(out)
		}
		sp.Success("ClickHouse password stored securely")
	}

	helmArgs := []string{
		"upgrade", "--install", release, helmChart,
		"--namespace", namespace,
		"--version", chartVer,
		"--set", "controlPlane.url=" + tok.Addr,
		"--set", "controlPlane.httpUrl=" + tok.URL,
		"--set", "controlPlane.dataPlaneId=" + tok.ID,
		"--set", "portalAgent.enabled=true",
		"--set", "portalAgent.existingSecret=origamy-byod-token",
		"--set", "portalAgent.existingSecretAuthKey=auth-token",
		"--set", "preset=" + selected.name,
	}
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
	if datastoreAuth {
		helmArgs = append(helmArgs, "--set", "datastores.auth.enabled=true")
	}
	// AI engine: the chart auto-generates the engine's KEK + API token when this
	// flips true, so we pass only the boolean — never a secret (disable is unused
	// at install; that path lives in `origamy upgrade`).
	aiArgs, _ := aiToggleArgs(aiEnabled, false)
	helmArgs = append(helmArgs, aiArgs...)
	if chMode == 1 {
		helmArgs = append(helmArgs, "--set", "clickhouse.enabled=true")
	} else {
		helmArgs = append(helmArgs,
			"--set", "clickhouse.enabled=false",
			"--set", "clickhouse.host="+chHost,
			// Password sourced from the origamy-clickhouse Secret created above,
			// never --set (which would leak it into helm release history).
			"--set", "clickhouse.existingSecret=origamy-clickhouse",
		)
	}
	switch exposeMode {
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
			"--set", "ingestGateway.ingress.host="+ingressHost,
			"--set", "ingestGateway.ingress.className="+ingressClass,
		)
	}

	sp = ui.Start("Installing data plane (%s) via Helm", selected.label)
	if out, err := runCaptured("helm", helmArgs...); err != nil {
		sp.Fail("Helm install failed")
		return diagnose(out)
	}
	sp.Success("Data plane installed (%s)", selected.label)

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

	// — Resolve the SDK event endpoint ————————————————————————————————————
	var eventURL, eventHint string
	switch exposeMode {
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
		eventURL = "https://" + ingressHost
	default: // Internal
		eventHint = "kubectl port-forward -n " + namespace + " svc/" + release + "-ingestion-gateway " + fmt.Sprintf("%d:%d", gatewayAPIPort, gatewayAPIPort)
	}

	// — Summary ———————————————————————————————————————————————————————————
	lines := []string{
		ui.Gray("Data plane  ") + ui.Bold(tok.ID),
		ui.Gray("Size        ") + selected.label,
		ui.Gray("Namespace   ") + namespace,
	}
	if aiEnabled {
		lines = append(lines, ui.Gray("AI engine   ")+ui.Green("enabled"))
	}
	if datastoreAuth {
		lines = append(lines, ui.Gray("Datastores  ")+"password-protected")
	}
	if eventURL != "" {
		lines = append(lines, ui.Gray("Send events ")+ui.Bold(eventURL+"/v1/identify"))
	}
	if allReady {
		lines = append(lines, "", ui.Green("Your dashboard will show it as Connected shortly."))
	} else {
		lines = append(lines,
			"",
			ui.Gray("Check progress:"),
			"  kubectl get pods -n "+namespace,
		)
	}
	if eventURL != "" {
		lines = append(lines, "", ui.Gray("Paste the event URL into your source's Setup tab in the dashboard."))
	} else if eventHint != "" {
		lines = append(lines, "", ui.Gray("Get your event endpoint:"), "  "+eventHint)
	}
	if !aiEnabled {
		lines = append(lines, "", ui.Gray("Add the AI engine later:"), "  origamy upgrade --enable-ai")
	}
	ui.Box("Deployed", lines)
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
		out, err := runCaptured("kubectl", "get", "svc", svc, "-n", ns,
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
	out, err := runCaptured("kubectl", "get", "pods", "-n", ns, "-o", "json")
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

func deployDocker(tok *token.Enrollment, keyPEM []byte, enableAI, aiSet bool) error {
	ui.Title("Target")
	ui.Success("Docker detected")

	dockerPresets := presets[:2] // Starter and Standard only for Docker
	ui.Title("Deployment size")
	for i, p := range dockerPresets {
		fmt.Printf("  %s  %s  %s\n", ui.Cyan(fmt.Sprintf("%d", i+1)), ui.Bold(fmt.Sprintf("%-11s", p.label)), ui.Gray(p.description))
	}
	tierIdx := promptChoice("Choose 1-2", 1, len(dockerPresets), 1)
	selected := dockerPresets[tierIdx-1]

	// — Engagement services ——————————————————————————————————————————————
	// Journeys, broadcasts and human tasks run in workflow-engine, which needs
	// the bundled Postgres (compose profile "full"). Default on, matching the
	// Kubernetes install where workflowEngine.enabled is true.
	ui.Title("Engagement services")
	fullProfile := promptYesNo("Enable journeys, broadcasts and human tasks? Adds Postgres + workflow-engine.", true)

	// — AI engine ————————————————————————————————————————————————————————
	// Same contract as Kubernetes: the CLI controls engine presence (compose
	// profile "agentic"), the control plane controls use. The KEK and internal
	// API token are generated on this host and never leave it.
	aiEnabled := enableAI
	if !aiSet {
		ui.Title("AI engine")
		aiEnabled = promptYesNo("Enable the Origamy AI engine? Adds the orchestrator engine; requires the AI package in your dashboard.", false)
	}

	// — Event endpoint ————————————————————————————————————————————————————
	// The gateway listens on :8081 over plain HTTP. With a domain, the bundled
	// Caddy (profile "ingress") terminates HTTPS with a Let's Encrypt cert.
	ui.Title("Event endpoint")
	ui.Step("Plain HTTP on :8081 by default. Give it a domain to serve HTTPS via the bundled Caddy")
	ui.Step("(needs ports 80/443 reachable and the domain's DNS A record pointing at this host).")
	ingestDomain := promptString("Event domain (e.g. events.mycompany.com) [none]")

	ui.Title("Provisioning")
	dir := "origamy-dp-" + tok.ID
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail("Could not create the working directory.", err.Error())
	}
	if err := os.Chdir(dir); err != nil {
		return fail("Could not enter the working directory.", err.Error())
	}
	ui.Success("Working directory ./%s", dir)

	// The bundle (compose + ClickHouse schema/users config, and the Caddyfile
	// for HTTPS ingress) is served by the control plane at /byod/*.
	files := []string{"docker-compose.yml", "clickhouse-init.sql", "clickhouse-users.xml"}
	if ingestDomain != "" {
		files = append(files, "Caddyfile")
	}
	sp := ui.Start("Downloading the deploy bundle")
	for _, f := range files {
		if err := fetchBundleFile(tok.URL, f); err != nil {
			sp.Fail("Could not download %s", f)
			if errors.Is(err, errNotServed) {
				return fail(fmt.Sprintf("Your control plane does not serve %s.", f),
					"It predates this CLI's deploy bundle — upgrade the control plane, or deploy with an older CLI.")
			}
			return diagnose(err.Error())
		}
	}
	sp.Success("Bundle downloaded (%s)", strings.Join(files, ", "))

	// Secrets are generated HERE, in the customer's environment, and written to
	// the local .env — they never reach Origamy. Reuse any already in .env from
	// a prior deploy so a re-deploy doesn't rotate them out from under the
	// running datastores (which hold data). The AI secrets are carried forward
	// even when AI is off: the KEK must survive a disable/enable cycle or the
	// per-workspace LLM keys encrypted under it become unreadable.
	orchKEK := readEnvVar(".env", "ORCH_KEK")
	orchToken := readEnvVar(".env", "ORCH_ENGINE_API_TOKEN")
	if aiEnabled {
		orchKEK = existingOrRandomKEK(".env", "ORCH_KEK")
		orchToken = existingOrRandom(".env", "ORCH_ENGINE_API_TOKEN")
	}
	var profiles []string
	if fullProfile {
		profiles = append(profiles, "full")
	}
	if aiEnabled {
		profiles = append(profiles, "agentic")
	}
	if ingestDomain != "" {
		profiles = append(profiles, "ingress")
	}
	env := renderDockerEnv(dockerEnvParams{
		TunnelAddr:   tok.Addr,
		HTTPURL:      tok.URL,
		DataPlaneID:  tok.ID,
		AuthToken:    tok.Tok,
		ImageTag:     helmVersion,
		Preset:       selected.name,
		Profiles:     profiles,
		IngestDomain: ingestDomain,
		RedisPw:      existingOrRandom(".env", "DP_REDIS_PASSWORD"),
		NatsPw:       existingOrRandom(".env", "NATS_PASSWORD"),
		ClickHousePw: existingOrRandom(".env", "CLICKHOUSE_PASSWORD"),
		DBPw:         existingOrRandom(".env", "DB_PASSWORD"),
		OrchKEK:      orchKEK,
		OrchToken:    orchToken,
		AIEnabled:    aiEnabled,
		MTLS:         tok.Cert != "",
	})
	if err := os.WriteFile(".env", []byte(env), 0o600); err != nil {
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
		}
		ui.Success("Wrote mTLS identity (certs/tls.crt, tls.key, ca.crt)")
	}

	ui.Title("Bringing services online")
	sp = ui.Start("Starting services (%s)", selected.label)
	// Profiles come from COMPOSE_PROFILES in .env, so every later compose
	// invocation (upgrade/status) sees the same service set.
	if out, err := runCaptured("docker", "compose", "--env-file", ".env", "up", "-d"); err != nil {
		sp.Fail("docker compose failed")
		return diagnose(out)
	}
	sp.Success("Services started")

	lines := []string{
		ui.Gray("Data plane  ") + ui.Bold(tok.ID),
		ui.Gray("Location    ") + "./" + dir,
		ui.Gray("Release     ") + helmVersion,
	}
	if fullProfile {
		lines = append(lines, ui.Gray("Engagement  ")+ui.Green("enabled"))
	}
	if aiEnabled {
		lines = append(lines, ui.Gray("AI engine   ")+ui.Green("enabled"))
	}
	if ingestDomain != "" {
		lines = append(lines,
			ui.Gray("Send events ")+ui.Bold("https://"+ingestDomain+"/v1/identify"),
			"",
			ui.Gray("Point "+ingestDomain+"'s DNS A record at this host; Caddy provisions the certificate on first request."))
	} else {
		lines = append(lines,
			ui.Gray("Send events ")+ui.Bold("http://<this host>:"+fmt.Sprintf("%d", gatewayAPIPort)+"/v1/identify"),
			"",
			ui.Gray("Plain HTTP — for production SDK traffic set INGEST_DOMAIN in .env and add \"ingress\" to COMPOSE_PROFILES."))
	}
	lines = append(lines,
		"",
		ui.Green("Your dashboard will show it as Connected shortly."),
		ui.Gray("Logs: ")+"docker compose -f ./"+dir+"/docker-compose.yml logs -f portal-agent")
	if !aiEnabled {
		lines = append(lines, "", ui.Gray("Add the AI engine later:"), "  origamy upgrade --enable-ai")
	}
	ui.Box("Deployed", lines)
	return nil
}

// ── Prompt helpers ────────────────────────────────────────────────────────────

func promptChoice(label string, min, max, defaultVal int) int {
	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("\n  %s %s ", label, ui.Gray(fmt.Sprintf("[%d]", defaultVal)))
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return defaultVal
		}
		var n int
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= min && n <= max {
			return n
		}
		ui.Warn("Please enter a number between %d and %d.", min, max)
	}
}

func promptString(label string) string {
	r := bufio.NewReader(os.Stdin)
	fmt.Printf("  %s: ", label)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

// promptYesNo asks a yes/no question, returning defaultYes on an empty answer.
func promptYesNo(label string, defaultYes bool) bool {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("\n  %s %s ", label, ui.Gray(suffix))
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return defaultYes
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		ui.Warn("Please answer y or n.")
	}
}

// ── error helpers ─────────────────────────────────────────────────────────────

// fail builds a styled, actionable error to return from a command.
func fail(headline, hint string) error {
	fmt.Println()
	ui.Fail(headline)
	if hint != "" {
		ui.Detail("%s", hint)
	}
	fmt.Println()
	return errSilent
}

// diagnose inspects captured command output, prints the diagnosis, and returns
// a silent error so cobra doesn't re-print a raw message.
func diagnose(output string) error {
	d := ui.DiagnoseHelm(output)
	fmt.Println()
	ui.Fail(d.Headline)
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

// runPipedCaptured pipes cmd1 → cmd2 and returns cmd2's combined output.
func runPipedCaptured(cmd1, cmd2 []string) (string, error) {
	var buf bytes.Buffer
	c1 := exec.Command(cmd1[0], cmd1[1:]...)
	c2 := exec.Command(cmd2[0], cmd2[1:]...)
	c2.Stdout = &buf
	c2.Stderr = &buf
	p, err := c1.StdoutPipe()
	if err != nil {
		return "", err
	}
	c2.Stdin = p
	if err := c1.Start(); err != nil {
		return "", err
	}
	if err := c2.Start(); err != nil {
		return "", err
	}
	_ = c1.Wait()
	err = c2.Wait()
	return buf.String(), err
}
