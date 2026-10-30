// run_types.go - Result and option types of the delegation runner, plus its timing tunables and shared HTTP client.

package delegate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	// Aliased: this file's `placement` struct (a resolved "run it HERE") predates
	// the package and is used at every placement site; the package is the
	// composite tier's decision TABLE (ADR 0052), so the alias names what it is.
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// LocalOptions is what a delegator that already DECIDED where a contract runs
// hands the local runner (ADR 0052): the seat to run instead of the planner
// default and the placement block to publish on the result. It exists because
// placement is decided once, delegator-side, over the same table a node uses
// — the runner must not re-derive (and possibly contradict) that decision, so
// it receives it. The zero value means "decide nothing here": the planner seat
// runs and no placed block is published, which is every pre-0.116 call.
// pipeline.AgentContractOptions is this type by alias, so the method value
// pipeline.RunAgentContract satisfies LocalRunner without a shim.
type LocalOptions struct {
	// Seat is the llama-swap id/alias to run the loop and the re-pack on; ""
	// keeps the box's planner seat.
	Seat string
	// Placed is published verbatim as the result's `placed` block; nil
	// publishes none (the byte-identical constraint for plain boxes).
	Placed *core.Placed
	// ParentJobID is the delegator's job id for this run (register C-62). The
	// runner's own ledger row becomes an inner row of that job — the
	// delegator's agent_delegate row is the job's one record — so a
	// route=local job is counted once, not twice. "" (every caller that is not
	// the delegator) keeps the runner's row a job of its own.
	ParentJobID string
}

// LocalRunner executes one contract in-process on the local node — the same
// read-only agent.Build path a fleet node runs (pipeline.RunAgentContract
// satisfies it). A seam rather than a *pipeline.Pipeline so this routing
// package does not drag the whole media pipeline into its dependency graph,
// and so tests fake local execution with a closure. The options carry a
// decided seat and placement (LocalOptions); the runner never places on its own.
type LocalRunner func(ctx context.Context, contract core.AgentContract, opts LocalOptions) (core.AgentWireResult, error)

