package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeProbe answers locateInstall's questions: kubectl's current context
// (ctx, "" = none), cluster-info (clusterOut/clusterErr) and helm status
// (helmOut/helmErr), with Docker up or not and a compose project here or not.
type fakeProbe struct {
	ctx                 string
	clusterOut, helmOut string
	clusterErr, helmErr bool
	docker, project     bool
}

func (p fakeProbe) install(t *testing.T) {
	t.Helper()
	// probeKube looks kubectl up on PATH before asking it anything.
	bin := t.TempDir()
	name := "kubectl"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	prevRun, prevDocker := kubeRunTimeout, dockerAvailable
	kubeRunTimeout = func(_ time.Duration, name string, args ...string) (string, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		switch {
		case strings.HasPrefix(cmd, "kubectl config current-context"):
			if p.ctx == "" {
				return "error: current-context is not set", errors.New("exit status 1")
			}
			return p.ctx + "\n", nil
		case strings.HasPrefix(cmd, "kubectl cluster-info"):
			if p.clusterErr {
				return p.clusterOut, errors.New("exit status 1")
			}
			return "Kubernetes control plane is running", nil
		case strings.HasPrefix(cmd, "helm status"):
			if p.helmErr {
				return p.helmOut, errors.New("exit status 1")
			}
			return "STATUS: deployed", nil
		}
		t.Errorf("unexpected command %s", cmd)
		return "", errors.New("unexpected")
	}
	dockerAvailable = func() bool { return p.docker }
	t.Cleanup(func() { kubeRunTimeout, dockerAvailable = prevRun, prevDocker })
	dir := t.TempDir()
	if p.project {
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DATA_PLANE_ID=dp-1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

func TestLocateInstall(t *testing.T) {
	const expired = "E1023 00:59:39 memcache.go:265] couldn't get current server API group list\n" +
		"error: You must be logged in to the server (the server has asked for the client to provide credentials)"
	notFound := "Error: release: not found"
	for name, tc := range map[string]struct {
		probe    fakeProbe
		want     installKind
		wantErr  bool
		headline string
	}{
		"release on the cluster":              {probe: fakeProbe{ctx: "eks", docker: true}, want: installKubernetes},
		"release wins over a compose project": {probe: fakeProbe{ctx: "eks", docker: true, project: true}, want: installKubernetes},
		"no kube context, docker":             {probe: fakeProbe{docker: true}, want: installDocker},
		"no kube context, no docker":          {probe: fakeProbe{}, want: installNone},
		"cluster unreachable, project here":   {probe: fakeProbe{ctx: "eks", clusterErr: true, clusterOut: expired, docker: true, project: true}, want: installDocker},
		"cluster unreachable, no project":     {probe: fakeProbe{ctx: "eks", clusterErr: true, clusterOut: expired, docker: true}, wantErr: true, headline: `Could not reach your Kubernetes cluster (kubectl context "eks").`},
		"cluster unreachable, no docker":      {probe: fakeProbe{ctx: "eks", clusterErr: true, clusterOut: expired}, wantErr: true, headline: "Could not reach your Kubernetes cluster"},
		"no release, project here":            {probe: fakeProbe{ctx: "eks", helmErr: true, helmOut: notFound, docker: true, project: true}, want: installDocker},
		"no release, no project":              {probe: fakeProbe{ctx: "eks", helmErr: true, helmOut: notFound, docker: true}, wantErr: true, headline: "No Origamy data plane found."},
		"no release, no docker":               {probe: fakeProbe{ctx: "eks", helmErr: true, helmOut: notFound}, want: installNone},
		"helm cannot tell, no project":        {probe: fakeProbe{ctx: "eks", helmErr: true, helmOut: "Error: Kubernetes cluster unreachable", docker: true}, wantErr: true, headline: "Could not tell whether the cluster"},
		"helm cannot tell, project here":      {probe: fakeProbe{ctx: "eks", helmErr: true, helmOut: "Error: Kubernetes cluster unreachable", docker: true, project: true}, want: installDocker},
	} {
		t.Run(name, func(t *testing.T) {
			tc.probe.install(t)
			var kind installKind
			var err error
			out := captureStdout(t, func() { kind, err = locateInstall("origamy upgrade") })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v\n%s", err, tc.wantErr, out)
			}
			if kind != tc.want {
				t.Errorf("kind = %v, want %v", kind, tc.want)
			}
			if tc.headline != "" && !strings.Contains(out, tc.headline) {
				t.Errorf("output lacks %q:\n%s", tc.headline, out)
			}
			if strings.Contains(out, "compose project found") {
				t.Errorf("output blames a missing compose project:\n%s", out)
			}
		})
	}
}

func TestLocateInstallUnreachableShowsWhy(t *testing.T) {
	fakeProbe{ctx: "arn:aws:eks:us-east-1:1:cluster/dp", clusterErr: true, docker: true,
		clusterOut: "E1023 00:59:39.087266   10554 memcache.go:265] \"Unhandled Error\" err=\"couldn't get current server API group list\"\n\n" +
			"To further debug and diagnose cluster problems, use 'kubectl cluster-info dump'.\n" +
			"error: You must be logged in to the server (Unauthorized)"}.install(t)
	out := captureStdout(t, func() { _, _ = locateInstall("origamy status") })
	for _, noise := range []string{"memcache.go", "cluster-info dump"} {
		if strings.Contains(out, noise) {
			t.Errorf("output keeps kubectl's %q:\n%s", noise, out)
		}
	}
	for _, want := range []string{"You must be logged in to the server", "rerun `origamy status`", "./origamy-dp-<id>"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A verify key given with --set is the release's signing from this run on:
// the summary says so instead of "not set up", and the key stored at deploy
// is not wired in over it.
func TestUpgradeKubernetesSigningFromSet(t *testing.T) {
	f := newFakeKube(t, "0.1.23", "JSON")
	f.signingSecret = true
	out, err := upgradeK8s(t, "0.1.23", "controlPlane.publicKey=dGVzdC1rZXk=")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if strings.Contains(out, "Config signing is not set up") {
		t.Errorf("summary says signing is not set up although --set gave a key:\n%s", out)
	}
	if !strings.Contains(out, "signature-verified") {
		t.Errorf("summary lacks the signing line:\n%s", out)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, helmUpgrade) && strings.Contains(c, "controlPlane.existingSecret") {
			t.Errorf("stored key wired in over the one given with --set: %s", c)
		}
	}
}

// So is the stored key's Secret named with --set.
func TestUpgradeKubernetesSigningSecretFromSet(t *testing.T) {
	newFakeKube(t, "0.1.23", "JSON")
	out, err := upgradeK8s(t, "0.1.23", "controlPlane.existingSecret="+configSigningSecret)
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if strings.Contains(out, "Config signing is not set up") || !strings.Contains(out, "signature-verified") {
		t.Errorf("summary does not count the Secret given with --set:\n%s", out)
	}
}

// Without a key the summary still points at how to set one.
func TestUpgradeKubernetesSigningNoteWithoutKey(t *testing.T) {
	newFakeKube(t, "0.1.22", "JSON")
	out, err := upgradeK8s(t, "0.1.23")
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Config signing is not set up") {
		t.Errorf("summary lacks the not-set-up note:\n%s", out)
	}
}
