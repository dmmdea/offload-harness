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
	// acceptance: both evalAcceptance call sites guard on !wire.Deferred and
	// every re-pack failure branch defers, so no check ever runs over it —
	// pinned by TestRunLocalDeferSkipsAcceptance.) So a subtask whose agent loop
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
	case "auto", "local", "remote":
	default:
		return nil, Summary{}, fmt.Errorf("delegate: route %q not recognized (want auto, local, or remote)", route)
	}
	if len(subtasks) == 0 {
		return nil, Summary{}, fmt.Errorf("delegate: at least one subtask required")
	}
	if len(subtasks) > maxSubtasks {
		return nil, Summary{}, fmt.Errorf("delegate: %d subtasks exceeds the max of %d", len(subtasks), maxSubtasks)
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
	results := make([]PlacedResult, len(subtasks))
	sem := make(chan struct{}, runConcurrency)
	var wg sync.WaitGroup
	for i, c := range subtasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, contract core.AgentContract) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = r.runOne(ctx, contract)
		}(i, c)
	}
	wg.Wait()

	var sum Summary
	for _, pr := range results {
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

// runOne places and executes one subtask, then verifies and records it. Every
// return path passes through finish() so no outcome can skip telemetry.
func (r *runner) runOne(ctx context.Context, contract core.AgentContract) PlacedResult {
	start := time.Now()
	// Delegator-mints-the-id (roast delta 14): minted per subtask, BEFORE
	// placement, so even a local run correlates its telemetry line.
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
	localView := NodeView{NodeID: r.localNodeID(), AgentSeat: r.cfg.AgentPlannerModel(""), Local: true}

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
	chosen := Place(st, localView, views, busy)

	var reason string
	// deadFleet marks a LOCAL placement taken while the configured fleet was
	// failing its health probe — see PlacedResult.remotesUnreachable.
	deadFleet := false
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

	if chosen.Local {
		pr := r.runLocal(ctx, contract, localView, reason)
		pr.remotesUnreachable = deadFleet
		return finish(pr)
	}
	base := ""
	for i := range views {
		if views[i] == chosen {
			base = bases[i]
			break
		}
	}
	if base == "" {
		// Unreachable (chosen came from views); kept for defense — a placement
		// with no dial target is a bug, not a defer.
		return finish(PlacedResult{Node: chosen.NodeID, PlacementReason: reason, Err: "internal: placed node has no base URL"})
	}
	pr := r.runRemote(ctx, base, jobID, contract)
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
		pr.AcceptanceFailures = evalAcceptance(contract, wire)
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
	if err := r.dispatch(ctx, base, jobID, payload); err != nil {
		pr.Err = err.Error()
		return pr
	}

	timeoutSec := contract.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = core.AgentTimeoutSecDefault
	}
	pollBudget := time.Duration(timeoutSec)*time.Second + pollGrace
	deadline := time.Now().Add(pollBudget)
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
				pr.Err = fmt.Sprintf("poll deadline after %s: %s", pollBudget, unownedDetail(sawNodeAnswer, saw404, redispatches, lastPollErr))
				return pr
			}
			// Roast delta 14: mark deferred, reason PREFIXED "poll deadline"
			// (stable key), and STOP. The node may still finish server-side; the
			// job id in the telemetry line lets an operator reconcile by hand.
			// The wording says only what is known: it acked and it never reached
			// a terminal state — not that it "could not complete the contract".
			reason := fmt.Sprintf("poll deadline after %s: node accepted the job but did not reach a terminal state", pollBudget)
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
			if err := r.dispatch(ctx, base, jobID, payload); err != nil {
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
				pr.AcceptanceFailures = evalAcceptance(contract, wire)
			}
			return pr
		case status == http.StatusOK && state == "error":
			pr.Err = "remote job error: " + jobErr
			return pr
		case status == http.StatusOK && (state == "accepted" || state == "running"):
			// The node answered AND says it owns the job: the only shape that
			// earns a defer at the deadline.
			sawNodeAnswer, sawJobOwned = true, true
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
// idempotently, so the retry can never buy a second run.
func (r *runner) dispatch(ctx context.Context, base, jobID string, payload json.RawMessage) error {
	env, err := json.Marshal(map[string]any{
		"job_id":    jobID,
		"task_type": string(core.TaskAgentRun),
		"payload":   payload,
	})
	if err != nil {
		return fmt.Errorf("marshaling dispatch envelope: %w", err)
	}
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/dispatch"
	var lastErr error
	for attempt := 0; attempt < dispatchAttempts; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, dispatchRequestTimeout)
		req, rerr := http.NewRequestWithContext(rctx, http.MethodPost, u, bytes.NewReader(env))
		if rerr != nil {
			cancel()
			return fmt.Errorf("dispatch request: %w", rerr)
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
			// answer, not doubt; retrying the same bytes would get the same
			// answer, so surface it.
			return fmt.Errorf("dispatch %s: status %d: %s", u, resp.StatusCode, truncate(body, 256))
		}
		return nil
	}
	return lastErr
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

// evalAcceptance runs every contract acceptance check against the result —
// DELEGATOR-side, before merge (roast delta 3). An unparseable check fails
// closed with the parse error as the failure: Validate should have caught it,
// and an unmet precondition is a failed check, never a skipped one.
func evalAcceptance(contract core.AgentContract, wire core.AgentWireResult) []string {
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
	TS                 int64              `json:"ts"`
	JobID              string             `json:"job_id"`
	Node               string             `json:"node"`
	Seat               string             `json:"seat"`
	PlacementReason    string             `json:"placement_reason"`
	Deferred           bool               `json:"deferred"`
	DeferClass         string             `json:"defer_class,omitempty"`
	AcceptancePass     bool               `json:"acceptance_pass"`
	WallMs             int64              `json:"wall_ms"`
	EstTokens          int                `json:"est_tokens"`
	Error              string             `json:"error,omitempty"`
	Contract           core.AgentContract `json:"contract"`
	Result             *core.AgentWireResult `json:"result,omitempty"`
	AcceptanceFailures []string           `json:"acceptance_failures,omitempty"`
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
