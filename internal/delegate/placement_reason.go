// placement_reason.go: D-105 (PR-5 item 8, the operator's "one word why") —
// placement_reason names EVERY reachable remote considered for a subtask
// with a ONE-WORD verdict and the number behind it, so an operator reading
// one line can tell "the fleet chose correctly" from "this node was
// wrongly passed over" without re-deriving the gate by hand.
//
// Vocabulary: chosen | queue | cap | slow | lease | cold | probe | unfit(ctx)
// | noschema. Each word names the FIRST reason (in gate order) a node is not
// the one running the subtask:
//
//	lease      leaseFences: an exclusive or draining hold, or a busy lease
//	           that is not a plain text reservation (fenced — W-14)
//	slow       feasibleFinal excluded it: its fitted final cannot clear the
//	           floor within its own effective wall (W-05)
//	unfit(ctx) the contract's estimate + reserve does not fit the advertised
//	           ceiling
//	noschema   the CONTRACT carries no output_schema — a subtask-level fact,
//	           true for every node alike, named once per node for symmetry
//	queue      saturated(): the node's own admission ceiling says the next
//	           dispatch is refused right now
//	cap        W-06: headroom is 0 — this DEAL has already committed as many
//	           subtasks to it as max_concurrent_jobs − jobs_running allows
//	cold       eligible, has headroom, not saturated — simply outranked by a
//	           better candidate (W-11's ranking), named "cold" because the
//	           overwhelmingly common reason one otherwise-adequate seat loses
//	           to another is a worse (colder/slower) expected completion
//	probe      the node's health read failed or never returned — reachable
//	           only in the sense that it is CONFIGURED, not that it answered
//	chosen     the one that ran it, with its own eta breakdown
//
// This file never changes ELIGIBILITY — it is a read-only narration over
// gate.go/fit.go/eta.go's existing decisions, called after placement, not
// consulted by it.
package delegate

import (
	"fmt"
	"strings"
)

// oneWordVerdict is st's placement verdict for v (dial base `base`) against
// one deal's state: "chosen" when base == chosenBase, else the first
// disqualifying/demoting reason in gate order, or "cold" when v was simply
// outranked. dealtSoFar is however many subtasks THIS deal has already
// committed to base, read at the moment v was considered (never mutated
// here).
func oneWordVerdict(st Subtask, v NodeView, base, chosenBase string, dealtSoFar int) string {
	if base != "" && base == chosenBase {
		return "chosen " + chosenVerdictDetail(st, v)
	}
	if !v.AgentEnabled {
		return "probe (agent lane not advertised)"
	}
	if v.LeaseExclusive {
		return "lease (exclusive)"
	}
	if v.LeaseDraining {
		return "lease (draining)"
	}
	if v.LeaseBusy && !v.LeasedText {
		return "lease (busy)"
	}
	if ok, reason := feasibleFinal(st, v); !ok {
		return "slow (" + reason + ")"
	}
	if !adequate(st, v) {
		return fmt.Sprintf("unfit(ctx) (%d needed > %d advertised)", st.EstTokens+specReserve, v.AgentCtxTokens)
	}
	if len(st.Contract.OutputSchema) == 0 {
		return "noschema"
	}
	if saturated(v) {
		if v.MaxQueueDepth > 0 {
			return fmt.Sprintf("queue (%d/%d queue_depth)", v.QueueDepth, v.MaxQueueDepth)
		}
		return "queue (saturation.high)"
	}
	if headroom(v) <= dealtSoFar {
		return fmt.Sprintf("cap (%d/%d running)", v.JobsRunning, v.MaxConcurrentJobs)
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
// node publishes none) with oneWordVerdict's judgment. Empty when views is
// empty — a caller appends it only when it produced something.
func placementVerdictLine(st Subtask, views []NodeView, bases []string, chosenBase string, dealtSoFar map[string]int) string {
	if len(views) == 0 {
		return ""
	}
	parts := make([]string, 0, len(views))
	for j, v := range views {
		base := bases[j]
		name := v.NodeID
		if name == "" {
			name = base
		}
		parts = append(parts, fmt.Sprintf("%s: %s", name, oneWordVerdict(st, v, base, chosenBase, dealtSoFar[base])))
	}
	return strings.Join(parts, "; ")
}
