// eta.go is the expected-completion layer of placement (W-11, register S-02,
// the INV-5 rider's clause (ii)): "an ordering key among seats that have
// already passed the capability/adequacy gate, with power-of-two-choices
// among near-ties so independent dispatchers do not herd." It never widens
// who is eligible — that is gate.go's eligibilityVerdict, which calls
// feasibleFinal below (W-05). It only orders the survivors.

package delegate

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// minimalFinalTokens is the smallest answer a contract can be said to have:
// the feasibility floor asks only whether the wall holds one tool step and
// this many tokens of final at the seat's rate.
const minimalFinalTokens = 64

// feasibleFinal reports whether v can produce ANY answer for st inside the
// contract's own effective wall (W-05, register S-03/S-05 — the INV-5
// rider's clause (i): a wall-time term enters a seat decision "as a refusal
// only below a minimum viable final, naming the arithmetic"). The question
// is the smallest one that still means something: one tool step (the read of
// the context document) plus a minimalFinalTokens-token final at the seat's
// measured rate — no think block, no structured re-pack, and NO cold load,
// because admission pays the cold load OUTSIDE the wall (D-64 warms the seat
// on the admission budget), so a wall shorter than the load is not
// infeasible. Everything above that floor — how much of the configured final
// the wall actually buys, the cold load, the queue wait — is a RANKING
// matter for etaFor, never a refusal.
//
// ok=true, reason="" — NO OPINION — when the rate is unknown (nil SeatRate,
// or zero tok_s/samples) or the contract's wall cannot be sized at all: the
// house rule every other capacity field in this package follows ("unknown is
// never credited", but also never PENALISED — see AgentCtxTokens==0).
//
// ok=false with reason naming the arithmetic ("one step and a 64-token
// answer need 42 s at 5.4 tok/s, the wall is 20 s") when the wall the
// contract would actually run under — TimeoutSec as given, or
// seatrate.AutoWallFor's sizing for a timeout_auto contract, exactly as
// autoPollBound computes it — cannot hold even that. This is never the
// seat's published min_turn_sec (its max-final worst case), which the rider
// forbids gating on, and since 0.128.1 it is no longer a fit of the
// configured final against seatrate.FinalBudgetFloor either: 0.128.0 shipped
// that rule and it refused a cold Aorus a 60 s contract the seat completes
// in ~25 s ("fitted final 0 < floor 1024").
//
// A composite node's published seat_rate describes its single advertised
// agent seat, not necessarily whatever layer remoteDecision would dispatch
// to; this reads it anyway (same imprecision autoPollBound already accepts
// for the poll bound) rather than inventing a second rate source.
func feasibleFinal(st Subtask, v NodeView) (ok bool, reason string) {
	policy, in, wallSec, known := seatWallFor(st, v)
	if !known || wallSec <= 0 {
		return true, ""
	}
	// One tool step plus a minimal final, no think block, no re-pack, no cold
	// load — the doc comment above says why each of those stays out.
	min := in
	min.ColdLoadSec = 0
	min.MaxSteps = 2 // one tool step, then the final
	min.FinalBudget = minimalFinalTokens
	min.RepackBudget = 0
	min.ThinkingAuto, min.ThinkingOn = false, false
	need := seatrate.Compute(min).TotalSec
	if need <= wallSec {
		return true, ""
	}
	return false, fmt.Sprintf("one step and a %d-token answer need %d s at %.1f tok/s, the wall is %d s", minimalFinalTokens, need, policy.TokS, wallSec)
}

// seatWallFor builds the seatrate policy/input for st on v and sizes the
// EFFECTIVE wall it would actually run under — the shared setup feasibleFinal
// and etaFor both need. known=false when v publishes no usable rate (nil
// SeatRate, or zero tok_s/samples) — the caller then has no opinion.
func seatWallFor(st Subtask, v NodeView) (policy seatrate.SeatPolicy, in seatrate.Input, wallSec int, known bool) {
	sr := v.SeatRate
	if sr == nil || sr.TokS <= 0 || sr.Samples <= 0 {
		return seatrate.SeatPolicy{}, seatrate.Input{}, 0, false
	}
	policy = seatrate.SeatPolicy{Seat: v.AgentSeat, TokS: sr.TokS, RateSamples: sr.Samples, RateSource: "health seat_rate", ColdLoadSec: sr.ColdLoadSec}
	if b := v.SeatBudget; b != nil {
		policy.StepTokens, policy.Thinking = b.StepTokens, b.Thinking
	}
	in = seatrate.InputFor(policy, st.Contract)
	wallSec = st.Contract.TimeoutSec
	if st.Contract.TimeoutAuto {
		wallSec, _ = seatrate.AutoWallFor(policy, st.Contract)
	}
	return policy, in, wallSec, true
}

