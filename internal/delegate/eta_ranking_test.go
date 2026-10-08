// eta_ranking_test.go: W-11 (PR-5 item 5, register S-02, INV-5 rider clause
// (ii)) — among seats already past the capability/adequacy/feasibility gate,
// order by expected completion. Mechanical work orders eta-first (replacing
// fit.go's old -window = smallest-seat-wins rule); reasoning-shaped work
// keeps window first, eta as its tie-break; two near-tied etas are decided by
// a seeded per-Run draw (power-of-two-choices) so independent dispatchers do
// not herd onto the single fastest seat.
//
// The eta every test here ranks by is the one ADR 0079 defines: cold + the
// node's own wait + the time to produce a reference final at the seat's rate,
// capped at no wall (it was capped at the wall until then). The tests that call
// etaFor/rankFor directly state what they assume of it where they do.

package delegate

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// etaFixtureRemote is a remote eligible for a one-step, thinking-off,
// schema-carrying contract: a small step/final budget (512 tokens, well
// under any fitted floor at the rates used here) so feasibility is never in
// question and only window/rate decide the ranking.
func etaFixtureRemote(id string, ctxTokens int, tokS, coldSec float64) NodeView {
	v := eligibleRemote()
	v.NodeID = id
	v.AgentCtxTokens = ctxTokens
	loaded := coldSec <= 0
	v.SeatLoaded = &loaded
	v.SeatRate = &SeatRateView{TokS: tokS, ColdLoadSec: coldSec, Samples: 5, MinTurnSec: 100}
	v.SeatBudget = &SeatBudgetView{StepTokens: 512, Thinking: "off"}
	return v
}

func oneStepSubtask(goal string, timeoutSec int) Subtask {
	return Subtask{
		Contract: core.AgentContract{
			SchemaVersion: core.AgentWireSchemaVersion,
			Goal:          goal,
			OutputSchema:  json.RawMessage(`{"properties":{"summary":{"type":"string"}}}`),
			Depth:         0,
			MaxSteps:      1,
			Thinking:      "off",
			TimeoutSec:    timeoutSec,
		},
		EstTokens: 1000,
	}
}

// TestPlaceMechanicalPrefersTheFasterSeatOnEqualWindows: two seats, equal
// windows/queue depth, 5 vs 34 tok/s — the 5.4-vs-34 shape from the
// diagnosis. Mechanical work must land on the faster seat (this REPLACES the
// old "-window" rule, which had no rate term at all and could send it to
// either seat arbitrarily).
func TestPlaceMechanicalPrefersTheFasterSeatOnEqualWindows(t *testing.T) {
	st := oneStepSubtask("extract every field from the report", 300)
	slow := etaFixtureRemote("node-c", 8192, 5, 0)
	fast := etaFixtureRemote("node-a", 8192, 34, 0)
	slow.QueueDepth, fast.QueueDepth = 1, 1

	got := Place("job-a", st, localNode(), []NodeView{slow, fast}, true)
	if got.NodeID != "node-a" {
		t.Fatalf("Place chose %q, want the 34 tok/s seat (node-a)", got.NodeID)
	}
	// Order in the roster must not matter.
	got = Place("job-a", st, localNode(), []NodeView{fast, slow}, true)
	if got.NodeID != "node-a" {
		t.Fatalf("Place chose %q with the roster reversed, still want node-a", got.NodeID)
	}
}

// TestPlaceReasoningPrefersTheRoomierSeatEvenWhenSlower: a roomier but slower
// seat that stays FEASIBLE (one step and a minimal answer fit the 300 s wall
// comfortably at either rate; the wall decides feasibility and is not an eta
// input) must win reasoning-shaped work over a smaller, faster seat — window
// is the PRIMARY key for reasoning, not eta.
func TestPlaceReasoningPrefersTheRoomierSeatEvenWhenSlower(t *testing.T) {
	st := oneStepSubtask("explain why the build failed across these files", 300)
	small := etaFixtureRemote("node-a", 8192, 34, 0) // small window, fast
	roomy := etaFixtureRemote("node-c", 32768, 5, 0) // roomy window, slow — must still be feasible
	small.QueueDepth, roomy.QueueDepth = 1, 1

	if ok, reason := feasibleFinal(st, roomy); !ok {
		t.Fatalf("fixture bug: the roomy slow seat must stay feasible; reason=%q", reason)
	}

	got := Place("job-b", st, localNode(), []NodeView{small, roomy}, true)
	if got.NodeID != "node-c" {
		t.Fatalf("Place chose %q, want the roomier seat (node-c) for reasoning-shaped work", got.NodeID)
	}
}

