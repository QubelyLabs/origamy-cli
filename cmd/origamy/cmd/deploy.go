package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	helmChart   = "oci://ghcr.io/qubelylabs/charts/origamy-data-plane"
	helmVersion = "0.1.10"
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

Get your enrollment token from the Connections page in your Origamy dashboard.`,
	Example: `  origamy deploy --token dpe_xxx`,
	RunE: func(cmd *cobra.Command, args []string) error {
		t, _ := cmd.Flags().GetString("token")
		return runDeploy(t)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	deployCmd.Flags().StringP("token", "t", "", "Enrollment token from your Origamy dashboard (required)")
	_ = deployCmd.MarkFlagRequired("token")
}

func runDeploy(raw string) error {
	tok, err := token.Decode(raw)
	if err != nil {
		return fail("Invalid enrollment token.",
			"Get a fresh token from the Connections page in your dashboard — tokens expire after 72 hours.")
	}

	if tok.Exp > 0 && time.Now().Unix() > tok.Exp {
		return fail("Your enrollment token has expired.",
			"Generate a new one from the Connections page in your dashboard.")
	}

	ui.Title("Origamy data plane")
	ui.KV("Data plane", ui.Bold(tok.ID))
	ui.KV("Control", tok.Addr)

	switch {
	case hasKubernetes():
		return deployKubernetes(tok)
	case hasDocker():
		return deployDocker(tok)
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

func deployKubernetes(tok *token.Enrollment) error {
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

	helmArgs := []string{
		"upgrade", "--install", release, helmChart,
		"--namespace", namespace,
		"--version", helmVersion,
		"--set", "controlPlane.url=" + tok.Addr,
		"--set", "controlPlane.httpUrl=" + tok.URL,
		"--set", "controlPlane.dataPlaneId=" + tok.ID,
		"--set", "portalAgent.enabled=true",
		"--set", "portalAgent.existingSecret=origamy-byod-token",
		"--set", "portalAgent.existingSecretAuthKey=auth-token",
		"--set", "preset=" + selected.name,
	}
	if chMode == 1 {
		helmArgs = append(helmArgs, "--set", "clickhouse.enabled=true")
	} else {
		helmArgs = append(helmArgs,
			"--set", "clickhouse.enabled=false",
			"--set", "clickhouse.host="+chHost,
			"--set", "clickhouse.password="+chPassword,
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

	// — Summary ———————————————————————————————————————————————————————————
	lines := []string{
		ui.Gray("Data plane  ") + ui.Bold(tok.ID),
		ui.Gray("Size        ") + selected.label,
		ui.Gray("Namespace   ") + namespace,
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
	ui.Box("Deployed", lines)
	return nil
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

func deployDocker(tok *token.Enrollment) error {
	ui.Title("Target")
	ui.Success("Docker detected")

	dockerPresets := presets[:2] // Starter and Standard only for Docker
	ui.Title("Deployment size")
	for i, p := range dockerPresets {
		fmt.Printf("  %s  %s  %s\n", ui.Cyan(fmt.Sprintf("%d", i+1)), ui.Bold(fmt.Sprintf("%-11s", p.label)), ui.Gray(p.description))
	}
	tierIdx := promptChoice("Choose 1-2", 1, len(dockerPresets), 1)
	selected := dockerPresets[tierIdx-1]

	ui.Title("Provisioning")
	dir := "origamy-dp-" + tok.ID
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail("Could not create the working directory.", err.Error())
	}
	if err := os.Chdir(dir); err != nil {
		return fail("Could not enter the working directory.", err.Error())
	}
	ui.Success("Working directory ./%s", dir)

	sp := ui.Start("Downloading compose file and ClickHouse schema")
	if out, err := runCaptured("curl", "-fsSL", tok.URL+"/byod/docker-compose.yml", "-o", "docker-compose.yml"); err != nil {
		sp.Fail("Download failed")
		return diagnose(out)
	}
	if out, err := runCaptured("curl", "-fsSL", tok.URL+"/byod/clickhouse-init.sql", "-o", "clickhouse-init.sql"); err != nil {
		sp.Fail("Download failed")
		return diagnose(out)
	}
	sp.Success("Compose file and schema downloaded")

	env := fmt.Sprintf(
		"CONTROL_PLANE_ADDR=%s\nCONFIG_URL=%s\nDATA_PLANE_ID=%s\nAUTH_TOKEN=%s\nTLS_ENABLED=true\nDP_IMAGE_TAG=main\nLOG_LEVEL=info\nDEPLOYMENT_PRESET=%s\n",
		tok.Addr, tok.URL, tok.ID, tok.Tok, selected.name,
	)
	if err := os.WriteFile(".env", []byte(env), 0o600); err != nil {
		return fail("Could not write .env.", err.Error())
	}
	ui.Success("Wrote .env")

	ui.Title("Bringing services online")
	sp = ui.Start("Starting services (%s)", selected.label)
	if out, err := runCaptured("docker", "compose", "--env-file", ".env", "up", "-d"); err != nil {
		sp.Fail("docker compose failed")
		return diagnose(out)
	}
	sp.Success("Services started")

	ui.Box("Deployed", []string{
		ui.Gray("Data plane  ") + ui.Bold(tok.ID),
		ui.Gray("Location    ") + "./" + dir,
		"",
		ui.Green("Your dashboard will show it as Connected shortly."),
		ui.Gray("Logs: ") + "docker compose -f ./" + dir + "/docker-compose.yml logs -f portal-agent",
	})
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
