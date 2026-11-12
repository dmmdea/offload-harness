package ledger

// The root-cause catalogue: for every failure class of the closed reason-code set (reasoncode.go), why it happens,
// what that rests on and what would reduce it. It sits beside the class definitions because a class and its cause
// are read together, and the failure register (failures.go, `ledger --failures`) prints it with each class.
//
// How an entry was written (2026-11-12): the code that emits the class was read, then the rows of the 30 days to
// 2026-11-12 in the real ledger (their reasons, doors and seats), then the all-time counts for a class with no row
// in the window. An entry is Investigated only when the emitter and the rows together settle the cause it states.
// Where they do not, RootCause starts with "not investigated" and says what would settle it; no cause is guessed.
// The counts in Evidence are that snapshot, not live figures: the live count is the class's own `count`.
//
// These are findings about the harness at that date, kept in code so a class cannot be listed without one; they are
// not read from a row, and a row's text never decides them. When a cause changes, change the entry.

// The cause kinds. A kind says whose fix the cause is, which is not what failurekinds.go's Kind says (that one
// says who owns the failure for the stack-fault share).
const (
	// CauseCapacity: the stack was healthy and full; the signal is the design working.
	CauseCapacity = "capacity signal"
	// CauseNodeConfig: a node (or this box) is set up in a way that cannot serve the call.
	CauseNodeConfig = "node configuration"
	// CauseHarnessDefect: the harness itself does the wrong thing.
	CauseHarnessDefect = "harness defect"
	// CauseCallerContract: the request, its limits or its deadline, is what ended the call.
	CauseCallerContract = "caller contract"
	// CauseUpstream: a service the harness depends on (an engine, a node host, a network path) failed.
	CauseUpstream = "upstream service"
	// CauseUnknown: nothing read so far settles it.
	CauseUnknown = "unknown"
)

// notInvestigated opens the RootCause and Action of an entry nobody has settled.
const notInvestigated = "not investigated"

// FailureCause is the established cause of one failure class.
type FailureCause struct {
	// Kind is one of the Cause* constants.
	Kind string `json:"kind"`
	// RootCause is why the class happens, in one or two sentences, or "not investigated: ..." naming what would
	// settle it.
	RootCause string `json:"root_cause"`
	// Evidence is the function that emits the class and the count read in the 30 days to 2026-11-12.
	Evidence string `json:"evidence"`
	// Action is what would reduce the class, or "none: working as designed", or "not investigated".
	Action string `json:"action"`
	// Investigated is false when RootCause starts with "not investigated".
	Investigated bool `json:"investigated"`
}

// FailureCauseOf is the catalogue entry of a class. A class with no entry reads as not investigated, so a new
// reason code is listed as a gap and never as a blank.
func FailureCauseOf(class string) FailureCause {
	if c, ok := failureCauses[class]; ok {
		return c
	}
	return FailureCause{Kind: CauseUnknown, Investigated: false,
		RootCause: notInvestigated + ": the class has no catalogue entry (internal/ledger/rootcause.go); read its emitter and its rows to settle it",
		Evidence:  "none", Action: notInvestigated}
}

// noRow is the entry of a class the ledger has never held a row for: its emitter can be read, but no failure has
// been seen, so no cause is stated.
func noRow(emitter string) FailureCause {
	return FailureCause{Kind: CauseUnknown,
		RootCause: notInvestigated + ": the ledger holds no row of this class, so no failure of it has been observed; it settles when a row appears and its reason and node are read",
		Evidence:  emitter + "; 0 rows in the 30 days to 2026-11-12 and 0 in the whole ledger",
		Action:    notInvestigated}
}

