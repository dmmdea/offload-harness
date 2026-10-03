// gate.go is the placement decision (§S3 as reshaped by roast deltas 3+5):
// a pure, exhaustively-tested function over NodeViews. Quality-first is the
// whole design: an idle local node ALWAYS runs the work (Place never
// load-balances for speed), and a remote node is even ELIGIBLE only when the
// contract is mechanically verifiable and provably fits the remote seat's
// context ceiling with room to run.

package delegate

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// Subtask pairs one delegation contract with its token estimate. EstTokens is
// carried beside the contract (not recomputed inside Place) so a caller that
// gains a REAL tokenizer count later can supply it without the gate changing.
type Subtask struct {
	Contract  core.AgentContract
	EstTokens int
}

// specReserve is the context the contract's own text does NOT account for on
// the remote node, reserved off the advertised ceiling before any fit check.
// Budget behind the number (conservative on purpose, roast delta 5 — "ctx
// honesty"): the loop's system prompt (~600 tok) + read-only tool specs
// (~700) + per-step transcript growth (tool call + result + plan text,
// ~150 tok/step) × core.AgentMaxStepsCap (12) ≈ 1800 — totalling ≈ 3100,
// held at a round 3072. At an 8k seat this leaves the documented ~2–4k-token
// effective doc budget; a bigger reserve would mostly refuse work a 12-step
// run can actually finish.
const specReserve = 3072

// EstimateTokens is the v1 token ESTIMATE for a contract: ceil(chars/3) over
// every part the remote node will hold in context — goal, each context doc
// (name AND text: both are materialized for the sub-agent to read), the raw
// output schema bytes (echoed into the constrained final step), and each
// acceptance string (echoed to the sub-agent).
//
// WHY chars/3, stated honestly (roast delta 5 requires the label): this is a
// DELIBERATE conservative bound, not a tokenizer. The house lesson is
// "tokenize, don't estimate" — chars/4 was measured 2x off on Gemma — but at
// placement time v1 has no remote tokenizer to ask, so the estimate leans the
// safe direction instead: /3 overshoots typical English prose (~4 chars/token
// ⇒ ~33% inflation), and the cases where even /3 undershoots (tokenizer-dense
// content) are backstopped by specReserve's padding and the 12-step remote
// cap. It is used ONLY as an upper-bound GATE — never to claim a fit — and a
// real-tokenizer count replacing it is the recorded v2 upgrade.
//
// The arithmetic lives in placetable.EstimateTokens since ADR 0039 so the
// delegator, the placement table and the node size a contract identically;
// this is the delegate's name for it.
func EstimateTokens(c core.AgentContract) int { return placetable.EstimateTokens(c) }

// Place decides which node runs st. The rule, exactly as reshaped:
//
//   - localBusy false ⇒ LOCAL, unconditionally. An idle local node always
//     wins — delegation exists to keep work flowing while the local GPU is
//     occupied, never to chase throughput on a weaker seat (quality-first,
//     operator verbatim: "not a race about speed").
//   - localBusy true ⇒ the best remote that passes the hard gate, ranked by
//     capacity, then by QueueDepth, then by GPU utilization (ties: first
//     listed, so the caller's roster order is the stable preference order —
//     see betterRemote). No remote passes ⇒ LOCAL regardless — queued-local
//     beats ineligible-remote every time.
//
// Place is pure: it never probes anything. Callers build the inputs from
// FetchNodeView + LocalBusy. seed is the W-11 P2C draw's input (the wire job
// id when the caller has minted one, else any string a caller wants two
// otherwise-identical calls to agree on) — see betterRemote.
func Place(seed string, st Subtask, local NodeView, remotes []NodeView, localBusy bool) NodeView {
	// A contract that names a layer (register A-100) is not the idle-local
	// rule's to keep: an idle local box that does not DECLARE the layer would
	// run it on its planner seat, silently. It goes to the remote that
	// declares it; with none, it still lands local, where the decision defers
	// naming the layer instead of running on the wrong seat.
	if !localBusy && (st.Contract.Layer == "" || declaresLayer(local, st.Contract.Layer)) {
		return local
	}
	// The rate assumption is computed ONCE, from this roster, and used for every
	// comparison in this decision — never per pair. See fleetTokSPrior.
	prior := fleetTokSPrior(remotes)
	var best NodeView
	found := false
	for _, r := range remotes {
		if !remoteEligible(st, r) {
			continue
		}
		if !found || betterRemote(seed, &st, prior, r, best) {
			best, found = r, true
		}
	}
	if !found {
		return local
	}
	return best
}

// declaresLayer reports whether a node's advertised rows carry the named
// layer — declared, not necessarily admissible: admission is the table's
// verdict (remoteDecision / runner.decide), this only says who to ask.
func declaresLayer(v NodeView, name string) bool {
	for _, row := range v.Layers {
		if row.Name == name {
			return true
		}
	}
	return false
}

// anyDeclaresLayer reports whether some node in views advertises the named layer.
func anyDeclaresLayer(views []NodeView, name string) bool {
	for _, v := range views {
		if declaresLayer(v, name) {
			return true
		}
	}
	return false
}

