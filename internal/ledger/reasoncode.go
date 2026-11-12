package ledger

import (
	"regexp"
	"strings"
)

// Reason codes (register C-68, ADR 0064; every deferred row since F22): a CLOSED set that says why a
// delegated job ended the way it did, written beside the free-text Reason on every `agent_delegate`
// row and on every row of any task that deferred. The free text is for a human and varies with every job
// ("queue deadline after 5m0s (31 poll(s) answered `accepted`)"); the code is what
// a reader groups, counts and joins on. Before it, a fresh question about the
// fleet (how many jobs died queued? how many stalled in prefill?) needed a bespoke
// regex over prose, and the answers disagreed.
//
// A delegate row that completed cleanly carries ReasonOK, so "reason_code present on every
// delegate row" is checkable without knowing which rows failed. A row of any other task carries a
// code only when it DEFERRED (F22, 2026-10-09: 21 of 100 deferred rows on one node, all 23 on
// another, none had one, because the code was settled for agent_delegate alone and every other
// door's defer was a free-text reason and, on the media doors, an err_class): a successful cascade
// or media row stays without one. The set is closed: Record replaces a code that is not in it with
// the classification of the row, so a typo at a writer cannot mint a new group. Adding a member is
// an additive change.
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
	// ReasonCapacityWait: no node had room within the placement wait — the
	// delegator's OWN outcome, a result no node ran (PlacedResult.Unplaced).
	ReasonCapacityWait = "capacity_wait"
	// ReasonNodeBusy: a node (or this box's own seat) took the job and answered a
	// capacity defer AFTER admission: its seat was at its run cap ("seat busy:"), a
	// GPU lease or fence held its card ("gpu busy:"), or its engine was held busy by
	// other work. Kept apart from ReasonCapacityWait because they are different root
	// causes with different fixes — a seat at its cap is the node's admission, a
	// capacity wait is the fleet having no room — and a reader counting one must not
	// have to read the prose to exclude the other.
	ReasonNodeBusy = "node_busy"
	// ReasonShed: sheddable work found no idle node.
	ReasonShed = "shed"
	// ReasonNoEligibleNode: route=remote, or a failing fleet, with nothing eligible.
	ReasonNoEligibleNode = "no_eligible_node"

	// ReasonSeatDown: the seat the job needed could not serve it: it was unreachable,
	// went down under the run and did not come back inside the node's recovery wait
	// (`seat down:`, ADR 0066), or answered llama-swap's 5xx past the node's busy-seat
	// wait (`seat not serving:`, the same ADR) — the seat itself, not its peers.
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

	// ReasonBadInput: the call's own input cannot be processed as given: an audio file with no audio
	// stream, a picture that does not load, a video that cannot be sampled. Nothing is wrong with the
	// stack or the contract; the file is what has to change.
	ReasonBadInput = "bad_input"

	// ReasonOther: a failure no code above names. The classifier is total, so a
	// row is never without a code — but a growing share here means the set needs a
	// new member.
	ReasonOther = "other"
)

// NoReasonRecorded is the reason Record files on a deferred row whose writer gave none.
const NoReasonRecorded = "deferred: no reason recorded"

// reasonCodes is the closed set, in the order a report should list it.
var reasonCodes = []string{
	ReasonOK, ReasonStarted, ReasonFailedVerification,
	ReasonQueueFull, ReasonQueueDeadline, ReasonQueueWithdrawn, ReasonPollDeadline,
	ReasonCanceled, ReasonNodeUnreachable, ReasonJobLost, ReasonDispatchRefused,
	ReasonRemoteError, ReasonCapacityWait, ReasonNodeBusy, ReasonShed, ReasonNoEligibleNode,
	ReasonSeatDown, ReasonStallAdmission, ReasonStallColdLoad, ReasonStallPrefill,
	ReasonStallDecode, ReasonStallTool, ReasonStallRepack, ReasonStallEngine, ReasonStallOther,
	ReasonBudget, ReasonAbstention, ReasonContract, ReasonConfig, ReasonWrite,
	ReasonInfrastructure, ReasonBadInput, ReasonOther,
}

