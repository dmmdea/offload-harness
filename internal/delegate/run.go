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
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	// Aliased: this file's `placement` struct (a resolved "run it HERE") predates
	// the package and is used at every placement site; the package is the
	// composite tier's decision TABLE (ADR 0039), so the alias names what it is.
	placetable "github.com/dmmdea/offload-harness/internal/placement"
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
	intentRecorded  bool
	orphanable      bool
	PlacementReason string
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
	// remotesUnreachable marks a LOCAL placement that happened while the
	// configured fleet was failing its health probe. The work is fine (an idle
	// or queued local box is the quality-first placement either way) but the
	// fleet is not, and route=auto used to discard that verdict entirely — so a
	// fleet down for a week read green forever. Counted into
	// Summary.Infrastructure, which is what makes it audible.
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
	// refused is separate from `refusalStatus != 0` because 0 is a real value
	// (a transport failure), and because a marshaling error inside dispatch is
	// a delegator bug rather than any node's answer.
	refused       bool
	refusalStatus int
	// retryAfterNote (item 7, register D-105/D-106) names a 503 dispatch
	// refusal that carried a Retry-After header and was honored with a
	// courtesy wait-then-retry to the SAME node, INSIDE runRemote — never
	// surfaced to placeAndRun as a discrete refusal (the node is not marked
	// `tried` for it), so this is the only trace of it: appended to
	// PlacementReason by attempt() when the retry succeeded.
	retryAfterNote string

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
	// have room before one took it — the capacity wait, agent_placement_wait_sec
	// — and is NOT part of timeout_sec. waited marks it for Summary.Waited; shed
	// marks a sheddable subtask that found no idle node (Summary.Shed).
	CapacityWaitSec float64
	waited          bool
	shed            bool
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
	// sequential chunks RunBatched ran (1 for a plain Run).
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
	// MaxSubtasks is the same bound, exported for callers that batch (RunBatched).
	MaxSubtasks = maxSubtasks
	// runConcurrency bounds the fan-out. 4: enough to overlap remote polls,
	// small enough that a local fallback burst cannot stampede the one GPU.
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
// TimeoutSec: the node enforces TimeoutSec as its own wall, so the delegator
// allows that plus transport slack before declaring the poll dead.
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

// maxQueuedWait is the ABSOLUTE ceiling on how long a subtask may sit in a
// node's backlog before the delegator gives up on it, whatever the contract's
// own timeout says.
//
// Why a second ceiling on top of the contract budget: a subtask that has not
// been handed a worker in five minutes is behind a backlog no fan-out is going
// to clear in time, and the caller is better served by a loud, immediate
// failure than by a longer wait it did not ask for. Without this, a contract
// declaring the 900s cap could park a delegation for a quarter of an hour
// before reporting that nothing ever happened.
//
// It is a CEILING, not the bound itself: the effective bound is
// min(pollBudget, maxQueuedWait), so a short contract never waits longer for a
// slot than it was willing to spend on the work.
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

