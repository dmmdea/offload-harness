// pin_gate_test.go: a reasonless remote hint the process gate turned away takes the idle seat where no wait can carry it
// (R4e review, F2; ADR 0078 decision 5).
//
// The deal gives a remote hint a node because the node's health shows headroom, and never asks the process gate. When this
// process already holds the node's whole admission ceiling open (a second concurrent Run of the same MCP server), the
// dispatch is turned away with nothing sent, and the subtask goes to the capacity wait, which reads the fleet once and
// then takes the idle seat. But two exits leave before any tick: the operator's off switch (agent_placement_wait_sec < 0)
// defers, and a sheddable run (priority -1) is shed, both while the idle seat has room, where a remote PIN would have
// deferred too. The hint is the one that may fall back to the seat, so it does, under the guards the deal's fallback has.

package delegate

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// gatedNode is a node with room by its own health (one execution slot, none running) whose admission ceiling
// (max_queue_depth 2) this process has used up: two other Runs of it hold both dispatch slots, so the deal gives the
// node a subtask and the gate then turns the dispatch away.
func gatedNode(t *testing.T) (*fakeNode, string) {
	t.Helper()
	node, url := acceptingNode(t, "node-a", "zorblax from A", withHeadroom(1, 0))
	holdGate(t, url, 2, 2, 0)
	return node, url
}

// TestAGatedRemoteHintTakesTheIdleSeatWhenTheCapacityWaitIsOff: the operator's off switch. The hint ran nowhere before (a
// capacity defer, with the seat idle); it runs on the seat and says why. The control is the same call as a remote pin,
// which defers and touches no seat.
func TestAGatedRemoteHintTakesTheIdleSeatWhenTheCapacityWaitIsOff(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := gatedNode(t)
	cfg := pinCfg(t)
	cfg.AgentPlacementWaitSec = -1 // testCfg's default, stated: the wait is the subject
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(1), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Succeeded != 1 || sum.Deferred != 0 || localCalls.Load() != 1 || node.dispatches.Load() != 0 || !pr.ranLocal {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, ran local %v, want the idle seat to run what the gate turned away", sum, localCalls.Load(), node.dispatches.Load(), pr.ranLocal)
	}
	for _, want := range []string{
		"route=remote was a hint (no pin_reason), overridden: placed on the local seat",
		"the dealt node was full for this process and the capacity wait is switched off (agent_placement_wait_sec=-1)",
		"the idle local seat takes it instead, a remote hint falling back where no remote has room",
		"process gate: this process already holds 2 dispatch(es) open on node-a",
	} {
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
	if psum.Deferred != 1 || pinned[0].Result.DeferClass != core.DeferClassCapacity || pinned[0].ranLocal {
		t.Fatalf("pinned: summary %+v, class %q, ran local %v, want the capacity defer a remote pin always got", psum, pinned[0].Result.DeferClass, pinned[0].ranLocal)
	}
}

// TestAGatedRemoteHintTakesTheIdleSeatWhenTheRunIsSheddable: priority -1 never waits, so the hint was shed with the seat
// idle (the wait being on changed nothing). The control is the same sheddable call as a remote pin, which is shed.
func TestAGatedRemoteHintTakesTheIdleSeatWhenTheRunIsSheddable(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := gatedNode(t)
	cfg := pinCfg(t)
	cfg.AgentPlacementWaitSec = 3
	var localCalls atomic.Int64
	opts, tally := countedOpts("")
	opts.Priority = core.BandSheddable
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls), contracts(1), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Succeeded != 1 || sum.Shed != 0 || localCalls.Load() != 1 || node.dispatches.Load() != 0 || !pr.ranLocal {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, ran local %v, want the idle seat to run what the gate turned away", sum, localCalls.Load(), node.dispatches.Load(), pr.ranLocal)
	}
	for _, want := range []string{
		"route=remote was a hint (no pin_reason), overridden: placed on the local seat",
		"sheddable work (priority -1) does not wait",
		"a remote hint falling back where no remote has room",
	} {
		if !strings.Contains(pr.PlacementReason, want) {
			t.Errorf("placement = %q, want it to contain %q", pr.PlacementReason, want)
		}
	}
	if st := tally.Snapshot(); st.Hints != 1 || st.HintsOverridden != 1 {
		t.Errorf("tally %+v, want 1 hint, overridden", st)
	}

	pinnedOpts := doorOpts(PinOperator)
	pinnedOpts.Priority = core.BandSheddable
	pinned, psum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, pinnedOpts)
	if err != nil {
		t.Fatal(err)
	}
	if psum.Shed != 1 || !pinned[0].shed || pinned[0].ranLocal {
		t.Fatalf("pinned: summary %+v, shed %v, ran local %v, want the shed a remote pin always got", psum, pinned[0].shed, pinned[0].ranLocal)
	}
}

