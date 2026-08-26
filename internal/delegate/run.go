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
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

// LocalRunner executes one contract in-process on the local node — the same
// read-only agent.Build path a fleet node runs (pipeline.RunAgentContract
// satisfies it). A seam rather than a *pipeline.Pipeline so this routing
// package does not drag the whole media pipeline into its dependency graph,
// and so tests fake local execution with a closure.
type LocalRunner func(ctx context.Context, contract core.AgentContract) (core.AgentWireResult, error)

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
	PlacementReason    string
	// Err is non-empty when the subtask FAILED for transport/config reasons
	// (dispatch refused, auth rejected, undecodable result). Counted in
	// Summary.Failed, never in Deferred — eight quiet defers and one broken
	// wire are different outcomes and must read differently.
	Err string
	// wallMs is the DELEGATOR-observed wall (placement through verification) —
	// the round-trip number the break-even telemetry wants; the node's own
	// wall stays in Result.WallMs.
	wallMs int64
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
	ranLocal       bool
	// ranBase is the dial base this attempt used ("" for local). It is what the
	// re-placement loop excludes on, rather than the node id: a node that
	// answered health without a node_id would otherwise collide with every
	// other such node under one empty key, and the dial target is the thing
	// actually being re-tried.
	ranBase string
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
}

// Summary is the per-run outcome tally, reported AT THE TOP of every surface's
// result (roast delta 14): eight quiet defers must read as a loud outcome,
// not eight green jobs.
type Summary struct {
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
}

