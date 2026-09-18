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

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
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
	if !localBusy {
		return local
	}
	var best NodeView
	found := false
	for _, r := range remotes {
		if !remoteEligible(st, r) {
			continue
		}
		if !found || betterRemote(seed, &st, r, best) {
			best, found = r, true
		}
	}
	if !found {
		return local
	}
	return best
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
func betterRemote(seed string, st *Subtask, candidate, incumbent NodeView) bool {
	// Key 0 (W-14, register S-15): a node still eligible under a lease-busy
	// verdict loses to any node that is not — "ranked last" means it does not
	// even get to compete on saturation or queue depth against a clean node.
	// See leaseBusyDemoted for why checking LeaseBusy alone is safe here: the
	// gate has already excluded every OTHER lease-busy shape (exclusive,
	// draining, non-text), so a lease-busy survivor is always the one case
	// this key exists to demote.
	if c, i := leaseBusyDemoted(candidate), leaseBusyDemoted(incumbent); c != i {
		return i // candidate wins only when the incumbent is the demoted one
	}
	if c, i := saturated(candidate), saturated(incumbent); c != i {
		return i // candidate wins only when the incumbent is the saturated one
	}
	if c, i := provablyStartsNow(candidate), provablyStartsNow(incumbent); c != i {
		return c
	}
	if st != nil {
		if better, decided := betterRanked(seed, inferKind(*st), rankFor(*st, candidate), rankFor(*st, incumbent)); decided {
			return better
		}
	} else if better, decided := visionEtaBetter(seed, candidate, incumbent); decided {
		return better
	}
	if candidate.QueueDepth != incumbent.QueueDepth {
		return candidate.QueueDepth < incumbent.QueueDepth
	}
	if candidate.GpuUtilKnown && incumbent.GpuUtilKnown {
		return candidate.GpuUtilPct < incumbent.GpuUtilPct
	}
	return false
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
	// LOCAL seat a non-target. Before this a leased Lenovo had to STOP its fleet
	// node to keep foreign digests off the card, and every in-flight remote job
	// on it was cut ("Lenovo dropped mid-way", 2026-09-06).
	// leaseFenceReason (W-14, register S-15) is what decides whether the lease
	// is a HARD refusal here — see its own doc for the exclusive/draining/media
	// cases that still fence, and the plain-text-busy case that no longer does.
	if fenced, why := leaseFenceReason(r); fenced {
		return false, "lease", why
	}
	if len(st.Contract.OutputSchema) == 0 {
		return false, "noschema", ""
	}
	if st.Contract.Depth != 0 {
		return false, "hop", fmt.Sprintf("depth %d (only an origin contract may travel — hop limit 1)", st.Contract.Depth)
	}
	// W-05 (register S-03/S-05, INV-5 rider clause (i)): a seat whose fitted
	// final cannot clear seatrate.FinalBudgetFloor within its own effective
	// wall is refused here, naming the arithmetic (fit.go's feasibleFinal).
	// An unknown rate is no opinion — see its own doc.
	if ok, reason := feasibleFinal(st, r); !ok {
		return false, "slow", reason
	}
	if dec, ok := remoteDecision(st, r); ok {
		if dec.Defer || dec.Wait {
			return false, "layer", dec.Reason
		}
		return true, "", ""
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
func remoteDecision(st Subtask, r NodeView) (placetable.Decision, bool) {
	if len(r.Layers) == 0 {
		return placetable.Decision{}, false
	}
	layers, live := placetable.FromRows(r.Layers)
	return placetable.Decide(placetable.RequestForContract(st.Contract, st.EstTokens, 0), layers, live), true
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
func leaseFenceReason(r NodeView) (fenced bool, why string) {
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

// leaseBusyDemoted is betterRemote's read of the ONE lease shape remoteEligible
// still admits: LeaseBusy alone. Safe to check without re-deriving the
// exclusive/draining/text conditions, because remoteEligible has already
// excluded every other LeaseBusy shape (leaseFenceReason) before a node ever
// reaches ranking — a lease-busy survivor here is always the plain-text case.
func leaseBusyDemoted(v NodeView) bool { return v.LeaseBusy }

// PlaceVision picks the fleet node that runs ONE vision task (vqa / ocr /
// assess_image, 0.116.0) when the caller has decided the work leaves the box
// (route remote, or route auto on a busy local card — that decision is the
// caller's, exactly as localBusy is Place's caller's). It returns ok=false
// when no remote is eligible; the caller then defers (route remote) or runs
// local (route auto) — never a silent local run under route remote.
//
// Eligibility is the vision lane's own gate, not the agent lane's: the node
// must ADVERTISE the lane (ServesVision — an older node never does, so it is
// never a target), and its card must not be spoken for (the same LeasedText /
// LeaseBusy refusals remoteEligible applies: a reserved card is a non-target
// whatever the lane). AgentEnabled, residency, ctx arithmetic and the
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
func PlaceVision(remotes []NodeView) (int, bool) {
	seed := mintP2CSeed()
	best := -1
	for i, r := range remotes {
		if !visionEligible(r) {
			continue
		}
		if best < 0 || betterRemote(seed, nil, r, remotes[best]) {
			best = i
		}
	}
	return best, best >= 0
}

// visionEtaBetter is PlaceVision's W-11 key: there is no contract to fit a
// generation term against, so it compares only the node's own queue-wait
// estimate (queueWaitFor — the same one etaFor folds cold+generation onto for
// an agent contract), with the same P2C near-tie draw.
func visionEtaBetter(seed string, candidate, incumbent NodeView) (better, decided bool) {
	c, i := queueWaitFor(candidate), queueWaitFor(incumbent)
	if c == i {
		return false, false
	}
	return etaPreferred(seed, c, i), true
}

// visionEligible is PlaceVision's hard gate: the lane advertised, the card
// not reserved.
func visionEligible(r NodeView) bool {
	return r.ServesVision() && !r.LeasedText && !r.LeaseBusy
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
	return gpulease.InspectDir(dir)
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
func Reserved(info gpulease.Info) bool {
	return info.Held && info.Class == gpulease.ClassText && !inheritedLease(info)
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
func Fenced(info gpulease.Info) (bool, string) {
	if !modelaffinity.BlocksNewRun(info) {
		return false, ""
	}
	switch {
	case info.Class == gpulease.ClassMedia:
		return true, "media render holds the cards"
	case info.Exclusive:
		return true, "exclusive text lease (the holder cleared the cards)"
	default:
		return true, "draining text lease (the seat is cordoned; no new run is admitted)"
	}
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
func ForeignFence(info gpulease.Info) (bool, string) {
	if inheritedLease(info) {
		return false, ""
	}
	return Fenced(info)
}

// inheritedLease reports whether this process runs under the lease info
// describes: GPU_LEASE_EPOCH (threaded to children by gpu reserve and the
// pipeline's ambient lease env) equals the held epoch.
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