// betterRemote reports whether candidate should displace the incumbent. Only a
// STRICTLY better candidate displaces, so equal seats are kept in roster order
// and the caller's list stays the stable preference order it has always been.
//
// Four ordered keys, all boolean-or-int. Keys 1-3 form a total preorder over
// the WHOLE fleet — some nodes publish capacity, some do not, but "more free
// slots" is answered for every node by provablyStartsNow's PROVABLY (an
// unknown reads as false, never as an invented number), so those three keys
// never go incomparable.
//
// Key 4 does not carry that guarantee: it is comparable only within the
// SUBSET of nodes that publish gpu_util_known, because an unknown utilization
// is deliberately neither credited nor blamed rather than coerced into an
// order. That is a real gap in the preorder — A(util 80, known) vs B(unknown)
// vs C(util 10, known) gives C beats A while both A~B and B~C — so on a mixed
// fleet where key 4 is the only key left undecided, the tie-break outcome
// depends on roster order (the single left-to-right scan in Place) BY DESIGN,
// exactly like every other tie these keys leave open. The fix, when it
// matters, is operator-side: upgrade every node so gpu_util_known is uniformly
// true and the ordering is total again.
//
//  1. NOT provably saturated beats saturated. `queue_depth` alone was never a
//     placement signal — it is a count with no scale, and the node that
//     produces `503 queue full` is precisely the one whose depth has reached
//     max_queue_depth. A node at 1 of 1 is a certain refusal; a node at 500
//     with no published ceiling is not, and must win. This is the key that
//     makes placement capacity-aware.
//
//     It DEMOTES, it does not exclude, and that is deliberate — see saturated()
//     for why the delegator's copy of these numbers is stale BY CONSTRUCTION
//     rather than by caching. Re-placement (run.go) is the net that catches the
//     case where this demotion guessed wrong in the other direction.
//
//  2. A provably free execution slot beats one that is not provable. The job
//     starts NOW there rather than waiting in `accepted` — which is exactly
//     the state 0.100.0's `queue deadline` failure reports, so preferring it
//     removes refusals AND queue-deadline losses. See provablyStartsNow for
//     why an idle node that publishes nothing still qualifies: without that,
//     this key would demote every pre-0.100.0 node in a mixed fleet, including
//     a completely idle one.
//
//  3. Lower QueueDepth — the original rule, with its original meaning
//     (accepted + running), unchanged and still deciding every case the two
//     keys above do not.
//
//  4. Lower GPU utilization — ONLY when both publish it. An unknown is
//     neither credited nor blamed (the AgentCtxTokens == 0 rule), so a
//     pre-0.113.0 node keeps its roster-order tie. A tie-breaker, never a
//     primary signal — it only ever decides a case QueueDepth left tied.
//
// GPU routing P1 adds two more, both per node: the lease rung of key 0 now has
// an OVERDUE step below a long lease (leaseDemotionRank), and a free-card key
// (cardTier: a node with a card that can take the contract beats one whose cards
// are all busy; a node that published no per-card truth sits between them) sits
// after the ETA key and before QueueDepth.
//
// W-11 (register S-02, INV-5 rider clause (ii), eta.go) inserts a FIFTH key
// between provablyStartsNow and QueueDepth: expected completion among seats
// tied on capacity so far — eta.go's betterRanked, fed by st and seeded by
// `seed` for its power-of-two-choices draw on a near-tie. Undecided (neither
// side publishes a usable seat_rate, or the two are truly identical) falls
// through to QueueDepth/GpuUtil exactly as before — "unknown rate keeps
// today's window ordering" widened to "today's WHOLE ordering".
//
// st is a POINTER, and nil is a real, meaningful input: PlaceVision has no
// agent contract to fit a generation term against (a vision judgment is one
// call, never an agent loop — "no feasibility term; vision is single-shot"),
// so nil st runs the SIMPLER visionEtaBetter key (queue-wait only, still
// P2C-drawn) instead of the full window+generation ranking.
func betterRemote(seed string, st *Subtask, priorTokS float64, candidate, incumbent NodeView) bool {
	// Key 0 (W-14, register S-15): a node still eligible under a lease-busy
	// verdict loses to any node that is not — "ranked last" means it does not
	// even get to compete on saturation or queue depth against a clean node.
	// See leaseBusyDemoted for why checking LeaseBusy alone is safe here: the
	// gate has already excluded every OTHER lease-busy shape (exclusive,
	// draining, non-text), so a lease-busy survivor is always the one case
	// this key exists to demote.
	//
	// An OVERDUE lease (GPU routing P1) is demoted one rung further down: its
	// declared end has already passed, so nothing the node says about when the
	// card frees is evidence, where a long lease at least names its end. It
	// still ranks and still takes work when nothing better exists (the node
	// queues it); it is never excluded.
	if c, i := leaseDemotionRank(candidate, st), leaseDemotionRank(incumbent, st); c != i {
		return c < i // candidate wins only when the incumbent is the lower-ranked one
	}
	if c, i := saturated(candidate), saturated(incumbent); c != i {
		return i // candidate wins only when the incumbent is the saturated one
	}
	if c, i := provablyStartsNow(candidate), provablyStartsNow(incumbent); c != i {
		return c
	}
	if st != nil {
		if better, decided := betterRanked(seed, inferKind(*st), rankFor(*st, candidate, priorTokS), rankFor(*st, incumbent, priorTokS)); decided {
			return better
		}
	} else if better, decided := visionEtaBetter(seed, candidate, incumbent); decided {
		return better
	}
	// Free cards (GPU routing P1): a node with a card that can take this
	// contract beats one whose cards are all busy, and a node that published no
	// per-card truth sits between them (cardTier: some > unknown > none), so an
	// older node is neither credited nor blamed. This sits above the queue
	// COUNT, which cannot see a card held by work the harness does not own, and
	// ahead of the util scalar below, which answers "is there room" with the
	// busiest card on the box.
	if c, i := cardTier(candidate, layerOf(st)), cardTier(incumbent, layerOf(st)); c != i {
		return c > i
	}
	if candidate.QueueDepth != incumbent.QueueDepth {
		return candidate.QueueDepth < incumbent.QueueDepth
	}
	// The last key compares how busy each node's HARNESS cards are, not its
	// whole box. GpuUtilPct is the busiest card anywhere, so a node whose
	// operator is using the desktop — or gaming — advertised that load and lost
	// ties to an idler node it should have won (2026-09-20: a node read 33% from
	// a game while every card the harness could use was at 0%). WorkUtilPct
	// skips a display card, so it is the figure to compare on.
	//
	// placementUtil picks which figure PER NODE, never per pair. An earlier cut
	// fell back to GpuUtilPct whenever either side lacked WorkUtilPct, meaning
	// the same three nodes were ranked on two different metrics depending on who
	// was being compared — and during a rollout, which is the only time a mixed
	// fleet exists, that produced a strict CYCLE: a gaming upgraded node beat a
	// lightly loaded upgraded node on work_util, which beat an old node on
	// gpu_util, which beat the gaming node on gpu_util. bestRemote folds this
	// relation over a slice, so the winner became whichever node the slice
	// happened to start from.
	cu, ck := placementUtil(candidate)
	iu, ik := placementUtil(incumbent)
	if ck && ik && cu != iu {
		return cu < iu
	}
	return false
}