// settleReasonCode is the code Record writes on an agent_delegate row and on any deferred row: the
// writer's own when it is in the closed set, and otherwise the truthful default for the row: the
// dispatch marker is "started", a delegate row that completed is "ok", and a deferred row no writer
// classified is classified from its err_class and its reason (DeferCode), "other" when neither names
// it (a code outside the set is replaced, never stored: an invented member would be a group nobody
// can join on).
func settleReasonCode(e Entry) string {
	switch {
	case IsReasonCode(e.ReasonCode):
		return RefileStamped(e.ReasonCode, e.Reason)
	case e.Phase == PhaseStarted:
		return ReasonStarted
	case e.Deferred:
		return DeferCode(e.ErrClass, e.Reason)
	}
	return ReasonOK
}

// errClassCodes maps the err_class vocabulary of the media, vision and speech doors to the closed set.
// The classes are written in internal/core (ErrClass*), internal/pipeline (classifyErr, the lane and
// lease classes) and internal/gpugen (ClassifyErr, the iGPU runners' typed classes); a test in
// internal/pipeline pins every core.ErrClass* constant to a member here. A class missing from the table
// falls through to the reason's prefix and then to "other", which is how a new class is noticed.
var errClassCodes = map[string]string{
	// Another job held what the call needed: congestion, a place in line, not a failure.
	"gpu_busy": ReasonNodeBusy, "gpu_queued": ReasonNodeBusy, "compose_busy": ReasonNodeBusy,
	// Something in the stack failed.
	"gpu_lease_unavailable": ReasonInfrastructure, "timeout": ReasonInfrastructure, "conn_refused": ReasonInfrastructure,
	"http_5xx": ReasonInfrastructure, "oom": ReasonInfrastructure, "disk_full": ReasonInfrastructure,
	"out_dir_unwritable": ReasonInfrastructure, "tts_endpoint": ReasonInfrastructure, "lane_unavailable": ReasonInfrastructure,
	"engine_crashed": ReasonInfrastructure, "gpu_reset": ReasonInfrastructure, "cpu_placement": ReasonInfrastructure,
	"cpu_backend_refused": ReasonInfrastructure, "illegal_instruction": ReasonInfrastructure,
	// A ComfyUI that stopped answering or answers with a broken CUDA context (D-38, issue #72).
	"comfy_unusable": ReasonInfrastructure,
	// The box or the binding cannot do what was asked as configured.
	"not_configured": ReasonConfig, "binary_not_absolute": ReasonConfig, "model_incompatible": ReasonConfig,
	"device_invalid": ReasonConfig, "ffmpeg_unavailable": ReasonConfig,
	// A call that names a node (or a lane's node roster) this box is not set up to dial or that has not opened the
	// door the call needs (a node without media-job, a roster left out for configuration): the setup, not the load.
	"no_node_configured": ReasonConfig,
	// A route the config binds whose file is gone (CT-54): another node may serve it, this one is misconfigured now.
	"route_not_ready": ReasonConfig,
	// The request itself is what no node can run.
	"token_cap_exceeded": ReasonContract, "extra_args_refused": ReasonContract,
	// The engine ran and its output failed the quality gate.
	"black_clip": ReasonFailedVerification, "frozen_clip": ReasonFailedVerification, "depth_frames_invalid": ReasonFailedVerification,
	"dead_air": ReasonFailedVerification, "unmeasurable": ReasonFailedVerification,
	// compose_video's own checks of a composition it rendered (a page error, a layout check) failed. The composition
	// is what the caller wrote, so bad_input is the alternative reading; the checks are the lane's acceptance of the
	// result, which is what failed_verification names.
	"check_failed": ReasonFailedVerification,
	// The input file, or a composition the caller wrote that the lint refuses.
	"bad_input": ReasonBadInput, "lint_errors": ReasonBadInput,
}