// PlacedResult is one subtask's outcome: where it ran, what came back, and
// what the delegator-side verification found. Beyond the placement/result
// core, JobID and PlacementReason are carried for the surfaces and the
// telemetry (mis-routing hygiene: every result names its node, seat, and why
// it landed there), and Err marks a transport/config FAILURE — distinct from
// a defer, which is the node honestly reporting it could not complete.
type PlacedResult struct {
	Node               string
	Seat               string
	Result             core.AgentWireResult
	AcceptanceFailures []string
	// AcceptanceAuthority is the authority the contract closed at (core.ClosedAt, set by gradeAcceptance): the weakest
	// authority among its checks, stated only when every check passed and nothing else failed the result. AuthNone says
	// nothing (a contract with an unqualified check, a failure, an unknown, an excused fingerprint, or no answer).
	AcceptanceAuthority core.Authority
	JobID               string
	// intentRecorded / orphanable steer the intent ledger (intent.go): the
	// first says a "d" line exists for JobID; the second marks the exits
	// (cancel, owned-job poll deadline, queued give-up) where the node may
	// still finish the job — those stay OPEN for the recovery pass.
	intentRecorded bool
	orphanable     bool
	// withdrawn: the node CONFIRMED it took the job back before starting it (DELETE
	// /fleet/jobs/{id}, ADR 0064). Nothing is left on the node for the recovery
	// pass, so the intent closes as "withdrawn" instead of staying open.
	// queuedWait is the time the job provably spent in that node's backlog, which
	// placements.noteRefusal credits back when the result is re-placed.
	withdrawn bool
	// nodeNeverRan holds what a node said when its OWN terminal state told the
	// delegator the job never ran — "reaped: ..." (nobody polled it within the poll
	// lease: this process was away) or "withdrawn: ..." — read on a poll. Nothing ran,
	// so the subtask is re-placeable like a confirmed withdrawal, and the intent closes
	// as "never started: <this>", the note recovery writes for the same observation.
	nodeNeverRan string
	// nodeTerminal marks a failure the poll loop READ off the node as its last word on the job:
	// it ended in error, its result could not be decoded, or the node denied holding it after
	// the bounded re-dispatches. The job is over, or gone, so when the call deadline cuts such
	// an outcome (cutOutcome) it says the job had ended and not that it is still on the node,
	// and the intent closes as the terminal observation it is. It is the producer that says
	// so, not the error text, because other failures of an acked job leave its fate unknown: a
	// cancelled poll and a poll deadline are the give-up's (orphanable), and a refused
	// re-dispatch may have landed.
	nodeTerminal    bool
	queuedWait      time.Duration
	PlacementReason string
	// PinReason is the closed reason the call was pinned under (RunOptions.PinReason, ADR 0078), stamped on the
	// result as the caller receives it and on its ledger row; "" for a call with no reasoned pin.
	PinReason string
	// deadlineCut marks a result the whole-call deadline (RunOptions.Deadline)
	// published as a budget defer instead of an answer: the subtask was still
	// running, waiting or not yet started when the call ended. Set by
	// cutByDeadline and the deadline's own constructors; never published.
	deadlineCut bool
	// abandoned marks the result the call published for a subtask whose goroutine had not returned when the unwind
	// allowance ran out (abandoned in calldeadline.go): a seat or a node may still be running it, so the call cannot say
	// where it was placed, and a hinted call's clause says that instead of "no node took it" (hintClause). Never published.
	abandoned bool
	// queueSeen is what the queue holder said about this job when the call deadline
	// took a last look (calldeadline.go lookAtHolder): "accepted", "running", "absent"
	// (no such job) or "unknown" (no answer inside the look). "" = never looked.
	queueSeen string
	// rescueSpent is the wall a delegator-side rescue of this result's finished answer
	// spent and FAILED to turn into a validated object (rescueSchemaMiss); zero when no
	// rescue ran or it delivered. It is delegator time spent on neither node's work, so a
	// seat-down defer that is re-placed after it is credited it back (admissionCredit),
	// like the wait its node spent on the dead seat.
	rescueSpent time.Duration
	// Err is non-empty when the subtask FAILED for transport/config reasons
	// (dispatch refused, auth rejected, undecodable result). Counted in
	// Summary.Failed, never in Deferred — eight quiet defers and one broken
	// wire are different outcomes and must read differently.
	Err string
	// wallMs is the DELEGATOR-observed wall (placement through verification) —
	// the round-trip number the break-even telemetry wants; the node's own
	// wall stays in Result.WallMs.
	wallMs int64
	// pair* pin the NVIDIA PAIR card identity of this subtask (pairevents.go):
	// the model/engine named by the first in-flight frame and its clock, so the
	// terminal frame merges into the same card. Empty = no frame was emitted.
	pairModel   string
	pairEngine  string
	pairCreated int64
	pairStarted int64
	// pairNode / pairAliases are the node names the in-flight frame carried
	// ("" = this box); the terminal frame names the same node, plus whatever
	// the node called itself on the wire, so the card never moves.
	pairNode    string
	pairAliases []string
	// remotesUnreachable marks a LOCAL placement that happened while the
	// configured fleet was failing its health probe. The work is fine (an idle
	// or queued local box is the quality-first placement either way) but the
	// fleet is not, and route=auto used to discard that verdict entirely — so a
	// fleet down for a week read green forever. Counted into
	// Summary.Infrastructure, which is what makes it audible.
	//
	// On a waitCapacity sentinel it is the same fact carried forward: the remotes were
	// failing their health probe when the placement was decided and the local seat was
	// reserved or fenced by a lease, so the subtask waits instead of falling local. The wait
	// stamps it on whatever ends it (awaitCapacity) unless a remote answered while it waited.
	remotesUnreachable bool
	// RetriedOn names the node a second attempt ran on after the first attempt
	// came back failed_verification or an honest abstention. The published
	// result is the BETTER attempt (a success beats any failure; otherwise the
	// first attempt stands), and RetryNote says what the other attempt did, so a
	// reader can tell "the 27B fixed what the 4B missed" from "both seats missed".
	// Empty when no retry ran (no different node was available, or the first
	// attempt did not qualify).
	RetriedOn string
	RetryNote string
	// retryRecovered: this published result IS the retry, and it succeeded where
	// the first attempt did not (Summary.RetryRecovered). ranLocal records where
	// the attempt ran so the retry can pick a DIFFERENT node.
	retryRecovered bool
	// retryRanAndFailed: the retry ran on a seat and produced no verified digest (the
	// per-page cap's own reading of its result, pageIssueFailed) while the published
	// result is still the FIRST attempt. A first attempt that stands as a seat-down
	// defer says nothing about the page, because its seat died; the retry seat is the
	// one that ran it, so the cap reads the retry's verdict from here.
	retryRanAndFailed bool
	// retried: a verification retry actually RAN for this subtask. Deliberately
	// not `RetriedOn != ""`: a retry whose own placement was refused by every
	// node names no node at all, and keying the tally off the string silently
	// dropped exactly the retries most worth counting.
	retried  bool
	ranLocal bool
	// ranBase is the dial base this attempt used ("" for local). It is what the
	// re-placement loop excludes on, rather than the node id: a node that
	// answered health without a node_id would otherwise collide with every
	// other such node under one empty key, and the dial target is the thing
	// actually being re-tried.
	ranBase string
	// viaQueue marks an answer the queue lane took (takeQueueAnswer): ranBase is then the HOLDER, the one URL this side
	// asked, which is not the node that ran the job and is no place to send a rescue. ran_on reads ranBase and stays
	// "node"; the rescue origin does not (rescueSchemaMiss).
	viaQueue bool
	// PollNote (register D-116) names the bound the delegator polled a
	// timeout_auto contract at and where the number came from — the node's
	// advertised seat rate, the node's own published wall, or the cap when it
	// advertised neither. Empty for a contract that named its own timeout_sec,
	// which is polled at timeout_sec + grace exactly as before.
	PollNote string
	// refusalStatus records a DISPATCH-TIME refusal: the node answered the
	// dispatch with something other than the one 202 ack (the status it sent),
	// or the delegator never reached it at all (0). Only set when Err is also
	// set. It is the input to replaceableRefusal — the class decision keys on
	// the STATUS the node actually sent, never on the text of an error string.
	//
	// The one refusal that is not dispatch-time is a job the node itself says it
	// never ran (ADR 0064): refuseAsWithdrawn and refuseAsNeverRan file it AFTER
	// the ack, as a 503 the delegator assigns to that outcome and the node never
	// sent, so the machinery that re-places a refused dispatch takes it as the
	// capacity refusal it amounts to. Err is set on it too.
	//
	// refused is separate from `refusalStatus != 0` because 0 is a real value
	// (a transport failure), and because a marshaling error inside dispatch is
	// a delegator bug rather than any node's answer.
	refused       bool
	refusalStatus int
	// refusalRow marks the result of an attempt whose corpus row was written as a REFUSAL (the node answered the
	// dispatch with a capacity refusal, 503 or 429): the row records a place in line, not the subtask's outcome,
	// and the delegation log says so (delegationLogLine.Refusal). Set by attempt's finish, on the attempt's own
	// result only; every row the engine writes for an outcome (settle, exhaustedSettled) clears it, because those
	// copy a refused attempt's result and are the terminal record that refusal is not.
	refusalRow bool
	// retryAfterSec is the node's own Retry-After (seconds) on a 503 dispatch
	// refusal, 0 when it sent none. It is a HINT about when the node expects a
	// worker to free, never a promise, and it is NEVER slept on inside
	// runRemote (ADR 0063): the refusal returns at once, and noteCooldown turns
	// the hint into a per-node cooldown that only the capacity wait consumes.
	retryAfterSec int

	// Replacements counts how many times this subtask was RE-PLACED on a
	// different node after a node REFUSED it at dispatch. 0 on the
	// overwhelming majority of results, so a healthy run publishes exactly
	// what it published before this existed.
	Replacements int
	// ReplacementNote names every node that refused and what it said, in the
	// order they were tried. Present whenever Replacements > 0 — on the result
	// that finally ran as much as on the one that ran out of nodes — because a
	// fleet quietly shedding load onto one box otherwise looks identical to a
	// healthy one.
	ReplacementNote string
	// CapacityWaitSec (0.113.18) is how long this subtask waited for a node to
	// have room before one took it — the capacity wait (the call's deadline less a
	// reserve, else agent_placement_wait_sec; ADR 0073) — and is NOT part of timeout_sec. waited marks it for Summary.Waited; shed
	// marks a sheddable subtask that found no idle node (Summary.Shed).
	CapacityWaitSec float64
	// PlaceKeeping and RetryAfterSec (GPU routing P7) are what a capacity wait that ended with
	// nothing taking the work knew about the places it stood in: every node it stood behind with
	// what it waited on and when that is expected to clear, and the soonest of those as a hint
	// for when a re-ask can succeed. Empty on every result that did not end that way.
	PlaceKeeping  []PlaceWait
	RetryAfterSec int
	waited        bool
	shed          bool
	// Unplaced marks a result NO node ran (a capacity defer, a shed, or a
	// route=remote with nothing eligible): it carries Replacements for the
	// refusals it collected, but must never be tallied as a replacement that
	// RECOVERED — nothing took the work.
	//
	// EXPORTED because a SURFACE has to tell "the node answered and deferred"
	// from "the node was never exercised": both carry Deferred, but only the
	// first one's Node and Seat describe the box under test. The second stamps
	// the DECIDING (local) box, and fleet-smoke rendering that as the node under
	// test made a dead remote read as a failure of the operator's own machine
	// (issue #250).
	Unplaced bool
	// waitCapacity is attempt()'s SENTINEL: the placement it computed can only
	// run on a seat a text lease reserves — or, on a composite box, the
	// decided seat would evict a busy pair seat (decided set) — so nothing was
	// dispatched, nothing was recorded, and placeAndRun must run the capacity
	// wait (awaitCapacity) with pendingReason as the first refusal. Never
	// published.
	waitCapacity  bool
	pendingReason string
	// gated marks a waitCapacity sentinel raised by the PROCESS GATE rather than
	// by a lease or a composite decision: this process already holds as many
	// dispatches open on the chosen node as its admission ceiling allows, so
	// nothing was sent and the subtask waits in line (processgate.go). Never
	// published, and never a refusal: the node was not asked.
	gated bool
	// fenced marks a waitCapacity sentinel raised because a lease fences every local seat the
	// contract could run on (fencedLocal): nothing was dialled, because the seat would have turned
	// the run away. It is a place in line like a text reservation, but a fence is no
	// "reservation": with the wait off it ends as a capacity defer (the seat's own pre-check
	// would have filed the same class), never as the holder-naming infrastructure defer a
	// reservation gets. Never published.
	fenced bool
	// overflow marks a waitCapacity sentinel raised by a capacity-aware DEAL
	// (route=spread's or route=auto's): every remote with room was already dealt to
	// its headroom by this run, and the local seat was kept out of the subtask's
	// rotation because it read busy. Never published. It is neither a lease (none is
	// held: nothing may claim one cleared, and with the wait off the outcome is a
	// capacity defer, never the holder-naming one) nor a refusal (no node was asked).
	// The wait holds the subtask for the first node that frees - the local seat too,
	// once it no longer reads busy by the reading the deal used.
	overflow bool
	// decided is the placement-table decision behind a waitCapacity sentinel
	// on a composite box (ADR 0052, council R1: the pair's long seat waits for
	// the agent seat to drain). It exists so the capacity wait can tell "the
	// local seat is reserved by a lease" from "the decided seat is waiting for
	// an eviction to become free": the first ends in the holder-naming
	// deferral, the second re-runs the decider on every tick and RUNS on the
	// decided seat once the pair is idle — or when the wait expires — and must
	// never produce the lease text or a capacity defer. nil on every
	// pre-composite sentinel.
	decided *placetable.Decision
	// Placed (ADR 0052, 0.116.0) is the placement decision this result was
	// produced under — the block published as `placed` beside the untouched
	// `placement` reason string. Local: the delegator's own decision, replaced
	// by the pipeline's stamp once the run answers (identical by construction).
	// Remote: the delegator's decision over the node's advertised rows until
	// the node's wire result carries its own (the node re-decides with its live
	// guards, council R5). nil on a plain box and for a plain node, so every
	// pre-0.116 result publishes byte-identically.
	Placed *core.Placed
}

