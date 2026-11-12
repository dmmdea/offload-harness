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
		RootCause: "The seat or card the call needed was held by other work when it arrived: the seat was at its local run cap (`seat busy:`), a GPU lease of another class held the card (`gpu busy: held by ...`), or a media job waited for host RAM. It also holds the two text-lease holds filed elsewhere until 2026-11-12: the delegator's `local seat reserved (gpu lease ...)` (a text lease holds the only seat the call may use and no remote had room; filed `infrastructure`) and a node's `a text lease holds its card; it may take the call later` (filed `other`). The harness defers rather than overload the card.",
		Evidence:  "pipeline/agenttask.go (seat busy, defer class capacity), pipeline/pipeline_gpu.go (gpu busy), ledger RefileStamped and DeferCode (the lease holds); 628 rows in 30 days as stamped: video 247, research 205, audio 109 (the agent pool at its cap, a media lease on the cards, host RAM), and 93 more once the lease holds are read here (79 from infrastructure, 14 from other)",
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
		RootCause: "The caller's context ended (the caller went away, or the call's own deadline passed) before or while a subtask ran. It also holds the node's `agent loop: canceled (the parent context ended ...)`, which the wire files as a budget defer because the wire has no canceled class; the ledger had filed it `budget` until 2026-11-12.",
		Evidence:  "delegate/calldeadline.go and ledger RefileStamped (pipeline/agenttask.go:1295); 29 rows in 30 days as stamped: `canceled: context canceled` and `the call's context ended before this subtask started`, and 17 more once the loop cancellations are read here",
		Action:    "none: working as designed."},
	ReasonNodeUnreachable: {Kind: CauseUpstream, Investigated: true,
		RootCause: "A node host did not answer on the network: a dial that timed out (`the connected party did not properly respond`) or a poll that never got an answer. Why the host did not answer (asleep, off, network) is not recorded on the row.",
		Evidence:  "delegate/run_remote.go; 3 rows in 30 days, 2 on one node",
		Action:    "none from the harness: the node host or path is the fault; `local-offload doctor` shows which nodes answer now."},
	ReasonJobLost: noRow("delegate/run_remote.go (a poll 404 after an acknowledged dispatch)"),
	ReasonDispatchRefused: {Kind: CauseNodeConfig, Investigated: true,
		RootCause: "A node refused the dispatch itself: it rejected the delegator's bearer token (status 401 `unauthorized`, one node: a fleet_auth_token mismatch). Until 2026-11-12 the class also held a node whose own data folder could not be created (`creating pipeline-jobs dir`, an ack-time 400); that is a fault of the node's configuration and not of the request, and it is now filed `config`.",
		Evidence:  "delegate/calldeadline.go (401 unauthorized); 145 rows in 30 days as stamped, 145 on one node's seat: 115 are the 401 and 30 the folder fault, which ledger RefileStamped now reads as config",
		Action:    "re-pair that node's token (`local-offload doctor` checks fleet tokens); an edit on the node."},
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
		RootCause: "A limit the call carried ended the run before the agent finished: the call's own deadline (`call deadline reached`, `wall timeout after ... the caller's deadline`), or the step budget (`step budget exhausted`). Most are the caller's deadline, not a node ceiling. The agent loop's `agent loop: canceled` was filed here until 2026-11-12 and is now `canceled`; the browse door's `browse: budget:` (max_actions spent) is read here.",
		Evidence:  "core/calldeadline.go, pipeline/liveness.go:411, pipeline/agenttask.go:1295; 250 rows in 30 days as stamped, 241 from research subtasks; 17 of them are loop cancellations that ledger RefileStamped now reads as canceled",
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
		RootCause: "A seat or node was set up so that it cannot serve the call: a seat's tool-call parser does not match its chat template, so a tool call came back as text (2 rows), or a node's configured data folder does not exist on that machine, so it could not create its pipeline-jobs folder for a dispatch (30 rows, one node; filed `dispatch_refused` until 2026-11-12).",
		Evidence:  "pipeline/agenttask.go:1303 and fleetnode/tasks.go:1031 (read by ledger RefileStamped); 2 rows in 30 days as stamped, 32 once the folder fault is read here",
		Action:    "set that seat's --tool-call-parser to the one its chat template needs; fix or remove the node's missing data folder (its `home`)."},
	ReasonWrite: noRow("pipeline/agenttask.go (the write door refused: agent_allow_write false, or a write set past a door cap)"),
	ReasonInfrastructure: {Kind: CauseUpstream, Investigated: true,
		RootCause: "Mostly a seat whose engine was gone from llama-swap's /running (the node's defer class infrastructure carrying the same sentence as seat_down), then a vision seat that was cold and timed out, and the browse door's `browse: BROWSER_UNAVAILABLE:` and `browse: DECISION_UNAVAILABLE:`. The delegator's `local seat reserved (gpu lease ...)` was filed here until 2026-11-12; it is a capacity hold and is now `node_busy` (the wire class of that defer stays infrastructure, so the exit code and summary still count it).",
		Evidence:  "agent/seatdown.go:185, pipeline_vision.go; 195 rows in 30 days as stamped: research 90, fleet 82; 79 of them were `local seat reserved (gpu lease ...)`, which ledger RefileStamped now reads as node_busy",
		Action:    "see seat_down; not investigated: why the engine goes away."},
	ReasonBadInput: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "The input cannot be used as given: an image path that does not exist, a template variable the template does not declare.",
		Evidence:  "pipeline (image load) and compose_video BAD_INPUT; 2 rows in 30 days",
		Action:    "none: working as designed. The caller fixes the input."},
	ReasonOther: {Kind: CauseUnknown,
		RootCause: notInvestigated + ": the class is the residue no rule names, a mix of causes. Since 2026-11-12 a node's `a text lease holds its card; it may take the call later` is read as node_busy, and the browse door's `BROWSER_UNAVAILABLE`, `DECISION_UNAVAILABLE`, `HOST_NOT_ALLOWED` and `budget` errors by their own classes; what is left is mostly the browse door's `blocked` (the model reported it cannot proceed, 4 rows) and media and compose sentences no rule names",
		Evidence:  "reasoncode.go DeferCode (the residue); 55 rows in 30 days as stamped (audio 18, browse 15, video 7, compose 6), 30 once the rules above are read",
		Action:    notInvestigated + ": list the distinct reasons of the rows left and add a rule per sentence that ties to one class"},
}
