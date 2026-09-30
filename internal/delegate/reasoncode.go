package delegate

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// defaultDelegateDoor is the door a delegation row names when the contract that
// produced it carries none: a direct caller of the engine that stamped no surface.
// It is the engine's own name, so the row still says where the call came in.
const defaultDelegateDoor = "delegate"

// doorOf is the door a delegation row records: the surface that admitted the
// contract (the MCP tool name or the CLI verb it was stamped with), else the
// engine's own name — never empty, so "door on every agent_delegate row" holds.
func doorOf(contract core.AgentContract) string {
	if d := strings.TrimSpace(contract.Door); d != "" {
		return d
	}
	return defaultDelegateDoor
}

// reasonCodeFor is the closed-set code (ledger.ReasonCodes) for a placed result:
// why the job ended the way it did, in a form a reader can group and count. It is
// TOTAL — every result maps to a member of the set, with ledger.ReasonOther for a
// shape nobody named — so a row is never without one.
//
// It keys on the STRUCTURE of the result first (the flags and classes the
// delegator and the node set) and on the stable prefixes of the messages this
// package itself writes ("queue deadline", "poll deadline", "canceled", ...) and
// the node's own liveness prefix ("stalled:", bare or behind the re-pack's own
// prefix, see isStallReason) second. A node's free prose is never
// the basis for anything but the seat-down hint, which is documented as a hint.
func reasonCodeFor(pr PlacedResult) string {
	switch {
	case pr.Err != "":
		return errReasonCode(pr)
	case pr.Result.Deferred:
		return deferReasonCode(pr)
	case len(pr.AcceptanceFailures) > 0:
		return ledger.ReasonFailedVerification
	}
	return ledger.ReasonOK
}

// errReasonCode classifies a FAILURE (a result with Err set: a transport or
// config problem, distinct from the node honestly deferring).
func errReasonCode(pr PlacedResult) string {
	e := pr.Err
	switch {
	case pr.withdrawn && (strings.HasPrefix(e, "queue deadline") || strings.HasPrefix(e, replacementExhaustedPrefix)):
		// The node confirmed it took the job back at the queue deadline (ADR 0064),
		// whether the result is that deadline itself or the exhausted placement
		// that followed it.
		return ledger.ReasonQueueWithdrawn
	case pr.nodeNeverRan != "":
		// The node's own terminal record says the job never ran (reaped while this
		// process was away, or withdrawn): the same fact a confirmed withdrawal is,
		// read off a poll instead of an answer to a DELETE.
		return ledger.ReasonQueueWithdrawn
	case pr.PlacementReason == "refused before placement":
		return ledger.ReasonContract
	case strings.HasPrefix(e, "queue deadline"), strings.HasPrefix(e, "queue wait deadline"):
		return ledger.ReasonQueueDeadline
	case strings.HasPrefix(e, "canceled"):
		return ledger.ReasonCanceled
	case pr.refused:
		// A dispatch-time refusal, or the placement that ran out of nodes after one:
		// the status the last node sent (0 = never reached) decides which.
		switch {
		case pr.refusalStatus == 0:
			return ledger.ReasonNodeUnreachable
		case capacityRefusal(pr.refusalStatus):
			return ledger.ReasonQueueFull
		}
		return ledger.ReasonDispatchRefused
	case strings.HasPrefix(e, "poll deadline"):
		switch {
		case strings.Contains(e, "never answered"):
			return ledger.ReasonNodeUnreachable
		case strings.Contains(e, "DENIES"):
			return ledger.ReasonJobLost
		}
		return ledger.ReasonRemoteError
	case strings.HasPrefix(e, "node lost job"), strings.HasPrefix(e, "holder denies"):
		return ledger.ReasonJobLost
	case strings.HasPrefix(e, "poll: 401"), strings.HasPrefix(e, "queue poll: 401"):
		return ledger.ReasonDispatchRefused
	case strings.HasPrefix(e, "remote job error"), strings.HasPrefix(e, "job done but data is not"), strings.HasPrefix(e, "queue job failed"):
		return ledger.ReasonRemoteError
	case strings.HasPrefix(e, "local run:"):
		return ledger.ReasonInfrastructure
	case strings.HasPrefix(e, "no local runner wired"):
		return ledger.ReasonConfig
	}
	return ledger.ReasonOther
}