const (
	// maxSubtasks bounds one Run (the MCP arg schema mirrors it).
	maxSubtasks = 8
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
	// may be offered to after its first-choice node refused it at dispatch.
	// Local is NOT counted against it — the fallback to the one seat that
	// always exists is reserved, so a wide roster can never spend the bound
	// before reaching it. Total placements per subtask are therefore at most
	// 1 (first choice) + maxRemoteReplacements + 1 (local) = 4.
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

// pollEvery/pollGrace are vars (not consts) so tests compress the cadence;
// production never mutates them. Grace rides ON TOP of the contract's
// TimeoutSec: the node enforces TimeoutSec as its own wall, so the delegator
// allows that plus transport slack before declaring the poll dead.
var (
	pollEvery = 3 * time.Second
	pollGrace = 60 * time.Second
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
func Run(ctx context.Context, cfg config.Config, local LocalRunner, subtasks []core.AgentContract, route string, remotes []string) ([]PlacedResult, Summary, error) {
	switch route {
	case "":
		route = "auto"
	case "auto", "local", "remote", "spread":
	default:
		return nil, Summary{}, fmt.Errorf("delegate: route %q not recognized (want auto, spread, local, or remote)", route)
	}
	if len(subtasks) == 0 {
		return nil, Summary{}, fmt.Errorf("delegate: at least one subtask required")
	}
	if len(subtasks) > maxSubtasks {
		return nil, Summary{}, fmt.Errorf("delegate: %d subtasks exceeds the max of %d", len(subtasks), maxSubtasks)
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

	r := &runner{cfg: cfg, local: local, route: route, remotes: remotes, led: led, ledgerUnopened: ledgerUnopened}
	// route=spread probes the fleet ONCE per run: every subtask deals itself
	// across the same roster, so per-subtask probing would be N identical GETs
	// and could even deal two subtasks against different snapshots.
	if route == "spread" {
		r.spreadViews, r.spreadBases, r.spreadProbeErrs = r.fetchViews(ctx)
		// The deal is computed HERE, once, over every subtask at once —
		// dealSpread's comment carries the proof that a per-subtask pick cannot
		// hold the one-per-seat-per-cycle invariant. It must run before the
		// goroutines below: they read it, they never build it.
		r.spreadDeal = r.dealSpread(subtasks, r.localView())
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
		if pr.RetriedOn != "" {
			sum.Retried++
		}
		if pr.retryRecovered {
			sum.RetryRecovered++
		}
		if pr.Replacements > 0 {
			sum.Replaced++
			// "Recovered" is stated on the REFUSAL, not on the answer: Err == ""
			// means some node took the work and reported on it. Whether that
			// report was good is what the four buckets below are for.
			if pr.Err == "" {
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
	led     *ledger.Ledger
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
}

// placement is a resolved "run it HERE" — the node, its dial base ("" for
// local) and the human-readable reason that rides the result.
type placement struct {
	view   NodeView
	base   string
	reason string
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
	// The subtask's timeout_sec is the WALL CEILING the caller was told about,
	// and every mechanism inside runOne spends from this one budget: the
	// re-placement loop below and the verification retry both measure what is
	// left against `start`, so neither can silently extend the other's.
	budget := contract.TimeoutSec
	if budget <= 0 {
		budget = core.AgentTimeoutSecDefault
	}
	first := r.placeAndRun(ctx, i, contract, nil, start, budget)
	if !retryable(first) {
		return first
	}
	// The retry lives INSIDE the subtask's own timeout_sec: the caller was told
	// that number is the wall ceiling per subtask, and a second full attempt would
	// have doubled it silently. What is left after the first attempt is the
	// retry's budget; under the floor there is no honest retry to run.
	remaining := remainingSec(start, budget)
	if remaining < minRetrySec {
		first.RetryNote = fmt.Sprintf("retry skipped: %ds of the %ds timeout_sec budget left after the first attempt (floor %ds)", remaining, budget, minRetrySec)
		return first
	}
	alt, ok := r.alternativeNode(ctx, first, contract)
	if !ok {
		return first
	}
	retryContract := contract
	retryContract.TimeoutSec = remaining
	second := r.placeAndRun(ctx, i, retryContract, &alt, start, budget)
	return mergeAttempts(first, second)
}

// remainingSec is what is LEFT of a subtask's timeout_sec budget. Elapsed
// rounds UP: crediting a 1.2 s attempt as 1 s overstates what is left, and no
// later placement may be promised time the subtask does not have. It can go
// negative, which every caller reads as "nothing left" through a floor check.
func remainingSec(start time.Time, budget int) int {
	return budget - int((time.Since(start).Milliseconds()+999)/1000)
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
// on a node nobody is polling any more — the agent lane is the read-only
// agent.Build path, so there are no effects to duplicate — and the alternative
// is losing the work with certainty. Named here rather than left implicit.
func (r *runner) placeAndRun(ctx context.Context, i int, contract core.AgentContract, forced *placement, start time.Time, budget int) PlacedResult {
	pr := r.attempt(ctx, i, contract, forced)
	if !isReplaceable(pr) {
		return pr
	}
	tried := map[string]bool{pr.ranBase: true}
	refusals := []string{refusalLine(pr)}
	for {
		// Deadline discipline, identical to the retry's: a re-placement is
		// handed what is LEFT of the contract's own timeout_sec, never a fresh
		// copy of it, and under the floor there is no honest placement to make.
		remaining := remainingSec(start, budget)
		if remaining < minRetrySec {
			return exhausted(pr, refusals, fmt.Sprintf(
				"%ds of the %ds timeout_sec budget was left after %d placement(s), under the %ds floor for another",
				remaining, budget, len(refusals), minRetrySec))
		}
		next, why, ok := r.replacementNode(ctx, contract, tried, len(refusals)-1)
		if !ok {
			return exhausted(pr, refusals, why)
		}
		tried[next.base] = true
		replaced := contract
		replaced.TimeoutSec = remaining
		pr = r.attempt(ctx, i, replaced, &next)
		pr.Replacements = len(refusals)
		if !isReplaceable(pr) {
			// Some node TOOK it. Whether its answer was any good is the four
			// outcome buckets' business, not this loop's.
			pr.ReplacementNote = replacementNote(refusals, true)
			return pr
		}
		refusals = append(refusals, refusalLine(pr))
	}
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
// used is how many re-placements have already been made, checked against
// maxRemoteReplacements. Local is deliberately NOT charged against that bound:
// it is the one seat always able to take the contract, so a wide roster must
// not be able to spend the bound before reaching it.
//
// ok=false returns the sentence the exhausted message ends with. It is built
// from BOTH facts (the bound, and whether local was available) because a reader
// who is told only one of them will look in the wrong place.
func (r *runner) replacementNode(ctx context.Context, contract core.AgentContract, tried map[string]bool, used int) (placement, string, bool) {
	boundHit := used >= maxRemoteReplacements
	if r.route != "local" && !boundHit {
		st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
		views, bases := r.spreadViews, r.spreadBases
		if r.route != "spread" {
			// Re-probe: the roster this subtask was placed against is now known
			// to be at least partly wrong (a node just refused), and health is
			// the only thing that can say which of the others has room.
			views, bases, _ = r.fetchViews(ctx)
		}
		freshViews, freshBases := untried(views, bases, tried)
		if chosen := Place(st, r.localView(), freshViews, true); !chosen.Local {
			return placement{
				view:   chosen,
				base:   baseFor(chosen, freshViews, freshBases),
				reason: fmt.Sprintf("re-placed on %s after %d refusal(s)", chosen.NodeID, used+1),
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
	if tried[""] {
		return placement{}, head + ", and the local seat had already been tried", false
	}
	return placement{
		view:   r.localView(),
		reason: fmt.Sprintf("re-placed on the local seat after %d refusal(s) — queued-local beats work nobody ran", used+1),
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
func exhausted(last PlacedResult, refusals []string, why string) PlacedResult {
	last.Replacements = len(refusals) - 1
	last.ReplacementNote = replacementNote(refusals, false)
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
func retryable(pr PlacedResult) bool {
	if pr.Err != "" {
		return false
	}
	if len(pr.AcceptanceFailures) > 0 {
		return true
	}
	return pr.Result.Deferred && pr.Result.DeferClass == core.DeferClassAbstention
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
func (r *runner) alternativeNode(ctx context.Context, first PlacedResult, contract core.AgentContract) (placement, bool) {
	st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
	localView := r.localView()
	why := attemptOutcome(first)
	if !first.ranLocal {
		return placement{view: localView, reason: "retry on local after " + first.Node + " " + why}, true
	}
	if r.route == "local" {
		return placement{}, false
	}
	views, bases := r.spreadViews, r.spreadBases
	if r.route != "spread" {
		views, bases, _ = r.fetchViews(ctx)
	}
	chosen := Place(st, localView, views, true)
	if chosen.Local {
		return placement{}, false
	}
	return placement{view: chosen, base: baseFor(chosen, views, bases), reason: "retry on " + chosen.NodeID + " after local " + why}, true
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
	if clean {
		second.RetriedOn = second.Node
		second.RetryNote = "first attempt on " + first.Node + " " + attemptOutcome(first) + "; this result is the retry"
		second.retryRecovered = true
		return carryReplacements(first, second)
	}
	first.RetriedOn = second.Node
	first.RetryNote = "retry on " + second.Node + " also " + attemptOutcome(second) + "; this result is the first attempt"
	return carryReplacements(second, first)
}

// carryReplacements folds the LOSING attempt's re-placement history onto the
// published one. A node that refused the first attempt still refused it even
// when the retry is what gets published, and dropping that would make
// summary.replaced silently under-report a fleet shedding load — the exact
// blindness this release exists to remove.
func carryReplacements(from, to PlacedResult) PlacedResult {
	if from.Replacements == 0 {
		return to
	}
	to.Replacements += from.Replacements
	if to.ReplacementNote == "" {
		to.ReplacementNote = from.ReplacementNote
	} else {
		to.ReplacementNote = from.ReplacementNote + "; then " + to.ReplacementNote
	}
	return to
}

// baseFor resolves the dial base of a chosen remote view ("" when absent).
func baseFor(chosen NodeView, views []NodeView, bases []string) string {
	for i := range views {
		if views[i] == chosen {
			return bases[i]
		}
	}
	return ""
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
	// dealt holds the node ids already given a subtask in the CURRENT cycle.
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
// The LOCAL rotation slot is never contested, and that is a deliberate bound on
// this heuristic, not an oversight:
//
//   - Subtask 0 lands local, whatever its shape — the documented guarantee that
//     a spread's first subtask (and therefore a SINGLE-subtask spread, the
//     riskiest case for any shape heuristic) stays on-box. One regex match must
//     not be able to send an entire run off-box.
//   - The same holds for every later local slot (i mod len == 0), because the
//     fit score ranks seats by their ADVERTISED context ceiling and the local
//     seat advertises none in a delegator run. Scoring it would mean inventing
//     a number for it; leaving it in the rotation keeps its share of the fan-out
//     exactly as before, which is also what stops an all-reasoning fan-out from
//     collapsing back onto one seat.
//
// Widening the contest to the local slot is a small change once the local seat
// advertises a ceiling of its own — it is not blocked, it is unearned.
func (r *runner) placeSpread(i int, st Subtask, localView NodeView, dealt map[string]bool) spreadSlot {
	nodes := []NodeView{localView}
	bases := []string{""}
	for j, v := range r.spreadViews {
		if remoteEligible(st, v) {
			nodes = append(nodes, v)
			bases = append(bases, r.spreadBases[j])
		}
	}
	if len(nodes) == 1 {
		why, class := r.noEligibleRemote(st, r.spreadViews, r.spreadProbeErrs)
		return spreadSlot{placement{view: localView, reason: "route=spread: no eligible remote — local (" + why + ")"}, class == core.DeferClassInfrastructure}
	}
	slot := i % len(nodes)
	if nodes[slot].Local {
		// A local slot opens a new cycle: the deck of remotes is reshuffled, so
		// the next len(nodes)-1 subtasks deal one to each seat again.
		clear(dealt)
		return spreadSlot{placement{view: localView, reason: fmt.Sprintf("route=spread → local (slot %d of %d)", slot+1, len(nodes))}, false}
	}
	k := fitPick(st, nodes, slot, dealt)
	if k < 0 {
		// Every eligible remote has already taken a subtask this cycle — which a
		// ragged eligible set can reach without passing through a local slot.
		// Reshuffle rather than stack: after the clear a pick always exists,
		// because len(nodes) > 1 guarantees at least one remote.
		clear(dealt)
		k = fitPick(st, nodes, slot, dealt)
	}
	dealt[nodes[k].NodeID] = true
	kind, rule := shapeOf(st)
	return spreadSlot{placement{view: nodes[k], base: bases[k],
		reason: fmt.Sprintf("route=spread → %s (slot %d of %d, fit=%s/%s)", nodes[k].NodeID, slot+1, len(nodes), kind, rule)}, false}
}

// fitPick returns the index of the best-scoring seat in nodes that is neither
// local nor already dealt this cycle, or -1 when the cycle has no seat left.
// The scan starts at the rotation slot and wraps, and only a STRICTLY better
// score displaces the incumbent — so equal seats are dealt in rotation order
// and an all-equal roster behaves exactly as blind round-robin did.
func fitPick(st Subtask, nodes []NodeView, slot int, dealt map[string]bool) int {
	k, best := -1, 0
	for c := 0; c < len(nodes); c++ {
		j := (slot + c) % len(nodes)
		if nodes[j].Local || dealt[nodes[j].NodeID] {
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
		r.record(contract, pr)
		return pr
	}

	// Defensive re-validate: the surfaces run PrepareContract, but Run is an
	// exported engine — an invalid contract must die here, before any network.
	if err := contract.Validate(); err != nil {
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
	default:
		// Placement. Health is fetched ONLY when a remote could actually be
		// chosen (route=remote, or route=auto with the local GPU spoken for):
		// Place ignores remotes entirely when the local node is idle, so probing
		// them would be pure chatter.
		busy := false
		switch r.route {
		case "remote":
			busy = true // forced remote behaves as "local unavailable" for Place
		case "auto":
			busy = LocalBusy(r.cfg.GPULockPath, r.cfg.StateDir)
		}
		var views []NodeView
		var bases []string
		var probeErrs []string
		if busy && r.route != "local" {
			views, bases, probeErrs = r.fetchViews(ctx)
		}
		chosen = Place(st, localView, views, busy)

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
		pr := r.runLocal(ctx, contract, localView, reason)
		pr.remotesUnreachable = deadFleet
		pr.ranLocal = true
		return finish(pr)
	}
	if base == "" {
		// Unreachable (chosen came from views); kept for defense — a placement
		// with no dial target is a bug, not a defer.
		return finish(PlacedResult{Node: chosen.NodeID, PlacementReason: reason, Err: "internal: placed node has no base URL"})
	}
	pr := r.runRemote(ctx, base, jobID, contract)
	pr.ranBase = base
	pr.PlacementReason = reason
	if pr.Node == "" {
		pr.Node = chosen.NodeID
	}
	if pr.Seat == "" {
		pr.Seat = chosen.AgentSeat
	}
	return finish(pr)
}

// runLocal executes in-process via the LocalRunner seam and shapes the result.
func (r *runner) runLocal(ctx context.Context, contract core.AgentContract, view NodeView, reason string) PlacedResult {
	pr := PlacedResult{Node: view.NodeID, Seat: view.AgentSeat, PlacementReason: reason}
	if r.local == nil {
		pr.Err = "no local runner wired (delegator surfaces must supply one)"
		return pr
	}
	wire, err := r.local(ctx, contract)
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
	if !wire.Deferred {
		// Same guard runRemote applies: a defer produced no answer, so running
		// acceptance over it manufactures "failures" about content that was
		// never claimed — turning an honest defer into a verification failure
		// in the ledger and the corpus.
		pr.AcceptanceFailures = EvalAcceptance(contract, wire)
	}
	return pr
}

// runRemote drives the fleet wire: dispatch (202 ack, retried once on
// transport doubt under the SAME job id), then poll to a terminal state or
// the poll deadline.
func (r *runner) runRemote(ctx context.Context, base, jobID string, contract core.AgentContract) PlacedResult {
	var pr PlacedResult
	payload, err := json.Marshal(contract)
	if err != nil {
		pr.Err = "marshaling contract: " + err.Error()
		return pr
	}
	if refused, status, err := r.dispatch(ctx, base, jobID, payload); err != nil {
		pr.Err = err.Error()
		// Carry the class so runOne can decide whether ANOTHER node is worth
		// asking. Set here and nowhere else: this is the one moment at which
		// the node has answered and no seat can possibly hold the contract.
		pr.refused, pr.refusalStatus = refused, status
		return pr
	}

	timeoutSec := contract.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = core.AgentTimeoutSecDefault
	}
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
	pollBudget := time.Duration(timeoutSec)*time.Second + pollGrace
	start := time.Now()
	deadline := start.Add(pollBudget)
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
			return pr
		}
		if time.Now().After(deadline) {
			if !sawJobOwned {
				// Never a defer: nothing on that node ever reported OWNING this
				// job, so there is no defer to report. A FAILURE (Summary.Failed,
				// non-zero CLI exit) is the honest outcome — a broken node, or one
				// denying the job, must read broken.
				pr.Err = fmt.Sprintf("poll deadline after %s%s: %s", pollBudget, queuedNote(queuedCredit), unownedDetail(sawNodeAnswer, saw404, redispatches, lastPollErr))
				return pr
			}
			// Roast delta 14: mark deferred, reason PREFIXED "poll deadline"
			// (stable key), and STOP. The node may still finish server-side; the
			// job id in the telemetry line lets an operator reconcile by hand.
			// The wording says only what is known: it acked and it never reached
			// a terminal state — not that it "could not complete the contract".
			reason := fmt.Sprintf("poll deadline after %s%s: node accepted the job but did not reach a terminal state", pollBudget, queuedNote(queuedCredit))
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
			return pr
		}
		state, data, jobErr, status, perr := r.pollOnce(ctx, base, jobID)
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
					// from `start` rather than incremented, so a capped credit
					// cannot drift the deadline past the intended bound.
					deadline = start.Add(pollBudget + queuedCredit)
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
		case <-time.After(pollEvery):
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

// dispatch POSTs the job envelope, expecting the contract's one acceptance
// shape (202). A transport-level failure is retried ONCE with the same job id
// — if the first POST actually landed, the node's known-job path re-acks
// idempotently, so the retry can never buy a second run. That bounded retry
// (dispatchAttempts) is about DOUBT over one node's transport and is entirely
// separate from re-placement, which is about a node that answered no.
//
// The returns say which of three things happened:
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
func (r *runner) dispatch(ctx context.Context, base, jobID string, payload json.RawMessage) (refused bool, status int, err error) {
	env, merr := json.Marshal(map[string]any{
		"job_id":    jobID,
		"task_type": string(core.TaskAgentRun),
		"payload":   payload,
	})
	if merr != nil {
		return false, 0, fmt.Errorf("marshaling dispatch envelope: %w", merr)
	}
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/dispatch"
	var lastErr error
	for attempt := 0; attempt < dispatchAttempts; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, dispatchRequestTimeout)
		req, rerr := http.NewRequestWithContext(rctx, http.MethodPost, u, bytes.NewReader(env))
		if rerr != nil {
			cancel()
			return false, 0, fmt.Errorf("dispatch request: %w", rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		if r.cfg.FleetAuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+r.cfg.FleetAuthToken)
		}
		resp, derr := fleetClient.Do(req)
		if derr != nil {
			cancel()
			lastErr = fmt.Errorf("dispatch %s: %w", u, derr)
			continue // transport doubt → one more POST, same job id
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusAccepted {
			// A non-202 is the node REFUSING (400/401/403/409/503) — an
			// answer, not doubt; re-POSTing the same bytes to the SAME node
			// would get the same answer. Whether ANOTHER node is worth asking
			// is replaceableRefusal's decision, made from this status.
			return true, resp.StatusCode, fmt.Errorf("dispatch %s: status %d: %s", u, resp.StatusCode, truncate(body, 256))
		}
		return false, resp.StatusCode, nil
	}
	// Both POSTs failed at transport level: the delegator never reached this
	// node. Status 0 says exactly that — no node authored this answer.
	return true, 0, lastErr
}

// pollOnce GETs the job state. perr covers transport-level failure only; an
// HTTP answer (any status) comes back as (state, data, jobErr, status, nil).
func (r *runner) pollOnce(ctx context.Context, base, jobID string) (state string, data json.RawMessage, jobErr string, status int, perr error) {
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/jobs/" + jobID
	rctx, cancel := context.WithTimeout(ctx, pollRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return "", nil, "", 0, err
	}
	if r.cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.cfg.FleetAuthToken)
	}
	resp, err := fleetClient.Do(req)
	if err != nil {
		return "", nil, "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFleetBody))
	if err != nil {
		return "", nil, "", 0, err
	}
	var wire struct {
		State string          `json:"state"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if resp.StatusCode == http.StatusOK {
		if uerr := json.Unmarshal(body, &wire); uerr != nil {
			return "", nil, "", 0, fmt.Errorf("job poll %s: not JSON: %w", u, uerr)
		}
	}
	return wire.State, wire.Data, wire.Error, resp.StatusCode, nil
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
		if pass, reason := chk.Eval(wire.Structured, wire.Output); !pass {
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
func (r *runner) fetchViews(ctx context.Context) (views []NodeView, bases []string, probeErrs []string) {
	for _, base := range r.remotes {
		v, err := FetchNodeView(ctx, base, r.cfg.FleetAuthToken)
		if err != nil {
			// Logged once per BASE per RUN, not once per (base, subtask):
			// fetchViews is called inside runOne, so an 8-subtask fan-out at two
			// dead remotes printed sixteen identical lines. The error still rides
			// probeErrs on every subtask — the defer REASON must name the cause
			// for each result even when the log line is spent.
			if _, warned := r.probeWarned.LoadOrStore(base, struct{}{}); !warned {
				log.Printf("delegate: health probe of %s failed (not a placement candidate): %v", base, err)
			}
			probeErrs = append(probeErrs, err.Error())
			continue
		}
		views = append(views, v)
		bases = append(bases, base)
	}
	return views, bases, probeErrs
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
func (r *runner) noEligibleRemote(st Subtask, views []NodeView, probeErrs []string) (reason, class string) {
	if len(r.remotes) == 0 {
		return "no remote fleet nodes are configured (pass --remote / the remotes argument)", core.DeferClassConfig
	}
	lanes, advertisedTooSmall, roomiest, unadvertised := laneStats(st, views)
	contractWhy := contractIneligible(st, lanes, advertisedTooSmall, roomiest, len(unadvertised))
	nodeWhy, nodeClass := nodeSideVerdict(lanes, unadvertised, views, probeErrs)
	if nodeClass == "" {
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
		r.ledgerTried.Add(1)
		if err := r.led.Record(ledger.Entry{
			Task:      "agent_delegate",
			LatencyMs: pr.wallMs,
			// TokensIn stays 0 ON PURPOSE: the summary counts a completed
			// row's TokensIn as tokens-saved, and a delegation row claiming
			// savings would double-count the node-side agent row.
			TokensOut: pr.Result.TokensOut,
			Deferred:  pr.Result.Deferred || pr.Err != "" || len(pr.AcceptanceFailures) > 0,
			Reason:    reason,
			// ModelTier carries placement:seat — the ledger has no placement
			// column, and "which node/seat ran it" is the row's whole story.
			ModelTier: pr.Node + ":" + pr.Seat,
		}); err != nil {
			r.ledgerLost.Add(1)
			r.warnLedger.Do(func() {
				log.Printf("delegate: ledger row write failed for %s; this run's savings accounting is incomplete (results unaffected): %v", r.cfg.LedgerPath, err)
			})
		}
	}
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