// TestPlaceP2CDrawDistributesAcrossNearTiedSeats: two seats whose etas are
// within the 20% near-tie band — over 100 different job ids, the deterministic
// per-Run draw must land on BOTH seats at least once (never always the same
// one, which is exactly the herding INV-5's power-of-two-choices clause
// exists to prevent).
func TestPlaceP2CDrawDistributesAcrossNearTiedSeats(t *testing.T) {
	st := oneStepSubtask("extract every field from the report", 300)
	a := etaFixtureRemote("node-a", 8192, 10, 0)
	b := etaFixtureRemote("node-b", 8192, 11, 0) // close enough to be a near-tie
	a.QueueDepth, b.QueueDepth = 1, 1

	etaA, okA := etaFor(st, a)
	etaB, okB := etaFor(st, b)
	if !okA || !okB {
		t.Fatalf("fixture bug: both seats must publish a usable rate (etaA ok=%v, etaB ok=%v)", okA, okB)
	}
	hi, lo := etaA, etaB
	if lo > hi {
		hi, lo = lo, hi
	}
	if (hi-lo)/hi > p2cNearTieFrac {
		t.Fatalf("fixture bug: etaA=%.1f etaB=%.1f are not within the %.0f%% near-tie band", etaA, etaB, p2cNearTieFrac*100)
	}

	seen := map[string]int{}
	for i := 0; i < 100; i++ {
		jobID := "job-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		got := Place(jobID, st, localNode(), []NodeView{a, b}, true)
		seen[got.NodeID]++
	}
	if seen["node-a"] == 0 || seen["node-b"] == 0 {
		t.Fatalf("the P2C draw must land on both near-tied seats across 100 job ids; got %v", seen)
	}
}

// TestScoreFitMechanicalPrefersTheFasterSeatWhenRatesArePublished pins
// scoreFit's own W-11 replacement of the plain "-window" rule: with BOTH
// seats publishing a usable rate, the smaller-window seat no longer
// automatically wins mechanical work just for being small — the faster one
// does, even when it happens to be the ROOMIER seat.
func TestScoreFitMechanicalPrefersTheFasterSeatWhenRatesArePublished(t *testing.T) {
	st := oneStepSubtask("list every exported function name in these files", 300)
	smallSlow := etaFixtureRemote("small-slow", 8192, 5, 0)
	bigFast := etaFixtureRemote("big-fast", 32768, 34, 0)
	if !scoreFit(st, bigFast).beats(scoreFit(st, smallSlow)) {
		t.Fatalf("mechanical scoreFit must prefer the faster seat when both publish a rate: big-fast=%+v small-slow=%+v",
			scoreFit(st, bigFast), scoreFit(st, smallSlow))
	}
}