// deferReasonCode classifies a DEFER: the delegator or the node reporting, in a
// result rather than an error, that the job did not complete.
func deferReasonCode(pr PlacedResult) string {
	w := pr.Result
	switch {
	case pr.shed:
		return ledger.ReasonShed
	case strings.HasPrefix(w.Reason, "poll deadline"):
		return ledger.ReasonPollDeadline
	case isStallReason(w.Reason):
		return stallReasonCode(w.Reason)
	case pr.Unplaced && strings.HasPrefix(w.Reason, "route=remote:"):
		return ledger.ReasonNoEligibleNode
	}
	switch w.DeferClass {
	case core.DeferClassCapacity:
		// Two different root causes carry this class, and PlacedResult.Unplaced (the
		// flag a surface reads to tell "the node answered and deferred" from "the node
		// was never exercised") says which: a result NO node ran is the delegator's own
		// outcome — its capacity wait ran out, or a placement decision found no room;
		// one a node (or this box's own seat) answered is that node refusing after
		// admission — a seat at its run cap, a card under a lease or fence, an engine
		// held busy by other work.
		if pr.Unplaced {
			return ledger.ReasonCapacityWait
		}
		return ledger.ReasonNodeBusy
	case core.DeferClassBudget:
		return ledger.ReasonBudget
	case core.DeferClassAbstention:
		return ledger.ReasonAbstention
	case core.DeferClassContract:
		return ledger.ReasonContract
	case core.DeferClassConfig:
		return ledger.ReasonConfig
	case core.DeferClassWrite:
		return ledger.ReasonWrite
	case core.DeferClassInfrastructure:
		if seatUnreachable(w.Reason) {
			return ledger.ReasonSeatDown
		}
		return ledger.ReasonInfrastructure
	}
	return ledger.ReasonOther
}

// repackUnreachablePrefix opens the reason a node files when the seat went silent
// DURING the structured re-pack: the stall's own text, behind this stable prefix
// (internal/pipeline/agenttask.go; the loop's stall is filed bare). It is a stall
// like any other and the phase word in it ("in repack") names it.
const repackUnreachablePrefix = "structured re-pack unreachable: "

// isStallReason reports whether a defer reason is a node's liveness verdict
// ("stalled: ..."), filed bare or behind the re-pack prefix. A re-pack that
// failed any other way (an unreachable seat, an expired wall) carries the same
// prefix and is not a stall.
func isStallReason(reason string) bool {
	return strings.HasPrefix(strings.TrimPrefix(reason, repackUnreachablePrefix), "stalled:")
}

// stallReasonCode names the phase a node's liveness verdict went silent in. The
// reasons are the node's own ("stalled: no progress for 300s in prefill (...)",
// internal/agent.StallError), and the phase words are the run registry's phase
// names, so this keys on a documented prefix and a closed vocabulary.
func stallReasonCode(reason string) string {
	switch {
	case strings.Contains(reason, "engine did no work"), strings.Contains(reason, "kept stepping"):
		return ledger.ReasonStallEngine
	case strings.Contains(reason, "cold-load"):
		return ledger.ReasonStallColdLoad
	case strings.Contains(reason, " in prefill"):
		return ledger.ReasonStallPrefill
	case strings.Contains(reason, " in decoding"):
		return ledger.ReasonStallDecode
	case strings.Contains(reason, " in admission"):
		return ledger.ReasonStallAdmission
	case strings.Contains(reason, " in repack"):
		return ledger.ReasonStallRepack
	case strings.Contains(reason, " in tool"):
		return ledger.ReasonStallTool
	}
	return ledger.ReasonStallOther
}

// seatUnreachable is the seat-down HINT: an infrastructure defer whose text is a
// dial failure to the seat. It is the one place prose decides a code, because the
// loop's transport errors carry no structure to key on; a wrong guess costs one
// row filed under seat_down instead of infrastructure.
func seatUnreachable(reason string) bool {
	r := strings.ToLower(reason)
	for _, hint := range []string{"connection refused", "actively refused", "no such host", "dial tcp", "seat down", "seat_down"} {
		if strings.Contains(r, hint) {
			return true
		}
	}
	return false
}
