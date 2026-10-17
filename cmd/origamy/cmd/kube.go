package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Kubernetes objects the CLI creates itself are rendered here and handed to
// `kubectl apply -f -` on stdin. Secrets in particular must never travel as
// `--from-literal=…` arguments: a command line is readable by every local
// user through ps and /proc/*/cmdline for as long as kubectl runs, and execve
// audit rules keep it for good.

// kubeRun runs a helm or kubectl command and returns its combined output;
// kubeRunTimeout, one that must finish within a time; kubeRunStdin, one fed
// input on stdin, within a time (0: no limit). Variables so tests can stand
// in for the cluster.
var (
	kubeRun        = runCaptured
	kubeRunTimeout = func(d time.Duration, name string, args ...string) (string, error) {
		return runCapturedEnvTimeout(d, nil, name, args...)
	}
	kubeRunStdin = runWithStdinTimeout
)

// runWithStdinTimeout is runWithStdin that gives up after d (0: never): the
// command is killed and an error returned with whatever it printed so far.
func runWithStdinTimeout(d time.Duration, input, name string, args ...string) (string, error) {
	if d <= 0 {
		return runWithStdin(input, name, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var buf bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stdin = strings.NewReader(input)
	c.Stdout = &buf
	c.Stderr = &buf
	c.WaitDelay = 2 * time.Second
	err := c.Run()
	if ctx.Err() != nil {
		return buf.String(), fmt.Errorf("no answer within %s", d)
	}
	return buf.String(), err
}

// kubectlApply applies one manifest from stdin and returns kubectl's output.
func kubectlApply(manifest string) (string, error) {
	return kubeRunStdin(0, manifest, "kubectl", "apply", "-f", "-")
}

// namespaceManifest renders a Namespace.
func namespaceManifest(ns string) string {
	return "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns + "\n"
}

// configMapManifest renders a ConfigMap with the values under `data` (keys
// sorted), each a double-quoted YAML string.
func configMapManifest(ns, name string, data map[string]string) string {
	return configMapManifestAt(ns, name, data, "")
}

// configMapManifestAt is configMapManifest for `kubectl replace` of the
// ConfigMap as it was at resourceVersion: the replace fails when it has
// changed since.
func configMapManifestAt(ns, name string, data map[string]string, resourceVersion string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n  namespace: %s\n", name, ns)
	if resourceVersion != "" {
		fmt.Fprintf(&b, "  resourceVersion: %q\n", resourceVersion)
	}
	b.WriteString("  labels:\n    app.kubernetes.io/managed-by: origamy-cli\ndata:\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, data[k])
	}
	return b.String()
}

// secretManifest renders an Opaque Secret with every value base64-encoded
// under `data` (keys sorted, so the output is stable and testable).
func secretManifest(ns, name string, data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Secret\ntype: Opaque\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n  namespace: %s\ndata:\n", name, ns)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(data[k])))
	}
	return b.String()
}
