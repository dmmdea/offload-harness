// pin_fallback_test.go: where a remote hint may fall back to the local seat, and where it may not (R4e review: F3 and the
// low findings; ADR 0078 decision 5).
//
// A remote hint is placed fleet-first, and the seat is its fallback where a remote pin would have deferred. The fallback has
// guards (never over a lease, an occupant, a full line or a load in progress), several places it can fire (the deal, the
// re-placement after a refusal, the capacity wait, the case where no remote could ever run the contract), and narration
// that has to stay true when the box has no seat or the call is cut by its deadline. Each is pinned here.

package delegate

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fullNode is a node that is full in both senses the engine reads: no headroom for the deal, and a queue at its depth for
// the wait. The deal therefore finds no remote with room, and the hint's only way onto the seat is the fallback.
func fullNode(f *fakeNode) {
	f.maxConcurrentJobs, f.jobsRunning, f.maxQueueDepth, f.queueDepth, f.jobsQueued = 1, 1, 2, 2, 1
}

// TestAReasonlessRemoteRouteNeverRunsOnAnOccupiedAFullOrALoadingSeat is the fallback's guard (hintSeatFree), which no test
// pinned: the only remote-hint test whose seat was not free held a text lease. With the wait ON, a full node, and a seat
// that another vLLM seat occupies, that has its run-cap line full by what llama-swap reports in flight, or that is
// loading, the subtask waits in line and ends as a capacity defer that says no node took it. The control is the same call
// against an idle seat, which runs it. A refactor that computed the guard from the lease alone would hand the seat the
// subtask, load it over the operator's session, and leave every other test green.
func TestAReasonlessRemoteRouteNeverRunsOnAnOccupiedAFullOrALoadingSeat(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	for _, tc := range []struct {
		name       string
		cfg        func(t *testing.T) config.Config
		wantSeat   bool
		wantReason string
	}{
		{"the seat is idle", func(t *testing.T) config.Config {
			cfg := pinCfg(t)
			cfg.Endpoint = stateSwap(t, "local-seat", true, false, 0)
			return cfg
		}, true, ""},
		{"another vLLM seat occupies it", func(t *testing.T) config.Config {
			cfg := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
			cfg.FleetMaxConcurrentJobs = 4
			return cfg
		}, false, "opencode-seat"},
		{"its run-cap line is full: 4 in flight at a cap of 4", func(t *testing.T) config.Config {
			cfg := pinCfg(t)
			cfg.Endpoint = busySwap(t, "local-seat", 4)
			return cfg
		}, false, ""},
		{"a load is in progress", func(t *testing.T) config.Config {
			cfg := pinCfg(t)
			cfg.Endpoint = stateSwap(t, "local-seat", true, true, 0)
			return cfg
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, url := acceptingNode(t, "node-a", "zorblax from A", fullNode)
			cfg := tc.cfg(t)
			cfg.AgentPlacementWaitSec = 1 // the wait is on: a seat that is not free is a place in line, then a defer
			var localCalls atomic.Int64
			results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(1), "remote", []string{url}, doorOpts(""))
			if err != nil {
				t.Fatal(err)
			}
			pr := results[0]
			if tc.wantSeat {
				if sum.Succeeded != 1 || localCalls.Load() != 1 || node.dispatches.Load() != 0 || !strings.HasPrefix(pr.PlacementReason, "route=remote was a hint (no pin_reason), overridden: placed on the local seat") {
					t.Fatalf("summary %+v, local runs %d, node dispatches %d, placement %q, want the idle seat to take what the full node cannot", sum, localCalls.Load(), node.dispatches.Load(), pr.PlacementReason)
				}
				return
			}
			if sum.Deferred != 1 || localCalls.Load() != 0 || node.dispatches.Load() != 0 || pr.ranLocal || pr.Result.DeferClass != core.DeferClassCapacity {
				t.Fatalf("summary %+v, local runs %d, node dispatches %d, ran local %v, class %q: the seat was not free, so the subtask waits in line and defers for capacity",
					sum, localCalls.Load(), node.dispatches.Load(), pr.ranLocal, pr.Result.DeferClass)
			}
			if want := "route=remote was a hint (no pin_reason): placed remotes-first, and no node took it"; !strings.HasPrefix(pr.PlacementReason, want) {
				t.Errorf("placement = %q, want it to open with %q", pr.PlacementReason, want)
			}
			if tc.wantReason != "" && !strings.Contains(pr.Result.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to name %q", pr.Result.Reason, tc.wantReason)
			}
		})
	}
}

