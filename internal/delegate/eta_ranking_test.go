// eta_ranking_test.go: W-11 (PR-5 item 5, register S-02, INV-5 rider clause
// (ii)) — among seats already past the capability/adequacy/feasibility gate,
// order by expected completion. Mechanical work orders eta-first (replacing
// fit.go's old -window = smallest-seat-wins rule); reasoning-shaped work
// keeps window first, eta as its tie-break; two near-tied etas are decided by
// a seeded per-Run draw (power-of-two-choices) so independent dispatchers do
// not herd onto the single fastest seat.

package delegate

import (
	"encoding/json"
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
	slow := etaFixtureRemote("lenovo", 8192, 5, 0)
	fast := etaFixtureRemote("aorus", 8192, 34, 0)
	slow.QueueDepth, fast.QueueDepth = 1, 1

	got := Place("job-a", st, localNode(), []NodeView{slow, fast}, true)
	if got.NodeID != "aorus" {
		t.Fatalf("Place chose %q, want the 34 tok/s seat (aorus)", got.NodeID)
	}
	// Order in the roster must not matter.
	got = Place("job-a", st, localNode(), []NodeView{fast, slow}, true)
	if got.NodeID != "aorus" {
		t.Fatalf("Place chose %q with the roster reversed, still want aorus", got.NodeID)
	}
}

// TestPlaceReasoningPrefersTheRoomierSeatEvenWhenSlower: a roomier but slower
// seat, whose fitted final does NOT floor (StepTokens=512 clears the wall
// comfortably at either rate), must win reasoning-shaped work over a smaller,
// faster seat — window is the PRIMARY key for reasoning, not eta.
func TestPlaceReasoningPrefersTheRoomierSeatEvenWhenSlower(t *testing.T) {
	st := oneStepSubtask("explain why the build failed across these files", 300)
	small := etaFixtureRemote("aorus", 8192, 34, 0)  // small window, fast
	roomy := etaFixtureRemote("lenovo", 32768, 5, 0) // roomy window, slow — must still not floor
	small.QueueDepth, roomy.QueueDepth = 1, 1

	if ok, reason := feasibleFinal(st, roomy); !ok {
		t.Fatalf("fixture bug: the roomy slow seat must not floor; reason=%q", reason)
	}

	got := Place("job-b", st, localNode(), []NodeView{small, roomy}, true)
	if got.NodeID != "lenovo" {
		t.Fatalf("Place chose %q, want the roomier seat (lenovo) for reasoning-shaped work", got.NodeID)
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
	if scoreFit(st, bigFast) <= scoreFit(st, smallSlow) {
		t.Fatalf("mechanical scoreFit must prefer the faster seat when both publish a rate: big-fast=%d small-slow=%d",
			scoreFit(st, bigFast), scoreFit(st, smallSlow))
	}
}

// TestQueueWaitForFormula pins the queueWait arithmetic directly: the node's
// published queue_wait_estimate_sec wins outright when present; otherwise
// max(0, running+queued-max+1) × recent_wall / max(1, max).
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
	published := 7.5
	v.QueueWaitEstimateSec = &published
	if got := queueWaitFor(v); got != 7.5 {
		t.Fatalf("queueWaitFor must prefer the node's own published estimate: got %v, want 7.5", got)
	}
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
	nodes = append(nodes, busy, cold, coldNoRate, busyNoRate)

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