// fitColdSec is the cold-load charge feasibleFinal and etaFor use:
// policy.ColdLoadSec ONLY when SeatLoaded is KNOWN false — a positive
// statement that the seat is not resident right now — never on an unknown
// reading, the same "never credit a penalty toward an unmeasured field" rule
// this package already follows for AgentCtxTokens==0 and GpuUtilKnown. A load
// already IN PROGRESS (SeatStarting) is charged HALF regardless of what
// SeatLoaded says (a starting seat is "not ready even though SeatLoaded may
// read true too" — nodeview.go): the load is partway there, and charging the
// full cold load again would double-count whatever it has already spent.
func fitColdSec(p seatrate.SeatPolicy, v NodeView) float64 {
	if v.SeatStarting != nil && *v.SeatStarting {
		return p.ColdLoadSec / 2
	}
	if v.SeatLoaded != nil && !*v.SeatLoaded {
		return p.ColdLoadSec
	}
	return 0
}

// otherSecExcludingCold is the "prefill + steps + think" seconds
// seatrate.Compute bakes into Estimate.OtherSec ALONGSIDE the cold load.
// Cold is charged separately (fitColdSec) because whether it applies at all
// depends on the seat's CURRENT residency (NodeView), a fact seatrate.Input
// has no field for. Zeroing ColdLoadSec before calling Compute isolates
// exactly the think+step terms — nothing else in Compute's formula reads
// ColdLoadSec, so this changes no other term.
func otherSecExcludingCold(in seatrate.Input) float64 {
	coldFree := in
	coldFree.ColdLoadSec = 0
	return float64(seatrate.Compute(coldFree).OtherSec)
}

// etaFor is W-11's expected-completion estimate for st on v: the cold load
// (fitColdSec's tri-state rule) + the node's own queue wait + the generation
// time for a final answer FITTED to v's own wall at v's measured rate — the
// same fit feasibleFinal computes, so a node that floors here is never the
// one an eta comparison prefers on a technicality (a floored fit still
// produces SOME budget, seatrate.FinalBudgetFloor, and its generation time
// is priced honestly; remoteEligible has already excluded it if it cannot
// even hold the floor).
//
// ok=false when v publishes no usable rate — the caller then keeps today's
// window-only ordering (rankFor/scoreFit).
func etaFor(st Subtask, v NodeView) (etaSec float64, ok bool) {
	policy, in, wallSec, known := seatWallFor(st, v)
	if !known {
		return 0, false
	}
	cold := fitColdSec(policy, v)
	fitBudget := in.FinalBudget
	if wallSec > 0 {
		// The final the node will actually budget (D-95): fitted to the WALL, not
		// to the wall minus the cold load — the wall starts after admission.
		fit := seatrate.FitFinalBudget(seatrate.FinalFit{
			ConfiguredFinal: in.FinalBudget,
			RemainingSec:    float64(wallSec),
			OtherSec:        otherSecExcludingCold(in),
			TokS:            policy.TokS,
			Schema:          len(st.Contract.OutputSchema) > 0,
		})
		fitBudget = fit.Budget
	}
	genIn := in
	genIn.ColdLoadSec = 0
	genIn.FinalBudget = fitBudget
	if in.RepackBudget > 0 {
		genIn.RepackBudget = fitBudget
	}
	gen := float64(seatrate.Compute(genIn).TotalSec)
	if wallSec > 0 && gen > float64(wallSec) {
		// The wall is the stop: a run never generates longer than its wall,
		// whatever the configured budgets add up to (a floored fit still runs).
		gen = float64(wallSec)
	}
	return cold + queueWaitFor(v) + gen, true
}