// RunBatched runs contracts in consecutive chunks of MaxSubtasks, in order.
// offload_research builds ONE contract per usable page and allows 12 URLs per
// call, so a 9–12-page call hit Run's 8-subtask refusal and every page was
// lost (2026-09-01, three sessions). Chunks run SEQUENTIALLY on purpose: the
// fan-out bound exists to keep one GPU from being stampeded, and two chunks in
// flight would be a 16-wide fan-out by another name. Results come back in
// input order; Summary counters are summed; the first error is returned WITH
// the results collected before it, so a caller can render the partial work.
func RunBatched(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *RunOptions) ([]PlacedResult, Summary, error) {
	var all []PlacedResult
	var total Summary
	for start := 0; start < len(subtasks); start += MaxSubtasks {
		end := start + MaxSubtasks
		if end > len(subtasks) {
			end = len(subtasks)
		}
		res, sum, err := RunWith(ctx, cfg, local, subtasks[start:end], route, remotes, opts)
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
		return RunWith(ctx, cfg, local, subtasks, route, remotes, opts) // the "at least one subtask" error, unchanged
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

// RunWith is Run with caller-owned options (see RunOptions).
func RunWith(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *RunOptions) ([]PlacedResult, Summary, error) {
	switch route {
	case "":
		route = "auto"
	case "auto", "local", "remote", "spread", "queue":
	default:
		return nil, Summary{}, fmt.Errorf("delegate: route %q not recognized (want auto, spread, local, remote, or queue)", route)
	}
	if len(subtasks) == 0 {
		return nil, Summary{}, fmt.Errorf("delegate: at least one subtask required")
	}
	if len(subtasks) > maxSubtasks {
		return nil, Summary{}, fmt.Errorf("delegate: %d subtasks exceeds the max of %d", len(subtasks), maxSubtasks)
	}
	// route "queue" bypasses the whole push machinery (ADR 0030): the holder
	// owns durability and the claim loops own placement.
	if route == "queue" {
		return runQueued(ctx, cfg, subtasks)
	}
	// Fleet membership is configuration: a call that names no remotes uses the
	// config's delegate_remotes. A call's own list REPLACES it (never merges) so
	// one node can still be targeted deliberately.
	if len(remotes) == 0 {
		remotes = cfg.DelegateRemotes
	}
	for _, base := range remotes {
		if err := netguard.TailnetURL(base); err != nil {
			return nil, Summary{}, fmt.Errorf("delegate: remote %q: %w", base, err)
		}
	}

	// Ledger row per delegation (roast delta 9). ledger.Open is the existing
	// RECORDLESS-COMPATIBLE append path: a standalone O_APPEND JSONL writer,
	// safe beside a live MCP server's handle — nothing here touches the
	// pipeline's cache/shadow/exemplar stores. Telemetry failure never blocks
	// delivery (same posture as pipeline.record's ignored error).
	var led *ledger.Ledger
	ledgerUnopened := false
	if cfg.LedgerPath != "" {
		if l, err := ledger.Open(cfg.LedgerPath); err == nil {
			led = l
			defer led.Close()
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
	r := &runner{cfg: cfg, local: local, route: route, remotes: remotes, intent: openIntentLedger(cfg), led: led, ledgerUnopened: ledgerUnopened, pair: pairworkloads.New(pairworkloads.FromConfig(cfg))}
	if opts != nil {
		r.quarantine = opts.Quarantine
		r.priority = core.ClampBand(opts.Priority)
		r.tenant = opts.Tenant
		r.decider = opts.LocalDecider
	}
	// route=spread probes the fleet ONCE per run: every subtask deals itself
	// across the same roster, so per-subtask probing would be N identical GETs
	// and could even deal two subtasks against different snapshots.
	if route == "spread" {
		r.spreadViews, r.spreadBases, r.spreadProbeErrs = r.fetchViews(ctx)
		r.spreadLease = LocalLease(cfg.GPULockPath, cfg.StateDir)
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
		localBusy := busyReading{}
		if route == "auto" {
			localBusy = r.probeLocalBusy(ctx)
			busy = leaseInfo.Held || localBusy.inflight >= cfg.FleetConcurrencyLimit() || localBusy.loading
			// One line per run, mirroring route=spread's own local-slot log
			// (review round 1 item 4): before this the identical W-01 read had
			// no trace at all, so an operator could not tell "busy" from
			// "idle" without re-deriving it from the placement_reason.
			log.Printf("delegate: auto local slot: busy=%v inflight=%d loading=%v (%s)", busy, localBusy.inflight, localBusy.loading, localBusy.note)
		}
		var failed map[string]string
		if busy {
			r.autoViews, r.autoBases, r.autoProbeErrs, failed = r.fetchViewsDetailed(ctx)
		}
		r.autoDeal = r.dealAutoRemote(subtasks, r.localView(), r.autoViews, r.autoBases, busy, failed)
	}
	results := make([]PlacedResult, len(subtasks))
	sem := make(chan struct{}, runConcurrency)
	var wg sync.WaitGroup
	for i, c := range subtasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, contract core.AgentContract) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = r.runOne(ctx, i, contract)
		}(i, c)
	}
	wg.Wait()

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
		// "did anything get eaten?" (the MCP error flag) cannot answer it from
		// the merged count.
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

	// quarantine is the caller's Quarantine (RunOptions), nil = inert.
	quarantine  *Quarantine
	quarantined atomic.Int64
	// priority / tenant are RunOptions.Priority (clamped) and RunOptions.Tenant,
	// stamped on every dispatch this run makes (see dispatch).
	priority int
	tenant   string
	// decider is RunOptions.LocalDecider; snapshot is the ONE memoised reader
	// set the production decider reads through (built lazily on the first
	// composite decision, shared by every subtask goroutine — Snapshot is
	// mutex-guarded). Both inert on a non-composite box.
	decider  func(ctx context.Context, contract core.AgentContract, st Subtask) placetable.Decision
	snapOnce sync.Once
	snapshot *placetable.Snapshot
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
	alt, fenceNote, ok := r.alternativeNode(altCtx, first, contract, pl)
	cancel()
	if !ok {
		// A fence is a REASON, not a silence: the caller has to be able to tell
		// "there was nowhere else to go" from "the only seat left is behind a
		// measurement that has the cards" (D-94).
		if fenceNote != "" {
			first.RetryNote = fenceNote
		}
		return first
	}
	// Never land the retry on a seat that is already generating for another
	// job (D-46): the two runs halve each other's tok/s and the retry, on the
	// leftover budget, is the one that dies (2026-09-10: the ledger-01 retry
	// joined the 27B mid-generation of ledger-00 and both crawled at 27 tok/s).
	if busy, why := r.retrySeatBusy(ctx, alt); busy {
		first.RetryNote = fmt.Sprintf("retry skipped: the retry seat on %s is already running another job (%s); a shared seat would only slow both", alt.view.NodeID, why)
		return first
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
	return mergeAttempts(first, second)
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
func (r *runner) retryFloorFor(first PlacedResult) (int, string) {
	floor := r.retryFloorSec()
	if m := first.Result.MinTurnSec; m > floor {
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
	// it back. Bounded by agent_placement_wait_sec.
	credit time.Duration
	// capacityRefusal records that at least one node refused for CAPACITY
	// (503/429: queue full, leased, draining, shed) rather than because the
	// request or the address was wrong. Only then is a capacity wait worth
	// running: a roster that 404s or is unreachable does not free up.
	capacityRefusal bool
	// excluded is every dial base that refused for a NON-capacity reason
	// (404/408/409, or unreachable): nothing about such a node frees up, so
	// the capacity wait never asks it again, however much room its health
	// claims. A capacity refusal (503/429) gets a cooldown instead.
	excluded map[string]bool
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
	if pl.attempts > 0 {
		return pr
	}
	if pr.JobID == "" {
		pr.JobID = mintJobID()
	}
	pr.wallMs = time.Since(since).Milliseconds()
	r.record(contract, pr)
	return pr
}

func newPlacements() *placements {
	return &placements{tried: map[string]bool{}, excluded: map[string]bool{}}
}

// noteRefusal files a refused attempt in the ledger: a capacity refusal makes
// the subtask wait-worthy; any other refusal excludes that node from the wait.
func (pl *placements) noteRefusal(pr PlacedResult) {
	if capacityRefusal(pr.refusalStatus) {
		pl.capacityRefusal = true
		return
	}
	if pr.ranBase != "" {
		pl.excluded[pr.ranBase] = true
	}
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
// timeout_sec by design (bounded by maxQueuedWait). So the wall a subtask can
// consume is timeout_sec + pollGrace + queued credit + this placement overhead,
// all bounded, and the loop CONVERGES because every blocking leg is charged to
// the next measurement and the floor is re-checked immediately before dispatch.
// Anything that says timeout_sec is an end-to-end wall ceiling is wrong; it is
// the execution budget.
func (r *runner) placeAndRun(ctx context.Context, i int, contract core.AgentContract, forced *placement, start time.Time, budget int, pl *placements) PlacedResult {
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
		refusals := []string{pr.pendingReason}
		if pr.decided != nil {
			refusals = nil
		}
		return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, "")
	}
	// Recorded whatever the outcome — a SUCCESSFUL placement must exclude its
	// own seat from the verification retry just as firmly as a refused one.
	pl.tried[pr.ranBase] = true
	pl.attempts++
	if !isReplaceable(pr) {
		return pr
	}
	refusals := []string{refusalLine(pr)}
	pl.noteRefusal(pr)
	for {
		remaining := pl.remaining(start, budget)
		if remaining < minRetrySec {
			return exhausted(pr, refusals, budgetSpent(remaining, budget, len(refusals)))
		}
		// Selection BLOCKS (fetchViews). Bound it by what is actually left, so
		// the probe cannot outlive the budget it is probing on behalf of.
		selCtx, cancel := context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
		next, why, ok := r.replacementNode(selCtx, contract, pl, len(refusals))
		cancel()
		if !ok {
			// Every node that could take it is FULL, not broken — or the one
			// seat left is reserved by a text lease (whatever the refusals
			// were: a 409 elsewhere does not make the lease less releasable):
			// wait for something to free rather than fail on a snapshot of one
			// minute. A roster that only 404s or is unreachable is exhausted.
			reserved := r.route != "remote" && !pl.tried[""] && Reserved(LocalLease(r.cfg.GPULockPath, r.cfg.StateDir))
			if pl.capacityRefusal || reserved {
				return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, why)
			}
			return exhausted(pr, refusals, why)
		}
		// RE-MEASURE. `remaining` above was true when taken and the probe may
		// have consumed most of it; committing the stale number to the wire is
		// exactly how a seat gets handed time the subtask no longer has. The
		// ledger is not touched until this check passes, so a placement
		// abandoned here costs neither a slot in the bound nor an exclusion.
		remaining = pl.remaining(start, budget)
		if remaining < minRetrySec {
			return exhausted(pr, refusals, budgetSpent(remaining, budget, len(refusals)))
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
		pr = r.attempt(ctx, i, replaced, &next)
		if pr.waitCapacity {
			// The local last resort's composite decision must WAIT (the
			// pair's long seat over a busy agent seat): nothing ran, so this
			// is the capacity wait's from here, carrying the refusals that
			// emptied the roster — never a published sentinel.
			return r.awaitCapacity(ctx, i, contract, start, budget, pl, pr, refusals, why)
		}
		pl.attempts++
		pr.Replacements = len(refusals)
		if !isReplaceable(pr) {
			// Some node TOOK it. Whether its answer was any good is the four
			// outcome buckets' business, not this loop's.
			pr.ReplacementNote = replacementNote(refusals, true)
			return pr
		}
		refusals = append(refusals, refusalLine(pr))
		pl.noteRefusal(pr)
	}
}

// capacityRefusal: the node declined because it has no room right now (queue
// full, leased, draining, shed) — the refusals a capacity wait can outlast.
func capacityRefusal(status int) bool {
	return status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests
}

// placementPollInterval is how often the capacity wait re-reads the fleet's
// health; refusalCooldown keeps a node that just refused INSIDE the wait off
// the candidate list for a moment, because its health may still advertise the
// room it just denied (the snapshot is stale by construction — see saturated).
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

// awaitCapacity is the delegator's queue (0.113.18, L5 of the fleet-flow
// chapter): a subtask every fitting node has just refused for CAPACITY — or
// whose only placement is a seat a text lease reserves — waits here, with a
// TTL (agent_placement_wait_sec, or agent_lease_wait_sec when that is longer),
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
func (r *runner) awaitCapacity(ctx context.Context, i int, contract core.AgentContract, start time.Time, budget int, pl *placements, seed PlacedResult, refusals []string, why string) PlacedResult {
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
		return r.settle(contract, r.shedResult(localView, seed, refusals), pl, waitStart)
	}
	wait := r.cfg.PlacementWait()
	if lw := time.Duration(r.cfg.AgentLeaseWaitSec) * time.Second; lw > wait {
		wait = lw
	}
	if wait <= 0 {
		if decided != nil {
			// The wait is switched off: a zero TTL. The expiry rule applies at
			// once — RUN on the decided seat (it holds the contract; only the
			// eviction was worth waiting for), never a capacity defer and
			// never the holder-naming deferral (no lease is held).
			note := fmt.Sprintf("capacity wait disabled (agent_placement_wait_sec=%d) — running on the decided seat %s at once; it evicts %s",
				r.cfg.AgentPlacementWaitSec, decided.Seat, decided.Evicts)
			return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, 0, *decided, false, note, waitStart)
		}
		if seed.waitCapacity {
			return r.settle(contract, r.reservedDefer(localView, LocalLease(r.cfg.GPULockPath, r.cfg.StateDir), 0, seed.pendingReason), pl, waitStart)
		}
		return exhausted(seed, refusals, why)
	}
	deadline := waitStart.Add(wait)
	spanStart := waitStart
	var idle time.Duration
	// credit moves the idle span just ended onto the ledger, so an attempt
	// started now is measured against a budget that does not include it.
	credit := func() {
		now := time.Now()
		idle += now.Sub(spanStart)
		pl.credit += now.Sub(spanStart)
		spanStart = now
	}
	// refusedUntil holds, per base, the instant it becomes a candidate again.
	// An instant rather than the refusal's timestamp because the cooldown is
	// JITTERED once when the refusal happens: K dispatchers that all refused the
	// same node must not all re-ask it on the same beat.
	refusedUntil := map[string]time.Time{}
	// probeFails is every base whose health probe failed DURING the wait, by
	// base: the tick used to drop these on the floor, so a wait that never
	// reached a single node reported "0 refusal(s)".
	probeFails := map[string]*probeFailTally{}
	var lease gpulease.Info
	for wait > 0 && ctx.Err() == nil {
		if decided == nil && r.route != "remote" && !pl.tried[""] {
			lease = LocalLease(r.cfg.GPULockPath, r.cfg.StateDir)
			if !Reserved(lease) {
				credit()
				remaining := pl.remaining(start, budget)
				if remaining < minRetrySec {
					return exhausted(seed, refusals, budgetSpent(remaining, budget, len(refusals)))
				}
				replaced := contract
				replaced.TimeoutSec = remaining
				replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
				// Reaching here means the local seat WAS reserved when the wait
				// began (an unreserved, untried local seat is taken by
				// replacementNode before any wait): the lease cleared.
				forced := placement{view: localView, reason: fmt.Sprintf("local seat was reserved, lease cleared after %s — running local (capacity wait)", idle.Round(time.Second))}
				pr := r.attempt(ctx, i, replaced, &forced)
				if pr.waitCapacity {
					// The lease cleared, and the composite decision now asks
					// to wait for a busy pair seat to drain: the same wait
					// continues as a DECIDED one (nothing ran, nothing to
					// record); the tick below re-decides and runs the seat.
					decided = pr.decided
					continue
				}
				pl.tried[""] = true
				pl.attempts++
				return r.landedAfterWait(pr, idle, refusals)
			}
		}
		if r.route != "local" {
			// The tick's probe may not outlive the wait it serves; the per-base
			// bound inside the fan-out caps what any ONE remote can cost
			// (probeTickBound explains why nothing tighter belongs here).
			tickCtx, cancelTick := context.WithTimeout(ctx, probeTickBound(deadline))
			views, bases, _, failed := r.fetchViewsDetailed(tickCtx)
			cancelTick()
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
			for j, v := range views {
				if !remoteEligible(st, v) || !hasRoom(v, false) || pl.excluded[bases[j]] {
					continue
				}
				if until, ok := refusedUntil[bases[j]]; ok && time.Now().Before(until) {
					continue
				}
				if best < 0 || betterRemote(p2cSeed, &st, v, views[best]) {
					best = j
				}
			}
			if best >= 0 {
				credit()
				remaining := pl.remaining(start, budget)
				if remaining < minRetrySec {
					return exhausted(seed, refusals, budgetSpent(remaining, budget, len(refusals)))
				}
				replaced := contract
				replaced.TimeoutSec = remaining
				replaced.TimeoutAuto = false // what is left — explicit, never auto (D-03)
				forced := placement{view: views[best], base: bases[best],
					reason: fmt.Sprintf("capacity wait → %s (room after %s)", views[best].NodeID, idle.Round(time.Second))}
				pr := r.attempt(ctx, i, replaced, &forced)
				spanStart = time.Now() // the attempt is charged; the wait resumes here
				pl.tried[bases[best]] = true
				pl.attempts++
				if !isReplaceable(pr) {
					return r.landedAfterWait(pr, idle, refusals)
				}
				seed = pr
				refusals = append(refusals, refusalLine(pr))
				refusedUntil[bases[best]] = time.Now().Add(jittered(refusalCooldown))
				pl.noteRefusal(pr) // a non-capacity answer excludes the node from the rest of the wait
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
		if decided != nil && r.route != "remote" && ctx.Err() == nil && !Reserved(LocalLease(r.cfg.GPULockPath, r.cfg.StateDir)) {
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
		if info := LocalLease(r.cfg.GPULockPath, r.cfg.StateDir); Reserved(info) {
			// A text lease took the cards during the wait: the holder is real,
			// and running on the reserved seat is the one thing the lease
			// forbids. The established deferral, naming the holder.
			return r.settle(contract, r.reservedDefer(localView, info, idle, decided.Reason), pl, waitStart)
		}
		// TTL expiry for a decided seed RUNS on the decided seat (the plan's
		// rule): the seat holds the contract and is free to load; the wait was
		// only ever about sparing the seat it evicts. Never capacityDefer —
		// "no node had room" would be false.
		note := fmt.Sprintf("capacity wait expired after %s (agent_placement_wait_sec=%d) with %s still busy — running on the decided seat %s; it evicts %s",
			idle.Round(time.Second), r.cfg.AgentPlacementWaitSec, decided.Evicts, decided.Seat, decided.Evicts)
		return r.runDecided(ctx, i, contract, start, budget, pl, seed, refusals, idle, *decided, true, note, waitStart)
	}
	if r.route != "remote" && Reserved(lease) {
		// Nothing freed and the local seat is still reserved: the established
		// deferral, naming the holder (class infrastructure — a human's timing
		// decision), with the refusals appended.
		return r.settle(contract, r.reservedDefer(localView, lease, idle, "no eligible remote had room — "+strings.Join(refusals, "; ")), pl, waitStart)
	}
	return r.settle(contract, r.capacityDefer(localView, seed, idle, wait, refusals, probeFails), pl, waitStart)
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
			return exhausted(seed, refusals, budgetSpent(remaining, budget, len(refusals)))
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

// landedAfterWait annotates a result some node TOOK after a capacity wait.
func (r *runner) landedAfterWait(pr PlacedResult, idle time.Duration, refusals []string) PlacedResult {
	pr.waited = true
	pr.CapacityWaitSec = idle.Seconds()
	pr.Replacements = len(refusals)
	if len(refusals) > 0 {
		pr.ReplacementNote = replacementNote(refusals, true)
	}
	return pr
}

// capacityDefer is the TTL outcome: every node that could run the subtask was
// full for the whole wait. Deferred, class capacity — the fleet is healthy and
// the contract is sound; it was not this contract's turn — with every refusal
// and the wait named so the caller can re-run, widen the wait, or add a node.
func (r *runner) capacityDefer(local NodeView, seed PlacedResult, idle, wait time.Duration, refusals []string, probeFails map[string]*probeFailTally) PlacedResult {
	reason := fmt.Sprintf("capacity wait: no node had room within %s (waited %s; agent_placement_wait_sec=%d; %d refusal(s): %s%s) — re-run later, raise agent_placement_wait_sec, or add a node",
		wait, idle.Round(time.Second), r.cfg.AgentPlacementWaitSec, len(refusals), strings.Join(refusals, "; "),
		probeFailureNote(probeFails))
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
// differently.
func isReplaceable(pr PlacedResult) bool {
	return pr.refused && replaceableRefusal(pr.refusalStatus)
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
// ok=false returns the sentence the exhausted message ends with. It is built
// from BOTH facts (the bound, and whether local was available) because a reader
// who is told only one of them will look in the wrong place.
func (r *runner) replacementNode(ctx context.Context, contract core.AgentContract, pl *placements, refused int) (placement, string, bool) {
	boundHit := pl.used >= maxRemoteReplacements
	if r.route != "local" && !boundHit {
		st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
		views, bases := r.spreadViews, r.spreadBases
		if r.route != "spread" {
			// Re-probe: the roster this subtask was placed against is now known
			// to be at least partly wrong (a node just refused), and health is
			// the only thing that can say which of the others has room.
			views, bases, _ = r.fetchViews(ctx)
		}
		freshViews, freshBases := untried(views, bases, pl.tried)
		if chosen := Place(mintP2CSeed(), st, r.localView(), freshViews, true); !chosen.Local {
			return placement{
				view:   chosen,
				base:   baseFor(chosen, freshViews, freshBases),
				reason: fmt.Sprintf("re-placed on %s after %d refusal(s)", chosen.NodeID, refused),
			}, "", true
		}
	}
	// Local is the reserved last resort. route=remote never takes it: an
	// explicit remote route must not silently fall local, which is the same
	// posture the "no eligible remote" defer already holds.
	head := "no further eligible remote was available"
	if boundHit {
		head = fmt.Sprintf("the re-placement bound of %d further node(s) was reached", maxRemoteReplacements)
	}
	if r.route == "remote" {
		return placement{}, head + ", and route=remote never falls back to local", false
	}
	if pl.tried[""] {
		return placement{}, head + ", and the local seat had already been tried", false
	}
	// A text lease reserves the local seat for re-placement exactly as it does
	// for first placement (0.113.18; before this a remote's 503 fell straight
	// onto the reserved cards — the 2026-09-05 incident through a side door).
	// The capacity wait, not this fall-back, is what watches for the release.
	if info := LocalLease(r.cfg.GPULockPath, r.cfg.StateDir); Reserved(info) {
		return placement{}, head + ", and the local seat is reserved (" + HolderLine(info) + ")", false
	}
	return placement{
		view:   r.localView(),
		reason: fmt.Sprintf("re-placed on the local seat after %d refusal(s) — queued-local beats work nobody ran", refused),
	}, "", true
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

// minRetrySec is the least timeout_sec budget a retry is worth starting with: a
// cold seat needs seconds to load and a contract that cannot finish in this
// would only add a budget defer on top of the verified failure.
const minRetrySec = 10

// retryable: the node answered and the ANSWER was the problem — a verified
// wrong result, or a seat honestly abstaining. A transport failure, a budget
// defer, or a broken/misconfigured stack is not something another seat fixes,
// and a contract-classed defer is the caller's to fix.
//
// The ONE infrastructure defer that IS retryable is the admission-time
// coherence defer (register D-118): the seat itself is broken and it was caught
// before the contract's wall started, so the budget is still there to fund a
// retry — which is precisely the case another node fixes. What the node's
// admission spent getting there is credited back in runOne (admissionCredit),
// because the cold load that triggers the probe would otherwise eat most of a
// default budget before the retry floor is applied. The general infrastructure
// rule is untouched; see IncoherentSeatDefer.
func retryable(pr PlacedResult) bool {
	if pr.Err != "" {
		return false
	}
	if len(pr.AcceptanceFailures) > 0 {
		return true
	}
	if IncoherentSeatDefer(pr.Result) {
		return true
	}
	return pr.Result.Deferred && pr.Result.DeferClass == core.DeferClassAbstention
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

// admissionCredit is the node-side admission time a coherence defer already
// spent — its cordon wait, pre-flight, cold load and the probe itself — which
// the subtask's execution budget must not be charged for: the contract's wall
// never started, and the wire carries admission_wait_sec for exactly this.
// Zero for every other result: no other shape has a claim on the credit, and a
// node that reports no admission (a pre-D-118 node, or an unmeasured one) is
// credited nothing rather than guessed at.
func admissionCredit(pr PlacedResult) time.Duration {
	if !IncoherentSeatDefer(pr.Result) || pr.Result.AdmissionWaitSec <= 0 {
		return 0
	}
	return time.Duration(pr.Result.AdmissionWaitSec * float64(time.Second))
}

// alternativeNode picks the node a retry runs on: the best eligible remote
// when the first attempt ran locally (probing the fleet now if this run has
// not yet), the local seat when it ran remotely. ok=false when no different
// node can take the contract.
//
// Note the asymmetry and its cost: recovering a wrong REMOTE answer puts the
// work back on the local box the harness exists to keep free — so a placement
// the fit score reads too cheap is paid for in local GPU time, not just in
// latency. That is accepted deliberately (local is the one seat always able to
// take the contract, and a second remote hop would need a fresh gate pass
// mid-timeout), but it is a real cost, not a free retry.
// It consults the SUBTASK's placement ledger, which is what makes "a DIFFERENT
// node" true rather than merely intended: a seat the first attempt already used
// — including one it reached by re-placement, and including the local seat — is
// excluded here. Without that, a first attempt that ended up local after two
// remotes refused could be "retried" on local again.
func (r *runner) alternativeNode(ctx context.Context, first PlacedResult, contract core.AgentContract, pl *placements) (placement, string, bool) {
	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	localView := r.localView()
	why := attemptOutcome(first)
	if !first.ranLocal {
		// D-94: the local seat is only a retry target if its lease would ADMIT
		// the run. Read fresh — the deal's snapshot can be minutes old by the
		// time a first attempt has failed somewhere else — and read the VERDICT
		// rather than dialling: a fenced seat answers a dial by holding the
		// request at the affinity cordon for agent_lease_wait_sec and then
		// deferring as capacity, which is five minutes of a retry's budget spent
		// discovering something the lease record already said.
		lease := LocalLease(r.cfg.GPULockPath, r.cfg.StateDir)
		if fenced, fence := Fenced(lease); fenced {
			if chosen, base, found := r.remoteAlternative(ctx, st, pl); found {
				return placement{view: chosen, base: base,
					reason: "retry on " + chosen.NodeID + " after " + nodeLabel(first) + " " + why +
						" — the local seat is fenced: " + fence}, "", true
			}
			return placement{}, fmt.Sprintf(
				"retry skipped: the local seat is fenced (%s — %s) and no other node is eligible; "+
					"a retry placed there would wait out agent_lease_wait_sec at the affinity cordon and defer as capacity anyway",
				fence, HolderLine(lease)), false
		}
		// No pl.tried[""] check here, and that is a proof rather than an
		// oversight: a LOCAL placement is always terminal for its chain,
		// because only runRemote can set `refused` and therefore local can
		// never be re-placed away from. So pl.tried[""] implies first.ranLocal,
		// and this branch cannot run with local already used.
		//
		// A guard was written here first. The mutation battery could not kill
		// it from any fixture — which is the tell for a branch that reads as
		// protection while protecting nothing — so it is stated as an invariant
		// instead. If local ever becomes re-placeable, this is the line to
		// revisit, and the other direction (a retry's own chain falling back
		// onto an already-used local seat) is guarded in replacementNode, where
		// it IS reachable and IS covered.
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
	views, bases := r.spreadViews, r.spreadBases
	if r.route != "spread" {
		views, bases, _ = r.fetchViews(ctx)
	}
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
func (r *runner) dealAutoRemote(contracts []core.AgentContract, localView NodeView, views []NodeView, bases []string, localBusy bool, failed map[string]string) []spreadSlot {
	dealt := make(map[string]int, len(views))
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
		slot := r.placeAutoRemote(jobID, st, localView, views, bases, localBusy, dealt, failed)
		slot.jobID = jobID
		out[i] = slot
	}
	return out
}

// placeAutoRemote deals ONE subtask within dealAutoRemote's joint pass: an
// idle local seat wins unconditionally (Place's own rule, unchanged); busy,
// the best REMOTE that passes remoteEligible AND still has headroom over what
// this deal has already committed to it. A node at 0 headroom gets NOTHING —
// no floor, no "at least one" — and the next-best candidate is tried.
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
	if !localBusy {
		return spreadSlot{placement: placement{view: localView, reason: "local idle"}}
	}
	var best NodeView
	var bestBase string
	found, anyEligible := false, false
	for j, v := range views {
		if !remoteEligible(st, v) {
			continue
		}
		anyEligible = true
		if headroom(v) <= dealt[bases[j]] {
			continue // W-06: no floor — a node at 0 headroom gets nothing this deal
		}
		if !found || betterRemote(seed, &st, v, best) {
			best, bestBase, found = v, bases[j], true
		}
	}
	// D-105 (review round 1, BLOCKER item 2): the verdict line is built ONCE,
	// from `dealt` as it stood WHILE scanning (every candidate's headroom read
	// against the state at decision time, matching what the loop above
	// actually saw), and printed on EVERY exit — chosen, capacity-waiting, or
	// nothing eligible at all — not only the happy path. `failed` carries the
	// dead/unreachable bases from this SAME snapshot, so a node this deal
	// never even heard from is named too, not silently dropped.
	verdicts := placementVerdictLine(st, views, bases, bestBase, dealt, failed)
	if found {
		dealt[bestBase]++
		reason := fmt.Sprintf("route=%s → %s (headroom)", r.route, best.NodeID)
		if verdicts != "" {
			reason += "; " + verdicts
		}
		return spreadSlot{placement: placement{view: best, base: bestBase, reason: reason}}
	}
	if anyEligible {
		reason := fmt.Sprintf("route=%s: every eligible remote is at headroom", r.route)
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
// immediately: the smallest seat wins EVERY mechanical slot and the roomiest
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
	dealt := make(map[string]bool, len(r.spreadViews))
	out := make([]spreadSlot, len(contracts))
	for i, c := range contracts {
		st := Subtask{Contract: c, EstTokens: EstimateTokens(c)}
		out[i] = r.placeSpread(i, st, localView, dealt)
	}
	return out
}

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
func (r *runner) placeSpread(i int, st Subtask, localView NodeView, dealt map[string]bool) spreadSlot {
	return r.placeSpreadWith(i, st, localView, dealt, r.skipsBusyLocal())
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
	endpoint, seat := strings.TrimSpace(r.cfg.Endpoint), strings.TrimSpace(r.cfg.AgentPlannerModel(""))
	if endpoint == "" || seat == "" {
		log.Printf("delegate: local seat busy probe skipped (endpoint %q, agent seat %q); dealing the local slot as idle", endpoint, seat)
		return busyReading{note: "no local endpoint or agent seat configured"}
	}
	pctx, cancel := context.WithTimeout(ctx, localBusyProbeTimeout)
	defer cancel()
	rd, err := seatload.Inflight(pctx, localBusyClient, endpoint, seat)
	if err != nil {
		log.Printf("delegate: local seat busy probe of %s/%s failed; dealing the local slot as idle: %v", endpoint, seat, err)
		return busyReading{note: "busy probe failed: " + err.Error()}
	}
	if !rd.Loaded {
		if rd.Ambiguous {
			// Degraded read: the roster could not resolve the seat and /running
			// holds other entries. Idle is the SAFE reading for a deal (the
			// worst case is the pre-0.113.20 stacking), but it must be visible.
			log.Printf("delegate: local seat busy probe of %s/%s is ambiguous (roster unreadable: %v; /running lists %d other model(s)); dealing the local slot as idle", endpoint, seat, rd.RosterErr, rd.RunningOthers)
			return busyReading{note: "ambiguous: roster unreadable"}
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

// skipsBusyLocal reports whether this run deals its local rotation slots away:
// the seat read busy at deal time AND the config did not pin the slot local.
func (r *runner) skipsBusyLocal() bool {
	return r.spreadLocalBusy.busy && r.cfg.SpreadLocalSlot() != config.SpreadLocalAlways
}

// placeSpreadWith is placeSpread with the busy rule as an explicit argument, so
// the no-remote-with-room fallback can re-deal WITHOUT it and the two paths
// share one body.
func (r *runner) placeSpreadWith(i int, st Subtask, localView NodeView, dealt map[string]bool, skipBusy bool) spreadSlot {
	// A TEXT reservation takes the local seat out of the rotation — a spread
	// used to ignore the lease entirely, which is how three foreign contracts
	// loaded a reserved seat mid-measurement (2026-09-05 08:04–08:09). A media
	// lease is deliberately NOT consulted here: spread never read it before
	// 0.113.14 and its render is arbitrated at the affinity gate (ADR 0026), so
	// a media holder changes nothing about a spread's deal (review 2026-09-06).
	var nodes []NodeView
	var bases []string
	localIn := !Reserved(r.spreadLease)
	// A BUSY local seat (0.113.20) leaves the rotation exactly like a leased
	// one, but only while a remote with room exists to take its slots: the
	// remotes are filtered by hasRoom (a sheddable run needs an idle slot), and
	// with none left the slot falls back to the ordinary deal below, reason
	// attached — the busy rule is an optimisation, never a way to lose work.
	skip := skipBusy && localIn
	if localIn && !skip {
		nodes, bases = []NodeView{localView}, []string{""}
	}
	eligible := 0
	for j, v := range r.spreadViews {
		if !remoteEligible(st, v) {
			continue
		}
		eligible++
		if skip && !hasRoom(v, r.priority < core.BandNormal) {
			continue
		}
		nodes = append(nodes, v)
		bases = append(bases, r.spreadBases[j])
	}
	if skip && len(nodes) == 0 {
		// The reason must not send an operator chasing capacity when no remote
		// could take this contract at all.
		sl := r.placeSpreadWith(i, st, localView, dealt, false)
		if eligible == 0 {
			sl.reason += fmt.Sprintf(" (local seat busy: %d in flight; no eligible remote)", r.spreadLocalBusy.inflight)
		} else {
			sl.reason += fmt.Sprintf(" (local seat busy: %d in flight; no remote with room)", r.spreadLocalBusy.inflight)
		}
		return sl
	}
	if len(nodes) == 0 || (len(nodes) == 1 && nodes[0].Local) {
		why, class := r.noEligibleRemote(st, r.spreadViews, r.spreadProbeErrs)
		dead := class == core.DeferClassInfrastructure
		if Reserved(r.spreadLease) {
			// The one placement the lease exists to forbid. Dealt local so the
			// slot has a view, but flagged: attempt() waits or defers.
			return spreadSlot{placement: placement{view: localView, reason: "route=spread: local seat reserved (" + HolderLine(r.spreadLease) + "); no eligible remote — " + why}, deadFleet: dead, reserved: true}
		}
		return spreadSlot{placement: placement{view: localView, reason: "route=spread: no eligible remote — local (" + why + ")"}, deadFleet: dead}
	}
	slot := i % len(nodes)
	if nodes[slot].Local {
		// A local slot opens a new cycle: the deck of remotes is reshuffled, so
		// the next len(nodes)-1 subtasks deal one to each seat again.
		clear(dealt)
		return spreadSlot{placement: placement{view: localView, reason: fmt.Sprintf("route=spread → local (slot %d of %d)", slot+1, len(nodes))}}
	}
	k := fitPick(st, nodes, bases, slot, dealt)
	if k < 0 {
		// Every eligible remote has already taken a subtask this cycle — which a
		// ragged eligible set can reach without passing through a local slot.
		// Reshuffle rather than stack: after the clear a pick always exists,
		// because len(nodes) > 1 guarantees at least one remote.
		clear(dealt)
		k = fitPick(st, nodes, bases, slot, dealt)
	}
	dealt[bases[k]] = true
	kind, rule := shapeOf(st)
	reason := fmt.Sprintf("route=spread → %s (slot %d of %d, fit=%s/%s)", nodes[k].NodeID, slot+1, len(nodes), kind, rule)
	if skip {
		reason += fmt.Sprintf("; local seat busy: %d in flight", r.spreadLocalBusy.inflight)
	}
	return spreadSlot{placement: placement{view: nodes[k], base: bases[k], reason: reason}}
}

// fitPick returns the index of the best-scoring seat in nodes that is neither
// local nor already dealt this cycle, or -1 when the cycle has no seat left.
// The scan starts at the rotation slot and wraps, and only a STRICTLY better
// score displaces the incumbent — so equal seats are dealt in rotation order
// and an all-equal roster behaves exactly as blind round-robin did.
// bases is parallel to nodes and is what `dealt` is keyed on — see dealSpread
// for why the node id is not a usable key here.
func fitPick(st Subtask, nodes []NodeView, bases []string, slot int, dealt map[string]bool) int {
	k, best := -1, 0
	for c := 0; c < len(nodes); c++ {
		j := (slot + c) % len(nodes)
		if nodes[j].Local || dealt[bases[j]] {
			continue
		}
		if s := scoreFit(st, nodes[j]); k < 0 || s > best {
			k, best = j, s
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

// attempt places (per route, or as forced by a retry) and executes one
// subtask, then verifies and records it. Every return path passes through
// finish() so no outcome can skip telemetry.
func (r *runner) attempt(ctx context.Context, i int, contract core.AgentContract, forced *placement) PlacedResult {
	start := time.Now()
	// Delegator-mints-the-id (roast delta 14): minted per attempt, BEFORE
	// placement, so even a local run correlates its telemetry line — and a
	// retry never reuses the id a node may still hold.
	jobID := mintJobID()

	finish := func(pr PlacedResult) PlacedResult {
		pr.JobID = jobID
		pr.wallMs = time.Since(start).Milliseconds()
		// Intent ledger close-out (Option A, intent.go): an acked job whose
		// terminal answer THIS process observed is closed; the orphanable
		// exits (cancel / owned-deadline / queued give-up) stay open for the
		// recovery pass — that gap IS the durability feature.
		if pr.intentRecorded && !pr.orphanable {
			r.intent.done(jobID, "terminal observed")
		}
		r.record(contract, pr)
		r.pairTerminal(jobID, &pr)
		return pr
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
		if d.reserved {
			// Reserved at deal time; the lease may have cleared since (a run
			// can wait minutes in the semaphore), so re-read before deciding.
			// Still reserved: hand the subtask to the capacity wait (0.113.18),
			// which watches the lease AND every remote's room — not through
			// finish(): nothing ran, so there is nothing to record.
			if Reserved(LocalLease(r.cfg.GPULockPath, r.cfg.StateDir)) {
				// reason already names the holder and why no remote qualified
				// (placeSpread built it); it is the deferral's text when nothing frees.
				return PlacedResult{waitCapacity: true, pendingReason: reason, PlacementReason: reason}
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
		}
		switch {
		case d.capacityWait:
			// At least one remote was otherwise eligible; every one of them
			// was already dealt to its own headroom by this subtask's turn.
			// The existing capacity wait watches for room to free — the same
			// mechanism a 503 refusal or a held lease already sends work to.
			return PlacedResult{waitCapacity: true, pendingReason: d.reason, PlacementReason: d.reason}
		case d.noRemote && r.route == "remote":
			// Nothing in the fleet could ever take this contract (capability,
			// not capacity): the established "route=remote: no eligible
			// remote" Unplaced defer, unchanged.
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
			leaseInfo := LocalLease(r.cfg.GPULockPath, r.cfg.StateDir)
			why, class := r.noEligibleRemote(st, r.autoViews, r.autoProbeErrs)
			if Reserved(leaseInfo) {
				return PlacedResult{waitCapacity: true, pendingReason: "local seat reserved (" + HolderLine(leaseInfo) + "); no eligible remote — " + why}
			}
			chosen = localView
			reason = "local busy; no eligible remote — " + why + " (queued-local beats ineligible-remote)"
			deadFleet = class == core.DeferClassInfrastructure
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
			leaseInfo = LocalLease(r.cfg.GPULockPath, r.cfg.StateDir)
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
			busy = leaseInfo.Held || local.inflight >= r.cfg.FleetConcurrencyLimit() || local.loading
		}
		var views []NodeView
		var bases []string
		var probeErrs []string
		if busy && r.route != "local" {
			views, bases, probeErrs = r.fetchViews(ctx)
		}
		chosen = Place(jobID, st, localView, views, busy)

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
			why, _ := r.noEligibleRemote(st, views, probeErrs)
			return PlacedResult{waitCapacity: true, pendingReason: "local seat reserved (" + HolderLine(leaseInfo) + "); no eligible remote — " + why}
		case chosen.Local:
			why, class := r.noEligibleRemote(st, views, probeErrs)
			reason = "local busy; no eligible remote — " + why + " (queued-local beats ineligible-remote)"
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
	pr := r.runRemote(ctx, base, jobID, dispatched, chosen, runSeat)
	pr.ranBase = base
	pr.PlacementReason = reason
	if pr.retryAfterNote != "" {
		pr.PlacementReason += "; " + pr.retryAfterNote
	}
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
	opts := LocalOptions{Seat: dec.Seat, Placed: pr.Placed}
	if dec.Seat != "" {
		pr.Seat = dec.Seat
	}
	// A local placement starts the moment it is handed to the runner: no
	// queue, no ack. "" as the node = this box's PAIR identity — unless the
	// endpoint is another box's engine, whose name the card then carries
	// (register C-58: PAIR showed the Qube doing the Lenovo's work).
	pairNode := ""
	if host := modelaffinity.EndpointHost(r.cfg.Endpoint); host != "" {
		pairNode = host
		pr.PlacementReason += "; engine " + r.cfg.Endpoint + " is " + host + "'s (attributed there)"
	}
	r.pairInflight(&pr, jobID, pairNode, pr.Seat, "running")
	wire, err := r.local(ctx, contract, opts)
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
	r.pairInflight(&pr, jobID, pairNodeName(base, view.NodeID), intendedSeat, "queued")
	disp := r.dispatchDetailed(ctx, base, jobID, payload)
	if disp.refused && disp.status == http.StatusServiceUnavailable && disp.retryAfterSec > 0 {
		// Item 7 (register D-105/D-106): a 503 carrying its own Retry-After
		// is the node stating exactly how long to wait, not a generic
		// capacity refusal — honor it and retry the SAME node ONCE before
		// falling to the ordinary re-placement loop (placeAndRun's refusal
		// chain, which would otherwise spend a whole different node's dial
		// just to avoid a wait the first node already told us to take).
		// Bounded by what the contract can still afford, so a node with a
		// generous Retry-After never eats a budget it does not own.
		wait := time.Duration(disp.retryAfterSec) * pollSecond
		if budget := time.Duration(executionBudgetSec(contract)) * pollSecond; budget > 0 && wait > budget {
			wait = budget
		}
		select {
		case <-ctx.Done():
			pr.Err = "canceled: " + ctx.Err().Error()
			return pr
		case <-time.After(wait):
		}
		retried := r.dispatchDetailed(ctx, base, jobID, payload)
		if !retried.refused {
			pr.retryAfterNote = fmt.Sprintf("dispatch 503 (Retry-After %ds) honored; landed here on retry", disp.retryAfterSec)
		}
		disp = retried
	}
	if disp.err != nil {
		pr.Err = disp.err.Error()
		// Carry the class so runOne can decide whether ANOTHER node is worth
		// asking. Set here and nowhere else: this is the one moment at which
		// the node has answered and no seat can possibly hold the contract.
		pr.refused, pr.refusalStatus = disp.refused, disp.status
		return pr
	}
	// The node ACKED: from here the job can be orphaned by delegator death,
	// so persist the intent before any polling (Option A, intent.go).
	r.intent.dispatched(jobID, base, contract.Goal)
	pr.intentRecorded = true
	r.pairInflight(&pr, jobID, pairNodeName(base, view.NodeID), intendedSeat, "running")

	timeoutSec := executionBudgetSec(contract)
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
	pollBudget := time.Duration(timeoutSec)*pollSecond + pollGrace
	// THE AUTO BOUND (register D-116). For a contract the caller left unsized,
	// executionBudgetSec hands back the wire CAP — the delegator cannot cut a
	// 700 s wall the node chose at the 300 s the wire happens to carry. But the
	// cap is not the node's wall either, and holding the clock there abandoned
	// a silently dead node 900 s after its own 300 s wall had expired. So the
	// bound is sized from what the node ADVERTISES, by the same arithmetic the
	// node sizes its wall with (autoPollBound → seatrate.AutoWallFor), and the
	// cap stays the ceiling: a node that advertises no rate is still polled to
	// it. pollNote names the source and rides the result (poll_note).
	pollNote := ""
	// slack is the ADMISSION allowance carried on top of the sized bound
	// (review finding 1). The node stamps a job `running` the moment it claims
	// it, and everything before its wall — the cordon wait, the llama-swap
	// pre-flight, a vLLM seat's 125–250 s cold load, the coherence probe —
	// happens in that state, earning no queued credit. A clock started at
	// dispatch and sized at wall + grace therefore abandoned an auto contract
	// that landed on a COLD seat while the node was still inside its own wall.
	// It is dropped the moment a started wall is observed (wallAnchored).
	var slack time.Duration
	if bound, note := autoPollBound(view, contract, runSeat); bound > 0 {
		slack = admissionSlack()
		pollBudget = bound + slack + pollGrace
		pollNote = note
		pr.PollNote = note
	}
	start := time.Now()
	// anchor is where the CLOCK is measured from. It is the dispatch instant
	// until the node publishes a started wall, and that wall's start after —
	// so the budget bounds the node's WALL, not the wall plus however long the
	// node spent admitting the job.
	anchor := start
	wallAnchored := false
	// queuedWaitBudget bounds the credit — an unbounded wait is its own
	// failure mode. A job may wait for a slot at most as long as it was
	// allowed to run, and never more than maxQueuedWait. Total wall clock is
	// therefore bounded by pollBudget + queuedWaitBudget.
	queuedWaitBudget := pollBudget
	if queuedWaitBudget > maxQueuedWait {
		queuedWaitBudget = maxQueuedWait
	}
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
	// setDeadline is the ONE place the deadline is computed, so no arm can
	// rebuild it from a stale base. Once the clock is anchored on the node's
	// own wall start, the backlog credit is deliberately NOT added again: the
	// anchor already subsumes every second spent before the wall began — the
	// backlog wait included — and adding it twice would hand the run more
	// budget than either clock granted. queuedCredit itself keeps accumulating
	// for the operator-facing message, which reports an observed fact.
	var deadline time.Time
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
	// pollFails bounds the failure logging (both arms below fired once PER
	// POLL) and summarizes on the way out, whichever exit is taken.
	pollFails := newPollFailLog(jobID, base)
	defer pollFails.summarize()
	for {
		if err := ctx.Err(); err != nil {
			pr.Err = "canceled: " + err.Error()
			pr.orphanable = true // the node may still finish it — recovery's case
			return pr
		}
		if time.Now().After(deadline) {
			if !sawJobOwned {
				// Never a defer: nothing on that node ever reported OWNING this
				// job, so there is no defer to report. A FAILURE (Summary.Failed,
				// non-zero CLI exit) is the honest outcome — a broken node, or one
				// denying the job, must read broken.
				pr.Err = fmt.Sprintf("poll deadline after %s%s: %s%s", pollBudget, queuedNote(queuedCredit), unownedDetail(sawNodeAnswer, saw404, redispatches, lastPollErr), boundNote(pollNote, slack))
				return pr
			}
			// Roast delta 14: mark deferred, reason PREFIXED "poll deadline"
			// (stable key), and STOP. The node may still finish server-side; the
			// job id in the telemetry line lets an operator reconcile by hand.
			// The wording says only what is known: it acked and it never reached
			// a terminal state — not that it "could not complete the contract".
			reason := fmt.Sprintf("poll deadline after %s%s: node accepted the job but did not reach a terminal state%s", pollBudget, queuedNote(queuedCredit), boundNote(pollNote, slack))
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
			pr.Result = core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, DeferClass: class, Reason: reason}
			pr.orphanable = true // acked, owned, no terminal state seen — recovery's case
			return pr
		}
		poll, perr := r.pollOnce(ctx, base, jobID)
		state, data, jobErr, status := poll.State, poll.Data, poll.JobErr, poll.Status
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
					if queuedWaitBudget = pollBudget; queuedWaitBudget > maxQueuedWait {
						queuedWaitBudget = maxQueuedWait
					}
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
			return pr
		case status == http.StatusOK && state == "done":
			sawNodeAnswer, sawJobOwned = true, true
			var wire core.AgentWireResult
			if uerr := json.Unmarshal(data, &wire); uerr != nil {
				pr.Err = "job done but data is not an AgentWireResult: " + uerr.Error()
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
			return pr
		case status == http.StatusOK && (state == "accepted" || state == "running"):
			// The node answered AND says it owns the job: the only shape that
			// earns a defer at the deadline.
			sawNodeAnswer, sawJobOwned = true, true
			// `accepted` and `running` are no longer the same fact. Since
			// 0.100.0 `accepted` means ADMITTED BUT NOT STARTED — the job is in
			// the node's backlog waiting for one of its concurrency slots — and
			// `running` means a seat is actually working on it. A node too old
			// to have a queue simply never lingers in `accepted`, so it accrues
			// no credit and behaves exactly as it did before.
			if state == "accepted" {
				queuedPolls++
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
					pr.orphanable = true // still queued on the node — it may run later; recovery's case
					pr.Err = fmt.Sprintf("queue deadline after %s: the node accepted the job but never started it — it waited in the node's backlog and never reached running (%d poll(s) answered `accepted`)",
						queuedWaitBudget, queuedPolls)
					return pr
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
			return pr
		case <-time.After(jitteredWithin(pollEvery, time.Until(deadline))):
		}
	}
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
// 3-value shape every caller but runRemote's 503 courtesy retry (item 7,
// register D-105/D-106) uses.
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
	if r.quarantine == nil {
		return
	}
	for _, f := range failures {
		if strings.Contains(f, "(?P<docanchor>") {
			if r.quarantine.Strike(base) {
				r.quarantined.Add(1)
				log.Printf("delegate: node %s quarantined for %s after 2 off-document answers (document fingerprint failed twice)", base, DefaultQuarantineTTL)
			}
			return
		}
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
	if s := r.memoisedProbe(); s != nil {
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
func (r *runner) memoisedProbe() *probeSnapshot {
	if fetchViewsMemoTTL <= 0 {
		return nil
	}
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.probeMemo == nil || time.Since(r.probeMemo.at) >= fetchViewsMemoTTL {
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
func (r *runner) reservedDefer(local NodeView, info gpulease.Info, waited time.Duration, why string) PlacedResult {
	reason := why
	if !strings.Contains(why, "reserved") {
		reason = "local seat reserved (" + HolderLine(info) + "); " + why
	}
	reason += fmt.Sprintf("; waited %s (agent_lease_wait_sec=%d) — set agent_lease_wait_sec to wait longer, add a remote, or release the lease", waited.Round(time.Second), r.cfg.AgentLeaseWaitSec)
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
// a fleet, "qube-2 did not" is one ssh away.
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
	// endpoint is the Lenovo's arm) is that box's work: name the node after
	// the endpoint host, as a remote placement is named after its base
	// (register C-58: PAIR showed the Qube doing the Lenovo's work).
	if host := modelaffinity.EndpointHost(r.cfg.Endpoint); host != "" {
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
	line := delegationLogLine{
		TS:                 time.Now().Unix(),
		JobID:              pr.JobID,
		Node:               pr.Node,
		Seat:               pr.Seat,
		PlacementReason:    pr.PlacementReason,
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
			Deferred:     pr.Result.Deferred || pr.Err != "" || len(pr.AcceptanceFailures) > 0,
			Reason:       reason,
			// ModelTier carries placement:seat — the ledger has no placement
			// column, and "which node/seat ran it" is the row's whole story.
			ModelTier: pr.Node + ":" + pr.Seat,
			// The job behind the row (D-101 / F15): what this result already
			// knew, so a reader never has to open the corpus for it. The
			// session that asked is stamped by ledger.Record itself.
			JobID:            pr.JobID,
			Route:            r.route,
			Placement:        pr.PlacementReason,
			Steps:            pr.Result.Steps,
			StopReason:       pr.Result.StopReason,
			RepackMs:         pr.Result.RepackMs,
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
