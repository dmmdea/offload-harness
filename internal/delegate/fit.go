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
	"math"
	"regexp"
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
func scoreFit(st Subtask, v NodeView) int {
	if !adequate(st, v) {
		return fitInadequate
	}
	if inferKind(st) == KindReasoning {
		return v.AgentCtxTokens
	}
	return -v.AgentCtxTokens
}
