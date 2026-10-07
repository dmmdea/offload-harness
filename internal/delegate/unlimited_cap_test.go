package delegate

// fleet_max_concurrent_jobs < 0 means unlimited, and an unlimited cap is never reached (the diagnosis' F11).
//
// FleetConcurrencyLimit resolves a negative setting to 0, "unlimited". route=auto's busy formula compared
// the local seat's in-flight count against it with `>=` at four sites and guarded the comparison at one,
// so with the cap unlimited `inflight >= 0` read an IDLE seat as permanently busy, and the work left the
// box although the setting says the seat can take any number of runs.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestAtRunCapIsNeverReachedWhenTheCapIsUnlimited is the helper's own truth table.
func TestAtRunCapIsNeverReachedWhenTheCapIsUnlimited(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setting  int
		inflight int
		want     bool
	}{
		{"unlimited, idle", -1, 0, false},
		{"unlimited, forty in flight", -1, 40, false},
		{"unset (the built-in 4), three in flight", 0, 3, false},
		{"unset (the built-in 4), four in flight", 0, 4, true},
		{"unset, none in flight", 0, 0, false},
		{"explicit 2, two in flight", 2, 2, true},
		{"explicit 2, one in flight", 2, 1, false},
	} {
		r := &runner{cfg: config.Config{FleetMaxConcurrentJobs: tc.setting}}
		if got := r.atRunCap(tc.inflight); got != tc.want {
			t.Errorf("%s: atRunCap(%d) = %v, want %v", tc.name, tc.inflight, got, tc.want)
		}
	}
}

// TestAutoKeepsAnIdleLocalSeatWhenTheCapIsUnlimited_PerSubtask: the per-subtask placement (attempt).
func TestAutoKeepsAnIdleLocalSeatWhenTheCapIsUnlimited_PerSubtask(t *testing.T) {
	remote, url := eligibleNode(t, "node-a", "zorblax from the remote")
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = -1
	var localCalls atomic.Int64
	r := &runner{
		cfg: cfg, local: passingLocal(&localCalls), route: "auto", remotes: []string{url},
		intent:         openIntentLedger(cfg),
		localBusyProbe: func(context.Context) busyReading { return busyReading{busy: false, inflight: 0} }, // an idle seat
	}
	r.runOne(context.Background(), 0, remoteContract())
	if localCalls.Load() != 1 || remote.dispatches.Load() != 0 {
		t.Fatalf("local runs %d, remote dispatches %d: an idle local seat with an unlimited run cap was read as busy and the work left the box", localCalls.Load(), remote.dispatches.Load())
	}
	if r.dealReadLocalBusy() {
		t.Fatal("the placement recorded the idle local seat as read busy (the capacity wait would keep every subtask off it)")
	}
}

// TestTheJointDealReadsTheLocalSeatBusyOnlyAtAFiniteCap pins the deal's own recording: the busy flag it
// hands the placement and the "read busy" flag the capacity wait keeps for every subtask it holds.
func TestTheJointDealReadsTheLocalSeatBusyOnlyAtAFiniteCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setting  int
		inflight int
		want     bool
	}{
		{"unlimited, forty in flight", -1, 40, false},
		{"unlimited, idle", -1, 0, false},
		{"built-in cap of 4, four in flight", 0, 4, true},
		{"built-in cap of 4, three in flight", 0, 3, false},
	} {
		cfg := testCfg(t)
		cfg.FleetMaxConcurrentJobs = tc.setting
		inflight := tc.inflight
		r := &runner{cfg: cfg, route: "auto", localBusyProbe: func(context.Context) busyReading { return busyReading{inflight: inflight} }}
		if got := r.readAutoLocalSlot(context.Background(), gpulease.Info{}, []core.AgentContract{remoteContract()}); got != tc.want {
			t.Errorf("%s: the deal's busy flag = %v, want %v", tc.name, got, tc.want)
		}
		if got := r.dealReadLocalBusy(); got != tc.want {
			t.Errorf("%s: the recorded \"read busy\" flag = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAutoKeepsAnIdleLocalSeatWhenTheCapIsUnlimited_JointDeal: the same through the public entry, whose one
// joint deal reads the seat once for every subtask of the run (no endpoint is configured, so the seat reads
// idle).
func TestAutoKeepsAnIdleLocalSeatWhenTheCapIsUnlimited_JointDeal(t *testing.T) {
	remote, url := eligibleNode(t, "node-a", "zorblax from the remote")
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = -1
	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), []core.AgentContract{remoteContract()}, "auto", []string{url}, nil)
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if localCalls.Load() != 1 || remote.dispatches.Load() != 0 {
		t.Fatalf("local runs %d, remote dispatches %d (summary %+v, placement %q): the joint deal read an idle local seat with an unlimited cap as busy", localCalls.Load(), remote.dispatches.Load(), sum, results[0].PlacementReason)
	}
}

// TestAWaitingSubtaskIsNotKeptOffAnIdleSeatByAnUnlimitedCap: the capacity wait's own re-reading of a seat a
// deal kept it off (localStillBusy) shares the guard.
func TestAWaitingSubtaskIsNotKeptOffAnIdleSeatByAnUnlimitedCap(t *testing.T) {
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = -1
	r := &runner{cfg: cfg, route: "auto", localBusyProbe: func(context.Context) busyReading { return busyReading{inflight: 0} }}
	if busy, note := r.localStillBusy(context.Background(), gpulease.Info{}); busy {
		t.Fatalf("an idle local seat with an unlimited cap still reads busy to the wait (%s)", note)
	}
	cfg.FleetMaxConcurrentJobs = 0
	r = &runner{cfg: cfg, route: "auto", localBusyProbe: func(context.Context) busyReading { return busyReading{inflight: 4} }}
	if busy, _ := r.localStillBusy(context.Background(), gpulease.Info{}); !busy {
		t.Fatal("control: a seat at the built-in cap of 4 must still read busy")
	}
}
