// placement_reason.go: D-105 (PR-5 item 8, the operator's "one word why") —
// placement_reason names EVERY reachable remote considered for a subtask
// with a ONE-WORD verdict and the number behind it, so an operator reading
// one line can tell "the fleet chose correctly" from "this node was
// wrongly passed over" without re-deriving the gate by hand.
//
// Vocabulary: chosen | queue | cap | slow | lease | cold | probe | hop |
// unfit(ctx) | noschema | layer. Each word names the FIRST reason (in the
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
//	slow       feasibleFinal excluded it: its fitted final cannot clear the
//	           floor within its own effective wall (W-05)
//	layer      a composite node's placement table refused or would wait
//	           (ADR 0039); detail is the table's own Reason
//	unfit(ctx) the contract's estimate + reserve does not fit the advertised
//	           ceiling
//	queue      saturated(): the node's own admission ceiling says the next
//	           dispatch is refused right now
//	cap        W-06: headroom is 0 — this DEAL has already committed as many
//	           subtasks to it as max_concurrent_jobs − jobs_running allows
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
)

// oneWordVerdict is st's placement verdict for v (dial base `base`) against
// one deal's state: "chosen" when base == chosenBase, else
// eligibilityVerdict's word/detail when v is not eligible at all, else the
// ranking-based demotion (queue/cap) or "cold" when v was simply outranked.
// dealtSoFar is however many subtasks THIS deal has already committed to
// base, read at the moment v was considered (never mutated here).
func oneWordVerdict(st Subtask, v NodeView, base, chosenBase string, dealtSoFar int) string {
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
	if headroom(v) <= dealtSoFar {
		left := headroom(v) - dealtSoFar
		if left < 0 {
			left = 0
		}
		return fmt.Sprintf("cap (%d/%d running, %d headroom)", v.JobsRunning, v.MaxConcurrentJobs, left)
	}
	return "cold (outranked)"
}

// chosenVerdictDetail renders the winner's own eta breakdown ("eta 41 s
// (cold 15 + 26 gen)"), or just the bare word when the seat publishes no
// usable rate — the same "unknown rate, no opinion" reading etaFor follows.
func chosenVerdictDetail(st Subtask, v NodeView) string {
	eta, ok := etaFor(st, v)
	if !ok {
		return "(no rate published)"
	}
	policy, _, wallSec, known := seatWallFor(st, v)
	if !known || wallSec <= 0 {
		return fmt.Sprintf("eta %.0f s", eta)
	}
	cold := fitColdSec(policy, v)
	gen := eta - cold - queueWaitFor(v)
	if gen < 0 {
		gen = 0
	}
	return fmt.Sprintf("eta %.0f s (cold %.0f + %.0f gen)", eta, cold, gen)
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
func placementVerdictLine(st Subtask, views []NodeView, bases []string, chosenBase string, dealtSoFar map[string]int, failed map[string]string) string {
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
		parts = append(parts, fmt.Sprintf("%s: %s", name, oneWordVerdict(st, v, base, chosenBase, dealtSoFar[base])))
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