// TestQueueWaitForFormula pins the queueWait arithmetic directly: what the node
// publishes for a NEW job wins outright when present (new_job_wait_sec as-is; the
// older queue_wait_estimate_sec, the wait of its deepest QUEUED job, one slot
// deeper - ADR 0073); otherwise max(0, running+queued-max+1) × recent_wall /
// max(1, max).
func TestQueueWaitForFormula(t *testing.T) {
	v := NodeView{JobsRunning: 3, JobsQueued: 2, MaxConcurrentJobs: 4, RecentAgentWallSec: 40}
	// ahead = 3+2-4+1 = 2; workers = 4 → 2*40/4 = 20
	if got := queueWaitFor(v); got != 20 {
		t.Fatalf("queueWaitFor = %v, want 20", got)
	}
	idle := NodeView{JobsRunning: 0, JobsQueued: 0, MaxConcurrentJobs: 4, RecentAgentWallSec: 40}
	if got := queueWaitFor(idle); got != 0 {
		t.Fatalf("queueWaitFor(idle) = %v, want 0 (nothing ahead)", got)
	}
	noWall := NodeView{JobsRunning: 5, JobsQueued: 5, MaxConcurrentJobs: 4}
	if got := queueWaitFor(noWall); got != 0 {
		t.Fatalf("queueWaitFor(no recent wall) = %v, want 0 — unmeasured backlog is no opinion, not a penalty", got)
	}
	// An older node's estimate is the wait of the deepest job ALREADY queued, so a new job waits one slot
	// (wall 40 s / 4 workers = 10 s) longer: the node's own number plus the slot, not the bare estimate.
	published := 7.5
	v.QueueWaitEstimateSec = &published
	if got := queueWaitFor(v); got != 17.5 {
		t.Fatalf("queueWaitFor must prefer the node's own published estimate, priced one slot deeper for a new job: got %v, want 7.5 + 10", got)
	}
	// A node that publishes the wait of a NEW job is believed as it stands: it already counted the slot.
	forNew := 12.0
	v.NewJobWaitSec = &forNew
	if got := queueWaitFor(v); got != 12 {
		t.Fatalf("queueWaitFor must use new_job_wait_sec as it stands: got %v, want 12", got)
	}
}

// TestAPublishedZeroNewJobWaitIsBelievedOverTheCounters: a node that says a new job waits 0 s is believed,
// whatever jobs_running says. Those counters include uncapped jobs (ten renders on a node whose four agent
// slots are idle read as 10 running against 4 workers), which is why the node publishes the zero at all.
// Without the field the counters price the node: (10 + 0 - 4 + 1) x 60 s / 4 = 105 s.
func TestAPublishedZeroNewJobWaitIsBelievedOverTheCounters(t *testing.T) {
	zero := 0.0
	v := NodeView{JobsRunning: 10, JobsQueued: 0, MaxConcurrentJobs: 4, RecentAgentWallSec: 60, NewJobWaitSec: &zero}
	if got := queueWaitFor(v); got != 0 {
		t.Fatalf("queueWaitFor with a published new_job_wait_sec of 0 = %v, want 0: the node counted its capped backlog", got)
	}
	if sec, known := etaStartFor(v); !known || sec != 0 {
		t.Fatalf("etaStartFor = %v (known %v), want a known 0", sec, known)
	}
	v.NewJobWaitSec = nil
	if got := queueWaitFor(v); got != 105 {
		t.Fatalf("queueWaitFor without the field = %v, want the counters' 105 s: the misreading the explicit zero prevents", got)
	}
}

// mixedFleetRoster is the roster the ordering tests share: measured and unmeasured seats, four window
// sizes, backlogs and cold loads - the shapes a real fleet mid-rollout actually has.
func mixedFleetRoster() []NodeView {
	mk := func(id string, ctx int, tokS float64) NodeView { return etaFixtureRemote(id, ctx, tokS, 0) }
	nodes := []NodeView{
		mk("a", 8192, 10), mk("b", 8192, 11), mk("c", 8192, 12), mk("d", 8192, 13),
		mk("e", 32768, 10), mk("f", 8192, 34), mk("g", 8192, 5),
		mk("h", 4096, 10), mk("i", 16384, 11), mk("j", 32768, 12),
	}
	for _, nr := range []struct {
		id  string
		ctx int
	}{{"norate-small", 4096}, {"norate-mid", 8192}, {"norate-big", 16384}, {"norate-huge", 32768}} {
		v := eligibleRemote()
		v.NodeID, v.AgentCtxTokens = nr.id, nr.ctx
		nodes = append(nodes, v)
	}
	busy := mk("busy", 8192, 10)
	busy.JobsRunning, busy.JobsQueued, busy.RecentAgentWallSec = 2, 3, 90
	cold := mk("cold", 8192, 10)
	notLoaded := false
	cold.SeatLoaded = &notLoaded
	cold.SeatRate.ColdLoadSec = 120
	coldNoRate := eligibleRemote()
	coldNoRate.NodeID, coldNoRate.SeatLoaded = "cold-norate", &notLoaded
	busyNoRate := eligibleRemote()
	busyNoRate.NodeID = "busy-norate"
	busyNoRate.JobsRunning, busyNoRate.JobsQueued, busyNoRate.RecentAgentWallSec = 2, 3, 90
	return append(nodes, busy, cold, coldNoRate, busyNoRate)
}

