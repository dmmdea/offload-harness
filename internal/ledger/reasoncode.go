package ledger

import "strings"

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
		return e.ReasonCode
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
	// A route the config binds whose file is gone (CT-54): another node may serve it, this one is misconfigured now.
	"route_not_ready": ReasonConfig,
	// The request itself is what no node can run.
	"token_cap_exceeded": ReasonContract, "extra_args_refused": ReasonContract,
	// The engine ran and its output failed the quality gate.
	"black_clip": ReasonFailedVerification, "frozen_clip": ReasonFailedVerification, "depth_frames_invalid": ReasonFailedVerification,
	"dead_air": ReasonFailedVerification, "unmeasurable": ReasonFailedVerification,
	// The input file.
	"bad_input": ReasonBadInput,
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
	{"generate_svg: missing kind", ReasonBadInput}, {"generate_svg: bad spec", ReasonBadInput},
	// The media doors' own refusals of an input that cannot be run as given (the sentence each door writes).
	{"empty audio prompt", ReasonBadInput}, {"empty video prompt", ReasonBadInput}, {"empty character/background prompt", ReasonBadInput},
	{"animate needs both a reference image and a driver video", ReasonBadInput},
	{"unknown audio kind ", ReasonContract}, {"unknown voice ", ReasonContract},
	{"empty transcript", ReasonAbstention}, {"empty vision output", ReasonAbstention}, {"vision output truncated", ReasonAbstention},
	{"non-JSON output", ReasonAbstention},
	{"transcribe call failed:", ReasonInfrastructure}, {"forecast call failed:", ReasonInfrastructure}, {"image generation failed:", ReasonInfrastructure},
	{"audio generation failed:", ReasonInfrastructure}, {"video generation failed:", ReasonInfrastructure},
	{"character animation failed:", ReasonInfrastructure}, {"run-graph failed:", ReasonInfrastructure},
	{"batch job failed:", ReasonInfrastructure}, {"cannot create out_dir:", ReasonInfrastructure},
	// "<kind> refused: <why>" is a binding the node cannot run as configured (an engine binary that does not
	// resolve is the usual case); a request the bindings refuse by size or flags sets its own err_class.
	{"audio generation refused:", ReasonConfig}, {"video generation refused:", ReasonConfig}, {"character animation refused:", ReasonConfig},
	{"animategen_engine is sdcpp but ", ReasonConfig}, {"compose_video: no composition route configured", ReasonConfig},
	{"voice=endpoint requested but tts_endpoint is unset", ReasonConfig},
	// A named family the node does not serve is the request's; a family whose overlay is incomplete is the config's.
	{"unknown image family", ReasonContract}, {"unknown edit family", ReasonContract}, {"imagegen_families[", ReasonConfig},
	{"probe:", ReasonInfrastructure}, {"generate_svg: write:", ReasonInfrastructure}, {"generate_svg:", ReasonInfrastructure},
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
