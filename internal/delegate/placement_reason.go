// placement_reason.go: D-105 (PR-5 item 8, the operator's "one word why") —
// placement_reason names EVERY reachable remote considered for a subtask
// with a ONE-WORD verdict and the number behind it, so an operator reading
// one line can tell "the fleet chose correctly" from "this node was
// wrongly passed over" without re-deriving the gate by hand.
//
// Vocabulary: chosen | queue | backlog | cap | slow | lease | cold | probe |
// hop | unfit(ctx) | noschema | layer. Each word names the FIRST reason (in the
// EXACT order gate.go's eligibilityVerdict checks it — see that function's
// own doc; the two are read-only narration over ONE predicate sequence and
// cannot diverge) a node is not the one running the subtask:
//
//	probe      AgentEnabled false, seat not resident/served on this node's
//	           cached roster, OR the node's health probe itself failed —
//	           three shapes of "this node did not prove it can serve the
//	           seat", the third one carrying the dial/cache detail
//	lease      leaseFenceReason: an exclusive or draining hold, or a busy
//	           lease that is not a plain text reservation (fenced — W-14)
//	noschema   the CONTRACT carries no output_schema — a subtask-level fact,
//	           true for every node alike, named once per node for symmetry
//	hop        the CONTRACT's depth != 0 (only an origin contract may
//	           travel — hop limit 1) — a subtask-level fact, like noschema
//	slow       feasibleFinal excluded it: one tool step and a minimal answer
//	           do not fit the contract's effective wall at its rate (W-05)
//	layer      a composite node's placement table refused or would wait
//	           (ADR 0039); detail is the table's own Reason
//	unfit(ctx) the contract's estimate + reserve does not fit the advertised
//	           ceiling
//	queue      saturated(): the node's own admission ceiling says the next
//	           dispatch is refused right now
//	backlog    ADR 0063: the node cannot START this contract inside the wait the
//	           caller gave it - printed as "backlog (a new job would wait ~444 s
//	           to start (<arithmetic>), past the 300 s this contract will wait
//	           for a start)". A placement FEASIBILITY refusal in the same class
//	           as slow (feasibleFinal), never a preference for a faster seat: the
//	           capacity wait re-reads the node every tick and asks it the moment
//	           its backlog fits
//	cap        W-06: this DEAL has already committed as many subtasks to the
//	           node as its headroom (max_concurrent_jobs − jobs_running)
//	           allows — printed as "cap (running/max running, headroom N,
//	           dealt N)"
//	cold       eligible, has headroom, not saturated — simply outranked by a
//	           better candidate (W-11's ranking), named "cold" because the
//	           overwhelmingly common reason one otherwise-adequate seat loses
//	           to another is a worse (colder/slower) expected completion
//	chosen     the one that ran it, with its own eta breakdown
//
// This file never changes ELIGIBILITY — it is a read-only narration over
// gate.go/fit.go/eta.go's existing decisions, called after placement, not
// consulted by it. The `eligible`/`word`/`detail` triple for every
// EXCLUDING reason comes from gate.go's eligibilityVerdict, the SAME
// function remoteEligible calls — this file adds only the RANKING-based
// words (queue/cap/cold) for a node eligibilityVerdict already admitted.
package delegate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// patienceFn answers, for contract c on node v, how long the caller will wait for a job to START
// there and, when something shorter than the contract's own poll budget set that (the time the
// call has left, ADR 0073), the clause a reason prints behind the gate's arithmetic. The runner's
// patience method is the gate's own; the narration takes the same function so the two read one
// number and cannot diverge.
type patienceFn func(c core.AgentContract, v NodeView) (time.Duration, string)

// ownPatience is the contract's poll budget, unclamped: what a caller with no call deadline gets.
func ownPatience(c core.AgentContract, v NodeView) (time.Duration, string) { return patienceFor(c, v), "" }