// reasonPrefixCodes classify a deferred row whose err_class names nothing, by the stable prefix of the
// reason the pipeline wrote ("gpu busy: ...", "audio convert: ..."): the review lane's local defer and
// the other door-specific writes that set no err_class. Matched in order; a prefix is a sentence the
// harness itself writes, never a model's or a node's prose.
var reasonPrefixCodes = []struct{ prefix, code string }{
	{"gpu busy:", ReasonNodeBusy}, {"seat busy:", ReasonNodeBusy},
	// A conversion that could not make a temp directory is the box's disk; ffmpeg that could not be run is
	// typed upstream (err_class ffmpeg_unavailable); what remains of "audio convert:" is ffmpeg refusing the file.
	{"audio convert: audioio: tempdir:", ReasonInfrastructure},
	{"the input has no audio stream", ReasonBadInput}, {"audio convert:", ReasonBadInput}, {"image load:", ReasonBadInput},
	{"frame sampling:", ReasonBadInput}, {"empty image prompt", ReasonBadInput},
	// The media verb's own refusal of a call with no in or out. Only this whole sentence is named: "media <op>: " also
	// wraps what ffmpeg itself reported, which is not the caller's input.
	{"media convert: convert: in and out are required", ReasonBadInput},
	{"generate_svg: missing kind", ReasonBadInput}, {"generate_svg: bad spec", ReasonBadInput},
	// The media doors' own refusals of an input that cannot be run as given (the sentence each door writes).
	{"empty audio prompt", ReasonBadInput}, {"empty video prompt", ReasonBadInput}, {"empty character/background prompt", ReasonBadInput},
	{"animate needs both a reference image and a driver video", ReasonBadInput},
	{"unknown audio kind ", ReasonContract}, {"unknown voice ", ReasonContract},
	{"empty transcript", ReasonAbstention}, {"empty vision output", ReasonAbstention}, {"vision output truncated", ReasonAbstention},
	// transcribe --words: the upstream answered but gave no per-word timings, so the words door has nothing to return.
	{"the whisper upstream returned no word timings", ReasonAbstention},
	{"non-JSON output", ReasonAbstention},
	// The reasoning tier's own gate: the classify answer arrived and the model's own confidence in it was below the
	// configured minimum, so it declined to commit (pipeline_attempt.go). The rest of "reasoning tier: <verdict>" is the
	// verifier's wording for other faults and is not named.
	{"reasoning tier: low classify confidence ", ReasonAbstention},
	{"transcribe call failed:", ReasonInfrastructure}, {"forecast call failed:", ReasonInfrastructure}, {"image generation failed:", ReasonInfrastructure},
	{"audio generation failed:", ReasonInfrastructure}, {"video generation failed:", ReasonInfrastructure},
	{"character animation failed:", ReasonInfrastructure}, {"run-graph failed:", ReasonInfrastructure},
	{"batch job failed:", ReasonInfrastructure}, {"cannot create out_dir:", ReasonInfrastructure},
	// "<kind> refused: <why>" is a binding the node cannot run as configured (an engine binary that does not
	// resolve is the usual case); a request the bindings refuse by size or flags sets its own err_class.
	{"audio generation refused:", ReasonConfig}, {"video generation refused:", ReasonConfig}, {"character animation refused:", ReasonConfig},
	{"animategen_engine is sdcpp but ", ReasonConfig},
	// The same two shapes on the iGPU image doors (upscale_image, inpaint_image); an out-of-range inpaint
	// argument is the request's, like an unknown audio kind.
	{"image upscale refused:", ReasonConfig}, {"inpaint refused:", ReasonConfig},
	{"upscale_engine is sdcpp but ", ReasonConfig}, {"inpaint_engine is sdcpp but ", ReasonConfig},
	{"inpaint denoise must be ", ReasonContract}, {"inpaint grow_mask must be ", ReasonContract},
	{"compose_video: no composition route configured", ReasonConfig},
	{"voice=endpoint requested but tts_endpoint is unset", ReasonConfig},
	// A named family the node does not serve is the request's; a family whose overlay is incomplete is the config's.
	{"unknown image family", ReasonContract}, {"unknown edit family", ReasonContract}, {"imagegen_families[", ReasonConfig},
	{"probe:", ReasonInfrastructure}, {"generate_svg: write:", ReasonInfrastructure}, {"generate_svg:", ReasonInfrastructure},
	// The browse door's own typed errors (pipeline/browse.go: "browse: <CLASS>: <detail>"; docs/systems/browse-lane.md),
	// the five that name one member: no browser or no decision endpoint to drive it is the stack (as lane_unavailable),
	// a page outside allow_hosts is the request's, max_actions spent is a limit the call carried, and "blocked" (the model
	// or the loop reported no progress) is a loop that ended with no usable answer, abstention; why the page could not be
	// driven is not on the row. "blocked" is matched on the door's own sentence and is not an errClassCodes entry: the
	// word means other things elsewhere. The door's other classes are not named here.
	{"browse: BROWSER_UNAVAILABLE:", ReasonInfrastructure}, {"browse: DECISION_UNAVAILABLE:", ReasonInfrastructure},
	{"browse: HOST_NOT_ALLOWED:", ReasonContract}, {"browse: budget:", ReasonBudget},
	{"browse: blocked:", ReasonAbstention},
}