// placementUtil is the ONE busy-ness figure a placement comparison reads off a
// node: work_util_pct when the node publishes it (0.132.2 and later, which
// skips a card driving a display), the box-wide gpu_util_pct when it does not.
//
// Choosing it per node rather than per pair is what keeps betterRemote's last
// key a total order across a mixed fleet; see the cycle described there. The
// cost is that during a rollout an upgraded node's desktop-free figure is
// compared against an old node's desktop-inclusive one — which biases toward
// the upgraded node, the one whose number is actually true.
func placementUtil(v NodeView) (int, bool) {
	if v.WorkUtilKnown {
		return v.WorkUtilPct, true
	}
	if v.GpuUtilKnown {
		return v.GpuUtilPct, true
	}
	return 0, false
}

// saturated reports whether v's own advertisement says the next dispatch will
// be REFUSED: its admission ceiling on queue_depth is already met.
//
// It is a RANKING input, never a capability: remoteEligible is untouched, and a
// saturated node is still chosen when nothing better exists.
//
// The reason is that the DELEGATOR'S COPY of these numbers is stale by
// construction. (Not because the node caches them — it does not. fleetnode's
// health handler walks the job store live, in the same request, for exactly
// these counters; what IS cached over there is the VRAM snapshot and
// agent_seat_resident. An earlier draft of this comment said "cached read on
// the node side", which was simply false about how the node works.) Two things
// make the number old the moment it arrives:
//
//   - The snapshot ages between the health GET and the dispatch POST. Placement
//     is not atomic with admission, and any node can admit or finish jobs in
//     that gap in either direction.
//   - This run's own siblings eat the headroom it measured. Run fans out at
//     runConcurrency, and those subtasks probe within milliseconds of each
//     other — so several of them can read the same free slot and then compete
//     for it.
//
// Hard-excluding on a number that is stale by construction would strand a node
// that has since drained, on evidence that was never current. Demoting costs
// nothing when the reading was right and forfeits nothing when it was wrong,
// and re-placement (run.go) is what covers the case where it WAS right.
//
// MaxQueueDepth == 0 is UNKNOWN, not unlimited and not full: the node publishes
// 0 for unlimited and a node too old to publish the field decodes to 0 as well.
// Neither credited nor blamed — the same treatment AgentCtxTokens == 0 gets.
func saturated(v NodeView) bool {
	// A node that publishes saturation (0.113.18) says it in one word: `high`
	// is "a new dispatch is refused right now" by the node's OWN arithmetic
	// (queue cap, drain, text lease). It is OR'd with the local arithmetic, not
	// substituted for it, so an older node ranks exactly as before.
	return (v.SaturationKnown && v.SaturationHigh) || (v.MaxQueueDepth > 0 && v.QueueDepth >= v.MaxQueueDepth)
}

// hasRoom reports whether v would take a NEW dispatch right now by its own
// advertisement: not saturated, and — for a sheddable contract — holding an
// idle execution slot. It is the capacity wait's "try this one" predicate
// (run.go awaitCapacity); like saturated it is a RANKING input over a snapshot
// that is stale by construction, so a node that passes may still refuse, and
// the wait loop treats that refusal as one more tick, never as proof.
//
// A node that does not publish saturation is judged on the fields it does
// publish: provablyStartsNow answers "idle slot" for a sheddable contract
// (unknown is never a yes), and !saturated answers it for a band-0 one.
func hasRoom(v NodeView, sheddable bool) bool {
	if saturated(v) {
		return false
	}
	if !sheddable {
		return true
	}
	if v.SaturationKnown {
		return v.IdleSlot
	}
	return provablyStartsNow(v)
}

// hasRoomWithin is hasRoom plus the backlog gate: v would take a NEW dispatch
// right now AND could start it inside `patience` (startsWithinPatience). It is
// the predicate the capacity wait, the deals and re-placement use - a node that
// passes hasRoom but whose backlog outlasts the caller's patience is a node the
// job would sit in for longer than anyone is waiting, and that the delegator
// would then abandon while the node ran it anyway. patience <= 0 is no bound.
func hasRoomWithin(v NodeView, sheddable bool, patience time.Duration) bool {
	if !hasRoom(v, sheddable) {
		return false
	}
	ok, _ := startsWithinPatience(v, patience)
	return ok
}

// provablyStartsNow reports whether v's own numbers prove the next job begins
// executing immediately rather than sitting in the backlog. PROVABLY: an
// unknown is never counted as a yes.
//
// Two ways to prove it, and the first is what keeps this fair across a mixed
// fleet:
//
//   - QueueDepth == 0 — the node holds no job at all, neither running nor
//     queued, so whatever its concurrency limit is (every node has at least
//     one worker) the next job starts. True for a node that publishes no
//     limits whatsoever, which is why an idle pre-0.100.0 node is not demoted
//     below a loaded node that does publish them.
//   - a free worker AND nobody ahead in line: JobsRunning < MaxConcurrentJobs
//     with MaxConcurrentJobs published, and JobsQueued == 0. The queued check
//     is not redundant — a node with a free worker and a non-empty backlog is
//     a node mid-transition, and the honest reading of that snapshot is "not
//     proven".
func provablyStartsNow(v NodeView) bool {
	if v.QueueDepth == 0 {
		return true
	}
	return v.MaxConcurrentJobs > 0 && v.JobsRunning < v.MaxConcurrentJobs && v.JobsQueued == 0
}

