package cmd

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// Kubernetes objects the CLI creates itself are rendered here and handed to
// `kubectl apply -f -` on stdin. Secrets in particular must never travel as
// `--from-literal=…` arguments: a command line is readable by every local
// user through ps and /proc/*/cmdline for as long as kubectl runs, and execve
// audit rules keep it for good.

// kubectlApply applies one manifest from stdin and returns kubectl's output.
func kubectlApply(manifest string) (string, error) {
	return runWithStdin(manifest, "kubectl", "apply", "-f", "-")
}

// namespaceManifest renders a Namespace.
func namespaceManifest(ns string) string {
	return "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns + "\n"
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