// TestEtaRankingIsAStrictWeakOrdering brute-forces the property the ranking
// has to have and twice did not. bestRemote FOLDS betterRemote over a roster,
// and Go states the requirement for any comparison used to order a set
// (slices.SortFunc: a strict weak ordering, transitive incomparability
// included). Two defects broke it:
//
//   - the near-tie coin hashed the SEED alone, so it answered the same
//     whichever way round it was asked and P beat Q while Q beat P;
//   - a seat with no measured rate fell back to comparing WINDOWS for that pair
//     only, so a mixed roster was ranked on two different metrics depending on
//     who was being compared, and cycled.
//
// The roster mixes measured and unmeasured seats, four window sizes, backlogs
// and cold loads — the shapes a real fleet mid-rollout actually has.
func TestEtaRankingIsAStrictWeakOrdering(t *testing.T) {
	st := oneStepSubtask("extract every field from the report", 300)
	assertStrictWeakOrdering(t, st, mixedFleetRoster())
}

// TestEtaRankingIsAStrictWeakOrderingAcrossSeatsPastTheWall: the same property over a roster whose slow seats
// read past the 300 s wall, which the uncapped eta of ADR 0079 now orders by rate (under the cap they all read
// the wall and tied), mixed with a seat that publishes no rate.
func TestEtaRankingIsAStrictWeakOrderingAcrossSeatsPastTheWall(t *testing.T) {
	st := oneStepSubtask("extract every field from the report", 300)
	var nodes []NodeView
	for _, r := range []float64{43, 20, 12, 6.57, 3.9} {
		nodes = append(nodes, etaSeat(fmt.Sprintf("seat-%v", r), r))
	}
	unmeasured := eligibleRemote()
	unmeasured.NodeID = "unmeasured"
	nodes = append(nodes, unmeasured)
	if eta, _ := etaFor(st, nodes[len(nodes)-2]); eta <= 300 {
		t.Fatalf("fixture bug: the slowest seat reads %.0f s, inside the 300 s wall", eta)
	}
	assertStrictWeakOrdering(t, st, nodes)
}

// assertStrictWeakOrdering is the brute force above over any roster.
func assertStrictWeakOrdering(t *testing.T, st Subtask, nodes []NodeView) {
	t.Helper()
	prior := fleetTokSPrior(nodes)
	if prior <= 0 {
		t.Fatal("fixture bug: this roster publishes rates, so it must have a prior")
	}
	for i := 0; i < 40; i++ {
		seed := "job-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		better := func(x, y NodeView) bool {
			ok, _ := betterRanked(seed, inferKind(st), rankFor(st, x, prior), rankFor(st, y, prior))
			return ok
		}
		for _, a := range nodes {
			for _, b := range nodes {
				if a.NodeID != b.NodeID && better(a, b) && better(b, a) {
					t.Fatalf("seed %s: %s and %s each beat the other", seed, a.NodeID, b.NodeID)
				}
				for _, c := range nodes {
					if better(a, b) && better(b, c) && better(c, a) {
						t.Fatalf("seed %s: cycle %s > %s > %s > %s", seed, a.NodeID, b.NodeID, c.NodeID, a.NodeID)
					}
				}
			}
		}
	}
}