// TestARefusedDispatchOfARemoteHintFallsBackToTheIdleSeatWhereAPinFails: ADR 0078 names "a dispatch is refused: the
// re-placement falls back to the seat as it does for auto" as one of the four places a remote hint falls back, and no test
// drove it (the one refusal test excludes the seat by construction). The only node answers 503. A hint is placed as auto, so
// replacementNode's last resort is open and the idle seat runs the subtask; a remote PIN keeps replacementNode's
// "route=remote never falls back to local" and fails the subtask with the seat untouched.
func TestARefusedDispatchOfARemoteHintFallsBackToTheIdleSeatWhereAPinFails(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := refusingNode(t, "node-a", http.StatusServiceUnavailable, nil)
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(1), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Succeeded != 1 || localCalls.Load() != 1 || node.dispatches.Load() != 1 || !pr.ranLocal || pr.Replacements != 1 {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, ran local %v, replacements %d, want one refused dispatch and the idle seat", sum, localCalls.Load(), node.dispatches.Load(), pr.ranLocal, pr.Replacements)
	}
	for _, want := range []string{"route=remote was a hint (no pin_reason), overridden: placed on the local seat", "re-placed on the local seat after 1 refusal(s)"} {
		if !strings.Contains(pr.PlacementReason, want) {
			t.Errorf("placement = %q, want it to contain %q", pr.PlacementReason, want)
		}
	}
	if st := tally.Snapshot(); st.Hints != 1 || st.HintsOverridden != 1 {
		t.Errorf("tally %+v, want 1 hint, overridden", st)
	}

	pinned, psum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, doorOpts(PinOperator))
	if err != nil {
		t.Fatal(err)
	}
	if psum.Failed != 1 || pinned[0].ranLocal || !strings.Contains(pinned[0].Err, "route=remote never falls back to local") {
		t.Fatalf("pinned: summary %+v, ran local %v, err %q, want the failure a remote pin always got", psum, pinned[0].ranLocal, pinned[0].Err)
	}
}

// TestARemoteHintWithNoEligibleRemoteQueuesOnAFullOrLoadingSeatButWaitsForAnOccupant is the decision ADR 0078 decision 5 had
// wrong: with NO remote able to run the contract at all, the seat's own run-cap line is the only queue there is (ADR
// 0076; replacementNode and awaitCapacity say the same), so a full or loading seat takes the subtask at once, behind its
// in-flight work, exactly as route=auto does ("queued-local beats ineligible-remote"). Only what loading the seat would
// destroy holds it back: a vLLM seat the operator has loaded (operator, 2026-10-06: wait in line) or a lease. The wait is ON
// so that "waits" means waiting, and the auto column shows the hint falling back where auto would.
func TestARemoteHintWithNoEligibleRemoteQueuesOnAFullOrLoadingSeatButWaitsForAnOccupant(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	for _, tc := range []struct {
		name     string
		cfg      func(t *testing.T) config.Config
		wantSeat bool
	}{
		{"its run-cap line is full: 4 in flight at a cap of 4", func(t *testing.T) config.Config {
			cfg := pinCfg(t)
			cfg.Endpoint = busySwap(t, "local-seat", 4)
			return cfg
		}, true},
		{"a load is in progress", func(t *testing.T) config.Config {
			cfg := pinCfg(t)
			cfg.Endpoint = stateSwap(t, "local-seat", true, true, 0)
			return cfg
		}, true},
		{"another vLLM seat occupies it", func(t *testing.T) config.Config {
			cfg := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
			cfg.FleetMaxConcurrentJobs = 4
			return cfg
		}, false},
	} {
		for _, kind := range []string{"a remote hint", "route auto"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				_, url := acceptingNode(t, "node-off", "never", func(f *fakeNode) { f.agentEnabled = false })
				cfg := tc.cfg(t)
				cfg.AgentPlacementWaitSec = 1
				route, opts := "remote", doorOpts("")
				if kind == "route auto" {
					route, opts = "auto", nil
				}
				var localCalls atomic.Int64
				results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(1), route, []string{url}, opts)
				if err != nil {
					t.Fatal(err)
				}
				pr := results[0]
				if tc.wantSeat {
					if sum.Succeeded != 1 || localCalls.Load() != 1 || !pr.ranLocal || !strings.Contains(pr.PlacementReason, "no eligible remote") {
						t.Fatalf("summary %+v, local runs %d, placement %q, want the subtask taken into the seat's own line at once", sum, localCalls.Load(), pr.PlacementReason)
					}
					if kind == "a remote hint" && !strings.Contains(pr.PlacementReason, "a remote hint falls back to the local seat") {
						t.Errorf("placement = %q, want it to say a remote hint fell back to the seat", pr.PlacementReason)
					}
					return
				}
				if sum.Deferred != 1 || localCalls.Load() != 0 || pr.ranLocal || pr.Result.DeferClass != core.DeferClassCapacity {
					t.Fatalf("summary %+v, local runs %d, ran local %v, class %q, want the subtask to wait for the occupant to leave and defer for capacity", sum, localCalls.Load(), pr.ranLocal, pr.Result.DeferClass)
				}
			})
		}
	}
}