// PlaceWait is one place in line a subtask stood in when its capacity wait ended with nothing
// having taken the work (GPU routing P7, invariant I4: a busy card is a place in line). It is
// what the caller re-asks by: which node, behind what, and when that is expected to clear.
type PlaceWait struct {
	// Node is the node the subtask stood in line for (its node_id; the delegator's own box for
	// the local seat).
	Node string `json:"node"`
	// On is what it stood behind: lease (a lease on the cards the contract needs), queue (the
	// node's own admission ceiling or no room), backlog (a queue longer than the caller will
	// wait), cooldown (the node's own refusal), seat (another vLLM seat holds the local cards
	// and loading the agent seat would unload it).
	On string `json:"on"`
	// Detail is the sentence behind On.
	Detail string `json:"detail"`
	// EtaSec is when the place is expected to clear, in seconds from the end of the wait; 0 when
	// nothing says (an overdue lease, a node that publishes no estimate).
	EtaSec int `json:"eta_sec,omitempty"`
}

// Summary is the per-run outcome tally, reported AT THE TOP of every surface's
// result (roast delta 14): eight quiet defers must read as a loud outcome,
// not eight green jobs.
type Summary struct {
	// Waited / Shed (0.113.18): subtasks that waited for fleet capacity before a
	// node took them, and sheddable subtasks shed for lack of an idle node.
	Waited int
	Shed   int

	Succeeded          int
	Deferred           int
	FailedVerification int
	Failed             int
	// Infrastructure counts the results whose story is a broken stack rather
	// than the work: defers whose defer_class says so (infrastructure|config),
	// PLUS a local placement taken while every configured remote was failing
	// its health probe. It is an ANNOTATION, not a fifth bucket — the four
	// counts above still add up to len(results) without it. It exists because a
	// node with a dead llama-swap otherwise reports exactly what a small model
	// honestly abstaining reports, and both exit 0. A non-zero Infrastructure
	// makes the CLI exit non-zero.
	//
	// Contract-side defers (core.DeferClassContract — no output_schema, past
	// the origin hop, too big for any advertised ceiling) are deliberately NOT
	// counted: the fleet is healthy and the CALLER has a contract to fix, so
	// telling the operator a box is broken sends them to the wrong machine.
	Infrastructure int
	// LostToStack counts the subtasks that DELIVERED NO USABLE RESULT because the
	// stack failed them — a defer whose class is infrastructure|config. It is the
	// half of Infrastructure that represents lost WORK, split out because
	// Infrastructure conflates two states a caller must act on differently:
	//
	//	remotesUnreachable → a result that SUCCEEDED anyway (local took it)
	//	BrokenStackDefer   → a subtask whose contracted output never arrived
	//
	// "No usable result" is not always "no bytes", and the distinction is load-
	// bearing because the PREDICATE is what ships: pipeline/agenttask.go sets
	// wire.Output before the re-pack and keeps it populated on every failure
	// branch below, so the CALLER still receives the loop's answer in the
	// result's `output` field. (It is preserved for the caller, NOT for
	// acceptance. THE RULE, binding on every caller of EvalAcceptance in any
	// package: never evaluate a DEFERRED result. A defer's preserved prose was
	// never offered as an answer, so checking it manufactures failures about
	// content nobody claimed — an honest defer would land in the ledger and the
	// corpus as a verification failure. This package keeps the rule by guarding
	// both call sites on !wire.Deferred, pinned by
	// TestRunLocalDeferSkipsAcceptance; mcpserver's ask lane keeps it by
	// returning on wire.Deferred before it ever reaches the call. Stated as a
	// rule rather than as a tally of call sites on purpose: the tally said "both"
	// while there were three, and a doc comment reads back as evidence the code
	// obeys it.) So a subtask whose agent loop
	// FINISHED and whose re-pack seat was unreachable publishes prose beside
	// defer_class:"infrastructure" and IS counted here. That is
	// deliberate, not an oversight: a contract carrying an output_schema asked for
	// a mechanically checkable deliverable, and prose with no `structured` is not
	// one — the contracted output genuinely did not arrive, so the caller must be
	// told rather than left to merge an unchecked answer. Read this field as "the
	// contracted output was lost", never as "the result is empty".
	//
	// Without the split, "was anything lost?" had no answer in the summary, and
	// the MCP surface approximated it with `Succeeded == 0` — which silenced a
	// subtask genuinely eaten by a broken box the moment ANY sibling succeeded.
	// `Deferred > 0 && Infrastructure > 0` is NOT the same predicate and cannot
	// replace this: a contract-classed defer sitting beside a fleet-down local
	// success satisfies both while nothing was lost to the stack at all.
	//
	// Like Infrastructure it is an ANNOTATION, not a fifth bucket — it is a
	// subset of BOTH Deferred and Infrastructure, and the four counts above
	// still add up to len(results) without it.
	LostToStack int
	// CorpusRows*/LedgerRows* count the telemetry rows this run ATTEMPTED and
	// LOST. Telemetry never fails the work, but "this run's corpus rows are
	// LOST" reads identically whether 1 of 8 or 8 of 8 failed — and the MCP
	// caller, this lane's primary consumer, could not see it at all until these
	// rode the published summary.
	//
	// Attempted ships beside Lost because the doc promised "N of M" and only N
	// was published, leaving the caller unable to reconstruct M. And the TOTAL
	// loss — ledger.Open itself failing — used to increment nothing at all
	// (record()'s `if r.led != nil` guard), so the worst case published
	// byte-identically to a run that wrote every row.
	CorpusRowsAttempted int
	CorpusRowsLost      int
	LedgerRowsAttempted int
	LedgerRowsLost      int
	// Retried counts subtasks that got a second attempt on a different node
	// after a failed_verification / abstention; RetryRecovered is the subset the
	// second attempt turned into a success. Annotations, not buckets: the four
	// outcome counts above still add up to len(results).
	Retried        int
	RetryRecovered int
	// Replaced counts subtasks RE-PLACED at least once after a node refused
	// them at dispatch. ReplacementRecovered is the subset that then reached a
	// node which TOOK the work — the subtask stopped being a refusal.
	//
	// Read ReplacementRecovered precisely: it says the work was PLACED, not
	// that the answer was good. A re-placed subtask whose seat then deferred,
	// or whose result failed acceptance, still counts here, because the defect
	// this fixes is work that nobody ran at all. The four outcome counts above
	// say what the answer was. Annotations, not buckets.
	Replaced             int
	ReplacementRecovered int
	// Quarantined counts nodes that FLIPPED into quarantine during this run
	// (two document-fingerprint failures inside the TTL). Batches counts the
	// deals RunBatched ran: 1 for a batched call of up to MaxBatchSubtasks (it was
	// 2 for 9 to 12 pages), more only for a longer list (ADR 0076); 0 for a plain Run.
	Quarantined int
	Batches     int
	// Skipped counts subtasks RunBatched never attempted because an earlier
	// chunk returned a top-level error — they are in no result and in no other
	// counter, so without this they would vanish from the summary.
	Skipped int
}

