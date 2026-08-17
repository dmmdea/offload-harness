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
// buy a duplicate run. Polling stops hard at TimeoutSec + a grace window;
// past it the subtask is marked deferred (reason "poll deadline"), because a
// delegator that polls forever pins a goroutine on a dead node.

package delegate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
		case len(pr.AcceptanceFailures) > 0:
			sum.FailedVerification++
		default:
			sum.Succeeded++
		}
	}
	return results, sum, nil
}

type runner struct {
	cfg     config.Config
	local   LocalRunner
	route   string
	remotes []string
	led     *ledger.Ledger
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
	if busy && r.route != "local" {
		views, bases = r.fetchViews(ctx)
	}
	chosen := Place(st, localView, views, busy)

	var reason string
	switch {
	case r.route == "local":
		reason = "route=local forced"
	case r.route == "remote" && chosen.Local:
		// An explicit remote route with nothing eligible must NOT silently
		// fall local — defer loudly and let the caller decide.
		return finish(PlacedResult{
			Node: localView.NodeID, Seat: localView.AgentSeat,
			PlacementReason: "route=remote: no eligible remote",
			Result:          core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, Reason: "route=remote: no eligible remote node (gate: enabled+resident+ctx-fit+schema)"},
		})
	case r.route == "remote":
		reason = "route=remote forced → " + chosen.NodeID
	case !busy:
		reason = "local idle"
	case chosen.Local:
		reason = "local busy; no eligible remote (queued-local beats ineligible-remote)"
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
	pr.AcceptanceFailures = evalAcceptance(contract, wire)
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
	deadline := time.Now().Add(time.Duration(timeoutSec)*time.Second + pollGrace)
	redispatches := 0
	for {
		if err := ctx.Err(); err != nil {
			pr.Err = "canceled: " + err.Error()
			return pr
		}
		if time.Now().After(deadline) {
			// Roast delta 14, verbatim: mark deferred, reason "poll deadline",
			// and STOP. The node may still finish server-side; the job id in
			// the telemetry line lets an operator reconcile by hand.
			pr.Result = core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true, Reason: "poll deadline"}
			return pr
		}
		state, data, jobErr, status, perr := r.pollOnce(ctx, base, jobID)
		switch {
		case perr != nil:
			// Transient transport noise: keep polling until the deadline —
			// the deadline, not one dropped packet, decides abandonment.
		case status == http.StatusNotFound:
			// The node never saw (or forgot) the job: the lost-ack shape.
			// Re-dispatch the SAME id — the store's duplicate path re-acks
			// 202 without a second run — bounded so a state-losing node
			// cannot be made to re-run the contract forever.
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
		case state == "done":
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
		case state == "error":
			pr.Err = "remote job error: " + jobErr
			return pr
		}
		select {
		case <-ctx.Done():
			pr.Err = "canceled: " + ctx.Err().Error()
			return pr
		case <-time.After(pollEvery):
		}
	}
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
// views and a PARALLEL base-URL slice (NodeView carries no base — placement
// is pure; the pairing is the runner's business). A node that cannot answer
// is simply not a candidate — the gate's fail-toward-local posture.
func (r *runner) fetchViews(ctx context.Context) ([]NodeView, []string) {
	var views []NodeView
	var bases []string
	for _, base := range r.remotes {
		v, err := FetchNodeView(ctx, base, r.cfg.FleetAuthToken)
		if err != nil {
			continue
		}
		views = append(views, v)
		bases = append(bases, base)
	}
	return views, bases
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
	_ = appendDelegationLog(r.cfg.BaseDir(), line)

	if r.led != nil {
		reason := pr.Err
		if reason == "" && pr.Result.Deferred {
			reason = pr.Result.Reason
		}
		if reason == "" && len(pr.AcceptanceFailures) > 0 {
			reason = "failed verification: " + pr.AcceptanceFailures[0]
		}
		_ = r.led.Record(ledger.Entry{
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
		})
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