// TestAGatedRemoteHintNeverTakesAnOccupiedSeatWithNoWait: the new door is no way round the guard of the old ones. Another
// vLLM seat holds the cards, so the deal read the seat busy and the hint's fallback was never open; at either exit the
// subtask is deferred (wait off) or shed (sheddable), as before, and nothing is loaded over the occupant.
func TestAGatedRemoteHintNeverTakesAnOccupiedSeatWithNoWait(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	for _, tc := range []struct {
		name      string
		wait      int
		priority  int
		wantShed  bool
		wantDefer bool
	}{
		{"the wait is off", -1, core.BandNormal, false, true},
		{"the run is sheddable", 3, core.BandSheddable, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, url := gatedNode(t)
			cfg := occupiedCfg(t, occupiedSwap(t, "opencode-seat"))
			cfg.FleetMaxConcurrentJobs = 4
			cfg.AgentPlacementWaitSec = tc.wait
			opts := doorOpts("")
			opts.Priority = tc.priority
			results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), contracts(1), "remote", []string{url}, opts)
			if err != nil {
				t.Fatal(err)
			}
			pr := results[0]
			if pr.ranLocal || node.dispatches.Load() != 0 || (tc.wantShed && sum.Shed != 1) || (tc.wantDefer && (sum.Deferred != 1 || pr.Result.DeferClass != core.DeferClassCapacity)) {
				t.Fatalf("summary %+v, ran local %v, node dispatches %d, class %q, want the subtask shed or deferred with the occupied seat untouched", sum, pr.ranLocal, node.dispatches.Load(), pr.Result.DeferClass)
			}
		})
	}
}

// gatedScenario is one remote hint the process gate turned away, as awaitCapacity receives it.
type gatedScenario struct {
	r        *runner
	pl       *placements
	c        core.AgentContract
	start    time.Time
	seed     PlacedResult
	refusals []string
	calls    *atomic.Int64
	// what the local seat was handed: the wall it got, and whether it was an auto wall
	seatWall atomic.Int64
	seatAuto atomic.Bool
}

// gatedHintExit runs awaitCapacity for a remote hint the process gate turned away, at an exit that has no wait to carry it
// (the operator's off switch, or a sheddable run), and returns what the subtask became and the scenario it ran in. tune
// changes the world the hint finds, after the deal's reading (hintSeatFree: true) was taken.
func gatedHintExit(t *testing.T, sheddable bool, tune func(t *testing.T, s *gatedScenario)) (PlacedResult, *gatedScenario) {
	t.Helper()
	_, url := acceptingNode(t, "node-y", "answer from y", withHeadroom(2, 0))
	cfg := pinCfg(t)
	cfg.AgentPlacementWaitSec = -1
	s := &gatedScenario{pl: newPlacements(), c: plainContract(), start: time.Now(), calls: new(atomic.Int64)}
	s.pl.waitNote = "test: the dealt node was at its process gate"
	s.seed = PlacedResult{waitCapacity: true, gated: true, pendingReason: s.pl.waitNote}
	seat := passingLocal(s.calls)
	local := func(ctx context.Context, c core.AgentContract, o LocalOptions) (core.AgentWireResult, error) {
		s.seatWall.Store(int64(c.TimeoutSec))
		s.seatAuto.Store(c.TimeoutAuto)
		return seat(ctx, c, o)
	}
	s.r = &runner{cfg: cfg, route: "auto", pin: pinCall{asked: "remote", hint: true}, remotes: []string{url}, local: local, hintSeatFree: true}
	if sheddable {
		s.r.priority = core.BandSheddable
		s.r.cfg.AgentPlacementWaitSec = 3 // the wait is on: a sheddable run still never waits
	}
	if tune != nil {
		tune(t, s)
	}
	return s.r.awaitCapacity(t.Context(), 0, s.c, s.start, 30, s.pl, s.seed, s.refusals, ""), s
}

