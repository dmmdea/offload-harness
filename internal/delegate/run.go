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
// What the deadline PRODUCES depends on whether the node ever answered about
// the job: one that answered gets an honest "poll deadline …" defer, one that
// never answered gets a FAILURE. The delegator may report what a node said; it
// may never author a report on a silent node's behalf.

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
}

// Summary is the per-run outcome tally, reported AT THE TOP of every surface's
// result (roast delta 14): eight quiet defers must read as a loud outcome,
// not eight green jobs.
type Summary struct {
	Succeeded          int
	Deferred           int
	FailedVerification int
	Failed             int
	// Infrastructure counts the SUBSET of Deferred whose defer_class says the
	// stack or the config — not the work — is what failed (infrastructure or
	// config). It is a subset, not a fifth bucket: the totals still add up to
	// len(results) without it. It exists because a node with a dead llama-swap
	// otherwise reports exactly what a small model honestly abstaining reports,
	// and both exit 0. A non-zero Infrastructure makes the CLI exit non-zero.
	Infrastructure int
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
	if cfg.LedgerPath != "" {
		if l, err := ledger.Open(cfg.LedgerPath); err == nil {
			led = l
			defer led.Close()
		} else {
			// Not fatal — but not silent either. Without this line a run that
			// records nothing looks exactly like a run that records everything,
			// and `local-offload stats` quietly under-reports forever.
			log.Printf("delegate: ledger %s could not be opened; this run records no ledger rows: %v", cfg.LedgerPath, err)
		}
	}

	r := &runner{cfg: cfg, local: local, route: route, remotes: remotes, led: led}
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
		switch {
		case pr.Err != "":
			sum.Failed++
		case pr.Result.Deferred:
			sum.Deferred++
			if BrokenStackDefer(pr.Result.DeferClass) {
				sum.Infrastructure++
			}
		case len(pr.AcceptanceFailures) > 0:
			sum.FailedVerification++
		default:
			sum.Succeeded++
		}
	}
	return results, sum, nil
}

// BrokenStackDefer reports whether a defer_class means an OPERATOR, not a
// bigger model, has to act: the stack failed (infrastructure) or the
// node/contract combination can never work as configured (config). An empty
// class — a pre-0.63 node, which cannot classify — is deliberately NOT broken:
// assuming the worst about an older peer would fail every mixed-version fleet.
func BrokenStackDefer(class string) bool {
	return class == core.DeferClassInfrastructure || class == core.DeferClassConfig
}