// remoteEligible is the §S3 HARD gate — every condition must hold, and each
// one fails toward local:
//
//   - AgentEnabled: the node's operator opted it into the agent lane.
//   - AgentResident: the seat is roster-VERIFIED on the node (advertised from
//     its cached probe), not merely configured.
//   - seatServed: the node's served_models roster, when published, names the
//     agent seat — a stronger check than the cached residency flag above. An
//     unpublished roster (pre-0.113.0 node) is UNKNOWN and never a refusal.
//   - adequate: EstTokens+specReserve <= AgentCtxTokens — the contract provably
//     fits the advertised ceiling with room for the loop itself. An
//     unadvertised ceiling (0) can never fit — "unknown" is not a capacity.
//     The arithmetic lives in fit.go's adequate() so the gate and the
//     smallest-ADEQUATE-seat fit score can never drift apart on what "fits"
//     means.
//   - OutputSchema present (len>0 — bytes, not merely non-nil): the reshaped
//     verifiability requirement (roast delta 3). Remote output merges only
//     after mechanical verification, and the schema is what makes the result
//     mechanically checkable; free-prose acceptance no longer counts.
//   - Depth == 0: only an ORIGIN contract may travel (hop limit 1). The
//     requester's depth is checked here at placement; the receiving node
//     additionally derives effectiveDepth ≥ 1 for whatever arrives.
//   - Layers (ADR 0039, council R5): a node that advertises layer rows is
//     gated by the SAME placement table the local box and the node itself
//     use — eligibility is "the table, run over the node's rows, neither
//     defers nor waits". The residency, roster and ceiling checks above are
//     the implicit single layer's and apply only to a node without rows: a
//     composite node's residency is per seat inside the rows, and its
//     ceiling is whichever layer's window the table picks. A decision that
//     WAITS (the node's long seat would evict its busy pair) is not
//     eligible right now — a capacity condition the wait re-polls, never a
//     dispatch that evicts a remote's busy seat without asking.
func remoteEligible(st Subtask, r NodeView) bool {
	eligible, _, _ := eligibilityVerdict(st, r)
	return eligible
}

// eligibilityVerdict is the SINGLE predicate sequence remoteEligible and
// placement_reason.go's D-105 narration both consume, so the two can never
// diverge on WHY a node is not the one running a subtask (review round 1,
// BLOCKER item 1: the narration used to run its OWN copy of this gate in a
// DIFFERENT order — schema/depth checked LAST instead of early — so a
// schema-less contract was narrated `slow`/`unfit(ctx)` on every remote
// instead of `noschema`, contradicting placement_reason.go's own "read-only
// narration, never re-derives the decision" contract).
//
// eligible is byte-for-byte what remoteEligible has always returned. word is
// the D-105 vocabulary entry for the FIRST disqualifying condition in gate
// order, detail its one-line arithmetic/context — both empty when eligible
// (the caller then applies its own RANKING-based verdict: queue/cap/cold,
// which are about ORDER among eligible seats, never about admission).
//
// Order, exactly as remoteEligible has always checked it:
//
//	AgentEnabled → lease fence (W-14) → output_schema present → origin hop
//	(Depth == 0) → feasibility (W-05) → the layer table (composite) OR
//	residency/served/adequate (plain node).
func eligibilityVerdict(st Subtask, r NodeView) (eligible bool, word, detail string) {
	if !r.AgentEnabled {
		return false, "probe", "agent lane not advertised"
	}
	// A node advertising a held TEXT lease is not a target at all (0.113.16):
	// its card is reserved for a measurement, exactly as Reserved() makes the
	// LOCAL seat a non-target. Before this a leased <node-c> had to STOP its fleet
	// node to keep foreign digests off the card, and every in-flight remote job
	// on it was cut ("<node-c> dropped mid-way", 2026-09-06).
	// leaseFenceReason (W-14, register S-15) is what decides whether the lease
	// is a HARD refusal here — see its own doc for the exclusive/draining/media
	// cases that still fence, and the plain-text-busy case that no longer does.
	if fenced, why := leaseFenceReason(r, &st); fenced {
		return false, "lease", why
	}
	if len(st.Contract.OutputSchema) == 0 {
		return false, "noschema", ""
	}
	if st.Contract.Depth != 0 {
		return false, "hop", fmt.Sprintf("depth %d (only an origin contract may travel — hop limit 1)", st.Contract.Depth)
	}
	// W-05 (register S-03/S-05, INV-5 rider clause (i)): a seat that cannot
	// produce one tool step and a minimal answer within the contract's own
	// effective wall is refused here, naming the arithmetic (eta.go's
	// feasibleFinal). An unknown rate is no opinion — see its own doc.
	if ok, reason := feasibleFinal(st, r); !ok {
		return false, "slow", reason
	}
	if dec, ok := remoteDecision(st, r); ok {
		if dec.Defer || dec.Wait {
			return false, "layer", dec.Reason
		}
		return true, "", ""
	}
	// A named layer (register A-100) can only be served by a node that
	// declares it; a node with no rows would run the contract on its planner
	// seat and never say so.
	if st.Contract.Layer != "" {
		return false, "layer", fmt.Sprintf("layer %s requested; node declares no layers", st.Contract.Layer)
	}
	if !r.AgentResident || !seatServed(r) {
		return false, "probe", "seat not resident on this node's cached roster"
	}
	if !adequate(st, r) {
		return false, "unfit(ctx)", fmt.Sprintf("%d needed > %d advertised", st.EstTokens+specReserve, r.AgentCtxTokens)
	}
	return true, "", ""
}