// TestAGatedRemoteHintTakesTheSeatOnlyWhileTheSeatIsFreeNow is the fallback's guards one at a time, at both exits. The
// deal read the seat free (hintSeatFree), and each row changes one thing the hint finds when the gate turns it away,
// which can be minutes later: a conjunct that is dropped from the fallback turns exactly its row red.
func TestAGatedRemoteHintTakesTheSeatOnlyWhileTheSeatIsFreeNow(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	idleProbe := func(context.Context) busyReading { return busyReading{} }
	probing := func(rd busyReading) func(t *testing.T, s *gatedScenario) {
		return func(t *testing.T, s *gatedScenario) {
			s.r.localBusyProbe = func(context.Context) busyReading { return rd }
		}
	}
	// onTheFlagshipBox swaps the scenario's box for the flagship fixture with a render held on a card; its agent seat is on card 0.
	onTheFlagshipBox := func(card string) func(t *testing.T, s *gatedScenario) {
		return func(t *testing.T, s *gatedScenario) {
			b := newFlagshipLeaseBox(t)
			b.hold(gpulease.ClassMedia, card)
			b.cfg.AgentPlacementWaitSec = s.r.cfg.AgentPlacementWaitSec
			s.r.cfg, s.r.decider = b.cfg, noDecision
		}
	}
	for _, tc := range []struct {
		name     string
		tune     func(t *testing.T, s *gatedScenario)
		wantSeat bool
	}{
		{"the seat is free", func(t *testing.T, s *gatedScenario) { s.r.localBusyProbe = idleProbe }, true},
		{"the seat cannot be read, and keeps the deal's answer: free", probing(busyReading{unknown: true, note: "busy probe failed"}), true},
		{"a render holds a card the seat does not use", onTheFlagshipBox(leaseCard2), true},
		{"part of the budget is spent: the seat is given what is left, as an explicit wall", func(t *testing.T, s *gatedScenario) {
			s.start = time.Now().Add(-12 * time.Second)
			s.c.TimeoutAuto = true
		}, true},
		{"the gate left no note", func(t *testing.T, s *gatedScenario) { s.pl.waitNote = "" }, true},
		{"after a refusal chain: the landing carries its history", func(t *testing.T, s *gatedScenario) {
			s.refusals = []string{"node-b refused the dispatch (503)"}
		}, true},
		{"the deal read the seat busy", func(t *testing.T, s *gatedScenario) { s.r.hintSeatFree = false }, false},
		{"a text lease reserves the seat now", func(t *testing.T, s *gatedScenario) {
			dir, _ := holdLease(t, gpulease.ClassText, "weights A/B")
			s.r.cfg.GPULockPath = dir
		}, false},
		{"a render fences the seat now", func(t *testing.T, s *gatedScenario) {
			onTheFlagshipBox(leaseCard0)(t, s)
			lease := s.r.localLease(s.c)
			if fenced, _ := Fenced(lease); !fenced || Reserved(lease) {
				t.Fatalf("fixture: want a lease that fences the seat and does not reserve it, got %+v", lease)
			}
		}, false},
		{"another vLLM seat occupies the seat now", probing(busyReading{busy: true, occupiedBy: "opencode-seat"}), false},
		{"the seat is at its run cap now", probing(busyReading{busy: true, inflight: 4}), false},
		{"a load is in progress now", probing(busyReading{busy: true, loading: true}), false},
		{"the seat's run-cap line is full", func(t *testing.T, s *gatedScenario) {
			s.r.cfg.FleetMaxConcurrentJobs = 1
			occupyTheLocalSeat(t, s.r.cfg)
			s.r.localBusyProbe = idleProbe
		}, false},
		{"the contract names a layer this box does not declare", func(t *testing.T, s *gatedScenario) { s.c.Layer = "fast" }, false},
		{"this box has no agent seat", func(t *testing.T, s *gatedScenario) { s.r.cfg.AgentModel, s.r.cfg.Model = "", "" }, false},
		{"the subtask already tried the seat", func(t *testing.T, s *gatedScenario) { s.pl.tried[""] = true }, false},
		{"the budget is spent", func(t *testing.T, s *gatedScenario) {
			s.start = time.Now().Add(-25 * time.Second) // 5 s of the 30 s budget are left, under the 10 s floor
		}, false},
		{"the call is a local hint", func(t *testing.T, s *gatedScenario) { s.r.pin = pinCall{asked: "local", hint: true} }, false},
		{"the call is a reasoned remote pin", func(t *testing.T, s *gatedScenario) {
			s.r.pin, s.r.route = pinCall{asked: "remote", reason: PinOperator}, "remote"
		}, false},
		{"the subtask is the deal's overflow, not a gate turn-away", func(t *testing.T, s *gatedScenario) {
			s.seed = PlacedResult{waitCapacity: true, overflow: true, pendingReason: s.pl.waitNote}
		}, false},
	} {
		for _, exit := range []struct {
			name      string
			sheddable bool
			why       string
		}{
			{"wait off", false, "the capacity wait is switched off (agent_placement_wait_sec=-1)"},
			{"sheddable", true, "sheddable work (priority -1) does not wait"},
		} {
			t.Run(tc.name+"/"+exit.name, func(t *testing.T) {
				pr, s := gatedHintExit(t, exit.sheddable, tc.tune)
				if tc.wantSeat {
					if s.calls.Load() != 1 || !pr.ranLocal || pr.Err != "" || pr.Result.Deferred || pr.shed {
						t.Fatalf("local runs %d, ran local %v, err %q, deferred %v (%q), shed %v, want the idle seat to run the subtask", s.calls.Load(), pr.ranLocal, pr.Err, pr.Result.Deferred, pr.Result.Reason, pr.shed)
					}
					if !s.pl.tried[""] || s.pl.attempts != 1 {
						t.Errorf("tried[seat] %v, attempts %d, want the seat recorded as one real placement", s.pl.tried[""], s.pl.attempts)
					}
					wants := []string{exit.why, "a remote hint falling back where no remote has room"}
					if s.pl.waitNote != "" {
						wants = append(wants, s.pl.waitNote)
					}
					for _, want := range wants {
						if !strings.Contains(pr.PlacementReason, want) {
							t.Errorf("placement = %q, want it to contain %q", pr.PlacementReason, want)
						}
					}
					if strings.HasSuffix(pr.PlacementReason, "; ") || strings.Contains(pr.PlacementReason, ";;") {
						t.Errorf("placement = %q: a dangling separator where the gate's note would have been", pr.PlacementReason)
					}
					if wall := s.seatWall.Load(); wall > 30 || wall < 17 || (s.c.TimeoutAuto && wall > 19) || s.seatAuto.Load() {
						t.Errorf("the seat was handed a wall of %d s (auto %v) for a contract with %d s and %v auto started 12 s or less ago, want what is left, explicit", wall, s.seatAuto.Load(), s.c.TimeoutSec, s.c.TimeoutAuto)
					}
					if pr.Replacements != len(s.refusals) {
						t.Errorf("replacements = %d for %d refusal(s) in the chain, want the history carried onto the landing", pr.Replacements, len(s.refusals))
					}
					return
				}
				if s.calls.Load() != 0 || pr.ranLocal {
					t.Fatalf("local runs %d, ran local %v, want the seat left alone", s.calls.Load(), pr.ranLocal)
				}
				if exit.sheddable && !pr.shed {
					t.Errorf("result = shed %v deferred %v (%q), want the shed a sheddable run gets", pr.shed, pr.Result.Deferred, pr.Result.Reason)
				}
				if !exit.sheddable && (!pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity) {
					t.Errorf("result = deferred %v class %q (%q), want the capacity defer of a wait that is off", pr.Result.Deferred, pr.Result.DeferClass, pr.Result.Reason)
				}
				if s.pl.attempts != 0 {
					t.Errorf("attempts %d, want none: nothing ran and nothing was recorded", s.pl.attempts)
				}
			})
		}
	}
}