// TestFleetPriorRanksAnUnmeasuredSeatAmongTheRest: a seat that publishes no
// rate is ranked as if it ran at the fleet's median, keeping its own backlog —
// not parked at one end of the order by a constant. A constant is what makes an
// unmeasured seat either win everything (and take placements from seats known
// to be fast) or win nothing (and never earn the samples that would rank it).
func TestFleetPriorRanksAnUnmeasuredSeatAmongTheRest(t *testing.T) {
	st := oneStepSubtask("extract every field from the report", 300)
	slow := etaFixtureRemote("slow", 8192, 5, 0)
	fast := etaFixtureRemote("fast", 8192, 50, 0)
	mid := etaFixtureRemote("mid", 8192, 25, 0)
	quiet := eligibleRemote()
	quiet.NodeID = "unmeasured"
	loaded := eligibleRemote()
	loaded.NodeID = "unmeasured-backlogged"
	loaded.JobsRunning, loaded.JobsQueued, loaded.MaxConcurrentJobs, loaded.RecentAgentWallSec = 1, 4, 1, 120

	roster := []NodeView{slow, fast, mid, quiet, loaded}
	prior := fleetTokSPrior(roster)
	if prior != 25 {
		t.Fatalf("fleet prior = %v, want the median published rate (25 tok/s)", prior)
	}
	if r := rankFor(st, quiet, prior); !r.etaKnown {
		t.Fatal("an unmeasured seat must be ranked on the assumed rate, not left unranked")
	}
	// Its own backlog still counts against it.
	quietEta := rankFor(st, quiet, prior).eta
	loadedEta := rankFor(st, loaded, prior).eta
	if !(loadedEta > quietEta) {
		t.Fatalf("a backlogged unmeasured seat (%.1fs) must rank behind an idle one (%.1fs)", loadedEta, quietEta)
	}
	// A measured seat is never displaced by the assumption: the fleet's fastest
	// still beats a seat we know nothing about.
	if !betterRemote("seed", &st, prior, fast, quiet) {
		t.Fatal("the fastest measured seat must still beat an unmeasured one")
	}

	// With nobody publishing a rate there is no prior, every seat is unranked,
	// and the ordering falls to window for ALL of them — the original rule,
	// applied uniformly, which is still a total order.
	none := []NodeView{quiet, loaded}
	if p := fleetTokSPrior(none); p != 0 {
		t.Fatalf("a roster with no published rate must have no prior; got %v", p)
	}
	if r := rankFor(st, quiet, 0); r.etaKnown {
		t.Fatal("with no prior available a seat must stay unranked, not be invented a rate")
	}
}

// TestScoreFitOrdersAMixedFleetAsThePairwiseRankingDoes extends the brute force above to the int fold
// route=spread deals by (ADR 0057, the diagnosis' F04). scoreFit used to rate a seat with no published
// rate on its window alone while rankFor priced it at the fleet's median, so the same roster was
// ordered one way by Place and another by the spread deal: an unmeasured seat's -window (about -3e4)
// sat five orders of magnitude above a measured seat's -eta x 10 x 2^24 and took every mechanical
// slot, and on reasoning work a measured 32k seat beat an unmeasured 131k one. Over the shared roster
// the fold now agrees with betterRanked for every pair the pairwise ranking decides without its
// seeded draw: reasoning pairs on different windows, and mechanical pairs whose expected completions
// are further apart than the draw can move them (p2cNearTieFrac, nudged +/-10 % either way).
func TestScoreFitOrdersAMixedFleetAsThePairwiseRankingDoes(t *testing.T) {
	nodes := mixedFleetRoster()
	prior := fleetTokSPrior(nodes)
	if prior <= 0 {
		t.Fatal("fixture bug: this roster publishes rates, so it must have a prior")
	}
	for _, goal := range []string{"extract every field from the report", "explain why the build failed across these files"} {
		st := oneStepSubtask(goal, 300)
		kind := inferKind(st)
		agree, checked := 0, 0
		for _, a := range nodes {
			for _, b := range nodes {
				if a.NodeID == b.NodeID {
					continue
				}
				sa, sb := scoreFitWith(st, a, prior), scoreFitWith(st, b, prior)
				ra, rb := rankFor(st, a, prior), rankFor(st, b, prior)
				if !sa.adequate || !sb.adequate {
					t.Fatalf("%s: %s or %s is inadequate for a 1,000-token contract: fixture bug", goal, a.NodeID, b.NodeID)
				}
				if !ra.etaKnown || !rb.etaKnown {
					t.Fatalf("%s: %s or %s has no assumed rate although the roster has a prior", goal, a.NodeID, b.NodeID)
				}
				// A pair the pairwise ranking may decide by its seeded draw is not comparable here.
				if kind == KindReasoning && ra.window == rb.window {
					continue
				}
				if kind == KindMechanical && math.Abs(ra.eta-rb.eta) <= 0.25*math.Max(ra.eta, rb.eta) {
					continue
				}
				if sa.score == sb.score {
					t.Errorf("%s: %s and %s tie on the fold although the pairwise ranking separates them (windows %d/%d, etas %.1f/%.1f)", goal, a.NodeID, b.NodeID, ra.window, rb.window, ra.eta, rb.eta)
					continue
				}
				checked++
				for i := 0; i < 8; i++ {
					seed := "job-" + string(rune('a'+i))
					better, decided := betterRanked(seed, kind, ra, rb)
					if !decided || better != sa.beats(sb) {
						t.Fatalf("%s seed %s: the fold ranks %s (%d) vs %s (%d) but the pairwise ranking says %v (decided %v)", goal, seed, a.NodeID, sa.score, b.NodeID, sb.score, better, decided)
					}
				}
				agree++
			}
		}
		if checked == 0 || agree != checked {
			t.Fatalf("%s: %d of %d comparable pairs agreed", goal, agree, checked)
		}
	}
}