// TestARemoteHintOnABoxWithNoAgentSeatGetsTheFleetsVerdictAndNoOverride: a delegation client has no agent seat, so the seat
// is no fallback for its remote hint. With no remote able to run the contract the hint ends as the same defer a remote pin
// gets, carrying the fleet's verdict, and not as a run on a seat that does not exist (the local runner answering "no agent
// seat resolvable"), which would narrate a placement on the local seat and count the hint as overridden.
func TestARemoteHintOnABoxWithNoAgentSeatGetsTheFleetsVerdictAndNoOverride(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := acceptingNode(t, "node-off", "never", func(f *fakeNode) { f.agentEnabled = false })
	cfg := seatlessCfg(t)
	opts, tally := countedOpts("")
	hinted, hsum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	pinned, psum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, doorOpts(PinOperator))
	if err != nil {
		t.Fatal(err)
	}
	h, p := hinted[0], pinned[0]
	if hsum.Deferred != 1 || psum.Deferred != 1 || !h.Unplaced || h.ranLocal {
		t.Fatalf("hinted: summary %+v, unplaced %v, ran local %v, want the fleet's defer and no run on a seat that does not exist", hsum, h.Unplaced, h.ranLocal)
	}
	if !strings.HasPrefix(h.Result.Reason, "route=remote: ") || strings.Contains(h.Result.Reason, "no agent seat resolvable") {
		t.Errorf("reason = %q, want the fleet's verdict (route=remote: ...) and not the local runner's config error", h.Result.Reason)
	}
	if h.Result.Reason != p.Result.Reason || h.Result.DeferClass != p.Result.DeferClass {
		t.Errorf("hint reason %q class %q, pin reason %q class %q, want the same defer", h.Result.Reason, h.Result.DeferClass, p.Result.Reason, p.Result.DeferClass)
	}
	if want := "route=remote was a hint (no pin_reason): placed remotes-first, and no node took it"; !strings.HasPrefix(h.PlacementReason, want) {
		t.Errorf("placement = %q, want it to open with %q", h.PlacementReason, want)
	}
	if st := tally.Snapshot(); st.Hints != 1 || st.HintsOverridden != 0 {
		t.Errorf("tally %+v, want 1 hint and no override: nothing was placed", st)
	}
}

