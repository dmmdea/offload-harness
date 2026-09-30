package ledger

// Reason codes (register C-68, ADR 0064): a CLOSED set that says why a delegated
// job ended the way it did, written beside the free-text Reason on every
// `agent_delegate` row. The free text is for a human and varies with every job
// ("queue deadline after 5m0s (31 poll(s) answered `accepted`)"); the code is what
// a reader groups, counts and joins on. Before it, a fresh question about the
// fleet (how many jobs died queued? how many stalled in prefill?) needed a bespoke
// regex over prose, and the answers disagreed.
//
// A row that completed cleanly carries ReasonOK, so "reason_code present on every
// row" is checkable without knowing which rows failed. The set is closed: Record
// replaces a code that is not in it with ReasonOther, so a typo at a writer cannot
// mint a new group. Adding a member is an additive change.
const (
	// ReasonOK: the job completed and passed every acceptance check.
	ReasonOK = "ok"
	// ReasonStarted: the dispatch marker (Phase "started"): no outcome yet.
	ReasonStarted = "started"
	// ReasonFailedVerification: a result arrived and failed the contract's own
	// acceptance checks.
	ReasonFailedVerification = "failed_verification"

	// ReasonQueueFull: every node the delegator asked refused for capacity
	// (queue full, draining, leased, shed by the node, 429).
	ReasonQueueFull = "queue_full"
	// ReasonQueueDeadline: a node accepted the job and never started it before the
	// queue patience ran out, and did NOT confirm taking it back — it may still run.
	ReasonQueueDeadline = "queue_deadline"
	// ReasonQueueWithdrawn: the same, and the node took the job back (ADR 0064): it
	// will never run there. Either it confirmed a withdrawal the delegator asked for
	// at the queue deadline, or its own record said so when the delegator next polled
	// (reaped because nobody polled it within the poll lease, or withdrawn).
	ReasonQueueWithdrawn = "queue_withdrawn"
	// ReasonPollDeadline: a node owned the job and it did not finish inside the
	// poll budget.
	ReasonPollDeadline = "poll_deadline"
	// ReasonCanceled: the caller went away.
	ReasonCanceled = "canceled"
	// ReasonNodeUnreachable: no node answered (dial failure, dropped connection, a
	// poll that never got an answer).
	ReasonNodeUnreachable = "node_unreachable"
	// ReasonJobLost: a node denied ever holding the job it had acked (poll 404).
	ReasonJobLost = "job_lost"
	// ReasonDispatchRefused: a node refused the request itself (400, 401, 403,
	// 409...): a fact about the request or the credentials, not about capacity.
	ReasonDispatchRefused = "dispatch_refused"
	// ReasonRemoteError: a node reported the job errored, or its answer could not
	// be used.
	ReasonRemoteError = "remote_error"
	// ReasonCapacityWait: no node had room within the placement wait.
	ReasonCapacityWait = "capacity_wait"
	// ReasonShed: sheddable work found no idle node.
	ReasonShed = "shed"
	// ReasonNoEligibleNode: route=remote, or a failing fleet, with nothing eligible.
	ReasonNoEligibleNode = "no_eligible_node"

	// ReasonSeatDown: the seat the job needed was unreachable.
	ReasonSeatDown = "seat_down"
	// The stall codes name the phase a run went silent in (ADR 0055, 0061): the
	// node's own liveness verdict, filed as infrastructure.
	ReasonStallAdmission = "stall_admission"
	ReasonStallColdLoad  = "stall_cold_load"
	ReasonStallPrefill   = "stall_prefill"
	ReasonStallDecode    = "stall_decode"
	ReasonStallTool      = "stall_tool"
	ReasonStallRepack    = "stall_repack"
	// ReasonStallEngine: the seat's engine did no work for anyone, or kept
	// stepping without producing a token (a wedged or thrashing seat).
	ReasonStallEngine = "stall_engine"
	// ReasonStallOther: a stall whose phase the reason does not name.
	ReasonStallOther = "stall_other"

	// The node's own defer classes (core.DeferClass*), for a defer no finer code names.
	ReasonBudget         = "budget"
	ReasonAbstention     = "abstention"
	ReasonContract       = "contract"
	ReasonConfig         = "config"
	ReasonWrite          = "write"
	ReasonInfrastructure = "infrastructure"

	// ReasonOther: a failure no code above names. The classifier is total, so a
	// row is never without a code — but a growing share here means the set needs a
	// new member.
	ReasonOther = "other"
)

// reasonCodes is the closed set, in the order a report should list it.
var reasonCodes = []string{
	ReasonOK, ReasonStarted, ReasonFailedVerification,
	ReasonQueueFull, ReasonQueueDeadline, ReasonQueueWithdrawn, ReasonPollDeadline,
	ReasonCanceled, ReasonNodeUnreachable, ReasonJobLost, ReasonDispatchRefused,
	ReasonRemoteError, ReasonCapacityWait, ReasonShed, ReasonNoEligibleNode,
	ReasonSeatDown, ReasonStallAdmission, ReasonStallColdLoad, ReasonStallPrefill,
	ReasonStallDecode, ReasonStallTool, ReasonStallRepack, ReasonStallEngine, ReasonStallOther,
	ReasonBudget, ReasonAbstention, ReasonContract, ReasonConfig, ReasonWrite,
	ReasonInfrastructure, ReasonOther,
}

// settleReasonCode is the code Record writes on an agent_delegate row: the
// writer's own when it is in the closed set, and otherwise the truthful default
// for the row — the dispatch marker is "started", a row that completed is "ok",
// and a failed row no writer classified is "other" (a code outside the set is
// replaced, never stored: an invented member would be a group nobody can join on).
func settleReasonCode(e Entry) string {
	switch {
	case IsReasonCode(e.ReasonCode):
		return e.ReasonCode
	case e.Phase == PhaseStarted:
		return ReasonStarted
	case e.Deferred:
		return ReasonOther
	}
	return ReasonOK
}

// ReasonCodes returns the closed set of reason codes (a copy).
func ReasonCodes() []string { return append([]string(nil), reasonCodes...) }

// IsReasonCode reports whether s is a member of the closed set.
func IsReasonCode(s string) bool {
	for _, c := range reasonCodes {
		if c == s {
			return true
		}
	}
	return false
}