// queueWaitFor is the node's own admission backlog translated into an
// expected wait: the node's published queue_wait_estimate_sec when it
// publishes one (a future node, 0.128+ — PR-6's own estimate, preferred
// outright because the node knows its live backlog better than this
// arithmetic over a health snapshot ever can), else
//
//	max(0, jobs_running + jobs_queued − max_concurrent_jobs + 1) ×
//	  recent_agent_wall_sec / max(1, max_concurrent_jobs)
//
// — "how many jobs are ahead of a NEW one past the node's own concurrency,
// times how long a job here recently took, spread over the node's workers".
// 0 when the node publishes no recent wall at all: an unmeasured backlog is
// no opinion, not a zero-cost one (the queueWait TERM reads as absent, same
// as every other unknown field here — it does not make etaFor's ok=false,
// because cold/gen may still be known).
func queueWaitFor(v NodeView) float64 {
	if v.QueueWaitEstimateSec != nil {
		return *v.QueueWaitEstimateSec
	}
	if v.RecentAgentWallSec <= 0 {
		return 0
	}
	ahead := v.JobsRunning + v.JobsQueued - v.MaxConcurrentJobs + 1
	if ahead <= 0 {
		return 0
	}
	workers := v.MaxConcurrentJobs
	if workers < 1 {
		workers = 1
	}
	return float64(ahead) * v.RecentAgentWallSec / float64(workers)
}

// p2cNearTieFrac is W-11's power-of-two-choices threshold: two etas within
// this fraction of the larger one are a near-tie, decided by a seeded draw
// rather than by whichever happened to be measured a few seconds faster —
// the exact herding independent dispatchers would otherwise produce by all
// picking the single argmax (INV-5 rider clause (ii)).
const p2cNearTieFrac = 0.20

// etaDrawn is the eta a seat is RANKED by: its estimate, nudged by a draw
// derived from the seed and the seat's OWN node id. It is how W-11's
// power-of-two-choices clause survives being a well-formed ordering.
//
// Two things were wrong with the coin it replaces. It hashed the SEED ALONE,
// so it answered the same whichever way round it was asked — betterRemote(P, Q)
// and betterRemote(Q, P) were both true for a near-tie, and the fold then kept
// whichever near-tied seat came last in the roster. And "near-tie" was a
// pairwise distance test, which is not transitive: with a 20% band, 100 s is
// near-tied with 118 s and 118 s with 139 s while 100 s clearly beats 139 s, so
// the relation could cycle no matter how the tie itself was broken. Go states
// the requirement outright for anything used to order a set (slices.SortFunc
// wants a strict weak ordering, transitive incomparability included), and
// bestRemote's fold needs exactly that.
//
// Perturbing the KEY instead of branching on the pair fixes both at once: every
// seat gets one number computed from its own fields, so the ordering is total
// by construction, while seats whose estimates sit within the near-tie band can
// still come out either way depending on the seed. Bucketing the eta would also
// have been total, but it puts a hard edge inside the band — two seats 19%
// apart land in different buckets and stop spreading, which is the herding this
// exists to prevent. Jitter has no edges.
//
// The nudge is bounded by p2cNearTieFrac, so it only ever reorders seats that
// were close to begin with: the slowest a seat can be made to look is 1.1x and
// the fastest 0.9x, so two etas more than ~22% apart can never swap, while
// anything nearer can. One Run is internally consistent (same seed, same
// answer, so a capacity-wait tick does not thrash); different Runs draw
// differently, which is what spreads K independent dispatchers instead of
// letting them all pile onto the single argmax. Unlike a coin it spreads across
// ANY number of tied seats, not just two.
func etaDrawn(seed, node string, eta float64) float64 {
	return eta * (1 + p2cNearTieFrac*(p2cJitter(seed, node)-0.5))
}

// p2cJitter maps a seat's draw onto [0,1).
func p2cJitter(seed, node string) float64 {
	return float64(p2cRank(seed, node)) / (float64(math.MaxUint32) + 1)
}

