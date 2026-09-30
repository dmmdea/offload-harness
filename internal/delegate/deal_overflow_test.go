// deal_overflow_test.go: the subtask a capacity-aware deal hands to the capacity
// wait (ADR 0063, decision 6) - every remote with room was already dealt to its
// headroom, and the local seat was kept out of the rotation because it read busy.
//
// That hand-off is a sentinel, and it is neither of the two things the wait already
// knew: not a lease (none is held, so nothing may claim one cleared, and with the
// wait off the outcome may not be a lease-holder defer naming a lease nobody holds)
// and not a refusal (no node was asked, so no replacement is counted). The wait
// holds the subtask for the first node that frees - the local seat too, but only once
// it stops reading busy by the same reading the deal used, or the deal's whole point
// ("instead of stacking on the busy seat") is undone at the first tick.

package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// busySwapLive is busySwap with an in-flight count the test can change while the
// run is going: the local seat reads busy now and idle a moment later.
func busySwapLive(t *testing.T, seat string, inflight func() int) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"running":[{"model":"` + seat + `","state":"ready","proxy":"http://` + r.Host + `/direct/` + seat + `"}]}`))
	})
	mux.HandleFunc("/direct/"+seat+"/metrics", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:num_requests_running{engine=\"0\"} " + strconv.Itoa(inflight()) + "\nvllm:num_requests_waiting{engine=\"0\"} 0\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// overflowNodes starts two nodes that each admit ONE job at a time and hold it for
// `jobPolls` polls (5 ms apiece) before answering; the slot frees on that answer.
func overflowNodes(t *testing.T, jobPolls int64) (urls []string) {
	t.Helper()
	for _, id := range []string{"node-a", "node-b"} {
		var open atomic.Int64
		var polled sync.Map
		_, url := acceptingNode(t, id, "answer from "+id, func(f *fakeNode) {
			f.maxConcurrentJobs, f.maxQueueDepth = 1, 1
			f.jobsRunningFn = func() int { return int(open.Load()) }
			f.queueDepthFn = func() int { return int(open.Load()) }
			f.onDispatch = func(string, core.AgentContract) { open.Add(1) }
			inner := f.pollByJob
			f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
				cnt, _ := polled.LoadOrStore(jobID, new(atomic.Int64))
				if cnt.(*atomic.Int64).Add(1) < jobPolls {
					return map[string]any{"state": "running"}, http.StatusOK
				}
				if _, seen := polled.LoadOrStore("done-"+jobID, true); !seen {
					open.Add(-1)
				}
				return inner(jobID, n)
			}
		})
		urls = append(urls, url)
	}
	return urls
}

// overflowRun runs three subtasks at two one-slot nodes while the local seat reads
// `inflight` requests in flight: the third is the deal's overflow.
func overflowRun(t *testing.T, route string, inflight func() int, jobPolls int64, waitSec int) (results []PlacedResult, sum Summary, localCalls int64) {
	t.Helper()
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	urls := overflowNodes(t, jobPolls)
	cfg := testCfg(t)
	cfg.Endpoint = busySwapLive(t, "local-seat", inflight)
	cfg.AgentPlacementWaitSec = waitSec
	var calls atomic.Int64
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	results, sum, err := RunWith(ctx, cfg, passingLocal(&calls), []core.AgentContract{plainContract(), plainContract(), plainContract()}, route, urls, nil)
	if err != nil {
		t.Fatal(err)
	}
	return results, sum, calls.Load()
}

func always(n int) func() int { return func() int { return n } }

// TestSpreadOverflowGoesThroughTheCapacityWait is the guard for the hand-off
// itself: deleting the sentinel branch in attempt() left the whole suite green,
// because the deal tests read the deal and the run tests held a text lease.
func TestSpreadOverflowGoesThroughTheCapacityWait(t *testing.T) {
	_, sum, _ := overflowRun(t, "spread", always(2), 60, 10)
	if sum.Succeeded != 3 || sum.Waited < 1 {
		t.Fatalf("summary = %+v, want 3 successes with the overflow subtask having waited in line", sum)
	}
}

// TestSpreadOverflowIsNotStackedOnTheBusyLocalSeat: the deal kept the overflow off
// a busy seat "instead of stacking on the busy seat"; the wait asked only the run
// registry, saw no run on the seat, and ran it there at the first tick while both
// remotes were about to free.
func TestSpreadOverflowIsNotStackedOnTheBusyLocalSeat(t *testing.T) {
	results, sum, localCalls := overflowRun(t, "spread", always(2), 60, 10)
	if localCalls != 0 {
		var why string
		for _, pr := range results {
			if pr.ranLocal {
				why = pr.PlacementReason
			}
		}
		t.Fatalf("the local seat ran %d subtask(s) although it read busy and both remotes were about to free (placement: %q)", localCalls, why)
	}
	if sum.Succeeded != 3 {
		t.Fatalf("summary = %+v, want all 3 to land on the remotes", sum)
	}
	for i, pr := range results {
		if strings.Contains(pr.PlacementReason, "lease cleared") {
			t.Errorf("subtask %d: placement %q claims a lease cleared, but no lease was ever held", i, pr.PlacementReason)
		}
	}
}

// The same rule on route=auto, whose deal raises the same sentinel: the seat reads
// busy at the fleet's own in-flight cap.
func TestAutoOverflowIsNotStackedOnTheBusyLocalSeat(t *testing.T) {
	results, sum, localCalls := overflowRun(t, "auto", always(4), 60, 10)
	if localCalls != 0 || sum.Succeeded != 3 {
		var why string
		for _, pr := range results {
			if pr.ranLocal {
				why = pr.PlacementReason
			}
		}
		t.Fatalf("summary = %+v, local ran %d (placement: %q); want the overflow to wait for a remote while the seat reads at its in-flight cap", sum, localCalls, why)
	}
}

// TestOverflowIsNoRefusalAndKeepsItsStory: nobody refused the overflow subtask, so it
// is not a replacement (Summary.Replaced is the shedding-fleet signal and counts
// subtasks re-placed after a node refused them); but the reason it waited stays on
// the result.
func TestOverflowIsNoRefusalAndKeepsItsStory(t *testing.T) {
	results, sum, _ := overflowRun(t, "spread", always(2), 60, 10)
	if sum.Replaced != 0 || sum.ReplacementRecovered != 0 {
		t.Fatalf("summary = %+v, want no replacement counted: no node refused anything", sum)
	}
	var waited *PlacedResult
	for i := range results {
		if results[i].waited {
			waited = &results[i]
		}
	}
	if waited == nil {
		t.Fatalf("summary = %+v: no subtask waited", sum)
	}
	if waited.Replacements != 0 || waited.ReplacementNote != "" {
		t.Fatalf("replacements = %d note = %q, want none", waited.Replacements, waited.ReplacementNote)
	}
	for _, want := range []string{"capacity wait", "already dealt to its headroom"} {
		if !strings.Contains(waited.PlacementReason, want) {
			t.Errorf("placement reason = %q, want it to keep %q (why the subtask waited)", waited.PlacementReason, want)
		}
	}
}

// TestSpreadOverflowTakesTheLocalSeatOnceItStopsReadingBusy: the other half. The
// remotes stay full for seconds; the local seat reads busy for 300 ms and then
// idle; the overflow subtask must not sit out the whole wait beside an idle seat.
func TestSpreadOverflowTakesTheLocalSeatOnceItStopsReadingBusy(t *testing.T) {
	began := time.Now()
	inflight := func() int {
		if time.Since(began) < 300*time.Millisecond {
			return 2
		}
		return 0
	}
	results, sum, localCalls := overflowRun(t, "spread", inflight, 300, 10)
	if sum.Succeeded != 3 || localCalls != 1 {
		t.Fatalf("summary = %+v local ran %d, want the overflow subtask on the local seat once it went idle", sum, localCalls)
	}
	var local *PlacedResult
	for i := range results {
		if results[i].ranLocal {
			local = &results[i]
		}
	}
	if local == nil || !local.waited || !strings.Contains(local.PlacementReason, "kept this subtask off it") {
		t.Fatalf("local result = %+v, want a waited placement that says the deal kept it off the seat", local)
	}
	if strings.Contains(local.PlacementReason, "lease") {
		t.Fatalf("placement %q names a lease, but none was ever held", local.PlacementReason)
	}
}

// TestSpreadOverflowWithTheWaitOffIsACapacityDeferNotALeaseDefer: with the wait
// switched off the overflow subtask has nowhere to wait. It comes back as a CAPACITY
// defer - never as the holder-naming deferral of a lease nobody holds, which is
// infrastructure class and drives a non-zero exit.
func TestSpreadOverflowWithTheWaitOffIsACapacityDeferNotALeaseDefer(t *testing.T) {
	results, sum, _ := overflowRun(t, "spread", always(2), 60, -1)
	var deferred *PlacedResult
	for i := range results {
		if results[i].Result.Deferred {
			deferred = &results[i]
		}
	}
	if deferred == nil {
		t.Fatalf("summary = %+v, want the overflow subtask deferred (no wait to go to)", sum)
	}
	if deferred.Result.DeferClass != core.DeferClassCapacity || sum.Infrastructure != 0 || strings.Contains(deferred.Result.Reason, "lease") {
		t.Fatalf("class = %q infrastructure = %d reason = %q, want a capacity defer that does not name a lease", deferred.Result.DeferClass, sum.Infrastructure, deferred.Result.Reason)
	}
	if !strings.Contains(deferred.Result.Reason, "already dealt to its headroom") {
		t.Fatalf("reason = %q, want it to say why the subtask had nowhere to go", deferred.Result.Reason)
	}
	if deferred.Replacements != 0 {
		t.Fatalf("replacements = %d, want 0: no node refused it", deferred.Replacements)
	}
}

// TestLocalStillBusyReadsTheDealsOwnPredicate: the wait takes the seat back on the
// same reading the deal kept the subtask off it on - any request in flight for
// route=spread (never, with agent_spread_local_slot: always), and for route=auto a
// held lease, an in-flight count at the fleet's own cap, or a load in progress.
func TestLocalStillBusyReadsTheDealsOwnPredicate(t *testing.T) {
	cases := []struct {
		name  string
		route string
		slot  string
		lease bool
		rd    busyReading
		want  bool
	}{
		{"spread: a request in flight", "spread", "", false, busyReading{busy: true, inflight: 1}, true},
		{"spread: idle", "spread", "", false, busyReading{}, false},
		{"spread: slot=always never reads busy", "spread", config.SpreadLocalAlways, false, busyReading{busy: true, inflight: 3}, false},
		{"auto: in flight under the cap", "auto", "", false, busyReading{busy: true, inflight: 3}, false},
		{"auto: in flight at the cap", "auto", "", false, busyReading{busy: true, inflight: 4}, true},
		{"auto: a load in progress", "auto", "", false, busyReading{busy: true, loading: true}, true},
		{"auto: a held lease", "auto", "", true, busyReading{}, true},
		{"auto: idle", "auto", "", false, busyReading{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{
				cfg:            config.Config{FleetMaxConcurrentJobs: 4, AgentSpreadLocalSlot: tc.slot},
				route:          tc.route,
				localBusyProbe: func(context.Context) busyReading { return tc.rd },
			}
			if got, _ := r.localStillBusy(t.Context(), gpulease.Info{Held: tc.lease}); got != tc.want {
				t.Fatalf("localStillBusy = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBusyReadingIsMemoisedForTheProbeTTL: twelve waiters read the engine once per
// tick between them, not twelve times - and a non-positive TTL (what the compressed
// clocks of the wait tests use) reads every time.
func TestBusyReadingIsMemoisedForTheProbeTTL(t *testing.T) {
	var probes atomic.Int64
	r := &runner{localBusyProbe: func(context.Context) busyReading {
		probes.Add(1)
		return busyReading{busy: true, inflight: 1}
	}}
	old := fetchViewsMemoTTL
	t.Cleanup(func() { fetchViewsMemoTTL = old })

	fetchViewsMemoTTL = time.Minute
	for i := 0; i < 12; i++ {
		r.busyReadingNow(t.Context())
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("12 reads inside the memo window probed the engine %d times, want 1", got)
	}
	fetchViewsMemoTTL = 0
	r2 := &runner{localBusyProbe: r.localBusyProbe}
	r2.busyReadingNow(t.Context())
	r2.busyReadingNow(t.Context())
	if got := probes.Load(); got != 3 {
		t.Fatalf("with the memo off two reads made %d probes in all, want 1 + 2 = 3", got)
	}
}
