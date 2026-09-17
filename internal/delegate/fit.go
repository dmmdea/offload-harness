// fit.go infers the coarse SHAPE of a contract from its own goal text so that
// route=spread can hand each subtask to a seat that suits it, instead of
// dealing the remote slots by blind rotation. Round-robin across heterogeneous
// seats (a big thinking seat, a 9B, a 4B) sends mechanical triage to the
// expensive seat and cross-file reasoning to the smallest one with equal
// probability; nothing in the deal knows the difference.
//
// Deterministic and local ON PURPOSE. A classifier CALL to choose a seat would
// spend a model round-trip per subtask to save one — a net loss on the cheapest
// contracts, which are exactly the ones this harness exists to run. Everything
// here is a pure function of the goal string and the nodes' own advertised
// numbers, so a placement is reproducible from the recorded contract alone.

package delegate

import (
	"fmt"
	"math"
	"regexp"

	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// Kind is the coarse shape of a contract.
type Kind int

const (
	// KindMechanical is extraction, listing, counting, filtering, digesting —
	// work whose answer is IN the material and needs no chain of inference. It
	// is also the DEFAULT for a goal no rule matches (see matchShape).
	KindMechanical Kind = iota
	// KindReasoning is explanation, causation, cross-file interaction, tracing,
	// comparison — work that needs the contract held in mind at once.
	KindReasoning
)

func (k Kind) String() string {
	if k == KindReasoning {
		return "reasoning"
	}
	return "mechanical"
}

// The rule patterns. Order between them is load-bearing and is the whole fix
// for one measured misroute — see shapeRules.
var (
	// quantityRe catches counting questions phrased as questions: "how many
	// files changed", "how much context does the seat advertise". These are
	// lookups, not explanations.
	quantityRe = regexp.MustCompile(`(?i)\bhow\s+(?:many|much|long|often|large|big|small)\b`)

	// explanationRe catches explanation, causation and cross-file relationship
	// work. The bare `how\s` alternative is deliberate — a genuine
	// how-question ("how the retry path interacts with the queue cap") is
	// reasoning — and it is SAFE only because quantityRe runs first.
	explanationRe = regexp.MustCompile(`(?i)(?:\b(?:explains?|explaining|explanation|why|interacts?|interacting|interaction|traces?|tracing|compares?|comparing|comparison|contrasts?|implications?|rationale|architecture|relationships?|reason\s+about|across\s+(?:these|the|both|all)\s+(?:files|modules|packages))\b|\bhow\s)`)

	// mechanicalRe catches the grunt-work verbs.
	mechanicalRe = regexp.MustCompile(`(?i)\b(?:extracts?|extracting|lists?|listing|enumerates?|enumerating|counts?|counting|tall(?:y|ies|ied)|tabulates?|tabulating|inventor(?:y|ies)|collects?|collecting|gathers?|gathering|digests?|summari[sz]e[sd]?|summari[sz]ing|classif(?:y|ies|ied|ying)|triage[sd]?|filters?|filtering|grep|report\s+the|find\s+(?:all|every))\b`)
)

// shapeRule is one deterministic pre-filter rule: a pattern, the shape it
// proves, and a name so a placement can say WHICH rule fired.
type shapeRule struct {
	name string
	re   *regexp.Regexp
	kind Kind
}

// shapeRules is an ORDERED pre-filter: the first rule that matches decides, and
// the order encodes the one collision worth spelling out.
//
// A bare "how " alternative inside the explanation pattern reads "how many
// files changed" as REASONING — a trivial count sent to the expensive seat.
// The fix is not a cleverer pattern (RE2 has no lookahead, so "how, but not how
// many" cannot be written as one expression) but PRECEDENCE: the quantity rule
// runs first and takes every quantity phrasing off the table, which is exactly
// what leaves the bare "how " alternative safe for genuine how-questions.
var shapeRules = []shapeRule{
	{"quantity", quantityRe, KindMechanical},
	{"explanation", explanationRe, KindReasoning},
	{"mechanical-verb", mechanicalRe, KindMechanical},
}

// matchShape is the deterministic PRE-FILTER, and it is named that rather than
// "classifier" because that is all it is: it decides the UNAMBIGUOUS cases and
// reports honestly (ok=false) when nothing fired.
//
// The no-match default is KindMechanical — the CHEAP seat. This harness exists
// to move grunt work off the expensive seat, so an unrecognised goal must fall
// toward cheap, never toward capable: defaulting ambiguity to the big seat
// silently rebuilds the round-robin problem for every goal the vocabulary does
// not cover. A wrong cheap placement costs a retry on a different seat (the
// engine already retries a failed-verification subtask elsewhere); a wrong
// expensive placement costs the capable seat, which is the resource being
// protected.
//
// ok=false is the single seam a better fallback plugs into — a cached
// per-contract-shape label carried on the contract, or a shape decided ONCE for
// a fan-out and reused across its subtasks. It must not become a per-subtask
// model round-trip: that spends a call to save a call.
func matchShape(goal string) (kind Kind, rule string, ok bool) {
	for _, r := range shapeRules {
		if r.re.MatchString(goal) {
			return r.kind, r.name, true
		}
	}
	return KindMechanical, "", false
}

// inferKind reports the shape of st from its own goal text.
func inferKind(st Subtask) Kind {
	k, _, _ := matchShape(st.Contract.Goal)
	return k
}

// shapeOf reports the shape AND the name of the rule that decided it, naming
// the no-match branch "default" rather than leaving it blank. It exists so a
// placement reason can say WHY a seat was chosen: "fit=mechanical/default" and
// "fit=mechanical/mechanical-verb" send an operator to completely different
// fixes — rephrase the goal, versus the vocabulary read it correctly — and a
// bare shape name cannot tell them apart.
func shapeOf(st Subtask) (Kind, string) {
	k, rule, ok := matchShape(st.Contract.Goal)
	if !ok {
		return k, "default"
	}
	return k, rule
}

// adequate reports whether v's ADVERTISED context ceiling provably holds this
// contract plus the reserve the agent loop itself consumes. It is the same
// arithmetic remoteEligible gates on, extracted so "smallest ADEQUATE seat" is
// a thing the code computes rather than a phrase in a comment.
//
// An unadvertised ceiling (0) is never adequate: unknown is not a capacity, and
// a seat that published no number must not win the mechanical contest by
// looking like the smallest one on the roster.
func adequate(st Subtask, v NodeView) bool {
	return st.EstTokens+specReserve <= v.AgentCtxTokens
}

// fitInadequate ranks a seat that cannot hold the contract below every seat
// that can, for BOTH kinds — below -AgentCtxTokens (mechanical) as well as
// below +AgentCtxTokens (reasoning). It is far past any plausible ceiling in
// tokens, so no advertised number can collide with it.
const fitInadequate = math.MinInt32

// scoreFit rates seat v for subtask st; higher wins. The ranking axis is the
// seat's ADVERTISED context ceiling, which is the only capability number the
// nodes actually publish:
//
//   - reasoning  → the roomiest adequate seat (headroom is what the work needs)
//   - mechanical → the SMALLEST adequate seat, so the roomier seat stays free
//     for work that needs it. An idle capable seat is the resource protected.
//
// A seat is only ranked once it is adequate, so "smallest" can never mean "too
// small". Callers still hand scoreFit an already-eligible roster — this is the
// second line of that defence, not the first.
//
// Note it never ranks the LOCAL seat above anything: local advertises no
// ceiling in a delegator run, so it reads as inadequate here. That is why
// placeSpread keeps the local rotation slot OUT of the contest instead of
// scoring it — inventing a ceiling for the local seat would be a fabricated
// capability claim, and the harness reports what a node advertised, never more.
//
// A node with layer rows (ADR 0039) is scored on the window of the seat the
// placement table DECIDES for the contract — the pair's agent seat for a
// contract that fits it, the long seat on overflow — not on the single
// advertised ceiling, which on a composite box is only one layer's. A
// decision that defers or waits is inadequate here exactly as it is in the
// gate.
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
	sr := v.SeatRate
	if sr == nil || sr.TokS <= 0 || sr.Samples <= 0 {
		return true, ""
	}
	policy := seatrate.SeatPolicy{Seat: v.AgentSeat, TokS: sr.TokS, RateSamples: sr.Samples, RateSource: "health seat_rate", ColdLoadSec: sr.ColdLoadSec}
	if b := v.SeatBudget; b != nil {
		policy.StepTokens, policy.Thinking = b.StepTokens, b.Thinking
	}
	in := seatrate.InputFor(policy, st.Contract)
	wallSec := st.Contract.TimeoutSec
	if st.Contract.TimeoutAuto {
		wallSec, _ = seatrate.AutoWallFor(policy, st.Contract)
	}
	if wallSec <= 0 {
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

// fitColdSec is the cold-load charge feasibleFinal (and the W-11 ETA key)
// use: policy.ColdLoadSec ONLY when SeatLoaded is KNOWN false — a positive
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
// Cold is charged separately here (fitColdSec) because whether it applies at
// all depends on the seat's CURRENT residency (NodeView), a fact
// seatrate.Input has no field for. Zeroing ColdLoadSec before calling Compute
// isolates exactly the think+step terms — nothing else in Compute's formula
// reads ColdLoadSec, so this changes no other term.
func otherSecExcludingCold(in seatrate.Input) float64 {
	coldFree := in
	coldFree.ColdLoadSec = 0
	return float64(seatrate.Compute(coldFree).OtherSec)
}

func scoreFit(st Subtask, v NodeView) int {
	window := v.AgentCtxTokens
	if dec, ok := remoteDecision(st, v); ok {
		if dec.Defer || dec.Wait {
			return fitInadequate
		}
		if dec.CtxTokens > 0 {
			window = dec.CtxTokens
		}
	} else if !adequate(st, v) {
		return fitInadequate
	}
	if inferKind(st) == KindReasoning {
		return window
	}
	return -window
}
