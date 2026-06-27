package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/qubelylabs/origamy-cli/internal/token"
)

const (
	helmChart   = "oci://ghcr.io/qubelylabs/charts/origamy-data-plane"
	helmVersion = "0.1.1"
	namespace   = "origamy-dp"
	release     = "odp"
)

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
}

func init() {
	deployCmd.Flags().StringP("token", "t", "", "Enrollment token from your Origamy dashboard (required)")
	_ = deployCmd.MarkFlagRequired("token")
}

func runDeploy(raw string) error {
	tok, err := token.Decode(raw)
	if err != nil {
		return fmt.Errorf("invalid enrollment token: %w\n\nGet a fresh token from your dashboard — tokens expire after 72 hours", err)
	}

	if tok.Exp > 0 && time.Now().Unix() > tok.Exp {
		return fmt.Errorf("enrollment token has expired — generate a new one from the Connections page in your dashboard")
	}

	step("Data plane  %s", tok.ID)
	step("Control     %s", tok.Addr)
	fmt.Println()

	switch {
	case hasKubernetes():
		return deployKubernetes(tok)
	case hasDocker():
		return deployDocker(tok)
	default:
		return fmt.Errorf(
			"no Kubernetes cluster (kubectl) or Docker found\n\n" +
				"Install options:\n" +
				"  Docker:     https://docs.docker.com/get-docker/\n" +
				"  Kubernetes: configure kubectl to point at your cluster, then retry",
		)
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
		return fmt.Errorf(
			"helm is required for Kubernetes installs\n" +
				"Install it from https://helm.sh/docs/intro/install/ and retry",
		)
	}

	step("Kubernetes cluster detected — installing via Helm")
	fmt.Println()

	step("Creating namespace %s...", namespace)
	_ = runPiped(
		[]string{"kubectl", "create", "namespace", namespace, "--dry-run=client", "-o", "yaml"},
		[]string{"kubectl", "apply", "-f", "-"},
	)

	step("Storing auth token as Kubernetes Secret...")
	_ = runPiped(
		[]string{
			"kubectl", "create", "secret", "generic", "origamy-byod-token",
			"--namespace", namespace,
			"--from-literal=auth-token=" + tok.Tok,
			"--dry-run=client", "-o", "yaml",
		},
		[]string{"kubectl", "apply", "-f", "-"},
	)

	step("Installing data plane via Helm...")
	if err := runVisible("helm", "upgrade", "--install", release, helmChart,
		"--namespace", namespace,
		"--version", helmVersion,
		"--set", "controlPlane.url="+tok.Addr,
		"--set", "controlPlane.dataPlaneId="+tok.ID,
		"--set", "portalAgent.enabled=true",
		"--set", "portalAgent.existingSecret=origamy-byod-token",
		"--set", "portalAgent.existingSecretAuthKey=auth-token",
	); err != nil {
		return fmt.Errorf("helm upgrade failed: %w", err)
	}

	fmt.Println()
	step("Waiting for pods...")
	_ = runVisible("kubectl", "rollout", "status", "deployment", "-n", namespace, "--timeout=180s")

	fmt.Println()
	done("Data plane %s deployed.", tok.ID)
	done("Your dashboard will show it as Connected shortly.")
	return nil
}

// ── Docker ────────────────────────────────────────────────────────────────────

func hasDocker() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return runQuiet("docker", "info") == nil
}

func deployDocker(tok *token.Enrollment) error {
	step("Docker detected — installing via Docker Compose")
	fmt.Println()

	dir := "origamy-dp-" + tok.ID
	step("Creating directory ./%s...", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("failed to enter directory: %w", err)
	}

	step("Downloading compose file and ClickHouse schema...")
	if err := curlTo(tok.URL+"/byod/docker-compose.yml", "docker-compose.yml"); err != nil {
		return err
	}
	if err := curlTo(tok.URL+"/byod/clickhouse-init.sql", "clickhouse-init.sql"); err != nil {
		return err
	}

	step("Writing .env...")
	env := fmt.Sprintf(
		"CONTROL_PLANE_ADDR=%s\nCONFIG_URL=%s\nDATA_PLANE_ID=%s\nAUTH_TOKEN=%s\nTLS_ENABLED=true\nDP_IMAGE_TAG=main\nLOG_LEVEL=info\n",
		tok.Addr, tok.URL, tok.ID, tok.Tok,
	)
	if err := os.WriteFile(".env", []byte(env), 0600); err != nil {
		return fmt.Errorf("failed to write .env: %w", err)
	}

	step("Starting services...")
	if err := runVisible("docker", "compose", "--env-file", ".env", "up", "-d"); err != nil {
		return fmt.Errorf("docker compose failed: %w", err)
	}

	fmt.Println()
	done("Data plane %s started in ./%s/", tok.ID, dir)
	done("Your dashboard will show it as Connected shortly.")
	return nil
}

// ── Shell helpers ─────────────────────────────────────────────────────────────

func runVisible(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func runPiped(cmd1, cmd2 []string) error {
	c1 := exec.Command(cmd1[0], cmd1[1:]...)
	c2 := exec.Command(cmd2[0], cmd2[1:]...)
	c2.Stdout = os.Stdout
	c2.Stderr = os.Stderr
	p, err := c1.StdoutPipe()
	if err != nil {
		return err
	}
	c2.Stdin = p
	if err := c1.Start(); err != nil {
		return err
	}
	if err := c2.Start(); err != nil {
		return err
	}
	_ = c1.Wait()
	return c2.Wait()
}

func curlTo(url, dest string) error {
	return runVisible("curl", "-fsSL", url, "-o", dest)
}

func step(format string, args ...any) {
	fmt.Printf("> "+format+"\n", args...)
}

func done(format string, args ...any) {
	fmt.Printf("✔ "+format+"\n", args...)
}