// TestSpreadDealRanksAnUnmeasuredSeatOnTheFleetMedian is the same defect through the deal itself, in
// the two shapes the brute force summarises. A seat that publishes no rate (the sixth PC joins with
// samples == 0) must neither take the only remote slot of a mechanical fan-out from a measured 40
// tok/s seat nor lose a reasoning slot to a measured seat with a smaller window.
func TestSpreadDealRanksAnUnmeasuredSeatOnTheFleetMedian(t *testing.T) {
	remoteSlot := func(r *runner, goal string) string {
		contracts := []core.AgentContract{fitSubtask(goal, 100).Contract, fitSubtask(goal, 100).Contract}
		slots := r.dealSpread(contracts, fitLocal())
		if !slots[0].view.Local {
			t.Fatalf("slot 0 = %s, want the local rotation slot", slots[0].view.NodeID)
		}
		return slots[1].view.NodeID
	}
	rated := func(id string, ctx int, tokS float64) NodeView {
		v := etaFixtureRemote(id, ctx, tokS, 0)
		v.QueueDepth = 0
		return v
	}
	unmeasured := func(id string, ctx int) NodeView {
		v := eligibleRemote()
		v.NodeID, v.AgentCtxTokens, v.QueueDepth = id, ctx, 0
		return v
	}

	// Mechanical: roster order puts the unmeasured seat last, so a rotation tie cannot hide the defect.
	mech := fitRunner(rated("slow", 32768, 5), rated("fast", 32768, 40), unmeasured("unmeasured", 32768))
	if got := remoteSlot(mech, fitMechGoal); got != "fast" {
		t.Errorf("mechanical: the remote slot went to %q, want the measured 40 tok/s seat", got)
	}
	// Reasoning: the roomiest adequate seat first - an unmeasured seat with the biggest window.
	reason := fitRunner(rated("small-measured", 32768, 40), unmeasured("big-unmeasured", 131072))
	if got := remoteSlot(reason, fitReasonGoal); got != "big-unmeasured" {
		t.Errorf("reasoning: the remote slot went to %q, want the roomier seat although it publishes no rate", got)
	}
	// The old API is the prior-less rule, uniformly: with nobody measured it is still window-only.
	if a, b := scoreFit(fitSubtask(fitMechGoal, 100), unmeasured("u1", 8192)), scoreFit(fitSubtask(fitMechGoal, 100), unmeasured("u2", 32768)); !a.beats(b) {
		t.Errorf("scoreFit without a prior: the smaller unmeasured seat scored %+v against %+v, want it to win mechanical work as before", a, b)
	}
}
