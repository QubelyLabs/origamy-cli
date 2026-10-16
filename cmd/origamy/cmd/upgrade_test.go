package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClickHouse stands in for dockerRun in the Docker storage reset: the
// ping fails until it has been tried failPings times (forever when < 0).
func fakeClickHouse(t *testing.T, failPings int) *[]string {
	t.Helper()
	var calls []string
	prevRun, prevTimeout, prevPoll := dockerRun, clickhouseReadyTimeout, clickhouseReadyPoll
	clickhouseReadyTimeout, clickhouseReadyPoll = 200*time.Millisecond, time.Millisecond
	pings := 0
	dockerRun = func(env []string, name string, args ...string) (string, error) {
		calls = append(calls, strings.Join(append(append(env, name), args...), " "))
		if strings.Contains(args[len(args)-1], "SELECT 1") {
			if pings++; failPings < 0 || pings <= failPings {
				return "Code: 210. DB::NetException: Connection refused (localhost:9000)", errors.New("exit status 210")
			}
		}
		return "", nil
	}
	t.Cleanup(func() { dockerRun, clickhouseReadyTimeout, clickhouseReadyPoll = prevRun, prevTimeout, prevPoll })
	return &calls
}

func TestResetDockerEventsStartsClickHouseFirst(t *testing.T) {
	calls := fakeClickHouse(t, 2)
	if err := resetDockerEvents("/dp", "/dp/.env", "0.1.20"); err != nil {
		t.Fatal(err)
	}
	// ClickHouse comes up on the compose file's image (with the target tag)
	// and answers BEFORE the old table is dropped and the schema applied, so
	// both run on the new server rather than the container still running.
	dc := "DP_IMAGE_TAG=0.1.20 docker compose --project-directory /dp --env-file /dp/.env "
	want := []string{
		dc + "up -d clickhouse",
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhousePing,
		dc + "exec -T clickhouse sh -c " + clickhouseDropEvents,
		dc + "run --rm --no-deps clickhouse-init",
	}
	if strings.Join(*calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(*calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestResetDockerEventsClickHouseNeverReady(t *testing.T) {
	calls := fakeClickHouse(t, -1)
	if err := resetDockerEvents("/dp", "/dp/.env", "0.1.20"); err == nil {
		t.Fatal("a ClickHouse that never answers must fail the reset")
	}
	for _, c := range *calls {
		if strings.Contains(c, "DROP TABLE") || strings.Contains(c, "clickhouse-init") {
			t.Fatalf("nothing may be dropped or applied before ClickHouse answers: %q", c)
		}
	}
}