// legacyReasonPrefixCodes classify a deferred row by the sentence the harness ITSELF wrote, for the rows
// written before the writer stamped a reason code (and for any writer that still does not). Each prefix is a
// sentence one writer owns, and each maps to the class that writer files the same defer under; none is a model's
// or a node's prose. Matched in order, after err_class and reasonPrefixCodes.
//
//   - "wall timeout after": internal/pipeline ceilingReason, filed under ceilingClass, which is budget unless the
//     monitor's own ceiling held the run (and then the sentence is the ceiling's, not this one).
//   - "step budget exhausted": the agent loop's step ceiling, class budget (agenttask.go).
//   - "output failed schema": the structured re-pack's output failing the contract's schema, class abstention.
//   - "failed verification:": the delegator's reason for a result that arrived and failed the acceptance checks
//     (delegate.record), the code failed_verification.
//   - "seat down: ": core.SeatDownReason, the node's typed seat-down outcome (ADR 0066), the code seat_down.
//   - the lane placement defers (a probe of every fleet node found none able to take the call), the code
//     no_eligible_node; "no fleet node advertises" is the accelerator forward's own.
//   - "no node that answered is called ": a media call that named a node (mediaremote/named.go) which was not among
//     the entries that answered the health probe; the sentence lists the entries that did not answer. It is the same
//     defer CodeForLaneDefer files through the wire class capacity, so the code is node_busy.
var legacyReasonPrefixCodes = []struct{ prefix, code string }{
	{"wall timeout after", ReasonBudget}, {"step budget exhausted", ReasonBudget},
	{"output failed schema", ReasonAbstention},
	{"failed verification:", ReasonFailedVerification},
	{"seat down: ", ReasonSeatDown},
	{"no fleet node is eligible for the ", ReasonNoEligibleNode}, {"no fleet node serves ", ReasonNoEligibleNode},
	{"no node that answered is called ", ReasonNodeBusy},
}

// queueWithdrawnClause ends a "queue deadline after" reason the node confirmed it took back
// (delegate.run_remote): the delegator's queue_withdrawn, against queue_deadline for one that may still run.
const queueWithdrawnClause = "the job was withdrawn from the node, which will never run it"

// dispatchCapacityRe matches the delegator's (and the accelerator forward's) own wrapper of a node's 503 or 429
// answer to a dispatch: "dispatch <url>: status 503: <the node's body>". The delegator files every such refusal
// as queue_full (capacityRefusal), whatever the body says (queue full, node leased, too many requests).
var dispatchCapacityRe = regexp.MustCompile(`dispatch \S+: status (?:503|429): `)

// dialFailureWords is the Go net package's own wording of a connection that could not be made, inside the delegator's
// "dispatch <url>: Post ...: dial tcp <addr>: <cause>" wrapper.
const dialFailureWords = " dial tcp "

// pollFailuresRe matches the media and the other remote lanes' own give-up on a node whose job polls stopped being
// answered: "node <id>: job <job id>: <N> consecutive poll failures, last: <error>" (mediaremote, textremote,
// visionremote and the others). The node stopped answering, which is the definition of node_unreachable.
var pollFailuresRe = regexp.MustCompile(`: job \S+: \d+ consecutive poll failures, last: `)

// renderedFetchFailedSentence is the media lane's defer when a node finished the render and the fetch of its output
// then failed (mediaremote.go: "node <id> rendered the job but fetching it failed: <error>"). The reply the node gave
// was cut or never came back, so it reads as the node not answering; the render itself succeeded.
const renderedFetchFailedSentence = " rendered the job but fetching it failed: "

// legacyReasonCode is the code of a reason one of the sentences above names, or "".
func legacyReasonCode(reason string) string {
	for _, p := range legacyReasonPrefixCodes {
		if strings.HasPrefix(reason, p.prefix) {
			return p.code
		}
	}
	switch {
	case strings.HasPrefix(reason, "queue deadline after"):
		if strings.Contains(reason, queueWithdrawnClause) {
			return ReasonQueueWithdrawn
		}
		return ReasonQueueDeadline
	case strings.HasPrefix(reason, "stalled: no progress"), strings.HasPrefix(reason, "structured re-pack unreachable: stalled: no progress"):
		return stallCodeOf(reason)
	case strings.Contains(reason, " (fleet): no fleet node advertises "):
		return ReasonNoEligibleNode
	case dispatchCapacityRe.MatchString(reason):
		return ReasonQueueFull
	case strings.Contains(reason, textLeaseHoldsCardSentence):
		return ReasonNodeBusy
	case strings.HasPrefix(reason, "dispatch ") && strings.Contains(reason, dialFailureWords):
		// The delegator's own wrapper of a dispatch POST that could not connect ("dispatch <url>: Post ...: dial tcp ...").
		// The prefix is what keeps an agent loop's own "agent loop: Post ...: dial tcp ..." (a seat's engine, seat_down or
		// infrastructure) out of it: that is the box's own seat, not a node host.
		return ReasonNodeUnreachable
	case pollFailuresRe.MatchString(reason):
		return ReasonNodeUnreachable
	case strings.HasPrefix(reason, "node ") && strings.Contains(reason, renderedFetchFailedSentence):
		return ReasonNodeUnreachable
	}
	return ""
}