// remoteDecision runs the placement table over a node's advertised layer rows
// for st — the delegator's half of council R5 (one rule everywhere). ok=false
// for a node without rows (the implicit single layer; today's gate applies).
// The request is the contract's, with the completion budget defaulted: the
// delegator does not know the remote's agent_max_tokens, and the table's
// default (1024) is the conservative side of every seat's real setting. The
// node re-runs the same decision for the dispatched layer with its OWN live
// guards, so a verdict carried in the rows is never the last word.
//
// A contract that already NAMES a layer (the caller's `layer`, register
// A-100) is decided FOR that layer, never re-placed by the free choice: a node
// that does not declare it defers by name and is ineligible for this
// contract, so the dispatch lands only where the requested seat is served.
// Before this the free choice overwrote the caller's layer on the dispatched
// copy, and a request for <node-c>'s fast layer ran on its planner default.
func remoteDecision(st Subtask, r NodeView) (placetable.Decision, bool) {
	if len(r.Layers) == 0 {
		return placetable.Decision{}, false
	}
	layers, live := placetable.FromRows(r.Layers)
	req := placetable.RequestForContract(st.Contract, st.EstTokens, 0)
	if st.Contract.Layer != "" {
		return placetable.DecideOnLayer(req, layers, st.Contract.Layer, live), true
	}
	return placetable.Decide(req, layers, live), true
}

// leaseFenceReason reports whether r's lease is a HARD refusal for remote
// placement (W-14, register S-15) — an EXCLUSIVE or DRAINING hold, gated to
// TEXT-class leases at creation (gpulease.TryAcquire only ever sets either
// flag for class=text, never for class=media) — or a lease the node's own
// verdict reads as long enough (LeaseBusy) that is NOT a plain text
// reservation. That second clause is what keeps a MEDIA render (a training
// run, up to hours) fencing exactly as before: a busy, non-text lease can
// never be Exclusive or Draining by construction, so without this clause it
// would fall straight through to the demotion this function does NOT grant
// it. It also returns the one-word D-105 detail for WHICH hold fenced it —
// read by eligibilityVerdict so the gate and the narration can never name a
// different reason than the one that actually excluded a node.
//
// The ONE case this does not fence: a plain (non-exclusive, non-draining)
// TEXT reservation the node calls busy. Before this it hard-refused on the
// node's DECLARED window alone — 47 measured contracts burned 300 s each on a
// lease whose cards were idle in 10 of them (register S-15) — because `busy`
// says "spoken for until 15:04", not "the cards are working". That case stays
// eligible and is demoted instead (betterRemote's leaseBusyDemoted key).
//
// Per card (GPU routing P7): a node that publishes its leases (NodeView.Leases) is fenced
// only for a contract whose seats ALL sit on a card a fencing lease holds, because the node's
// placement table falls back to a seat whose cards are free. The reason names the lease and
// how many cards it holds. A node that publishes none (an older node) is read exactly as
// before: its one lease block is the whole node.
func leaseFenceReason(r NodeView, st *Subtask) (fenced bool, why string) {
	if len(r.Leases) == 0 {
		switch {
		case r.LeaseExclusive:
			return true, "exclusive"
		case r.LeaseDraining:
			return true, "draining"
		case r.LeaseBusy && !r.LeasedText:
			return true, "busy"
		default:
			return false, ""
		}
	}
	holding := r.leasesHolding(st, func(l LeaseView) bool {
		return l.Exclusive || l.Draining || (l.Busy && !l.Overdue && l.Class != "text")
	})
	if len(holding) == 0 {
		return false, ""
	}
	// The same precedence the one-block rule has: exclusive, then draining, then busy.
	pick := holding[0]
	word := "busy"
	for _, l := range holding {
		switch {
		case l.Exclusive:
			pick, word = l, "exclusive"
		case l.Draining:
			pick, word = l, "draining"
		}
		if word == "exclusive" {
			break
		}
	}
	return true, fmt.Sprintf("%s: %s lease %d on %s", word, pick.Class, pick.Epoch, leaseCardsPhrase(pick))
}

// leaseCardsPhrase says how much of the node a lease holds, for a reason an operator reads.
func leaseCardsPhrase(l LeaseView) string {
	switch len(l.Devices) {
	case 0:
		return "the whole node"
	case 1:
		return "1 card"
	default:
		return fmt.Sprintf("%d cards", len(l.Devices))
	}
}

// leasesHolding returns the node's leases (those pick accepts) that stand between st and
// every seat it could run on, or nil when some seat sits on no such lease: the placement
// table falls back to it. The seats are the node's own rows' agent chain, each with the
// cards the node published for it (placement.RemoteSeatCards), so this is the node's own
// reading of its layout and the rule the local box applies to itself.
//
// Whole-node wherever nothing narrows: a nil contract (the vision and text lanes have none),
// a node that published no layer rows, a long-context contract, a layer the node does not
// declare, and any seat whose cards cannot be placed. A lease that names no cards holds every
// seat. The direction of every doubt is "held".
func (v NodeView) leasesHolding(st *Subtask, pick func(LeaseView) bool) []LeaseView {
	var picked []LeaseView
	for _, l := range v.Leases {
		if pick(l) {
			picked = append(picked, l)
		}
	}
	if len(picked) == 0 || st == nil {
		return picked
	}
	cards, _ := gpuprobe.BuildCards(v.Devices, "")
	seats, ok := placetable.RemoteSeatCards(v.Layers, cards, st.Contract, st.EstTokens)
	if !ok {
		return picked
	}
	var first []LeaseView
	for n, ids := range seats {
		on := leasesOnCards(picked, ids)
		if n == 0 {
			first = on
		}
		if len(on) == 0 {
			return nil
		}
	}
	return first
}

// leasesOnCards are the leases that sit on any of ids. An empty ids is a seat whose cards are
// unknown, which is every card; a lease that names no cards is the whole node.
func leasesOnCards(ls []LeaseView, ids []string) []LeaseView {
	var out []LeaseView
	for _, l := range ls {
		if len(ids) == 0 || len(l.Devices) == 0 || sharesCard(l.Devices, ids) {
			out = append(out, l)
		}
	}
	return out
}