// oneWordVerdict is st's placement verdict for v (dial base `base`) against
// one deal's state: "chosen" when base == chosenBase, else
// eligibilityVerdict's word/detail when v is not eligible at all, else the
// ranking-based demotion (queue/cap) or "cold" when v was simply outranked.
// dealtSoFar is however many subtasks THIS deal has already committed to
// base, read at the moment v was considered (never mutated here).
func oneWordVerdict(st Subtask, v NodeView, base, chosenBase string, dealtSoFar int) string {
	return oneWordVerdictWith(st, v, base, chosenBase, dealtSoFar, ownPatience)
}

// oneWordVerdictWith is oneWordVerdict judged against the patience the gate used (a nil
// patienceFn is the contract's own poll budget).
func oneWordVerdictWith(st Subtask, v NodeView, base, chosenBase string, dealtSoFar int, patience patienceFn) string {
	if patience == nil {
		patience = ownPatience
	}
	if base != "" && base == chosenBase {
		return "chosen " + chosenVerdictDetail(st, v)
	}
	if eligible, word, detail := eligibilityVerdict(st, v); !eligible {
		if detail == "" {
			return word
		}
		return fmt.Sprintf("%s (%s)", word, detail)
	}
	if saturated(v) {
		if v.MaxQueueDepth > 0 {
			return fmt.Sprintf("queue (%d/%d queue_depth)", v.QueueDepth, v.MaxQueueDepth)
		}
		return "queue (saturation.high)"
	}
	p, clamp := patience(st.Contract, v)
	if ok, why := startsWithinPatience(v, p); !ok {
		return "backlog (" + why + clamp + ")"
	}
	if headroom(v) <= dealtSoFar {
		return fmt.Sprintf("cap (%d/%d running, headroom %d, dealt %d)", v.JobsRunning, v.MaxConcurrentJobs, headroom(v), dealtSoFar)
	}
	return "cold (outranked)"
}

// chosenVerdictDetail renders the winner's own eta breakdown ("eta 41 s
// (cold 15 + 26 gen)"), or just the bare word when the seat publishes no
// usable rate - the same "unknown rate, no opinion" reading etaParts follows.
// The figures are etaParts' own terms: the total is cold + wait + gen, and the
// breakdown names the cold load and the reference generation (the node's wait is
// the remainder), so what is printed is exactly what the ranking compared.
func chosenVerdictDetail(st Subtask, v NodeView) string {
	cold, wait, gen, ok := etaParts(st, v)
	if !ok {
		return "(no rate published)"
	}
	return fmt.Sprintf("eta %.0f s (cold %.0f + %.0f gen)", cold+wait+gen, cold, gen)
}

// placementVerdictLine is D-105's full listing: every base in views, in
// roster order, named by its NodeID (falling back to the dial base when a
// node publishes none) with oneWordVerdict's judgment — PLUS every base in
// `failed` (review round 1, BLOCKER item 2: a dead/unreachable remote is
// still CONFIGURED and still worth an operator's attention, and was
// silently absent from every placement_reason before this, making the
// documented `probe` verdict dead code). failed's value is whatever
// probeRemotes/recentlyDead already produced — a live dial error, or the
// negative cache's own "<why> (cached Ns ago, re-dial in Ms)" — so the same
// arithmetic that decides whether to re-dial is what an operator reads
// here. Empty when there is nothing to report at all.
func placementVerdictLine(st Subtask, views []NodeView, bases []string, chosenBase string, dealtSoFar map[string]int, failed map[string]string, patience patienceFn) string {
	if len(views) == 0 && len(failed) == 0 {
		return ""
	}
	parts := make([]string, 0, len(views)+len(failed))
	for j, v := range views {
		base := bases[j]
		name := v.NodeID
		if name == "" {
			name = base
		}
		parts = append(parts, fmt.Sprintf("%s: %s", name, oneWordVerdictWith(st, v, base, chosenBase, dealtSoFar[base], patience)))
	}
	deadBases := make([]string, 0, len(failed))
	for base := range failed {
		deadBases = append(deadBases, base)
	}
	sort.Strings(deadBases) // map order is not stable; the line must read the same every call
	for _, base := range deadBases {
		parts = append(parts, fmt.Sprintf("%s: probe (%s)", base, failed[base]))
	}
	return strings.Join(parts, "; ")
}