// textLeaseHoldsCardSentence is the whole sentence a media lane writes when a node's card is under a text lease and
// the node itself would take the call once the lease ends (internal/mediaremote/named.go, defer class capacity):
// the node's own capacity defer, node_busy. It is matched whole on purpose: the shorter "a text lease holds its
// card" also ends a different sentence (a probe list in which no node served the call: no_eligible_node).
const textLeaseHoldsCardSentence = "a text lease holds its card; it may take the call later"

// Reasons that were stamped under the wrong class before 2026-11-12, each the sentence one harness writer owns and
// the class its stamp should have been. RefileStamped is the one place that knows them.
const (
	// localSeatReservedSentence opens (or follows "route=spread: " in) the defer the delegator files when the only
	// seat it may use is held by a text lease and no remote had room (delegate/run_wait.go reservedDefer). The wire
	// class of that defer stays infrastructure (the exit code and the summary count it so); the ledger class is the
	// capacity hold it is: the card is held by other work, node_busy.
	localSeatReservedSentence = "local seat reserved ("
	// agentLoopCanceledSentence is the node's defer when the agent loop's parent context ended (pipeline/agenttask.go).
	// The wire class is budget because the wire has no canceled; the ledger has.
	agentLoopCanceledSentence = "agent loop: canceled (the parent context ended"
	// nodeJobsDirSentence is a node that could not create its own pipeline-jobs folder (fleetnode/tasks.go), which
	// the delegator reads as an ack-time 400 and files as dispatch_refused, a fact about the request or the
	// credentials. The folder is the node's configured home, so the fault is the node's configuration.
	nodeJobsDirSentence = "agent contract: creating pipeline-jobs dir"
	// lastPollErrorClause is appended by the delegator to a "poll deadline after" reason when the last poll of the job
	// got no answer (delegate/run_remote.go): the node stopped answering while it owned the job. The class
	// poll_deadline is for a node that answered and kept working past the allowance, so this one is node_unreachable.
	lastPollErrorClause = " (last poll error: "
	// audioQAUnmeasuredSentence and audioQANoToolSentence are the audio QA's own words when it could not measure the
	// delivered audio because ffmpeg or ffprobe is not there (render/audio-qa.mjs: "the delivered audio could not be
	// measured (no measurement (ffmpeg/ffprobe unavailable))"). The audio was never judged, so nothing failed its check:
	// the box is missing a tool, which is configuration.
	audioQAUnmeasuredSentence = "could not be measured"
	audioQANoToolSentence     = "no measurement (ffmpeg/ffprobe unavailable)"
)

// RefileStamped is the class a row's stamped code should read as, given its reason: the stamp itself when the pair
// is not one of the known misfilings. It is applied where a stamp is settled (Record, so new rows are written
// right) and where one is read (failureClassOf, so rows written before 2026-11-12 count right), and nowhere else.
// Each rule needs BOTH the wrong stamp and the writer's own sentence, so no other row of those classes can move.
func RefileStamped(code, reason string) string {
	switch code {
	case ReasonInfrastructure:
		if strings.Contains(reason, localSeatReservedSentence) && !remoteFallbackFaulted(reason) {
			return ReasonNodeBusy
		}
	case ReasonBudget:
		if strings.HasPrefix(reason, agentLoopCanceledSentence) {
			return ReasonCanceled
		}
	case ReasonDispatchRefused:
		if strings.Contains(reason, nodeJobsDirSentence) {
			return ReasonConfig
		}
	case ReasonPollDeadline:
		if strings.Contains(reason, lastPollErrorClause) {
			return ReasonNodeUnreachable
		}
	case ReasonFailedVerification:
		if strings.Contains(reason, audioQAUnmeasuredSentence) && strings.Contains(reason, audioQANoToolSentence) {
			return ReasonConfig
		}
	}
	return code
}

