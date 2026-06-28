package ui

import "strings"

// Diagnosis is a human-readable explanation of a failure plus a concrete hint
// on how to resolve it.
type Diagnosis struct {
	Headline string // what went wrong, in plain language
	Hint     string // what to do about it
}

// DiagnoseHelm inspects captured helm/kubectl output and returns a friendly
// diagnosis. It pattern-matches the failure modes we actually hit in the field
// so the user sees guidance instead of a raw stack trace.
func DiagnoseHelm(output string) Diagnosis {
	o := strings.ToLower(output)
	switch {
	case contains(o, "another operation", "in progress"),
		contains(o, "cannot re-use a name that is still in use"):
		return Diagnosis{
			"A previous install is still in progress or stuck.",
			"Wait a minute and retry. If it persists: helm uninstall odp -n origamy-dp, then deploy again.",
		}
	case contains(o, "context deadline exceeded"), contains(o, "timed out waiting"):
		return Diagnosis{
			"The install timed out before every pod was ready.",
			"This is usually fine — pods keep starting. Check: kubectl get pods -n origamy-dp",
		}
	case contains(o, "401 unauthorized"), contains(o, "manifest unknown"),
		contains(o, "imagepullbackoff"), contains(o, "errimagepull"),
		contains(o, "pull access denied"):
		return Diagnosis{
			"A container image could not be pulled.",
			"The image may be private or missing. Verify the data-plane images are published and public.",
		}
	case contains(o, "insufficient cpu"), contains(o, "insufficient memory"),
		contains(o, "failedscheduling"), contains(o, "too many pods"):
		return Diagnosis{
			"The cluster doesn't have enough resources to schedule the data plane.",
			"Free up capacity or add a node, then retry. A Starter deployment needs ~4 GB RAM free.",
		}
	case contains(o, "kubernetes cluster unreachable"), contains(o, "connection refused"),
		contains(o, "no such host"), contains(o, "could not connect to the server"):
		return Diagnosis{
			"Could not reach your Kubernetes cluster.",
			"Check that kubectl points at the right cluster: kubectl cluster-info",
		}
	case contains(o, "is forbidden"), contains(o, "forbidden:"):
		return Diagnosis{
			"Your kubectl identity lacks permission to install into this namespace.",
			"Use a context with namespace-admin rights, or ask your cluster admin.",
		}
	case contains(o, "no matches for kind"), contains(o, "could not find the requested resource"):
		return Diagnosis{
			"Your cluster is missing an API your install needs.",
			"Check the Kubernetes version (the chart targets a recent cluster).",
		}
	default:
		return Diagnosis{
			"The install command failed.",
			"See the output above. Re-run with the same token to retry.",
		}
	}
}

// DiagnosePod maps a pod's waiting/phase reason to a one-line hint.
func DiagnosePod(reason string) string {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
		return "image can't be pulled — verify it's published and public"
	case "CrashLoopBackOff":
		return "starting then exiting — often a not-yet-ready dependency; usually self-recovers"
	case "CreateContainerConfigError":
		return "a referenced Secret/ConfigMap is missing"
	case "CreateContainerError", "RunContainerError":
		return "the container failed to start — check its logs"
	case "Pending":
		return "no node has room — the cluster may be out of CPU/memory"
	case "ContainerCreating", "PodInitializing":
		return "waiting on image/volume — usually transient"
	default:
		if reason != "" {
			return strings.ToLower(reason)
		}
		return "not ready yet"
	}
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
