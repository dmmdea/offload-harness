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
		RootCause: "The seat or card the call needed was held by other work when it arrived: the seat was at its local run cap (`seat busy:`), a GPU lease of another class held the card (`gpu busy: held by ...`), or a media job waited for host RAM. It also holds the two text-lease holds filed elsewhere until 2026-11-12: the delegator's `local seat reserved (gpu lease ...)` (a text lease holds the only seat the call may use and no remote had room; filed `infrastructure`) and a node's `a text lease holds its card; it may take the call later` (filed `other`). A media call that named a node that did not answer the health probe (`no node that answered is called ...`, the node's own capacity defer; 8 rows, filed `other` before) is read here too. The harness defers rather than overload the card.",
		Evidence:  "pipeline/agenttask.go (seat busy, defer class capacity), pipeline/pipeline_gpu.go (gpu busy), ledger RefileStamped and DeferCode (the lease holds); 628 rows in 30 days as stamped: video 247, research 205, audio 109 (the agent pool at its cap, a media lease on the cards, host RAM), 93 more once the lease holds are read here (79 from infrastructure, 14 from other), and 8 more once `no node that answered is called ...` is read (video 5, audio 3)",
		Action:    "none: working as designed. Fewer deferrals need more free card time (keep a card free for media while text leases run) or a higher run cap on a box with room."},
	ReasonFailedVerification: {Kind: CauseHarnessDefect, Investigated: true,
		RootCause: "Mostly the research lane's own acceptance checks rejecting answers that were right or honest, not seats failing. Of the 357 rows (346 from research subtasks, whose checks the lane writes itself), 307 are those checks rejecting an answer that was right or honest: 256 fail a page-word tripwire that is graded against the loop's final prose and not against the structured digest, and 51 fail a derived `min_items` check on the first required array. Not every one of the 307 is shown to be a false rejection. 202 rows are established by comparing the digest to the page: 188 tripwire rows whose quotes and figures are found verbatim in the page, and 14 `min_items` rows on pages that were empty shells, login walls or placeholders. 105 rest on the seat's own statement that the page lacks the content (68 tripwire, 37 `min_items`) and were not checked against the page. The other 50 are 10 seat misses (it could not read or complete the page, found 1 of 3 items on a long report, returned an empty digest, did not echo an id, or left a status empty), 4 caller regexes that were over-escaped, 1 misfile (an audio row, see config) and 35 undetermined, because the ledger does not keep the answer or the kind of check. The class also reads 5 compose rows (error class `check_failed`: a composition's own runtime or layout checks failed, from one caller iterating on one composition; filed `other` before), with `bad_input` as the other reading since the composition is what the caller wrote.",
		Evidence:  "research/anchors.go AnchorCheck, graded by core.AcceptanceCheck.Eval and evalText; delegate/run.go (failed verification); 357 rows in 30 days as stamped, 361 once the compose `check_failed` rows are read here (5 added, the audio row left). By door: research 345 and the research CLI 1, agent_delegate 8, delegate CLI 2, audio CLI 1; by seat: a 9B-class seat on three nodes 211, a 35B seat 82, the agent-pool seat 58, a 27B seat 5; 264 fall on five days (2026-10-19, -20, -23, -24, -25). Check shapes, counted apart from the causes above (the 282 tripwire rows include 26 that are seat misses or undetermined, and the tripwire and `min_items` shapes share 6 rows): docanchor regex 282, derived `min_items:<field>:1` with 0 items 70, `min_items` with 1 item against a wanted 3 on agent_delegate 2, a caller regex naming a drive path 4, `contains:<hex>` 2, a caller regex of three alternatives 1, `nonempty:status` 1, audio dead_air 1. Why the tripwire is the suspect: none of the 282 anchor sets appears in the graded prose and 5 of 282 appear in the structured digest; the 13 digests with the lowest page overlap were read and none was about another document; widening the honest-absence words (delegate/absence.go HonestAbsence) rescues 10 of 282; the failure rate among research pages that reached a verdict is 7.0% (345 against 4,604 passed) and does not step down across delegator builds (3 to 14%). Of the tripwire first attempts retried on another node, 17 ended ok, 11 failed again and 2 ended in budget. Measured 2026-11-12 by rebuilding the register's class rule outside the binary and joining the rows to the delegation log",
		Action:    "stop deriving a pass/fail `min_items:<first required array>:1` for research subtasks (excuse it when the digest says the page has none, as the existing empty-digest excuse does); do not dispatch near-empty pages (a minimum-prose floor in research.Build); grade the tripwire against the digest too, passing when its quotes, figures or titles are found verbatim in the page (research/quotes.go VerifyQuotes exists), which would have passed at least 188 of the 282; and record the answer and the acceptance kind in the ledger so the 35 undetermined rows can be settled"},
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
	ReasonPollDeadline: {Kind: CauseUpstream, Investigated: true,
		RootCause: "The class is for a node that answered and kept working past the allowance, and the 3 rows of the 30 days as stamped were not that: each is a research job that ran 681 to 978 s and ends `last poll error ... context deadline exceeded`, so the node stopped answering the delegator's 15 s poll requests while it owned the job. Two also show stale progress (128 s against a 60 s allowance, 922 s against a 79 s allowance) while other nodes completed normally in the same delegator session. They are filed `node_unreachable` here (ledger RefileStamped reads a poll deadline that carries a last poll error so), which leaves 0 in the window. Why the node went silent (asleep, off, network, hung) is not settled: no node-side log was available.",
		Evidence:  "delegate/run_remote.go runRemote (the poll loop and its 15 s poll request timeout) and delegate/reasoncode.go deferReasonCode (`poll deadline` is filed poll_deadline whatever follows it); 3 rows in 30 days as stamped, 0 once the last poll error is read as node_unreachable. One is the same incident as a node_unreachable row and carries `withdraw not confirmed: no answer within 5s`; one ran 877 s on a node whose normal p99 is 281 s and maximum 740 s; the third node's own health probes also timed out three times that day. The 20 older rows (2026-09-16 to -29) are pure budget shapes with no last poll error, and none has occurred since; they stay poll_deadline",
		Action:    "none for the filing (RefileStamped). For the node: `local-offload doctor` shows which nodes answer now; not investigated: why that node stopped answering. Not changed here: delegate/reasoncode.go deferReasonCode still stamps a poll deadline with a last poll error as poll_deadline when it writes the row (Record refiles it on the way to disk)"},
	ReasonCanceled: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "The caller's context ended (the caller went away, or the call's own deadline passed) before or while a subtask ran. It also holds the node's `agent loop: canceled (the parent context ended ...)`, which the wire files as a budget defer because the wire has no canceled class; the ledger had filed it `budget` until 2026-11-12.",
		Evidence:  "delegate/calldeadline.go and ledger RefileStamped (pipeline/agenttask.go:1295); 29 rows in 30 days as stamped: `canceled: context canceled` and `the call's context ended before this subtask started`, and 17 more once the loop cancellations are read here",
		Action:    "none: working as designed."},
	ReasonNodeUnreachable: {Kind: CauseUpstream, Investigated: true,
		RootCause: "A node host did not answer on the network: a dial that timed out (`the connected party did not properly respond`) or a poll that never got an answer. It also holds a dispatch whose connection could not be made (`dial tcp`), a job whose polls were not answered (`N consecutive poll failures`), a node that rendered a job and whose output then could not be fetched (`rendered the job but fetching it failed`, a dropped connection; the render itself succeeded, so infrastructure is the other reading), and the 3 poll-deadline rows that ended on a last poll error (poll_deadline until 2026-11-12). Why the host did not answer (asleep, off, network) is not recorded on the row.",
		Evidence:  "delegate/run_remote.go; 3 rows in 30 days as stamped, 2 on one node, and 6 more once the rules above are read here (3 from other, 3 from poll_deadline)",
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
	ReasonStallColdLoad: {Kind: CauseHarnessDefect, Investigated: true,
		RootCause: "A placement and filing defect, from one event on 2026-11-04: the 4 rows are two delegations 7 minutes apart on the delegator's own pair of cards. In the same call and the same second the placer put a fourth subtask on the pair's long-context seat, which shares the two cards with the pool seat, so loading one evicts the other. It read the pool seat as not loaded (cold, nothing to evict) and ignored the call's own pending siblings. The displaced subtasks then waited behind the swap (a warm request answered HTTP 500 after 117 s, `mid-swap`; about 145 s of seat-down wait and about 226 s of admission wait; 0 steps) and the liveness monitor filed that as a cold-load stall and not as a capacity hold. It was not a wedge and the 120 s bound was not too tight. Established from the ledger rows and the node's notes; not from an engine log, which was not available. A wedge is excluded only because the long-context seat completed in the same window.",
		Evidence:  "placement/decide.go pairLong (reads occupancy at decision time); agent/liveness.go onStall, fileStallLocked and StallError.Error; seatload/activity.go Summary (`seat not loaded`); 4 rows in 30 days, all on one node's pool alias. The long-context job of that call ran ok (943 s, 7,660 tokens) and that seat appears exactly once in the whole ledger, in that call; one older cold-load stall exists (2026-09-28). Of 87 seat_down rows on the same seat, 85 overlap no other seat's run, so they are a different cause and are not covered here",
		Action:    "placement/decide.go pairLong should count the same call's pending siblings on the pair and queue the long subtask behind them, or refuse the co-schedule; the liveness monitor should file a wait behind a known displacement as a capacity hold (node_busy). No sentence rule can move these rows: the sentence is true of what the monitor saw."},
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
		RootCause: "The answer was cut at its completion token cap, so there was nothing usable to return: the vision lane's `vision output truncated` (117 rows) and an agent answer cut at the completion budget so the re-pack was skipped (about 94 rows, `output_truncated`). The cap is a setting of the seat or the call. The reasoning tier's `reasoning tier: low classify confidence ...` (the model's own confidence in a classify answer under the minimum, 3 rows, filed `other` before) is read here too.",
		Evidence:  "pipeline/pipeline_vision.go:315 and pipeline/agentlistcap.go:75; 221 rows in 30 days as stamped, 107 on one vision seat's questions, 224 once the classify confidence defers are read (pipeline/pipeline_attempt.go)",
		Action:    "raise the vision lane's output cap for that seat, or ask for shorter answers; not investigated: why those answers run past the cap."},
	ReasonContract: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "The caller named something the node does not serve: an image family the node does not carry.",
		Evidence:  "config/families.go:730; 4 rows in 30 days, all one family name on generate_image",
		Action:    "the caller names a served family, or the node adds that family."},
	ReasonConfig: {Kind: CauseNodeConfig, Investigated: true,
		RootCause: "A seat or node was set up so that it cannot serve the call: a seat's tool-call parser does not match its chat template, so a tool call came back as text (2 rows), or a node's configured data folder does not exist on that machine, so it could not create its pipeline-jobs folder for a dispatch (30 rows, one node; filed `dispatch_refused` until 2026-11-12), a node that has not opened the media-job door a call with input files needs (error class `no_node_configured`, 2 rows, filed `other`), or the audio QA could not measure the audio because ffmpeg or ffprobe is not installed (1 row, filed `failed_verification`).",
		Evidence:  "pipeline/agenttask.go:1303 and fleetnode/tasks.go:1031 (read by ledger RefileStamped); 2 rows in 30 days as stamped, 32 once the folder fault is read here, 35 once the media-job and audio QA rows are read (render/audio-qa.mjs, ledger RefileStamped and errClassCodes)",
		Action:    "set that seat's --tool-call-parser to the one its chat template needs; fix or remove the node's missing data folder (its `home`)."},
	ReasonWrite: noRow("pipeline/agenttask.go (the write door refused: agent_allow_write false, or a write set past a door cap)"),
	ReasonInfrastructure: {Kind: CauseUpstream, Investigated: true,
		RootCause: "Mostly a seat whose engine was gone from llama-swap's /running (the node's defer class infrastructure carrying the same sentence as seat_down), then a vision seat that was cold and timed out, and the browse door's `browse: BROWSER_UNAVAILABLE:` and `browse: DECISION_UNAVAILABLE:`. The delegator's `local seat reserved (gpu lease ...)` was filed here until 2026-11-12; it is a capacity hold and is now `node_busy` (the wire class of that defer stays infrastructure, so the exit code and summary still count it).",
		Evidence:  "agent/seatdown.go:185, pipeline_vision.go; 195 rows in 30 days as stamped: research 90, fleet 82; 72 of them were `local seat reserved (gpu lease ...)`: ledger RefileStamped now reads the 55 that name only remotes without room as node_busy, and the 17 that also name a failed health probe stay here",
		Action:    "see seat_down; not investigated: why the engine goes away."},
	ReasonBadInput: {Kind: CauseCallerContract, Investigated: true,
		RootCause: "The input cannot be used as given: an image path that does not exist, a template variable the template does not declare, a media call with no in or out (`media convert: convert: in and out are required`), a composition the compose lint refuses (error class `lint_errors`).",
		Evidence:  "pipeline (image load) and compose_video BAD_INPUT and LINT_ERRORS; 2 rows in 30 days as stamped, 4 once the two added rules are read (1 media convert, 1 lint)",
		Action:    "none: working as designed. The caller fixes the input."},
	ReasonOther: {Kind: CauseUnknown, Investigated: true,
		RootCause: "The class is the residue no rule names, a bundle of misfiled sentences and not one cause; it stays kind unknown on purpose. Of the 55 rows as stamped, 25 are read by the rules of 2026-11-12 that came first (a node's `a text lease holds its card; it may take the call later`, and the browse door's `BROWSER_UNAVAILABLE`, `DECISION_UNAVAILABLE`, `HOST_NOT_ALLOWED` and `budget` errors), which leaves 30. 23 of those 30 are lane refusals and transport faults whose sentences or error classes nobody had mapped, and each is now settled by a sentence or error-class rule: `no node that answered is called ...` (8, node_busy), a dispatch whose connection could not be made (1), `N consecutive poll failures` (1) and `rendered the job but fetching it failed` (1), all node_unreachable; error class `no_node_configured` (2, config); `media convert: convert: in and out are required` (1) and error class `lint_errors` (1), bad_input; `reasoning tier: low classify confidence ...` (3, abstention); and the compose error class `check_failed` (5, failed_verification, with bad_input as the other reading, since the composition is what the caller wrote). 7 rows are left: 4 of the browse door's `blocked` (the model or the loop reported no progress, 9 to 18 s), which is proposed as abstention but whose cause is not settled and so has no rule, and 3 generate_svg rows with no reason, no reason code and no door, which cannot be traced.",
		Evidence:  "reasoncode.go DeferCode (the residue) and errClassCodes; 55 rows in 30 days as stamped (audio 18, browse 15, video 7, compose 6), 30 once the first rules are read, 7 once the rules above are read. Two further rows of those error classes are measurement runs, which the register leaves out",
		Action:    "read the 4 browse `blocked` rows against the pages they drove before ruling on them; the 3 rows with no reason need the writer to record one. Not changed here: the other verifier wordings behind `reasoning tier: ...` stay in other until a row shows them."},
}