// remoteFaultSentences are the delegator's own words for a remote fallback that failed because the fleet is broken
// or misconfigured (delegate/run_probe.go nodeSideVerdict and noEligibleRemote), as opposed to full or leased.
var remoteFaultSentences = []string{
	"failed the health probe",
	"no remote fleet nodes are configured",
	"no remote passed the capability gate",
	"advertise no context ceiling",
}

// remoteFallbackFaulted reports a lease hold whose remote fallback failed on a fault, not on room. Such a row stays
// infrastructure: the local seat being held must never hide a dead or misconfigured fleet behind a capacity class
// (2026-11-12 review). A row that names a fault keeps the fault class even when other remotes were only full: 17 of
// the 72 lease-hold rows the register then counted also named a failed health probe, and stay infrastructure.
func remoteFallbackFaulted(reason string) bool {
	for _, s := range remoteFaultSentences {
		if strings.Contains(reason, s) {
			return true
		}
	}
	return false
}

// stallCodeOf names the phase a node's liveness verdict ("stalled: no progress for 300s in prefill (...)") went
// silent in, by the phase words of the run registry, as the delegator reads them (delegate.stallReasonCode).
func stallCodeOf(reason string) string {
	switch {
	case strings.Contains(reason, "engine did no work"), strings.Contains(reason, "kept stepping"):
		return ReasonStallEngine
	case strings.Contains(reason, "cold-load"):
		return ReasonStallColdLoad
	case strings.Contains(reason, " in prefill"):
		return ReasonStallPrefill
	case strings.Contains(reason, " in decoding"):
		return ReasonStallDecode
	case strings.Contains(reason, " in admission"):
		return ReasonStallAdmission
	case strings.Contains(reason, " in repack"):
		return ReasonStallRepack
	case strings.Contains(reason, " in tool"):
		return ReasonStallTool
	}
	return ReasonStallOther
}

// deferClassCodes maps core.DeferClass* (the class a node's defer carries on the wire) to the closed set. The
// ledger imports no core, so the values are written as strings and a test in internal/pipeline pins every
// core.DeferClass* constant to an entry here. "capacity" is the node's own capacity defer (a seat at its run
// cap, a card under a lease): ReasonNodeBusy, as the delegator files the same class when a node answered it.
var deferClassCodes = map[string]string{
	"abstention": ReasonAbstention, "budget": ReasonBudget, "infrastructure": ReasonInfrastructure,
	"config": ReasonConfig, "contract": ReasonContract, "capacity": ReasonNodeBusy, "write": ReasonWrite,
}

// CodeForDeferClass is the reason code of a defer class, or "" when the class is empty or not one the table
// knows (the row is then classified from its err_class and its reason, DeferCode).
func CodeForDeferClass(class string) string { return deferClassCodes[class] }

// CodeForLaneDefer is the code a door stamps on the row of a result that carries a defer class and no code of its
// own (the remote lanes, the accelerator forward): the finer reading first (err_class, then the sentence the
// harness wrote, DeferCode), and the defer class only when neither names it. Without it a lane's capacity defer
// was filed "other": 154 of the 157 such rows in one 30-day register. "" when nothing names the row.
func CodeForLaneDefer(errClass, reason, deferClass string) string {
	if c := DeferCode(errClass, reason); c != ReasonOther {
		return c
	}
	return CodeForDeferClass(deferClass)
}

// DeferCode is the closed-set code of a deferred row no writer classified: its err_class when the table
// knows it, else the prefix of its reason ("no ... configured" is config), else ReasonOther. Total: every
// deferred row gets a member of the set.
func DeferCode(errClass, reason string) string {
	if c, ok := errClassCodes[errClass]; ok {
		return c
	}
	for _, p := range reasonPrefixCodes {
		if strings.HasPrefix(reason, p.prefix) {
			return p.code
		}
	}
	if c := legacyReasonCode(reason); c != "" {
		return c
	}
	if strings.HasPrefix(reason, "no ") && strings.Contains(reason, " configured") {
		return ReasonConfig
	}
	// "pipeline <task>: no artifacts configured" and "pipeline <task> failed: ...": the task name sits in the
	// middle, so no fixed prefix names them.
	if strings.HasPrefix(reason, "pipeline ") {
		switch {
		case strings.Contains(reason, ": no artifacts configured"):
			return ReasonConfig
		case strings.Contains(reason, " failed: "):
			return ReasonInfrastructure
		}
	}
	return ReasonOther
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