// TestAnAbandonedHintedSubtaskDoesNotClaimNoNodeTookIt: the call's deadline gave up on a subtask whose seat is deaf to its
// context, so the subtask is published as unplaced with no node or seat, and the call cannot say where it was running. The
// hint clause must not assert that no node took it (a seat is still running it); it says the placement is not known, and the
// tally counts a hint and no override. A subtask that never started keeps "no node took it", which is true of it.
func TestAnAbandonedHintedSubtaskDoesNotClaimNoNodeTookIt(t *testing.T) {
	for _, tc := range []struct {
		name, route, want string
	}{
		{"a local hint", "local", "route=local was a hint (no pin_reason): placed as route=auto, and where it was running is not known"},
		{"a remote hint with no eligible remote, taken by the seat", "remote", "route=remote was a hint (no pin_reason): placed remotes-first, and where it was running is not known"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pinCfg(t)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(func() {
				unblock()
				// the stuck goroutine records its own row when it finally returns; let it finish before the temp dir goes
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if rows, err := readFinished(cfg.LedgerPath); err == nil && len(rows) >= 2 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				time.Sleep(100 * time.Millisecond)
			})
			var started atomic.Int64
			local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
				started.Add(1)
				<-release // deaf to ctx
				return localOK(), nil
			}
			var remotes []string
			if tc.route == "remote" {
				_, url := acceptingNode(t, "node-off", "never", func(f *fakeNode) { f.agentEnabled = false })
				remotes = []string{url}
			}
			opts, tally := countedOpts("")
			opts.Deadline = time.Now().Add(300 * time.Millisecond)
			results, _, _ := runWithin(t, 6*time.Second, cfg, local, []core.AgentContract{{Goal: "slow one"}}, tc.route, remotes, opts, unblock)
			pr := results[0]
			if started.Load() != 1 || !pr.Unplaced || !pr.abandoned {
				t.Fatalf("fixture: seat runs %d, unplaced %v, abandoned %v, want a subtask the call gave up on while the seat was still running it", started.Load(), pr.Unplaced, pr.abandoned)
			}
			if !strings.HasPrefix(pr.PlacementReason, tc.want) {
				t.Errorf("placement = %q, want it to open with %q", pr.PlacementReason, tc.want)
			}
			if strings.Contains(pr.PlacementReason, "no node took it") {
				t.Errorf("placement = %q claims no node took a subtask a seat is still running", pr.PlacementReason)
			}
			if st := tally.Snapshot(); st.Hints != 1 || st.HintsOverridden != 0 {
				t.Errorf("tally %+v, want 1 hint and no override", st)
			}
		})
	}

	// the control: a subtask the call never started was placed nowhere, and says so
	r := &runner{pin: pinCall{asked: "local", hint: true}}
	if got := r.stampPin(PlacedResult{Unplaced: true, PlacementReason: "not started: the call ended first"}).PlacementReason; !strings.HasPrefix(got, "route=local was a hint (no pin_reason): placed as route=auto, and no node took it") {
		t.Errorf("an unstarted subtask's placement = %q, want it to keep %q", got, "no node took it")
	}
}

// TestTheCorpusRowOfAHintedSubtaskCarriesTheHintClause: record() writes the stamped placement to the delegation log as it
// does to the ledger, and the stamp's comment says it covers the corpus row; until now nothing read the corpus.
func TestTheCorpusRowOfAHintedSubtaskCarriesTheHintClause(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	_, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(4, 0))
	cfg := pinCfg(t)
	var localCalls atomic.Int64
	results, _, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), pages(5), "local", []string{url}, doorOpts(""))
	if err != nil {
		t.Fatal(err)
	}
	byJob := map[string]delegationLogLine{}
	for _, row := range corpusLines(t, cfg) {
		byJob[row.JobID] = row
	}
	if len(byJob) != len(results) {
		t.Fatalf("the corpus holds %d rows for %d subtasks", len(byJob), len(results))
	}
	for i, pr := range results {
		want := "route=local was a hint (no pin_reason), honoured: placed on the local seat"
		if i >= 4 {
			want = "route=local was a hint (no pin_reason), overridden: placed on node-a"
		}
		if got := byJob[pr.JobID].PlacementReason; !strings.HasPrefix(got, want) {
			t.Errorf("subtask %d: corpus placement_reason = %q, want it to open with %q", i, got, want)
		}
	}
}