// p2cRank scores one seat: FNV-1a over the seed and the node id, separated by a
// byte that cannot appear in either, so seed "a" + node "bc" and seed "ab" +
// node "c" are different draws. The tag keeps it from colliding with a hash
// used elsewhere for something else.
func p2cRank(seed, node string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("p2c:"))
	_, _ = h.Write([]byte(seed))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(node))
	// Avalanche (murmur3's fmix32) before the value is used as a FRACTION.
	// FNV-1a alone is fine for the low bit a coin reads, but its HIGH bits
	// barely move when two inputs differ only in their last byte — "node-a" and
	// "node-b" drew 0.715 and 0.703, so two seats whose ids differ by one
	// character got almost the same nudge and never swapped. Mixing first makes
	// the draws independent, which is the whole property.
	x := h.Sum32()
	x ^= x >> 16
	x *= 0x85ebca6b
	x ^= x >> 13
	x *= 0xc2b2ae35
	x ^= x >> 16
	return x
}

// rankInfo is what W-11's comparative ranking compares for one seat: the
// advertised window (fit.go's original axis, kept as the fallback and the
// reasoning-shaped primary key) and the eta this file adds.
type rankInfo struct {
	// node is the seat's node id, carried so the last-resort draw can score
	// THIS seat rather than flipping one coin for the whole comparison.
	node     string
	window   int
	eta      float64
	etaKnown bool
}

// fleetTokSPrior is the rate a seat is ASSUMED to run at when it publishes
// none: the median of the rates the rest of this roster actually publishes.
//
// It exists because the alternative is a comparison that changes metric per
// PAIR. Without a rate, etaFor fails, the pair falls back to comparing windows,
// and the same three nodes then get ranked on two different metrics depending
// on who is being compared — which produced real cycles in a brute-force probe
// over a mixed roster (an unmeasured node beat a measured one on window, that
// one beat a third on eta, the third beat the first on window). It is the same
// defect placementUtil fixes for utilization, and it has the same shape of fix:
// decide the metric ONCE per decision, not per pair.
//
// A fixed constant would have done that too, and would have been wrong in the
// way this harness already knows about: any constant is either "an unmeasured
// node always wins" (it takes every mechanical placement until it earns
// samples) or "an unmeasured node always loses" (it never gets work, so it
// never earns samples). The median is neither — it says "assume this seat is
// typical for this fleet until it tells us otherwise", which is exactly what
// agent.assumedPrefillTokS does for a stall wall, and it adapts as the fleet
// does. The node's OWN queue backlog and cold load still apply on top, so an
// unmeasured node that is visibly busy still ranks behind an idle one.
//
// Returns 0 when nobody published a rate: every seat is then unranked, the
// ordering falls to window for ALL of them — today's rule, unchanged — and it
// is uniform, so it is still a total order.
func fleetTokSPrior(remotes []NodeView) float64 {
	rates := make([]float64, 0, len(remotes))
	for _, r := range remotes {
		if sr := r.SeatRate; sr != nil && sr.TokS > 0 && sr.Samples > 0 {
			rates = append(rates, sr.TokS)
		}
	}
	if len(rates) == 0 {
		return 0
	}
	sort.Float64s(rates)
	mid := len(rates) / 2
	if len(rates)%2 == 1 {
		return rates[mid]
	}
	return (rates[mid-1] + rates[mid]) / 2
}

// rankFor builds st's rankInfo for v, resolving window exactly as scoreFit
// does (the layer table's CtxTokens when the node has rows, else the
// advertised ceiling) so the two never describe a seat's capacity
// differently.
//
// priorTokS (fleetTokSPrior over the roster being placed) is used only when v
// publishes no usable rate of its own: v is ranked as if it ran at the fleet's
// median rate, keeping its own queue backlog, cold load and budget. A measured
// rate always wins over the assumption, and one real run replaces it.
func rankFor(st Subtask, v NodeView, priorTokS float64) rankInfo {
	window := v.AgentCtxTokens
	if dec, ok := remoteDecision(st, v); ok && dec.CtxTokens > 0 {
		window = dec.CtxTokens
	}
	if eta, ok := etaFor(st, v); ok {
		return rankInfo{node: v.NodeID, window: window, eta: eta, etaKnown: true}
	}
	if priorTokS > 0 {
		assumed := v
		rate := SeatRateView{TokS: priorTokS, Samples: 1}
		if sr := v.SeatRate; sr != nil {
			rate.ColdLoadSec, rate.MinTurnSec = sr.ColdLoadSec, sr.MinTurnSec
		}
		assumed.SeatRate = &rate
		if eta, ok := etaFor(st, assumed); ok {
			return rankInfo{node: v.NodeID, window: window, eta: eta, etaKnown: true}
		}
	}
	return rankInfo{node: v.NodeID, window: window}
}

