package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestNewFleetJobsAppliesTheConfiguredPollLease: fleet_poll_lease_sec has to reach
// the store the node serves from. With the wiring gone the store would keep its
// built-in 60 s and an operator's setting — or the -1 that turns the abandoned-job
// rules off — would be silently ignored, with every other test green.
func TestNewFleetJobsAppliesTheConfiguredPollLease(t *testing.T) {
	for _, tc := range []struct {
		name string
		sec  int
		want time.Duration
	}{
		{"unset takes the default", 0, 60 * time.Second},
		{"an operator's value", 90, 90 * time.Second},
		{"a value under the floor is raised to it", 3, config.FleetPollLeaseSecFloor * time.Second},
		{"negative turns the rules off", -1, 0},
	} {
		jobs := newFleetJobs(config.Config{FleetPollLeaseSec: tc.sec})
		if got := jobs.PollLease(); got != tc.want {
			t.Errorf("%s: fleet_poll_lease_sec=%d gave a store with lease %v, want %v", tc.name, tc.sec, got, tc.want)
		}
		jobs.DrainAndStop(time.Second)
	}
	jobs := newFleetJobs(config.Config{FleetMaxConcurrentJobs: 2})
	defer jobs.DrainAndStop(time.Second)
	if got := jobs.MaxConcurrent(); got != 2 {
		t.Errorf("fleet_max_concurrent_jobs=2 gave a store with %d slots", got)
	}
}

// TestPollLeaseNoteSaysWhichLeaseIsInForce: the lease a node runs under is not always
// the one written. 0 takes the default, a positive value under the floor is raised to
// it, a negative one turns the abandoned-job rules off — and a substituted value with
// no runtime signal is a setting the operator believes in and is not running. The
// serve verb prints this note once at start-up.
func TestPollLeaseNoteSaysWhichLeaseIsInForce(t *testing.T) {
	for _, tc := range []struct {
		name string
		sec  int
		want []string // every fragment must appear
	}{
		{"unset takes the default", 0, []string{"poll lease 60 s", "default"}},
		{"an operator's value", 90, []string{"poll lease 90 s", "fleet_poll_lease_sec=90"}},
		{"a value under the floor says it was raised", 3, []string{"poll lease 15 s", "fleet_poll_lease_sec=3", "raised", "15 s floor"}},
		{"negative says the rules are off", -1, []string{"poll lease OFF", "fleet_poll_lease_sec=-1"}},
	} {
		got := pollLeaseNote(config.Config{FleetPollLeaseSec: tc.sec})
		for _, frag := range tc.want {
			if !strings.Contains(got, frag) {
				t.Errorf("%s: note %q lacks %q", tc.name, got, frag)
			}
		}
	}
}

// TestServeBuildsItsJobStoreThroughNewFleetJobs pins the call site: the serve verb
// is never run by a test, so the seam above proves nothing unless the verb goes
// through it. A store built directly, beside it, would have no lease wiring at all.
func TestServeBuildsItsJobStoreThroughNewFleetJobs(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(text, "jobs := newFleetJobs(cfg)") {
		t.Error("main.go's fleet-serve no longer builds its job store with newFleetJobs(cfg)")
	}
	if strings.Contains(text, "fleetnode.NewJobs(") {
		t.Error("main.go builds a job store with fleetnode.NewJobs directly: it would bypass the poll-lease wiring in newFleetJobs")
	}
	if !strings.Contains(text, "pollLeaseNote(cfg)") {
		t.Error("main.go's fleet-serve no longer prints the poll-lease note: a lease raised to the floor, or turned off, would carry no runtime signal")
	}
}
