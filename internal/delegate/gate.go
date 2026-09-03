// gate.go is the placement decision (§S3 as reshaped by roast deltas 3+5):
// a pure, exhaustively-tested function over NodeViews. Quality-first is the
// whole design: an idle local node ALWAYS runs the work (Place never
// load-balances for speed), and a remote node is even ELIGIBLE only when the
// contract is mechanically verifiable and provably fits the remote seat's
// context ceiling with room to run.

package delegate

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
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
func EstimateTokens(c core.AgentContract) int {
	chars := len(c.Goal) + len(c.OutputSchema)
	for _, d := range c.Context {
		chars += len(d.Name) + len(d.Text)
	}
	for _, a := range c.Acceptance {
		chars += len(a)
	}
	return (chars + 2) / 3 // ceil — a remainder rounds UP, same conservative direction
}

// Place decides which node runs st. The rule, exactly as reshaped:
//
//   - localBusy false ⇒ LOCAL, unconditionally. An idle local node always
//     wins — delegation exists to keep work flowing while the local GPU is
//     occupied, never to chase throughput on a weaker seat (quality-first,
//     operator verbatim: "not a race about speed").
//   - localBusy true ⇒ the best remote that passes the hard gate, ranked by
//     capacity then by QueueDepth (ties: first listed, so the caller's roster
//     order is the stable preference order — see betterRemote). No remote
//     passes ⇒ LOCAL regardless — queued-local beats ineligible-remote every
//     time.
//
// Place is pure: it never probes anything. Callers build the inputs from
// FetchNodeView + LocalBusy.
func Place(st Subtask, local NodeView, remotes []NodeView, localBusy bool) NodeView {
	if !localBusy {
		return local
	}
	var best NodeView
	found := false
	for _, r := range remotes {
		if !remoteEligible(st, r) {
			continue
		}
		if !found || betterRemote(r, best) {
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
// Four ordered keys, all boolean-or-int, so the ordering is a total preorder
// and cannot be intransitive on a MIXED fleet (some nodes publish capacity,
// some do not — a key like "more free slots" is uncomparable across those and
// would need a number invented for the silent half):
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
func betterRemote(candidate, incumbent NodeView) bool {
	if c, i := saturated(candidate), saturated(incumbent); c != i {
		return i // candidate wins only when the incumbent is the saturated one
	}
	if c, i := provablyStartsNow(candidate), provablyStartsNow(incumbent); c != i {
		return c
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
	return v.MaxQueueDepth > 0 && v.QueueDepth >= v.MaxQueueDepth
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
func remoteEligible(st Subtask, r NodeView) bool {
	return r.AgentEnabled &&
		r.AgentResident &&
		seatServed(r) &&
		adequate(st, r) &&
		len(st.Contract.OutputSchema) > 0 &&
		st.Contract.Depth == 0
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
func LocalBusy(gpuLockPath, stateDir string) bool {
	dir, err := gpulease.LeaseDir(gpuLockPath, stateDir)
	if err != nil {
		return false
	}
	return gpulease.InspectDir(dir).Held
}
