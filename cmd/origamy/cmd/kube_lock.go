package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ── The lease ────────────────────────────────────────────────────────────────
//
// Two origamy runs must not work on the events table at once: one's RENAME
// would race the other's check. A run that can touch the events table
// (kube_move.go) holds a lease for as long as it works: the ConfigMap
// kubeLeaseConfigMap, created if absent (kubectl create fails when it
// exists, so one run wins) with who holds it and since when. A lease older
// than kubeLeaseStale is left by a run that died without giving it up, and
// is taken over: replaced in the one step that also checks nobody else
// replaced it first (its resourceVersion). Every other upgrade, rollback and
// deploy takes none.

const kubeLeaseConfigMap = "origamy-reset-lock"

// kubeLeaseStale is how long a lease holds before it counts as left behind: a
// reset waits up to 5 minutes at each of several steps.
var kubeLeaseStale = 45 * time.Minute

// kubeLeaseHeld means another run holds the lease.
type kubeLeaseHeld struct {
	holder, command string
	since           time.Time
	// unreadable: the lease could not be read to tell how old it is.
	unreadable string
}

func (e *kubeLeaseHeld) Error() string {
	if e.unreadable != "" {
		return "the lease " + kubeLeaseConfigMap + " exists and could not be read: " + e.unreadable
	}
	return fmt.Sprintf("%q holds the lease %s since %s (%s ago)", e.holder, kubeLeaseConfigMap, e.since.Format(time.RFC3339), time.Since(e.since).Round(time.Second))
}

// kubeLease is the lease ConfigMap as it is in the cluster.
type kubeLease struct {
	present         bool
	holder, command string
	acquired        time.Time
	resourceVersion string
}

func readKubeLease() (kubeLease, error) {
	out, err := kubeRunTimeout(kubeRequestTimeout, "kubectl", "get", "configmap", kubeLeaseConfigMap, "-n", namespace, "--ignore-not-found", "-o", "json")
	if err != nil {
		return kubeLease{}, fmt.Errorf("kubectl get configmap %s: %s", kubeLeaseConfigMap, orDash(lastLines(out, 1)))
	}
	if strings.TrimSpace(out) == "" {
		return kubeLease{}, nil
	}
	var cm kubeObject
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return kubeLease{}, fmt.Errorf("could not read the ConfigMap %s: %w", kubeLeaseConfigMap, err)
	}
	l := kubeLease{present: true, holder: cm.Data["holder"], command: cm.Data["command"], resourceVersion: cm.Metadata.ResourceVersion}
	l.acquired, _ = time.Parse(time.RFC3339, cm.Data["acquired"])
	return l, nil
}

// newLeaseHolder names this run: host, process and a random part.
func newLeaseHolder() string {
	host, _ := os.Hostname()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b))
}

// acquireKubeLease takes the lease for command ("upgrade", "rollback",
// "deploy"). giveUp gives it up (once; only if it is still this run's). The
// error is a *kubeLeaseHeld when another run holds it.
func acquireKubeLease(command string) (giveUp func(), err error) {
	holder := newLeaseHolder()
	data := func() map[string]string {
		return map[string]string{"holder": holder, "command": command, "acquired": time.Now().UTC().Format(time.RFC3339)}
	}
	giveUp = func() {
		l, err := readKubeLease()
		if err != nil || !l.present || l.holder != holder {
			return
		}
		_, _ = kubeRunTimeout(kubeRequestTimeout, "kubectl", "delete", "configmap", kubeLeaseConfigMap, "-n", namespace, "--ignore-not-found")
	}
	for attempt := 0; attempt < 3; attempt++ {
		out, err := kubeRunStdin(kubeRequestTimeout, configMapManifest(namespace, kubeLeaseConfigMap, data()), "kubectl", "create", "-f", "-")
		if err == nil {
			return giveUp, nil
		}
		if !strings.Contains(strings.ToLower(out), "already exists") && !strings.Contains(strings.ToLower(out), "alreadyexists") {
			return nil, fmt.Errorf("kubectl create: %s", orDash(lastLines(out, 2)))
		}
		cur, err := readKubeLease()
		if err != nil {
			return nil, &kubeLeaseHeld{unreadable: err.Error()}
		}
		if !cur.present {
			continue // given up between the two calls: take it
		}
		if !cur.acquired.IsZero() && time.Since(cur.acquired) < kubeLeaseStale {
			return nil, &kubeLeaseHeld{holder: cur.holder, command: cur.command, since: cur.acquired}
		}
		// Left behind: replace it, unless another run already did.
		out, err = kubeRunStdin(kubeRequestTimeout, configMapManifestAt(namespace, kubeLeaseConfigMap, data(), cur.resourceVersion), "kubectl", "replace", "-f", "-")
		if err == nil {
			return giveUp, nil
		}
		if low := strings.ToLower(out); !strings.Contains(low, "conflict") && !strings.Contains(low, "the object has been modified") && !strings.Contains(low, "not found") {
			return nil, fmt.Errorf("kubectl replace: %s", orDash(lastLines(out, 2)))
		}
	}
	return nil, &kubeLeaseHeld{unreadable: "it keeps changing hands"}
}

// leaseProblem says why the lease could not be taken (headline and hint);
// nothing says what has not been changed.
func leaseProblem(err error, nothing string) (headline, hint string) {
	if held, ok := err.(*kubeLeaseHeld); ok {
		if held.unreadable != "" {
			return "Could not tell whether another origamy run holds the lease that keeps two runs off the events table.",
				held.Error() + "\n" + nothing
		}
		return "Another origamy run is working on this data plane's events table (" + held.Error() + ").",
			"Wait for it to finish, then rerun. If it died, the lease is taken over once it is older than " + kubeLeaseStale.String() + ", or delete it now: kubectl delete configmap " + kubeLeaseConfigMap + " -n " + namespace + ". " + nothing
	}
	return "Could not take the lease that keeps two origamy runs off the events table.",
		err.Error() + "\nA run that can touch the events table needs to create a ConfigMap in " + namespace + " (create on configmaps). " + nothing
}

// leaseFail fails a run that could not take the lease (nothing has been
// changed).
func leaseFail(err error) error {
	return fail(leaseProblem(err, "Nothing has been changed."))
}