func sharesCard(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}

// leaseBusyDemoted is betterRemote's read of the ONE lease shape remoteEligible
// still admits: LeaseBusy alone. Safe to check without re-deriving the
// exclusive/draining/text conditions, because remoteEligible has already
// excluded every other LeaseBusy shape (leaseFenceReason) before a node ever
// reaches ranking — a lease-busy survivor here is always the plain-text case.
//
// An overdue lease is NOT a LeaseBusy survivor: NodeView decodes it onto
// LeaseOverdue alone, so it is never hard-excluded as a busy non-text lease.
// leaseDemotionRank is what ranks it.
func leaseBusyDemoted(v NodeView) bool { return v.LeaseBusy }

// leaseDemotionRank orders the lease shapes remoteEligible admits, lowest
// first: no demotion (0), a genuinely long lease (1), an overdue lease (2). A
// node whose lease is both reads as overdue, the worse of the two.
//
// A node that publishes its leases is ranked by the ones that stand between THIS contract and
// its seats (leasesHolding): a long or overdue lease on a card the contract does not use says
// nothing about when this contract would start there.
func leaseDemotionRank(v NodeView, st *Subtask) int {
	if len(v.Leases) == 0 {
		switch {
		case v.LeaseOverdue:
			return 2
		case leaseBusyDemoted(v):
			return 1
		default:
			return 0
		}
	}
	switch {
	case len(v.leasesHolding(st, func(l LeaseView) bool { return l.Overdue })) > 0:
		return 2
	case len(v.leasesHolding(st, func(l LeaseView) bool { return l.Busy && !l.Overdue })) > 0:
		return 1
	default:
		return 0
	}
}

// PlaceVision picks the fleet node that runs ONE vision task (vqa / ocr /
// assess_image, 0.116.0) when the caller has decided the work leaves the box
// (route remote, or route auto on a busy local card — that decision is the
// caller's, exactly as localBusy is Place's caller's). It returns ok=false
// when no remote is eligible; the caller then defers (route remote) or runs
// local (route auto) — never a silent local run under route remote.
//
// Eligibility is the vision lane's own gate, not the agent lane's: the node
// must ADVERTISE the lane (ServesVision — an older node never does, so it is
// never a target), it must serve THIS task (ServesVisionTask — a node that
// publishes no vision_tasks serves all three, so an older node stays eligible
// for every task; one that lists a subset is skipped for the rest), and its
// card must not be spoken for (the same LeasedText / LeaseBusy refusals
// remoteEligible applies: a reserved card is a non-target whatever the lane). AgentEnabled, residency, ctx arithmetic and the
// output_schema rule are agent-contract facts and do not apply to an image.
//
// Ranking reuses betterRemote — not-saturated, then a provably free slot,
// then (W-11) a queue-wait tie-break, then queue depth, then GPU utilization,
// ties in roster order — so a vision placement and an agent placement agree
// on which of two nodes is the less loaded one. The seed for W-11's P2C draw
// is minted once per call (PlaceVision carries no wire job id of its own to
// thread through — visionremote's caller is outside this PR's scope): the
// property the draw needs is only that independent CALLS disagree, which a
// fresh seed per call already gives it.
//
// It returns the INDEX into remotes rather than the view, so a caller that
// holds a parallel slice of base URLs (a NodeView carries no address) can
// dispatch to the node it chose without matching on node_id — two
// misconfigured nodes can share one id, and an id is not an address.
func PlaceVision(remotes []NodeView, task string) (int, bool) {
	seed := mintP2CSeed()
	best := -1
	for i, r := range remotes {
		if !visionEligible(r, task) {
			continue
		}
		if best < 0 || betterRemote(seed, nil, 0, r, remotes[best]) {
			best = i
		}
	}
	return best, best >= 0
}

// PlaceText picks the fleet node that runs ONE text task (classify / extract, 0.154.0) when
// the caller has decided the work leaves the box (route remote, or route auto on a busy local
// card). It is PlaceVision with the text lane's own gate: the node must ADVERTISE the lane and
// list THIS task in text_tasks (ServesTextTask: no list means no lane, never "all", so an older
// node is never a target), and its card must not be spoken for. Ranking is betterRemote, as for
// vision; the return is the INDEX into remotes, for the same reason.
func PlaceText(remotes []NodeView, task string) (int, bool) {
	seed := mintP2CSeed()
	best := -1
	for i, r := range remotes {
		if !textEligible(r, task) {
			continue
		}
		if best < 0 || betterRemote(seed, nil, 0, r, remotes[best]) {
			best = i
		}
	}
	return best, best >= 0
}

// textEligible is PlaceText's hard gate: the lane advertised and the task served, the card not
// reserved.
func textEligible(r NodeView, task string) bool {
	return r.ServesTextTask(task) && !r.LeasedText && !r.LeaseBusy
}

// visionEtaBetter is PlaceVision's W-11 key: there is no contract to fit a
// generation term against, so it compares only the node's own queue-wait
// estimate (queueWaitFor — the same one etaFor folds cold+generation onto for
// an agent contract), with the same P2C near-tie draw.
func visionEtaBetter(seed string, candidate, incumbent NodeView) (better, decided bool) {
	c, i := queueWaitFor(candidate), queueWaitFor(incumbent)
	// Two nodes with the SAME estimate stay undecided here and fall through to
	// QueueDepth below, which is what orders the vision lane's common case: a
	// roster where nobody publishes a wall, so every estimate is 0.
	if c == i {
		return false, false
	}
	cd, id := etaDrawn(seed, candidate.NodeID, c), etaDrawn(seed, incumbent.NodeID, i)
	if cd != id {
		return cd < id, true
	}
	return candidate.NodeID < incumbent.NodeID, candidate.NodeID != incumbent.NodeID
}

