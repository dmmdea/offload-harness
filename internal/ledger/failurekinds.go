package ledger

// The kind of a failure class: what the failure says about WHO owns it, written in code beside the closed reason-code
// set so a count of failures can be read as the question it is usually asked as, "how often does the stack itself
// fail". Without it a full queue, a caller's bad contract, a ceiling the work outgrew and a dead seat sit in one
// list on the same footing: reasoncode.go files congestion as "a place in line, not a failure", and core's defer
// classes (agentwire.go) keep a node's capacity, the caller's contract and a broken stack apart for the same reason
// (delegate.BrokenStackDefer names only infrastructure and config as the operator's to fix).
//
// The kind is a fact about the CLASS, decided here once, and never about the row: no row's text is read. Where a
// class straddles two kinds the comment on its line says which way it was filed and why.
const (
	// KindStackFault: the stack itself failed or went silent (a seat, a node, a transport, a configuration that
	// cannot work). An operator, not a different request, is what fixes it.
	KindStackFault = "stack_fault"
	// KindCapacity: the stack was healthy and full: a place in line, a refusal for room, a wait that ran out.
	KindCapacity = "capacity"
	// KindCaller: the request is what cannot be served as given: its contract, its input, its credentials, a door
	// the node's policy keeps shut, or a caller that left.
	KindCaller = "caller"
	// KindCeiling: a configured ceiling (a step budget, a poll budget) stopped work that may have finished behind a
	// larger one.
	KindCeiling = "ceiling"
	// KindWork: the stack ran and the work itself failed: no usable answer, or one that failed its checks.
	KindWork = "work"
	// KindUnclassified: the failure no class names (ReasonOther). It is not filed under any other kind, so the
	// stack-fault share never counts a failure nobody could classify as the stack's.
	KindUnclassified = "unclassified"
)

// failureKinds is the kind of every failure member of the closed reason-code set: ok and started are not failures
// and have none. TestEveryFailureClassHasAKindFromTheClosedSet fails on a code with no entry and on an entry for a
// code the set does not hold.
var failureKinds = map[string]string{
	ReasonFailedVerification: KindWork, // the stack ran; the output failed its checks (the media doors' quality gates too)

	// Capacity: the delegator's and the nodes' queues.
	ReasonQueueFull: KindCapacity, ReasonQueueDeadline: KindCapacity, ReasonQueueWithdrawn: KindCapacity,
	ReasonCapacityWait: KindCapacity, ReasonNodeBusy: KindCapacity, ReasonShed: KindCapacity,
	// no_eligible_node reads "route=remote, or a failing fleet, with nothing eligible": filed as capacity because it is
	// the delegator's own placement outcome, like capacity_wait (no node ran the job); a fleet that was failing its
	// probes is visible on the node classes, not here.
	ReasonNoEligibleNode: KindCapacity,

	// poll_deadline: a node owned the job and the poll budget ran out. The class does not separate a job larger than
	// the budget from a node that stopped progressing; filed by what stopped it, the budget.
	ReasonPollDeadline: KindCeiling,
	ReasonBudget:       KindCeiling,

	// canceled: the caller left. dispatch_refused: a node refused the request itself (400, 401, 403, 409): the request
	// or the credentials, not capacity. write: the node will not open the write door (agent_allow_write is false, or
	// the write set broke a door cap): a decision about the contract, fixed by an opt-in or a smaller change, and
	// agentwire.go says it is neither a broken stack nor an unplaceable contract, so it is the caller's.
	//
	// dispatch_refused files a 401 or 403 answer as the caller's. Between the fleet's own nodes that is a different
	// thing: a credential refusal there, such as a stale fleet token after a rollout, is a configuration fault, the
	// operator's to fix and not the request's. The kind is decided by the class, and the class is the reason code,
	// so the register cannot tell such a refusal from a 400 or a 409 that really is the request (the status is
	// only in the reason text, which it never reads). Splitting 401 and 403 into a class of their own is a
	// possible later change; nothing here does it.
	ReasonCanceled: KindCaller, ReasonDispatchRefused: KindCaller, ReasonContract: KindCaller, ReasonBadInput: KindCaller,
	ReasonWrite: KindCaller,

	ReasonAbstention: KindWork,

	// The stack: a node or seat that did not answer, lost the job, errored, went down or stalled; and the two defer
	// classes BrokenStackDefer calls the operator's (infrastructure, config).
	ReasonNodeUnreachable: KindStackFault, ReasonJobLost: KindStackFault, ReasonRemoteError: KindStackFault,
	ReasonSeatDown: KindStackFault, ReasonInfrastructure: KindStackFault, ReasonConfig: KindStackFault,
	ReasonStallAdmission: KindStackFault, ReasonStallColdLoad: KindStackFault, ReasonStallPrefill: KindStackFault,
	ReasonStallDecode: KindStackFault, ReasonStallTool: KindStackFault, ReasonStallRepack: KindStackFault,
	ReasonStallEngine: KindStackFault, ReasonStallOther: KindStackFault, // the node's own liveness verdicts, filed as infrastructure

	ReasonOther: KindUnclassified,
}

// failureKindOrder is the closed kind set in the order a report lists it when counts tie.
var failureKindOrder = []string{KindStackFault, KindCapacity, KindCaller, KindCeiling, KindWork, KindUnclassified}

// FailureKinds is the closed set of failure kinds.
func FailureKinds() []string { return append([]string(nil), failureKindOrder...) }

// FailureKindOf is the kind of a failure class (a reason code), or "" for a code that is not a failure member of
// the closed set (ok, started, and anything outside it).
func FailureKindOf(class string) string { return failureKinds[class] }
