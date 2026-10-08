// run.go is the shared delegation execution engine (Task 6, §S4): both
// delegator surfaces — the MCP agent_delegate tool and the `delegate` CLI
// verb — hand their prepared contracts to Run, which places each subtask
// (gate.go's quality-first Place), executes it locally in-process or remotely
// over the fleet wire, then applies DELEGATOR-SIDE acceptance before anything
// counts as a success. A schema-valid result that fails an acceptance check is
// flipped to failed-verification — the wrong-valid-schema hole this engine
// exists to close (roast delta 3): the remote node can prove shape, only the
// delegator can prove the content is the content it asked for.
//
// Job protocol (roast delta 14): the DELEGATOR mints every job id
// ("agd-" + crypto/rand hex) so a re-dispatch on transport doubt carries the
// SAME id and the node's store re-acks 202 idempotently — a lost ack can never
// buy a duplicate run. Polling stops hard at TimeoutSec + a grace window,
// because a delegator that polls forever pins a goroutine on a dead node.
//
// What the deadline PRODUCES depends on whether the node ever reported OWNING
// the job — a 200 whose state is accepted/running/done/error. One that did gets
// an honest "poll deadline …" defer; anything else gets a FAILURE. Reachability
// is NOT ownership: a 404 is a positive denial that the job was ever there, and
// a 5xx says nothing about what the node holds. The delegator may report what a
// node said about its own work; it may never author a report on the behalf of a
// node that never claimed the work.

package delegate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"io"
	"log"
	"maps"
	// mathrand is the JITTER source only. crypto/rand above owns anything an
	// operator or another process could observe (job ids); a sleep length is
	// neither, and a lock-free generator is what a fan-out of dispatch
	// goroutines should be calling.
	//
	// semgrep's p/golang flags every math/rand import as a crypto finding. It is
	// refuted here rather than suppressed globally: the only consumer is
	// jittered(), whose output is a sleep duration nobody can observe and which
	// guards nothing. Seeding this from crypto/rand would buy no property and
	// would put a syscall on the dispatch path. If a SECOND consumer is ever
	// added, delete this line and make it argue its own case.
	mathrand "math/rand/v2" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used — timer jitter only, not a security use
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	// Aliased: this file's `placement` struct (a resolved "run it HERE") predates
	// the package and is used at every placement site; the package is the
	// composite tier's decision TABLE (ADR 0039), so the alias names what it is.
	placetable "github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatguard"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// LocalOptions is what a delegator that already DECIDED where a contract runs
// hands the local runner (ADR 0039): the seat to run instead of the planner
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
	JobID              string
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
	// on a composite box (ADR 0039, council R1: the pair's long seat waits for
	// the agent seat to drain). It exists so the capacity wait can tell "the
	// local seat is reserved by a lease" from "the decided seat is waiting for
	// an eviction to become free": the first ends in the holder-naming
	// deferral, the second re-runs the decider on every tick and RUNS on the
	// decided seat once the pair is idle — or when the wait expires — and must
	// never produce the lease text or a capacity defer. nil on every
	// pre-composite sentinel.
	decided *placetable.Decision
	// Placed (ADR 0039, 0.116.0) is the placement decision this result was
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

const (
	// maxSubtasks bounds one Run (the MCP arg schema mirrors it).
	maxSubtasks = 8
	// MaxSubtasks is the same bound, exported for callers that size a call against RunWith's.
	MaxSubtasks = maxSubtasks
	// maxBatchSubtasks is how many subtasks RunBatched deals in ONE joint deal (ADR 0076): twice RunWith's
	// bound, because the one caller that batches (offload_research, at most 12 pages) used to be cut into
	// sequential chunks of eight, and a chunk waited on the slowest page of the one before it. A longer list
	// is still cut, into consecutive deals of this size.
	maxBatchSubtasks = 16
	// MaxBatchSubtasks is the same bound, exported for callers that batch (RunBatched).
	MaxBatchSubtasks = maxBatchSubtasks
	// runConcurrency is the FLOOR of a call's fan-out and the most one node that publishes no
	// max_concurrent_jobs is given of it. 4 overlaps remote polls, and it is all a call has when nothing sizes
	// it: a call that has a deal (route=auto, remote or spread) is as wide as the deal (dealParallelism, ADR
	// 0076). It once also stood for "small enough that a local fallback burst cannot stampede the one GPU";
	// that is the local run-cap line's job now (pipeline/agenttask.go, register C-42/C-60: runs past
	// fleet_max_concurrent_jobs wait their turn in the seat's own FIFO), and the deal counts that line.
	runConcurrency = 4
	// dispatchAttempts: the initial POST + one retry on transport doubt
	// (roast delta 14's 202-reack — same job id, so the store dedupes).
	dispatchAttempts = 2
	// maxRedispatches bounds 404-triggered re-dispatches during polling: a
	// node that keeps forgetting the job after two re-acks is broken, and
	// re-POSTing forever would re-run the contract on every node restart.
	maxRedispatches = 2
	// maxRemoteReplacements bounds how many ADDITIONAL REMOTE nodes one subtask
	// may be offered to after a node refused it at dispatch. It is spent from
	// ONE per-subtask ledger (see the placements struct), so the verification
	// retry draws from what the first attempt left rather than starting a fresh
	// copy of the bound.
	//
	// Local is NOT counted against it — the fallback to the one seat that
	// always exists is reserved, so a wide roster can never spend the bound
	// before reaching it.
	//
	// The resulting ceiling per subtask is FIVE placements, and the arithmetic
	// is worth spelling out because a shorter number was wrong once already:
	//
	//	1  the first placement
	//	2  at most maxRemoteReplacements re-placements onto further remotes
	//	1  at most one fall-back to the local seat (bounded by the ledger's
	//	   `tried` set, not by this constant)
	//	1  at most one verification-retry placement — a SEPARATE mechanism with
	//	   its own bound of exactly one, which is why it is not folded in here
	//
	// and every one of those five targets a dial base the subtask has not used
	// before, because they all consult the same `tried` set.
	//
	// Why 2, and why a bound at all:
	//
	//   - Walking the whole roster is its own failure mode. Each refused
	//     placement costs up to dispatchAttempts × dispatchRequestTimeout = 60s
	//     of dial time before a transport verdict, so an unbounded walk turns
	//     one saturated fleet into minutes of wall clock spent collecting
	//     refusals — and the contract's budget is being spent the whole time.
	//   - The first choice plus two alternates covers a three-node remote fleet
	//     completely. A refusal that survives three DISTINCT nodes is a
	//     fleet-wide condition (everything saturated, everything draining), not
	//     a node condition, and a fourth dial does not fix a fleet-wide one.
	//   - Local is the one seat that is always able to take the contract, so
	//     the bound is a bound on HUNTING, not on getting the work done.
	//
	// Two independent things also bound the loop tighter in practice: each node
	// is tried at most once (the base-URL exclusion set), and every placement
	// must fit in what is LEFT of the contract's timeout_sec.
	maxRemoteReplacements = 2
	// dispatchRequestTimeout / pollRequestTimeout bound ONE HTTP exchange;
	// the overall poll deadline is the contract's business, not the client's.
	dispatchRequestTimeout = 30 * time.Second
	pollRequestTimeout     = 15 * time.Second
	// maxFleetBody bounds a decoded dispatch/poll response (a wire result is
	// small; the cap only guards a misconfigured base).
	maxFleetBody = 4 << 20
)

// jitterFrac is how far a fixed sleep is randomised either way (+/-20 %).
//
// Every dispatcher in this fleet sleeps on the SAME constants — pollEvery,
// placementPollInterval, refusalCooldown — so K sessions that start within a
// second of each other stay in lockstep for the whole run: they re-read health
// together, re-dispatch together and re-ask a refusing node together, which is
// exactly the convoy that makes a busy node look busier than it is. Randomising
// each sleep by a fifth spreads them apart within a few ticks and costs nothing
// else: the cadence's MEAN is unchanged, and no caller can observe an individual
// sleep's length.
//
// 20 % is the usual choice for this (it is what exponential-backoff jitter
// recipes use) and is deliberately small: a bigger spread would start to change
// how quickly the capacity wait notices a node that freed.
const jitterFrac = 0.2

// jittered scales d by a uniform factor in [0.8, 1.2]. A non-positive d is
// returned untouched — a test that compressed a clock to zero means zero.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	j := time.Duration(float64(d) * (1 - jitterFrac + 2*jitterFrac*mathrand.Float64()))
	if j <= 0 {
		j = 1
	}
	return j
}

// jitteredWithin is jittered, clamped so the sleep can never run PAST the
// deadline it lives under. Jitter is a de-synchroniser, never a licence to
// overshoot a budget: `left` is what remains of that deadline, and a
// non-positive `left` means there is no deadline left to protect (the caller's
// own loop condition decides what happens next).
func jitteredWithin(d, left time.Duration) time.Duration {
	j := jittered(d)
	if left > 0 && j > left {
		return left
	}
	return j
}

// pollEvery/pollGrace are vars (not consts) so tests compress the cadence;
// production never mutates them. Grace rides ON TOP of the contract's
// TimeoutSec: the node reports TimeoutSec as its wall (the run's expectation,
// ADR 0055: what ends a run is a stall or the node's ceiling), so the delegator
// allows that plus transport slack before declaring the poll dead, and holds the
// poll open past it while the node reports progress.
var (
	pollEvery = 3 * time.Second
	pollGrace = 60 * time.Second
	// pollSecond is the unit a contract's wall (an integer number of SECONDS)
	// is converted to wall clock with. One real second in production; a test
	// compresses it because an auto contract's bound never falls below
	// AgentTimeoutSecDefault (300 s) and a deadline test cannot wait that out.
	// A var for that reason only — production never mutates it.
	pollSecond = time.Second
)

// maxQueuedWait is the ceiling on how long a subtask waits for a node that
// publishes no ETA to START it, and the delegator's own yardstick for a node's
// Retry-After (the node clamps its hint at 300 s and says ">=300 s").
//
// Since ADR 0063 it is no longer THE queue deadline. A node that publishes an ETA
// (queue_wait_estimate_sec, or a recent wall to derive one from) is given
// clamp(1.5 x that wait + 30 s, 60 s, the caller's patience) by queueBudgetFor,
// which is longer than five minutes for a node that honestly says it needs it and
// shorter for one that says a job starts now. A node that publishes nothing is no
// opinion and keeps the wait that always applied: min(patience, maxQueuedWait), so
// a short contract never waits longer for a slot than it was willing to spend on
// the work. The pull-queue lane (queue.go) has no health view to derive from and
// still uses it as its ceiling.
const maxQueuedWait = 5 * time.Minute

// fleetClient rides netguard.SafeTransport like the health client: the
// delegation lane may only ever reach loopback or the operator's tailnet
// (never-cloud, ADR 0001), enforced at every dial. No client-level Timeout —
// per-request ctx deadlines own the budget.
var fleetClient = &http.Client{Transport: netguard.SafeTransport(nil)}

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
	// composite box (ADR 0039). nil = production: placetable.Decide over the
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

// Run is RunWith without options — the signature every existing caller uses.
func Run(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string) ([]PlacedResult, Summary, error) {
	return RunWith(ctx, cfg, local, subtasks, route, remotes, nil)
}

// RunBatched runs contracts in order as ONE joint deal of up to MaxBatchSubtasks
// (16), and a longer list as consecutive deals of that size (ADR 0076).
// offload_research builds ONE contract per usable page and allows 12 URLs per
// call, so a 9–12-page call hit Run's 8-subtask refusal and every page was
// lost (2026-09-01, three sessions); the fix cut it into chunks of eight and
// ran them SEQUENTIALLY, which left chunk two waiting on the slowest page of
// chunk one and a 12-page call using at most four of the fleet's slots. The
// whole list is now dealt at once, across every node, and sized by that deal
// (dealParallelism): the bound the sequential chunks kept, that a burst cannot
// stampede a GPU, is the node's published ceiling and the local seat's run-cap
// line, both of which the deal counts. Results come back in input order;
// Summary counters are summed; the first error is returned WITH the results
// collected before it, so a caller can render the partial work.
func RunBatched(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *RunOptions) ([]PlacedResult, Summary, error) {
	var all []PlacedResult
	var total Summary
	// The call deadline (ADR 0065) covers the WHOLE batched call, not each chunk:
	// one shared state, so the unfinished count in every published reason spans
	// every chunk, and a chunk that would begin after the deadline never does.
	dl := newCallDeadline(ctx, opts, len(subtasks))
	if dl != nil {
		o := *opts
		o.call = dl
		opts = &o
	}
	for start := 0; start < len(subtasks); start += MaxBatchSubtasks {
		end := min(start+MaxBatchSubtasks, len(subtasks))
		chunkOpts := opts
		if opts != nil && opts.OnProgress != nil {
			// Progress counts against the whole call, not against each chunk.
			o := *opts
			o.OnProgress = shiftedProgress(opts.OnProgress, len(all), len(subtasks))
			chunkOpts = &o
		}
		res, sum, err := runWith(ctx, cfg, local, subtasks[start:end], route, remotes, chunkOpts, MaxBatchSubtasks)
		all = append(all, res...)
		total = addSummary(total, sum)
		total.Batches++
		if err != nil {
			// The erroring chunk's own subtasks (no results came back) plus every
			// chunk after it were never attempted: count them, or the summary
			// reads as if the batch simply ended (silent-failure review, 2026-09-02).
			total.Skipped += (end - start - len(res)) + (len(subtasks) - end)
			return all, total, err
		}
	}
	if len(subtasks) == 0 {
		return runWith(ctx, cfg, local, subtasks, route, remotes, opts, MaxBatchSubtasks) // the "at least one subtask" error, unchanged
	}
	return all, total, nil
}

// addSummary sums every int counter of Summary by reflection, so a counter
// added later cannot be silently dropped from a batched total (the reflection
// test pins it). Non-int fields (none today) would need explicit handling.
func addSummary(a, b Summary) Summary {
	av := reflect.ValueOf(&a).Elem()
	bv := reflect.ValueOf(b)
	for i := 0; i < av.NumField(); i++ {
		if av.Field(i).Kind() == reflect.Int && av.Field(i).CanSet() {
			av.Field(i).SetInt(av.Field(i).Int() + bv.Field(i).Int())
		}
	}
	return a
}

// RunWith is Run with caller-owned options (see RunOptions). It refuses more than MaxSubtasks (8) subtasks, which
// is the bound of the MCP door (agent_delegate) and of the CLI verb; RunBatched is the entry that lifts it.
func RunWith(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *RunOptions) ([]PlacedResult, Summary, error) {
	return runWith(ctx, cfg, local, subtasks, route, remotes, opts, maxSubtasks)
}

// runWith is RunWith with the subtask bound as a parameter: RunWith passes maxSubtasks and RunBatched
// maxBatchSubtasks, so a research call's pages are one deal and the doors that cap a call at eight keep doing so
// (ADR 0076). Nothing else about a call depends on the bound.
func runWith(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *RunOptions, limit int) ([]PlacedResult, Summary, error) {
	switch route {
	case "":
		route = "auto"
	case "auto", "local", "remote", "spread", "queue":
	default:
		return nil, Summary{}, fmt.Errorf("delegate: route %q not recognized (want auto, spread, local, remote, or queue)", route)
	}
	// A pin_reason is checked against the route before anything is read or spent (ADR 0078), and a local or remote
	// route that has none, through a door that offers one, becomes a hint placed as auto (pin.go). From here `route`
	// is the route the run APPLIES; pin keeps what the caller asked.
	pin, route, perr := resolvePin(route, opts)
	if perr != nil {
		return nil, Summary{}, perr
	}
	// A browse grant is local work by construction (ADR 0060): the browse tool drives THIS machine's own browser, so a
	// call that carries one cannot have its local route overridden onto another node's. The grant is the reason: a bare
	// local that would have been a hint is pinned under the implied reason locality, and says so on its rows and results.
	if pin.hint && pin.asked == "local" && anyBrowse(subtasks) {
		pin.hint, pin.reason, route = false, PinLocality, "local"
	}
	if len(subtasks) == 0 {
		return nil, Summary{}, fmt.Errorf("delegate: at least one subtask required")
	}
	if len(subtasks) > limit {
		return nil, Summary{}, fmt.Errorf("delegate: %d subtasks exceeds the max of %d", len(subtasks), limit)
	}
	// The whole-call deadline (ADR 0065): from here every placement, probe, poll
	// and local run answers to a context that ends at RunOptions.Deadline. nil =
	// no deadline, and everything below behaves exactly as it did before.
	dl := newCallDeadline(ctx, opts, len(subtasks))
	if dl != nil {
		var cancelCall context.CancelFunc
		ctx, cancelCall = context.WithDeadlineCause(ctx, dl.at, ErrCallDeadline)
		defer cancelCall()
	}
	// route "queue" bypasses the whole push machinery (ADR 0030): the holder
	// owns durability and the claim loops own placement.
	if route == "queue" {
		var rescue RescueFunc
		if opts != nil {
			rescue = opts.Rescue
		}
		results, sum, err := runQueued(ctx, cfg, subtasks, rescue)
		return dl.cutQueued(cfg, subtasks, results, sum, err)
	}
	// Fleet membership is configuration: a call that names no remotes uses the
	// config's delegate_remotes. A call's own list REPLACES it (never merges) so
	// one node can still be targeted deliberately.
	named := len(remotes) > 0
	if !named {
		remotes = cfg.DelegateRemotes
	}
	for _, base := range remotes {
		if err := netguard.TailnetURL(base); err != nil {
			return nil, Summary{}, fmt.Errorf("delegate: remote %q: %w", base, err)
		}
	}
	// A list a model named may only narrow the configured fleet (ADR 0038, amendment 2026-10-07): the
	// roster is what the operator vetted, and a caller-named URL outside it is never dialled.
	if named && opts != nil && opts.RosterOnly {
		// The list that proceeds is the roster's own spelling of it, so the process in-flight gate and every
		// per-base map after this keep one node under one key (CanonicalRosterRemotes).
		canonical, err := CanonicalRosterRemotes(cfg, remotes)
		if err != nil {
			return nil, Summary{}, fmt.Errorf("delegate: %w", err)
		}
		remotes = canonical
	}

	// Ledger row per delegation (roast delta 9). ledger.Open is the existing
	// RECORDLESS-COMPATIBLE append path: a standalone O_APPEND JSONL writer,
	// safe beside a live MCP server's handle — nothing here touches the
	// pipeline's cache/shadow/exemplar stores. Telemetry failure never blocks
	// delivery (same posture as pipeline.record's ignored error).
	var led *ledger.Ledger
	ledgerUnopened := false
	// ledgerDrained is set only when the call deadline passed with subtask
	// goroutines still running: they write their rows when they end, so the ledger
	// closes after the last of them instead of under them.
	var ledgerDrained <-chan struct{}
	if cfg.LedgerPath != "" {
		if l, err := ledger.Open(cfg.LedgerPath); err == nil {
			led = l
			defer func() {
				if ledgerDrained != nil {
					go func() { <-ledgerDrained; l.Close() }()
					return
				}
				l.Close()
			}()
		} else {
			// Every row this run would have written is LOST, and record() must
			// count them as such — see runner.ledgerUnopened.
			ledgerUnopened = true
			// Not fatal — but not silent either. Without this line a run that
			// records nothing looks exactly like a run that records everything,
			// and `local-offload stats` quietly under-reports forever.
			log.Printf("delegate: ledger %s could not be opened; this run records no ledger rows: %v", cfg.LedgerPath, err)
		}
	}

	// Option A durability (intent.go): open the intent ledger for this run and
	// fire the once-per-process orphan recovery in the background. Both are
	// additions — a nil ledger and a failed recovery change nothing about how
	// this run places or reports.
	maybeRecoverOrphans(cfg)
	r := &runner{cfg: cfg, local: local, route: route, pin: pin, remotes: remotes, intent: openIntentLedger(cfg), led: led, ledgerUnopened: ledgerUnopened, pair: pairworkloads.New(pairworkloads.FromConfig(cfg))}
	// Deliver every PAIR frame before returning: the delegate CLI exits right
	// after this, and a frame still in flight dies with the process — PAIR then
	// shows the card "running" until its staleness sweep fails it. Each send is
	// bounded (2 s), so this cannot hold a run hostage to a down PAIR.
	defer r.pair.Wait()
	// Registered after the Wait above, so it runs BEFORE it: from here on a late
	// frame from an abandoned goroutine is dropped, never added to a WaitGroup that
	// is being waited on.
	defer r.shutPair()
	r.call = dl
	if opts != nil {
		r.quarantine = opts.Quarantine
		r.tally = opts.PinTally
		r.priority = core.ClampBand(opts.Priority)
		r.tenant = opts.Tenant
		r.decider = opts.LocalDecider
		r.rescue = opts.Rescue
	}
	// route=spread probes the fleet ONCE per run: every subtask deals itself
	// across the same roster, so per-subtask probing would be N identical GETs
	// and could even deal two subtasks against different snapshots.
	if route == "spread" {
		r.spreadViews, r.spreadBases, r.spreadProbeErrs = r.fetchViews(ctx)
		r.spreadLease = LocalLease(cfg.GPULockPath, cfg.StateDir) // read whole, once; spreadLeaseFor narrows it per contract
		// The local seat's load is read here, once, for the same reason the
		// fleet is probed once: every subtask must deal against ONE snapshot.
		if cfg.SpreadLocalSlot() == config.SpreadLocalAlways {
			r.spreadLocalBusy = busyReading{note: "agent_spread_local_slot=always"}
		} else {
			r.spreadLocalBusy = r.probeLocalBusy(ctx)
		}
		// One line per run says which rule dealt the local slot and from what
		// reading — the placement reasons only mention it when it fired.
		log.Printf("delegate: spread local slot: mode=%s busy=%v inflight=%d (%s)", cfg.SpreadLocalSlot(), r.spreadLocalBusy.busy, r.spreadLocalBusy.inflight, r.spreadLocalBusy.note)
		// The deal is computed HERE, once, over every subtask at once —
		// dealSpread's comment carries the proof that a per-subtask pick cannot
		// hold the one-per-seat-per-cycle invariant. It must run before the
		// goroutines below: they read it, they never build it.
		r.spreadDeal = r.dealSpread(subtasks, r.localView())
	}
	// route=auto/remote: ONE joint deal over ONE fleet snapshot (W-06,
	// register S-11/S-13) — the same reason route=spread already probes and
	// deals once. Before this, every subtask's attempt() probed the fleet and
	// called Place independently, so runConcurrency siblings could read the
	// same free slot within milliseconds of each other and pile onto it.
	if route == "auto" || route == "remote" {
		leaseInfo := LocalLease(cfg.GPULockPath, cfg.StateDir)
		// W-01 (register S-01): busy = the lease, OR the local seat's own
		// in-flight count at or past the fleet's own concurrency cap, OR a
		// load in progress — the SAME formula attempt() used per-subtask,
		// now read once so the whole batch agrees. route=remote forces busy
		// unconditionally: local is never a placement for an explicit remote.
		busy := route == "remote"
		// The local seat's run-cap room is read ONCE here, for the whole run (ADR 0076): the gate below needs it
		// before the deal, and the deal counts what it commits to the seat against it. route=remote never deals
		// the seat, so it reads none.
		room, roomNote := 0, ""
		if route == "auto" {
			busy = r.readAutoLocalSlot(ctx, leaseInfo, subtasks)
			room, roomNote = r.localRunCapRoom()
			// A remote hint (ADR 0078) is placed remotes-first, as route=remote is: the deal treats the seat as
			// unavailable whatever it read. The reading is still taken and kept, unlike route=remote's, because the
			// hint may fall back to the seat where remote would defer, and that fallback must still wait for a lease
			// or for the vLLM seat that holds the cards (autoDealBusy), never load over them. hintSeatFree is what
			// the seat read BEFORE the forcing: the deal may give the seat what no remote has room for only if it
			// is free, and a lease, a full line, a load in progress or an occupant each make it not free.
			if r.remoteHint() {
				r.hintSeatFree = !busy
				busy = true
			}
		}
		// The fleet is read when the local seat is busy; for an idle one, when some contract names a layer
		// this box does not declare (register A-108), because that contract is dealt to the node that declares
		// it; and when the call has more subtasks than the seat's run-cap line takes (ADR 0076), because the
		// overflow is dealt to the remotes with room instead of into a line it could not leave. Nothing else
		// about an idle box needs the roster. A box with no agent seat at all (a delegation client) reads it
		// for every contract, the same way.
		localView := r.localView()
		readFleet := busy || len(subtasks) > room
		for _, c := range subtasks {
			if !r.localServesLayer(localView, c) {
				readFleet = true
				break
			}
		}
		var failed map[string]string
		if readFleet {
			r.autoViews, r.autoBases, r.autoProbeErrs, failed = r.fetchViewsDetailed(ctx)
		}
		r.autoDeal = r.dealAutoRemoteRoom(subtasks, localView, r.autoViews, r.autoBases, busy, failed, room, roomNote)
	}
	board := newResultBoard(len(subtasks))
	progress := newProgressor(opts, len(subtasks))
	// The call is as wide as its deal (ADR 0076), not a constant: dealParallelism reads what the deal committed to
	// each node. A call with no deal to read (route=local) keeps runConcurrency.
	width := r.dealParallelism()
	if width != runConcurrency {
		log.Printf("delegate: fan-out width %d for %d subtask(s), sized from the %s deal (floor %d)", width, len(subtasks), route, runConcurrency)
	}
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	launched := 0
	var stop launchStop
launch:
	for i, c := range subtasks {
		if dl == nil {
			sem <- struct{}{} // no deadline: block for a slot exactly as always
		} else {
			// A subtask still waiting for a run slot when the call ends never begins.
			if dl.reached() {
				break launch
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break launch
			}
			// Nor does one that would begin inside the reserve (ADR 0073). The slot may have taken
			// minutes to come, and a job started with no more than a placed job needs left could only
			// be cut by the deadline and then run on in its node with nobody waiting for it.
			if left, noRoom := dl.noRoom(); noRoom {
				<-sem
				stop = launchStop{noRoom: true, left: left}
				break launch
			}
		}
		dl.begin()
		wg.Add(1)
		launched++
		go func(i int, contract core.AgentContract) {
			defer wg.Done()
			defer func() { <-sem }()
			progress.started(i)
			// runOne's outcome was already cut where it was PRODUCED (finish, settle, exhaustedSettled): the
			// only moment "after the deadline?" can be answered. Asking again here, with no
			// timestamp, would rewrite an answer that finished before the deadline — an
			// abstention whose retry was cut, for one — into a call-deadline defer.
			pr := r.runCapped(ctx, i, contract)
			if board.put(i, pr) {
				if !pr.deadlineCut {
					dl.answer()
				}
				progress.finished(i, pr)
			}
		}(i, c)
	}
	drained, abandoned := dl.await(ctx, &wg)
	if abandoned && led != nil {
		// Goroutines are still running and will record their rows when they end:
		// the ledger stays open for them and closes once the last has returned.
		ledgerDrained = drained
	}
	results, done := board.close()
	for i := range results {
		switch {
		case done[i]:
		case i >= launched:
			results[i] = r.unlaunched(subtasks[i], stop)
			progress.finished(i, results[i])
		default:
			results[i] = r.abandoned(i, subtasks[i])
			progress.finished(i, results[i])
		}
	}
	// The call's pin (ADR 0078) goes on every result the caller receives, once, here: the reason a pinned call carries
	// and the plain-words clause a hinted call's placement reason opens with. record() puts the same on the ledger
	// row of each attempt, from the same function. The pin accounting offload_status publishes counts these
	// published results, so a retried or re-placed subtask is one and a hint is judged by where it was placed.
	for i := range results {
		results[i] = r.stampPin(results[i])
	}
	r.tallyPins(results)

	var sum Summary
	for _, pr := range results {
		if pr.retried {
			sum.Retried++
		}
		if pr.retryRecovered {
			sum.RetryRecovered++
		}
		if pr.waited {
			sum.Waited++
		}
		if pr.shed {
			sum.Shed++
		}
		if pr.Replacements > 0 {
			sum.Replaced++
			// "Recovered" is stated on the REFUSAL, not on the answer: Err == ""
			// means some node took the work and reported on it. Whether that
			// report was good is what the four buckets below are for. A capacity
			// defer or a shed (Unplaced) is the one Err == "" result no node
			// ever ran, and it is not a recovery.
			if pr.Err == "" && !pr.Unplaced {
				sum.ReplacementRecovered++
			}
		}
		// The bucket and the broken-stack ANNOTATION are decided separately: a
		// local placement can succeed while the fleet it declined to use is
		// down, and that must still count as infrastructure.
		//
		// lost is the STRICTER half — this subtask delivered no usable result,
		// the contracted output never arrived, and the stack is why — kept apart
		// from remotesUnreachable, which annotates a result that succeeded. Note
		// what "no usable result" does NOT mean: `output` may be populated here
		// (a finished loop whose re-pack seat was unreachable), and the count is
		// on the CONTRACTED deliverable — see Summary.LostToStack above. Both are
		// infrastructure; only one is lost work, and a consumer that must decide
		// "did anything get eaten?" (the loud signal of a partial result, and the
		// MCP error flag when nothing succeeded) cannot answer it from the merged
		// count.
		lost := pr.Result.Deferred && BrokenStackDefer(pr.Result.DeferClass)
		infra := pr.remotesUnreachable || lost
		// DO NOT add "every remote refused" to this. It is tempting after
		// 0.101.0 — a fleet that 503s every dispatch and leaves the local seat
		// carrying the run feels like it should be loud — and it is wrong twice.
		// Infrastructure means THE FLEET IS BROKEN: a node that answered its
		// health probe and then declined a job is working exactly as designed
		// (back-pressure), and a local placement that SUCCEEDED is not a broken
		// stack by any reading. remotesUnreachable is deliberately narrower: it
		// is set only when the remotes failed their HEALTH PROBE, which is a
		// different fact from refusing work. Widening this field would flag
		// finished, verified work as infrastructure failure on both surfaces
		// (non-zero CLI exit, IsError to the MCP caller) — the exact
		// flag-on-successful-work defect delegateIsError was rewritten to
		// remove. The audibility for a shedding fleet is Summary.Replaced, which
		// exists for it and says the true thing without redefining this one.
		switch {
		case pr.Err != "":
			sum.Failed++
		case pr.Result.Deferred:
			sum.Deferred++
		case len(pr.AcceptanceFailures) > 0:
			sum.FailedVerification++
		default:
			sum.Succeeded++
		}
		if infra {
			sum.Infrastructure++
		}
		if lost {
			sum.LostToStack++
		}
	}
	// The denominators are carried only WITH a loss — one decision point, here,
	// so the wire renderer stays a straight copy and a healthy run's Summary
	// keeps the zero-valued telemetry block every caller already compares against.
	sum.Quarantined = int(r.quarantined.Load())
	if sum.CorpusRowsLost = int(r.corpusLost.Load()); sum.CorpusRowsLost > 0 {
		sum.CorpusRowsAttempted = int(r.corpusTried.Load())
	}
	if sum.LedgerRowsLost = int(r.ledgerLost.Load()); sum.LedgerRowsLost > 0 {
		sum.LedgerRowsAttempted = int(r.ledgerTried.Load())
	}
	r.reportTelemetryLoss()
	return results, sum, nil
}

// BrokenStackDefer reports whether a defer_class means an OPERATOR, not a
// bigger model, has to act: the stack failed (infrastructure) or the
// node/config combination can never work as configured (config). An empty
// class — a pre-0.65 node, which cannot classify — is deliberately NOT broken:
// assuming the worst about an older peer would fail every mixed-version fleet.
//
// core.DeferClassContract is explicitly NOT a broken stack. It was split out of
// `config` for exactly this line: three of the placement gate's five conditions
// are properties of the CALLER'S CONTRACT (no output_schema — legal per
// Validate; a non-origin depth; a token estimate no advertised ceiling can
// hold), and lumping them in here made `--route remote` exit non-zero, on a run
// where every node was healthy, with a message telling the delegating model a
// node was broken or misconfigured. Nobody has to touch a box to fix those.
func BrokenStackDefer(class string) bool {
	return class == core.DeferClassInfrastructure || class == core.DeferClassConfig
}

type runner struct {
	cfg     config.Config
	local   LocalRunner
	route   string
	remotes []string
	// pin is what the caller asked of placement with route local or remote (ADR 0078), resolved once at intake.
	// route above is the route the run APPLIES: a reasonless local or remote through a door that offers pin_reason
	// is a hint and is applied as auto (pin.go), so every site below that reads r.route sees the route placement
	// uses, and pin is what the ledger rows, the results and the pin accounting say the caller asked.
	pin pinCall
	// tally is RunOptions.PinTally; nil = the call's pins are not counted.
	tally *PinTally
	// hintSeatFree is, for a remote hint, whether the local seat read free (no lease, no full line, no load in
	// progress, no vLLM seat to evict) when the deal was made, before the hint dealt it as unavailable: the deal
	// may fall back to the seat only then. Written once before any goroutine starts, read-only after.
	hintSeatFree bool
	// intent is the delegator-death durability ledger (Option A, intent.go).
	// nil = inert (state root unresolvable); dispatch never depends on it.
	intent *intentLedger
	led    *ledger.Ledger
	// pair reports each subtask to NVIDIA PAIR's Jobs list (pairevents.go);
	// inert unless pair_workloads_enabled and PAIR is installed.
	pair *pairworkloads.Emitter
	// ledgerUnopened marks the TOTAL-loss case: a LedgerPath was configured and
	// ledger.Open failed, so there is no handle to try. record() must still
	// count a lost row per result — with the plain `if r.led != nil` guard it
	// counted nothing, LedgerRowsLost stayed 0, omitempty dropped it, and the
	// worst possible telemetry outcome published byte-identically to the best.
	ledgerUnopened bool
	// warnCorpus/warnLedger fire at most ONE warning each per Run. Once, not
	// per subtask: an 8-subtask fan-out against a full disk would otherwise
	// print the same line eight times and bury the results it is warning about.
	warnCorpus sync.Once
	warnLedger sync.Once
	// warnMarker is the same once-per-run rule for the dispatch marker (telemetry.go
	// recordStarted). A marker is a row the run does not OWE, so its loss stays out of
	// the ledgerLost tally; the warning is its only trace.
	warnMarker sync.Once
	// probeWarned bounds fetchViews' per-remote failure warning to ONCE PER
	// BASE PER RUN. fetchViews runs inside runOne, so the warning fired once
	// per remote PER SUBTASK — an 8-subtask fan-out against two dead remotes
	// printed sixteen identical lines, the exact hazard warnCorpus/warnLedger
	// exist to avoid. Keys are base URLs; values are unused.
	probeWarned sync.Map
	// probeMu guards the two probe caches fetchViews keeps FOR THIS RUN.
	//
	// probeMemo is the fleet snapshot the run's sibling subtasks share
	// (fetchViewsMemoTTL): route=spread always amortised its probe with one
	// fetch at run start, and auto/remote/re-placement/retry each paid their
	// own — per subtask, serially, at fetchNodeViewTimeout per unreachable
	// remote. probeDead is the negative cache: a base that failed at the
	// TRANSPORT is not re-dialled for probeNegativeTTL, and its reason is
	// replayed into probeErrs so the placement note still names it.
	//
	// Both are per RUNNER, which is per Run: a snapshot must never outlive the
	// call that took it, and a box that was down an hour ago must not be
	// pre-judged by a Run starting now.
	probeMu   sync.Mutex
	probeMemo *probeSnapshot
	probeDead map[string]deadProbe
	// cool is the run's per-node cooldown (ADR 0063): a node that refused a
	// dispatch for CAPACITY is not asked again before the instant recorded here.
	// Shared by every subtask of the run, because "node A is full" is a fact
	// about the node, not about the subtask that found out.
	cool cooldowns
	// localSlotMu/localSlotAt/localSlotFree memoise the local seat's run-cap
	// reading for fetchViewsMemoTTL: the capacity wait asks once per tick per
	// waiting subtask, and twelve waiters must not each read the registry.
	localSlotMu   sync.Mutex
	localSlotAt   time.Time
	localSlotFree bool
	localSlotNote string
	// localSlotWarn logs the local run registry's unreadable fail-open once per run.
	localSlotWarn sync.Once
	// localBusyMu/localBusyAt/localBusyRd memoise the local seat's LOAD reading
	// (probeLocalBusy) for the same reason: the capacity wait of a subtask a deal kept
	// off a busy seat re-reads it every tick, and twelve waiters must not each ask the
	// engine.
	localBusyMu sync.Mutex
	localBusyAt time.Time
	localBusyRd busyReading
	// corpus*/ledger* count telemetry rows ATTEMPTED and LOST across the run
	// (atomic: record() runs on every subtask goroutine). The once-per-run
	// warnings above say THAT telemetry failed; these say how much, which is
	// the difference between a transient and a full disk.
	corpusTried atomic.Int64
	corpusLost  atomic.Int64
	ledgerTried atomic.Int64
	ledgerLost  atomic.Int64
	// spreadViews/Bases/ProbeErrs are the ONE fleet snapshot a route=spread run
	// deals its subtasks across (fetched in Run, read-only afterwards).
	spreadViews     []NodeView
	spreadBases     []string
	spreadProbeErrs []string
	// spreadDeal is the whole run's placement, computed by dealSpread in Run
	// before any subtask goroutine starts and READ-ONLY from then on.
	spreadDeal []spreadSlot
	// spreadLease is the machine-wide GPU lease as read ONCE with the fleet
	// snapshot: a TEXT holder reserves the local seat (Reserved) and it leaves
	// the deal — the whole reason 0.113.14 exists. A media lease is not read by
	// the spread deal (it never was; ADR 0026 arbitrates renders elsewhere).
	spreadLease gpulease.Info
	// spreadLocalBusy is the local seat's in-flight reading taken ONCE with the
	// fleet snapshot (0.113.20): a seat that already holds a request leaves the
	// rotation the way a leased one does (placeSpread). localBusyProbe is the
	// seam tests drive it through; nil = the production probe (seatload).
	spreadLocalBusy busyReading
	localBusyProbe  func(ctx context.Context) busyReading
	// seatGuardCheck is probeLocalBusy's seam onto the seat guard (checkSeatGuard);
	// nil = the process-wide seatguard.Shared.
	seatGuardCheck func(ctx context.Context, model string) seatguard.Verdict
	// autoDealBusy is the reading route=auto's one deal was made from (RunWith), kept
	// so a remote placement can say the local seat was occupied, not merely busy.
	autoDealBusy busyReading
	// autoDealReadBusy records that route=auto's deal (or a per-subtask placement) read the
	// local seat busy and placed around it (dealReadLocalBusy). Atomic: subtask goroutines
	// set it while the capacity wait of a sibling reads it.
	autoDealReadBusy atomic.Bool
	// autoLocalBusyOnce/autoLocalBusy cache route=auto's ONE read of the local
	// seat's load (W-01, register S-01): every subtask of this Run must see
	// the SAME reading — the same one-probe-per-Run invariant spreadLocalBusy
	// already holds for route=spread, extended to auto so a busy local seat
	// (in-flight at or past the fleet's own concurrency cap, or mid-load) is
	// no longer indistinguishable from an idle one just because no GPU lease
	// happens to be held.
	autoLocalBusyOnce sync.Once
	autoLocalBusy     busyReading
	// autoViews/autoBases/autoProbeErrs/autoDeal are route=auto/remote's OWN
	// one-fetch, one-deal snapshot (W-06, register S-11/S-13) — the same
	// invariant spreadDeal holds for route=spread, extended here so
	// runConcurrency sibling subtasks stop probing the fleet and calling
	// Place independently (S-13: "those subtasks probe within milliseconds of
	// each other — so several of them can read the same free slot and then
	// compete for it"). Computed once in RunWith before any goroutine starts,
	// read-only afterwards. Empty/nil on route=local, route=spread and route=
	// queue, which never touch them.
	autoViews     []NodeView
	autoBases     []string
	autoProbeErrs []string
	autoDeal      []spreadSlot
	// dealRoom is the local seat's run-cap room (localRunCapRoom) as the run's deal read it, ONCE, before any
	// goroutine started (ADR 0076): what the auto deal may commit to an idle seat, what the spread deal gives its
	// local slots, and the local share of the call's width (dealParallelism). Written by the deal, read-only after.
	dealRoom int

	// rescue is RunOptions.Rescue; nil = no rescue.
	rescue RescueFunc
	// quarantine is the caller's Quarantine (RunOptions), nil = inert.
	quarantine  *Quarantine
	quarantined atomic.Int64
	// priority / tenant are RunOptions.Priority (clamped) and RunOptions.Tenant,
	// stamped on every dispatch this run makes (see dispatch).
	priority int
	tenant   string
	// beforeForced is a test seam: called with the chosen node's base between the
	// capacity wait choosing a node and dispatching to it, the one window in which
	// another Run of the process can take the node's last gate slot. nil in production.
	beforeForced func(base string)
	// decider is RunOptions.LocalDecider; snapshot is the ONE memoised reader
	// set the production decider reads through (built lazily on the first
	// composite decision, shared by every subtask goroutine — Snapshot is
	// mutex-guarded). Both inert on a non-composite box.
	decider  func(ctx context.Context, contract core.AgentContract, st Subtask) placetable.Decision
	snapOnce sync.Once
	snapshot *placetable.Snapshot
	// call is this run's whole-call deadline (RunOptions.Deadline), nil = none.
	// Every method on it is nil-safe, so no site needs a guard.
	call *callDeadline
	// pairMu/pairShut fence the PAIR emitter at the end of a run (calldeadline.go:
	// emitPair, shutPair): a goroutine the call deadline abandoned may report after
	// RunWith has begun draining the emitter.
	pairMu   sync.RWMutex
	pairShut bool
	// pairLate makes the "a late PAIR frame was dropped" line once per run.
	pairLate sync.Once
	// lastJob is, per subtask index, the job id of the attempt most recently started
	// (attempt mints one per attempt). A subtask the call gives up on is published under
	// it (abandoned), so the caller can reconcile the late row its goroutine writes.
	lastJob sync.Map
}

// decide is the composite box's placement decision for a contract that is
// about to run LOCALLY: the zero Decision on a plain box (nothing decided,
// nothing published — the byte-identical constraint), the injected decider
// when a caller supplied one, else the production table over the run's
// memoised snapshot. The request is built exactly as the node builds it
// (placetable.RequestForContract with the box's agent_max_tokens), so the
// delegator and the node describe the contract to the table identically.
//
// A contract that NAMES a layer (register A-100) is decided FOR that layer,
// never by the free choice — the same rule remoteDecision applies to a
// remote's rows. On a plain box the name cannot be served at all, and that is
// said as a contract defer naming the layer: the one case a plain box
// publishes a placed block, because the caller asked for a layer and a silent
// run on the planner seat would be a lie about where the work went.
func (r *runner) decide(ctx context.Context, contract core.AgentContract, st Subtask) placetable.Decision {
	if !r.cfg.Composite() {
		if contract.Layer != "" {
			return placetable.Decision{Placed: core.Placed{Layer: contract.Layer,
				Reason: fmt.Sprintf("layer %s requested; this box declares no layers", contract.Layer)}, Defer: true, DeferClass: core.DeferClassContract}
		}
		return placetable.Decision{}
	}
	if r.decider != nil {
		return r.decider(ctx, contract, st)
	}
	r.snapOnce.Do(func() { r.snapshot = placetable.NewSnapshot(r.cfg, placetable.DefaultSnapshotTTL) })
	req := placetable.RequestForContract(contract, st.EstTokens, r.cfg.AgentMaxTokens)
	if contract.Layer != "" {
		return placetable.DecideOnLayer(req, r.cfg.Layers, contract.Layer, r.snapshot.Live())
	}
	return placetable.Decide(req, r.cfg.Layers, r.snapshot.Live())
}

// placedOrNil is the block a decision publishes: nil for the zero Decision (a
// plain box), the decision's Placed otherwise. One helper so no site can
// publish an empty block with layer "" on a plain box.
func placedOrNil(dec placetable.Decision) *core.Placed {
	if dec.Layer == "" && dec.Reason == "" {
		return nil
	}
	p := dec.Placed
	return &p
}

// placement is a resolved "run it HERE" — the node, its dial base ("" for
// local) and the human-readable reason that rides the result. decided, when
// set on a forced LOCAL placement, is the placement-table decision the run
// must use instead of deciding again: the capacity wait re-ran the decider and
// is handing attempt() the seat it found free (or the expired wait's seat),
// and a second decision inside attempt could contradict it.
type placement struct {
	view    NodeView
	base    string
	reason  string
	decided *placetable.Decision
}

// reportTelemetryLoss emits the ONE end-of-run line naming how much telemetry
// this run lost, out of how much it tried. Silent when nothing was lost, so a
// healthy run's output is unchanged.
func (r *runner) reportTelemetryLoss() {
	if lost := r.corpusLost.Load(); lost > 0 {
		log.Printf("delegate: delegation-log corpus: %d of %d rows lost this run (results unaffected)", lost, r.corpusTried.Load())
	}
	if lost := r.ledgerLost.Load(); lost > 0 {
		log.Printf("delegate: ledger: %d of %d rows lost this run (savings accounting incomplete; results unaffected)", lost, r.ledgerTried.Load())
	}
}

// runOne runs one subtask: a first attempt placed per route, then — when the
// first attempt came back failed_verification or an honest abstention and a
// DIFFERENT node is available — exactly one retry there, publishing the better
// of the two. Measured motivation (2026-08-21): on the same four contracts the
// 27B seat and the 4B seat each missed a different one; acceptance caught both,
// and the retry is what turns "caught" into "recovered".
func (r *runner) runOne(ctx context.Context, i int, contract core.AgentContract) PlacedResult {
	start := time.Now()
	// timeout_sec is the EXECUTION budget, and every mechanism inside runOne
	// spends from this one copy of it: the re-placement loop and the
	// verification retry both measure what is left against this `start`, so
	// neither can silently extend the other's.
	//
	// It is NOT the end-to-end wall — see placeAndRun for what else the wall
	// contains and why none of it can be charged here.
	budget := executionBudgetSec(contract)
	// ONE ledger per subtask, shared by every placeAndRun below. See placements.
	pl := newPlacements()
	first := r.placeAndRun(ctx, i, contract, nil, start, budget, pl)
	if !retryable(first) {
		return first
	}
	// A retryable first attempt is one a node TOOK: it ran the job and its answer was the
	// problem. That seat is recorded here, once, whichever path placed it (the first
	// dispatch, a re-placement, a capacity wait), so the retry's capacity wait can never
	// hand the second chance back to the seat whose answer it exists to correct.
	pl.ran[first.ranBase] = true
	// A research page that failed ONLY its acceptance is not run a second time
	// (register C-74). Guarded here, not inside retryable(): that predicate sees
	// a placed result, never the contract that names the door.
	if skipsRetryAsResearchAcceptanceOnly(contract, first) {
		first.RetryNote = fmt.Sprintf("retry skipped: the first attempt on %s failed only its acceptance, and a research digest is graded against its page — a second seat repeats the verdict at the cost of a whole second run", nodeLabel(first))
		return first
	}
	// An EMPTY final (0.115.8: stop_reason reasoning_starved / empty) is not a
	// wrong answer another seat can correct — it is the seat's completion
	// budget or think block, and a second seat handed the wall's leftovers
	// repeats the shape (2026-09-10: the 4B's 603 s empty final was retried on
	// the 27B with 296 s, which generated 4,178 tokens of think and timed out).
	// The reasoning_starved shape is class budget and never reaches here (kept
	// in the match so the rule reads as one shape, not as a coincidence of the
	// class table); the `empty` abstention does, and is skipped by name.
	if stop := first.Result.StopReason; first.Result.Deferred && (stop == "reasoning_starved" || stop == "empty") {
		first.RetryNote = fmt.Sprintf("retry skipped: the first attempt on %s ended on an empty final (stop_reason %s) — a second seat given the wall's leftovers repeats the shape; the fix is the seat's completion budget or agent_thinking, not a retry", nodeLabel(first), stop)
		return first
	}
	// Credit back what the node's ADMISSION spent before the coherence defer
	// (reviewer finding, D-118). The retry budget is delegator wall clock since
	// `start`, and the defer's own trigger under the default "cold" policy is a
	// COLD LOAD — 125–250 s of a vLLM seat on this fleet, against a 300 s
	// default contract. Left uncredited, the promise this defer is retryable
	// for ("the wall never started, the budget is still on the table") would be
	// refused by the retry floor on the very path that produces it. It is the
	// same mechanism, and the same argument, as the capacity wait's credit:
	// time the subtask provably did not spend WORKING is not charged to
	// timeout_sec.
	pl.credit += admissionCredit(first)
	// The retry lives INSIDE the subtask's own timeout_sec: the caller was told
	// that number bounds the work per subtask, and a second full attempt would
	// have doubled it silently. What is left after the first attempt is the
	// retry's budget; under the floor there is no honest retry to run. The
	// floor is seat-aware through config (agent_retry_min_sec, D-46): a cold
	// load plus one turn at max_tokens on the retry seat, never the bare 10 s
	// that let a 296 s retry burn a thinking seat for nothing.
	// First pass, BEFORE a retry node is chosen: the configured floor raised by
	// the first attempt's own min_turn (the seat we know about). This is the
	// cheap refusal; the retry SEAT's floor is applied below once it is known.
	floor, floorSrc := r.retryFloorFor(first)
	remaining := pl.remaining(start, budget)
	if remaining < floor {
		first.RetryNote = fmt.Sprintf("retry skipped: %ds of the %ds timeout_sec budget left after the first attempt (floor %ds%s)", remaining, budget, floor, floorSrc)
		return first
	}
	// alternativeNode BLOCKS — it probes the fleet. Bound that probe by what
	// the contract still has, so a roster of blackholing nodes cannot spend a
	// budget the subtask no longer owns (fetchViews is sequential at
	// fetchNodeViewTimeout per remote and Run derives no deadline of its own).
	altCtx, cancel := context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
	alt, held, ok := r.alternativeNode(altCtx, first, contract, pl)
	cancel()
	if !ok {
		// A fence is a REASON, not a silence: the caller has to be able to tell
		// "there was nowhere else to go" from "the only seat left is behind a
		// measurement that has the cards" (D-94). The call's deadline is a second
		// reason: it ends the fleet read that would have named a node, and an empty
		// note after it reads as "there was nowhere else to go". The clock decides,
		// not altCtx, which also ends on the budget the retry is bounded by. A text reservation
		// of the local seat (C-81) is told the same way: `held` carries either clause, and
		// retryHeldWhy words what follows it. Like a fence it is skipped and not queued for: a
		// retry that found no node has no placement to wait with (ADR 0063, decision 10).
		//
		// The order is the point. "No other node is eligible" and "no other node could take
		// the contract" are claims about nodes, which a read the deadline ended cannot support
		// (ADR 0065), so once the clock has run out the note leads with the deadline, and when
		// the local seat is fenced it keeps that fact behind it: it is read from the lease
		// record and stays true, but the claim about the fleet goes. Next comes the fence on
		// its own, and a seat-down defer's own note (ADR 0066: the second placement it was
		// promised found no node) comes last.
		switch {
		case r.call.reached():
			first.RetryNote = "retry skipped: " + callDeadlinePrefix + " before a retry node was chosen"
			if held != "" {
				first.RetryNote += "; " + held + ", and " + retryHeldWhy(held)
			}
		case held != "":
			first.RetryNote = "retry skipped: " + held + " and no other node is eligible; " + retryHeldWhy(held)
		case SeatDownDefer(first.Result):
			// The defer promises a second placement on another node: when there is no
			// other node the caller must be told it was considered and why it did not
			// happen, not left with a retryable defer and an empty note.
			why := "every eligible node was already tried or is not eligible"
			switch {
			case r.route == "local":
				why = "route local places nothing on another node"
			case r.remotePinned():
				why += "; " + r.remotePinWhy()
			}
			first.RetryNote = fmt.Sprintf("retry skipped: the seat on %s went down and no other node could take the contract (%s)", nodeLabel(first), why)
		case r.remotePinned():
			// A reasoned remote pin retries on another fleet node or not at all (alternativeNode): when there is no other
			// node, a failed verification, an abstention or an admission defer would otherwise be published with no note
			// and read as a retry nobody considered. The pin is the reason there is no local retry, so the note names it.
			first.RetryNote = fmt.Sprintf("retry skipped: no other fleet node could take the contract after the first attempt on %s (%s); %s",
				nodeLabel(first), attemptOutcome(first), r.remotePinWhy())
		}
		return first
	}
	// Never land the retry on a seat that is already generating for another
	// job (D-46): the two runs halve each other's tok/s and the retry, on the
	// leftover budget, is the one that dies (2026-09-10: the ledger-01 retry
	// joined the 27B mid-generation of ledger-00 and both crawled at 27 tok/s).
	//
	// A busy seat is a place in line, never a reason to refuse the second chance
	// (INV-4, register C-76): the retry WAITS for the seat to have room - the same
	// capacity wait a refused first placement gets, agent_placement_wait_sec, credited
	// like it - and runs on it the moment it does, so the two runs still never share
	// it. The retry is skipped only when the seat stayed busy for the whole wait (or
	// the wait is switched off), and the note says how long it waited.
	//
	// Not for a seat-down defer (ADR 0066): this check neither waits nor skips. D-46's
	// case is a verification retry: the first attempt PRODUCED an answer, the retry is an
	// optional second opinion on the budget that was left, and joining a generating seat
	// costs it the budget it needs. A seat-down defer produced nothing — declining to
	// re-place it, or waiting out the placement wait and then declining, loses the job
	// — and its retry carries the credited budget of the wait the dead seat cost. Busy
	// is a place in line, never a refusal (INV-4): the node's own queue is the line, and
	// it answers 503 when it is full, which placeAndRun re-places at once (ADR 0063),
	// into the bounded capacity wait when nothing has room. During the 2026-09-29 outage
	// the fleet was saturated (44 runs in flight for ~13 slots), so a check here would
	// have held or refused nearly every re-placement.
	//
	// That line is a REMOTE alternative's. The local seat answers no 503: a run forced
	// onto it joins the run-cap line and waits there for the run's whole wall, a line the
	// subtask cannot leave. alternativeNode therefore sends a seat-down retry to an
	// untried remote while that line has no free slot ahead of a newcomer, so the
	// exemption below never hands the retry to a full local line when there is another
	// place to go.
	if !SeatDownDefer(first.Result) {
		if busy, why := r.retrySeatBusy(ctx, alt); busy {
			w := r.awaitRetrySeat(ctx, alt, why)
			pl.credit += w.waited
			if w.canceled {
				// The wait was ended from outside while the retry stood in line: the seat was
				// not shown to stay busy, so the note must not claim it did. Who ended it is
				// part of the truth: the call's own deadline (ADR 0065) is not the caller
				// cancelling, and the retry's fate is worded with the deadline's stable
				// opening, as it is for a retry the deadline cut while it ran.
				ended := "the caller canceled"
				if r.call.reached() {
					ended = callDeadlinePrefix
				}
				first.RetryNote = fmt.Sprintf("retry skipped: %s after the retry waited %s in line for the retry seat on %s, which was running another job (%s)", ended, w.waited.Round(time.Second), alt.view.NodeID, w.why)
				return first
			}
			if w.stillBusy {
				after := ""
				if w.waited > 0 {
					after = fmt.Sprintf(" after waiting %s in line", w.waited.Round(time.Second))
				}
				first.RetryNote = fmt.Sprintf("retry skipped: the retry seat on %s is already running another job%s (%s); a shared seat would only slow both", alt.view.NodeID, after, w.why)
				return first
			}
		}
	}
	// The floor of the seat the retry will actually run on (D-46 follow-up,
	// 0.117.2): the 2026-09-10 retry cleared a 201 s floor sized from the 4B's
	// numbers and landed on the 27B, whose own min_turn was ≈ 484 s, with 626 s
	// — enough for its loop, not for loop + re-pack. A remote node publishes
	// its seat rate on health; the local seat's is read from this box's store.
	floor, floorSrc = r.retryFloorOn(first, alt, contract)
	// RE-MEASURE after the probe. `remaining` above was true when it was taken
	// and can be minutes stale by now; writing that stale number into the retry
	// contract is what would hand a seat time the subtask no longer has.
	remaining = pl.remaining(start, budget)
	if remaining < floor {
		first.RetryNote = fmt.Sprintf("retry skipped: %ds of the %ds timeout_sec budget left after choosing a retry node (floor %ds%s)", remaining, budget, floor, floorSrc)
		return first
	}
	retryContract := contract
	retryContract.TimeoutSec = remaining
	retryContract.TimeoutAuto = false // a retry's wall is what is left — explicit, never auto (D-03)
	second := r.placeAndRun(ctx, i, retryContract, &alt, start, budget, pl)
	merged := mergeAttempts(first, second)
	if r.remotePinned() {
		// The retry ran on a fleet node because the pin allows no other place (alternativeNode), and the note says so whichever
		// attempt is published: the first attempt it annotates, or the retry that recovered it.
		merged.RetryNote += "; " + r.remotePinWhy()
	}
	return merged
}

// retryWait is what awaitRetrySeat found: how long the retry waited in line (time the
// caller credits to the retry's budget, because the wait is queueing and never work),
// whether the seat was still busy when the wait stopped, whether the context ended it
// (the caller cancelled, or the call's own deadline passed, ADR 0065: neither says
// anything about the seat), and the seat's own last reading.
type retryWait struct {
	waited    time.Duration
	stillBusy bool
	canceled  bool
	why       string
}

// awaitRetrySeat holds a verification retry in line for a seat that is running
// another job: it re-reads the seat every capacity-wait tick (placementPollInterval,
// jittered) until it has room, and gives up after the placement wait
// (agent_placement_wait_sec; a negative value switches the wait off and the retry is
// skipped at once, as before).
func (r *runner) awaitRetrySeat(ctx context.Context, alt placement, why string) retryWait {
	// The retry keeps the TTL under a call deadline too (ADR 0073): it is an optional second opinion on
	// an answer the subtask already holds, and holding that answer for the rest of the call to improve
	// on it would cost the caller more than the retry can return.
	wait := r.placementTTL()
	if wait <= 0 {
		return retryWait{stillBusy: true, why: why}
	}
	begin := time.Now()
	deadline := begin.Add(wait)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
		case <-time.After(jitteredWithin(placementPollInterval, time.Until(deadline))):
		}
		if ctx.Err() != nil {
			break
		}
		busy, w := r.retrySeatBusy(ctx, alt)
		if !busy {
			return retryWait{waited: time.Since(begin), why: ""}
		}
		why = w
	}
	return retryWait{waited: time.Since(begin), stillBusy: true, canceled: ctx.Err() != nil, why: why}
}

// executionBudgetSec is the delegator's execution budget for a contract: its
// timeout_sec, or the wire CAP for a contract the caller left unsized
// (timeout_auto, register D-03). The executing node sizes that wall from its
// seat's measured rate anywhere inside the cap, so the delegator's clock — the
// poll deadline, the re-placement ledger and a retry's remainder — must hold
// the cap open rather than cut a 700 s wall the node chose at the 300 s default
// the wire happens to carry. A retry never carries the marker: its wall is what
// is left, an explicit number.
//
// The accepted cost (review, 2026-09-16): a node that acks and then dies
// silently is abandoned at the CAP on this path, 900 s instead of the 300 s the
// default gave — the delegator cannot tell "a slow seat under a properly sized
// wall" from "a dead node" without waiting. Bounded, rare, and written down;
// the tighter bound is the node's advertised seat_rate (health), the same
// arithmetic the retry floor already reads — a follow-up, not this change.
func executionBudgetSec(c core.AgentContract) int {
	switch {
	case c.TimeoutAuto:
		return core.AgentTimeoutSecCap
	case c.TimeoutSec <= 0:
		return core.AgentTimeoutSecDefault
	}
	return c.TimeoutSec
}

// retryFloorSec is the least remaining budget a verification retry starts
// with: the historical 10 s (minRetrySec), raised by config `agent_retry_min_sec`.
func (r *runner) retryFloorSec() int {
	if r.cfg.AgentRetryMinSec > minRetrySec {
		return r.cfg.AgentRetryMinSec
	}
	return minRetrySec
}

// retryFloorFor (0.115.21, register D-03) is the seat's OWN floor when the
// first attempt published one — min_turn_sec: its cold load plus one turn at
// the final budget at its measured rate — and the configured floor otherwise.
// The larger wins: a box constant sized for the reference seat can sit under
// what a slower seat just measured for itself.
//
// A seat-down defer (ADR 0066) is the exception: its min_turn_sec is the DEAD
// seat's own (its cold load plus a turn at its measured rate), and the retry goes
// to another node, whose own floor retryFloorOn applies once it is chosen. Read
// here it refused the re-placement of the very defer it exists for — a flagship
// that honestly records a ~270 s cold load publishes a ~390 s min_turn, above
// any default contract — although the seat that is about to run the retry has
// nothing to do with it.
func (r *runner) retryFloorFor(first PlacedResult) (int, string) {
	floor := r.retryFloorSec()
	if m := first.Result.MinTurnSec; m > floor && !SeatDownDefer(first.Result) {
		return m, fmt.Sprintf(", min_turn_sec of the seat on %s", nodeLabel(first))
	}
	return floor, retryFloorSource(floor)
}

// retryFloorOn (0.117.2, register D-46) is the floor of the seat the retry
// LANDS on: the configured floor raised by that seat's own min_turn — its
// remembered cold load plus one final turn at its measured rate, plus the
// re-pack term when the contract carries an output_schema. A remote node's
// numbers come from its health (`seat_rate`, published since 0.117.2); the
// local seat's from this box's seat-rates store. A seat with no numbers falls
// back to the first attempt's min_turn (retryFloorFor), and the note says so.
func (r *runner) retryFloorOn(first PlacedResult, alt placement, contract core.AgentContract) (int, string) {
	floor := r.retryFloorSec()
	repack := 0
	var minTurn int
	var src string
	switch {
	case alt.base != "":
		if sr := alt.view.SeatRate; sr != nil && sr.TokS > 0 {
			final := sr.MinTurnSec
			if b := alt.view.SeatBudget; b != nil {
				if len(contract.OutputSchema) > 0 {
					repack = b.FinalTokens
				}
				minTurn = seatrate.MinTurnFor(sr.ColdLoadSec, b.FinalTokens, repack, sr.TokS)
			} else {
				// A node publishes seat_rate and seat_budget together (one gate
				// on the health handler), so this branch waits for a future
				// node that publishes the rate alone: its own floor, no re-pack.
				minTurn = final
			}
			src = fmt.Sprintf(", min_turn_sec published by %s", alt.view.NodeID)
		}
	default:
		if known, ok := r.localSeatRate(); ok {
			step := r.cfg.AgentMaxTokens
			if step <= 0 {
				step = 1024
			}
			final := seatrate.FinalBudgetFor(step)
			if len(contract.OutputSchema) > 0 {
				repack = final
			}
			minTurn = seatrate.MinTurnFor(known.ColdLoadSec, final, repack, known.TokS)
			src = ", min_turn_sec of the local seat (seat-rates store)"
		}
	}
	if minTurn <= 0 {
		f, s := r.retryFloorFor(first)
		if s != "" && strings.Contains(s, "min_turn_sec") {
			s += " (the retry seat published no rate)"
		}
		return f, s
	}
	if minTurn > floor {
		return minTurn, src
	}
	return floor, retryFloorSource(floor)
}

// localSeatRate reads this box's remembered rate for the local agent seat.
func (r *runner) localSeatRate() (seatrate.Seat, bool) {
	root, err := gpulease.ResolveStateRoot(r.cfg.StateDir)
	if err != nil {
		return seatrate.Seat{}, false
	}
	store, _ := seatrate.Load(seatrate.Path(root))
	known := store.Get(strings.TrimSpace(r.cfg.AgentPlannerModel("")))
	return known, known.TokS > 0
}

// retryFloorSource names, for the retry note, where a raised floor came from.
func retryFloorSource(floor int) string {
	if floor > minRetrySec {
		return ", agent_retry_min_sec"
	}
	return ""
}

// retrySeatBusy reports whether the seat the retry would land on is already
// generating for another job. A LOCAL landing reads the seat's in-flight count
// through llama-swap (probeLocalBusy, fail-open to idle); a REMOTE landing asks
// the node's OWN CEILING from a FRESH health probe. Fresh, not the run's cached
// view: route=spread probes the fleet once before the batch starts, so the
// cached view cannot show the load a SIBLING subtask of the same batch has
// since put on that node — which is exactly how the 2026-09-10 retry joined a
// seat mid-generation. A failed probe falls back to the cached view (fail-open,
// like every placement read).
//
// The remote predicate is !provablyStartsNow(view), the same ceiling-aware rule
// gate.go ranks placements with — not `JobsRunning > 0`. Register D-46 shipped
// the RULE and not the threshold, and 0 is the wrong threshold on every node
// that runs more than one worker: a four-worker box with ONE job in flight
// refused every cross-seat retry although three workers were idle. "Busy" here
// means "the retry would queue", which is precisely what provablyStartsNow
// answers — and it answers it conservatively, so an unknown ceiling still reads
// as busy exactly where it used to.
func (r *runner) retrySeatBusy(ctx context.Context, alt placement) (bool, string) {
	if alt.base == "" {
		rd := r.probeLocalBusy(ctx)
		if rd.busy {
			if rd.occupiedBy != "" {
				return true, rd.why()
			}
			if rd.inflight == 0 {
				return true, rd.note // a load in progress: the count is unknown, the note says why
			}
			return true, fmt.Sprintf("%d in flight on the local seat", rd.inflight)
		}
		return false, ""
	}
	view, source := alt.view, "cached view"
	if fresh, err := FetchNodeView(ctx, alt.base, r.cfg.FleetAuthToken); err == nil {
		view, source = fresh, "fresh health"
	} else if ctx.Err() == nil {
		// The cached view is the round's own snapshot and can be minutes old: a wait of up
		// to agent_placement_wait_sec against a node that stopped answering must not look
		// like a wait against a node that kept saying "busy". Named in the note, and logged
		// once per node per run.
		source = "cached view; fresh health could not be read: " + err.Error()
		if _, warned := r.probeWarned.LoadOrStore("retry-seat:"+alt.base, struct{}{}); !warned {
			log.Printf("delegate: retry seat %s: fresh health could not be read (%v); judging it from the cached view", alt.base, err)
		}
	}
	if !provablyStartsNow(view) {
		return true, seatBusyNote(view, source)
	}
	return false, ""
}

// seatBusyNote renders WHY the retry seat would queue, from the node's own
// published numbers. Two shapes because a ceiling of 0 is UNKNOWN, never a
// limit: printing "jobs_running 1 of max_concurrent_jobs 0" would state a
// capacity the node never advertised.
func seatBusyNote(v NodeView, source string) string {
	if v.MaxConcurrentJobs > 0 {
		return fmt.Sprintf("jobs_running %d of max_concurrent_jobs %d, jobs_queued %d (%s)",
			v.JobsRunning, v.MaxConcurrentJobs, v.JobsQueued, source)
	}
	return fmt.Sprintf("jobs_running %d, queue_depth %d, no published ceiling (%s)",
		v.JobsRunning, v.QueueDepth, source)
}

// placements is ONE SUBTASK's placement ledger. runOne owns it and hands the
// same pointer to every placeAndRun call it makes, which is the whole point:
// the exclusion set and the re-placement bound are properties of the SUBTASK,
// not of one placeAndRun invocation.
//
// Round-3 review found what per-call state actually cost, and neither symptom
// was visible from inside a single call:
//
//   - The bound doubled. runOne calls placeAndRun twice (first attempt, then
//     the verification retry) and each built its own counter, so a subtask
//     could make eight placements while the constant, the CHANGELOG and the
//     docs all said four.
//   - The retry could run on the seat the first attempt already used. Traced:
//     route=auto with the local GPU busy, two remotes refuse, the first attempt
//     falls to LOCAL and fails acceptance; the retry is forced remote, that
//     remote refuses, the bound trips, and the retry's own fresh map has no
//     record of local — so it lands local a second time and publishes
//     "this result is the retry" over two runs of the same seat. That defeats
//     the measured premise of the retry (the 27B and the 4B each missed a
//     DIFFERENT contract); a retry on the same seat is not a second opinion.
type placements struct {
	// tried is every dial base this subtask has been placed on ("" = the local
	// seat). EVERY attempt records itself here, including one that SUCCEEDED —
	// the retry's whole premise is a different seat, so the seat that just
	// answered has to be excluded from it.
	tried map[string]bool
	// used counts remote re-placements spent across the whole subtask.
	used int
	// credit is time this subtask spent WAITING FOR CAPACITY (awaitCapacity):
	// idle polling, never an attempt. It is not charged to timeout_sec — the
	// same rule as time provably spent queued on a node — so remaining() adds
	// it back. The idle polling is bounded by agent_placement_wait_sec. Two spans of
	// queueing also land here when their attempt is refused (noteRefusal): the time a
	// job provably sat in the backlog of a node that then took it back (ADR 0064;
	// bounded by the queue budget) and the admission wait a local capacity defer
	// already spent in the seat's own line.
	credit time.Duration
	// capacityRefusal records that the subtask's trouble is CAPACITY (a node
	// refused it with a 503/429: queue full, leased, draining, shed; or
	// replacementNode found an eligible node, or the local run-cap line, merely
	// busy: no room, a cooldown, a backlog past the caller's patience, a full
	// process gate) rather than a request or an address that was wrong. Only then
	// is a capacity wait worth running: a roster that 404s, is unreachable or can
	// never run the contract does not free up.
	capacityRefusal bool
	// excluded is every dial base that refused for a NON-capacity reason
	// (404/408/409, or unreachable): nothing about such a node frees up, so
	// the capacity wait never asks it again, however much room its health
	// claims. A capacity refusal (503/429) gets a cooldown instead.
	excluded map[string]bool
	// waitNote is why the subtask went to the capacity wait when nothing REFUSED it:
	// the process gate turned its dispatch away, or a deal handed it over because
	// every node with room was already dealt to its headroom. It is narration only -
	// carried on the landing's placement reason and the defer's reason - and never
	// counted as a refusal or a replacement.
	waitNote string
	// ran is every dial base ("" = the local seat) that TOOK a job of this subtask:
	// it ran the job or answered it, and did not decline it at dispatch. A node that
	// REFUSED for capacity is in tried but not here, because the capacity wait's whole
	// job is to ask it again; one that took the first attempt is excluded from the
	// wait's candidates, because the verification retry's premise is a DIFFERENT seat
	// from the one whose answer it corrects.
	ran map[string]bool
	// attempts counts REAL placements (a dispatch or a local run — anything
	// that went through attempt()'s finish and so wrote its telemetry row).
	// A subtask that ends with none of them (reserved seat, nothing freed) is
	// recorded by settle(); one that made at least one is already in the
	// corpus and the ledger through that attempt, the way exhausted() relies on.
	attempts int
}

// settle is the telemetry close-out for an outcome NO attempt produced —
// a capacity defer, a shed, or the holder-naming deferral reached without a
// single dispatch. attempt()'s finish records every real placement; these
// results are built outside it, and before this helper a subtask whose whole
// life was "reserved seat, nothing freed" left zero corpus rows and zero
// ledger rows (review finding, 2026-09-06). Recorded once, only when the
// subtask made no real attempt at all — otherwise the last attempt's row
// already stands, exactly as exhausted() leaves it.
func (r *runner) settle(contract core.AgentContract, pr PlacedResult, pl *placements, since time.Time) PlacedResult {
	// A capacity wait (or a lease wait) that ended because the CALL did is the call
	// deadline, not "no node had room" (ADR 0065). It quotes nothing of the wait's own
	// text: the placement narration carries that history.
	cutBefore := pr.deadlineCut
	pr = r.cutOutcome(pr, false)
	cutHere := pr.deadlineCut && !cutBefore
	if pl.attempts > 0 && !cutHere {
		return pr
	}
	// A wait the deadline ended after refused attempts (ADR 0065 decision 2) is recorded
	// here like any other cut: no attempt produced this outcome, so no attempt's row can
	// say it. The row gets a job id of its own — capacityDefer copies the id of the last
	// REFUSED dispatch, whose row already says "dispatch ... 503" for a job no node held,
	// and a second row under it would double-count one id — and the caller is given that
	// id, so the result, the ledger row and the corpus row are one thing.
	if pr.JobID == "" || (cutHere && pl.attempts > 0) {
		pr.JobID = mintJobID()
	}
	pr.wallMs = time.Since(since).Milliseconds()
	r.record(contract, pr)
	return pr
}

func newPlacements() *placements {
	return &placements{tried: map[string]bool{}, excluded: map[string]bool{}, ran: map[string]bool{}}
}

// noteRefusal files a refused attempt in the ledger: a capacity refusal (a 503 or
// 429) makes the subtask wait-worthy; any other refusal excludes that node from the
// wait. The local seat's own capacity defer files nothing here on purpose: it is a
// refusal to LIST (refusalLine) but not, by itself, a reason to wait - replacementNode
// marks the subtask wait-worthy when some ELIGIBLE node is merely busy, and a defer
// beside a remote that can never run the contract would otherwise hold the subtask for
// the whole placement wait to publish the same defer.
func (pl *placements) noteRefusal(pr PlacedResult) {
	// Time the refused job provably spent QUEUED on a node that then took it back
	// (a confirmed withdrawal at the queue deadline) is not execution: it is
	// credited like every other span the subtask spent waiting rather than
	// working, so the re-placement still owns the budget the contract asked for.
	pl.credit += pr.queuedWait
	if capacityRefusal(pr.refusalStatus) {
		pl.capacityRefusal = true
		return
	}
	if pr.ranBase != "" {
		pl.excluded[pr.ranBase] = true
	}
}

// noteRefusal is the runner's filing of a refused attempt: the subtask's own
// ledger (placements.noteRefusal), the run's per-node cooldown for a capacity
// refusal, and the time a LOCAL capacity defer already spent waiting in the seat's
// own line - which is queueing, so it is credited back exactly as the capacity
// wait's idle time is and never charged to the contract's execution budget.
func (r *runner) noteRefusal(pl *placements, pr PlacedResult) {
	pl.noteRefusal(pr)
	r.noteCooldown(pr)
	if capacityDeferRefusal(pr) && pr.Result.AdmissionWaitSec > 0 {
		pl.credit += time.Duration(pr.Result.AdmissionWaitSec * float64(time.Second))
	}
}

// cooldowns is a run's per-node "do not ask before" instants, keyed by dial base.
// The zero value is ready to use.
type cooldowns struct {
	mu    sync.Mutex
	until map[string]time.Time
	// at is when the latest refusal that held a node was noted: a health read taken before it is the
	// picture that refusal just proved wrong, so it can say nothing about the hold (lift).
	at map[string]time.Time
	// lifted marks a node whose hold a post-refusal health read lifted once this run, and firm a node
	// that refused again after that: its counters did not predict its refusals, so its Retry-After
	// stands for the rest of the run. One lift is the bound on how often a node whose health reports
	// free workers while it keeps answering 503 (a stale VRAM snapshot, say) can be re-asked early.
	lifted map[string]bool
	firm   map[string]bool
	// liftNote is, per base, the words for a lift that has not yet been followed by anything else: the
	// note and the instant the hold it ended would have run out on its own. A new hold (the node refused
	// after the lift) forgets it, and so does liftNarration once that instant has passed.
	liftNote map[string]liftedCooldown
}

// liftedCooldown is a Retry-After hold that a health read ended early (cooldowns.lift): the words that
// narrate it, and when the hold would have ended on its own.
type liftedCooldown struct {
	note  string
	until time.Time
}

// hold records that base is off limits until `until` (never shortening a hold
// already in force).
func (c *cooldowns) hold(base string, until time.Time) {
	if base == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.until == nil {
		c.until = map[string]time.Time{}
		c.at = map[string]time.Time{}
		c.lifted = map[string]bool{}
		c.firm = map[string]bool{}
		c.liftNote = map[string]liftedCooldown{}
	}
	if cur, ok := c.until[base]; !ok || until.After(cur) {
		c.until[base] = until
	}
	c.at[base] = time.Now()
	delete(c.liftNote, base) // refused after a lift: a dispatch after THIS hold ends was not allowed by that lift
	if c.lifted[base] {
		c.firm[base] = true // refused again after its hold was lifted
	}
}

// newestHold is the latest refusal that holds any node at now, or the zero time when none does:
// a health read for the capacity wait is taken after it, so a hold can be judged against it.
func (c *cooldowns) newestHold(now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var newest time.Time
	for base, until := range c.until {
		if now.Before(until) && c.at[base].After(newest) {
			newest = c.at[base]
		}
	}
	return newest
}

// lift ends base's hold when a health read taken AFTER its latest refusal proves a free worker
// (freeWorkerProven): the hold is the node's Retry-After, its own estimate at refusal time of when a
// worker frees, and a later read that shows one free has outdated it. readAfter is the instant the
// read was guaranteed to postdate (fetchViewsDetailedSince's notBefore); a hold set after it was not
// judged by that read. Unknown and saturated readings keep the hold, as does a node that already
// refused after a lift (firm).
func (c *cooldowns) lift(base string, v NodeView, readAfter, now time.Time) (note string, lifted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, held := c.until[base]
	if !held || !now.Before(until) || c.firm[base] || readAfter.IsZero() || c.at[base].After(readAfter) || !freeWorkerProven(v) {
		return "", false
	}
	delete(c.until, base)
	c.lifted[base] = true
	note = fmt.Sprintf("its Retry-After cooldown (%s left) was lifted: its health, read after the refusal, shows %d of %d worker(s) running and none queued",
		until.Sub(now).Round(time.Second), v.JobsRunning, v.MaxConcurrentJobs)
	if c.liftNote == nil {
		c.liftNote = map[string]liftedCooldown{}
	}
	c.liftNote[base] = liftedCooldown{note: note, until: until}
	return note, true
}

// liftNarration is the lift note a dispatch to base may carry, or "". A lift explains a dispatch only while
// it is what let the node be asked: the hold it ended would still be in force at now, and the node has not
// refused since (hold forgets the note). A node lifted on one tick and dispatched on a later one, after the
// hint had run out anyway, was asked because the hint ended; saying its cooldown "was lifted" would claim a
// speed-up that never happened.
func (c *cooldowns) liftNarration(base string, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.liftNote[base]
	if !ok || !now.Before(l.until) {
		return ""
	}
	return l.note
}

// heldUntil reports the instant base becomes a candidate again, when that is
// still in the future.
func (c *cooldowns) heldUntil(base string, now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[base]
	if !ok || !now.Before(until) {
		return time.Time{}, false
	}
	return until, true
}

// noteCooldown turns a capacity refusal into a cooldown on the node that sent it.
//
// The instant is JITTERED once, here: K dispatchers that were refused by the same
// node must not all ask it again on the same beat (jitterFrac). The length is the
// node's own Retry-After (in the wall's unit, so a test that compresses the wall
// compresses the hint) or, when the refusal carried none, refusalCooldown - the
// node's health may still advertise the room it just denied, because the snapshot
// is stale by construction (see saturated).
//
// A hint at or above maxQueuedWait - the node clamps its own hint at 300 s and
// says ">=300 s" - is a backlog the delegator would not wait out anyway: the
// comparison is the header's number against the delegator's OWN constant, never
// the node's prose, and the cooldown is capped at that ceiling. The refusal has
// already re-placed the subtask; nothing sleeps.
func (r *runner) noteCooldown(pr PlacedResult) {
	if !pr.refused || !capacityRefusal(pr.refusalStatus) || pr.ranBase == "" {
		return
	}
	d := refusalCooldown
	if pr.retryAfterSec > 0 {
		d = time.Duration(pr.retryAfterSec) * pollSecond
	}
	// The ceiling holds AFTER the jitter: capped first and jittered up by 20 %, a node's
	// 300 s hint held it out for 360 s, while the docs, the ADR and this comment all say
	// 300. Clamping the jittered value would fix that and pile every dispatcher onto the
	// same instant, so the excess is folded back under the ceiling instead: a hint at or
	// above it holds the node out for 240-300 s, spread, never past 300.
	ceiling := time.Duration(int(maxQueuedWait/time.Second)) * pollSecond
	hold := jittered(min(d, ceiling))
	if hold > ceiling {
		hold = 2*ceiling - hold
	}
	r.cool.hold(pr.ranBase, time.Now().Add(hold))
}

// remainingSec is what is LEFT of a subtask's timeout_sec budget. Elapsed
// rounds UP: crediting a 1.2 s attempt as 1 s overstates what is left, and no
// later placement may be promised time the subtask does not have. It can go
// negative, which every caller reads as "nothing left" through a floor check.
func remainingSec(start time.Time, budget int) int {
	return budget - int((time.Since(start).Milliseconds()+999)/1000)
}

// remaining is remainingSec with the subtask's capacity-wait credit added
// back: the ledger's `start` is shifted forward by exactly the idle time the
// wait spent, so a contract that waited 90 s for a node still owns its whole
// execution budget when one finally takes it.
func (pl *placements) remaining(start time.Time, budget int) int {
	return remainingSec(start.Add(pl.credit), budget)
}

// replacementExhaustedPrefix opens the message for a subtask NO NODE TOOK. It
// is a stable grep key and is deliberately distinct from the two deadline
// sentences 0.100.0 already produces, because they are three different facts
// about three different failures:
//
//	"placement refused"  — every node the delegator was willing to ask said no
//	                       (or could not be reached). No seat ever saw the
//	                       contract, so there is nothing to defer about.
//	"queue deadline"     — ONE node accepted it and never started it.
//	"poll deadline"      — ONE node started it and never finished it.
//
// It is an Err (Summary.Failed, non-zero CLI exit, IsError on the MCP surface)
// and never a defer, for the same reason the queue deadline is: a defer
// manufactures an AgentWireResult shaped like something a SEAT produced, and
// the only class that would fit — `budget` — teaches every consumer that the
// seat needed more time. No seat was ever asked.
const replacementExhaustedPrefix = "placement refused"

// placeAndRun runs ONE placement and, for as long as the node REFUSES the job
// at dispatch, re-places the subtask on another node — other eligible remotes
// first, then the local seat.
//
// WHY ONLY AT DISPATCH, and why that is a safety property rather than a
// convenience: a refusal is a NON-ACK. The node never took ownership, so no
// seat anywhere can be running the contract, and re-placing it cannot produce
// two concurrent runs. Everything that happens after a 202 — a poll 404, a
// queue deadline, a poll deadline — leaves a job the node may still hold, and
// re-placing THOSE would be the delegator arranging a double run. They stay
// exactly as they were.
//
// The one exception is a job the NODE says it never ran (ADR 0064): the queue
// deadline at which the node confirmed it took the job back (DELETE
// /fleet/jobs/{id}), and a poll that reads the node's own `reaped` / `withdrawn` /
// `not started` record. No seat holds such a job and none ever will, so it is filed
// as the capacity refusal it amounts to (refuseAsWithdrawn, refuseAsNeverRan) and
// re-placed like any other, with the time it sat queued credited back. A withdraw
// the node did not confirm changes nothing: the job may still start, so the result
// stays what it was.
//
// The one honest residual: when both dispatch attempts fail at TRANSPORT level
// (status 0), the first POST may have landed and had its ack lost, so the
// abandoned node could still run the contract once. That costs wasted compute
// on a node nobody is polling any more and the alternative is losing the work
// with certainty. It is safe because the fleet agent loop is built with no
// write/run/fetch capability (internal/pipeline/agenttask.go) and the node
// registers no delegate tool (internal/fleetnode/tasks.go) — so a duplicate run
// has nothing to duplicate. Note where that guarantee LIVES, though: in another
// package, and agenttask.go already anticipates a v2 delegate tool. If the
// fleet lane ever gains a mutating tool, this residual stops being free.
//
// WHAT THE BUDGET DOES AND DOES NOT BOUND. Every placement is handed what is
// LEFT of the contract's timeout_sec, re-measured immediately before dispatch,
// so no seat is ever promised execution time the subtask no longer owns. That
// is a claim about EXECUTION, not about end-to-end wall, and the difference is
// real rather than pedantic: two legs of a placement are bounded but are not
// charged to timeout_sec, because neither can be known before it is paid —
//
//	selection  fetchViews probes remotes SEQUENTIALLY at fetchNodeViewTimeout
//	           each. Bounded below by a ctx of whatever the contract has left,
//	           which is what stops a roster of blackholing nodes from spending
//	           a budget the subtask no longer owns.
//	dispatch   up to dispatchAttempts × dispatchRequestTimeout before a
//	           transport verdict.
//
// and 0.100.0's queued-time credit already extends the poll wall past
// timeout_sec by design (bounded by queueBudgetFor). So the wall a subtask can
// consume is timeout_sec + pollGrace + queued credit + this placement overhead,
// all bounded, and the loop CONVERGES because every blocking leg is charged to
// the next measurement and the floor is re-checked immediately before dispatch.
// Anything that says timeout_sec is an end-to-end wall ceiling is wrong; it is
// the execution budget.
func (r *runner) placeAndRun(ctx context.Context, i int, contract core.AgentContract, forced *placement, start time.Time, budget int, pl *placements) PlacedResult {
	// Why an earlier attempt of this subtask waited when nothing refused it belongs to
	// THAT attempt: the verification retry is another placeAndRun over the same ledger.
	pl.waitNote = ""
	pr := r.attempt(ctx, i, contract, forced)
	if pr.waitCapacity {
		// Nothing was dispatched: the only placement was a seat a text lease
		// reserves. The capacity wait owns it from here (0.113.18) — before this
		// the subtask waited on the LOCAL lease alone and then deferred, even
		// when a remote had freed up in the meantime.
		//
		// A DECIDED sentinel (the pair's long seat waiting for a busy agent
		// seat to drain, ADR 0039) carries no refusal: nobody declined the
		// work, so the decision's reason must not be tallied as a replacement.
		// Neither does a gate turn-away or a deal's overflow (pl.waitNote): the
		// node was never asked, so their reason says why the subtask waits and
		// is no refusal, is not counted in Summary.Replaced, and names no node
		// that "refused". A lease sentinel keeps its long-standing convention.
		refusals := []string{pr.pendingReason}
		pl.waitNote = ""
		switch {
		case pr.decided != nil:
			refusals = nil
		case pr.gated || pr.overflow:
			refusals, pl.waitNote = nil, pr.pendingReason
		}
		return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, "")
	}
	// Recorded whatever the outcome — a SUCCESSFUL placement must exclude its
	// own seat from the verification retry just as firmly as a refused one.
	pl.tried[pr.ranBase] = true
	pl.attempts++
	if !r.isReplaceable(pr) {
		return pr
	}
	refusals := []string{refusalLine(pr)}
	r.noteRefusal(pl, pr)
	for {
		// What the iteration spends after the last attempt returned (selection, mostly) is
		// the span a closing row for a chain the call deadline ended would measure.
		iterStart := time.Now()
		remaining := pl.remaining(start, budget)
		if remaining < minRetrySec {
			return r.exhaustedSettled(contract, pr, refusals, budgetSpent(remaining, budget, len(refusals)), iterStart)
		}
		// Selection BLOCKS (fetchViews). Bound it by what is actually left, so
		// the probe cannot outlive the budget it is probing on behalf of.
		selCtx, cancel := context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
		// The re-placement will be dispatched with what is LEFT of the budget, so its
		// candidates are judged against that wall - feasibility and the patience the
		// backlog gate holds a node to - not against the contract's original one: a node
		// that fits the whole budget but not the remainder would be handed a job the
		// delegator then gives up on while the node runs it.
		left := contract
		left.TimeoutSec, left.TimeoutAuto = remaining, false
		next, why, ok := r.replacementNode(selCtx, left, pl, len(refusals))
		cancel()
		if !ok {
			// Every node that could take it is FULL, not broken — or the one
			// seat left is reserved by a text lease (whatever the refusals
			// were: a 409 elsewhere does not make the lease less releasable):
			// wait for something to free rather than fail on a snapshot of one
			// minute. A roster that only 404s or is unreachable is exhausted.
			//
			// So is a roster whose only seat left is a lease's when this box declares no layer the
			// contract names (register A-108): the lease frees a seat that could never run it, and the
			// wait keeps that seat out (localCan), so it could place nothing and would spend its whole
			// TTL to end as a capacity defer saying no node had room. `why` already says it.
			reserved := r.route != "remote" && !pl.tried[""] && r.localServesLayer(r.localView(), contract) &&
				Reserved(r.localLease(contract))
			if pl.capacityRefusal || reserved {
				return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, why)
			}
			return r.exhaustedSettled(contract, pr, refusals, why, iterStart)
		}
		// RE-MEASURE. `remaining` above was true when taken and the probe may
		// have consumed most of it; committing the stale number to the wire is
		// exactly how a seat gets handed time the subtask no longer has. The
		// ledger is not touched until this check passes, so a placement
		// abandoned here costs neither a slot in the bound nor an exclusion.
		remaining = pl.remaining(start, budget)
		if remaining < minRetrySec {
			return r.exhaustedSettled(contract, pr, refusals, budgetSpent(remaining, budget, len(refusals)), iterStart)
		}
		pl.tried[next.base] = true
		if next.base != "" {
			// REMOTE re-placements only. Charging the local fall-back against
			// the bound would contradict the whole reason local is reserved: a
			// wide roster could then spend the bound before reaching the one
			// seat that is always able to take the work. Local is held to one
			// use per subtask by pl.tried instead.
			//
			// HONESTLY: under today's control flow this guard changes no
			// outcome, and the mutation battery confirmed it — charging local
			// too is semantically inert. Proof: replacementNode returns local
			// only when the untried remote pool is EMPTY (so no later remote
			// placement is possible at all, `tried` only grows) or when the
			// bound is ALREADY spent (used >= maxRemoteReplacements, so one
			// more makes no difference). It is kept because it encodes the
			// documented rule at the point the rule applies, and it becomes
			// load-bearing the moment the bound, the ordering or local's
			// reservation changes. It is not claimed as tested.
			pl.used++
		}
		replaced := contract
		replaced.TimeoutSec = remaining
		replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
		if r.beforeForced != nil && next.base != "" {
			r.beforeForced(next.base)
		}
		pr = r.attempt(ctx, i, replaced, &next)
		if pr.waitCapacity {
			// The local last resort's composite decision must WAIT (the
			// pair's long seat over a busy agent seat), or the process gate
			// turned the dispatch away: nothing ran, so this is the capacity
			// wait's from here, carrying the refusals that emptied the roster —
			// never a published sentinel.
			if pr.gated {
				// Nothing was sent to that node, so it is neither tried nor
				// spent against the re-placement bound.
				delete(pl.tried, next.base)
				if next.base != "" {
					pl.used--
				}
				pl.waitNote = pr.pendingReason
			}
			return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, why)
		}
		pl.attempts++
		pr.Replacements = len(refusals)
		if !r.isReplaceable(pr) {
			// Some node TOOK it (or the call's deadline ended the chain before one was asked:
			// annotateLanding). Whether its answer was any good is the four outcome buckets'
			// business, not this loop's.
			return annotateLanding(pr, refusals)
		}
		refusals = append(refusals, refusalLine(pr))
		r.noteRefusal(pl, pr)
	}
}

// capacityRefusal: the node declined because it has no room right now (queue
// full, leased, draining, shed) — the refusals a capacity wait can outlast.
func capacityRefusal(status int) bool {
	return status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests
}

// placementPollInterval is how often the capacity wait re-reads the fleet's
// health; refusalCooldown keeps a node that just refused for capacity off the
// candidate list for a moment when its refusal carried no Retry-After of its
// own, because its health may still advertise the room it just denied (the
// snapshot is stale by construction — see saturated). A Retry-After replaces it
// (noteCooldown).
var (
	placementPollInterval = 3 * time.Second
	refusalCooldown       = 10 * time.Second
)

// probeTickBound bounds ONE capacity-wait tick's fleet probe: WHAT IS LEFT of
// the wait, and deliberately nothing tighter. The wait used to hand fetchViews
// the raw run context while every other call site wrapped it in a
// remaining-budget one, so a probe could outlive the wait it was serving.
//
// Two tighter bounds were tried and both were wrong, for reasons worth keeping:
//
//   - TWICE THE POLL INTERVAL (6 s in production). That sits below
//     fetchNodeViewTimeout (15 s), which is this repo's own boundary between
//     slow and down — "a node that cannot answer inside this is down, not busy".
//     A remote whose health takes 6–15 s under load was therefore cancelled on
//     EVERY tick for the whole wait; it never became a candidate, and it was
//     never negative-cached either, because noteDeadProbe rightly refuses to
//     blame a node for the caller giving up. The wait then expired saying "no
//     node had room", which was false.
//   - min(fetchNodeViewTimeout, remaining). Same number as the per-base bound,
//     and that is the trap: the per-base context is DERIVED from this one, so
//     the two deadlines land on the same instant and the tick's fires first
//     (it was created first). Every probe failure then looks like the caller
//     giving up, nothing is ever attributable to a node, the negative cache
//     stays empty, and a dead base is re-dialled at full cost on every tick —
//     measured at 30 dials across one compressed wait.
//
// Nothing tighter is NEEDED, which is what makes the simple bound the right
// one: the fan-out is concurrent and each goroutine is capped at
// fetchNodeViewTimeout, so one black-holed remote costs a tick that bound ONCE
// and is then skipped by the 30 s negative cache — never the sum over the
// roster, and never the whole TTL. This bound's only job is the one no other
// bound covers: a probe must not outlive the wait.
func probeTickBound(deadline time.Time) time.Duration {
	left := time.Until(deadline)
	if left <= 0 {
		// The caller re-checks the deadline immediately after; a zero-length
		// context would cancel the probe before it was sent.
		left = time.Millisecond
	}
	return left
}

// probeFailTally is one base's probe failures during a capacity wait: how many
// ticks it failed, and the most recent reason. Counted per base rather than
// appended per tick because a 40-tick wait against one dead node would
// otherwise render the same sentence forty times into the defer reason.
type probeFailTally struct {
	n    int
	last string
}

// probeFailureNote renders the per-base probe failures a capacity wait
// accumulated, or "" when every tick's probe answered. Sorted by base so the
// sentence is stable across runs (map order is not).
//
// It exists because the tick used to discard probeErrs with `_`: a wait spent
// entirely on remotes that never answered ended as "no node had room … 0
// refusal(s)", which told an operator to add a node when the nodes they had
// were failing to answer. A probe failure is NOT a refusal — nobody declined
// the work — so it is reported as its own clause.
func probeFailureNote(fails map[string]*probeFailTally) string {
	if len(fails) == 0 {
		return ""
	}
	bases := make([]string, 0, len(fails))
	for base := range fails {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	parts := make([]string, 0, len(bases))
	total := 0
	for _, base := range bases {
		f := fails[base]
		total += f.n
		if f.n == 1 {
			parts = append(parts, fmt.Sprintf("%s: %s", base, f.last))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s (last of %d)", base, f.last, f.n))
	}
	return fmt.Sprintf("; %d probe(s) failed during the wait: %s", total, strings.Join(parts, "; "))
}

// heldOutNote renders the nodes the last tick of a capacity wait passed over
// because they could not START the job inside the caller's patience, with the
// arithmetic each one printed, or "" when none was. A held-out node is not a
// refusal - it was re-read every tick and simply never fit - so it is its own
// clause, exactly as a probe failure is.
func heldOutNote(held map[string]string, already string) string {
	if len(held) == 0 {
		return ""
	}
	bases := make([]string, 0, len(held))
	for base := range held {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	parts := make([]string, 0, len(bases))
	for _, base := range bases {
		// The deal's own verdict line (a refusal-chain entry) may already carry
		// this node's arithmetic verbatim; print it once.
		if !strings.Contains(already, held[base]) {
			parts = append(parts, held[base])
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "; held out by the backlog gate (re-read every tick): " + strings.Join(parts, "; ")
}

// awaitCapacity is the delegator's queue (0.113.18, L5 of the fleet-flow
// chapter): a subtask every fitting node has just refused for CAPACITY — or
// whose only placement is a seat a text lease reserves — waits here, until its
// call's deadline less a reserve when the call has one, else for a TTL
// (agent_placement_wait_sec, or agent_lease_wait_sec when that is longer; see
// capacityWaitFor, ADR 0073),
// and is placed on the FIRST node that frees: the local seat once the lease
// clears, or a remote whose health says it has room (hasRoom). A node that
// passes the check and still refuses (its snapshot was stale) is one more tick,
// not a verdict. The idle time is credited to the contract's budget
// (placements.credit); the attempts themselves are charged as always.
//
// A SHEDDABLE run (priority -1) never waits: measurement traffic that finds no
// idle node is shed at once, class capacity, so it can never queue in front
// of — or behind — production work anywhere in the fleet.
//
// seed is the last refused attempt (or the sentinel); its published fields
// are reused the way exhausted reuses them. refusals is every refusal so far,
// oldest first; why is the sentence the refusal chain ended with (exhausted's
// tail), used verbatim when the wait is switched off.
//
// With the wait DISABLED (agent_placement_wait_sec < 0, and no lease wait)
// the outcome is exactly the pre-0.113.18 one: "placement refused" for a
// refused chain, the holder-naming deferral for a reserved seat.
func (r *runner) awaitCapacity(ctx context.Context, i int, contract core.AgentContract, start time.Time, budget int, pl *placements, seed PlacedResult, refusals []string, why string) (res PlacedResult) {
	// The fleet was failing its health probe when the sentinel was raised (seed.remotesUnreachable).
	// Whatever ends the wait carries that unless a remote answered while it waited: a local run
	// after the lease cleared, the defer that ends it, a budget defer. `seed` is replaced by
	// refused attempts, so the fact is taken here, and answered is declared up here so the
	// deferred stamp can read it. A remote that answered means the fleet was not dead.
	deadFleet := seed.remotesUnreachable
	answered := map[string]bool{}
	defer func() {
		if deadFleet && len(answered) == 0 {
			res.remotesUnreachable = true
		}
	}()
	localView := r.localView()
	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	// p2cSeed is W-11's ranking seed for every betterRemote comparison this
	// wait makes: minted ONCE so the draw stays consistent across the wait's
	// own ticks (never named `seed` — that identifier is this function's own
	// PlacedResult parameter).
	p2cSeed := mintP2CSeed()
	waitStart := time.Now()
	// decided is the composite decision behind the seed (ADR 0039, council
	// R1): the decided seat exists and holds the contract, it would only evict
	// a busy seat. Held in a local so a remote refusal inside the wait (which
	// replaces `seed`) cannot turn the decided wait into the lease wait — the
	// lease branch would then run local through attempt(), re-decide, and
	// hand back the sentinel as a result.
	decided := seed.decided
	if r.priority < core.BandNormal {
		// A sheddable run never waits, so a remote hint the process gate turned away has no tick to take the idle seat in:
		// it takes it here, under the deal's own guards, or it is shed.
		if pr, ok := r.hintSeatWithoutAWait(ctx, i, contract, start, budget, pl, seed, refusals, fmt.Sprintf("sheddable work (priority %d) does not wait", r.priority)); ok {
			return pr
		}
		return r.settle(contract, r.shedResult(localView, seed, refusals), pl, waitStart)
	}
	// How long the wait runs, and what bounds it (ADR 0073): a call that has a whole-call deadline waits
	// until it, less the reserve a placed job needs; any other call waits agent_placement_wait_sec.
	cw := r.capacityWaitFor(waitStart, decided != nil)
	wait := cw.wait
	if wait <= 0 {
		if cw.call != nil {
			// Neither the operator's off switch nor a chain of refusals that ended: the call has less left
			// than a placed job needs, so there is nothing to wait for (and a job placed now could only be
			// cut). A plain lease reservation keeps its holder-naming deferral; every other place in line
			// is the capacity defer, saying why it did not wait.
			if seed.waitCapacity && !seed.gated && !seed.overflow && !seed.fenced {
				return r.settle(contract, r.reservedDefer(localView, r.localLease(contract), 0, seed.pendingReason, cw.call), pl, waitStart)
			}
			return r.settle(contract, r.capacityDefer(localView, seed, 0, 0, refusals, waitEvidence{note: pl.waitNote, call: cw.call}), pl, waitStart)
		}
		if decided != nil {
			// The wait is switched off: a zero TTL. The expiry rule applies at
			// once — RUN on the decided seat (it holds the contract; only the
			// eviction was worth waiting for), never a capacity defer and
			// never the holder-naming deferral (no lease is held).
			note := fmt.Sprintf("capacity wait disabled (agent_placement_wait_sec=%d) — running on the decided seat %s at once; it evicts %s",
				r.cfg.AgentPlacementWaitSec, decided.Seat, decided.Evicts)
			return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, 0, *decided, false, note, waitStart)
		}
		if seed.gated || seed.overflow {
			// A remote hint the process gate turned away has no wait to take the idle seat in either: the operator switched
			// it off (agent_placement_wait_sec), and the choice is the seat or a defer.
			if pr, ok := r.hintSeatWithoutAWait(ctx, i, contract, start, budget, pl, seed, refusals,
				fmt.Sprintf("the capacity wait is switched off (agent_placement_wait_sec=%d)", r.cfg.AgentPlacementWaitSec)); ok {
				return pr
			}
			// Nothing is held and nobody refused: the subtask simply has nowhere to
			// wait. That is a capacity defer - the fleet is healthy, it was not this
			// contract's turn - never the holder-naming deferral of a lease nobody holds.
			return r.settle(contract, r.capacityDefer(localView, seed, 0, 0, refusals, waitEvidence{note: pl.waitNote}), pl, waitStart)
		}
		if seed.waitCapacity {
			if seed.fenced {
				return r.settle(contract, r.fencedDefer(localView, seed.pendingReason, 0), pl, waitStart)
			}
			return r.settle(contract, r.reservedDefer(localView, r.localLease(contract), 0, seed.pendingReason, nil), pl, waitStart)
		}
		return r.exhaustedSettled(contract, seed, refusals, why, waitStart)
	}
	deadline := cw.deadline
	spanStart := waitStart
	var idle time.Duration
	// stLeft is st with the wall the NEXT dispatch would carry: what is left of the
	// budget once the idle time so far is credited (never under the retry floor). The
	// candidates of a tick are judged against it - the same wall re-placement uses - so
	// a node is asked only when it can start the job inside the wait the dispatched
	// contract will actually be given.
	stLeft := func() Subtask {
		c := contract
		c.TimeoutSec = max(remainingSec(start.Add(pl.credit+time.Since(spanStart)), budget), minRetrySec)
		c.TimeoutAuto = false
		return Subtask{Contract: c, EstTokens: st.EstTokens}
	}
	// credit moves the idle span just ended onto the ledger, so an attempt
	// started now is measured against a budget that does not include it.
	credit := func() {
		now := time.Now()
		idle += now.Sub(spanStart)
		pl.credit += now.Sub(spanStart)
		spanStart = now
	}
	// The nodes' cooldowns live on the run (r.cool): an instant per base,
	// JITTERED once when the refusal happened (noteCooldown) so K dispatchers that
	// all refused the same node do not all re-ask it on the same beat, and set from
	// the node's own Retry-After when it sent one. Every subtask of the run reads
	// them: "node A is full" is a fact about A, not about the subtask that met it.
	//
	// heldOut is, per base, the LATEST reason a node that had room was passed over
	// because it could not start the job inside the caller's patience
	// (startsWithinPatience). Such a node is re-read every tick and asked the moment
	// its backlog fits; the reason is only kept so a wait that ends without placing
	// anywhere can print the arithmetic.
	heldOut := map[string]string{}
	// cooling is, per base, the LATEST reason an otherwise eligible node was not asked
	// this wait because its own refusal put it on cooldown: its health may advertise
	// room on every tick while a Retry-After longer than the wait keeps it out, and a
	// defer that said only "no node had room" would not be what happened.
	cooling := map[string]string{}
	// A node whose Retry-After cooldown a health read ended early (cooldowns.lift) is narrated on the
	// placement reason of the dispatch it allowed: cooldowns.liftNarration says when a lift still explains one.
	// probeFails is every base whose health probe failed DURING the wait, by
	// base: the tick used to drop these on the floor, so a wait that never
	// reached a single node reported "0 refusal(s)".
	probeFails := map[string]*probeFailTally{}
	// answered (declared at the top) is every base whose probe returned a view at least once in
	// this wait.
	var lease gpulease.Info
	// localCan: this box can run the contract at all. One that names a layer the box does not declare
	// (register A-108) never takes the local seat in this wait, whatever the seat's load or lease: the
	// wait is for a node that declares the layer.
	localCan := r.localServesLayer(localView, contract)
	// overflow: a deal kept this subtask off the local seat - because it read busy, or because
	// the deal had already spent an idle seat's run-cap line (ADR 0076) - and handed it over
	// because every node with room was already dealt to its headroom. The seat stays off limits
	// until it stops reading busy (localStillBusy) and its run-cap line has a free slot
	// (localSlotAhead, read every tick) - the wait exists to spare the subtask that pile-up, not
	// to rebuild it at tick zero.
	overflow := seed.overflow
	// dealBusy: the deal read the local seat busy (spread's busy rule, route=auto's busy
	// formula) and dealt around it. A subtask it sent to a remote that then turned out full
	// (the process gate, a refusal) stands in this wait exactly like an overflow subtask and
	// keeps the same rule: the seat is taken only once it stops reading busy. Before this only
	// an overflow subtask kept it, so a gate turn-away took the busy seat at the first tick
	// (the TestOverflowStaysOffABusySeatWhoseLoadBecomesUnreadable CI flake, also on main).
	dealBusy := overflow || r.dealReadLocalBusy()
	// reservedSeen: the wait began under, or saw, a text lease on the local seat -
	// it names the placement "lease cleared" when the seat opens. A gate turn-away
	// and a deal's overflow are not leases: no lease is held.
	reservedSeen := seed.waitCapacity && !seed.gated && !seed.overflow && !seed.fenced
	// fencedSeen: the wait began under, or saw, a lease that fences every local seat the contract
	// could run on (fencedLocal) - it names the placement "fence cleared" when the seat opens.
	fencedSeen := seed.fenced
	// occupiedSeen: the wait saw another vLLM seat occupy the local cards (the seat it names).
	occupiedSeen := ""
	// noteSuffix carries why the subtask waited, when nothing refused it, onto the
	// placement reasons the wait writes.
	noteSuffix := ""
	if pl.waitNote != "" {
		noteSuffix = "; " + pl.waitNote
	}
	// budgetGone ends the wait when the contract has no budget left for another
	// attempt. After refusals that is the chain's own exhausted message; with none -
	// a gate turn-away or a deal's overflow that no node ever refused, so nothing was
	// refused and nothing ran - it is a budget defer, as runDecided files it, and
	// never a `placement refused` failure naming a refusal nobody made.
	budgetGone := func(remaining int) PlacedResult {
		if len(refusals) > 0 {
			return r.exhaustedSettled(contract, seed, refusals, budgetSpent(remaining, budget, len(refusals)), waitStart)
		}
		return r.settle(contract, r.waitBudgetDefer(localView, seed, idle, budgetSpent(remaining, budget, pl.attempts), pl.waitNote), pl, waitStart)
	}
	// places is, per dial base ("" = the local seat), the place in line this wait last saw the
	// subtask standing in (GPU routing P7): what the node was held behind and when that is
	// expected to clear. Each part is replaced only when its own reading succeeds (the local
	// seat's when the lease is read, the remotes' when the fleet probe answers), so a tick the
	// deadline cuts short leaves the last good reading standing, and only that survives to the
	// defer that ends the wait, which publishes it (placeKept).
	places := map[string]PlaceWait{}
	// fleetFirst: a remote hint (ADR 0078) is placed remotes-first, and a subtask the process gate turned away has had
	// no look at the fleet since the deal: its node is full for THIS process, which says nothing about the others. So
	// the first tick reads the fleet before it takes the idle seat. Every other way into the wait (a deal's overflow,
	// a refusal, a lease) has just established that no remote has room, and keeps the seat's place in line at once.
	fleetFirst := r.remoteHint() && seed.gated
	for wait > 0 && ctx.Err() == nil {
		if cw.call != nil && !time.Now().Before(deadline) {
			// The last tick of a TTL wait still looks once more after its sleep; a wait bounded by the call
			// does not, because what it would place now is placed in the reserve it exists to keep.
			break
		}
		if decided == nil && r.route != "remote" && !pl.tried[""] && !fleetFirst {
			delete(places, "")
			lease = r.localLease(contract)
			if Reserved(lease) {
				reservedSeen = true
				if localCan {
					places[""] = localPlace(localView, "reserved by a lease", lease)
				}
			}
			// A fence on every seat of the chain keeps the local seat out of the wait too: the
			// seat would turn the run away at its own pre-check (fencedLocal, GPU routing P7).
			// A box that cannot run the contract at all (it declares no layer the contract
			// names) is no place in line, whatever leases it holds.
			fence, fenceWhy, fenced := r.fencedLocal(contract)
			fenced = fenced && localCan
			if fenced {
				fencedSeen = true
				places[""] = localPlace(localView, "fenced ("+fenceWhy+")", fence)
			}
			// The local seat is a candidate once no text lease reserves it AND its
			// run-cap line has a free slot ahead of a newcomer (localSlotAhead) - a
			// full seat is a line this subtask could not leave, and the wait exists to
			// spare it that. Where there is no other node to spare it for (route=local,
			// or no remotes configured) the seat's own line IS the wait, bounded by the
			// run's wall, and the subtask joins it as soon as no lease reserves it.
			free, _ := r.localSlotAhead()
			if r.route == "local" || len(r.remotes) == 0 {
				free = true
			}
			// With no node to wait for (route=local, or no remotes) the seat's own decision still
			// answers a layer it does not declare, by deferring it naming the layer; otherwise the
			// seat is no candidate for it.
			if !localCan && r.route != "local" && len(r.remotes) > 0 {
				free = false
			}
			// A subtask a deal kept off the seat for reading busy stays off it while
			// the seat still reads busy by the deal's own reading: the registry above
			// counts only delegated runs, and a seat busy with anyone else's requests
			// shows nothing there.
			// Whatever started the wait (a refusal, a lease, a deal, no remote at all), a seat
			// another vLLM seat occupies is no candidate: taking it unloads that seat
			// (probeLocalBusy's occupiedBy; operator 2026-10-06: "wait in line"). It becomes one
			// when the occupant leaves. Read first, so the reason can name the occupant.
			occupied := false
			if !Reserved(lease) && !fenced && free && r.route != "local" {
				if rd := r.busyReadingNow(ctx); rd.occupiedBy != "" {
					occupied, occupiedSeen = true, rd.occupiedBy
					places[""] = PlaceWait{Node: localView.NodeID, On: "seat", Detail: rd.why()}
				}
			}
			stillBusy := false
			if !occupied && dealBusy && free && !Reserved(lease) {
				// The overflow keeps the deal's whole reading; a subtask that reached the wait
				// another way (the gate, a refusal) keeps only the seat's load: a lease is judged
				// by the wait itself above (Reserved, fenced), and one this process holds is no
				// reason to stay off the seat.
				readLease := lease
				if !overflow {
					readLease = gpulease.Info{}
				}
				stillBusy, _ = r.localStillBusy(ctx, readLease)
			}
			if !Reserved(lease) && !fenced && free && !stillBusy && !occupied {
				credit()
				remaining := pl.remaining(start, budget)
				if remaining < minRetrySec {
					return budgetGone(remaining)
				}
				replaced := contract
				replaced.TimeoutSec = remaining
				replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
				// Reaching here means the local seat could not take the subtask when the
				// wait began - reserved by a text lease, its run-cap line full (an
				// unreserved local seat with a free slot is taken by replacementNode before
				// any wait), or busy when a deal dealt the run - and now can.
				reason := fmt.Sprintf("local seat had no free run-cap slot, one freed after %s — running local (capacity wait)", idle.Round(time.Second))
				switch {
				case reservedSeen:
					reason = fmt.Sprintf("local seat was reserved, lease cleared after %s — running local (capacity wait)", idle.Round(time.Second))
				case fencedSeen:
					reason = fmt.Sprintf("local seat was fenced by a lease, fence cleared after %s — running local (capacity wait)", idle.Round(time.Second))
				case occupiedSeen != "":
					reason = fmt.Sprintf("local seat was occupied by the vLLM seat %s, which left after %s — running local (capacity wait)", occupiedSeen, idle.Round(time.Second))
				case r.remoteHint() && !r.dealReadLocalBusy():
					// The deal kept the subtask off a seat that never read busy: a remote hint prefers the remotes
					// (ADR 0078), and none had room.
					reason = fmt.Sprintf("no remote had room and the local seat was free after %s — a remote hint falls back to it (capacity wait)", idle.Round(time.Second))
				case dealBusy:
					reason = fmt.Sprintf("local seat was busy when the deal kept this subtask off it, idle after %s — running local (capacity wait)", idle.Round(time.Second))
				}
				forced := placement{view: localView, reason: reason + noteSuffix}
				pr := r.attempt(ctx, i, replaced, &forced)
				if pr.waitCapacity {
					// The lease cleared, and the composite decision now asks
					// to wait for a busy pair seat to drain: the same wait
					// continues as a DECIDED one (nothing ran, nothing to
					// record); the tick below re-decides and runs the seat.
					decided = pr.decided
					continue
				}
				spanStart = time.Now() // the attempt is charged; the wait resumes here
				pl.tried[""] = true
				pl.attempts++
				if !r.isReplaceable(pr) {
					return r.landedAfterWait(pr, idle, refusals)
				}
				// The local seat deferred it as capacity after all (its line filled between
				// the read and the run): one more refusal, and the wait goes on for a
				// remote - the local seat is tried now and is not asked twice.
				seed = pr
				refusals = append(refusals, refusalLine(pr))
				r.noteRefusal(pl, pr)
			}
		}
		if r.route != "local" {
			// The tick's probe may not outlive the wait it serves; the per-base
			// bound inside the fan-out caps what any ONE remote can cost
			// (probeTickBound explains why nothing tighter belongs here).
			tickCtx, cancelTick := context.WithTimeout(ctx, probeTickBound(deadline))
			views, bases, _, failed, readAfter := r.fleetReadForWait(tickCtx)
			cancelTick()
			fleetFirst = false // the fleet has been read once: from the next tick the seat may take its place in line
			if ctx.Err() != nil || !time.Now().Before(deadline) {
				// The tick's probe is bounded by the wait's own deadline, and by the
				// context the wait runs under (the call's deadline, ADR 0065, or the
				// caller giving up), so a probe still in flight when either ended may
				// have failed because the WAIT or the CALL ended, not because its node
				// is down. For a node that answered earlier in this wait that is no
				// evidence at all: keep what the previous tick learned (cooling down,
				// held out) for the defer's reason. A node that never answered is still
				// named: nobody could ask it.
				for base, why := range failed {
					if answered[base] {
						continue
					}
					f := probeFails[base]
					if f == nil {
						f = &probeFailTally{}
						probeFails[base] = f
					}
					f.n, f.last = f.n+1, why
				}
				break
			}
			for _, b := range bases {
				answered[b] = true
			}
			// A base that did not answer is not a node with no room — it is a
			// node nobody could ask. Kept per base so the defer can say so.
			for base, why := range failed {
				f := probeFails[base]
				if f == nil {
					f = &probeFailTally{}
					probeFails[base] = f
				}
				f.n, f.last = f.n+1, why
			}
			best := -1
			prior := fleetTokSPrior(views)
			now := time.Now()
			clear(heldOut) // only THIS tick's read decides who is held out
			clear(cooling)
			for base := range places {
				if base != "" {
					delete(places, base)
				}
			}
			sn := stLeft()
			for j, v := range views {
				if pl.excluded[bases[j]] || pl.ran[bases[j]] {
					continue
				}
				if eligible, word, detail := eligibilityVerdict(sn, v); !eligible {
					// A node a lease fences for this contract is a place in line (it frees when the
					// lease ends); one that can never run the contract is not.
					if word == "lease" {
						places[bases[j]] = remoteLeasePlace(v, &sn, detail)
					}
					continue
				}
				// A node that refused for capacity is not asked again before its
				// cooldown ends, unless this tick's read - taken after the refusal - proves it
				// has a free worker (cooldowns.lift): its Retry-After was the node's own
				// estimate of when one would free, and a later read that shows one free has
				// outdated it. Health that proves nothing (an unknown, a saturated node, a
				// backlog) never lifts it: the snapshot may still advertise the room the
				// node just denied.
				if until, isCooling := r.cool.heldUntil(bases[j], now); isCooling {
					if _, liftedNow := r.cool.lift(bases[j], v, readAfter, now); !liftedNow {
						cooling[bases[j]] = fmt.Sprintf("%s: cooling down after its own refusal (%s left)", laneID(v), until.Sub(now).Round(time.Second))
						places[bases[j]] = PlaceWait{Node: laneID(v), On: "cooldown", Detail: "cooling down after its own refusal", EtaSec: int(until.Sub(now).Seconds())}
						continue
					}
				}
				patience, clamp := r.patience(sn.Contract, v)
				if !hasRoom(v, false) || !processGate.available(bases[j], admissionCeiling(v)) {
					places[bases[j]] = PlaceWait{Node: laneID(v), On: "queue", Detail: whyNoRoom(v, false, patience, clamp)}
					continue
				}
				// Room is not enough: a job sent to a node whose backlog outlasts what the
				// caller will wait is one the delegator abandons while the node runs it
				// anyway. Held out - re-read next tick, never refused for good.
				if ok, why := startsWithinPatience(v, patience); !ok {
					why += clamp
					heldOut[bases[j]] = laneID(v) + ": backlog (" + why + ")"
					places[bases[j]] = PlaceWait{Node: laneID(v), On: "backlog", Detail: why, EtaSec: int(queueWaitFor(v))}
					continue
				}
				if best < 0 || betterRemote(p2cSeed, &sn, prior, v, views[best]) {
					best = j
				}
			}
			if best >= 0 {
				credit()
				remaining := pl.remaining(start, budget)
				if remaining < minRetrySec {
					return budgetGone(remaining)
				}
				replaced := contract
				replaced.TimeoutSec = remaining
				replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
				forced := placement{view: views[best], base: bases[best],
					reason: fmt.Sprintf("capacity wait → %s (room after %s)", views[best].NodeID, idle.Round(time.Second)) + liftedClause(r.cool.liftNarration(bases[best], time.Now())) + noteSuffix}
				if r.beforeForced != nil {
					r.beforeForced(bases[best])
				}
				pr := r.attempt(ctx, i, replaced, &forced)
				spanStart = time.Now() // the attempt is charged; the wait resumes here
				if pr.gated {
					// Another Run of this process took the node's last slot between the read
					// and the dispatch: nothing was sent, so nothing is tried, counted or
					// cooled - the wait goes on and reads the fleet again next tick.
				} else {
					pl.tried[bases[best]] = true
					pl.attempts++
					if !r.isReplaceable(pr) {
						return r.landedAfterWait(pr, idle, refusals)
					}
					seed = pr
					refusals = append(refusals, refusalLine(pr))
					// The refusal cools the node (its own Retry-After, else refusalCooldown); a
					// non-capacity answer excludes it from the rest of the wait.
					r.noteRefusal(pl, pr)
					if capacityRefusal(pr.refusalStatus) || capacityDeferRefusal(pr) {
						eta := 0
						if until, cool := r.cool.heldUntil(bases[best], time.Now()); cool {
							eta = int(time.Until(until).Seconds())
						}
						places[bases[best]] = PlaceWait{Node: laneID(views[best]), On: "queue", Detail: "refused the dispatch for capacity", EtaSec: eta}
					}
				}
				// Fall through to the sleep: a refusal is charged (its attempt
				// wrote telemetry and spent budget), so re-asking is paced by the
				// poll interval — never a tight loop of dispatches.
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		tick := jitteredWithin(placementPollInterval, time.Until(deadline))
		select {
		case <-ctx.Done():
		case <-time.After(tick):
		}
		// The decided seed's tick (after the sleep — the decision that sent
		// the subtask here was made moments ago, so the first re-read is one
		// poll interval later): re-run the decider with fresh readings and run
		// on the decided seat the moment nothing has to be evicted. A text
		// lease that appeared mid-wait keeps the seat off limits; a decision
		// that now DEFERS (a guard flipped) ends the wait as that defer.
		if decided != nil && r.route != "remote" && ctx.Err() == nil && !Reserved(r.localLease(contract)) {
			dec := r.decide(ctx, contract, st)
			switch {
			case dec.Defer:
				credit()
				pr := r.decidedDefer(localView, dec, seed.PlacementReason)
				pr.waited, pr.CapacityWaitSec = true, idle.Seconds()
				return r.settle(contract, pr, pl, waitStart)
			case !dec.Wait:
				credit()
				note := fmt.Sprintf("capacity wait → local seat %s (%s drained after %s)", dec.Seat, decided.Evicts, idle.Round(time.Second))
				return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, idle, dec, true, note, waitStart)
			}
			decided = &dec // the freshest reason, for the expiry note
		}
	}
	credit()
	if decided != nil && r.route != "remote" && !pl.tried[""] {
		if info := r.localLease(contract); Reserved(info) {
			// A text lease took the cards during the wait: the holder is real,
			// and running on the reserved seat is the one thing the lease
			// forbids. The established deferral, naming the holder.
			return r.settle(contract, r.reservedDefer(localView, info, idle, decided.Reason, nil), pl, waitStart)
		}
		// TTL expiry for a decided seed RUNS on the decided seat (the plan's
		// rule): the seat holds the contract and is free to load; the wait was
		// only ever about sparing the seat it evicts. Never capacityDefer —
		// "no node had room" would be false.
		note := fmt.Sprintf("capacity wait expired after %s (agent_placement_wait_sec=%d) with %s still busy — running on the decided seat %s; it evicts %s",
			idle.Round(time.Second), r.cfg.AgentPlacementWaitSec, decided.Evicts, decided.Seat, decided.Evicts)
		return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, idle, *decided, true, note, waitStart)
	}
	if r.route != "remote" && localCan && Reserved(lease) {
		// Nothing freed and the local seat is still reserved: the established
		// deferral, naming the holder (class infrastructure — a human's timing
		// decision), with the refusals appended.
		return r.settle(contract, placeKept(r.reservedDefer(localView, lease, idle, "no eligible remote had room — "+strings.Join(refusals, "; ")+noteSuffix, cw.call), places), pl, waitStart)
	}
	return r.settle(contract, placeKept(r.capacityDefer(localView, seed, idle, wait, refusals, waitEvidence{note: pl.waitNote, probeFails: probeFails, heldOut: heldOut, cooling: cooling, call: cw.call, held: cw.held}), places), pl, waitStart)
}

// hintSeatWithoutAWait is the remote hint's seat fallback (ADR 0078 decision 5) for a subtask the process gate turned
// away when the capacity wait cannot carry it: the operator switched the wait off, or the run is sheddable and never
// waits. The deal gave the subtask a node because its health showed headroom, and never asked the gate; this is the
// first place the hint learns that node is full for THIS process. A remote pin would defer here while the seat idles, and
// the hint's whole difference is that the seat may run what no remote can take, so with no wait to read the fleet in
// and take the seat on a later tick, the choice is the seat now or a defer (a shed, for a sheddable run).
//
// The seat is taken under the guards of the deal's own fallback (dealAutoRemoteRoom): the seat read free when the deal was
// made (hintSeatFree: no lease, no full line, no load in progress, no vLLM seat to evict), and it serves the contract's
// layer. The deal's picture can be minutes old, so the conditions that can change are read again now: no lease reserves
// or fences the seat, it is not at its run cap, loading or occupied, and its run-cap line has a free slot ahead of a
// newcomer. An unreadable seat keeps the deal's answer (free), as the deal does. ok=false leaves the caller's outcome (the
// shed, the capacity defer) as it was, and nothing has run or been recorded: that includes a composite decision that
// asks the seat to wait, which is the wait's business and not this fallback's.
func (r *runner) hintSeatWithoutAWait(ctx context.Context, i int, contract core.AgentContract, start time.Time, budget int, pl *placements, seed PlacedResult, refusals []string, why string) (PlacedResult, bool) {
	if !r.remoteHint() || !seed.gated || !r.hintSeatFree || pl.tried[""] {
		return PlacedResult{}, false
	}
	localView := r.localView()
	if !r.localServesLayer(localView, contract) {
		return PlacedResult{}, false
	}
	if Reserved(r.localLease(contract)) {
		return PlacedResult{}, false
	}
	if _, _, fenced := r.fencedLocal(contract); fenced {
		return PlacedResult{}, false
	}
	if rd := r.busyReadingNow(ctx); rd.occupiedBy != "" || r.atRunCap(rd.inflight) || rd.loading {
		return PlacedResult{}, false
	}
	if free, _ := r.localSlotAhead(); !free {
		return PlacedResult{}, false
	}
	remaining := pl.remaining(start, budget)
	if remaining < minRetrySec {
		return PlacedResult{}, false
	}
	replaced := contract
	replaced.TimeoutSec = remaining
	replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
	reason := "the dealt node was full for this process and " + why + " — the idle local seat takes it instead, a remote hint falling back where no remote has room"
	if pl.waitNote != "" {
		reason += "; " + pl.waitNote
	}
	pr := r.attempt(ctx, i, replaced, &placement{view: localView, reason: reason})
	if pr.waitCapacity {
		return PlacedResult{}, false
	}
	pl.tried[""] = true
	pl.attempts++
	return annotateLanding(pr, refusals), true
}

// localPlace is the place the local seat holds in line while a lease keeps the contract off it:
// the lease that holds it and, when the lease declares an end that has not passed, when that is.
func localPlace(local NodeView, what string, info gpulease.Info) PlaceWait {
	eta := 0
	for _, l := range info.Each() {
		if l.ExpiresAt.IsZero() || l.ExpiresAt.UnixMilli() <= 0 {
			continue
		}
		if left := int(time.Until(l.ExpiresAt).Seconds()); left > 0 && (eta == 0 || left < eta) {
			eta = left
		}
	}
	return PlaceWait{Node: local.NodeID, On: "lease", Detail: what + " - " + HolderLine(info), EtaSec: eta}
}

// remoteLeasePlace is the place a remote node holds in line while a lease on the cards this
// contract needs fences it: the node's own reason, and the earliest end any such lease
// published (an overdue lease says nothing about when it frees).
func remoteLeasePlace(v NodeView, st *Subtask, detail string) PlaceWait {
	eta := 0
	for _, l := range v.leasesHolding(st, func(l LeaseView) bool { return !l.Overdue && l.RemainingSec > 0 }) {
		if eta == 0 || l.RemainingSec < eta {
			eta = l.RemainingSec
		}
	}
	return PlaceWait{Node: laneID(v), On: "lease", Detail: "fenced: " + detail, EtaSec: eta}
}

// placeKept stamps the places a wait ended behind onto the defer that ends it (GPU routing P7):
// the structured list, the soonest known end as a retry hint, and the same facts in prose, so a
// caller (or a model reading the reason) re-asks when it can succeed instead of guessing. The
// durable token that RESUMES a place across calls belongs to media admission (plan P13); this is
// the delegator's half, the facts a re-call is placed by. A wait that stood behind nothing it
// could name publishes nothing new.
func placeKept(pr PlacedResult, places map[string]PlaceWait) PlacedResult {
	if len(places) == 0 {
		return pr
	}
	list := make([]PlaceWait, 0, len(places))
	for _, p := range places {
		list = append(list, p)
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].Node != list[b].Node {
			return list[a].Node < list[b].Node
		}
		return list[a].On < list[b].On
	})
	soonest := 0
	parts := make([]string, 0, len(list))
	for _, p := range list {
		part := fmt.Sprintf("%s: %s (%s", p.Node, p.On, p.Detail)
		if p.EtaSec > 0 {
			part += ", expected to clear in " + etaPhrase(p.EtaSec)
			if soonest == 0 || p.EtaSec < soonest {
				soonest = p.EtaSec
			}
		}
		parts = append(parts, part+")")
	}
	clause := "; standing in line behind: " + strings.Join(parts, "; ")
	if soonest > 0 {
		clause += "; the soonest of those clears in about " + etaPhrase(soonest) + " (retry_after_sec)"
	}
	pr.PlaceKeeping, pr.RetryAfterSec = list, soonest
	pr.PlacementReason += clause
	pr.Result.Reason += clause
	return pr
}

// etaPhrase words a number of seconds for a reason an operator reads.
func etaPhrase(sec int) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	case sec >= 90:
		return fmt.Sprintf("%dm", (sec+30)/60)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// runDecided runs a decided-seed subtask on its decided seat once the capacity
// wait is over (drained, expired, or disabled): a forced LOCAL placement that
// carries dec (Wait cleared) so attempt() runs it rather than deciding again,
// with what is left of the budget after the wait's credit. waited marks the
// result as one that waited (Summary.Waited, capacity_wait_sec); a disabled
// wait ran at once and is not counted as one. A budget that cannot start the
// seat is a BUDGET defer when nobody refused the work (the wait is credited,
// so this is a contract whose timeout_sec was under the floor to begin with)
// and the established "placement refused" failure when nodes did.
func (r *runner) runDecided(ctx context.Context, i int, contract core.AgentContract, start time.Time, budget int, pl *placements, seed PlacedResult, refusals []string, idle time.Duration, dec placetable.Decision, waited bool, note string, waitStart time.Time) PlacedResult {
	remaining := pl.remaining(start, budget)
	if remaining < minRetrySec {
		if len(refusals) > 0 {
			return r.exhaustedSettled(contract, seed, refusals, budgetSpent(remaining, budget, len(refusals)), waitStart)
		}
		local := r.localView()
		reason := fmt.Sprintf("%s: %s — the decided seat %s was not started", budgetSpent(remaining, budget, pl.attempts), note, dec.Seat)
		pr := PlacedResult{
			Node: local.NodeID, Seat: dec.Seat, PlacementReason: seed.PlacementReason, Placed: placedOrNil(dec), Unplaced: true,
			waited: waited, CapacityWaitSec: idle.Seconds(),
			Result: core.AgentWireResult{
				SchemaVersion: core.AgentWireSchemaVersion,
				NodeID:        local.NodeID,
				Seat:          dec.Seat,
				Deferred:      true,
				DeferClass:    core.DeferClassBudget,
				Reason:        reason,
				Placed:        placedOrNil(dec),
			},
		}
		return r.settle(contract, pr, pl, waitStart)
	}
	replaced := contract
	replaced.TimeoutSec = remaining
	replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
	dec.Wait = false
	forced := placement{view: r.localView(), reason: note, decided: &dec}
	pr := r.attempt(ctx, i, replaced, &forced)
	pl.tried[""] = true
	pl.attempts++
	if !waited {
		return pr
	}
	return r.landedAfterWait(pr, idle, refusals)
}

// decidedDefer is the result for a LOCAL placement the composite decision
// refused (ADR 0039): a guard refused the layer, or no layer window holds the
// contract. Deferred under the decision's class with its reason, the placed
// block naming the guard or the largest window on the wire AND in the ledger
// row — never a silent trim, never a fall-back seat (spec D4). Nothing ran:
// Unplaced, and the seat named is the one that was refused (the planner seat
// never saw the contract).
func (r *runner) decidedDefer(local NodeView, dec placetable.Decision, reason string) PlacedResult {
	seat := local.AgentSeat
	if dec.Seat != "" {
		seat = dec.Seat
	}
	placed := placedOrNil(dec)
	return PlacedResult{
		Node: local.NodeID, Seat: seat, PlacementReason: reason, Placed: placed, Unplaced: true,
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			NodeID:        local.NodeID,
			Seat:          seat,
			Deferred:      true,
			DeferClass:    dec.DeferClass,
			Reason:        dec.Reason,
			Placed:        placed,
		},
	}
}

// landedAfterWait annotates a result some node TOOK after a capacity wait (or that the call's
// deadline cut before one was asked: annotateLanding).
func (r *runner) landedAfterWait(pr PlacedResult, idle time.Duration, refusals []string) PlacedResult {
	pr.waited = true
	pr.CapacityWaitSec = idle.Seconds()
	return annotateLanding(pr, refusals)
}

// annotateLanding files the refusal history on the attempt that ENDED a chain of refusals without
// being refused itself. Usually a node took it, and the subtask was re-placed once per refusal.
// The exception is an attempt the call's deadline cut before any node was asked (an Unplaced cut,
// ADR 0065: attempt starts nothing once the deadline has passed): nothing took it, so "re-placed
// after N refusals" would claim a placement that never happened, and Summary.Replaced would count
// it. It is filed the way exhausted() files a chain nobody took: one re-placement fewer than
// refusals (the placement the deadline ended is not one), the note worded for a subtask nobody
// ran, and no note when nothing was re-placed (a single refusal with nowhere to go).
//
// Only the deadline's cut is read this way. A subtask the local seat's own decision deferred was
// placed there (the seat was asked), and one the deadline cut while a node was running it was
// taken: both keep the wording and the count they had.
func annotateLanding(pr PlacedResult, refusals []string) PlacedResult {
	if pr.deadlineCut && pr.Unplaced {
		pr.Replacements = max(len(refusals)-1, 0)
		pr.ReplacementNote = ""
		if pr.Replacements > 0 {
			pr.ReplacementNote = replacementNote(refusals, false)
		}
		return pr
	}
	pr.Replacements = len(refusals)
	if len(refusals) > 0 {
		pr.ReplacementNote = replacementNote(refusals, true)
	}
	return pr
}

// waitEvidence is what a capacity wait learned besides its refusal chain, for the defer
// that ends it: why it waited when nothing refused it (note), the probes that failed
// during it, the nodes the backlog gate held out, and the nodes their own refusal's
// cooldown kept out. None of it is a refusal.
type waitEvidence struct {
	note       string
	probeFails map[string]*probeFailTally
	heldOut    map[string]string
	cooling    map[string]string
	// call is set when the call's deadline bounded the wait (ADR 0073), so the defer says that
	// and does not tell the caller to raise a setting that never bounded it.
	call *callWaitInfo
	// held is how many subtasks of the call had not started when a wait that kept its configured bound began
	// (capacityWait.held): the defer says why a call with time left stopped waiting.
	held int
}

// capacityWait is how long and until when one subtask's capacity wait runs, and what bounds it.
type capacityWait struct {
	// wait is how long the wait may run; 0 means there is nothing to wait for: the operator
	// switched the wait off, or (call set) the call has less left than a placed job needs.
	wait time.Duration
	// deadline is when the wait ends.
	deadline time.Time
	// call is non-nil when the call's deadline, not agent_placement_wait_sec, bounds the wait.
	call *callWaitInfo
	// held is how many subtasks of the call had not started when the wait began; non-zero means the wait
	// kept its configured bound, and not the call's, because it holds a run slot they are waiting for.
	held int
}

// callWaitInfo is what a wait bounded by the call's deadline was given, for the words of the
// defer that ends it.
type callWaitInfo struct {
	left    time.Duration // the call's remaining time when the wait began
	reserve time.Duration // what the wait leaves unspent, for a placed job to run in
	noTime  bool          // the call had no more than the reserve left, so the wait did not run
}

// words says what bounded the wait, in the order a reader of a defer needs it: the call's own
// clock, the reserve, and that the setting a caller might reach for does not apply.
func (c *callWaitInfo) words() string {
	if c.noTime {
		return fmt.Sprintf("the call had only %s left, no more than the %s a placed job needs to run in, so it did not wait", c.left.Round(time.Second), c.reserve.Round(time.Second))
	}
	return fmt.Sprintf("bounded by the call's deadline, not by agent_placement_wait_sec: the call had %s left when the wait began, less the %s a placed job needs to run in", c.left.Round(time.Second), c.reserve.Round(time.Second))
}

// placementWaitDefault is DefaultPlacementWait as the engine reads it: a var so a test can
// compress the built-in wait the way it compresses placementPollInterval. Production never
// mutates it.
var placementWaitDefault = config.DefaultPlacementWait

// placementTTL is the capacity wait of a call that has no whole-call deadline:
// agent_placement_wait_sec, else the built-in 120 s, and 0 when the operator switched the wait
// off. A call that has a deadline waits until it instead (capacityWaitFor), except a wait that
// holds a run slot while subtasks of the call have not started; the retry's wait
// (awaitRetrySeat) and a composite decision's eviction wait keep this TTL under any call
// (ADR 0073).
func (r *runner) placementTTL() time.Duration {
	if r.cfg.AgentPlacementWaitSec == 0 {
		return placementWaitDefault
	}
	return r.cfg.PlacementWait()
}

// capacityWaitFor decides how long a subtask's capacity wait runs (ADR 0073, completing the
// change ADR 0063 and ADR 0065 name as next): a call that has a whole-call deadline waits until
// that deadline less callWaitReserve, whatever agent_placement_wait_sec says, so band-0 work
// never defers for capacity while the call still has time to place it; a call that has none
// (the CLI verbs, agent_run, offload_ask) waits agent_placement_wait_sec as before.
//
// Three things keep the TTL under a deadline. A negative agent_placement_wait_sec is the
// operator's off switch for every call, and the deadline does not turn it back on. A decided
// seed (a composite box's pair seat draining) keeps it because its expiry RUNS the contract on
// the decided seat, which needs the time the reserve would take away. And a wait that holds a
// run slot while subtasks of the call have not started keeps the bound it had before the
// deadline (the larger of the TTL and agent_lease_wait_sec) unless the call ends sooner: only
// the last subtask to start waits for the horizon.
//
// agent_lease_wait_sec is part of that bound, never of the call's: a wait the call bounds ends
// at the horizon even when the lease wait is longer, because a wait past the deadline helps no
// one.
func (r *runner) capacityWaitFor(start time.Time, decided bool) capacityWait {
	// The wait a call with no deadline has, and the ceiling of every wait that is not call-bound: the larger of
	// agent_placement_wait_sec and agent_lease_wait_sec, as it always was.
	base := max(r.placementTTL(), time.Duration(r.cfg.AgentLeaseWaitSec)*time.Second)
	w := capacityWait{wait: base, deadline: start.Add(base)}
	if r.cfg.AgentPlacementWaitSec < 0 || decided {
		return w
	}
	horizon, ok := r.call.waitHorizon()
	if !ok {
		return w
	}
	bound := max(horizon.Sub(start), 0)
	// A wait holds one of the call's run slots (RunWith's sem), so while subtasks of the call have not started
	// - the ones waiting for a slot and the ones of later chunks - it may not run to the horizon: they would
	// start late, or never, and with less than the reserve left. Such a wait keeps the bound it had before
	// ADR 0073, unless the call's own end comes sooner. The last subtask to start waits for the horizon, since
	// nothing is behind it.
	if held := r.call.unstarted(); held > 0 && base <= bound {
		w.held = held
		return w
	}
	// The call is the bound, and a longer agent_lease_wait_sec does not extend it: a wait past the call's
	// deadline cannot help anyone, the call is over before it ends.
	left, _ := r.call.timeLeft()
	w.deadline, w.wait = horizon, bound
	w.call = &callWaitInfo{left: left, reserve: callWaitReserve, noTime: bound <= 0}
	return w
}

// coolingNote renders the nodes the last tick of a capacity wait did not ask because
// their own refusal put them on cooldown, or "" when none. Like a held-out node they
// are re-read every tick and asked when the cooldown ends; a Retry-After longer than
// the wait means that never happens inside it, and the defer says so.
func coolingNote(cooling map[string]string) string {
	if len(cooling) == 0 {
		return ""
	}
	bases := make([]string, 0, len(cooling))
	for base := range cooling {
		bases = append(bases, base)
	}
	sort.Strings(bases)
	parts := make([]string, 0, len(bases))
	for _, base := range bases {
		parts = append(parts, cooling[base])
	}
	return "; not asked again inside the wait (re-read every tick, asked when the cooldown ends): " + strings.Join(parts, "; ")
}

// capacityDefer is the TTL outcome: every node that could run the subtask was
// full for the whole wait. Deferred, class capacity — the fleet is healthy and
// the contract is sound; it was not this contract's turn — with every refusal
// and the wait named so the caller can re-run, widen the wait, or add a node.
//
// note is why the subtask waited when nothing refused it (PlacedResult.gated and
// .overflow: a process-gate turn-away, a deal's overflow): narration, never counted
// among the refusals.
func (r *runner) capacityDefer(local NodeView, seed PlacedResult, idle, wait time.Duration, refusals []string, ev waitEvidence) PlacedResult {
	chain := fmt.Sprintf("%d refusal(s)", len(refusals))
	if len(refusals) > 0 {
		chain += ": " + strings.Join(refusals, "; ")
	}
	if ev.note != "" {
		chain += "; waiting because: " + ev.note
	}
	slotNote := ""
	if ev.held > 0 && ev.call == nil {
		// The call had time left, and this wait ended anyway: it held a run slot that subtasks of the call were
		// waiting for (ADR 0073). Saying so keeps "no node had room within 2m0s" from reading as the call's own limit.
		slotNote = fmt.Sprintf("; %d subtask(s) of the call had not started, so this wait could not hold its run slot to the call's deadline", ev.held)
	}
	reason := fmt.Sprintf("capacity wait: no node had room within %s (waited %s; agent_placement_wait_sec=%d; %s%s%s%s%s) — re-run later, raise agent_placement_wait_sec, or add a node",
		wait, idle.Round(time.Second), r.cfg.AgentPlacementWaitSec, chain,
		heldOutNote(ev.heldOut, strings.Join(refusals, "; ")), coolingNote(ev.cooling), probeFailureNote(ev.probeFails), slotNote)
	if ev.call != nil {
		// The call's deadline bounded this wait (ADR 0073). The advice to raise agent_placement_wait_sec
		// would be wrong: that key never bounded it. What helps is a later call or a bigger fleet.
		reason = fmt.Sprintf("capacity wait: no node had room before the call's deadline (waited %s; %s; %s%s%s%s) — re-run later or add a node",
			idle.Round(time.Second), ev.call.words(), chain,
			heldOutNote(ev.heldOut, strings.Join(refusals, "; ")), coolingNote(ev.cooling), probeFailureNote(ev.probeFails))
	}
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, JobID: seed.JobID,
		PlacementReason: reason, waited: true, Unplaced: true, CapacityWaitSec: idle.Seconds(),
		Replacements: len(refusals),
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        reason,
		},
	}
}

// fencedDefer is the result for a contract whose only local seats a lease FENCES, with the wait
// switched off: deferred, class capacity (the seat's own pre-check files the same class, and the
// delegator re-places it), the fence and its holder named. It never runs the contract.
func (r *runner) fencedDefer(local NodeView, why string, waited time.Duration) PlacedResult {
	reason := why + fmt.Sprintf("; waited %s (agent_placement_wait_sec=%d) - raise agent_placement_wait_sec, add a remote, or wait for the lease to end", waited.Round(time.Second), r.cfg.AgentPlacementWaitSec)
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, PlacementReason: reason, Unplaced: true,
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        reason,
		},
	}
}

// waitBudgetDefer is the outcome of a wait that ran out of contract budget before
// any node was asked and none refused the subtask (a gate turn-away, a deal's
// overflow): nobody declined the work and nothing ran, so it is a BUDGET defer,
// as runDecided files it, and never a `placement refused` failure.
func (r *runner) waitBudgetDefer(local NodeView, seed PlacedResult, idle time.Duration, spent, note string) PlacedResult {
	reason := spent
	if note != "" {
		reason += " — " + note
	}
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, JobID: seed.JobID,
		PlacementReason: reason, waited: true, Unplaced: true, CapacityWaitSec: idle.Seconds(),
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassBudget,
			Reason:        reason,
		},
	}
}

// shedResult is the sheddable run's outcome when no node had an idle slot:
// deferred at once, class capacity, marked shed.
func (r *runner) shedResult(local NodeView, seed PlacedResult, refusals []string) PlacedResult {
	reason := fmt.Sprintf("shed (priority %d): no node had an idle slot for sheddable work (%d refusal(s): %s) — sheddable contracts take idle capacity only; re-run when the fleet is quieter or drop the priority flag",
		r.priority, len(refusals), strings.Join(refusals, "; "))
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat, JobID: seed.JobID,
		PlacementReason: reason, shed: true, Unplaced: true, Replacements: len(refusals),
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        reason,
		},
	}
}

// budgetSpent is the exhausted-message tail for a subtask that ran out of
// contract budget rather than out of nodes. One function because it is now
// produced from two places in the loop (before selection and after it), and two
// copies of a sentence drift.
func budgetSpent(remaining, budget, placements int) string {
	return fmt.Sprintf("%ds of the %ds timeout_sec budget was left after %d placement(s), under the %ds floor for another",
		remaining, budget, placements, minRetrySec)
}

// isReplaceable: this attempt ended because a node DECLINED the job at
// dispatch, and the answer it declined with is one another node may answer
// differently - or because the LOCAL seat's run-cap line held the contract for
// its wall and deferred it as capacity with nothing run (capacityDeferRefusal).
//
// The second half is the one comment after comment called re-placeable while
// nothing consumed it (a local capacity defer was terminal). It applies only when
// there is somewhere else to go: route=local waits in place - the seat's own line
// IS the wait, and its defer is the answer - and a delegator with no remotes has
// no other node to offer the work to.
func (r *runner) isReplaceable(pr PlacedResult) bool {
	if pr.refused && replaceableRefusal(pr.refusalStatus) {
		return true
	}
	return capacityDeferRefusal(pr) && r.route != "local" && len(r.remotes) > 0
}

// capacityDeferRefusal reports whether an attempt is the LOCAL leg's own capacity
// defer: it ran on this box's seat, the seat's admission (the run-cap line, the
// cordon, a lease) refused it as capacity, and no step ever ran. Nothing owned
// the contract, so another node may take it without arranging a double run.
//
// LOCAL only, on purpose. A capacity defer a REMOTE node files after acking the
// job is an observed terminal answer of a job that node held; "never re-place
// after a 202" (placeAndRun) keeps it exactly as it came. The one thing re-placed
// after a 202 is a job the node itself says it never ran (a confirmed withdrawal, or
// its own reaped / withdrawn / not-started record: ADR 0064), and that arrives as a
// refused 503, not as a defer.
func capacityDeferRefusal(pr PlacedResult) bool {
	return pr.ranLocal && pr.Err == "" && pr.Result.Deferred &&
		pr.Result.DeferClass == core.DeferClassCapacity && pr.Result.Steps == 0
}

// replaceableRefusal decides, from the status a node answered a DISPATCH with,
// whether offering the job to a DIFFERENT node is worth the wall clock.
//
// The line is WHO THE ANSWER IS ABOUT.
//
// About THIS NODE, RIGHT NOW → re-place. Another node answers these
// differently, and the whole point of a fleet is that it can:
//
//	0   the delegator never reached it at all (dial refused, dropped
//	    connection, per-request deadline) — a statement about one address.
//	404 nothing at this address serves /fleet/dispatch.
//	408 it ran out of time reading THIS request.
//	409 "job previously failed on this node" — the node's own message says
//	    on this node; it is a fact about that node's job store, and no other
//	    node holds that record.
//	429 too many requests, to it.
//	5xx it, or something in front of it, is failing: `503 queue full`,
//	    `503 node draining`, `503 vram snapshot stale`, a 500 from a proxy.
//	    An unknown 5xx is still an "it is broken" answer, so the default for
//	    that range is to re-place.
//
// About THE REQUEST → terminal. Every other 4xx: 400 (a malformed envelope, a
// body over the node's 1 MiB cap, an unsupported task_type, a contract the
// node's own Validate rejects), 401 (the fleet_auth_token — the delegator
// sends the SAME token to every node, so a fleet-wide credential mismatch is
// an operator fix, not a routing one), 403 (the agent lane requires a token
// this delegator does not have), 405, 413, 415, 422, and any future 4xx. The
// next node is handed byte-identical bytes and the same bearer, so it returns
// the same answer; re-placing only collects it N times and spends the
// contract's budget doing it. An unknown 4xx defaults to terminal for exactly
// that reason — the 4xx range means "your request", by definition.
//
// The rule keys on the STATUS, never on the text of an error string: a node's
// prose is not a protocol, and matching on it is how a reworded message
// silently changes routing.
//
// KNOWN BOUND, stated rather than hidden: a fleet whose nodes carry DIFFERENT
// bearer tokens gets no re-placement out of a 401/403. That is deliberate —
// docs/FLEET-NODE.md specifies one shared token for the whole fleet, and
// spraying a rejected credential across a roster is not something to build in
// on the chance the deployment disobeys it.
func replaceableRefusal(status int) bool {
	if status >= 400 && status < 500 {
		switch status {
		case http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return false
	}
	// status 0 (never reached), any 5xx, and the pathological non-202 2xx/3xx
	// (something at that address is not a fleet node) are all about the node.
	return true
}

// replacementNode picks where a refused subtask goes next: the best eligible
// remote not yet tried, and — reserved as the last resort — the local seat.
//
// The bound and the exclusion set both come from the SUBTASK's ledger, not
// from this call: pl.used is every re-placement the subtask has spent (across
// the first attempt AND the verification retry) and pl.tried is every dial base
// it has already been placed on. Local is deliberately not charged against the
// bound — it is the one seat always able to take the contract, so a wide roster
// must not be able to spend the bound before reaching it — but it IS in
// pl.tried, which is what keeps it to one use per subtask.
//
// refused is this chain's refusal count, used only for the human-readable
// reason; the bound is pl.used and nothing else.
//
// THE FLEET IS READ AGAIN, for every route (ADR 0063). route=spread used to reuse
// the snapshot taken when the run started, so a node the run's own siblings had
// filled since was still "the node with room". The read goes through the probe
// memo, but only a snapshot taken AFTER this refusal counts (fetchViewsSince): one
// that predates it is exactly the picture the refusal just proved wrong. In practice
// each re-placement reads the fleet itself - a sibling's probe is rarely newer than
// this caller's own refusal - which is the one extra health read per re-placement
// ADR 0063 names in its Consequences.
//
// A candidate must pass the room check (hasRoomWithin: not saturated, and able
// to START the job inside the caller's patience), must not be in cooldown after
// a refusal of its own, and must not be a node whose process gate is already
// full (processgate.go). A node that fails those is not "refused for good": with
// no candidate left the subtask goes to the capacity wait, which re-reads every
// one of them each tick.
//
// The local seat is the reserved last resort only when its run-cap line has a
// free slot ahead of a newcomer (localSlotAhead). A full seat used to take the
// subtask at once and hold it in a line it could not leave - 26 of the day's 52
// local re-placements died there.
//
// ok=false returns the sentence the exhausted message ends with. It is built
// from BOTH facts (the bound, and whether local was available) because a reader
// who is told only one of them will look in the wrong place.
func (r *runner) replacementNode(ctx context.Context, contract core.AgentContract, pl *placements, refused int) (placement, string, bool) {
	refusedAt := time.Now()
	boundHit := pl.used >= maxRemoteReplacements
	// busy names every eligible node the filters below held out only because it is
	// busy right now (withRoom); nothing was dispatched to any of them.
	var busy []string
	if r.route != "local" && !boundHit {
		st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
		// Re-probe: the roster this subtask was placed against is now known to
		// be at least partly wrong (a node just refused), and health is the
		// only thing that can say which of the others has room.
		views, bases, _ := r.fetchViewsSince(ctx, refusedAt)
		freshViews, freshBases := untried(views, bases, pl.tried)
		var candViews []NodeView
		var candBases []string
		var lifted map[string]string
		candViews, candBases, busy, lifted = r.withRoomAfter(st, freshViews, freshBases, refusedAt)
		if chosen := Place(mintP2CSeed(), st, r.localView(), candViews, true); !chosen.Local {
			base := baseFor(chosen, candViews, candBases)
			return placement{
				view:   chosen,
				base:   base,
				reason: fmt.Sprintf("re-placed on %s after %d refusal(s)", chosen.NodeID, refused) + liftedClause(lifted[base]),
			}, "", true
		}
	}
	// Local is the reserved last resort. route=remote never takes it: an
	// explicit remote route must not silently fall local, which is the same
	// posture the "no eligible remote" defer already holds.
	head := "no further eligible remote was available"
	if r.call.reached() {
		// The read above may be the very thing the call's deadline ended, and what a cut
		// read held is not evidence about the nodes it never reached (ADR 0065).
		head = "the call's deadline had passed, so the fleet read may not have named every eligible remote"
	}
	if len(busy) > 0 {
		// A node the filters held out is BUSY, not broken: INV-4 sends the subtask to
		// the capacity wait, which re-reads every one of them each tick - whatever
		// kind of refusal started this chain. Only a 503 or 429 used to say so, and a
		// 500 followed by a full node failed a subtask that a node about to free could
		// have taken.
		pl.capacityRefusal = true
		head = "no further remote could take it right now (" + strings.Join(busy, "; ") + ")"
	}
	if boundHit {
		head = fmt.Sprintf("the re-placement bound of %d further node(s) was reached", maxRemoteReplacements)
	}
	if r.route == "remote" {
		return placement{}, head + ", and route=remote never falls back to local", false
	}
	if pl.tried[""] {
		return placement{}, head + ", and the local seat had already been tried", false
	}
	// A contract that names a layer this box does not declare (register A-108) has no local seat to
	// fall back on: it could only defer there by name. Returning here sends a refusal that was a place
	// in line (pl.capacityRefusal) to the capacity wait, for the node that declares the layer.
	if !r.localServesLayer(r.localView(), contract) {
		return placement{}, head + ", and " + localRefusal(r.localView(), contract) + ", so the local seat cannot take it", false
	}
	// A text lease reserves the local seat for re-placement exactly as it does
	// for first placement (0.113.18; before this a remote's 503 fell straight
	// onto the reserved cards — the 2026-09-05 incident through a side door).
	// The capacity wait, not this fall-back, is what watches for the release.
	if info := r.localLease(contract); Reserved(info) {
		return placement{}, head + ", and the local seat is reserved (" + HolderLine(info) + ")", false
	}
	// A seat a lease FENCES is no last resort either (GPU routing P7): the run would be turned
	// away at the seat's own pre-check. The fence is a place in line, and the wait watches it.
	if info, why, fenced := r.fencedLocal(contract); fenced {
		pl.capacityRefusal = true
		return placement{}, head + ", and the local seat is fenced (" + why + " — " + HolderLine(info) + ")", false
	}
	// An occupied seat is no last resort either (0.165.2): loading it would unload another
	// loaded vLLM seat (probeLocalBusy's occupiedBy; the reference box's opencode seat). A remote
	// was eligible and refused, so this is a place in line: the capacity wait re-reads every node
	// each tick and takes the local seat only once the occupant has left.
	if r.route != "local" {
		if rd := r.busyReadingNow(ctx); rd.occupiedBy != "" {
			pl.capacityRefusal = true
			return placement{}, head + ", and " + rd.why(), false
		}
	}
	if free, note := r.localSlotAhead(); !free {
		// A full run-cap line is a place in line, not a refusal: the wait watches it and
		// takes the seat when a slot frees (INV-4), whatever kind of refusal started
		// this chain.
		pl.capacityRefusal = true
		return placement{}, head + ", and the local seat's run-cap line has no free slot (" + note + ")", false
	}
	return placement{
		view:   r.localView(),
		reason: fmt.Sprintf("re-placed on the local seat after %d refusal(s) — queued-local beats work nobody ran", refused),
	}, "", true
}

// withRoom filters a fleet snapshot down to the nodes that can take a NEW
// dispatch of st right now: eligible, room by their own advertisement, a start
// inside the caller's patience, no cooldown from a refusal of their own, and a
// process gate that is not already full. Views and bases stay index-parallel.
//
// busy has one line for every ELIGIBLE node that was held out only because it is
// busy right now - the arithmetic or the cooldown that says so - and nothing for a
// node that cannot run the contract at all (remoteEligible): the first kind frees
// up and is worth waiting for, the second never does.
func (r *runner) withRoom(st Subtask, views []NodeView, bases []string) (outV []NodeView, outB []string, busy []string) {
	outV, outB, busy, _ = r.withRoomAfter(st, views, bases, time.Time{})
	return outV, outB, busy
}

// withRoomAfter is withRoom for views read no earlier than readAfter (the zero time: a read of unknown
// age, which lifts nothing). Given one, a node on a Retry-After cooldown whose health in these views
// proves a free worker has its cooldown lifted (cooldowns.lift) and is judged like any other; lifted
// names, per base, why.
func (r *runner) withRoomAfter(st Subtask, views []NodeView, bases []string, readAfter time.Time) (outV []NodeView, outB []string, busy []string, lifted map[string]string) {
	outV = make([]NodeView, 0, len(views))
	outB = make([]string, 0, len(bases))
	now := time.Now()
	sheddable := r.priority < core.BandNormal
	for j, v := range views {
		if !remoteEligible(st, v) {
			continue
		}
		if until, cooling := r.cool.heldUntil(bases[j], now); cooling {
			note, liftedNow := r.cool.lift(bases[j], v, readAfter, now)
			if !liftedNow {
				busy = append(busy, fmt.Sprintf("%s: cooling down after its own refusal (%s left)", laneID(v), until.Sub(now).Round(time.Second)))
				continue
			}
			if lifted == nil {
				lifted = map[string]string{}
			}
			lifted[bases[j]] = note
		}
		if patience, clamp := r.patience(st.Contract, v); !hasRoomWithin(v, sheddable, patience) {
			busy = append(busy, laneID(v)+": "+whyNoRoom(v, sheddable, patience, clamp))
			continue
		}
		if !processGate.available(bases[j], admissionCeiling(v)) {
			busy = append(busy, fmt.Sprintf("%s: process gate (this process already holds %d open, at its admission ceiling of %d)", laneID(v), processGate.load(bases[j]), admissionCeiling(v)))
			continue
		}
		outV = append(outV, v)
		outB = append(outB, bases[j])
	}
	return outV, outB, busy, lifted
}

// liftedClause is the bracket a placement reason carries when the node it names was asked before
// its Retry-After cooldown ended, because its health proved a free worker.
func liftedClause(note string) string {
	if note == "" {
		return ""
	}
	return " [" + note + "]"
}

// fleetReadForWait is the capacity wait's per-tick read of the fleet. A node on a cooldown is judged
// only by a read taken after its latest refusal (cooldowns.lift), so the read must postdate the newest
// hold in force: the 2 s probe memo may serve a snapshot another waiter's probe took just before a
// refusal, and a picture older than the refusal is the one thing that proves nothing about it. With
// no hold in force it is the ordinary memoised read (the zero time). readAfter is the instant the read
// is guaranteed to postdate.
func (r *runner) fleetReadForWait(ctx context.Context) (views []NodeView, bases []string, probeErrs []string, failed map[string]string, readAfter time.Time) {
	readAfter = r.cool.newestHold(time.Now())
	views, bases, probeErrs, failed = r.fetchViewsDetailedSince(ctx, readAfter)
	return views, bases, probeErrs, failed, readAfter
}

// whyNoRoom says why hasRoomWithin refused v: no room by its own advertisement, or
// room but a backlog past the caller's patience (with the arithmetic). clamp is the
// note runner.patience returns when the call's remaining time, not the contract's own
// poll budget, set that patience; it rides only the backlog verdict, since a node with
// no room is refused whatever the patience.
func whyNoRoom(v NodeView, sheddable bool, patience time.Duration, clamp string) string {
	if !hasRoom(v, sheddable) {
		switch {
		case v.SaturationKnown && v.SaturationHigh:
			return "no room (the node reports itself saturated)"
		case v.MaxQueueDepth > 0 && v.QueueDepth >= v.MaxQueueDepth:
			return fmt.Sprintf("no room (queue_depth %d of max_queue_depth %d)", v.QueueDepth, v.MaxQueueDepth)
		case sheddable:
			return "no room (no idle execution slot, and sheddable work takes idle capacity only)"
		}
		return "no room (not accepting new work)"
	}
	_, why := startsWithinPatience(v, patience)
	return "backlog (" + why + clamp + ")"
}

// localSlotAhead reports whether the local seat's run-cap line has a free slot
// ahead of a NEWCOMER - the question the C-60 FIFO (modelaffinity.AwaitSeatSlot)
// answers for every run that reaches the seat. A newcomer's own record is not in
// the registry yet, so every registered run on the seat is ahead of it (waiters
// included: the FIFO counts a waiter that arrived first), and a slot is free while
// fewer than fleet_max_concurrent_jobs are. An unreadable registry, an unset seat
// and an unlimited cap all read as a free slot: fail toward the behaviour every
// earlier version had, exactly as the busy probe does.
//
// Memoised for fetchViewsMemoTTL like the fleet probe, so a dozen subtasks waiting
// on the capacity wait read the registry once per tick between them.
func (r *runner) localSlotAhead() (free bool, note string) {
	if fetchViewsMemoTTL > 0 {
		r.localSlotMu.Lock()
		if !r.localSlotAt.IsZero() && time.Since(r.localSlotAt) < fetchViewsMemoTTL {
			free, note = r.localSlotFree, r.localSlotNote
			r.localSlotMu.Unlock()
			return free, note
		}
		r.localSlotMu.Unlock()
	}
	free, note = r.readLocalSlot()
	if fetchViewsMemoTTL > 0 {
		r.localSlotMu.Lock()
		r.localSlotAt, r.localSlotFree, r.localSlotNote = time.Now(), free, note
		r.localSlotMu.Unlock()
	}
	return free, note
}

// localLease is the machine-wide lease as it stands between THIS contract and a local seat
// (LeaseForContract): the leases on the cards of the seats the contract could run on, and
// nothing else. Every read of the local lease that asks "is the local seat reserved, fenced
// or busy for this contract" goes through it, so a lease on one card is not a reason to
// route, wait or defer work a free card can take.
func (r *runner) localLease(c core.AgentContract) gpulease.Info {
	return LeaseForContract(r.cfg, LocalLease(r.cfg.GPULockPath, r.cfg.StateDir), c)
}

// fencedLocal reports whether a lease this process does not hold FENCES every local seat c
// could run on, so that a local run would be turned away at the seat's own pre-check
// (pipeline/agenttask.go, S-26) after the delegator had spent an attempt, a ledger row and an
// intent record on it: the wasted local leg (GPU routing P7, register C-86). It is the
// pre-check's own question (ForeignFence, which exempts the holder's own child) asked of the
// contract's chain of seats (placement.LeasesAgainstContract), so the two cannot disagree: a
// lease that fences the seat of a card-2 render does not fence a contract whose card-0 seat is
// free.
//
// It answers true only when there is somewhere else to wait for: a delegator with no remotes
// has no other node to spare the leg for, and route=local is the caller's explicit choice and
// is never gated. In those cases the local run's own pre-check is the fast, honest answer.
func (r *runner) fencedLocal(c core.AgentContract) (info gpulease.Info, why string, fenced bool) {
	if r.route == "local" || len(r.remotes) == 0 {
		return gpulease.Info{}, "", false
	}
	return fencedLocalIn(r.cfg, LocalLease(r.cfg.GPULockPath, r.cfg.StateDir), c)
}

// fencedLocalIn is fencedLocal over a lease reading the caller already holds (the spread run's
// ONE snapshot), without the route and roster conditions.
func fencedLocalIn(cfg config.Config, info gpulease.Info, c core.AgentContract) (gpulease.Info, string, bool) {
	held := placetable.LeasesAgainstContract(cfg, info, c, func(l gpulease.Info) bool {
		fenced, _ := ForeignFence(l)
		return fenced
	})
	if !held.Held {
		return gpulease.Info{}, "", false
	}
	_, why := ForeignFence(held)
	return held, why, true
}

// fencedLocalSpread is fencedLocal over the spread run's ONE lease snapshot.
func (r *runner) fencedLocalSpread(c core.AgentContract) (gpulease.Info, string, bool) {
	if r.route == "local" || len(r.remotes) == 0 {
		return gpulease.Info{}, "", false
	}
	return fencedLocalIn(r.cfg, r.spreadLease, c)
}

// spreadLeaseFor narrows the spread run's ONE lease read (r.spreadLease: every subtask deals
// against one snapshot) to a contract's own local seats.
func (r *runner) spreadLeaseFor(c core.AgentContract) gpulease.Info {
	return LeaseForContract(r.cfg, r.spreadLease, c)
}

// anyLeaseHeld is the deal's busy reading of the lease: true when ANY subtask has no local
// seat free of every lease. One contract that cannot run locally is enough to consider the
// fleet; a free seat for the others is not lost, because each subtask's own placement reads
// its seats again.
func (r *runner) anyLeaseHeld(info gpulease.Info, subtasks []core.AgentContract) bool {
	if !info.Held {
		return false
	}
	for _, c := range subtasks {
		if LeaseForContract(r.cfg, info, c).Held {
			return true
		}
	}
	return len(subtasks) == 0 // nothing to narrow by: the whole-node reading
}

// localStillBusy is the deal's own reading of the local seat, taken again: the
// capacity wait of a subtask a DEAL kept off the seat because it read busy
// (PlacedResult.overflow) may take the seat only once the deal would have. route=spread
// reads busy on any request in flight (agent_spread_local_slot: always never reads
// busy); route=auto on a held lease, an in-flight count at the fleet's own cap, a
// load in progress (W-01), or another vLLM seat a load would evict (occupiedBy). An UNREADABLE seat keeps the deal's busy answer: the deal
// fails open because it has no evidence, but here it read the seat busy, and a probe
// that fails afterwards (a box too loaded to answer in time) is no evidence of idle.
// Failing open here ran the overflow on the busy seat (register C-88, CI run
// 36937589311); a seat that never reads again leaves the subtask to the remotes or
// to the wait's own end.
func (r *runner) localStillBusy(ctx context.Context, lease gpulease.Info) (busy bool, note string) {
	rd := r.busyReadingNow(ctx)
	if r.route == "spread" {
		return (rd.busy || rd.unknown) && r.cfg.SpreadLocalSlot() != config.SpreadLocalAlways, rd.note
	}
	return lease.Held || r.atRunCap(rd.inflight) || rd.loading || rd.occupiedBy != "" || rd.unknown, rd.note
}

// atRunCap reports whether `inflight` requests on the local seat have reached the fleet's own
// concurrency cap (fleet_max_concurrent_jobs): the cap above which route=auto reads the seat as busy.
// An UNLIMITED cap (a negative setting, which FleetConcurrencyLimit resolves to 0) is never reached.
// The comparison used to be written out at four sites and guarded at one: `inflight >= 0` is always
// true, so with fleet_max_concurrent_jobs < 0 an IDLE local seat read as permanently busy and the work
// left the box although the setting says unlimited (the diagnosis' F11). One helper, so the four
// cannot drift again.
func (r *runner) atRunCap(inflight int) bool {
	limit := r.cfg.FleetConcurrencyLimit()
	return limit > 0 && inflight >= limit
}

// readAutoLocalSlot is route=auto's one joint reading of the local seat for the whole run (W-01): it
// records the reading (autoDealBusy) and whether the seat's LOAD read busy (autoDealReadBusy), logs the
// one line per run, and returns the deal's busy flag, which also counts a lease. It is a method, and not
// a block in RunWith, so the reading can be pinned without a live run (unlimited_cap_test.go).
func (r *runner) readAutoLocalSlot(ctx context.Context, leaseInfo gpulease.Info, subtasks []core.AgentContract) (busy bool) {
	localBusy := r.probeLocalBusy(ctx)
	// occupiedBy: another vLLM seat holds the cards and loading this one would evict it.
	busy = r.anyLeaseHeld(leaseInfo, subtasks) || r.atRunCap(localBusy.inflight) || localBusy.loading || localBusy.occupiedBy != ""
	r.autoDealBusy = localBusy
	// The seat's LOAD only: a lease is the wait's own business (Reserved, fencedLocal),
	// and a lease this process holds must not keep its own subtasks off the seat.
	r.autoDealReadBusy.Store(r.atRunCap(localBusy.inflight) || localBusy.loading || localBusy.occupiedBy != "")
	// One line per run, mirroring route=spread's own local-slot log
	// (review round 1 item 4): before this the identical W-01 read had
	// no trace at all, so an operator could not tell "busy" from
	// "idle" without re-deriving it from the placement_reason.
	log.Printf("delegate: auto local slot: busy=%v inflight=%d loading=%v occupied_by=%q (%s)", busy, localBusy.inflight, localBusy.loading, localBusy.occupiedBy, localBusy.note)
	return busy
}

// busyReadingNow is probeLocalBusy memoised for fetchViewsMemoTTL, so a dozen
// waiting subtasks read the engine once per tick between them. A non-positive TTL
// switches the memo off, as it does for the fleet probe.
func (r *runner) busyReadingNow(ctx context.Context) busyReading {
	if fetchViewsMemoTTL > 0 {
		r.localBusyMu.Lock()
		if !r.localBusyAt.IsZero() && time.Since(r.localBusyAt) < fetchViewsMemoTTL {
			rd := r.localBusyRd
			r.localBusyMu.Unlock()
			return rd
		}
		r.localBusyMu.Unlock()
	}
	rd := r.probeLocalBusy(ctx)
	if fetchViewsMemoTTL > 0 {
		r.localBusyMu.Lock()
		r.localBusyAt, r.localBusyRd = time.Now(), rd
		r.localBusyMu.Unlock()
	}
	return rd
}

// readLocalSlot is localSlotAhead's reading, unmemoised: a slot is free while the seat's
// line has room (localRunCapRoom).
func (r *runner) readLocalSlot() (bool, string) {
	room, note := r.localRunCapRoom()
	return room > 0, note
}

// localRunCapRoom is how many MORE runs the local seat's run-cap line takes: the cap
// (fleet_max_concurrent_jobs, default 4) minus the runs registered on the seat, floored
// at 0, or unlimitedHeadroom when no cap is configured. Its fail-open is the real
// gate's: pipeline/agenttask.go skips the wait in line when the registry will not open,
// so an unreadable registry counts nothing ahead of a newcomer - but the reading says
// so in its note, and the process logs it once, because a wrong "free" is what silently
// re-creates the livelock this count exists to prevent.
//
// It reads the planner seat, the seat every run on a plain box takes (agenttask.go
// resolves it first, and only a composite decision moves a run to a layer seat).
func (r *runner) localRunCapRoom() (room int, note string) {
	limit := r.cfg.FleetConcurrencyLimit()
	if limit <= 0 {
		return unlimitedHeadroom, ""
	}
	seat := strings.TrimSpace(r.cfg.AgentPlannerModel(""))
	if seat == "" {
		return limit, "no agent seat configured, so no run could be counted"
	}
	reg, err := gpuactivity.Open(r.cfg.GPULockPath, r.cfg.StateDir)
	if err != nil {
		r.localSlotWarn.Do(func() {
			log.Printf("delegate: the local run registry could not be opened (%v); the local seat's run-cap line is read as empty", err)
		})
		return limit, fmt.Sprintf("run registry unreadable (%v), read as empty", err)
	}
	// A seat pinned to ONE card shares it with every other seat on it, so its line is the
	// card's (plan P5): the same reading the node's own gate makes (OnSeatPinned). A seat that
	// spans cards, or whose pin is unknown, keeps the per-seat line.
	pins, _ := r.cfg.ModelPins(seat)
	runs := reg.OnSeatPinned(time.Now(), pins, seat)
	ahead := len(runs)
	where := "on seat " + seat
	if len(pins) == 1 {
		where = "on card " + pins[0] + " (seat " + seat + ")"
	}
	return max(limit-ahead, 0), fmt.Sprintf("%d run(s) registered %s, cap %d", ahead, where, limit)
}

// untried filters a fleet snapshot down to the nodes this subtask has not
// already been refused by, keeping views and bases index-parallel.
//
// It excludes on the DIAL BASE, not the node id: a node that answered health
// without a node_id would otherwise share one empty key with every other such
// node, and the dial target is the thing actually being re-tried.
func untried(views []NodeView, bases []string, tried map[string]bool) ([]NodeView, []string) {
	outV := make([]NodeView, 0, len(views))
	outB := make([]string, 0, len(bases))
	for j := range views {
		if tried[bases[j]] {
			continue
		}
		outV = append(outV, views[j])
		outB = append(outB, bases[j])
	}
	return outV, outB
}

// refusalLine renders ONE node's refusal for the operator-facing list. The node
// is named from what it advertised; a node that published no node_id gets a
// shape that reads as MISSING rather than a fabricated name.
func refusalLine(pr PlacedResult) string {
	name := pr.Node
	if name == "" {
		name = "(a remote that reported no node_id)"
	}
	if capacityDeferRefusal(pr) {
		// A defer, not an error: the local seat answered, and what it said is
		// its own reason.
		return name + " (local seat): deferred (capacity): " + pr.Result.Reason
	}
	return name + ": " + pr.Err
}

// replacementNote is the annotation carried on every re-placed result — the one
// that finally ran as much as the one nobody took. landed separates the two,
// because one wording cannot describe both without lying about one of them:
// "re-placed after 2 refusals" on a subtask nobody ran claims a placement that
// never happened.
func replacementNote(refusals []string, landed bool) string {
	if landed {
		return fmt.Sprintf("re-placed after %d refusal(s) — %s", len(refusals), strings.Join(refusals, "; "))
	}
	return fmt.Sprintf("%d refusal(s) and no node took it — %s", len(refusals), strings.Join(refusals, "; "))
}

// exhausted turns "nobody took it" into the one honest outcome: a FAILURE
// naming every node that refused and what it said, plus why the delegator
// stopped asking. last is the final refused attempt, whose telemetry row was
// already written by finish(); only what is PUBLISHED is rewritten here (the
// same posture mergeAttempts takes with its retry annotations).
//
// Node and Seat are CLEARED. `last` carries the id of the node that refused
// LAST, and publishing it as the result's node attributes the subtask to a box
// that explicitly declined it — the one attribution nobody in this path earned.
// No node ran this subtask, so it names none; every node that was asked, and
// exactly what each said, is in Err verbatim.
//
// The note is omitted when nothing was actually RE-placed (a single refusal
// with nowhere to go). Replacements is 0 there and correctly so, and shipping a
// `replacement_note` beside `replacements: 0` — with summary.replaced not
// counting it either — read as a contradiction on the wire. Err already carries
// the same refusal list, so nothing is lost by the omission.
func exhausted(last PlacedResult, refusals []string, why string) PlacedResult {
	if capacityDeferRefusal(last) {
		// The last thing refused was the local seat's own capacity DEFER, and
		// nothing else had room either. That is still a defer - the fleet is
		// healthy, it was not this contract's turn - and publishing it as a
		// "placement refused" FAILURE would tell the caller no node was willing
		// when every node was merely busy. Node and Seat stay: the local seat did
		// answer, and its own reason is the outcome.
		last.Replacements = len(refusals) - 1
		last.ReplacementNote = ""
		if last.Replacements > 0 {
			last.ReplacementNote = replacementNote(refusals, false)
		}
		last.Unplaced = true // nothing ran it: never counted as a recovered re-placement
		last.Result.Reason += "; no other node could take it (" + why + ")"
		return last
	}
	last.Node, last.Seat = "", ""
	last.Replacements = len(refusals) - 1
	last.ReplacementNote = ""
	if last.Replacements > 0 {
		last.ReplacementNote = replacementNote(refusals, false)
	}
	last.Err = fmt.Sprintf("%s: %d node(s) refused this subtask and none of them ran it (%s); %s",
		replacementExhaustedPrefix, len(refusals), strings.Join(refusals, "; "), why)
	return last
}

// exhaustedSettled is exhausted() with the call deadline's answer applied (ADR 0065 decision
// 2). Every outcome is cut at the moment it is PRODUCED, and a refusal chain is produced here,
// outside finish and settle. One closed after the call's deadline has passed is not evidence
// about the nodes: the fleet read that would have found one may be the very thing the deadline
// ended, so "no further eligible remote was available" would accuse nodes that were never
// asked, and a failure flags a one-subtask call as an error when it only ran out of time.
//
// Until the deadline has passed it is exactly exhausted(): the last refused attempt's row
// stands as the record. After it the outcome is the call-deadline defer with the chain quoted
// behind the marker, recorded here under a job id of its own for the reason settle gives one:
// the id of the last refused attempt belongs to its row, which says "dispatch ... 404" for a
// job no node held, and a second row under it would double-count one id.
//
// Nothing ran it, so it is cut as an outcome that was never placed. What the refused attempt
// left on the result says where THAT attempt was (the local seat's capacity defer, a job a
// node acked and then took back, a lease's sentinel), not where this subtask is, and must not
// steer the cut's wording ("was still running on the local seat") or its node and seat; the
// attempt's own intent was closed by its finish. since is when the span the closing row
// measures began.
func (r *runner) exhaustedSettled(contract core.AgentContract, last PlacedResult, refusals []string, why string, since time.Time) PlacedResult {
	pr := exhausted(last, refusals, why)
	if !r.call.reached() {
		return pr
	}
	pr.ranLocal, pr.intentRecorded, pr.orphanable, pr.waitCapacity = false, false, false, false
	pr.withdrawn, pr.nodeNeverRan, pr.nodeTerminal = false, "", false
	pr = r.cutOutcome(pr, true)
	pr.JobID = mintJobID()
	pr.wallMs = time.Since(since).Milliseconds()
	r.record(contract, pr)
	return pr
}

// minRetrySec is the least timeout_sec budget a retry is worth starting with: a
// cold seat needs seconds to load and a contract that cannot finish in this
// would only add a budget defer on top of the verified failure.
const minRetrySec = 10

// retryable: the node answered and the ANSWER was the problem — a verified
// wrong result, or a seat honestly abstaining. A transport failure, a budget
// defer, or a broken/misconfigured stack is not something another seat fixes,
// and a contract-classed defer is the caller's to fix.
//
// The TWO infrastructure defers that ARE retryable at admission are the
// admission-time ones: the coherence defer (register D-118) and the warm-up defer
// (register C-76, R-05a). The seat itself is broken (it answers nonsense, or its
// process did not start) and it was caught before the contract's wall started, so
// the budget is still there to fund a retry — which is precisely the case another
// node fixes. What the node's admission spent getting there is credited back in
// runOne (admissionCredit), because the cold load that leads to either verdict
// would otherwise eat most of a default budget before the retry floor is
// applied. The general infrastructure rule is untouched; see IncoherentSeatDefer
// and SeatWarmFailedDefer.
//
// The third is the seat-down defer (ADR 0066, SeatDownDefer): the seat's engine
// went down under the run and did not come back inside the node's own bounded
// wait. Same argument — a property of THIS seat, a sound contract, the cure is
// another node — and the wait the node spent on the dead seat is credited back
// the same way, with the wall of a delegator-side rescue that failed first (a seat
// lost in the structured re-pack is rescued before it is re-placed).
func retryable(pr PlacedResult) bool {
	if pr.Err != "" {
		return false
	}
	if len(pr.AcceptanceFailures) > 0 {
		return true
	}
	if admissionDefer(pr.Result) || SeatDownDefer(pr.Result) {
		return true
	}
	return pr.Result.Deferred && pr.Result.DeferClass == core.DeferClassAbstention
}

// SeatDownDefer reports whether a result is the seat-down defer (ADR 0066,
// register C-72): an `infrastructure` defer whose reason carries
// core.SeatDownReason, i.e. the executing node's seat went down under the run —
// the engine hung with work outstanding, or died and llama-swap no longer serves
// it — and the node's own bounded wait and one re-issue did not bring it back.
//
// It is the THIRD infrastructure defer that is worth a retry, after the two
// admission-time ones (IncoherentSeatDefer, SeatWarmFailedDefer), and for the same
// reason: the fault is a property of THIS seat and the contract itself is sound,
// so the same contract on another node is the cure. It is safe to re-place
// because a node-filed defer is an OBSERVED terminal — the node reported it, so
// nothing is still running the contract (the "never re-place after a 202" rule is
// about jobs whose outcome nobody observed). It matches on the CONSTANT the
// producer writes, never on prose. A node without ADR 0066 never emits it, and
// keeps emitting `stalled:` for the same outage until it is upgraded — for such a
// node this changes nothing.
func SeatDownDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassInfrastructure &&
		strings.HasPrefix(r.Reason, core.SeatDownReason)
}

// skipsRetryAsResearchAcceptanceOnly reports a placed result the verification
// retry must leave alone: a research page whose only problem is its own checks
// (a shape, an item count). Those are graded against the fetched page, so a
// second node given the same page mostly repeats the verdict; before the anchor
// fix of 0.141.1, 209 of a week's 271 such failures were the checks' own faults,
// and each retry was a whole second run of five to thirteen minutes.
//
// A failed DOCUMENT FINGERPRINT is the exception: the answer is about another
// document, which is a fact about the node that wrote it (strikeOnFingerprint
// quarantines a node for it), and another node is the cure. An abstention, an
// admission-time defer (coherence, warm-up) and the seat-down defer are likewise
// facts about the SEAT and keep their retry.
func skipsRetryAsResearchAcceptanceOnly(contract core.AgentContract, pr PlacedResult) bool {
	return researchDoors[contract.Door] && pr.Err == "" && !pr.Result.Deferred &&
		len(pr.AcceptanceFailures) > 0 && !failsDocumentFingerprint(pr.AcceptanceFailures)
}

// failsDocumentFingerprint reports whether any failed acceptance is the DOCUMENT
// FINGERPRINT (research.AnchorCheck tags its regex with the named group
// `docanchor`).
func failsDocumentFingerprint(failures []string) bool {
	for _, f := range failures {
		if strings.Contains(f, "(?P<docanchor>") {
			return true
		}
	}
	return false
}

// IncoherentSeatDefer reports whether a result is the admission-time coherence
// defer: an `infrastructure` defer whose reason carries core.IncoherentSeatReason,
// i.e. the executing node asked its freshly loaded seat one bounded question
// and the seat answered with the NaN shape (a run of one repeated byte, an
// unparsable tool call, or nothing at all at the cap with no hidden reasoning
// reported), or the same verdict remembered about a seat it already caught.
//
// It matches on the CONSTANT the producer writes, never on prose: pipeline
// (and the MCP door) build the reason from core.IncoherentSeatReason, so the
// two sides cannot drift. A pre-D-118 node never emits it and is unaffected.
//
// Why it is worth a retry when no other infrastructure defer is: the contract
// spent seconds, not its wall, and the fault is a property of THIS seat — the
// same contract on another node is the cure, and re-placing it is the whole
// reason the probe fires before the wall instead of after it.
func IncoherentSeatDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassInfrastructure &&
		strings.HasPrefix(r.Reason, core.IncoherentSeatReason)
}

// SeatWarmFailedDefer reports whether a result is the admission-time warm-up
// defer: an `infrastructure` defer whose reason carries core.SeatWarmFailedReason,
// i.e. the executing node's warm request was refused with a server error and there
// was positive evidence the seat's process did not start (a busy answer, a seat
// that reads starting or another model mid-swap never produce it). Like the
// coherence defer it is a property of THIS seat, was caught before the wall
// started, and is cured by the same contract on another node.
func SeatWarmFailedDefer(r core.AgentWireResult) bool {
	return r.Deferred && r.DeferClass == core.DeferClassInfrastructure &&
		strings.HasPrefix(r.Reason, core.SeatWarmFailedReason)
}

// admissionDefer is either of the two defers the node files before its wall
// starts, on evidence about its own seat, and that another node may cure.
func admissionDefer(r core.AgentWireResult) bool {
	return IncoherentSeatDefer(r) || SeatWarmFailedDefer(r)
}

// admissionCredit is the node-side admission time an admission defer already
// spent — its cordon wait, pre-flight, cold load and the probe or warm-up itself
// — which the subtask's execution budget must not be charged for: the contract's
// wall never started, and the wire carries admission_wait_sec for exactly this.
// Zero for every other result: no other shape has a claim on the credit, and a
// node that reports no admission (a pre-D-118 node, or an unmeasured one) is
// credited nothing rather than guessed at.
//
// A seat-down defer (ADR 0066) is credited its admission, the wait the node
// spent on the downed seat (seat_down_wait_sec) AND the wall a failed
// delegator-side rescue spent (rescueSpent): time the contract provably did not
// spend working, on a seat that could not serve it or on this box's attempt to
// save a finished answer. The last term is the one a seat lost in the structured
// re-pack depends on: the loop's recovery does not cover the re-pack, so that
// defer reports no wait of its own, and the rescue that runs first on this box's
// clock may cold-load its own seat and run a completion to its allowance —
// minutes that, charged to the retry budget, made the retry floor refuse the
// re-placement the defer is promised. The credit is bounded by one full contract
// wall (core.AgentTimeoutSecCap): neither a node's number nor a slow rescue buys
// the retry more than that.
func admissionCredit(pr PlacedResult) time.Duration {
	secs := 0.0
	switch {
	case admissionDefer(pr.Result):
		secs = pr.Result.AdmissionWaitSec
	case SeatDownDefer(pr.Result):
		secs = pr.Result.AdmissionWaitSec + pr.Result.SeatDownWaitSec + pr.rescueSpent.Seconds()
		if secs > core.AgentTimeoutSecCap {
			secs = core.AgentTimeoutSecCap
		}
	}
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// retryFenceWhy is why a retry is not placed on a fenced local seat: dialling it would hold the
// retry at the model-affinity cordon for agent_lease_wait_sec, and it would end as a capacity
// defer anyway (D-94). It is a fact about the seat, read from the lease record, so it stays true
// when the call's deadline ended the read of the fleet.
const retryFenceWhy = "a retry placed there would wait out agent_lease_wait_sec at the affinity cordon and defer as capacity anyway"

// localReservedPrefix opens the clause alternativeNode returns for a local seat it left out of a
// retry because a text lease RESERVES it (register C-81), as "the local seat is fenced (...)" does
// for a fence. retryHeldWhy tells the two apart by this opening: the one coupling between the clause
// and the closing reason of the note, and a string match on purpose, because alternativeNode returns
// the clause as text and a typed return would reach past its helpers. The opening is edited here,
// where reservedClause and retryHeldWhy both read it, and TestRetryHeldWhyTellsAReservationFromAFence
// fails with this coupling named when a clause that reservedClause writes stops being told as one.
const localReservedPrefix = "the local seat is reserved ("

// reservedClause names a reservation for a retry in the sentence replacementNode ends a refused
// re-placement with ("..., and the local seat is reserved (...)"), which replacementNode spells out
// itself; TestAReservedSeatReadsTheSameInARefusedReplacementAndASkippedRetry keeps the two copies
// identical. The first placement's own reasons ("local seat reserved (...); no eligible remote - ...")
// word a reservation differently and are not this clause.
func reservedClause(info gpulease.Info) string {
	return localReservedPrefix + HolderLine(info) + ")"
}

// retryReservedWhy is why a retry is not placed on a reserved local seat. It is not the fence's
// reason: the affinity gate admits the load of a plain reservation, so a dial would not wait at the
// cordon, it would run, on the cards the holder reserved. It says that and nothing wider: the note
// reaches callers, and "a reservation keeps every placement off them" is false for route=local (the
// caller's explicit choice, never gated) and for the holder's own child (the inherited lease exempts
// it), so the words stay about this retry.
const retryReservedWhy = "a retry placed there would run on the cards the holder reserved"

// retryHeldWhy closes the note of a retry that was not placed because a lease holds the local seat:
// the reservation's reason for the clause reservedClause writes, and the fence's for any other.
func retryHeldWhy(clause string) string {
	if strings.HasPrefix(clause, localReservedPrefix) {
		return retryReservedWhy
	}
	return retryFenceWhy
}

// alternativeNode picks the node a retry runs on: the best eligible remote
// when the first attempt ran locally (probing the fleet now if this run has
// not yet), the local seat when it ran remotely, unless a lease keeps the retry off the
// seat, a fence (D-94) or a text reservation (C-81), when the best untried remote takes
// it instead. ok=false when no different node can take the contract; the string is then
// the clause naming the lease ("the local seat is fenced (...)" or "the local seat is
// reserved (...)") when the local seat was left out for it, "" otherwise. runOne words
// the note around it (retryHeldWhy).
//
// Note the asymmetry and its cost: recovering a wrong REMOTE answer puts the
// work back on the local box the harness exists to keep free — so a placement
// the fit score reads too cheap is paid for in local GPU time, not just in
// latency. That is accepted deliberately (local is the one seat always able to
// take the contract, and a second remote hop would need a fresh gate pass
// mid-timeout), but it is a real cost, not a free retry.
//
// The one exception is a REASONED remote pin (remotePinned, ADR 0078 decision 3): the caller
// said why this call runs on a fleet node, and the ledger rows and offload_status say placement
// obeyed it, so its second chance is another fleet node or none, never the local seat. The bare
// remote route of a caller with no reason channel keeps the asymmetry above.
//
// It consults the SUBTASK's placement ledger, which is what makes "a DIFFERENT
// node" true rather than merely intended: a seat the first attempt already RAN
// on — including one it reached by re-placement, and including the local seat — is
// excluded here. Without that, a first attempt that ended up local after two
// remotes refused could be "retried" on local again. A seat that only DECLINED the
// subtask (the local seat's capacity defer, ADR 0063 decision 3) took nothing and is
// not excluded: see the local branch below.
func (r *runner) alternativeNode(ctx context.Context, first PlacedResult, contract core.AgentContract, pl *placements) (placement, string, bool) {
	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	localView := r.localView()
	why := attemptOutcome(first)
	if !first.ranLocal {
		// A reasoned remote pin never retries on the local seat (ADR 0078 decision 3). Checked before the layer, fence and
		// reservation questions, which are all about a seat this call may not use: the retry is the best untried remote, and
		// with none there is no retry (runOne words the note). Keyed on the reasoned pin and not on r.route == "remote": the
		// bare remote route keeps remote -> local (retry_reserved_test.go, TestRunRetryRemoteFailureFallsBackToLocal).
		if r.remotePinned() {
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why + " — " + r.remotePinWhy()}, "", true
			}
			return placement{}, "", false
		}
		// A contract that names a layer this box does not declare (register A-108) has no local retry:
		// the seat would only defer it by name. A different node that declares the layer is the retry,
		// and with none there is no retry.
		if !r.localServesLayer(localView, contract) {
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				because := " — the local seat declares no layer " + contract.Layer
				if seatless(localView) {
					because = " — " + noLocalSeat
				}
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why + because}, "", true
			}
			return placement{}, "", false
		}
		// D-94: the local seat is only a retry target if its lease would ADMIT
		// the run. Read fresh — the deal's snapshot can be minutes old by the
		// time a first attempt has failed somewhere else — and read the VERDICT
		// rather than dialling: a fenced seat answers a dial by holding the
		// request at the affinity cordon for agent_lease_wait_sec and then
		// deferring as capacity, which is five minutes of a retry's budget spent
		// discovering something the lease record already said.
		lease := r.localLease(contract)
		if fenced, fence := Fenced(lease); fenced {
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why +
						" — the local seat is fenced: " + fence}, "", true
			}
			return placement{}, fmt.Sprintf("the local seat is fenced (%s — %s)", fence, HolderLine(lease)), false
		}
		// C-81: a plain text reservation does not FENCE (the affinity gate admits the load, which is
		// why Fenced above leaves it alone), but it RESERVES the cards. A first placement on route auto
		// or spread never takes the local seat under one (auto waits for the holder, spread deals
		// without the seat) and neither does replacementNode's last resort, and the forced local
		// placement below is never read against the lease again, since attempt() runs a forced
		// placement as given. So without this check the retry of an attempt that ran on a fleet node,
		// a verification retry or a seat-down re-issue alike, was dialled onto cards a measurement had
		// reserved. The reading is replacementNode's: Reserved, which exempts the holder's own child.
		// An untried remote is the retry's place; with none the retry is not placed, as for a fence,
		// and the note names the holder (replacementNode's refusal for a reserved seat is the same
		// sentence, and its caller then waits in the capacity wait, which a retry does not have).
		if Reserved(lease) {
			held := reservedClause(lease)
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why +
						" — " + held}, "", true
			}
			return placement{}, held, false
		}
		// A seat-down defer is not held for a busy retry seat (runOne, ADR 0066 decision
		// 3): the node's own queue is the line, and its 503 is re-placed at once. That is
		// true of a REMOTE alternative and false of the local seat, which answers no 503:
		// a run forced onto it joins the run-cap line and waits there for the run's whole
		// wall before it defers as capacity, a line the subtask could not leave (the
		// state replacementNode declines to re-place into, ADR 0063 decision 2). So while
		// that line has no free slot ahead of a newcomer, an untried remote is the better
		// place for the retry. With none, the local seat is the only place there is, and
		// joining its line beats losing the job.
		if SeatDownDefer(first.Result) {
			if free, note := r.localSlotAhead(); !free {
				if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
					return placement{view: chosen, base: base,
						reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why +
							" — the local seat's run-cap line has no free slot (" + note + ")"}, "", true
				}
			}
		}
		// No pl.tried[""] check here, on purpose. It used to be stated as a proof:
		// a LOCAL placement is always terminal for its chain (only runRemote sets
		// `refused`), so pl.tried[""] implies first.ranLocal and this branch cannot
		// run with local already used. The proof stopped holding with ADR 0063
		// decision 3: the local seat's own capacity defer is re-placeable
		// (isReplaceable, capacityDeferRefusal), so placeAndRun records the seat as
		// tried, re-places the subtask on a remote, and that remote's failed answer
		// is the first attempt this branch sees. The local seat is then the
		// DIFFERENT node the retry asks for: it declined the job and never ran it,
		// and pl.ran, the exclusion the retry's premise needs, does not hold it.
		//
		// A retry back onto it is wanted, not a leak. A guard on pl.tried[""] would
		// refuse a seat-down defer its only other place, and lose the job, and
		// refuse a verification retry its second opinion. What such a guard is for,
		// not rejoining a line that is still full, is done at the seat the retry
		// lands on: the run-cap line check above for a seat-down defer, and
		// retrySeatBusy and awaitRetrySeat in runOne for a verification retry (by
		// llama-swap's in-flight count, for the local seat).
		// TestARetryMayReturnToALocalSeatThatOnlyCapacityDeferredTheSubtask drives
		// the case. The other direction, a retry's own chain falling back onto a
		// local seat already used, is guarded in replacementNode.
		return placement{view: localView, reason: "retry on local after " + nodeLabel(first) + " " + why}, "", true
	}
	if r.route == "local" {
		return placement{}, "", false
	}
	chosen, base, found := r.remoteAlternative(ctx, st, pl)
	if !found {
		return placement{}, "", false
	}
	return placement{view: chosen, base: base, reason: "retry on " + chosen.NodeID + " after local " + why}, "", true
}

// remoteAlternative picks the best UNTRIED remote for a retry, or reports that
// there is none. It is Place with the local node forced out of contention
// (localBusy=true), so the preference order is the fleet's own — capacity first,
// then a provably free execution slot (an IDLE node beats one that would queue),
// then queue depth, then utilization. Route local has no remotes by definition
// and is handled by the caller.
func (r *runner) remoteAlternative(ctx context.Context, st Subtask, pl *placements) (NodeView, string, bool) {
	if r.route == "local" {
		return NodeView{}, "", false
	}
	// Always read the fleet (the probe memo dedupes siblings): a retry runs after a
	// whole attempt, and route=spread's run-start snapshot is minutes old by then.
	views, bases, _ := r.fetchViews(ctx)
	freshViews, freshBases := untried(views, bases, pl.tried)
	chosen := Place(mintP2CSeed(), st, r.localView(), freshViews, true)
	if chosen.Local {
		return NodeView{}, "", false
	}
	return chosen, baseFor(chosen, freshViews, freshBases), true
}

// nodeLabel names an attempt's node for an operator-facing annotation. An
// attempt NO node took (an exhausted re-placement) has no node to name — say so
// rather than rendering an empty string mid-sentence.
func nodeLabel(pr PlacedResult) string {
	if pr.Node == "" {
		return "(no node took it)"
	}
	return pr.Node
}

// jobLabel names a subtask's job for a log line ("(unnamed)" before it has one).
func jobLabel(pr PlacedResult) string {
	if pr.JobID == "" {
		return "(unnamed)"
	}
	return pr.JobID
}

// attemptOutcome names an attempt's outcome for the retry annotations.
func attemptOutcome(pr PlacedResult) string {
	switch {
	case pr.Err != "":
		return "failed: " + pr.Err
	case pr.Result.Deferred:
		return "deferred (" + pr.Result.DeferClass + "): " + pr.Result.Reason
	case len(pr.AcceptanceFailures) > 0:
		return fmt.Sprintf("failed_verification: %v", pr.AcceptanceFailures)
	}
	return "succeeded"
}

// mergeAttempts publishes the better attempt: a clean second attempt wins
// (and is marked recovered); otherwise the FIRST attempt stands — its
// verified-wrong answer is still the more informative artifact — annotated
// with what the retry did. Both attempts were recorded by finish() already.
//
// The published attempt is not the only one that says what became of the page: the
// per-page cap counts an issue by what a seat that RAN it produced, and a first attempt
// that stands as a seat-down defer was never run by a seat that could tell. The retry's
// own verdict is carried on the published result (retryRanAndFailed) for the cap.
func mergeAttempts(first, second PlacedResult) PlacedResult {
	clean := second.Err == "" && !second.Result.Deferred && len(second.AcceptanceFailures) == 0
	var published PlacedResult
	if clean {
		second.RetriedOn = second.Node
		second.RetryNote = "first attempt on " + nodeLabel(first) + " " + attemptOutcome(first) + "; this result is the retry"
		second.retryRecovered = true
		published = second
	} else {
		first.RetriedOn = second.Node
		first.RetryNote = "retry on " + nodeLabel(second) + " also " + attemptOutcome(second) + "; this result is the first attempt"
		first.retryRanAndFailed = pageIssueFailed(second)
		published = first
	}
	published.retried = true
	return carryReplacements(first, second, published)
}

// carryReplacements folds BOTH attempts' re-placement history onto whichever
// one is published. A node that refused the first attempt still refused it when
// the retry is what gets published, and dropping that would make
// summary.replaced silently under-report a fleet shedding load — the exact
// blindness this release exists to remove.
//
// The note is assembled in CHRONOLOGICAL order — first attempt, then retry —
// independently of which attempt won. The predecessor folded "the loser" onto
// "the winner" and joined with "; then ", so on the losing-retry path it
// rendered the retry's refusals BEFORE the first attempt's under a word that
// asserts the opposite order.
func carryReplacements(first, second, published PlacedResult) PlacedResult {
	total := first.Replacements + second.Replacements
	if total == 0 {
		return published
	}
	published.Replacements = total
	published.ReplacementNote = joinNotes(first.ReplacementNote, second.ReplacementNote)
	return published
}

// joinNotes concatenates two replacement notes in the order given, tolerating
// an empty one on either side.
func joinNotes(earlier, later string) string {
	switch {
	case earlier == "":
		return later
	case later == "":
		return earlier
	}
	return earlier + "; then " + later
}

// baseFor resolves the dial base of a chosen remote view ("" when absent).
// Exact-value comparison, not NodeID: NodeID is neither guaranteed unique nor
// guaranteed present (a node may omit it), and tried is keyed by dial base
// elsewhere in this file — so the only safe match is the full view itself.
// NodeView carries a slice field (ServedModels) since 0.113.0, which == can no
// longer compare, hence reflect.DeepEqual here instead of ==.
func baseFor(chosen NodeView, views []NodeView, bases []string) string {
	for i := range views {
		if reflect.DeepEqual(views[i], chosen) {
			return bases[i]
		}
	}
	return ""
}

// unlimitedHeadroom stands in for an unpublished max_concurrent_jobs (0 =
// unknown, never a limit) — far past any real dealt count, so it can never be
// mistaken for one.
const unlimitedHeadroom = 1 << 30

// headroom is a node's remaining execution capacity: max_concurrent_jobs −
// jobs_running, floored at 0. An unpublished ceiling (0) reads as UNLIMITED —
// the same convention MaxQueueDepth/MaxConcurrentJobs already follow
// everywhere in this package (0 = unknown, never a limit), so a pre-0.100.0
// node's headroom is never artificially exhausted by this key.
func headroom(v NodeView) int {
	if v.MaxConcurrentJobs <= 0 {
		return unlimitedHeadroom
	}
	if h := v.MaxConcurrentJobs - v.JobsRunning; h > 0 {
		return h
	}
	return 0
}

// dealParallelism is how many subtasks of the call run at once: the width of RunWith's semaphore, sized from the
// deal instead of a constant (ADR 0076, the diagnosis' F01). The old constant, 4, let the joint deal commit two
// subtasks to each of four nodes while only four goroutines ran, so each node saw about one and a 12-page research
// call used four of the fleet's slots.
//
// The width is the sum, over the deal's dealt slots, of what each place takes at once, never less than
// runConcurrency:
//
//   - a remote that PUBLISHES max_concurrent_jobs counts every subtask dealt to it: the deal already held that
//     count to the node's headroom (ADR 0050 decision 4), and the process gate holds the dispatches this process
//     has open to its admission ceiling (ADR 0063 decision 7);
//   - a remote that publishes none counts at most runConcurrency, per node: an unpublished ceiling is unknown and
//     never a limit (headroom), so the deal can send it every subtask, and the bound the constant gave a call is
//     all the evidence there is that it can take them. Two such nodes give eight, not four. The semaphore is one
//     pool, so once the call's other legs finish only the process gate keeps such a node at four: admissionCeiling
//     holds a node that publishes neither ceiling to runConcurrency open dispatches;
//   - the local seat counts min(dealt, run-cap room): past its room a subtask waits in the seat's own FIFO (register
//     C-60), and holding a run slot for that wait is what the constant was bounding. A seat with no run cap
//     (fleet_max_concurrent_jobs < 0) has unlimited room, so it counts at most runConcurrency, the bound the
//     constant gave it;
//   - a slot the deal gave no place (capacityWait: every node with room was already dealt to its headroom; reserved:
//     a lease holds the seat) counts nothing. It reaches the wait only when a slot frees, so the launch loop still
//     starts it behind the dealt ones and ADR 0073 decision 9 holds as written: a wait keeps its TTL while a subtask of
//     the call has not started, and only the last to start waits to the call's horizon.
//
// A call with no deal to read (route=local, which has none) has runConcurrency.
func (r *runner) dealParallelism() int {
	var deal []spreadSlot
	switch r.route {
	case "spread":
		deal = r.spreadDeal
	case "auto", "remote":
		deal = r.autoDeal
	}
	local := 0
	perNode := make(map[string]int, len(deal))
	published := make(map[string]bool, len(deal))
	for _, sl := range deal {
		switch {
		case sl.capacityWait || sl.reserved:
		case sl.view.Local:
			local++
		default:
			perNode[sl.base]++
			published[sl.base] = sl.view.MaxConcurrentJobs > 0
		}
	}
	room := r.dealRoom
	if room >= unlimitedHeadroom {
		room = runConcurrency
	}
	width := min(local, room)
	for base, n := range perNode {
		if !published[base] {
			n = min(n, runConcurrency)
		}
		width += n
	}
	return max(runConcurrency, width)
}

// dealAutoRemote computes route=auto/remote's placement for EVERY subtask of
// the run in ONE ordered pass over ONE fleet snapshot — auto/remote's own
// version of dealSpread's invariant (W-06, register S-11/S-13). Before this,
// attempt() called fetchViews and Place PER SUBTASK, independently, on
// whichever goroutine reached it first; runConcurrency siblings then probed
// the fleet within milliseconds of each other and could all read the SAME
// free slot on the SAME node before any of them had dispatched — gate.go's
// saturated() DEMOTES rather than excludes a full node precisely because this
// snapshot is stale by construction, and re-placement (the refusal loop) is
// the net that used to catch it, one 503 at a time, after the fact.
//
// dealt tracks, per dial base, how many subtasks THIS deal has already
// assigned — placeAutoRemote checks it against headroom(v) before a node is
// even considered, so a run that fans 8 subtasks at a 4-worker node deals it
// AT MOST 4, and the rest fall to the next-best eligible node instead of
// queuing behind siblings that have not even dispatched yet.
//
// An IDLE local seat takes the work it can run only while its run-cap line has room (ADR 0076, the diagnosis'
// F02): the deal counts every subtask it gives the seat against localRunCapRoom, read once, exactly as it counts a
// remote's headroom (ADR 0063 decision 6 did the same for the spread deal). Dealing the seat all of them put
// the overflow into a FIFO the subtasks could not leave while eligible remotes idled. The overflow goes through
// the ranking below, the remoteEligible gate unchanged, to the remotes with headroom, and with none to the capacity
// wait; only while no remote could run the contract at all is the seat's own line the only queue there is.
func (r *runner) dealAutoRemote(contracts []core.AgentContract, localView NodeView, views []NodeView, bases []string, localBusy bool, failed map[string]string) []spreadSlot {
	room, note := 0, ""
	if !localBusy {
		room, note = r.localRunCapRoom()
	}
	return r.dealAutoRemoteRoom(contracts, localView, views, bases, localBusy, failed, room, note)
}

// dealAutoRemoteRoom is dealAutoRemote over a run-cap room the caller already read: RunWith reads it before the
// deal, to decide whether the roster is needed at all, and the deal must count against that same reading
// (note is what the reading was, for the reason a spent line gives).
func (r *runner) dealAutoRemoteRoom(contracts []core.AgentContract, localView NodeView, views []NodeView, bases []string, localBusy bool, failed map[string]string, room int, note string) []spreadSlot {
	r.dealRoom = room
	dealt := make(map[string]int, len(views)+1)
	out := make([]spreadSlot, len(contracts))
	for i, c := range contracts {
		st := Subtask{Contract: c, EstTokens: EstimateTokens(c)}
		// Review round 1, MEDIUM item 3: the job id is minted HERE, once, and
		// used as W-11's P2C draw seed — not a throwaway random string
		// unrelated to what the result eventually carries. attempt() reuses
		// this exact id (spreadSlot.jobID) instead of minting a second one,
		// so the seed the ranking drew on and the id the published
		// placement/ledger/corpus row names are the SAME value.
		jobID := mintJobID()
		// The seat is dealt as a busy one for this subtask once the deal has spent its line AND some remote could
		// run the contract: placeAutoRemote then ranks the remotes and never takes the idle shortcut.
		busy, spent := localBusy, false
		if !localBusy && dealt[""] >= room && r.localServesLayer(localView, c) && anyRemoteEligible(st, views) {
			busy, spent = true, true
		}
		slot := r.placeAutoRemote(jobID, st, localView, views, bases, busy, dealt, failed)
		if spent {
			slot.reason += fmt.Sprintf("; the idle local seat's run-cap line is spent by this deal (%d of %d free slot(s) dealt; %s)", dealt[""], room, note)
		}
		// A remote hint (ADR 0078) is placed on the remotes first and may fall back to the local seat where route=remote
		// would have deferred. Where no remote has room the subtask goes to the seat HERE, in the deal, when the seat
		// read free and its run-cap line has room, and not to the capacity wait: the wait would start it only after a
		// dealt subtask finished (the overflow counts nothing in the width), and with the wait switched off it would
		// defer while the seat idled. A seat that read busy, leased or occupied, or whose line this deal has spent,
		// leaves the subtask to the wait, as for an auto call.
		if slot.capacityWait && r.remoteHint() && r.hintSeatFree && dealt[""] < room && r.localServesLayer(localView, c) {
			dealt[""]++
			slot = spreadSlot{placement: placement{view: localView, reason: slot.reason + fmt.Sprintf("; the idle local seat takes it instead, a remote hint falling back where no remote has room (%d of %d free slot(s) dealt; %s)", dealt[""], room, note)}}
		}
		slot.jobID = jobID
		out[i] = slot
	}
	return out
}

// anyRemoteEligible reports whether some remote in views could run st at all: capability (remoteEligible), not
// capacity. It is the question that decides whether a seat whose line is spent has anywhere else to send the work.
func anyRemoteEligible(st Subtask, views []NodeView) bool {
	for _, v := range views {
		if remoteEligible(st, v) {
			return true
		}
	}
	return false
}

// placeAutoRemote deals ONE subtask within dealAutoRemote's joint pass: an
// idle local seat wins the work it can run (Place's own rule); busy, the
// best REMOTE that passes remoteEligible AND still has headroom over what
// this deal has already committed to it. A node at 0 headroom gets NOTHING —
// no floor, no "at least one" — and the next-best candidate is tried.
//
// A contract that names a layer this box does not declare is not work the
// idle seat can run (register A-108): it is dealt exactly as a busy seat's
// subtask is, to the best remote that declares the layer, and the idle-local
// shortcut is for the rest.
//
//   - Some node had headroom: dealt, resolved, headroom decremented for the
//     next subtask in this same deal.
//   - No node had headroom, but at least one was otherwise eligible: the
//     capacityWait sentinel — attempt() hands it to the existing capacity
//     wait (awaitCapacity), which watches for room to free the same way it
//     already watches a 503 refusal or a held lease.
//   - Nothing was eligible at all (capability, not capacity): the noRemote
//     sentinel — attempt() re-derives the exact pre-W-06 sentence
//     (r.noEligibleRemote) over this same snapshot; unrelated to headroom.
func (r *runner) placeAutoRemote(seed string, st Subtask, localView NodeView, views []NodeView, bases []string, localBusy bool, dealt map[string]int, failed map[string]string) spreadSlot {
	if !localBusy && r.localServesLayer(localView, st.Contract) {
		dealt[""]++ // the local seat's run-cap line is counted like a remote's headroom (ADR 0076)
		return spreadSlot{placement: placement{view: localView, reason: "local idle"}}
	}
	var best, bestRanked NodeView
	var bestBase string
	found, anyEligible := false, false
	prior := fleetTokSPrior(views)
	for j, v := range views {
		if !remoteEligible(st, v) {
			continue
		}
		anyEligible = true
		// The backlog gate (ADR 0063): a node that cannot START this job inside
		// the caller's patience is held out of the deal - a placement feasibility
		// refusal that prints its arithmetic (the verdict line below), never a
		// preference for a faster seat. The capacity wait re-reads it every tick.
		patience, _ := r.patience(st.Contract, v)
		if ok, _ := startsWithinPatience(v, patience); !ok {
			continue
		}
		if headroom(v) <= dealt[bases[j]] {
			continue // W-06: no floor — a node at 0 headroom gets nothing this deal
		}
		// GPU routing P1: a node's cards are spent as the deal gives them out. The
		// ranking copy carries how many subtasks this deal already put on the
		// node's cards, so a node with two free cards is preferred for its first
		// two subtasks and not its third. The stored view is never the copy:
		// baseFor matches the chosen view against the roster by value.
		ranked := v.withCardsDealt(dealt[bases[j]])
		if !found || betterRemote(seed, &st, prior, ranked, bestRanked) {
			best, bestRanked, bestBase, found = v, ranked, bases[j], true
		}
	}
	// D-105 (review round 1, BLOCKER item 2): the verdict line is built ONCE,
	// from `dealt` as it stood WHILE scanning (every candidate's headroom read
	// against the state at decision time, matching what the loop above
	// actually saw), and printed on EVERY exit — chosen, capacity-waiting, or
	// nothing eligible at all — not only the happy path. `failed` carries the
	// dead/unreachable bases from this SAME snapshot, so a node this deal
	// never even heard from is named too, not silently dropped.
	verdicts := placementVerdictLine(st, views, bases, bestBase, dealt, failed, r.patience)
	if found {
		dealt[bestBase]++
		reason := fmt.Sprintf("route=%s → %s (headroom)", r.route, best.NodeID)
		if room := cardRoomFor(best, st.Contract.Layer); room.known && cardTier(bestRanked, st.Contract.Layer) == tierSome {
			// The basis, in the node's own numbers: what it published BEFORE this
			// subtask was dealt to it.
			reason += fmt.Sprintf(" [free cards %d of %d]", room.free, room.total)
		}
		if verdicts != "" {
			reason += "; " + verdicts
		}
		if r.autoDealBusy.occupiedBy != "" {
			reason += "; " + r.autoDealBusy.why()
		}
		return spreadSlot{placement: placement{view: best, base: bestBase, reason: reason}}
	}
	if anyEligible {
		reason := fmt.Sprintf("route=%s: every eligible remote is at headroom or cannot start the job inside the caller's patience", r.route)
		if r.autoDealBusy.occupiedBy != "" {
			reason += "; " + r.autoDealBusy.why()
		}
		if verdicts != "" {
			reason += "; " + verdicts
		}
		return spreadSlot{
			placement:    placement{view: localView, reason: reason},
			capacityWait: true,
		}
	}
	return spreadSlot{placement: placement{view: localView, reason: verdicts}, noRemote: true}
}

// dealSpread computes the spread placement for EVERY subtask of the run in ONE
// ordered pass, before any dispatch goroutine starts. It is a deal, not N
// independent picks, and that is forced rather than stylistic:
//
// The invariant spread exists to hold is that within each deal CYCLE — each
// aligned window of len(nodes) slots — every eligible seat receives at most one
// subtask, so `runConcurrency` sibling subtasks never queue behind each other on
// one seat while another seat idles. A fit score that re-picks freely breaks it
// immediately: the best-scoring seat wins EVERY mechanical slot and the roomiest
// wins every reasoning slot, which is the stacking spread was built to remove.
//
// That invariant cannot be recovered by any per-subtask pure function of
// (index, own shape, roster). Proof, because it decided the shape of this code:
// let f(slot, shape) be such a function. Distinctness within a cycle forces
// f(·, shape) to be a bijection over the seats for each shape. Distinctness
// across a MIXED-shape cycle additionally forces f(p, mechanical) != f(q,
// reasoning) for all p != q — and since both are bijections, that holds only if
// f(p, mechanical) == f(p, reasoning) for every p, i.e. only if the shape does
// not influence the placement at all. Fit scoring and the invariant coexist only
// when the deal can see its siblings, so the deal is computed jointly, once,
// with every subtask's shape in hand.
//
// Computing it up front also keeps placement DETERMINISTIC and free of shared
// mutable state: the cycle bookkeeping lives in this single-threaded pass, the
// result is read-only by the time the goroutines start (same posture as the
// spreadViews snapshot), and the same contracts always produce the same deal.
func (r *runner) dealSpread(contracts []core.AgentContract, localView NodeView) []spreadSlot {
	room, note := r.localRunCapRoom()
	r.dealRoom = room
	book := &dealBook{
		// dealt holds the DIAL BASES already given a subtask in the CURRENT cycle.
		//
		// The base, not the node id, and that is the fix for a real collapse: every
		// other exclusion in this file (pl.tried, pl.excluded, the refusal cooldown,
		// the quarantine) keys on the base, and a node id is neither unique nor
		// guaranteed to be published. Two remotes that both advertise "" — or the
		// same id, which C-19 shows does drift here — shared one entry, so the
		// second remote of a cycle found its key already taken, the cycle was
		// reshuffled, and the fit score handed the SAME seat both subtasks while
		// the other one idled. The base is what the dispatcher actually dials, so
		// it is the only key that can mean "this seat already has one".
		dealt: make(map[string]bool, len(r.spreadViews)),
		// counts is the WHOLE RUN's tally per dial base (dealt is one cycle's): how many
		// subtasks this deal has already given each node, held against the node's
		// headroom so no node is dealt more than it can start (placeSpreadWith). The
		// local seat is the base "".
		counts:    make(map[string]int, len(r.spreadViews)+1),
		localRoom: room,
		localNote: note,
	}
	out := make([]spreadSlot, len(contracts))
	for i, c := range contracts {
		st := Subtask{Contract: c, EstTokens: EstimateTokens(c)}
		out[i] = r.placeSpread(i, st, localView, book)
	}
	return out
}

// dealBook is the running tally of ONE spread deal, owned by dealSpread's
// single-threaded pass.
type dealBook struct {
	dealt  map[string]bool // dial bases already given a subtask in the current cycle
	counts map[string]int  // subtasks dealt per dial base over the whole deal; "" = the local seat
	// localRoom is how many MORE runs the local seat's run-cap line takes when the
	// deal starts (localRunCapRoom, read once so every subtask deals against ONE
	// snapshot), localNote what that reading was. The local seat is counted against it
	// exactly as a remote is counted against its headroom.
	localRoom int
	localNote string
}

// localRoomLeft is what the local seat's run cap still has for this deal.
func (b *dealBook) localRoomLeft() int { return b.localRoom - b.counts[""] }

// spreadSlot is one subtask's resolved spread placement plus the deadFleet flag
// route=auto also raises — see PlacedResult.remotesUnreachable.
type spreadSlot struct {
	placement
	deadFleet bool
	// reserved marks a local slot dealt while a TEXT lease reserves the seat
	// and no remote was eligible: attempt() must wait on the lease (or defer)
	// before running it, never run it outright.
	reserved bool
	// capacityWait (W-06, register S-11/S-13): at least one remote passed
	// remoteEligible but every one of them was already dealt to its own
	// headroom by the time this subtask's turn came — attempt() must hand it
	// to the capacity wait (awaitCapacity), which watches for room to free,
	// rather than dispatch to an already-full node or defer a contract the
	// fleet can plainly run once something clears.
	capacityWait bool
	// noRemote marks a subtask for which NO node in the snapshot passed
	// remoteEligible at all — the pre-W-06 "no eligible remote" outcome,
	// unrelated to headroom. attempt() re-derives the exact sentence with
	// r.noEligibleRemote over the SAME snapshot the deal used.
	noRemote bool
	// jobID (review round 1, MEDIUM item 3): the wire job id dealAutoRemote
	// pre-mints for this subtask and uses as W-11's P2C draw seed — attempt()
	// REUSES it rather than minting a second, unrelated one, so the seed
	// recorded in the ranking and the id the published result/ledger/corpus
	// row carries are the SAME value, matching ADR 0050's "seeded from the
	// job id" claim literally rather than only in spirit. "" on route=spread
	// (dealSpread mints no id at deal time) and on the pre-W-06 fallback path
	// (a caller that reached attempt() without RunWith's precompute).
	jobID string
}

// placeSpread deals ONE subtask across the run's fleet snapshot: slot 0 is the
// local seat, then every remote that passes the hard gate FOR THIS SUBTASK in
// roster order; i mod len picks the rotation slot. The eligible set is per
// subtask on purpose — a contract too big for the 8k seat must not be dealt to
// it just because its sibling fit. With nothing eligible the subtask runs local
// and the reason says why; an infrastructure-class reason is flagged deadFleet
// exactly as route=auto flags it.
//
// A remote slot goes to the best-FIT-SCORED seat (fit.go) among the seats not
// yet dealt in this cycle — fit decides WHICH seat inside the cycle, never a
// free re-pick, so the one-subtask-per-seat-per-cycle invariant survives and a
// same-shaped fan-out still reaches every seat. `dealt` is the cycle's
// bookkeeping and is mutated here; dealSpread owns it. Ties fall back to the
// rotation order, so an all-equal roster deals exactly as it did before fit
// scoring existed.
//
// The LOCAL rotation slot is never contested by SHAPE, and that is a deliberate
// bound on this heuristic, not an oversight:
//
//   - With the local seat idle, subtask 0 lands local whatever its shape — the
//     documented guarantee that a spread's first subtask (and therefore a
//     SINGLE-subtask spread, the riskiest case for any shape heuristic) stays
//     on-box. One regex match must not be able to send an entire run off-box.
//   - The same holds for every later local slot (i mod len == 0), because the
//     fit score ranks seats by their ADVERTISED context ceiling and the local
//     seat advertises none in a delegator run. Scoring it would mean inventing
//     a number for it; leaving it in the rotation keeps its share of the fan-out
//     exactly as before, which is also what stops an all-reasoning fan-out from
//     collapsing back onto one seat.
//
// It IS contested by LOAD (0.113.20, operator decision 2026-09-06): when the
// local seat already holds a request at deal time (spreadLocalBusy, read once
// with the fleet snapshot), every local slot goes to the best-fit eligible
// remote with room instead — K delegating sessions used to stack K × 3 of every
// 8 subtasks on one local seat while the remotes idled (first-local subtask
// 155–189 s under K=3 vs 91–105 s for its siblings). An idle seat keeps every
// slot it had; `agent_spread_local_slot: "always"` restores the unconditional
// slot.
//
// And it is contested by the CONTRACT's layer (register A-108): a subtask that
// names a layer this box does not declare is never dealt the local slot - the
// seat could only defer it by name - so it deals among the remotes that declare
// the layer, and the guarantee above (subtask 0 stays local) is for the
// contracts the box can run.
func (r *runner) placeSpread(i int, st Subtask, localView NodeView, book *dealBook) spreadSlot {
	return r.placeSpreadWith(i, st, localView, book, r.skipsBusyLocal())
}

// busyReading is what the deal knows about the local seat's load at deal time
// (0.113.20). note says how the reading was obtained, or why there is none.
type busyReading struct {
	busy     bool
	inflight int
	note     string
	// loading is true when a load is IN PROGRESS (probeLocalBusy's Starting
	// branch): the in-flight count is unknown, not zero, and W-01's auto-busy
	// rule reads it as its own signal — a seat mid-load is not idle, whatever
	// inflight (0, by construction) says.
	loading bool
	// unknown is true when the seat's load could not be read (the probe failed,
	// or the read was ambiguous): busy is false only because the deal fails open.
	// The capacity wait must not read that as "now idle" (register C-88).
	unknown bool
	// occupiedBy names the declared vLLM seat that holds this box's cards while
	// the agent seat is NOT loaded, when loading the agent seat would make
	// llama-swap unload it (seatguard's verdict for the agent seat). busy is
	// true with it: the seat has nothing in flight, but taking the local slot
	// costs the occupant a cold load and its prefix cache. The reference box's
	// three-card seat is opencode's model, so a harness contract dealt to an idle
	// agent seat there evicted the operator's session (2026-10-06).
	occupiedBy string
}

// why renders the reading for a placement reason: the seat a load would evict
// when another vLLM seat holds the cards, else the in-flight count.
func (b busyReading) why() string {
	if b.occupiedBy != "" {
		return "local seat occupied: loading it would evict the loaded vLLM seat " + b.occupiedBy
	}
	return fmt.Sprintf("local seat busy: %d in flight", b.inflight)
}

// localBusyProbeTimeout bounds the one-shot read of the local seat's load: two
// loopback GETs (three with the roster). A slow or dead llama-swap deals as
// idle — the pre-0.113.20 deal — and logs why.
const localBusyProbeTimeout = 4 * time.Second

var localBusyClient = &http.Client{Timeout: localBusyProbeTimeout}

// probeLocalBusy reads the local agent seat's in-flight count through
// llama-swap (seatload.Inflight). Any failure deals as idle: the busy rule is
// an optimisation of the deal, never a gate, so "could not read" must fall
// toward the behaviour every earlier version had.
func (r *runner) probeLocalBusy(ctx context.Context) busyReading {
	if r.localBusyProbe != nil {
		return r.localBusyProbe(ctx)
	}
	seat := strings.TrimSpace(r.cfg.AgentPlannerModel(""))
	if seat == "" {
		// Nothing to probe and nothing to deal: a box with no agent seat is never given a slot
		// (localServesLayer), so "idle" would misreport it.
		return busyReading{note: noLocalSeat}
	}
	return probeSeatBusy(ctx, r.cfg, seat, "agent seat", r.checkSeatGuard)
}

// probeSeatBusy is the reading behind every "is this local seat busy" question the harness asks of a seat: the
// delegator's own (probeLocalBusy, for the agent seat) and the lane doors' (LocalSeatBusy, for the seat a single call
// runs on, ADR 0078). One function, so the two cannot disagree about what busy is. `what` names the seat's role in the
// log and the note ("agent seat", "vision seat"); guard is the seat guard's verdict for a model.
func probeSeatBusy(ctx context.Context, cfg config.Config, seat, what string, guard func(context.Context, string) seatguard.Verdict) busyReading {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		log.Printf("delegate: local seat busy probe skipped (endpoint %q, %s %q); dealing the local slot as idle", endpoint, what, seat)
		return busyReading{note: "no local endpoint or " + what + " configured"}
	}
	pctx, cancel := context.WithTimeout(ctx, localBusyProbeTimeout)
	defer cancel()
	rd, err := seatload.Inflight(pctx, localBusyClient, endpoint, seat)
	if err != nil {
		log.Printf("delegate: local seat busy probe of %s/%s failed; dealing the local slot as idle: %v", endpoint, seat, err)
		return busyReading{note: "busy probe failed: " + err.Error(), unknown: true}
	}
	if !rd.Loaded {
		// Not loaded is idle only while nothing else would be pushed off the cards to
		// load it. A loaded vLLM seat that the agent seat's load would unload (the
		// reference box's opencode-only three-card seat shares the agent seat's cards
		// and llama-swap set) makes the slot as unavailable as a busy one: the deal
		// prefers a remote with room, waits in line when every remote is full, and
		// falls back here only when no remote can take the contract at all. The guard
		// is asked before the ambiguity below: it resolves names from the serving
		// config, not the roster, so it still answers when the roster read failed (and
		// it answers "nothing evicted" when the agent seat is itself the one running).
		// A /running it cannot read names no seat and changes nothing (the deal fails
		// open, as the probe always has); a serving config it cannot read makes it
		// protect whatever vLLM seat is loaded, which only sends the work to the fleet.
		if v := guard(pctx, seat); v.Protect && v.Seat != "" {
			return busyReading{busy: true, occupiedBy: v.Seat, note: "local seat not loaded; " + v.Reason}
		}
		if rd.Ambiguous {
			// Degraded read: the roster could not resolve the seat and /running
			// holds other entries. Idle is the SAFE reading for a deal (the
			// worst case is the pre-0.113.20 stacking), but it must be visible.
			log.Printf("delegate: local seat busy probe of %s/%s is ambiguous (roster unreadable: %v; /running lists %d other model(s)); dealing the local slot as idle", endpoint, seat, rd.RosterErr, rd.RunningOthers)
			return busyReading{note: "ambiguous: roster unreadable", unknown: true}
		}
		return busyReading{note: "local seat not loaded"}
	}
	if rd.Starting {
		// A load in progress: the request that triggered it is queued on the
		// engine, and the probe deliberately did not ask the upstream (it would
		// have blocked for the whole load — register D-92). Busy, with the count
		// unknown rather than zero.
		return busyReading{busy: true, loading: true, note: "local seat " + strings.TrimPrefix(rd.Source, "running-state:") + " (a load is in progress)"}
	}
	return busyReading{busy: rd.Inflight > 0, inflight: rd.Inflight, note: rd.Source}
}

// dealReadLocalBusy reports whether this run's placement read the local seat busy and
// dealt around it: spread's busy rule (skipsBusyLocal), or route=auto's busy formula at the
// deal or a per-subtask placement. The capacity wait keeps that reading for every subtask
// it holds, not only the deal's overflow (awaitCapacity's dealBusy).
func (r *runner) dealReadLocalBusy() bool {
	switch r.route {
	case "spread":
		return r.skipsBusyLocal()
	case "auto":
		return r.autoDealReadBusy.Load()
	}
	return false
}

// autoOccupant is route=auto's reading when it found the local seat occupied: the joint
// deal's (autoDealBusy) or the per-subtask placement's (autoLocalBusy); zero otherwise.
func (r *runner) autoOccupant() busyReading {
	if r.autoDealBusy.occupiedBy != "" {
		return r.autoDealBusy
	}
	if r.autoLocalBusy.occupiedBy != "" {
		return r.autoLocalBusy
	}
	return busyReading{}
}

// checkSeatGuard asks the box's seat guard (internal/seatguard, the same reading
// the cascade uses to keep a rung from evicting a vLLM seat) whether loading model
// would unload a loaded vLLM seat. seatGuardCheck is the test seam; nil = the
// process-wide guard, which is nil (and every verdict zero) on a box that declares
// no vllm_seats, names no endpoint or turns cascade_seat_guard off.
func (r *runner) checkSeatGuard(ctx context.Context, model string) seatguard.Verdict {
	if r.seatGuardCheck != nil {
		return r.seatGuardCheck(ctx, model)
	}
	return seatguard.Shared(r.cfg).Check(ctx, model)
}

// skipsBusyLocal reports whether this run deals its local rotation slots away:
// the seat read busy at deal time AND the config did not pin the slot local.
func (r *runner) skipsBusyLocal() bool {
	return r.spreadLocalBusy.busy && r.cfg.SpreadLocalSlot() != config.SpreadLocalAlways
}

// placeSpreadWith is placeSpread with the busy rule as an explicit argument, so
// the no-remote-with-room fallback can re-deal WITHOUT it and the two paths
// share one body.
func (r *runner) placeSpreadWith(i int, st Subtask, localView NodeView, book *dealBook, skipBusy bool) spreadSlot {
	// A TEXT reservation takes the local seat out of the rotation — a spread
	// used to ignore the lease entirely, which is how three foreign contracts
	// loaded a reserved seat mid-measurement (2026-09-05 08:04–08:09). A media
	// lease was deliberately NOT consulted here (review 2026-09-06): its render
	// was arbitrated at the affinity gate (ADR 0026), which waited for the render
	// and admitted the load. That gate now turns a run away at once (S-26), so a
	// lease that fences every local seat of the contract also leaves the rotation
	// (below, GPU routing P7); a lease on cards the contract does not use does not.
	//
	// A contract that names a layer this box does not declare leaves the rotation the same
	// way (register A-108): the seat would only defer it by name while a node that declares
	// the layer idles. It deals among the remotes alone, and what they cannot take yet waits
	// in line for them below.
	localServes := r.localServesLayer(localView, st.Contract)
	spreadLease := r.spreadLeaseFor(st.Contract)
	// A lease that FENCES every local seat of the contract's chain takes the seat out of the
	// rotation too (GPU routing P7): a local run dealt onto it is turned away at the seat's own
	// pre-check, so the slot is spent on a leg that cannot take the work. A fence of a card the
	// contract does not need leaves the seat in, exactly as before.
	_, _, spreadFenced := r.fencedLocalSpread(st.Contract)
	localIn := !Reserved(spreadLease) && !spreadFenced && localServes
	// A BUSY local seat (0.113.20) leaves the rotation exactly like a leased
	// one, but only while a remote with room exists to take its slots: the
	// remotes are filtered by hasRoom (a sheddable run needs an idle slot), and
	// with none left the slot falls back to the ordinary deal below, reason
	// attached — the busy rule is an optimisation, never a way to lose work.
	skip := skipBusy && localIn
	var remotes []NodeView
	var remoteBases []string
	eligible := 0
	var heldBack, atCap []string
	for j, v := range r.spreadViews {
		if !remoteEligible(st, v) {
			continue
		}
		eligible++
		// The backlog gate (ADR 0063): eligible, but it cannot START this job
		// inside the caller's patience, so it is out of THIS deal - named in the
		// reason with its arithmetic, and re-read by the capacity wait.
		patience, clamp := r.patience(st.Contract, v)
		if ok, why := startsWithinPatience(v, patience); !ok {
			heldBack = append(heldBack, laneID(v)+": backlog ("+why+clamp+")")
			continue
		}
		if skip && !hasRoom(v, r.priority < core.BandNormal) {
			continue
		}
		// Capacity (ADR 0063, W-06's rule for the spread deal): a node is never
		// dealt more subtasks in one run than it has free execution slots
		// (headroom, no floor; an unpublished ceiling is unlimited). Capacity
		// decides FEASIBILITY only - it drops a full node from this subtask's
		// rotation and never re-ranks the rest, so the fit order, the cycle and
		// the one-subtask-per-seat-per-cycle invariant are exactly what they
		// were. The overflow is handed to the capacity wait below.
		if headroom(v) <= book.counts[r.spreadBases[j]] {
			atCap = append(atCap, fmt.Sprintf("%s: cap (%d/%d running, headroom %d, dealt %d)", laneID(v), v.JobsRunning, v.MaxConcurrentJobs, headroom(v), book.counts[r.spreadBases[j]]))
			continue
		}
		remotes = append(remotes, v)
		remoteBases = append(remoteBases, r.spreadBases[j])
	}
	// The LOCAL seat is counted the same way (ADR 0063, decision 6): its run-cap line
	// takes fleet_max_concurrent_jobs runs, minus those already registered on it, and a
	// deal that has spent that room takes the seat out of the rotation exactly as a
	// remote at its headroom is taken out - the overflow waits in line for the first
	// node that frees (the seat too) instead of piling into a line it cannot leave.
	// Only while some remote COULD run the contract (eligible > 0): with none, the
	// seat's own line is the only queue there is, as it always was.
	inRotation := localIn && !skip
	localSpent := false
	if inRotation && eligible > 0 && book.localRoomLeft() <= 0 {
		inRotation, localSpent = false, true
	}
	var nodes []NodeView
	var bases []string
	if inRotation {
		nodes, bases = []NodeView{localView}, []string{""}
	}
	nodes, bases = append(nodes, remotes...), append(bases, remoteBases...)
	held := append(append([]string(nil), heldBack...), atCap...)
	if skip && len(nodes) == 0 {
		if len(atCap) > 0 {
			// Remotes with room exist, but this run's own deal has already spent
			// every free slot they have, and the local seat is busy: the overflow
			// waits in line (the capacity wait, INV-4) instead of stacking on the
			// busy seat or on a node that would refuse it.
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote with room is already dealt to its headroom (%s); %s", strings.Join(atCap, "; "), r.spreadLocalBusy.why())}, capacityWait: true}
		}
		// An occupied seat is never the fallback (operator 2026-10-06: "wait in line"): the
		// subtask waits for the occupant to leave or a remote to free, whichever comes first.
		if r.spreadLocalBusy.occupiedBy != "" {
			what := "no remote with room"
			if eligible == 0 {
				what = "no eligible remote"
			}
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: %s; %s — waiting in line", r.spreadLocalBusy.why(), what)}, capacityWait: true}
		}
		// The reason must not send an operator chasing capacity when no remote
		// could take this contract at all.
		sl := r.placeSpreadWith(i, st, localView, book, false)
		if eligible == 0 {
			sl.reason += fmt.Sprintf(" (%s; no eligible remote)", r.spreadLocalBusy.why())
		} else {
			sl.reason += fmt.Sprintf(" (%s; no remote with room)", r.spreadLocalBusy.why())
		}
		return sl
	}
	if localSpent && len(nodes) == 0 {
		// Every remote that could run it is at its headroom (or holds a backlog the
		// caller will not wait out) AND this deal has spent the local seat's run cap:
		// the overflow waits in line rather than stack on either.
		return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote with room is already dealt to its headroom or holds a backlog past the caller's patience (%s), and the local seat's run cap is spent (%d of %d free slot(s) dealt; %s)", strings.Join(held, "; "), book.counts[""], book.localRoom, book.localNote)}, capacityWait: true}
	}
	if !localServes && len(nodes) == 0 && len(held) > 0 {
		// Every remote that could take this contract is dealt to its headroom or holds a backlog
		// the caller will not wait out, and the local seat declares no such layer: the overflow
		// waits in line for the first of them to free (INV-4), as it does when the seat's own run
		// cap is spent - never for the seat, which cannot take it.
		if seatless(localView) {
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote that can take it is already dealt to its headroom or holds a backlog past the caller's patience (%s); %s", strings.Join(held, "; "), noLocalSeat)}, capacityWait: true}
		}
		return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: every remote that can take layer %s is already dealt to its headroom or holds a backlog past the caller's patience (%s); this box declares no such layer", st.Contract.Layer, strings.Join(held, "; "))}, capacityWait: true}
	}
	if len(nodes) == 0 || (len(nodes) == 1 && nodes[0].Local) {
		why, class := r.noEligibleRemote(st, r.spreadViews, r.spreadProbeErrs)
		what := "no eligible remote"
		if len(held) > 0 {
			// Not "no eligible remote": every remote that could run it is at its
			// headroom or holds a backlog the caller will not wait out, and saying
			// otherwise would send an operator to add a node.
			what = "no remote with room"
			why = "every eligible remote is at headroom or holds a backlog past the caller's patience — " + strings.Join(held, "; ")
			class = core.DeferClassCapacity
			if len(r.spreadProbeErrs) > 0 {
				// A remote that failed its health probe is a BROKEN node, not a busy
				// one (noEligibleRemote's DEFAULT-TO-LOUD rule): name it, and keep the
				// loud class so the run reads as the broken fleet it is.
				why += fmt.Sprintf("; and %d other remote(s) failed the health probe: %s", len(r.spreadProbeErrs), strings.Join(r.spreadProbeErrs, "; "))
				class = core.DeferClassInfrastructure
			}
		}
		dead := class == core.DeferClassInfrastructure
		if !localServes {
			// No node on the fleet can take it and this box declares no layer by that name: nothing
			// runs here. The slot is dealt local only so it has a view - the seat's own decision
			// defers the contract naming the layer (runner.decide), with the fleet's verdict beside
			// it. It takes no run-cap slot and waits on no lease.
			return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread: %s, and %s so the seat cannot take it (%s)", what, localRefusal(localView, st.Contract), why)}, deadFleet: dead}
		}
		if spreadLease := r.spreadLeaseFor(st.Contract); Reserved(spreadLease) {
			// The one placement the lease exists to forbid. Dealt local so the
			// slot has a view, but flagged: attempt() waits or defers.
			return spreadSlot{placement: placement{view: localView, reason: "route=spread: local seat reserved (" + HolderLine(spreadLease) + "); " + what + " — " + why}, deadFleet: dead, reserved: true}
		}
		if fence, fenceWhy, fenced := r.fencedLocalSpread(st.Contract); fenced {
			// Likewise a fenced seat: flagged so attempt() waits in line for the lease to end.
			return spreadSlot{placement: placement{view: localView, reason: "route=spread: local seat fenced (" + fenceWhy + " — " + HolderLine(fence) + "); " + what + " — " + why}, deadFleet: dead, reserved: true}
		}
		book.counts[""]++
		return spreadSlot{placement: placement{view: localView, reason: "route=spread: " + what + " — local (" + why + ")"}, deadFleet: dead}
	}
	slot := i % len(nodes)
	if nodes[slot].Local {
		// A local slot opens a new cycle: the deck of remotes is reshuffled, so
		// the next len(nodes)-1 subtasks deal one to each seat again.
		clear(book.dealt)
		book.counts[""]++
		return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread → local (slot %d of %d)", slot+1, len(nodes))}}
	}
	prior := fleetTokSPrior(r.spreadViews)
	k := fitPickWith(st, nodes, bases, slot, book.dealt, book.counts, prior)
	if k < 0 {
		// Every eligible remote has already taken a subtask this cycle — which a
		// ragged eligible set can reach without passing through a local slot.
		// Reshuffle rather than stack: after the clear a pick always exists,
		// because len(nodes) > 1 guarantees at least one remote.
		clear(book.dealt)
		k = fitPickWith(st, nodes, bases, slot, book.dealt, book.counts, prior)
	}
	book.dealt[bases[k]] = true
	book.counts[bases[k]]++
	kind, rule := shapeOf(st)
	reason := fmt.Sprintf("route=spread → %s (slot %d of %d, fit=%s/%s)", nodes[k].NodeID, slot+1, len(nodes), kind, rule)
	if skip {
		reason += "; " + r.spreadLocalBusy.why()
	}
	if len(held) > 0 {
		// Nodes this subtask could have been dealt to and was not: named on the slot
		// they were passed over for, with their arithmetic, so an operator can see why
		// a node got nothing.
		reason += "; held back: " + strings.Join(held, "; ")
	}
	return spreadSlot{placement: placement{view: nodes[k], base: bases[k], reason: reason}}
}

// fitPick returns the index of the best seat in nodes that is neither local
// nor already dealt this cycle, or -1 when the cycle has no seat left. The scan
// starts at the rotation slot and wraps, and only a STRICTLY better key
// displaces the incumbent — so equal seats are dealt in rotation order and an
// all-equal roster behaves exactly as blind round-robin did.
// bases is parallel to nodes and is what `dealt` is keyed on — see dealSpread
// for why the node id is not a usable key here.
//
// The key is the free-card tier (GPU routing P1) first, then the fit score:
// among the seats still undealt this cycle, one whose cards can take the
// contract goes before one whose cards are all busy, and fit orders seats
// within a tier. The cycle invariant is untouched, because the tier only
// chooses WHICH undealt seat takes the slot; every seat still takes exactly one
// per cycle. counts is the deal's whole-run tally per dial base (nil = none
// dealt yet), which spends a node's free cards as subtasks land on them. Nodes
// that publish no per-card truth are one tier, so a fleet of them deals exactly
// as it did before.
func fitPick(st Subtask, nodes []NodeView, bases []string, slot int, dealt map[string]bool, counts map[string]int) int {
	return fitPickWith(st, nodes, bases, slot, dealt, counts, fleetTokSPrior(nodes))
}

// fitPickWith is fitPick with the fleet's assumed rate chosen by the caller: the spread deal passes
// fleetTokSPrior over the WHOLE roster it probed, once for every subtask of the deal, so a seat's
// score does not move as nodes at their headroom leave the rotation (Place and the joint auto deal
// take the median over the whole roster too).
//
// Two demotion keys come before all of it, the two betterRemote leads with (gate.go): a node a lease
// demotes (a long text lease, then an overdue one), then a node whose own advertisement says the next
// dispatch is refused (saturated: its admission ceiling is met, or it reports saturation.high). Both
// DEMOTE and never exclude, as there (see saturated): the
// delegator's copy of those numbers is stale by construction, a demoted node still takes its slot of
// the cycle when nothing better is left in it, and re-placement is the net that catches a wrong
// guess. Before this the spread deal read neither, so with an idle local seat a draining or
// queue-capped node kept its cycle slot while a healthy node idled: a refused dispatch, a cooldown on
// that node, one of the subtask's maxRemoteReplacements spent, and the cycle's order broken.
func fitPickWith(st Subtask, nodes []NodeView, bases []string, slot int, dealt map[string]bool, counts map[string]int, prior float64) int {
	k, best, bestTier := -1, 0, 0
	bestLease, bestSat := 0, false
	for c := 0; c < len(nodes); c++ {
		j := (slot + c) % len(nodes)
		if nodes[j].Local || dealt[bases[j]] {
			continue
		}
		lease, sat := leaseDemotionRank(nodes[j], &st), saturated(nodes[j])
		tier := cardTier(nodes[j].withCardsDealt(counts[bases[j]]), st.Contract.Layer)
		s := scoreFitWith(st, nodes[j], prior)
		better := false
		switch {
		case k < 0:
			better = true
		case lease != bestLease:
			better = lease < bestLease
		case sat != bestSat:
			better = !sat
		case tier != bestTier:
			better = tier > bestTier
		default:
			better = s > best
		}
		if better {
			k, best, bestTier, bestLease, bestSat = j, s, tier, lease, sat
		}
	}
	return k
}

// localView is THE local seat's NodeView. One constructor because the deal and
// the retry's alternativeNode must describe the same seat — two literals here
// is how a node id drifts between a placement and its telemetry.
func (r *runner) localView() NodeView {
	return NodeView{NodeID: r.localNodeID(), AgentSeat: r.cfg.AgentPlannerModel(""), Local: true}
}

// localServesLayer reports whether the local seat may be handed c. A contract that names no layer is
// the local seat's like any other. One that names a layer is the local seat's only if this box DECLARES
// that layer (register A-108): a box that does not has nothing to run it on, and the seat can only defer
// it by name (runner.decide) while a node that declares the layer sits idle. Place applies the same rule
// for its own callers; the two deals do not call Place, so each of them, and every other place that
// chooses the local seat, asks here.
//
// A delegator states its layers in its own config, never in a health document, so localView carries no
// rows and declaresLayer alone would read every composite box as layerless. cfg is the production answer
// and the first test DecideOnLayer applies (an undeclared layer defers by name), so a box that fails it
// can only ever have deferred the contract. A view that does carry rows (Place's own convention, and what
// a white-box deal is handed) is honoured as well.
//
// A view that names no agent seat serves no contract (seatless): a delegation client names no model,
// so every contract it places goes to the fleet or waits in line for it, the same way a contract naming
// an undeclared layer does, instead of landing on a seat that can only defer it.
func (r *runner) localServesLayer(local NodeView, c core.AgentContract) bool {
	if seatless(local) {
		return false
	}
	if c.Layer == "" || declaresLayer(local, c.Layer) {
		return true
	}
	_, declared := r.cfg.Layer(c.Layer)
	return declared
}

// seatless reports whether the local view names no agent seat. localView fills it with the resolution
// the local run makes (agent_model, else model); a delegation client (`install client`) writes both
// empty, so its local run could only defer "no agent seat resolvable".
func seatless(local NodeView) bool {
	return strings.TrimSpace(local.AgentSeat) == ""
}

// localRefusal says why the local seat cannot take c, for a placement reason. It is called only where
// localServesLayer said no.
func localRefusal(local NodeView, c core.AgentContract) string {
	if seatless(local) {
		return noLocalSeat
	}
	return "this box declares no layer " + c.Layer
}

// noLocalSeat is the placement-reason phrase for a box with no agent seat.
const noLocalSeat = "this box has no agent seat (agent_model and model are both empty: a delegation client)"

// attempt places (per route, or as forced by a retry) and executes one
// subtask, then verifies and records it. Every return path passes through
// finish() so no outcome can skip telemetry.
func (r *runner) attempt(ctx context.Context, i int, contract core.AgentContract, forced *placement) PlacedResult {
	start := time.Now()
	// Delegator-mints-the-id (roast delta 14): minted per attempt, BEFORE
	// placement, so even a local run correlates its telemetry line — and a
	// retry never reuses the id a node may still hold.
	jobID := mintJobID()
	r.lastJob.Store(i, jobID)

	finish := func(pr PlacedResult) PlacedResult {
		// A finished answer whose structured re-pack failed is re-packed here,
		// before the row is recorded, so the ledger and the corpus say what the
		// caller receives (rescue.go). The job is named first: the rescue's log line
		// has to say WHICH job's answer it tried to save.
		pr.JobID = jobID
		pr = r.rescueSchemaMiss(ctx, contract, pr, start)
		pr.wallMs = time.Since(start).Milliseconds()
		// An outcome that was not an answer and arrived once the call deadline had
		// passed is published — and recorded — as the call deadline (ADR 0065).
		pr = r.cutByDeadline(pr)
		// Intent ledger close-out (Option A, intent.go): an acked job whose
		// terminal answer THIS process observed is closed; the orphanable
		// exits (cancel / owned-deadline / queued give-up) stay open for the
		// recovery pass — that gap IS the durability feature.
		if pr.intentRecorded && !pr.orphanable {
			switch {
			case pr.withdrawn:
				r.intent.withdrawn(jobID) // the node confirmed it took the job back
			case pr.nodeNeverRan != "":
				r.intent.neverStarted(jobID, pr.nodeNeverRan) // the node's own record says it never ran
			default:
				r.intent.done(jobID, intentNoteTerminal)
			}
		}
		r.record(contract, pr)
		r.pairTerminal(jobID, &pr)
		return pr
	}

	// A call whose deadline has passed starts nothing new (ADR 0065): no probe, no
	// dispatch, no local run. cutByDeadline (in finish) publishes it as the call
	// deadline.
	if r.call.reached() {
		return finish(PlacedResult{Err: "canceled: the call's deadline passed before this placement began"})
	}

	// Defensive re-validate: the surfaces run PrepareContract, but Run is an
	// exported engine — an invalid contract must die here, before any network.
	// At the BOX's cap (config.AgentContextCapBytes): a composite box admits a
	// contract sized for its long seats, a plain box keeps the 256 KiB cap.
	if err := contract.ValidateWithCap(r.cfg.AgentContextCapBytes()); err != nil {
		return finish(PlacedResult{Err: err.Error(), PlacementReason: "refused before placement"})
	}

	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	localView := r.localView()

	var chosen NodeView
	var base, reason string
	// deadFleet marks a LOCAL placement taken while the configured fleet was
	// failing its health probe — see PlacedResult.remotesUnreachable.
	deadFleet := false
	switch {
	case forced != nil:
		chosen, base, reason = forced.view, forced.base, forced.reason
	case r.route == "spread":
		// Read, never re-derive: the deal is a joint assignment across all the
		// subtasks, so recomputing one subtask's slot in isolation here would
		// throw away the very constraint that makes it correct.
		d := r.spreadDeal[i]
		deadFleet = d.deadFleet
		chosen, base, reason = d.view, d.base, d.reason
		if d.capacityWait {
			// Every remote with room was already dealt to its headroom and the
			// local seat is not in this subtask's rotation: hand it to the
			// capacity wait, which places it on the first node that frees.
			return PlacedResult{waitCapacity: true, overflow: true, pendingReason: reason, PlacementReason: reason}
		}
		if d.reserved {
			// Reserved at deal time; the lease may have cleared since (a run
			// can wait minutes in the semaphore), so re-read before deciding.
			// Still reserved: hand the subtask to the capacity wait (0.113.18),
			// which watches the lease AND every remote's room — not through
			// finish(): nothing ran, so there is nothing to record.
			if Reserved(r.localLease(contract)) {
				// reason already names the holder and why no remote qualified
				// (placeSpread built it); it is the deferral's text when nothing frees.
				return PlacedResult{waitCapacity: true, pendingReason: reason, PlacementReason: reason, remotesUnreachable: deadFleet}
			}
			if _, _, fenced := r.fencedLocal(contract); fenced {
				// Dealt local under a fence (fencedLocalSpread) and still fenced.
				return PlacedResult{waitCapacity: true, fenced: true, pendingReason: reason, PlacementReason: reason, remotesUnreachable: deadFleet}
			}
			reason += " — lease cleared, running local"
		}
	case (r.route == "auto" || r.route == "remote") && r.autoDeal != nil:
		// Read, never re-derive: W-06's joint deal (dealAutoRemote), computed
		// once in RunWith over one fleet snapshot — see its own doc for why a
		// per-subtask re-probe/re-place here would throw away the very
		// invariant that makes headroom accounting correct. r.autoDeal is nil
		// only for a caller that reached attempt() without going through
		// RunWith (a white-box test driving runOne/attempt directly); that
		// case falls to default below, unchanged from before W-06.
		d := r.autoDeal[i]
		deadFleet = d.deadFleet
		if d.jobID != "" {
			// Review round 1, MEDIUM item 3: reuse the SAME id W-11's P2C
			// draw was seeded with at deal time, rather than minting an
			// unrelated one here — the seed the ranking recorded and the id
			// the published result/ledger/corpus row carries must agree.
			jobID = d.jobID
			// The id the runner remembers for this subtask (lastJob) is the one the
			// abandon publishes when this goroutine never returns. It was stored above
			// with the id minted for this attempt, which the deal's has just replaced:
			// the attempt's seat, node and rows all carry the deal's, so that is the id
			// the caller must be given (ADR 0065, decision 3).
			r.lastJob.Store(i, jobID)
		}
		switch {
		case d.capacityWait:
			// At least one remote was otherwise eligible; every one of them
			// was already dealt to its own headroom by this subtask's turn.
			// The existing capacity wait watches for room to free — the same
			// mechanism a 503 refusal or a held lease already sends work to.
			return PlacedResult{waitCapacity: true, overflow: true, pendingReason: d.reason, PlacementReason: d.reason}
		case d.noRemote && (r.route == "remote" || (r.remoteHint() && seatless(localView))):
			// Nothing in the fleet could ever take this contract (capability,
			// not capacity): the established "route=remote: no eligible
			// remote" Unplaced defer, unchanged. A remote HINT gets it too on a
			// box with no agent seat (a delegation client): the seat is the
			// hint's fallback only where there is one, and sending the contract
			// to a seat that does not exist would answer "configure an agent
			// seat" to a client built to have none, count a placement on the
			// local seat that never happened, and leave the fleet's verdict in
			// the placement text alone.
			why, class := r.noEligibleRemote(st, r.autoViews, r.autoProbeErrs)
			// The deal narrated every node it saw (D-105); keep that beside the
			// aggregate verdict so the operator reads WHICH gate refused WHOM.
			placementReason := "route=remote: no eligible remote"
			if strings.TrimSpace(d.reason) != "" {
				placementReason += "; " + d.reason
				why += " — " + d.reason
			}
			return finish(PlacedResult{
				Node: localView.NodeID, Seat: localView.AgentSeat,
				Unplaced:        true,
				PlacementReason: placementReason,
				Result: core.AgentWireResult{
					SchemaVersion: core.AgentWireSchemaVersion,
					Deferred:      true,
					DeferClass:    class,
					Reason:        "route=remote: " + why,
				},
			})
		case d.noRemote:
			// route=auto, nothing eligible. A TEXT lease held NOW (re-read at
			// dispatch time — the deal's own snapshot can be minutes stale by
			// the time this subtask's turn comes, same as the spread case
			// above) still reserves the seat; otherwise queued-local beats
			// ineligible-remote, exactly as before W-06.
			leaseInfo := r.localLease(contract)
			why, class := r.noEligibleRemote(st, r.autoViews, r.autoProbeErrs)
			// A contract that names a layer this box does not declare (register A-108) waits on no lease
			// and is not "queued-local": the seat cannot take it. It lands here only so the seat's own
			// decision defers it naming the layer, and the fleet's verdict rides beside that.
			localServes := r.localServesLayer(localView, contract)
			// The class of "no eligible remote" rides every branch below, the waits included: a fleet
			// that fails every probe is reported whether the seat runs the work now or the subtask
			// waits in line for a lease to end (a fallback must not hide a failure).
			deadFleet = class == core.DeferClassInfrastructure
			if localServes && Reserved(leaseInfo) {
				return PlacedResult{waitCapacity: true, pendingReason: "local seat reserved (" + HolderLine(leaseInfo) + "); no eligible remote — " + why, remotesUnreachable: deadFleet}
			}
			if fence, fenceWhy, fenced := r.fencedLocal(contract); localServes && fenced {
				return PlacedResult{waitCapacity: true, fenced: true, pendingReason: "local seat fenced (" + fenceWhy + " — " + HolderLine(fence) + "); no eligible remote — " + why, remotesUnreachable: deadFleet}
			}
			// An occupied seat (operator 2026-10-06: "wait in line") is a place in line too, never
			// "queued-local": loading it would unload the vLLM seat holding the cards. The wait takes
			// it once the occupant leaves (localStillBusy reads occupiedBy) or a remote frees.
			if occ := r.autoOccupant(); localServes && occ.occupiedBy != "" {
				return PlacedResult{waitCapacity: true, overflow: true, pendingReason: occ.why() + "; no eligible remote — " + why, remotesUnreachable: deadFleet}
			}
			chosen = localView
			reason = r.queuedLocalReason(why)
			if !localServes {
				reason = fmt.Sprintf("no eligible remote, and %s so the seat cannot take it (%s)", localRefusal(localView, contract), why)
			}
		default:
			chosen, base, reason = d.view, d.base, d.reason
		}
	default:
		// Placement. Health is fetched ONLY when a remote could actually be
		// chosen (route=remote, or route=auto with the local GPU spoken for):
		// Place ignores remotes entirely when the local node is idle, so probing
		// them would be pure chatter.
		busy := false
		var leaseInfo gpulease.Info
		switch r.route {
		case "remote":
			busy = true // forced remote behaves as "local unavailable" for Place
		case "auto":
			leaseInfo = r.localLease(contract)
			// W-01 (register S-01): a lease is one way the local seat is
			// spoken for, but the overwhelming majority of runs hold no lease
			// at all — and a local seat already carrying more in-flight
			// requests than the fleet's own concurrency cap is exactly as
			// unavailable as one under a lease, yet Place never looked at the
			// fleet for it. probeLocalBusy is read ONCE per Run (cached on
			// the runner, sync.Once) so runConcurrency sibling subtasks agree
			// on one reading, the same invariant spreadLocalBusy holds for
			// route=spread. A probe failure fails OPEN to idle, exactly as
			// probeLocalBusy always has.
			r.autoLocalBusyOnce.Do(func() { r.autoLocalBusy = r.probeLocalBusy(ctx) })
			local := r.autoLocalBusy
			busy = leaseInfo.Held || r.atRunCap(local.inflight) || local.loading || local.occupiedBy != ""
			if r.atRunCap(local.inflight) || local.loading || local.occupiedBy != "" {
				r.autoDealReadBusy.Store(true) // the seat's load, never the lease (see the joint deal)
			}
			// A remote hint (ADR 0078) is placed remotes-first: the seat is unavailable whatever it read, and the
			// reading above is kept for the fallback to wait on a lease or an occupant.
			if r.remoteHint() {
				busy = true
			}
		}
		var views []NodeView
		var bases []string
		var probeErrs []string
		if busy && r.route != "local" {
			views, bases, probeErrs = r.fetchViews(ctx)
		}
		chosen = Place(jobID, st, localView, views, busy)
		// The wasted local leg (GPU routing P7): nothing else could take it, so the local seat
		// would - but a lease this process does not hold fences every seat of the contract's
		// chain, and the seat would turn the run away at its own pre-check.
		var fence gpulease.Info
		var fenceWhy string
		var fenced bool
		if chosen.Local && busy && r.route == "auto" && r.localServesLayer(localView, contract) {
			fence, fenceWhy, fenced = r.fencedLocal(contract)
		}

		switch {
		case r.route == "local":
			reason = "route=local forced"
		case r.route == "remote" && chosen.Local:
			// An explicit remote route with nothing eligible must NOT silently
			// fall local — defer loudly and let the caller decide. The diagnosis
			// distinguishes the causes that used to share one sentence, and the
			// class separates "a box is broken" from "this contract cannot be
			// placed anywhere, however healthy the fleet".
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return finish(PlacedResult{
				Node: localView.NodeID, Seat: localView.AgentSeat,
				// NO node ran this: the route was forced remote and nothing was
				// eligible, so this is the same "nobody took the work" state a
				// capacity defer or a shed reports. Node/Seat above are the
				// DECIDING box, not the box that ran it — a surface that renders
				// them as the node under test names the operator's own machine
				// and hides which base failed (issue #250), so it needs this flag
				// to tell the two apart. Inert for the tallies: the one branch
				// that reads Unplaced also requires Replacements > 0.
				Unplaced:        true,
				PlacementReason: "route=remote: no eligible remote",
				Result: core.AgentWireResult{
					SchemaVersion: core.AgentWireSchemaVersion,
					Deferred:      true,
					DeferClass:    class,
					Reason:        "route=remote: " + why,
				},
			})
		case r.route == "remote":
			reason = "route=remote forced → " + chosen.NodeID
		case !busy:
			reason = "local idle"
		case chosen.Local && Reserved(leaseInfo):
			// A TEXT lease reserves the seat: "queued-local beats
			// ineligible-remote" is exactly the placement the reservation
			// exists to forbid. Wait for the holder (agent_lease_wait_sec),
			// then defer naming it — never run on the reserved cards.
			// Since 0.113.18 the wait is the capacity wait (awaitCapacity):
			// it watches the lease AND every remote's room, so a remote that
			// frees while the local card is reserved takes the work.
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return PlacedResult{waitCapacity: true, pendingReason: "local seat reserved (" + HolderLine(leaseInfo) + "); no eligible remote — " + why, remotesUnreachable: class == core.DeferClassInfrastructure}
		case chosen.Local && fenced:
			// A lease fences every local seat this contract could run on (fencedLocal): the same
			// place in line as a reservation, without the dial that would be turned away.
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return PlacedResult{waitCapacity: true, fenced: true, pendingReason: "local seat fenced (" + fenceWhy + " — " + HolderLine(fence) + "); no eligible remote — " + why, remotesUnreachable: class == core.DeferClassInfrastructure}
		case chosen.Local && r.route == "auto" && r.autoOccupant().occupiedBy != "" && r.localServesLayer(localView, contract):
			// The occupied seat waits in line like a reservation (operator 2026-10-06: "wait in
			// line"): never loaded over the vLLM seat holding the cards while that seat stays.
			why, class := r.noEligibleRemote(st, views, probeErrs)
			return PlacedResult{waitCapacity: true, overflow: true, pendingReason: r.autoOccupant().why() + "; no eligible remote — " + why, remotesUnreachable: class == core.DeferClassInfrastructure}
		case chosen.Local:
			why, class := r.noEligibleRemote(st, views, probeErrs)
			reason = r.queuedLocalReason(why)
			// USE the class here too. route=remote already exits non-zero on a
			// fleet that failed every probe; route=auto discarded the identical
			// verdict (`why, _ :=`), so a fleet that had been down for a week read
			// green forever behind a series of correct local placements. The
			// placement stays right — the work runs locally — but the broken fleet
			// gets reported. Only the INFRASTRUCTURE class is loud: "no remotes
			// configured" and "they answered and did not qualify" are ordinary
			// idle-local life and must never make a normal run exit non-zero.
			deadFleet = class == core.DeferClassInfrastructure
		default:
			reason = "local busy; placed on " + chosen.NodeID
			if r.remoteHint() {
				reason = "remotes first; placed on " + chosen.NodeID
			}
			if r.route == "auto" && r.autoLocalBusy.occupiedBy != "" {
				reason = r.autoLocalBusy.why() + "; placed on " + chosen.NodeID
			}
		}
		if !chosen.Local {
			base = baseFor(chosen, views, bases)
		}
	}

	if chosen.Local {
		// The composite decision (ADR 0039): which layer and seat serve this
		// contract on THIS box. A forced placement from the capacity wait
		// carries the decision it found; otherwise decide now. The zero
		// Decision (a plain box) runs the planner seat and publishes nothing.
		var dec placetable.Decision
		if forced != nil && forced.decided != nil {
			dec = *forced.decided
		} else {
			dec = r.decide(ctx, contract, st)
		}
		switch {
		case dec.Defer:
			pr := r.decidedDefer(localView, dec, reason)
			pr.remotesUnreachable = deadFleet
			return finish(pr)
		case dec.Wait:
			// The decided seat would evict a busy seat (the pair's long seat
			// over a loaded agent-pool): hold in the capacity wait, which
			// re-runs the decider per tick and runs here once it drains —
			// not through finish(): nothing ran, nothing to record yet.
			return PlacedResult{waitCapacity: true, pendingReason: dec.Reason, PlacementReason: reason, decided: &dec}
		}
		pr := r.runLocal(ctx, jobID, contract, localView, reason, dec)
		pr.remotesUnreachable = deadFleet
		pr.ranLocal = true
		return finish(pr)
	}
	if base == "" {
		// Unreachable (chosen came from views); kept for defense — a placement
		// with no dial target is a bug, not a defer.
		return finish(PlacedResult{Node: chosen.NodeID, PlacementReason: reason, Err: "internal: placed node has no base URL"})
	}
	// A composite remote (advertised rows): the dispatched copy names the layer
	// the table chose so the node runs the same decision for that layer with
	// its own live guards (council R5); the delegator's block is published
	// until the node's own arrives on the wire.
	dispatched := contract
	var remotePlaced *core.Placed
	// runSeat is the seat that decision puts the contract on — the layer's
	// seat, not the advertised agent seat, whenever the table chose a layer.
	// It is carried into the poll bound (register D-116, review finding 2):
	// the node sizes its wall from the seat it actually runs on, and health
	// advertises a rate for the agent seat alone.
	runSeat := ""
	if dec, ok := remoteDecision(st, chosen); ok && !dec.Defer {
		dispatched.Layer = dec.Layer
		remotePlaced = placedOrNil(dec)
		runSeat = dec.Seat
	}
	// The process gate (processgate.go): this process never holds more dispatches
	// open on a node than the node's published admission ceiling, across EVERY
	// concurrent Run. A dispatch that would is not sent - nothing reached the node,
	// so this is no refusal and writes no row - and the subtask waits in line for the
	// first node that frees.
	release, admitted := processGate.tryAcquire(base, admissionCeiling(chosen))
	if !admitted {
		why := gateFullReason(base, chosen)
		return PlacedResult{waitCapacity: true, gated: true, pendingReason: why, PlacementReason: why}
	}
	defer release()
	pr := r.runRemote(ctx, base, jobID, dispatched, chosen, runSeat)
	// The node's part of the job is over: runRemote returns on its terminal answer, on
	// the delegator giving up on the job, or on a refusal. What finish() does next can be
	// the delegator's own rescue of the answer (rescue.go), on this box and allowed
	// minutes; a slot still counted through it would keep an idle node out of every
	// sibling Run's reach (ADR 0063 decision 7: the count ends with a terminal answer
	// or with the delegator giving up). The deferred release above stays as the net for
	// a panic; release is safe to run twice.
	release()
	pr.ranBase = base
	pr.PlacementReason = reason
	if pr.Node == "" {
		pr.Node = chosen.NodeID
	}
	pr.Placed = remotePlaced
	if pr.Result.Placed != nil {
		pr.Placed = pr.Result.Placed
	}
	if pr.Seat == "" {
		pr.Seat = chosen.AgentSeat
		if remotePlaced != nil && remotePlaced.Seat != "" {
			pr.Seat = remotePlaced.Seat
		}
	}
	return finish(pr)
}

// runLocal executes in-process via the LocalRunner seam and shapes the result.
// dec is the composite decision the run is made under: its seat and placed
// block are handed to the runner (LocalOptions) and published on the result;
// the zero Decision (a plain box) hands zero options, exactly as before.
func (r *runner) runLocal(ctx context.Context, jobID string, contract core.AgentContract, view NodeView, reason string, dec placetable.Decision) PlacedResult {
	pr := PlacedResult{Node: view.NodeID, Seat: view.AgentSeat, PlacementReason: reason, Placed: placedOrNil(dec)}
	if r.local == nil {
		pr.Err = "no local runner wired (delegator surfaces must supply one)"
		return pr
	}
	opts := LocalOptions{Seat: dec.Seat, Placed: pr.Placed, ParentJobID: jobID}
	if dec.Seat != "" {
		pr.Seat = dec.Seat
	}
	// A local placement starts the moment it is handed to the runner: no
	// queue, no ack. "" as the node = this box's PAIR identity — unless the
	// endpoint is another box's engine, whose name the card then carries
	// (register C-58: PAIR showed <node-b> doing <node-c>'s work).
	pairNode := ""
	if host := modelaffinity.EndpointHost(r.cfg.Endpoint, r.cfg.FleetNodeID); host != "" {
		pairNode = host
		pr.PlacementReason += "; engine " + r.cfg.Endpoint + " is " + host + "'s (attributed there)"
	}
	// The run is handed to the seat now: mark it in the ledger (PR-14, ADR 0064),
	// so a runner that wedges is visible while it does.
	r.recordStarted(contract, jobID, "", pr.Node, pr.Seat, pr.PlacementReason)
	// Queued until the seat is working on it (seatWorking): the run's own
	// progress reports flip the card, so a cold load does not read "running".
	r.pairInflight(&pr, jobID, pairNode, nil, pr.Seat, "queued", true)
	gate := newPairStartGate(func() { r.pairInflight(&pr, jobID, pairNode, nil, pr.Seat, "running", true) })
	wire, err := r.local(core.WithProgressReport(ctx, gate.observe), contract, opts)
	gate.stop()
	if err != nil {
		pr.Err = "local run: " + err.Error()
		return pr
	}
	pr.Result = wire
	if wire.NodeID != "" {
		pr.Node = wire.NodeID
	}
	if wire.Seat != "" {
		pr.Seat = wire.Seat
	}
	if wire.Placed != nil {
		// The pipeline's own stamp wins: identical by construction (it was
		// handed this block), and the one source of truth for the wire.
		pr.Placed = wire.Placed
	}
	if !wire.Deferred {
		// Same guard runRemote applies: a defer produced no answer, so running
		// acceptance over it manufactures "failures" about content that was
		// never claimed — turning an honest defer into a verification failure
		// in the ledger and the corpus.
		pr.AcceptanceFailures = EvalAcceptance(contract, wire)
		// No strikeOnFingerprint here ON PURPOSE: the local seat is the one
		// placement always able to take a contract (replacementNode reserves it
		// as the last resort), so quarantining it would strand every subtask.
		// A local off-document answer still fails verification and is counted.
	}
	return pr
}

// runRemote drives the fleet wire: dispatch (202 ack, retried once on
// transport doubt under the SAME job id), then poll to a terminal state or
// the poll deadline.
//
// view is the health view of the node being dispatched to. It is the input to
// the auto poll bound (register D-116): what that node advertises about its
// seat is what the delegator's clock is sized from. A zero view simply bounds
// nothing — the cap, as before. runSeat is the seat the decision puts this run
// on ("" when none was decided); it guards the bound against being sized from
// a seat the run will not use.
func (r *runner) runRemote(ctx context.Context, base, jobID string, contract core.AgentContract, view NodeView, runSeat string) PlacedResult {
	var pr PlacedResult
	payload, err := json.Marshal(contract)
	if err != nil {
		pr.Err = "marshaling contract: " + err.Error()
		return pr
	}
	// Queued: the node is about to be asked. The seat named here is the one
	// the decision intends (the layer's seat on a composite node, else the
	// advertised agent seat) and it is what the card is keyed on from now on.
	intendedSeat := runSeat
	if intendedSeat == "" {
		intendedSeat = view.AgentSeat
	}
	r.pairInflight(&pr, jobID, pairNodeName(base, view.NodeID), []string{view.NodeID}, intendedSeat, "queued", false)
	disp := r.dispatchDetailed(ctx, base, jobID, payload)
	// A 503 is a refusal that returns AT ONCE (ADR 0063). It used to sleep the
	// node's Retry-After here (up to the 300 s the node clamps it to), retry the
	// same node once and charge the sleep to the contract's budget: on 2026-09-29
	// that was 232 refusals and 16 caller-hours asleep on nodes that had just said
	// no while others had room. The hint is carried on the result instead
	// (retryAfterSec); noteCooldown turns it into the node's cooldown, and the
	// capacity wait - which credits what it idles and re-reads health every few
	// seconds - is the only thing that ever waits on it.
	if disp.err != nil {
		pr.Err = disp.err.Error()
		// Carry the class so runOne can decide whether ANOTHER node is worth
		// asking. Set here and nowhere else: this is the one moment at which
		// the node has answered and no seat can possibly hold the contract.
		pr.refused, pr.refusalStatus, pr.retryAfterSec = disp.refused, disp.status, disp.retryAfterSec
		return pr
	}
	// The node ACKED: from here the job can be orphaned by delegator death,
	// so persist the intent before any polling (Option A, intent.go).
	r.intent.dispatched(jobID, base, contract.Goal)
	pr.intentRecorded = true
	// And say so in the ledger now, not when the job ends: a hang, or a ghost that
	// outlives this process, is then visible while it matters (PR-14, ADR 0064).
	r.recordStarted(contract, jobID, jobID, view.NodeID, intendedSeat, "")
	// The card stays queued past the ack: it turns running when a poll shows
	// the node's seat working on the job (seatWorking), not at the ack.
	pairRunning := false
	var pairSince time.Time

	// pollBudget is the budget for WORK. Before the node gained a real queue
	// (0.100.0) that distinction did not exist: the node's Accept was its
	// start, so dispatch → running was microseconds and the whole budget went
	// to execution by construction. Now a job can legitimately sit in the
	// node's backlog in state `accepted`, and charging that wait to the
	// contract's own timeout would convert a node's loud, immediate
	// "503 queue full" into a slow timeout that burns the entire budget and
	// then hands back a manufactured "node accepted the job but did not reach
	// a terminal state" defer — the same lost work with less signal and more
	// wall clock. So QUEUED TIME IS CREDITED BACK: the deadline moves out by
	// the time the job provably spent waiting, and the contract still gets the
	// execution budget it asked for.
	//
	// pollBudgetFor sizes it: the contract's wall + grace, or — for a contract
	// the caller left unsized (register D-116) — the bound sized from what the
	// node ADVERTISES, plus the admission allowance `slack` carried on top of
	// it until a started wall is observed (wallAnchored). pollNote names the
	// source and rides the result (poll_note).
	pollBudget, slack, pollNote := pollBudgetFor(view, contract, runSeat)
	pr.PollNote = pollNote
	start := time.Now()
	// anchor is where the CLOCK is measured from. It is the dispatch instant
	// until the node publishes a started wall, and that wall's start after —
	// so the budget bounds the node's WALL, not the wall plus however long the
	// node spent admitting the job.
	anchor := start
	wallAnchored := false
	// queuedWaitBudget bounds the credit — an unbounded wait is its own
	// failure mode. A job may wait for a slot at most as long as it was
	// allowed to run (pollBudget), and the wait it is GIVEN is derived from the
	// node's own ETA (queueBudgetFor, ADR 0063): 1.5 x the wait a new job faces
	// + 30 s, floored at 60 s. It used to be a fixed five minutes whatever the
	// node said, which abandoned jobs a node was about to start — and the node
	// ran them anyway. Total wall clock is therefore bounded by
	// pollBudget + queuedWaitBudget.
	//
	// queueView is the node's picture the budget is derived from: the placement
	// snapshot at first (a route=spread run's is as old as the run), replaced by a
	// fresh read the moment the job is seen sitting in the node's backlog - see the
	// `accepted` arm below.
	queueView := view
	queuedWaitBudget := queueBudgetFor(queueView, pollBudget)
	// queuedCredit is time PROVABLY spent in the backlog: an interval is banked
	// only when it is bracketed by two CONSECUTIVE `accepted` observations with
	// nothing else in between — see the reset at the top of the poll loop. The
	// partial spans either side (dispatch → first queued poll, last queued poll
	// → first running poll) and any interval interrupted by a transport error,
	// a 404 or a 5xx are deliberately NOT credited. Each omission is at most
	// one pollEvery of real backlog, and under-crediting is the safe direction:
	// over-crediting silently hands a job more execution budget than the
	// contract granted, and makes the give-up message assert the job "waited in
	// the node's backlog" across time the node was doing something else.
	var queuedCredit time.Duration
	var lastQueuedAt time.Time
	// queuedPolls counts ONLY the polls that actually answered `accepted`.
	// Deliberately not the total poll count: this run may also have seen 404s
	// and 5xxs, and a message saying "N polls, every one queued" over a total
	// that included them would be the delegator authoring a claim the node
	// never made — the exact failure the shapes in unownedDetail exist to
	// prevent.
	queuedPolls := 0
	// The queue-budget refresh (below) is attempted on the first queued polls until one read
	// succeeds, at most maxQueueRefreshTries times: refreshed says one did, refreshErr is
	// the newest failure while none has - the deadline message names it.
	refreshTries, refreshed := 0, false
	var refreshErr error
	// setDeadline is the ONE place the deadline is computed, so no arm can
	// rebuild it from a stale base. Once the clock is anchored on the node's
	// own wall start, the backlog credit is deliberately NOT added again: the
	// anchor already subsumes every second spent before the wall began — the
	// backlog wait included — and adding it twice would hand the run more
	// budget than either clock granted. queuedCredit itself keeps accumulating
	// for the operator-facing message, which reports an observed fact.
	var deadline time.Time
	// Liveness (0.131.0): progressUntil is how far the node's reported progress
	// holds the poll open past `deadline`; the loop stops only when BOTH have
	// passed. lastProgress/lastAllowance feed the deadline message.
	var progressUntil, lastProgress time.Time
	var lastAllowance time.Duration
	extendedOnProgress := 0
	setDeadline := func() {
		credit := queuedCredit
		if wallAnchored {
			credit = 0
		}
		deadline = anchor.Add(pollBudget + credit)
	}
	setDeadline()
	redispatches := 0
	// A poll failure is NOT nothing. Before these, pollOnce's error was bound
	// and dropped, so a node that died after acking (refused dial, per-poll
	// deadline, 500/502/503, a 200 whose body is not JSON) ran out the clock and
	// was handed a MANUFACTURED defer that runOne then stamped with that node's
	// id and seat — the delegator inventing a sentence the node never said, and
	// exiting 0.
	//
	// lastPollErr keeps the newest UNRETIRED failure for the operator: a healthy
	// answer CLEARS it, because one early 503 followed by fifty clean `running`
	// answers is a node that recovered, not a broken one.
	//
	// Two flags, not one, and only the second decides defer-vs-failure:
	//   - sawNodeAnswer: something at that address answered at all. Message text
	//     only — it says "reachable", never "running your job".
	//   - sawJobOwned: a 200 whose state says THIS NODE HOLDS THE JOB
	//     (accepted/running/done/error). That, and only that, earns a defer at
	//     the deadline, because a defer asserts the node accepted the work.
	// A poll 404 is a POSITIVE DENIAL — the node stating it never held the job.
	// It used to set the single answered flag, so a deadline landing inside the
	// re-dispatch window published {deferred, class:budget, "node accepted the
	// job but did not reach a terminal state"} for a job that node never ran.
	var lastPollErr error
	sawNodeAnswer := false
	sawJobOwned := false
	// saw404 records that a 404 ACTUALLY happened. The failure message used to
	// print the 404-denial sentence for every answering node, so a node that
	// returned only 503s published "a poll 404 DENIES it ever held it" — a denial
	// nobody made, authored by the delegator, which is the exact fabrication the
	// failure path exists to prevent.
	saw404 := false
	// lastState is the last state the node reported for the job ("" = it never
	// answered): a give-up asks the node to withdraw a job only while it can still
	// be unstarted. ask is the one withdraw this loop's queue-deadline arm may ask
	// for, and what the node answered when it was not a confirmation: the arm asks
	// once, and a give-up that follows a `running` answer — a cancel or a deadline
	// that lands before the next poll has answered, or a node that then says
	// `accepted` again — reads it instead of asking again, and can still say what
	// the node was told and what it said.
	lastState := ""
	var ask withdrawAsk
	// pollFails bounds the failure logging (both arms below fired once PER
	// POLL) and summarizes on the way out, whichever exit is taken.
	pollFails := newPollFailLog(jobID, base)
	defer pollFails.summarize()
	for {
		if err := ctx.Err(); err != nil {
			pr.Err = "canceled: " + err.Error()
			// The node may still start it — recovery's case — unless it confirms it
			// took the job back (ADR 0064). A withdraw that was asked and not confirmed
			// says why on the row.
			if note := r.giveUp(ctx, base, jobID, lastState, ask, &pr); note != "" {
				pr.Err += "; " + note
			}
			return pr
		}
		if now := time.Now(); now.After(deadline) && now.After(progressUntil) {
			if !sawJobOwned {
				// Never a defer: nothing on that node ever reported OWNING this
				// job, so there is no defer to report. A FAILURE (Summary.Failed,
				// non-zero CLI exit) is the honest outcome — a broken node, or one
				// denying the job, must read broken.
				pr.Err = fmt.Sprintf("poll deadline after %s%s: %s%s", pollBudget, queuedNote(queuedCredit), unownedDetail(sawNodeAnswer, saw404, redispatches, lastPollErr), boundNote(pollNote, slack))
				// The node ACKED this job, so it may hold it however little it has said
				// since, and nothing observed it end: the intent must not claim so. Ask the
				// node to take the job back like every other give-up — a confirmation
				// closes the intent, anything else leaves it for the recovery pass (ADR 0064).
				if note := r.giveUp(ctx, base, jobID, lastState, ask, &pr); note != "" {
					pr.Err += "; " + note
				}
				return pr
			}
			// Roast delta 14: mark deferred, reason PREFIXED "poll deadline"
			// (stable key), and STOP. The node may still finish server-side; the
			// job id in the telemetry line lets an operator reconcile by hand.
			// The wording says only what is known: it acked and it never reached
			// a terminal state — not that it "could not complete the contract".
			reason := fmt.Sprintf("poll deadline after %s%s: node accepted the job but did not reach a terminal state%s", pollBudget, queuedNote(queuedCredit), boundNote(pollNote, slack))
			if !lastProgress.IsZero() {
				// The node reported liveness and it went STALE: say when it
				// last moved and what it was allowed, so the reader can tell a
				// node that stopped reporting from one that never did.
				reason += fmt.Sprintf("; the node's last progress was %.0fs ago against a %.0fs allowance", time.Since(lastProgress).Seconds(), lastAllowance.Seconds())
				if extendedOnProgress > 0 {
					reason += fmt.Sprintf(" (the poll was extended on progress %d time(s) past its %s budget)", extendedOnProgress, pollBudget)
				}
			}
			// A node that answered every poll normally simply ran out of clock:
			// a BUDGET defer. One whose last answer we could not use (a 5xx, an
			// unknown state) is a broken box, and classing the two alike is how
			// a broken node keeps reading as a slow one.
			class := core.DeferClassBudget
			if redispatches > 0 {
				// It also LOST the job at least once. unownedDetail renders the
				// re-dispatch count on the FAILURE path and this arm discarded it,
				// so a node that dropped the work twice and then answered normally
				// to the deadline published the same quiet budget defer a
				// healthy-but-slow node earns — with nothing anywhere saying it had
				// lost the job. A node that forgets jobs is broken, not slow.
				reason += fmt.Sprintf(" (the node LOST the job %d time(s) first; each was re-dispatched under the same id)", redispatches)
				class = core.DeferClassInfrastructure
			}
			if lastPollErr != nil {
				reason += " (last poll error: " + lastPollErr.Error() + ")"
				class = core.DeferClassInfrastructure
			}
			// Acked, owned, no terminal state seen — recovery's case. Unless the node
			// still holds the job as `accepted` and confirms it takes it back: a job
			// that never started must not be run later for nobody (ADR 0064), and the
			// reason then says what became of it.
			note := r.giveUp(ctx, base, jobID, lastState, ask, &pr)
			if pr.withdrawn {
				reason += "; the job never started and was withdrawn from the node"
			} else if note != "" {
				reason += "; " + note
			}
			pr.Result = core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, DeferClass: class, Reason: reason}
			return pr
		}
		poll, perr := r.pollOnce(ctx, base, jobID)
		state, data, jobErr, status := poll.State, poll.Data, poll.JobErr, poll.Status
		// Liveness (0.131.0, ADR 0055): a node that reports PROGRESS is polled
		// past any clock sized in advance — the job stays alive while its last
		// progress event is inside the node's own stall allowance (+ grace),
		// bounded by the node's ceiling. A node that reports none is polled
		// exactly as before; a node whose report goes stale is not extended.
		if status == http.StatusOK && poll.Progress != nil && poll.Progress.LastProgressMs > 0 {
			lastProgress = time.UnixMilli(poll.Progress.LastProgressMs)
			allow := time.Duration(poll.Progress.AllowanceMs) * time.Millisecond
			if allow <= 0 {
				allow = 60 * pollSecond
			}
			lastAllowance = allow
			until := lastProgress.Add(allow + pollGrace)
			if poll.Progress.CeilingSec > 0 {
				if cap := anchor.Add(time.Duration(poll.Progress.CeilingSec)*pollSecond + pollGrace); until.After(cap) {
					until = cap
				}
			}
			if until.After(progressUntil) {
				if until.After(deadline) && !progressUntil.After(deadline) {
					extendedOnProgress++
				}
				progressUntil = until
			}
		}
		if !pairRunning && perr == nil && status == http.StatusOK && state == "running" {
			if pairSince.IsZero() {
				pairSince = time.Now()
			}
			if seatWorking(poll.Progress, pairSince, time.Now()) {
				pairRunning = true
				r.pairInflight(&pr, jobID, pairNodeName(base, view.NodeID), []string{view.NodeID}, intendedSeat, "running", false)
			}
		}
		// The node's OWN wall, published while the job runs (jobWire
		// `wall_sec`, register D-116), beats every estimate: it is the number
		// the run is actually executing under. It can only RAISE the bound —
		// the delegator must never abandon a job the node is still running
		// inside its own wall — and a node too old to publish it changes
		// nothing. Only for an auto contract: an explicit timeout_sec is the
		// caller's own number and the node runs exactly it.
		if contract.TimeoutAuto && status == http.StatusOK && poll.WallSec > 0 {
			switch {
			case !wallAnchored:
				// FIRST started wall (review finding 1). The node stamps
				// wall_sec at the line that opens its wall context, so this is
				// the delegator's only observation of where the node's clock
				// actually began — after the cordon, the pre-flight, the cold
				// load and the coherence probe. Re-anchor here and drop the
				// admission allowance: from this instant the honest budget is
				// the node's own wall plus transport slack, and nothing else.
				wallAnchored = true
				anchor = time.Now()
				slack = 0
				pollBudget, pollNote = anchoredPollBound(poll.WallSec)
				pr.PollNote = pollNote
				setDeadline()
			default:
				// Already anchored: only a LARGER wall moves anything, and it
				// can only ever raise (raisedPollBound).
				if raised, note, ok := raisedPollBound(pollBudget, poll.WallSec); ok {
					pollBudget = raised
					pollNote = note
					pr.PollNote = note
					setDeadline()
					queuedWaitBudget = max(queuedWaitBudget, queueBudgetFor(queueView, pollBudget))
				}
			}
		}
		// EVERY observation closes the queued span; only the `accepted` arm
		// below re-opens it. Reset-then-re-arm is deliberate structure, not
		// style: lastQueuedAt used to be cleared only in the `running` arm, so
		// the transport-error, 404 and 5xx arms all left it set — and an
		// interval bracketed by two `accepted` answers was banked IN FULL even
		// when the node spent the middle of it returning 503s, or denying with
		// a 404 that it had ever held the job. The endpoints being `accepted`
		// does not make the interior backlog wait. Written this way a future
		// poll arm cannot silently inherit an open span.
		prevQueuedAt := lastQueuedAt
		lastQueuedAt = time.Time{}
		switch {
		case perr != nil:
			// Transient transport noise: keep polling until the deadline —
			// the deadline, not one dropped packet, decides abandonment. But
			// RECORD it: this is the only trace a dead node leaves.
			lastPollErr = perr
			pollFails.note(perr)
		case status == http.StatusNotFound:
			// The node answered — with "I have no such job". That is the
			// lost-ack shape AND a positive denial of ownership, so it sets
			// sawNodeAnswer (reachable) and never sawJobOwned: at the deadline
			// this must read as a failure, not as a node that "accepted the job".
			// Re-dispatch the SAME id — the store's duplicate path re-acks
			// 202 without a second run — bounded so a state-losing node
			// cannot be made to re-run the contract forever.
			sawNodeAnswer, saw404 = true, true
			// LOGGED (bounded by the shape cap) because this arm recorded
			// nothing at all: a node that keeps losing jobs left no trace
			// anywhere except a dispatch counter nobody prints.
			pollFails.note(fmt.Errorf("poll: 404 — the node denies ever holding job %s", jobID))
			if redispatches >= maxRedispatches {
				pr.Err = fmt.Sprintf("node lost job %s %d times (poll 404 after re-dispatch)", jobID, redispatches+1)
				pr.nodeTerminal = true
				return pr
			}
			redispatches++
			// A refusal HERE is deliberately NOT marked re-placeable. This node
			// already acked this job id once; its 404 denies holding it NOW,
			// which is not a promise it never ran it and never will. Moving the
			// contract to a second node from inside the polling window is the
			// one shape that could arrange two concurrent runs, and no amount of
			// saved work is worth buying that.
			if _, _, err := r.dispatch(ctx, base, jobID, payload); err != nil {
				pr.Err = err.Error()
				return pr
			}
		case status == http.StatusUnauthorized:
			pr.Err = "poll: 401 unauthorized (fleet_auth_token mismatch)"
			// A refusal of THIS process's credentials is a fact about the caller, not
			// about the job (ADR 0064, decision 6): the node may still hold or finish
			// it, so the intent stays open for a process holding the right token. No
			// withdraw is sent — it would carry the token that was just refused.
			pr.orphanable = true
			return pr
		case status == http.StatusOK && state == "done":
			sawNodeAnswer, sawJobOwned = true, true
			var wire core.AgentWireResult
			if uerr := json.Unmarshal(data, &wire); uerr != nil {
				pr.Err = "job done but data is not an AgentWireResult: " + uerr.Error()
				pr.nodeTerminal = true
				return pr
			}
			pr.Result = wire
			pr.Node = wire.NodeID
			pr.Seat = wire.Seat
			if !wire.Deferred {
				pr.AcceptanceFailures = EvalAcceptance(contract, wire)
				r.strikeOnFingerprint(base, pr.AcceptanceFailures)
			}
			return pr
		case status == http.StatusOK && state == "error":
			pr.Err = "remote job error: " + jobErr
			pr.nodeTerminal = true
			if neverRan(jobErr) {
				// The node took this job out of its backlog without running it: reaped
				// because nobody polled it within the poll lease (this process was away
				// for longer — a suspended host, a partition), or withdrawn. Nothing ran,
				// so offering the subtask to another node cannot arrange a double run: it
				// is filed as the capacity refusal a confirmed withdrawal is, and the
				// re-placement machinery takes it from here (ADR 0064).
				pr.refuseAsNeverRan(jobErr, queuedCredit)
			}
			return pr
		case status == http.StatusOK && (state == "accepted" || state == "running"):
			// The node answered AND says it owns the job: the only shape that
			// earns a defer at the deadline.
			sawNodeAnswer, sawJobOwned = true, true
			lastState = state
			// `accepted` and `running` are no longer the same fact. Since
			// 0.100.0 `accepted` means ADMITTED BUT NOT STARTED — the job is in
			// the node's backlog waiting for one of its concurrency slots — and
			// `running` means a seat is actually working on it. A node too old
			// to have a queue simply never lingers in `accepted`, so it accrues
			// no credit and behaves exactly as it did before.
			if state == "accepted" {
				queuedPolls++
				if !refreshed && refreshTries < maxQueueRefreshTries {
					// The job is sitting in the node's backlog, so the ETA the wait was
					// derived from is what matters now - and it may be a snapshot minutes old
					// (route=spread deals once, at the start of the run). Read the node again
					// and EXTEND the wait when its ETA is longer than the snapshot said; a
					// shorter one changes nothing (the job will simply start sooner). One GET
					// per attempt, only for a job that is actually queued, and it fails open to
					// the snapshot - but not silently: a failed read is counted and logged with
					// the poll failures, tried again on the next queued poll (up to
					// maxQueueRefreshTries), and named in the queue-deadline message if it
					// never succeeded, because the budget the job is then abandoned at is one
					// nobody re-checked.
					refreshTries++
					rctx, cancelRead := context.WithTimeout(ctx, pollRequestTimeout)
					fresh, ferr := FetchNodeView(rctx, base, r.cfg.FleetAuthToken)
					cancelRead()
					if ferr == nil {
						refreshed, refreshErr = true, nil
						queueView = fresh
						queuedWaitBudget = max(queuedWaitBudget, queueBudgetFor(queueView, pollBudget))
					} else {
						refreshErr = ferr
						pollFails.note(fmt.Errorf("queue-budget refresh: %w", ferr))
					}
				}
				now := time.Now()
				if !prevQueuedAt.IsZero() {
					queuedCredit += now.Sub(prevQueuedAt)
					if queuedCredit > queuedWaitBudget {
						queuedCredit = queuedWaitBudget
					}
					// Push the execution deadline out by the wait. Recomputed
					// from the anchor rather than incremented, so a capped
					// credit cannot drift the deadline past the intended bound.
					setDeadline()
				}
				lastQueuedAt = now
				if queuedCredit >= queuedWaitBudget {
					// Bounded give-up while STILL QUEUED. Deliberately an
					// ERROR, not a defer, for three reasons — and note that
					// "a defer carries the node id and seat" is NOT among them:
					// runOne stamps those onto the failure path too.
					//
					//  1. A defer manufactures an AgentWireResult{Deferred:
					//     true} — a payload shaped like something the SEAT
					//     produced. No seat ever saw this contract.
					//  2. The only honest class would be `budget`, which
					//     teaches every consumer of that class (the sizing
					//     path especially) that the seat needed more time.
					//     The seat needed nothing; it was never asked.
					//  3. Decisive: what this replaces was ALREADY A FAILURE.
					//     Before the node had a queue, a saturated node
					//     answered 503 at dispatch, which becomes pr.Err and
					//     counts in summary.failed. Filing this as a defer
					//     would quietly downgrade the severity of a refusal
					//     while changing nothing about the work not getting
					//     done.
					//
					// The message says only what was observed: the node acked
					// it, and N polls answered `accepted`. The "queue deadline"
					// prefix is stable and distinct from the "poll deadline"
					// one a job that actually ran produces.
					//
					// Before giving up, ask the node to take the job back (ADR 0064):
					// a job left in its backlog runs later on a seat nobody is waiting
					// for. One try, and one only — a `running` answer is final for
					// this job.
					verdict := withdrawUnconfirmed
					if !ask.tried {
						ask.tried = true
						verdict, ask.why = r.withdraw(ctx, base, jobID)
					}
					if verdict != withdrawStarted {
						// The deadline is the ETA-derived budget above (ADR 0063), and its refresh note
						// says when that budget was never re-checked. It comes BEFORE the withdraw clause:
						// how the wait was sized, then what was done at its end, so a row that says the
						// node did not take the job back still ENDS with that clause (ADR 0064).
						pr.Err = fmt.Sprintf("queue deadline after %s: the node accepted the job but never started it — it waited in the node's backlog and never reached running (%d poll(s) answered `accepted`)",
							queuedWaitBudget, queuedPolls) + queueRefreshNote(refreshErr, refreshTries)
						if verdict == withdrawConfirmed {
							// The node took it back: it will never run there, so the
							// subtask may be offered to another node, and the intent
							// has nothing left for recovery to collect.
							pr.Err += "; the job was withdrawn from the node, which will never run it"
							pr.refuseAsWithdrawn(queuedCredit)
						} else {
							// Still queued on the node — it may run later; recovery's case. The
							// row says what the node answered instead of taking it back: an old
							// node with no route and an upgraded node that refused would
							// otherwise read alike.
							if note := notConfirmed(ask.why); note != "" {
								pr.Err += "; " + note
							}
							pr.orphanable = true
						}
						return pr
					}
					// `running`: the job left the backlog between the last poll and the
					// withdraw — a slot took it. It is not abandoned; keep polling and
					// let liveness govern from here. The node's answer stays in ask: a
					// give-up that comes before the next poll reads it, and never asks
					// again.
				}
			}
			// `running`: the span stays closed by the reset above. Anything
			// already banked stays banked — the job earned that credit.
			// A healthy answer RETIRES the previous failure. lastPollErr was
			// assigned and never cleared, so ONE early 503 followed by fifty
			// clean `running` answers still ended classed infrastructure, exited
			// non-zero, and quoted an error fifty polls stale — contradicting
			// the deadline branch's own comment about a node that "answered
			// every poll normally". The pollFails counter keeps the history.
			lastPollErr = nil
		default:
			// Anything else IS an answer we cannot act on — a 500/502/503 from
			// the node or something in front of it, or a 200 carrying a state
			// this delegator does not know. Something answered, so this is not
			// the never-answered shape; keep polling (the state may still
			// resolve) but record it, because silently discarding it is what let
			// a broken node look like a busy one. It does NOT prove ownership:
			// a 503 from a proxy says nothing about what the node holds.
			sawNodeAnswer = true
			lastPollErr = fmt.Errorf("poll: unusable answer (status %d, state %q)", status, state)
			pollFails.note(lastPollErr)
		}
		select {
		case <-ctx.Done():
			pr.Err = "canceled: " + ctx.Err().Error()
			// A give-up like the one at the top of the loop, and it used to skip the
			// orphanable mark: a cancel that landed while the delegator slept closed
			// its intent as "terminal observed" for a job the node may still run.
			if note := r.giveUp(ctx, base, jobID, lastState, ask, &pr); note != "" {
				pr.Err += "; " + note
			}
			return pr
		case <-time.After(jitteredWithin(pollEvery, time.Until(deadline))):
		}
	}
}

// maxQueueRefreshTries bounds the reads of a queued job's node for its queue budget.
const maxQueueRefreshTries = 3

// queueRefreshNote says, on a queue deadline, that the queue budget the job was abandoned
// at was derived from the placement snapshot because the node's health could not be read
// again once the job was seen queued (and why). "" when the read succeeded or was never
// needed. A safeguard that did not run must leave a trace where its absence matters.
func queueRefreshNote(err error, tries int) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("; the queue budget was derived from the placement snapshot: the node's health could not be read again once the job was seen queued (%d attempt(s), last: %v)", tries, err)
}

// queuedNote renders the backlog credit for a deadline message, and renders
// NOTHING when there was none. The empty case is load-bearing: a node with no
// queue (or a job that started at once) must produce the exact message it
// produced before this accounting existed, so an operator grepping for the old
// wording — and the tests pinning it — still match. When it IS non-empty it
// answers the first question a deadline raises on a queued fleet: was this job
// slow, or was it merely late to start?
// boundNote renders the auto poll bound for a deadline message, and NOTHING
// when there is none — a contract that named its own timeout_sec must produce
// the exact message it produced before D-116, so an operator grepping the old
// wording (and the tests pinning it) still match.
// slack, when non-zero, is the admission allowance still carried in the budget
// — the node's pre-wall window. Naming it is the difference between an
// operator reading "the node had 300 s of wall" and reading the number the
// clock was actually held to; it disappears from the message exactly when it
// disappears from the budget (the node published a started wall).
func boundNote(note string, slack time.Duration) string {
	if note == "" {
		return ""
	}
	if slack > 0 {
		return fmt.Sprintf(" (poll bound: %s, + %s allowed for the node's admission before its wall starts)", note, slack)
	}
	return " (poll bound: " + note + ")"
}

func queuedNote(queuedCredit time.Duration) string {
	if queuedCredit <= 0 {
		return ""
	}
	return fmt.Sprintf(" (+%s credited back for time queued on the node)", queuedCredit.Round(time.Millisecond))
}

// errText renders a possibly-nil error for an operator-facing message. A
// never-answered node with no recorded error means the deadline was already
// spent before the first poll — say that, don't print "<nil>".
func errText(err error) string {
	if err == nil {
		return "no poll completed before the deadline"
	}
	return err.Error()
}

// unownedDetail renders WHY a poll deadline produced a FAILURE rather than a
// defer. THREE shapes, because they send an operator to three different places
// — and because the delegator may only ever report what a node actually said:
//
//	never answered      → a dead box, a wrong address, dropped connections.
//	answered with a 404 → a POSITIVE denial that it ever held the work, which no
//	                      amount of waiting turns into a result and which
//	                      re-dispatching only re-asks.
//	answered otherwise  → reachable, and it said nothing about this job either
//	                      way: a 5xx from it or a proxy, or a 200 carrying a
//	                      state this delegator does not know (a newer peer's
//	                      "queued").
//
// The third shape used to print the SECOND one's sentence: a node answering only
// 503s published "a poll 404 DENIES it ever held it (0 re-dispatch(es) made)" —
// a denial that never happened, invented inside the very message that was
// written to stop the delegator authoring claims for nodes.
func unownedDetail(sawNodeAnswer, saw404 bool, redispatches int, lastPollErr error) string {
	switch {
	case !sawNodeAnswer:
		return fmt.Sprintf("node never answered (last: %s)", errText(lastPollErr))
	case saw404:
		return fmt.Sprintf("the node answered but never reported owning the job — a poll 404 DENIES it ever held it (%d re-dispatch(es) made)%s",
			redispatches, lastErrSuffix(lastPollErr))
	default:
		return fmt.Sprintf("the node answered but never reported owning the job, and never denied holding it either — no poll returned a state this delegator can act on%s",
			lastErrSuffix(lastPollErr))
	}
}

// lastErrSuffix appends the newest UNRETIRED poll failure when there is one, and
// nothing when there is not: "last: no poll completed before the deadline" beside
// a stack of 404 answers reads as a contradiction of the sentence it follows.
func lastErrSuffix(err error) string {
	if err == nil {
		return ""
	}
	return "; last: " + err.Error()
}

// pollFailShapeCap bounds how many DISTINCT failure texts one job LOGS in full
// and enumerates in its summary. A node whose error text varies per poll (a
// changing upstream port, a rotating request id) must not turn the log into an
// unbounded transcript of its own. It does NOT bound the tally: every occurrence
// is counted whatever its shape (see note).
const pollFailShapeCap = 8

// pollFailLog bounds one job's poll-failure logging. Both failure arms in
// runRemote used to log once PER POLL — in the very commit that introduced
// sync.Once because unbounded per-subtask logging buries the results it warns
// about (see warnCorpus/warnLedger). Measured: 53 lines for ONE subtask at the
// compressed test cadence, ~120 in production, ~1000 for an 8-way fan-out at a
// dead node. So: the FIRST occurrence of each distinct shape logs in full, every
// occurrence is counted, and ONE summary line reports the totals on the way out.
type pollFailLog struct {
	jobID, base string
	counts      map[string]int
	order       []string // first-seen order, so the summary reads chronologically
	total       int
}

func newPollFailLog(jobID, base string) *pollFailLog {
	return &pollFailLog{jobID: jobID, base: base, counts: map[string]int{}}
}

// note records one failure, logging it only the first time its shape appears.
// The count is taken BEFORE the cap check: the early return used to fire first,
// so every occurrence of the 9th and later shapes was dropped from the tally
// entirely — 36 failures rendered as a 24-occurrence breakdown, presented as
// complete, with the text of those shapes appearing nowhere at all. The map is
// bounded in practice by the number of polls the deadline allows.
func (p *pollFailLog) note(err error) {
	p.total++
	shape := err.Error()
	_, known := p.counts[shape]
	p.counts[shape]++
	if known || len(p.order) >= pollFailShapeCap {
		return // counted; summarize() reports the omission and its residual
	}
	p.order = append(p.order, shape)
	log.Printf("delegate: poll %s at %s failed: %v", p.jobID, p.base, err)
}

// summarize emits the single end-of-poll line. Silent when nothing failed, so a
// healthy job's output is unchanged. Shapes past the cap are not enumerated —
// that is the cap's whole point — but their COUNT is stated, so the breakdown
// can never again read as complete when it is not.
func (p *pollFailLog) summarize() {
	if p.total == 0 {
		return
	}
	parts := make([]string, 0, len(p.order)+1)
	enumerated := 0
	for _, shape := range p.order {
		parts = append(parts, fmt.Sprintf("%s ×%d", shape, p.counts[shape]))
		enumerated += p.counts[shape]
	}
	if omitted := len(p.counts) - len(p.order); omitted > 0 {
		parts = append(parts, fmt.Sprintf("%d further shape(s) omitted past the log cap ×%d", omitted, p.total-enumerated))
	}
	log.Printf("delegate: poll %s at %s: %d failed poll(s) this job (%s)", p.jobID, p.base, p.total, strings.Join(parts, "; "))
}

// dispatchResult is a node's answer to one dispatch POST — dispatchDetailed's
// return shape (see its own doc for what each field means and when).
type dispatchResult struct {
	refused bool
	status  int
	// retryAfterSec is the node's `Retry-After` header on a 503 (capacity)
	// refusal — a CAPACITY hint, not a promise, so the caller treats it as
	// advisory. 0 when absent or unparsable, or on any status but 503.
	retryAfterSec int
	err           error
}

// dispatch is dispatchDetailed without the Retry-After hint — the plain
// 3-value shape every caller but runRemote uses. runRemote reads the hint
// itself (PlacedResult.retryAfterSec) and files it as the node's cooldown;
// nothing sleeps on it (ADR 0063).
func (r *runner) dispatch(ctx context.Context, base, jobID string, payload json.RawMessage) (refused bool, status int, err error) {
	res := r.dispatchDetailed(ctx, base, jobID, payload)
	return res.refused, res.status, res.err
}

// dispatchDetailed POSTs the job envelope, expecting the contract's one
// acceptance shape (202). A transport-level failure is retried ONCE with the
// same job id — if the first POST actually landed, the node's known-job path
// re-acks idempotently, so the retry can never buy a second run. That bounded
// retry (dispatchAttempts) is about DOUBT over one node's transport and is
// entirely separate from re-placement, which is about a node that answered
// no.
//
// The result says which of three things happened:
//
//	err == nil                     the node acked 202.
//	err != nil, refused == true    the node DECLINED (status = what it sent) or
//	                               could not be reached at all (status = 0).
//	                               placeAndRun classifies it; nothing here
//	                               decides routing.
//	err != nil, refused == false   a DELEGATOR-side fault (an envelope that
//	                               will not marshal, a request that will not
//	                               build). No node said anything, so there is
//	                               nothing for another node to say differently.
func (r *runner) dispatchDetailed(ctx context.Context, base, jobID string, payload json.RawMessage) dispatchResult {
	envelope := map[string]any{
		"job_id":    jobID,
		"task_type": string(core.TaskAgentRun),
		"payload":   payload,
	}
	// Scheduling band (0.113.18) rides the envelope's contract-reserved
	// `priority` — a field every node since v2 accepts (older ones ignore it),
	// sent only when non-zero so a band-0 run's bytes are unchanged. The tenant
	// rides a header (fleetnode.TenantHeader) because the node's envelope decode
	// rejects unknown FIELDS, and a new one would 400 on every node a release
	// behind; a header is ignored by nodes that do not read it.
	if r.priority != 0 {
		envelope["priority"] = r.priority
	}
	env, merr := json.Marshal(envelope)
	if merr != nil {
		return dispatchResult{err: fmt.Errorf("marshaling dispatch envelope: %w", merr)}
	}
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/dispatch"
	var lastErr error
	for attempt := 0; attempt < dispatchAttempts; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, dispatchRequestTimeout)
		req, rerr := http.NewRequestWithContext(rctx, http.MethodPost, u, bytes.NewReader(env))
		if rerr != nil {
			cancel()
			return dispatchResult{err: fmt.Errorf("dispatch request: %w", rerr)}
		}
		req.Header.Set("Content-Type", "application/json")
		if r.cfg.FleetAuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+r.cfg.FleetAuthToken)
		}
		if r.tenant != "" {
			req.Header.Set(core.TenantHeader, r.tenant)
		}
		// Who asked, and whether the serving node must card the job because this box will not
		// (D7/D11): a header, for the same reason the tenant is one.
		r.pair.SetWireHeaders(req.Header)
		resp, derr := fleetClient.Do(req)
		if derr != nil {
			cancel()
			lastErr = fmt.Errorf("dispatch %s: %w", u, derr)
			continue // transport doubt → one more POST, same job id
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
		retryAfter := 0
		if resp.StatusCode == http.StatusServiceUnavailable {
			retryAfter = parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
		}
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusAccepted {
			// A non-202 is the node REFUSING (400/401/403/409/503) — an
			// answer, not doubt; re-POSTing the same bytes to the SAME node
			// would get the same answer. Whether ANOTHER node is worth asking
			// is replaceableRefusal's decision, made from this status.
			return dispatchResult{refused: true, status: resp.StatusCode, retryAfterSec: retryAfter,
				err: fmt.Errorf("dispatch %s: status %d: %s", u, resp.StatusCode, truncate(body, 256))}
		}
		return dispatchResult{status: resp.StatusCode}
	}
	// Both POSTs failed at transport level: the delegator never reached this
	// node. Status 0 says exactly that — no node authored this answer.
	return dispatchResult{refused: true, err: lastErr}
}

// parseRetryAfterSeconds reads a `Retry-After` header's DELTA-SECONDS form
// (the shape this fleet's own nodes send — never the HTTP-date form, which
// this parser deliberately does not attempt). Empty, unparsable or negative
// is 0 = no hint, the safe "the caller decides its own pacing" reading.
// retryAfterUnparsableWarnOnce bounds parseRetryAfterSeconds' log to ONCE per
// process — review round 1, LOW item 6: a future proxy in front of a node
// could start sending the HTTP-date form (RFC 9110 §10.2.3), which this
// parser deliberately does not attempt (this fleet's own nodes only ever
// send delta-seconds), and that would otherwise silently discard the hint on
// every single 503 forever with no trace anywhere.
var retryAfterUnparsableWarnOnce sync.Once

func parseRetryAfterSeconds(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		retryAfterUnparsableWarnOnce.Do(func() {
			log.Printf("delegate: Retry-After %q is not a delta-seconds integer (an HTTP-date form is not parsed); ignoring it as a capacity hint — logged once per process", v)
		})
		return 0
	}
	return n
}

// pollWaitSec is what runRemote's poll asks a node to hold ONE connection
// open for (?wait=12) before answering (item 7): at fleetnode.MaxJobWaitSec,
// one second inside pollRequestTimeout (15 s) so the long poll always gets an
// answer before the client gives up on the exchange — a pairing pinned on
// the node side by a test that can read this package's own source. An older
// node (pre-0.127) does not read the parameter at all and answers at once,
// exactly as before: the fallback IS the parameter being a no-op there, not
// a second code path — the existing pollEvery sleep between polls is
// unchanged either way.
const pollWaitSec = 12

// pollOnce GETs the job state, long-polling up to pollWaitSec when the node
// supports it (`?wait=`). The error covers transport-level failure only; an
// HTTP answer (any status) comes back as a jobPoll with a nil error.
func (r *runner) pollOnce(ctx context.Context, base, jobID string) (jobPoll, error) {
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/jobs/" + jobID + fmt.Sprintf("?wait=%d", pollWaitSec)
	return pollJobOnceAt(ctx, r.cfg, u)
}

// EvalAcceptance runs every contract acceptance check against the result —
// DELEGATOR-side, before merge (roast delta 3). An unparseable check fails
// closed with the parse error as the failure: Validate should have caught it,
// and an unmet precondition is a failed check, never a skipped one.
//
// Exported (package-private until askjob) so the single-seat offload_ask lane,
// which runs its contract through Pipeline.RunAgentContract instead of
// delegate.Run, evaluates acceptance by the SAME rules rather than a second
// copy of them. An acceptance check nothing evaluates is decoration.
func EvalAcceptance(contract core.AgentContract, wire core.AgentWireResult) []string {
	var failures []string
	for _, a := range contract.Acceptance {
		chk, err := core.ParseAcceptanceCheck(a)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if pass, reason := chk.Eval(wire); !pass {
			failures = append(failures, reason)
		}
	}
	return failures
}

// fetchViews probes every configured remote's /fleet/health, returning the
// views, a PARALLEL base-URL slice (NodeView carries no base — placement is
// pure; the pairing is the runner's business), and the probe FAILURES. A node
// that cannot answer is not a candidate — the gate's fail-toward-local posture
// — but the reason it could not answer is the operator's whole diagnosis: a
// wrong token (401), a node that is down (dial refused), and a stale VRAM
// snapshot (503) all end as "not a candidate", and dropping the error made
// them indistinguishable from a node that merely failed the gate.
// strikeOnFingerprint strikes base in the caller's Quarantine when any failed
// acceptance is a DOCUMENT FINGERPRINT check (research.DocFingerprint tags its
// regex with the named group `docanchor`). Contract-caused failures — a
// user's over-strict contains:, a thin page's min_items — never strike.
func (r *runner) strikeOnFingerprint(base string, failures []string) {
	if r.quarantine == nil || !failsDocumentFingerprint(failures) {
		return
	}
	if r.quarantine.Strike(base) {
		r.quarantined.Add(1)
		log.Printf("delegate: node %s quarantined for %s after 2 off-document answers (document fingerprint failed twice)", base, DefaultQuarantineTTL)
	}
}

// probeSnapshot is ONE fleet probe, memoised on the runner (= on the Run) so
// the sibling subtasks of a fan-out share it instead of each paying their own.
type probeSnapshot struct {
	at        time.Time
	views     []NodeView
	bases     []string
	probeErrs []string
	// failed is probeErrs keyed by the base it belongs to. probeErrs is a flat
	// list because that is what every placement reason renders; a caller that
	// has to ACCUMULATE failures across calls (the capacity wait) needs to know
	// which base each one came from, and matching on substrings would be a
	// guess.
	failed map[string]string
}

// deadProbe is one base's last TRANSPORT failure — the negative cache's entry.
type deadProbe struct {
	at  time.Time
	why string
}

// fetchViewsMemoTTL is how long one fleet probe is reused by the rest of a Run;
// probeNegativeTTL is how long a base that failed at the transport is skipped
// rather than re-dialled. Vars so tests can compress them, exactly like
// pollEvery and placementPollInterval; production never mutates them.
//
// 2 s is chosen SHORTER than the SHORTEST GAP BETWEEN TWO TICKS on purpose: the
// capacity wait's whole job is to notice a node that just freed, so the memo
// must never be able to serve one of its ticks.
//
// The comparison is not against placementPollInterval itself. The tick is
// jittered, so the shortest gap is (1 - jitterFrac) x placementPollInterval =
// 2.4 s, and a memo of 2.5 s would read as "safely under the 3 s interval"
// while quietly answering ticks from cache. TestProbeMemoCannotServeACapacity-
// WaitTick pins the real relation over the PRODUCTION values, because every
// capacity-wait test zeroes the memo in order to compress the tick and so none
// of them would ever notice.
//
// 30 s for the negative cache is the other side of the same trade: long enough
// that a fan-out stops paying fetchNodeViewTimeout per subtask for a box that is
// down, short enough that a node rebooting inside one long Run is picked up
// again while the Run still has budget to use it.
var (
	fetchViewsMemoTTL = 2 * time.Second
	probeNegativeTTL  = 30 * time.Second
)

func (r *runner) fetchViews(ctx context.Context) (views []NodeView, bases []string, probeErrs []string) {
	views, bases, probeErrs, _ = r.fetchViewsDetailed(ctx)
	return views, bases, probeErrs
}

// fetchViewsDetailed is fetchViews plus the failures keyed by BASE, for the one
// caller that accumulates them across calls rather than rendering them once.
func (r *runner) fetchViewsDetailed(ctx context.Context) ([]NodeView, []string, []string, map[string]string) {
	return r.fetchViewsDetailedSince(ctx, time.Time{})
}

// fetchViewsSince is fetchViews for a caller that has just learned its picture of
// the fleet was wrong - a re-placement after a refusal: the memo serves it only
// when the snapshot was taken AT OR AFTER notBefore. A snapshot taken after the
// refusal is shared; one that predates it is the stale picture and is never reused.
// Siblings that refuse together are not deduped by this: each one's refusal is newer
// than the probe another began, so each reads the fleet once.
func (r *runner) fetchViewsSince(ctx context.Context, notBefore time.Time) ([]NodeView, []string, []string) {
	views, bases, probeErrs, _ := r.fetchViewsDetailedSince(ctx, notBefore)
	return views, bases, probeErrs
}

func (r *runner) fetchViewsDetailedSince(ctx context.Context, notBefore time.Time) ([]NodeView, []string, []string, map[string]string) {
	if s := r.memoisedProbe(notBefore); s != nil {
		v, b, e := s.copyOut()
		return v, b, e, maps.Clone(s.failed)
	}
	s := &probeSnapshot{at: time.Now()}
	s.views, s.bases, s.probeErrs, s.failed = r.probeRemotes(ctx)
	r.probeMu.Lock()
	r.probeMemo = s
	r.probeMu.Unlock()
	v, b, e := s.copyOut()
	return v, b, e, maps.Clone(s.failed)
}

// copyOut hands every caller its OWN slices. The snapshot is shared by the
// run's subtask goroutines, and callers index views/bases and build derived
// lists from them (untried, Place) — a shared backing array is how one
// subtask's filtering would silently rewrite a sibling's roster.
func (s *probeSnapshot) copyOut() ([]NodeView, []string, []string) {
	return append([]NodeView(nil), s.views...),
		append([]string(nil), s.bases...),
		append([]string(nil), s.probeErrs...)
}

// memoisedProbe returns the run's snapshot while it is inside the memo window,
// else nil. A non-positive TTL switches the memo off — what a test compressing
// the capacity wait's clocks does, because at a 20 ms tick a 2 s memo would
// swallow every re-read the wait exists to make.
func (r *runner) memoisedProbe(notBefore time.Time) *probeSnapshot {
	if fetchViewsMemoTTL <= 0 {
		return nil
	}
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.probeMemo == nil || time.Since(r.probeMemo.at) >= fetchViewsMemoTTL {
		return nil
	}
	if !notBefore.IsZero() && r.probeMemo.at.Before(notBefore) {
		return nil
	}
	return r.probeMemo
}

// recentlyDead answers the negative cache: a base that failed at the transport
// inside probeNegativeTTL is skipped, and the reason it failed is REPLAYED so
// the placement note still names the node and says what happened to it. A
// skipped base that vanished from probeErrs would read as a node nobody ever
// configured — the exact ambiguity fetchViews' error-carrying exists to remove.
func (r *runner) recentlyDead(base string) (string, bool) {
	if probeNegativeTTL <= 0 {
		return "", false
	}
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	d, ok := r.probeDead[base]
	if !ok {
		return "", false
	}
	elapsed := time.Since(d.at)
	if elapsed >= probeNegativeTTL {
		return "", false
	}
	// Elapsed and remaining, not the configured TTL: an operator reading this
	// wants to know how stale the verdict is and when the base is dialled
	// again. Printing the fixed window said neither.
	return fmt.Sprintf("%s (cached %s ago, re-dial in %s)",
		d.why, elapsed.Round(time.Millisecond), (probeNegativeTTL - elapsed).Round(time.Millisecond)), true
}

// noteDeadProbe records a TRANSPORT failure against the base.
//
// Two guards, and both are load-bearing. Only probeUnreachable errors are
// cached: a 503 or a 401 is a node that ANSWERED, and skipping it for 30 s would
// turn a momentary refusal into half a minute of ineligibility. And nothing is
// cached once the CALLER's context is done — the capacity wait's per-tick bound
// fails every base at the same instant, and that is a fact about the tick, never
// about any of the nodes.
func (r *runner) noteDeadProbe(ctx context.Context, base string, err error) {
	if ctx.Err() != nil || !probeUnreachable(err) {
		return
	}
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.probeDead == nil {
		r.probeDead = map[string]deadProbe{}
	}
	r.probeDead[base] = deadProbe{at: time.Now(), why: err.Error()}
}

// probeRemotes is the fan-out itself: every configured remote is probed
// CONCURRENTLY, each bounded by fetchNodeViewTimeout, and the answers are
// reassembled in the CONFIGURED order so views/bases/probeErrs keep the
// positional contract fetchViews has always published. Completion order is not
// an ordering any caller can use — a NodeView carries no base.
func (r *runner) probeRemotes(ctx context.Context) ([]NodeView, []string, []string, map[string]string) {
	type outcome struct {
		view NodeView
		ok   bool
		err  string
	}
	out := make([]outcome, len(r.remotes))
	var wg sync.WaitGroup
	for i, base := range r.remotes {
		if r.quarantine.Blocked(base) {
			// Two fingerprint failures inside the TTL: the node is producing
			// answers about the wrong document. Not a candidate until it expires.
			out[i].err = fmt.Sprintf("%s: quarantined until %s (repeated off-document answers)", base, r.quarantine.Until(base).Format(time.Kitchen))
			continue
		}
		if why, dead := r.recentlyDead(base); dead {
			out[i].err = why
			continue
		}
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			// The per-base bound is the whole point of the fan-out: ONE
			// unreachable remote costs fetchNodeViewTimeout, never N of them in
			// series. A shorter caller ctx (the capacity wait's per-tick bound,
			// a re-placement's remaining budget) still wins.
			cctx, cancel := context.WithTimeout(ctx, fetchNodeViewTimeout)
			defer cancel()
			v, err := FetchNodeView(cctx, base, r.cfg.FleetAuthToken)
			if err != nil {
				// Logged once per BASE per RUN, not once per (base, subtask):
				// fetchViews is called inside runOne, so an 8-subtask fan-out at
				// two dead remotes printed sixteen identical lines. The error
				// still rides probeErrs on every subtask — the defer REASON must
				// name the cause for each result even when the log line is spent.
				if _, warned := r.probeWarned.LoadOrStore(base, struct{}{}); !warned {
					log.Printf("delegate: health probe of %s failed (not a placement candidate): %v", base, err)
				}
				out[i].err = err.Error()
				r.noteDeadProbe(ctx, base, err)
				return
			}
			out[i].view, out[i].ok = v, true
		}(i, base)
	}
	wg.Wait()
	views := make([]NodeView, 0, len(out))
	bases := make([]string, 0, len(out))
	var probeErrs []string
	failed := map[string]string{}
	for i, o := range out {
		switch {
		case o.ok:
			views = append(views, o.view)
			bases = append(bases, r.remotes[i])
		case o.err != "":
			probeErrs = append(probeErrs, o.err)
			failed[r.remotes[i]] = o.err
		}
	}
	return views, bases, probeErrs, failed
}

// noEligibleRemote turns "nothing eligible" into the cause an operator can act
// on, plus the defer class that goes with it. Distinct situations used to share
// one manufactured sentence that always blamed the gate:
//
//	no remotes configured              → config: nothing was ever asked to run this
//	some remote never answered         → infrastructure: the fleet is down/unreachable
//	all answered, none offers the lane → config: the NODE side really is the reason
//	all answered, none advertises a ceiling → config: an operator sets agent_ctx_tokens
//	the node side is demonstrably fine → contract: only then is the CALLER at fault
//
// DEFAULT TO LOUD — the one rule this function now encodes. The contract check
// used to run FIRST, and two of its three conditions had no node-side guard at
// all, so a caller's missing output_schema published a quiet contract-side
// verdict on a run where NOT ONE node had answered: {succeeded:1,
// infrastructure:0} at exit 0 over a fleet that had been unreachable for a week,
// while the identical fleet state WITH a schema counted infrastructure. So the
// order is inverted and the rule is one line: a QUIET class requires POSITIVE
// evidence that the node side is fine (every configured remote answered, at
// least one offers the agent lane, and it advertises a real ceiling). Absence of
// evidence about the fleet is never evidence about the contract. When both are
// true the reason names both and the CLASS is the loud one — a false alarm costs
// an operator one look, a silent failure costs a night.
// reservedDefer is the result for a contract that could only have run on a
// seat a TEXT lease reserves: deferred, class infrastructure (the box needs a
// human's timing decision, not a rewritten contract), the holder named so the
// caller can wait, route elsewhere, or ask. It never runs the contract.
//
// call is non-nil when the call's deadline bounded the wait (ADR 0073): the advice to set
// agent_lease_wait_sec longer would then be wrong, because the wait already ran as long as the
// call could spare.
func (r *runner) reservedDefer(local NodeView, info gpulease.Info, waited time.Duration, why string, call *callWaitInfo) PlacedResult {
	reason := why
	if !strings.Contains(why, "reserved") {
		reason = "local seat reserved (" + HolderLine(info) + "); " + why
	}
	if call != nil {
		reason += fmt.Sprintf("; waited %s (%s) — add a remote, release the lease, or re-run when it ends", waited.Round(time.Second), call.words())
	} else {
		reason += fmt.Sprintf("; waited %s (agent_lease_wait_sec=%d) — set agent_lease_wait_sec to wait longer, add a remote, or release the lease", waited.Round(time.Second), r.cfg.AgentLeaseWaitSec)
	}
	return PlacedResult{
		Node: local.NodeID, Seat: local.AgentSeat,
		PlacementReason: reason,
		Result: core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassInfrastructure,
			Reason:        reason,
		},
	}
}

func (r *runner) noEligibleRemote(st Subtask, views []NodeView, probeErrs []string) (reason, class string) {
	if len(r.remotes) == 0 {
		return "no remote fleet nodes are configured (pass --remote / the remotes argument)", core.DeferClassConfig
	}
	lanes, advertisedTooSmall, roomiest, unadvertised := laneStats(st, views)
	contractWhy := contractIneligible(st, lanes, advertisedTooSmall, roomiest, len(unadvertised))
	nodeWhy, nodeClass := nodeSideVerdict(lanes, unadvertised, views, probeErrs)
	if nodeClass == "" {
		// A remote that answered with a fitting lane but holds a TEXT GPU lease
		// is ineligible by design (0.113.16), not by a bug: name the holder so the
		// caller can wait or route elsewhere. Before 0.113.17 this fell through to
		// the "placement and gate disagree — please report" line (seen live on the
		// first lease, 2026-09-06 17:22).
		if leased := leasedLanes(views); len(leased) > 0 {
			why := fmt.Sprintf("%d remote(s) hold a text GPU lease (their cards are reserved for a measurement): %s", len(leased), strings.Join(leased, "; "))
			if contractWhy != "" {
				why += "; and the contract could not be placed on the others as written: " + contractWhy
			}
			return why, core.DeferClassInfrastructure
		}
		// Composite nodes (ADR 0039): for a remote-shaped contract (schema,
		// origin depth) the table's own verdict per layered node is the
		// reason — a window no layer holds (contract), a guard the node's
		// rows refused or a busy pair the long seat would evict (capacity) —
		// because the single-lane ceiling sentence would name agent_ctx_tokens,
		// which on a composite box is one layer's window, not the box's. The
		// single-lane sentence rides along for the remotes that have no rows.
		// A plain fleet has no layered node and keeps every sentence it had.
		if len(st.Contract.OutputSchema) > 0 && st.Contract.Depth == 0 {
			if why, class := layerVerdicts(st, views); why != "" {
				if contractWhy != "" && anySingleLane(views) {
					why += "; on the remote(s) without layers: " + contractWhy
				}
				return why, class
			}
		}
		// The ONE positively-established quiet case: everything answered, the
		// lane is offered and sized, so the contract is the whole story.
		if contractWhy != "" {
			return contractWhy, core.DeferClassContract
		}
		// A contract that NAMES a layer (register A-100) that no answering remote declares: the name
		// is the story. A remote that publishes no layer rows declares none, and layerVerdicts above
		// speaks only for the ones that publish rows, so without this the line below would report a
		// disagreement between the gate and the placement - a bug in this package - where the cause
		// is the layer the caller chose.
		if layer := st.Contract.Layer; layer != "" && !anyDeclaresLayer(views, layer) {
			return fmt.Sprintf("layer %s requested and no answering remote declares it (%d answered with an agent lane; a node that publishes no layers declares none)", layer, lanes), core.DeferClassContract
		}
		// Defensive: every remote answered, at least one advertises a lane this
		// contract fits, and Place still found nothing. That is a bug in this
		// package, not a caller mistake — say so loudly rather than inventing a
		// contract-side excuse for it.
		return fmt.Sprintf("no remote passed the capability gate although %d answered with an agent lane this contract fits (delegate: placement and gate disagree — please report)", lanes), core.DeferClassConfig
	}
	if contractWhy != "" {
		// Both are true. Name both, class the LOUD one: the box is what needs a
		// human, and a rewritten contract would still have nowhere to go.
		return nodeWhy + "; the contract could not be placed as written either: " + contractWhy, nodeClass
	}
	return nodeWhy, nodeClass
}

// layerVerdicts names, per composite node, why the placement table would not
// place st on its advertised layers, and the class the refusals add up to:
// contract when every layered node deferred as a contract problem (no window
// holds it), capacity when any of them refused on a guard or would have to
// wait for a busy seat. "" when no answering node advertises layers or every
// layered node was eligible.
func layerVerdicts(st Subtask, views []NodeView) (reason, class string) {
	var parts []string
	class = core.DeferClassContract
	for _, v := range views {
		if !v.AgentEnabled || len(v.Layers) == 0 {
			continue
		}
		dec, _ := remoteDecision(st, v)
		switch {
		case dec.Defer:
			parts = append(parts, laneID(v)+": "+dec.Reason)
			if dec.DeferClass != core.DeferClassContract {
				class = core.DeferClassCapacity
			}
		case dec.Wait:
			parts = append(parts, laneID(v)+": "+dec.Reason)
			class = core.DeferClassCapacity
		}
	}
	if len(parts) == 0 {
		return "", ""
	}
	return fmt.Sprintf("no composite remote can place the contract on its layers — %s", strings.Join(parts, "; ")), class
}

// anySingleLane reports whether some agent-lane remote advertises no layer
// rows — the single-lane ceiling sentence is only spoken about those.
func anySingleLane(views []NodeView) bool {
	for _, v := range views {
		if v.AgentEnabled && v.AgentResident && len(v.Layers) == 0 {
			return true
		}
	}
	return false
}

// nodeSideVerdict reports what the NODE SIDE contributes to "nothing eligible".
// An EMPTY class is the one positive statement it can make: every configured
// remote answered, at least one offers the agent lane, and EVERY such lane
// advertises a real context ceiling — the node side is demonstrably fine, which
// is the only state in which a quiet contract-side class is honest.
func nodeSideVerdict(lanes int, unadvertised []string, views []NodeView, probeErrs []string) (reason, class string) {
	switch {
	case len(views) == 0 && len(probeErrs) > 0:
		return fmt.Sprintf("all %d configured remote(s) failed the health probe: %s", len(probeErrs), strings.Join(probeErrs, "; ")), core.DeferClassInfrastructure
	case len(probeErrs) > 0:
		return fmt.Sprintf("no remote passed the capability gate, and %d other remote(s) failed the health probe: %s",
			len(probeErrs), strings.Join(probeErrs, "; ")), core.DeferClassInfrastructure
	case lanes == 0:
		return "no remote passed the capability gate (none is both agent-enabled and roster-resident)", core.DeferClassConfig
	case len(unadvertised) > 0:
		// agent_ctx_tokens is omitempty on the wire and config documents 0 as
		// "not advertised — set it when opting a node in": an OPERATOR fix on a
		// box. A ceiling NOBODY advertised cannot make the caller's contract too
		// big, but an unset value counted into lanes and then into the too-small
		// tally, so a 30-token goal came back as "needs ~3102 tokens, the roomiest
		// remote advertises 0" — quiet, contract-classed, exit 0.
		//
		// WHO ACTUALLY PRODUCES A SILENT LANE (R6): a node running the agent lane
		// with agent_ctx_tokens unset. fleetnode.AgentLaneAdmissible gates the lane
		// on fleet_agent_enabled + a resolvable planner seat + a safely reachable
		// listener — never on a ceiling — and health advertises whatever is
		// configured, 0 included. It is NOT a peer predating the lane: such a peer
		// sends no agent_enabled either, decodes as AgentEnabled:false, and is
		// filtered out of `lanes` before this branch can see it. Getting that
		// wrong matters here because it is the whole justification for the class:
		// the reachable state is a fleet where one operator set the field and
		// another did not, and the fix is on the box that did not.
		//
		// PER-LANE, not fleet-wide (R5): this fires on ANY silent lane, not only
		// on a fleet where every lane is silent. The predecessor keyed off
		// `roomiest == 0`, a fleet-wide MAX, so one node with a real ceiling
		// supplied one for every peer that had published none, and that mixed
		// fleet fell straight through to the quiet class. UNKNOWN is not SMALL: a
		// silent node may be a 128k box, and the fix for it is on that box, so the
		// class is loud and the reason names it. The ctx-fit sentence still rides
		// along when every ADVERTISED lane is too small (contractIneligible) —
		// both causes are true and the operator is owed both.
		return fmt.Sprintf("%d of %d agent-enabled remote(s) advertise no context ceiling (agent_ctx_tokens is unset or 0 on %s — an operator sets it on the node), and an unadvertised ceiling can never satisfy the placement gate",
			len(unadvertised), lanes, strings.Join(unadvertised, ", ")), core.DeferClassConfig
	}
	return "", ""
}

// laneStats summarizes the ANSWERING remotes' agent lane for this contract: how
// many offer it at all (agent-enabled AND roster-resident), how many of the ones
// that ADVERTISED a ceiling are too small for it, the roomiest ceiling any of
// them advertises, and the ids of the ones that advertise NO ceiling at all.
//
// advertisedTooSmall deliberately excludes the silent lanes rather than counting
// them (`est+reserve > 0` is trivially true, so they used to inflate it): UNKNOWN
// is not SMALL, and the only honest ceiling arithmetic is over numbers the nodes
// themselves sent. It is what lets the ctx-fit sentence be spoken about the
// advertised half of a mixed fleet without claiming anything about the other.
//
// unadvertised is a list rather than a count because it is the operator's
// worklist: "some node did not publish agent_ctx_tokens" is unactionable across
// a fleet, "node-b2 did not" is one ssh away.
func laneStats(st Subtask, views []NodeView) (lanes, advertisedTooSmall, roomiest int, unadvertised []string) {
	for _, v := range views {
		if !v.AgentEnabled || !v.AgentResident {
			continue
		}
		lanes++
		if v.AgentCtxTokens == 0 {
			unadvertised = append(unadvertised, laneID(v))
			continue
		}
		if v.AgentCtxTokens > roomiest {
			roomiest = v.AgentCtxTokens
		}
		if st.EstTokens+specReserve > v.AgentCtxTokens {
			advertisedTooSmall++
		}
	}
	return lanes, advertisedTooSmall, roomiest, unadvertised
}

// leasedLanes names the agent-lane remotes whose health advertised a held TEXT
// lease (NodeView.LeasedText): eligible by every other measure, reserved by
// choice. The lease holder's details live on the node (its /fleet/health
// "lease"), so the name is what a caller needs to go and look.
func leasedLanes(views []NodeView) []string {
	var out []string
	for _, v := range views {
		switch {
		case !v.AgentEnabled || !v.AgentResident:
		case v.LeasedText:
			out = append(out, laneID(v)+" (text lease held)")
		case v.LeaseBusy:
			// 0.113.27: a long lease of any class refuses too, and an
			// operator reading "why did nothing land there" needs the same
			// sentence for it as for a text lease.
			out = append(out, laneID(v)+" (long GPU lease held)")
		case v.LeaseOverdue:
			// GPU routing P1: the node is ranked last, not excluded, so it is not
			// why a placement failed; it is named so an operator who sees work
			// land on the worse node knows the lease behind the order.
			out = append(out, laneID(v)+" (GPU lease overdue, ranked last)")
		}
	}
	return out
}

// laneID names a node for an operator-facing message. A node that answered
// health without a node_id gets a shape that reads as MISSING rather than a
// fabricated name — the delegator reports what a node said, never more.
func laneID(v NodeView) string {
	if v.NodeID == "" {
		return "(a remote that reported no node_id)"
	}
	return v.NodeID
}

// contractIneligible names the CALLER'S CONTRACT property that makes every
// remote ineligible, or "" when none does. It reports WHICH property; whether
// the contract is the CLASS is noEligibleRemote's decision, and only ever when
// the node side is positively established as fine.
//
// Three of remoteEligible's five conditions are properties of the contract, not
// of any node: a missing OutputSchema (legal per core.AgentContract.Validate,
// and legal for a LOCAL run), a Depth past the origin hop, and a token estimate
// no advertised ceiling can hold. The test that separates them from a node
// problem is WHO CAN FIX IT: these three are fixed by rewriting the contract,
// without touching a box.
func contractIneligible(st Subtask, lanes, advertisedTooSmall, roomiest, unadvertised int) string {
	if len(st.Contract.OutputSchema) == 0 {
		return "the contract carries no output_schema, which REMOTE placement requires — the delegator must hold a mechanical check before it merges a weak node's output (a schemaless contract is still legal locally)"
	}
	if st.Contract.Depth != 0 {
		return fmt.Sprintf("the contract is already at depth %d; only an ORIGIN contract (depth 0) may travel — hop limit 1", st.Contract.Depth)
	}
	// Ctx-fit needs a real number to be too big for, so it is spoken only over the
	// lanes that ADVERTISED one, and every one of those must be too small. Silent
	// lanes are excluded from both sides of that test (laneStats keeps them out of
	// advertisedTooSmall): their ceiling is UNKNOWN, and no verdict may be built
	// on an unknown.
	//
	// Whether this makes the CLASS `contract` is still noEligibleRemote's call and
	// still requires a positively-fine node side — with a silent lane present the
	// class stays LOUD and this sentence rides along as the second cause. That
	// composition is the R6 fix: the predecessor keyed the whole sentence off
	// `unadvertised == 0`, so on a mixed fleet it was SUPPRESSED entirely and the
	// operator was told to set agent_ctx_tokens on one box, fixed it, and only
	// then discovered on the next run that the contract does not fit the other
	// box's advertised ceiling either. Two true causes, one reported.
	//
	// The WORDING carries the scope, because the suppression was not gratuitous:
	// "the roomiest agent-enabled remote advertises 4096" is a fleet-wide MAX
	// claim, and over a fleet with a silent lane it implies that lane is SMALLER —
	// authoring a ceiling for a node that published none (docs: unknown, not
	// small; it may be a 128k box), the same defect class as the invented 404
	// denial a previous round removed. "every remote that DID advertise a ceiling
	// tops out at N" says exactly what was measured and nothing about the rest.
	advertised := lanes - unadvertised
	if advertised > 0 && advertisedTooSmall == advertised {
		ceiling := fmt.Sprintf("the roomiest agent-enabled remote advertises %d", roomiest)
		if unadvertised > 0 {
			ceiling = fmt.Sprintf("every remote that DID advertise a ceiling tops out at %d", roomiest)
		}
		return fmt.Sprintf("the contract needs ~%d context tokens (%d estimated + %d reserved for the loop) and %s",
			st.EstTokens+specReserve, st.EstTokens, specReserve, ceiling)
	}
	return ""
}

// localNodeID mirrors runAgentTask's rule: configured fleet_node_id, else the
// OS hostname, so a shared config never bakes one box's name into another's
// results.
func (r *runner) localNodeID() string {
	// A "local" run against another box's engine (a bench config whose
	// endpoint is <node-c>'s arm) is that box's work: name the node after
	// the endpoint host, as a remote placement is named after its base
	// (register C-58: PAIR showed <node-b> doing <node-c>'s work).
	if host := modelaffinity.EndpointHost(r.cfg.Endpoint, r.cfg.FleetNodeID); host != "" {
		return host
	}
	if r.cfg.FleetNodeID != "" {
		return r.cfg.FleetNodeID
	}
	if hn, err := os.Hostname(); err == nil {
		return hn
	}
	return "local"
}

// mintJobID mints the delegator-owned job id: "agd-" + 24 hex chars from
// crypto/rand (roast delta 14; no new deps — hex over ULID). crypto/rand
// failure falls back to a nanotime suffix rather than panicking: a weaker id
// still dedupes correctly against this delegator's own re-dispatches.
func mintJobID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("agd-t%d", time.Now().UnixNano())
	}
	return "agd-" + hex.EncodeToString(b[:])
}

// ---- telemetry (roast delta 9) ----

// delegationLogMu serializes in-process corpus appends. Unlike the ledger's
// small rows, a corpus line can run to hundreds of KiB (it carries the whole
// contract), so O_APPEND atomicity alone cannot be trusted to keep concurrent
// goroutines' lines from interleaving.
var delegationLogMu sync.Mutex

// delegationLogLine is one delegation-log corpus record: the spec'd telemetry
// keys plus the full (contract, result, acceptance) tuple — the standing
// small-model agent-task corpus (no sub-27B agent data exists anywhere; this
// accumulates it from real work).
type delegationLogLine struct {
	TS                 int64                 `json:"ts"`
	JobID              string                `json:"job_id"`
	Node               string                `json:"node"`
	Seat               string                `json:"seat"`
	PlacementReason    string                `json:"placement_reason"`
	Deferred           bool                  `json:"deferred"`
	DeferClass         string                `json:"defer_class,omitempty"`
	AcceptancePass     bool                  `json:"acceptance_pass"`
	WallMs             int64                 `json:"wall_ms"`
	EstTokens          int                   `json:"est_tokens"`
	Error              string                `json:"error,omitempty"`
	Contract           core.AgentContract    `json:"contract"`
	Result             *core.AgentWireResult `json:"result,omitempty"`
	AcceptanceFailures []string              `json:"acceptance_failures,omitempty"`
	// Arm labels which experimental arm produced this row.
	//
	// This is an ENABLER, not a convenience. The delegation log is append-only and is
	// written CONCURRENTLY by whatever sessions are running -- during one review pass it
	// grew 20 -> 24 rows, and some of those new rows were themselves a cross-seat replay.
	// Once arms are interleaved in one file with no label, they cannot be separated after
	// the fact: timestamps do not distinguish an experiment from ordinary traffic.
	//
	// Set from OFFLOAD_DELEGATE_ARM. Empty (omitted) for ordinary traffic, which is what
	// makes the field safe to add: existing rows and unlabelled runs read as "not part of
	// any arm" rather than as a missing value.
	//
	// NOTE the rejected alternative: pointing the delegation log at a scratch directory to
	// isolate a run. BaseDir() is the single state root for cache.db, ledger.jsonl, media,
	// exemplars, router weights and confhead labels -- relocating it would run the
	// experiment against an empty cache, which changes the thing being measured.
	Arm string `json:"arm,omitempty"`
	// DelegatorVersion / DelegatorBuildSHA256 pin the DELEGATOR-side code that
	// produced this row (A1, 0.81.0): acceptance evaluation, retry policy and
	// placement all run here, so a corpus row is only pairable with another
	// when BOTH ends are pinned — the node's end travels inside Result
	// (harness_version / harness_build_sha256 / seat_config_*), this is the
	// other end. Omitempty: pre-0.81 rows read as unknown, never as a value.
	DelegatorVersion     string `json:"delegator_version,omitempty"`
	DelegatorBuildSHA256 string `json:"delegator_build_sha256,omitempty"`
	// Placed (ADR 0039) is the placement decision the row was produced under
	// (PlacedResult.Placed). Omitempty: a plain box's corpus row is unchanged,
	// and a pre-0.116 row reads as "one implicit layer", never as unplaced.
	Placed *core.Placed `json:"placed,omitempty"`
}

// record writes one subtask's telemetry: the delegation-log corpus line and a
// ledger row (task=agent_delegate). Best-effort by design — telemetry must
// never fail the work it describes (pipeline.record's exact posture).
func (r *runner) record(contract core.AgentContract, pr PlacedResult) {
	// The row says what the caller pinned (ADR 0078): the hint clause opens the placement note, so the ledger's
	// short form of it (120 bytes) keeps it, and the closed reason and the asked route are columns of their own.
	// Only the note is read from the stamped copy: the reason code is decided from the result as the engine made it
	// (reasonCodeFor matches the placement text "refused before placement" exactly).
	placement := r.stampPin(pr).PlacementReason
	pinReason, routeAsked := r.pinLedgerFields()
	line := delegationLogLine{
		TS:                 time.Now().Unix(),
		JobID:              pr.JobID,
		Node:               pr.Node,
		Seat:               pr.Seat,
		PlacementReason:    placement,
		Deferred:           pr.Result.Deferred,
		DeferClass:         pr.Result.DeferClass,
		AcceptancePass:     pr.Err == "" && !pr.Result.Deferred && len(pr.AcceptanceFailures) == 0,
		WallMs:             pr.wallMs,
		EstTokens:          EstimateTokens(contract),
		Error:              pr.Err,
		Contract:           contract,
		AcceptanceFailures: pr.AcceptanceFailures,
		Arm:                strings.TrimSpace(os.Getenv("OFFLOAD_DELEGATE_ARM")),
		DelegatorVersion:   buildinfo.Version,
		// Computed once per process (sync.Once) — the per-row cost is a read.
		DelegatorBuildSHA256: buildinfo.BuildSHA256(),
		Placed:               pr.Placed,
	}
	if pr.Err == "" {
		res := pr.Result
		line.Result = &res
	}
	r.corpusTried.Add(1)
	if err := appendDelegationLog(r.cfg.BaseDir(), line); err != nil {
		r.corpusLost.Add(1)
		// The corpus is a DELIVERABLE of this lane (the standing small-model
		// agent-task dataset), not incidental logging: an unwritable corpus is
		// worth an operator's attention even though it can never fail the work.
		r.warnCorpus.Do(func() {
			log.Printf("delegate: delegation-log write failed under %s; this run's corpus rows are LOST (results unaffected): %v", r.cfg.BaseDir(), err)
		})
	}

	if r.led == nil {
		if r.ledgerUnopened {
			// Attempted-and-lost: the row was owed and no handle exists to write
			// it. (No configured LedgerPath at all is not a loss — nothing was
			// ever owed — so it stays uncounted.)
			r.ledgerTried.Add(1)
			r.ledgerLost.Add(1)
		}
	} else {
		reason := pr.Err
		if reason == "" && pr.Result.Deferred {
			reason = pr.Result.Reason
		}
		if reason == "" && len(pr.AcceptanceFailures) > 0 {
			reason = "failed verification: " + pr.AcceptanceFailures[0]
		}
		// Layer (council R8): the composite layer this row was placed on, so
		// the scoreboard can sum work per layer later; "" (omitted) on a
		// plain box.
		layer := ""
		if pr.Placed != nil {
			layer = pr.Placed.Layer
		}
		r.ledgerTried.Add(1)
		if err := r.led.Record(ledger.Entry{
			Task:      "agent_delegate",
			Layer:     layer,
			LatencyMs: pr.wallMs,
			// TokensIn stays 0 ON PURPOSE: the summary counts a completed
			// row's TokensIn as tokens-saved, and a delegation row claiming
			// savings would double-count the node-side agent row.
			TokensOut:    pr.Result.TokensOut,
			SeatTokensIn: pr.Result.SeatTokensIn,
			// tok_per_s on the DELEGATE row (0.131.0): until now only the
			// node's own `agent` twin row carried it, so the row the operator
			// reads showed 0 on a job that produced 6,236 tokens.
			TokPerSec: firstNonZero(pr.Result.SeatTokS, pr.Result.ObservedTokS),
			Deferred:  pr.Result.Deferred || pr.Err != "" || len(pr.AcceptanceFailures) > 0,
			Reason:    reason,
			// ModelTier carries placement:seat — the ledger has no placement
			// column, and "which node/seat ran it" is the row's whole story.
			ModelTier: pr.Node + ":" + pr.Seat,
			// The job behind the row (D-101 / F15): what this result already
			// knew, so a reader never has to open the corpus for it. The
			// session that asked is stamped by ledger.Record itself.
			JobID: pr.JobID,
			// PR-14 (ADR 0064): the surface that admitted the contract, the id the
			// fleet node knows the job by (a remote attempt only: a job that stayed
			// on this box has none) and the closed-set reason code — so a reader
			// never joins on latency or greps prose. The reason above is stored
			// whole: the ledger no longer cuts it.
			Door:             doorOf(contract),
			FleetJobID:       fleetJobIDOf(pr),
			ReasonCode:       reasonCodeFor(pr),
			Route:            r.route,
			RouteAsked:       routeAsked,
			PinReason:        pinReason,
			Placement:        placement,
			Steps:            pr.Result.Steps,
			StopReason:       pr.Result.StopReason,
			RepackMs:         pr.Result.RepackMs,
			RepackAttempts:   pr.Result.RepackAttempts,
			QueuedMs:         pr.Result.QueuedMs,
			AcceptanceResult: acceptanceResult(pr),
		}); err != nil {
			r.ledgerLost.Add(1)
			r.warnLedger.Do(func() {
				log.Printf("delegate: ledger row write failed for %s; this run's savings accounting is incomplete (results unaffected): %v", r.cfg.LedgerPath, err)
			})
		}
	}
}

// acceptanceResult is the ledger's one-word verdict for a placed result:
// "pass" (the run completed and every acceptance check held — including a
// contract that declared none), "fail" (a check failed), "" (nothing was
// evaluated: the run deferred or the wire failed). It mirrors the corpus
// row's acceptance_pass, spelled so a deferred row cannot read as a failed
// check.
func acceptanceResult(pr PlacedResult) string {
	switch {
	case len(pr.AcceptanceFailures) > 0:
		return "fail"
	case pr.Err == "" && !pr.Result.Deferred:
		return "pass"
	}
	return ""
}

// appendDelegationLog appends one JSONL line to
// BaseDir()/delegation-log/YYYY-MM-DD.jsonl (day-sharded so the corpus stays
// tail-able and old days archive by file).
func appendDelegationLog(baseDir string, line delegationLogLine) error {
	dir := filepath.Join(baseDir, "delegation-log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	val, err := json.Marshal(line)
	if err != nil {
		return err
	}
	val = append(val, '\n')
	path := filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl")
	delegationLogMu.Lock()
	defer delegationLogMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(val)
	return err
}

// firstNonZero is the ledger's tok_per_s preference: the calibrated seat rate,
// else the run's observed decode rate (0.131.1).
func firstNonZero(a, b float64) float64 {
	if a > 0 {
		return a
	}
	return b
}