// visionEligible is PlaceVision's hard gate: the lane advertised and the task
// served, the card not reserved.
func visionEligible(r NodeView, task string) bool {
	return r.ServesVisionTask(task) && !r.LeasedText && !r.LeaseBusy
}

// seatServed: true when the node publishes no roster (unknown) or when the
// roster names the agent seat (case-insensitive, like swapclient.Roster.Serves).
func seatServed(v NodeView) bool {
	if len(v.ServedModels) == 0 {
		return true
	}
	for _, m := range v.ServedModels {
		if strings.EqualFold(m, v.AgentSeat) {
			return true
		}
	}
	return false
}

// LocalBusy reports whether the machine-wide GPU lease is currently held —
// either class: a media render in flight or a text reservation both mean the
// local GPU is spoken for, which is Place's trigger for considering remotes.
//
// Mechanism (deliberately the LEAST invasive one gpulease offers): resolve
// the lease dir through gpulease.LeaseDir — THE one resolver; a second
// resolution order is how the lease silently splits, per
// docs/systems/gpu-lease.md — then read it with gpulease.InspectDir, the
// read-only inspection path built for consumers that do not own a Manager
// (the vision gate uses the same one). No Manager is opened (Open probes
// writability and mkdirs), nothing is acquired, no epoch is bumped: a
// placement probe must never CONTEND for the card it is asking about.
// InspectDir already applies the full reclaim rule, so a crashed holder's
// stale lease reads as not held.
//
// gpuLockPath/stateDir are the config's gpu_lock_path/state_dir, threaded by
// the caller (the plan sketched a zero-arg LocalBusy, but resolving the lease
// dir WITHOUT the config's overrides would re-create the split-lease defect
// on any box that sets them). Any resolution failure reads as NOT busy:
// Place then keeps the work local, which is always the safe placement.
func LocalBusy(gpuLockPath, stateDir string) bool { return LocalLease(gpuLockPath, stateDir).Held }

// LocalLease is LocalBusy's underlying read: the machine-wide lease's Info,
// resolved and inspected exactly as LocalBusy documents (never acquired, never
// contended). Any resolution failure returns the zero Info — Held=false, the
// same fail-toward-idle direction. Callers that need the CLASS or the holder
// (Reserved, the wait/defer path in run.go) use this; a caller that only asks
// "is the card spoken for?" keeps the boolean.
func LocalLease(gpuLockPath, stateDir string) gpulease.Info {
	dir, err := gpulease.LeaseDir(gpuLockPath, stateDir)
	if err != nil {
		return gpulease.Info{}
	}
	// With the inferred scope of a legacy whole-node lease filled in (modelaffinity.PeekLease):
	// the narrowing the per-seat readers apply needs it, and a lease with no evidence reads
	// exactly as InspectDir reads it. An inspection: the delegator's routing and offload_status
	// read it, and neither writes the sidecar or the ledger; the load gate does that.
	return modelaffinity.PeekLease(dir)
}

// LocalBusyFor is LocalBusy asked on behalf of a seat on the cards pins name: true only
// when a live lease sits on one of them. A render on card 2 leaves the card-0 seat idle; an
// unknown pin is every card, so the answer is LocalBusy's.
func LocalBusyFor(gpuLockPath, stateDir string, pins []string) bool {
	return LocalLeaseFor(gpuLockPath, stateDir, pins).Held
}

// LocalLeaseFor is LocalLease narrowed to the leases that sit on the cards pins name (a
// layer seat's Device, split by DeviceList): a render on card 2 is not a lease on the card-0
// seat. No pins is an unknown seat, which is every card, so the answer is LocalLease's.
func LocalLeaseFor(gpuLockPath, stateDir string, pins []string) gpulease.Info {
	return modelaffinity.ScopeToPins(LocalLease(gpuLockPath, stateDir), pins)
}

// LeaseForContract narrows a read of the local lease to what stands between a contract and
// a local seat (plan P4): the delegator is busy for a contract only when EVERY local agent
// seat it could run on sits on a held card, because the placement table falls back to the
// next layer whose cards are free. The seats are placement.AgentChain's, the same list the
// table walks, so the two cannot disagree. A contract that names its layer is asked of that
// layer's seat alone. The result is the first seat's leases when every seat is held (the
// holder a defer names), and the zero Info as soon as one seat is free of every lease.
//
// Unchanged, whole-node: a box that declares no layers, a long-context contract (it runs on
// a long seat the agent chain does not describe), and a contract no agent seat's window can
// hold. Nothing narrows on a guess.
func LeaseForContract(cfg config.Config, info gpulease.Info, c core.AgentContract) gpulease.Info {
	// The chain reading lives in placement (plan P7): the fleet node reads the same rule at
	// dispatch, and the delegator reads a remote's version of it off that node's rows.
	return placetable.LeasesAgainstContract(cfg, info, c, nil)
}

// ReservedFor is Reserved asked on behalf of a seat on the cards pins name.
func ReservedFor(info gpulease.Info, pins []string) bool {
	return Reserved(modelaffinity.ScopeToPins(info, pins))
}

// FencedFor is Fenced asked on behalf of a seat on the cards pins name.
func FencedFor(info gpulease.Info, pins []string) (bool, string) {
	return Fenced(modelaffinity.ScopeToPins(info, pins))
}

// ForeignFenceFor is ForeignFence asked on behalf of a seat on the cards pins name.
func ForeignFenceFor(info gpulease.Info, pins []string) (bool, string) {
	return ForeignFence(modelaffinity.ScopeToPins(info, pins))
}