// Run executes subtasks (bounded concurrency), placing each per route:
//
//	"auto"   — gate.go's Place: an idle local node always wins; a held GPU
//	           lease considers gate-passing remotes; no eligible remote =
//	           queued-local (quality-first).
//	"local"  — forced local, no network at all.
//	"remote" — forced remote; with no gate-passing remote the subtask DEFERS
//	           loudly rather than silently overriding an explicit route.
//
// The returned error is reserved for CONFIG mistakes (bad route, subtask
// bounds, a non-tailnet remote) where nothing executed; per-subtask transport
// failures land in PlacedResult.Err / Summary.Failed instead, so one broken
// node cannot void seven finished results.
// RunOptions carries caller-owned state a Run consults but does not own.
type RunOptions struct {
	// Quarantine, when set, excludes nodes it blocks from placement and is
	// struck on every document-fingerprint verification failure. Owned by the
	// caller so it survives RunBatched's chunks (and, in the MCP server, the
	// process lifetime).
	Quarantine *Quarantine
	// Priority is the scheduling band every subtask of this run is dispatched
	// with (core.BandSheddable / BandNormal / BandUrgent; clamped). A sheddable
	// run takes only idle fleet capacity: a node without an idle slot refuses it
	// (503, re-placeable), and with no idle node anywhere the subtask is SHED
	// (deferred, class capacity) instead of waiting or queuing behind
	// production work. Measurement and gate traffic runs at -1.
	Priority int
	// Tenant identifies the delegator to the fleet (one MCP server = one Claude
	// session; one CLI process = one tenant) so a node can round-robin its
	// backlog across tenants. Empty = anonymous, arrival order on the node.
	Tenant string
	// LocalDecider is the placement decision a LOCAL run is made under on a
	// composite box (ADR 0052). nil = production: placetable.Decide over the
	// config's layers with ONE memoised placetable.NewSnapshot per run (2 s
	// TTL), so a 32-subtask spread deciding per subtask per tick execs
	// nvidia-smi once, not 32 times. Tests inject readers here; a box with no
	// layers never calls it (the zero Decision is the pre-layer behaviour).
	LocalDecider func(ctx context.Context, contract core.AgentContract, st Subtask) placetable.Decision
	// Rescue re-packs a finished answer whose structured re-pack failed on the
	// executing node (register C-66, PR-4): the delegator holds the answer, so it
	// structures it itself instead of counting a finished loop as lost work.
	// nil = no rescue, the behaviour before the rescue existed: the defer is delivered as it is.
	// The surfaces that own a pipeline wire pipeline.RescueRepack here.
	Rescue RescueFunc
	// Deadline, when non-zero, is the instant the WHOLE call must be over: the
	// MCP doors set it below the client's own abort (config.CallDeadline) so a
	// call returns what has finished instead of being dropped with it (ADR 0065,
	// register C-67). At Deadline every subtask still running is cancelled and
	// published as a budget defer "call deadline reached; N unfinished" beside the
	// finished results, and nothing further is started. The zero value is no
	// deadline — every caller that is not an MCP door, and every pre-0065 call.
	Deadline time.Time
	// OnProgress, when set, is told each time a subtask starts and each time one
	// ends (progress.go). It is called from the run's own goroutines, concurrently,
	// so it must be safe for that and must not block: the MCP doors hand each event
	// to a channel. Nothing in the engine reads it back.
	OnProgress func(ProgressEvent)
	// RosterOnly says the call's remotes list was named by a MODEL (the MCP agent_delegate door), so
	// it may only NARROW the configured fleet: an entry outside cfg.DelegateRemotes, or any entry on a
	// box that configures none, is refused before anything is dialled (CheckRosterRemotes). The
	// operator's own surfaces (the CLI verbs, fleet-smoke) leave it false and keep naming any tailnet
	// node, which is how a node that has not joined the roster yet is tested.
	RosterOnly bool
	// PinReason is the closed reason (privacy, locality, measurement, operator: PinReasons) a caller gave for
	// pinning the call with route local or remote (ADR 0078). A reasoned pin is authoritative, exactly as the route
	// always was, and the reason is recorded on every ledger row and result. CheckPinReason refuses a value outside
	// the set, one the route does not take, and one given with a route that is not a pin, before any placement.
	PinReason string
	// PinNeedsReason is set by the doors that offer pin_reason (agent_delegate, offload_research and their CLI
	// verbs): a route local or remote WITHOUT a PinReason is then a hint that placement may override (see pin.go).
	// The zero value keeps today's authoritative pin for every caller that has no reason channel (fleet-smoke, the
	// review lane's remote fallthrough, offload_ask and agent_run).
	PinNeedsReason bool
	// RemoteRetryOnly keeps the second chance of a route=remote call (a failed verification, an abstention, a seat-down
	// re-issue) on a fleet node: another node takes it or it is not run, and it never lands on this box's seat. It is
	// the narrow half of a reasoned remote pin and no more: it records no pin_reason, so the call stays counted as
	// unreasoned, which is what it is. It is for a door with no reason channel whose own caller already moved the work
	// off this box (offload_ask's cross-seat retry), where the bare remote route's retry would hand it straight back.
	// It does nothing for a route other than remote, for a remote hint (a hint is placed as auto), or beside a
	// PinReason, which already says it.
	RemoteRetryOnly bool
	// PinTally, when set, is the accounting the call's pins are added to (the MCP server's, which offload_status
	// publishes). nil = not counted: the CLI verbs and fleet-smoke are one-call processes with nobody to report to.
	PinTally *PinTally
	// call is the deadline's shared state (calldeadline.go): RunBatched builds it
	// once so every chunk of a batched call counts its unfinished subtasks against
	// the whole call. Nil = RunWith builds its own from Deadline.
	call *callDeadline
}

// DefaultTenant is the tenant id a delegator process identifies itself with
// when the caller supplies none: host, pid and the process start second — one
// MCP server (one Claude session) or one CLI invocation is one tenant, which
// is the granularity the node's round-robin exists for. LOCAL_OFFLOAD_TENANT
// overrides it (a gate script emulating K sessions names them).
func DefaultTenant() string {
	if v := strings.TrimSpace(os.Getenv("LOCAL_OFFLOAD_TENANT")); v != "" {
		return v
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "delegator"
	}
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), processStart.Unix())
}

var processStart = time.Now()