type runner struct {
	cfg     config.Config
	local   LocalRunner
	route   string
	remotes []string
	led     *ledger.Ledger
	// warnCorpus/warnLedger fire at most ONE warning each per Run. Once, not
	// per subtask: an 8-subtask fan-out against a full disk would otherwise
	// print the same line eight times and bury the results it is warning about.
	warnCorpus sync.Once
	warnLedger sync.Once
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
	switch {
	case r.route == "local":
		reason = "route=local forced"
	case r.route == "remote" && chosen.Local:
		// An explicit remote route with nothing eligible must NOT silently
		// fall local — defer loudly and let the caller decide. The diagnosis
		// distinguishes the three causes that used to share one sentence.
		why, class := r.noEligibleRemote(views, probeErrs)
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
		why, _ := r.noEligibleRemote(views, probeErrs)
		reason = "local busy; no eligible remote — " + why + " (queued-local beats ineligible-remote)"
	default:
		reason = "local busy; placed on " + chosen.NodeID
	}

	if chosen.Local {
		return finish(r.runLocal(ctx, contract, localView, reason))
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
	// A poll failure is NOT nothing. Before these two, pollOnce's error was
	// bound and dropped, so a node that died after acking (refused dial, per-poll
	// deadline, 500/502/503, a 200 whose body is not JSON) ran out the clock and
	// was handed a MANUFACTURED defer that runOne then stamped with that node's
	// id and seat — the delegator inventing a sentence the node never said, and
	// exiting 0. lastPollErr keeps the newest failure for the operator;
	// sawNodeAnswer records whether the node ever answered about this job AT
	// ALL, and that is what decides defer-vs-failure at the deadline.
	var lastPollErr error
	sawNodeAnswer := false
	for {
		if err := ctx.Err(); err != nil {
			pr.Err = "canceled: " + err.Error()
			return pr
		}
		if time.Now().After(deadline) {
			if !sawNodeAnswer {
				// Never a defer: nothing on that node ever reported anything, so
				// there is no defer to report. A FAILURE (Summary.Failed, non-zero
				// CLI exit) is the honest outcome — a broken node must read broken.
				pr.Err = fmt.Sprintf("poll deadline after %s: node never answered (last: %s)", pollBudget, errText(lastPollErr))
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
			log.Printf("delegate: poll %s at %s failed: %v", jobID, base, perr)
		case status == http.StatusNotFound:
			// The node answered — with "I have no such job": the lost-ack shape.
			// Re-dispatch the SAME id — the store's duplicate path re-acks
			// 202 without a second run — bounded so a state-losing node
			// cannot be made to re-run the contract forever.
			sawNodeAnswer = true
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
			sawNodeAnswer = true
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
			// The node answered and owns the job: the only shape that earns a
			// defer at the deadline.
			sawNodeAnswer = true
		default:
			// Anything else IS an answer we cannot act on — a 500/502/503 from
			// the node or something in front of it, or a 200 carrying a state
			// this delegator does not know. Something answered, so this is not
			// the never-answered shape; keep polling (the state may still
			// resolve) but record it, because silently discarding it is what let
			// a broken node look like a busy one.
			sawNodeAnswer = true
			lastPollErr = fmt.Errorf("poll: unusable answer (status %d, state %q)", status, state)
			log.Printf("delegate: poll %s at %s: %v", jobID, base, lastPollErr)
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
			log.Printf("delegate: health probe of %s failed (not a placement candidate): %v", base, err)
			probeErrs = append(probeErrs, err.Error())
			continue
		}
		views = append(views, v)
		bases = append(bases, base)
	}
	return views, bases, probeErrs
}

// noEligibleRemote turns "nothing eligible" into the cause an operator can act
// on, plus the defer class that goes with it. Three distinct situations used
// to share one manufactured sentence that always blamed the gate:
//
//	no remotes configured        → config: nothing was ever asked to run this
//	every remote failed to answer → infrastructure: the fleet is down/unreachable
//	remotes answered, gate said no → config: the gate really is the reason
//
// A mix (some answered and failed the gate, some never answered) reports both
// and is classed infrastructure — part of the fleet is genuinely broken.
func (r *runner) noEligibleRemote(views []NodeView, probeErrs []string) (reason, class string) {
	switch {
	case len(r.remotes) == 0:
		return "no remote fleet nodes are configured (pass --remote / the remotes argument)", core.DeferClassConfig
	case len(views) == 0 && len(probeErrs) > 0:
		return fmt.Sprintf("all %d configured remote(s) failed the health probe: %s", len(probeErrs), strings.Join(probeErrs, "; ")), core.DeferClassInfrastructure
	case len(probeErrs) > 0:
		return fmt.Sprintf("no remote passed the capability gate (enabled+resident+ctx-fit+schema); %d other remote(s) failed the health probe: %s",
			len(probeErrs), strings.Join(probeErrs, "; ")), core.DeferClassInfrastructure
	default:
		return "no remote passed the capability gate (enabled+resident+ctx-fit+schema)", core.DeferClassConfig
	}
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
	if err := appendDelegationLog(r.cfg.BaseDir(), line); err != nil {
		// The corpus is a DELIVERABLE of this lane (the standing small-model
		// agent-task dataset), not incidental logging: an unwritable corpus is
		// worth an operator's attention even though it can never fail the work.
		r.warnCorpus.Do(func() {
			log.Printf("delegate: delegation-log write failed under %s; this run's corpus rows are LOST (results unaffected): %v", r.cfg.BaseDir(), err)
		})
	}

	if r.led != nil {
		reason := pr.Err
		if reason == "" && pr.Result.Deferred {
			reason = pr.Result.Reason
		}
		if reason == "" && len(pr.AcceptanceFailures) > 0 {
			reason = "failed verification: " + pr.AcceptanceFailures[0]
		}
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