// Reserved reports whether info is a held TEXT-class lease: a benchmark, eval
// or measured run has reserved the cards (`gpu reserve --class text`), so the
// local seat is not a placement target at all for route auto/spread — not
// merely a less-preferred one. Before 0.113.14 a held lease only steered
// placement toward a remote and the contract still ran locally when no remote
// qualified, which is exactly how three foreign contracts loaded a reserved
// two-card seat mid-measurement (2026-09-05 08:04–08:09).
//
// A media holder is deliberately NOT "reserved" here. Renders are arbitrated at
// the model-affinity gate (ADR 0026), which waits for the render and then admits
// the load; turning that into a placement defer would change every single-box
// render for no measured reason. Media keeps steering (LocalBusy) and nothing
// more.
//
// An INHERITED lease exempts the caller, exactly as the affinity gate's rule
// (modelaffinity.gpuwait): `gpu reserve --class text -- local-offload delegate …`
// runs the delegate as the holder's child with GPU_LEASE_EPOCH set, and the
// holder's own measured work must not be refused by its own reservation. The
// epoch is compared, never presence-checked, so a stale variable from a lease
// since handed on exempts nothing; the holder's PID is deliberately not an
// exemption (the MCP server holds leases and serves foreign calls in one
// process).
//
// Asked of EACH live lease (Info.Each): with card-scoped leases several are live at once,
// Info describes only the lowest, and the exemption is per lease.
func Reserved(info gpulease.Info) bool {
	for _, l := range info.Each() {
		if l.Held && l.Class == gpulease.ClassText && !inheritedLease(l) {
			return true
		}
	}
	return false
}

// Fenced reports whether a lease REFUSES A NEW RUN on the local seat, and names
// the fence for the note that has to explain it. It is the placement-side reading
// of modelaffinity.BlocksNewRun — the admission rule the run would meet — and it
// delegates to that function rather than restating it, so the two can never drift
// into a placement that dials a seat its own gate will refuse.
//
// Three holds fence: an EXCLUSIVE text lease (the holder cleared the cards and a
// model loaded now would land on top of a measurement), a DRAINING text lease
// (the seat is cordoned while in-flight work finishes), and a MEDIA lease (a
// render owns the VRAM). A plain text reservation does NOT fence: it steers
// placement (Reserved) but the affinity gate still admits the load, so treating
// it as a fence here would refuse work the box can actually do.
//
// Fenced is what a RETRY must consult. Register D-94 (2026-09-14): a retry was
// placed on the local seat under an exclusive lease, waited the whole
// `gpu-lease timeout after 5m0s` at the cordon and deferred as capacity, while an
// idle remote sat unused. The verdict was on disk before the dial.
//
// A retry asks Reserved after Fenced as well (register C-81), for the one hold Fenced leaves alone:
// a plain reservation does not refuse a run at the gate, but a first placement on route auto or
// spread never takes the seat under one, and neither may a retry.
//
// With card-scoped leases the question is asked of each live lease (Info.Each) and the
// reason names the first one that fences.
func Fenced(info gpulease.Info) (bool, string) {
	for _, l := range info.Each() {
		if !modelaffinity.BlocksNewRun(l) {
			continue
		}
		switch {
		case l.Class == gpulease.ClassMedia:
			return true, "media render holds the cards"
		case l.Exclusive:
			return true, "exclusive text lease (the holder cleared the cards)"
		default:
			return true, "draining text lease (the seat is cordoned; no new run is admitted)"
		}
	}
	return false, ""
}

// ForeignFence is Fenced asked on behalf of a caller that is NOT the holder:
// true only when the fence would refuse THIS process's next run. It exists
// because "the seat is fenced" and "the seat is fenced against me" are different
// questions, and a lane that routes work off the box must ask the second one.
//
// The holder's own child is exempt for exactly the reason Reserved documents:
// `gpu reserve --drain --unload-seat -- <session>` runs the delegate under
// GPU_LEASE_EPOCH, and the measured work the lease was taken FOR must keep
// running on the cards it cleared — routing it to a fleet node would defeat the
// reservation. The epoch is compared, never presence-checked, so a stale variable
// from a lease since handed on exempts nothing.
//
// Register D-110: offload_review_diff built its local loop unconditionally, so a
// review under a peer's lease waited the whole cordon bound and was filed as a
// capacity defer for the lease's entire length. The verdict was on disk before
// the dial — the same sentence D-94 wrote about the retry path.
//
// The exemption is PER LEASE. With card-scoped leases several are live at once; a child of
// lease A is exempt from A's fence and from no other lease's, so one device lease's child
// cannot walk through the fence another lease holds on other cards. (Until the consumers
// compare device sets, plan P4, a foreign lease fences whatever its cards: over-fencing,
// never under-fencing.)
func ForeignFence(info gpulease.Info) (bool, string) {
	for _, l := range info.Each() {
		if inheritedLease(l) {
			continue
		}
		if fenced, why := Fenced(l); fenced {
			return true, why
		}
	}
	return false, ""
}

// inheritedLease reports whether this process runs under the ONE lease info
// describes: GPU_LEASE_EPOCH (threaded to children by gpu reserve and the
// pipeline's ambient lease env) equals that lease's epoch. Callers with several
// live leases walk Info.Each and ask it of each; "inside any live lease" is not an
// exemption from every other lease's fence.
func inheritedLease(info gpulease.Info) bool {
	raw := strings.TrimSpace(os.Getenv("GPU_LEASE_EPOCH"))
	if raw == "" {
		return false
	}
	epoch, err := strconv.ParseUint(raw, 10, 64)
	return err == nil && epoch != 0 && epoch == info.Epoch
}

// HolderLine names a lease holder for a placement reason: class, pid, the
// reason/origin the holder stamped (when it did), and the expiry — what the
// deferred caller needs to decide whether to wait, route elsewhere, or ask.
func HolderLine(info gpulease.Info) string {
	var b strings.Builder
	fmt.Fprintf(&b, "gpu lease class=%s epoch=%d pid=%d", info.Class, info.Epoch, info.PID)
	if info.Reason != "" {
		fmt.Fprintf(&b, " reason=%q", info.Reason)
	}
	if info.Origin != "" {
		fmt.Fprintf(&b, " origin=%q", info.Origin)
	}
	if !info.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, " expires=%s", info.ExpiresAt.Local().Format("15:04:05"))
	}
	return b.String()
}