var failureCauses = map[string]FailureCause{
	ReasonNodeBusy: {Kind: CauseCapacity, Investigated: true,
		RootCause: "The seat or card the call needed was held by other work when it arrived: the seat was at its local run cap (`seat busy:`), a GPU lease of another class held the card (`gpu busy: held by ...`), or a media job waited for host RAM. The harness defers rather than overload the card.",
		Evidence:  "pipeline/agenttask.go (seat busy, defer class capacity) and pipeline/pipeline_gpu.go (gpu busy); 628 rows in 30 days: video 247, research 205, audio 109 (the agent pool at its cap, a media lease on the cards, host RAM)",
		Action:    "none: working as designed. Fewer deferrals need more free card time (keep a card free for media while text leases run) or a higher run cap on a box with room."},
	ReasonFailedVerification: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": the rows say which check failed (`min_items` on plans, quotes or numbers; a regex) but not whether the seat's answer was wrong or the check too strict; the acceptance list is shape-only (delegate/lint.go warns of it)",
		Evidence:  "delegate/run.go (failed verification); 357 rows in 30 days, 345 of them from research subtasks",
		Action:    notInvestigated + ": read the saved output of a sample of failed research subtasks against their acceptance lists"},
	ReasonQueueFull: {Kind: CauseCapacity, Investigated: true,
		RootCause: "Every node asked refused the dispatch for room: its queue was at fleet_max_queue_depth (the sampled bodies read `queue full (N jobs accepted+running, limit N)`) or its card was leased. The delegator files every 503 or 429 so.",
		Evidence:  "delegate/run_capacity.go capacityRefusal; 49 rows in 30 days (of the three commonest reason groups, 22 rows read queue full and 1 node leased)",
		Action:    "none: working as designed. A node with spare capacity can raise fleet_max_queue_depth."},
	ReasonQueueDeadline: {Kind: CauseCapacity, Investigated: true,
		RootCause: "A node accepted the job and its backlog never reached it inside the queue patience, and the node did not confirm taking it back. Jobs queue behind other work on a full node.",
		Evidence:  "delegate/run_remote.go (queue deadline after); 0 rows in 30 days, 318 in the whole ledger, all written before the withdrawal stamp existed",
		Action:    "none: working as designed (the delegator withdraws and re-places since ADR 0064; queue_withdrawn is the same event confirmed)."},
	ReasonQueueWithdrawn: {Kind: CauseCapacity, Investigated: true,
		RootCause: "The same event as queue_deadline, and the node confirmed taking the job back: it sat in the node's backlog (`accepted`, never `running`) past the queue patience and was withdrawn.",
		Evidence:  "delegate/run_remote.go; 186 rows in 30 days, 182 from research subtasks, 111 on one node",
		Action:    "none: working as designed. If one node keeps most of them (111 of 186), check its concurrency against the headroom it advertises."},
	ReasonPollDeadline: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": the class does not separate a job longer than the poll budget from a node that stopped making progress (failurekinds.go says so), and the 3 rows differ (a poll error, a stale last-progress, the node's own wall)",
		Evidence:  "delegate/run_remote.go (poll deadline after); 3 rows in 30 days",
		Action:    notInvestigated + ": for each row, compare the node's own record of the job with the delegator's poll budget"},
	ReasonCanceled: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "The caller's context ended (the caller went away, or the call's own deadline passed) before or while a subtask ran.",
		Evidence:  "delegate/calldeadline.go; 29 rows in 30 days: `canceled: context canceled` and `the call's context ended before this subtask started`",
		Action:    "none: working as designed."},
	ReasonNodeUnreachable: {Kind: CauseUpstream, Investigated: true,
		RootCause: "A node host did not answer on the network: a dial that timed out (`the connected party did not properly respond`) or a poll that never got an answer. Why the host did not answer (asleep, off, network) is not recorded on the row.",
		Evidence:  "delegate/run_remote.go; 3 rows in 30 days, 2 on one node",
		Action:    "none from the harness: the node host or path is the fault; `local-offload doctor` shows which nodes answer now."},
	ReasonJobLost: noRow("delegate/run_remote.go (a poll 404 after an acknowledged dispatch)"),
	ReasonDispatchRefused: {Kind: CauseNodeConfig, Investigated: true,
		RootCause: "A node refused the dispatch itself, and the sampled rows give two node-side causes: the node rejected the delegator's bearer token (status 401 `unauthorized`, 112 rows, one node: a fleet_auth_token mismatch) and a node whose configured data folder could not be created (`creating pipeline-jobs dir`, 33 rows, one node: the path is not on that machine now).",
		Evidence:  "delegate/calldeadline.go (401 unauthorized) and fleetnode/tasks.go:1031 (creating pipeline-jobs dir); 145 rows in 30 days, 145 on one node's seat",
		Action:    "re-pair that node's token (`local-offload doctor` checks fleet tokens) and fix or remove the node's missing data folder; both are edits on the node."},
	ReasonRemoteError: noRow("delegate/run_remote.go (a node's job reported errored, or its answer was unusable)"),
	ReasonCapacityWait: {Kind: CauseCapacity, Investigated: true,
		RootCause: "No node had room for the subtask before the call's deadline: 248 of 267 waited until the call's own deadline (not agent_placement_wait_sec), 259 for one node's agent pool alias.",
		Evidence:  "delegate/run_wait.go:436 and :442; 267 rows in 30 days, 259 on one pool alias",
		Action:    "none: working as designed. Reducing it needs more worker slots on the pool's node, or fewer research subtasks per call."},
	ReasonShed: noRow("delegate (sheddable work, priority -1, found no idle node)"),
	ReasonNoEligibleNode: {Kind: CauseUpstream, Investigated: true,
		RootCause: "A lane found no fleet node able to take the call. In the sampled rows each node probed failed its health check with a timeout (`context deadline exceeded`), so the nodes were not answering, not refusing for room. Why they did not answer is not recorded.",
		Evidence:  "the remote lanes' placement sentences (forecastremote and the others) and delegate route=remote; 76 rows in 30 days, 48 of them the vision lane",
		Action:    "none from the harness: bring the nodes back (`local-offload doctor`); not investigated: why the probes timed out at the failure times."},
	ReasonSeatDown: {Kind: CauseUpstream, Investigated: true,
		RootCause: "The inference engine behind the seat was gone from llama-swap's /running, or refused connections, when the run needed it, and did not return inside the node's recovery wait. What stopped the engine is not recorded.",
		Evidence:  "agent/seatdown.go:185; 89 rows in 30 days, 87 on one node's pool alias",
		Action:    "not investigated: why that node's engine goes away; read its llama-swap log at the failure times. The harness already waits and re-issues (ADR 0066)."},
	ReasonStallAdmission: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": the monitor reports a run silent in the admission phase; why is not on the row",
		Evidence:  "agent/liveness.go; 0 rows in 30 days, 1 in the whole ledger",
		Action:    notInvestigated + ": read the node's engine log at the time of the row"},
	ReasonStallColdLoad: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": the rows say no byte came for 120 s after the seat read ready, on its first completion since the load; whether the bound is too tight for a cold seat or the engine was wedged is not settled",
		Evidence:  "agent/liveness.go:268; 4 rows in 30 days, all on one node's pool alias",
		Action:    notInvestigated + ": time that seat's first completion after a load against the bound"},
	ReasonStallPrefill: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": the rows say no progress in prefill past an allowance computed from the prompt size and the seat's rate; a prompt larger than the allowance or a wedged engine both read so",
		Evidence:  "agent/liveness.go; 0 rows in 30 days, 246 in the whole ledger (written before the stamp)",
		Action:    notInvestigated + ": compare the prompt size of those rows with the seat's measured prefill rate"},
	ReasonStallDecode: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": no progress while tokens were generated; the rows do not say whether the engine wedged or the allowance was too tight",
		Evidence:  "agent/liveness.go; 0 rows in 30 days, 51 in the whole ledger (written before the stamp)",
		Action:    notInvestigated + ": read the engine log at the time of the rows"},
	ReasonStallTool: noRow("agent/liveness.go (silence inside a tool call)"),
	ReasonStallRepack: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": no progress in the structured re-pack of an answer; why is not on the row",
		Evidence:  "agent/liveness.go (`structured re-pack unreachable: stalled`); 0 rows in 30 days, 99 in the whole ledger (written before the stamp)",
		Action:    notInvestigated + ": read the re-pack requests of those rows against the seat's log"},
	ReasonStallEngine: noRow("agent/liveness.go (the engine did no work, or kept stepping without a token)"),
	ReasonStallOther:  noRow("delegate/reasoncode.go stallReasonCode (a stall whose phase the reason does not name)"),
	ReasonBudget: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "A limit the call carried ended the run before the agent finished: the call's own deadline (`call deadline reached`, `wall timeout after ... the caller's deadline`), a cancellation filed under this class (`agent loop: canceled`), or the step budget (`step budget exhausted`). Most are the caller's deadline, not a node ceiling.",
		Evidence:  "core/calldeadline.go, pipeline/liveness.go:411, pipeline/agenttask.go:1295; 250 rows in 30 days, 241 from research subtasks",
		Action:    "give long research subtasks a longer call deadline or fewer subtasks per call; a run that ends on the step budget needs a larger max_steps."},
	ReasonAbstention: {Kind: CauseNodeConfig, Investigated: true,
		RootCause: "The answer was cut at its completion token cap, so there was nothing usable to return: the vision lane's `vision output truncated` (117 rows) and an agent answer cut at the completion budget so the re-pack was skipped (about 94 rows, `output_truncated`). The cap is a setting of the seat or the call.",
		Evidence:  "pipeline/pipeline_vision.go:315 and pipeline/agentlistcap.go:75; 221 rows in 30 days, 107 on one vision seat's questions",
		Action:    "raise the vision lane's output cap for that seat, or ask for shorter answers; not investigated: why those answers run past the cap."},
	ReasonContract: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "The caller named something the node does not serve: an image family the node does not carry.",
		Evidence:  "config/families.go:730; 4 rows in 30 days, all one family name on generate_image",
		Action:    "the caller names a served family, or the node adds that family."},
	ReasonConfig: {Kind: CauseNodeConfig, Investigated: true,
		RootCause: "A seat was set up so that it cannot serve the call: its tool-call parser does not match its chat template, so a tool call came back as text.",
		Evidence:  "pipeline/agenttask.go:1303; 2 rows in 30 days, both on one seat",
		Action:    "set that seat's --tool-call-parser to the one its chat template needs."},
	ReasonWrite: noRow("pipeline/agenttask.go (the write door refused: agent_allow_write false, or a write set past a door cap)"),
	ReasonInfrastructure: {Kind: CauseUpstream, Investigated: true,
		RootCause: "Mostly a seat whose engine was gone from llama-swap's /running (the node's defer class infrastructure carrying the same sentence as seat_down), then a vision seat that was cold and timed out. Some rows are a capacity hold filed here (see the followups).",
		Evidence:  "agent/seatdown.go:185, pipeline_vision.go; 195 rows in 30 days: research 90, fleet 82; about 63 delegate rows read `local seat reserved (gpu lease ...)`",
		Action:    "see seat_down; not investigated: why the engine goes away. The lease-hold rows are a classification defect, not an infrastructure fault."},
	ReasonBadInput: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "The input cannot be used as given: an image path that does not exist, a template variable the template does not declare.",
		Evidence:  "pipeline (image load) and compose_video BAD_INPUT; 2 rows in 30 days",
		Action:    "none: working as designed. The caller fixes the input."},
	ReasonOther: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": the class is the residue no rule names, a mix of causes; the largest sampled sentence is a node's `a text lease holds its card; it may take the call later` (a capacity signal no rule names), and 15 rows are the browse door's own errors",
		Evidence:  "reasoncode.go DeferCode (the residue); 55 rows in 30 days: audio 18, browse 15, video 7, compose 6",
		Action:    notInvestigated + ": list the distinct reasons of the 55 rows and add a rule per sentence that ties to one class"},
}
