package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseComposePS(t *testing.T) {
	// Compose ≥ 2.21: one object per line.
	ndjson := `{"Service":"nats","State":"running","Health":"healthy","ExitCode":0}
{"Service":"nats-init","State":"exited","Health":"","ExitCode":0}
`
	got, err := parseComposePS(ndjson)
	if err != nil || len(got) != 2 || got[0].Service != "nats" || got[1].State != "exited" {
		t.Fatalf("ndjson: got %+v, err %v", got, err)
	}
	// Compose < 2.21: a JSON array.
	arr := `[{"Service":"portal-agent","State":"running","Health":"","ExitCode":0}]`
	got, err = parseComposePS(arr)
	if err != nil || len(got) != 1 || got[0].Service != "portal-agent" {
		t.Fatalf("array: got %+v, err %v", got, err)
	}
	if got, err := parseComposePS("  \n"); err != nil || got != nil {
		t.Fatalf("empty output must be (nil, nil), got %+v, %v", got, err)
	}
	if _, err := parseComposePS("{not json"); err == nil {
		t.Fatal("garbage must error")
	}
}

func TestClassifyCompose(t *testing.T) {
	svcs := []composeService{
		{Service: "nats", State: "running", Health: "healthy"},
		{Service: "portal-agent", State: "running"},                   // no healthcheck → ready
		{Service: "clickhouse", State: "running", Health: "starting"}, // still probing
		{Service: "nats-init", State: "exited", ExitCode: 0},          // one-shot, done
		{Service: "clickhouse-init", State: "exited", ExitCode: 1},    // one-shot that failed
		{Service: "bulker-worker", State: "exited", ExitCode: 2},      // exec format error, bad token, …
		{Service: "workflow-engine", State: "restarting"},
		{Service: "caddy", State: "created"},
		{Service: "redis", State: "running", Health: "unhealthy"},
	}
	pending, failed := classifyCompose(svcs)
	if want := []string{"caddy", "clickhouse"}; !reflect.DeepEqual(pending, want) {
		t.Fatalf("pending = %v, want %v", pending, want)
	}
	if want := []string{"bulker-worker", "clickhouse-init", "redis", "workflow-engine"}; !reflect.DeepEqual(failed, want) {
		t.Fatalf("failed = %v, want %v", failed, want)
	}
	// Everything up: nothing pending, nothing failed.
	pending, failed = classifyCompose([]composeService{
		{Service: "nats", State: "running", Health: "healthy"},
		{Service: "nats-init", State: "exited", ExitCode: 0},
	})
	if len(pending) != 0 || len(failed) != 0 {
		t.Fatalf("healthy stack reported pending=%v failed=%v", pending, failed)
	}
}

// chdir moves into dir for the test and restores the cwd afterwards.
func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

func writeProject(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := ""
	if id != "" {
		env = "DATA_PLANE_ID=" + id + "\nCOMPOSE_PROFILES=full\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestComposeProjectDir(t *testing.T) {
	root := t.TempDir()
	chdir(t, root)

	// Nothing here.
	if _, err := composeProjectDir(); err != errNoProject {
		t.Fatalf("empty dir: want errNoProject, got %v", err)
	}

	// A docker-compose.yml that is NOT ours (no DATA_PLANE_ID) must be ignored,
	// not adopted — `origamy upgrade` would otherwise rewrite its .env.
	writeProject(t, root, "")
	if _, err := composeProjectDir(); err != errNoProject {
		t.Fatalf("foreign compose project: want errNoProject, got %v", err)
	}
	if id := existingProjectID("."); id != "" {
		t.Fatalf("foreign project must have no id, got %q", id)
	}

	// Exactly one ./origamy-dp-<id>/ below the cwd is found.
	writeProject(t, filepath.Join(root, "origamy-dp-a"), "dp-a")
	dir, err := composeProjectDir()
	if err != nil || filepath.Base(dir) != "origamy-dp-a" {
		t.Fatalf("single project: got %q, %v", dir, err)
	}

	// Two candidates: refuse to guess, name both.
	writeProject(t, filepath.Join(root, "origamy-dp-b"), "dp-b")
	if _, err := composeProjectDir(); err == nil || !strings.Contains(err.Error(), "origamy-dp-a") || !strings.Contains(err.Error(), "origamy-dp-b") {
		t.Fatalf("two projects: want an error naming both, got %v", err)
	}

	// Inside a project, the cwd wins regardless of siblings.
	chdir(t, filepath.Join(root, "origamy-dp-b"))
	dir, err = composeProjectDir()
	if err != nil || filepath.Base(dir) != "origamy-dp-b" {
		t.Fatalf("inside project: got %q, %v", dir, err)
	}
	if id := existingProjectID("."); id != "dp-b" {
		t.Fatalf("existingProjectID = %q, want dp-b", id)
	}
}

func TestHasProfile(t *testing.T) {
	if !hasProfile([]string{"full", "agentic"}, "agentic") || hasProfile([]string{"full"}, "agentic") || hasProfile(nil, "full") {
		t.Fatal("hasProfile wrong")
	}
}
