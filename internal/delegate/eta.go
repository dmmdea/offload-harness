// eta.go is the expected-completion layer of placement (W-11, register S-02,
// the INV-5 rider's clause (ii)): "an ordering key among seats that have
// already passed the capability/adequacy gate, with power-of-two-choices
// among near-ties so independent dispatchers do not herd." It never widens
// who is eligible — that is remoteEligible/feasibleFinal's job (W-05,
// fit.go). It only orders the survivors.

package delegate

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// feasibleFinal reports whether v can hold a VIABLE final answer for st
// within its own effective wall (W-05, register S-03/S-05 — the INV-5
// rider's clause (i): "a feasibility floor computed from the contract's
// FITTED final at the seat's published rate — never the seat's max-final
// min_turn_sec — applied ... as a refusal only below a minimum viable final,
// naming the arithmetic").
//
// ok=true, reason="" — NO OPINION — when the rate is unknown (nil SeatRate,
// or zero tok_s/samples) or the contract's wall cannot be sized at all: the
// house rule every other capacity field in this package follows ("unknown is
// never credited", but also never PENALISED — see AgentCtxTokens==0). ok=true
// also when the fitted final comfortably holds the configured budget.
//
// ok=false with reason naming the arithmetic ("fitted final 312 < floor 1024
// at 5.4 tok/s in 300 s wall, cold 69 s") when the wall the contract would
// actually run under — TimeoutSec as given, or seatrate.AutoWallFor's sizing
// for a timeout_auto contract, exactly as autoPollBound computes it — cannot
// buy even seatrate.FinalBudgetFloor tokens for the final answer (and its
// structured re-pack, when the contract carries a schema) at the seat's
// measured rate. This is a FITTED-final floor, not the seat's published
// min_turn_sec (its max-final worst case) — the rider forbids gating on that.
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
	cold := fitColdSec(policy, v)
	fit := seatrate.FitFinalBudget(seatrate.FinalFit{
		ConfiguredFinal: in.FinalBudget,
		RemainingSec:    float64(wallSec) - cold,
		OtherSec:        otherSecExcludingCold(in),
		TokS:            policy.TokS,
		Schema:          len(st.Contract.OutputSchema) > 0,
	})
	if !fit.Floored {
		return true, ""
	}
	return false, fmt.Sprintf("fitted final %d < floor %d at %.1f tok/s in %d s wall, cold %.0f s", fit.Fit, seatrate.FinalBudgetFloor, policy.TokS, wallSec, cold)
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
		fit := seatrate.FitFinalBudget(seatrate.FinalFit{
			ConfiguredFinal: in.FinalBudget,
			RemainingSec:    float64(wallSec) - cold,
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
		// Round 2 review, BUG item 7: a schema contract's re-pack is the
		// SECOND TURN of the same fitted split — seatrate.FitFinalBudget's
		// Schema:true branch already divides the available budget in half
		// (turns=2) and hands back ONE shared number for both turns. Leaving
		// RepackBudget at its UNFITTED value (the full configured final —
		// FinalBudgets sets repack=final before any fit runs) double-counted
		// the second turn at a size the wall was never proven to hold:
		// worked example, a Lenovo-shaped {5.4 tok/s, cold 69, final 8192}
		// seat on a 900 s auto wall fits to 2019 tokens, yet the un-fixed eta
		// was 69 + 2019/5.4 + 8192/5.4 ~= 1960 s — nearly DOUBLE the wall the
		// fit just proved feasible.
		genIn.RepackBudget = fitBudget
	}
	gen := float64(seatrate.Compute(genIn).TotalSec)
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

// etaPreferred reports whether candidate's eta beats incumbent's under W-11's
// ranking, given both are known: a clear win when they are NOT a near-tie
// (outside p2cNearTieFrac of each other), otherwise a deterministic draw
// seeded from seed (the job id) so ONE Run's own placement decisions are
// internally consistent (repeatable across ticks/sub-calls of the SAME
// decision) while DIFFERENT Runs — different seeds — land on either side,
// spreading K independent dispatchers across the near-tied seats instead of
// all converging on the single fastest one.
func etaPreferred(seed string, candidateEta, incumbentEta float64) bool {
	hi, lo := candidateEta, incumbentEta
	if lo > hi {
		hi, lo = lo, hi
	}
	if hi > 0 && (hi-lo)/hi <= p2cNearTieFrac {
		return p2cDraw(seed)
	}
	return candidateEta < incumbentEta
}

// p2cDraw is the deterministic coin the near-tie case flips: FNV-1a over the
// seed (mixed with a fixed tag so it never collides with a hash used
// elsewhere for something else) reduced to one bit. Deterministic so a Run's
// own repeated placement checks (a capacity-wait tick re-reading the same
// snapshot) do not thrash between the two seats mid-wait; varying with the
// seed so DIFFERENT Runs disagree — the whole point (measured over 100 job
// ids, contracts/digest-8.json-shaped, landing on both seats).
func p2cDraw(seed string) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte("p2c:"))
	_, _ = h.Write([]byte(seed))
	return h.Sum32()%2 == 0
}

// rankInfo is what W-11's comparative ranking compares for one seat: the
// advertised window (fit.go's original axis, kept as the fallback and the
// reasoning-shaped primary key) and the eta this file adds.
type rankInfo struct {
	window   int
	eta      float64
	etaKnown bool
}

// rankFor builds st's rankInfo for v, resolving window exactly as scoreFit
// does (the layer table's CtxTokens when the node has rows, else the
// advertised ceiling) so the two never describe a seat's capacity
// differently.
func rankFor(st Subtask, v NodeView) rankInfo {
	window := v.AgentCtxTokens
	if dec, ok := remoteDecision(st, v); ok && dec.CtxTokens > 0 {
		window = dec.CtxTokens
	}
	eta, ok := etaFor(st, v)
	return rankInfo{window: window, eta: eta, etaKnown: ok}
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
	if kind == KindReasoning {
		if candidate.window != incumbent.window {
			return candidate.window > incumbent.window, true
		}
		if candidate.etaKnown && incumbent.etaKnown && candidate.eta != incumbent.eta {
			return etaPreferred(seed, candidate.eta, incumbent.eta), true
		}
		return false, false
	}
	if candidate.etaKnown && incumbent.etaKnown && candidate.eta != incumbent.eta {
		return etaPreferred(seed, candidate.eta, incumbent.eta), true
	}
	if candidate.window != incumbent.window {
		return candidate.window < incumbent.window, true
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