// TestAGatedRemoteHintWhoseSeatWantsToWaitKeepsTheCallersOutcome: on a composite box a contract too big for the agent seat
// is decided onto a long seat that would evict a busy agent seat, and the local attempt answers that it must WAIT. That is
// the capacity wait's business: the fallback publishes no sentinel, records nothing, and leaves the outcome the exit had.
func TestAGatedRemoteHintWhoseSeatWantsToWaitKeepsTheCallersOutcome(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	for _, sheddable := range []bool{false, true} {
		name := "wait off"
		if sheddable {
			name = "sheddable"
		}
		t.Run(name, func(t *testing.T) {
			cfg := compositeTestCfg(t)
			cfg.AgentPlacementWaitSec = -1
			if sheddable {
				cfg.AgentPlacementWaitSec = 3
			}
			rd := &readings{pairAgent: []placetable.SeatState{busyPair(3)}}
			var localCalls atomic.Int64
			r := &runner{cfg: cfg, route: "auto", pin: pinCall{asked: "remote", hint: true}, remotes: []string{"http://127.0.0.1:1"},
				local: passingLocal(&localCalls), hintSeatFree: true, decider: tableDecider(cfg, rd)}
			if sheddable {
				r.priority = core.BandSheddable
			}
			pl := newPlacements()
			seed := PlacedResult{waitCapacity: true, gated: true, pendingReason: "test: the dealt node was at its process gate"}
			pr := r.awaitCapacity(t.Context(), 0, contractOfTokens(205_000), time.Now(), 30, pl, seed, nil, "")
			if rd.decisions.Load() == 0 {
				t.Fatal("fixture: the composite decision was never asked, so the attempt never reached the point where the seat must wait")
			}
			if localCalls.Load() != 0 || pr.ranLocal || pr.waitCapacity || pl.attempts != 0 || pl.tried[""] {
				t.Fatalf("local runs %d, ran local %v, sentinel %v, attempts %d, tried %v, want nothing run, nothing recorded and no sentinel published", localCalls.Load(), pr.ranLocal, pr.waitCapacity, pl.attempts, pl.tried[""])
			}
			if (sheddable && !pr.shed) || (!sheddable && (!pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity)) {
				t.Errorf("result = shed %v deferred %v class %q, want the shed or capacity defer the exit had", pr.shed, pr.Result.Deferred, pr.Result.DeferClass)
			}
		})
	}
}