// betterRanked reports whether candidate should be preferred to incumbent
// under W-11 — decided=false when neither axis distinguishes them (the
// caller then falls through to whatever key comes after, e.g. betterRemote's
// QueueDepth/GpuUtil pair): for reasoning-shaped work the window decides
// first (the work needs the headroom) with eta as the tie-break among equal
// windows; for mechanical work eta decides first (the fastest expected
// completion among quality-adequate seats — this REPLACES fit.go's old
// "-window" = smallest seat wins) with window as the tie-break. Either side
// missing a known eta falls straight through to window-only comparison —
// "unknown rate keeps today's window ordering", the same rule feasibleFinal
// and every other capacity field in this package already follow.
func betterRanked(seed string, kind Kind, candidate, incumbent rankInfo) (better, decided bool) {
	ranked := candidate.etaKnown && incumbent.etaKnown
	drawn := func() (better, decided bool) {
		c, i := etaDrawn(seed, candidate.node, candidate.eta), etaDrawn(seed, incumbent.node, incumbent.eta)
		if c != i {
			return c < i, true
		}
		return false, false
	}
	if kind == KindReasoning {
		if candidate.window != incumbent.window {
			return candidate.window > incumbent.window, true
		}
		if ranked {
			if better, decided := drawn(); decided {
				return better, true
			}
		}
		return candidate.node < incumbent.node, candidate.node != incumbent.node
	}
	if ranked {
		if better, decided := drawn(); decided {
			return better, true
		}
	}
	if candidate.window != incumbent.window {
		return candidate.window < incumbent.window, true
	}
	// Two seats that tie on every key still need ONE answer, or the relation
	// stops being an ordering at exactly the point the fold depends on it.
	// Falling through to the later keys was safe only while nothing above could
	// order a pair one way and its mirror the other.
	if ranked {
		return candidate.node < incumbent.node, candidate.node != incumbent.node
	}
	return false, false
}

// etaScale/etaPrecision fold rankFor's two axes into the single int score
// route=spread's fitPick still maximises (fitPick has no pairwise seed to
// draw a P2C coin with — it scans nodes in ROTATION order, so an exact tie
// already falls to the rotation, which is itself a de-herding mechanism
// across a Run's own siblings). etaScale is far larger than any plausible
// window (context ceilings top out in the hundreds of thousands of tokens)
// so an eta difference of even 0.1 s always dominates a window difference —
// exactly the "eta first, window tie-break" / "window first, eta tie-break"
// ordering betterRanked gives the comparative callers, expressed as one
// maximisable number.
const (
	etaScale     = 1 << 24
	etaPrecision = 10 // eta rounded to 0.1 s before scaling
)

// scoreFitRanked is scoreFit's W-11 replacement for an ADEQUATE seat (the
// caller has already returned fitInadequate for anything that fails
// adequate()/the layer decision): folds rankFor's window+eta into one
// maximisable int, per kind, exactly as betterRanked orders a pairwise
// comparison. Unknown eta on v degrades to the ORIGINAL rule (-window /
// window) unchanged.
func scoreFitRanked(kind Kind, window int, eta float64, etaKnown bool) int {
	if kind == KindReasoning {
		if !etaKnown {
			return window
		}
		return window*etaScale - int(eta*etaPrecision)
	}
	if !etaKnown {
		return -window
	}
	return -int(eta*etaPrecision)*etaScale - window
}

// mintP2CSeed is a fallback seed for a Place() call site that has no wire job
// id in hand yet (re-placement / retry selection, run.go): a fresh random
// string, cheap and sufficient — the P2C property needed here is only "K
// independent dispatchers disagree", not "this exact decision is
// byte-reproducible", and the primary path (attempt()'s first placement)
// already threads the real job id.
func mintP2CSeed() string {
	return strings.TrimPrefix(mintJobID(), "agd-")
}
