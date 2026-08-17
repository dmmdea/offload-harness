// Node-side execution of a fleet "agent" delegation contract (multi-node
// delegation Task 4, §S2). runAgentTask drives the SAME agent.Build loop the
// MCP front door's agent_run uses — read-only tools over the job's context
// dir, recordless offload, no write/run/fetch/github capability — then
// re-packs the loop's final text into the contract's OutputSchema with ONE
// grammar-constrained completion on the same seat.
//
// Result contract: the pipeline result's Data is a marshaled
// core.AgentWireResult and OK is TRUE for every terminal outcome, defers
// included — a defer is a SUCCESS shape at the JOB level (the node did its
// job: it reported it could not complete the contract), mirroring the
// cascade's defer semantics; the fleet job must land terminal-done, never
// error. OK:false is reserved for internal wiring bugs (params missing), where
// an error-state job is the honest answer.
//
// Quarantine by construction: the wire result is assembled from a typed
// struct whose fields never include the transcript — remote reasoning cannot
// leak into the delegator's context.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/swapclient"
	"github.com/dmmdea/offload-harness/internal/tokclient"
	"github.com/dmmdea/offload-harness/internal/validator"
)

const (
	// agentRepackMaxTokens bounds the structured re-pack completion — the same
	// budget buildExtract gives the extract task's grammar output (the re-pack
	// IS an extract over the loop's final text).
	agentRepackMaxTokens = 512
	// agentRosterProbeTimeout bounds the seat-residency roster fetch — the same
	// 10s mcpserver's plannerUnserved uses.
	agentRosterProbeTimeout = 10 * time.Second
)

// runAgentTask executes one delegation contract. req.Params carries the
// DECODED contract + the materialized context dir (fleetnode.buildAgentRun
// owns decode/validation/materialization; nothing is re-validated here).
func (p *Pipeline) runAgentTask(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	// Params shape errors are internal wiring bugs (this task type is only
	// reachable through buildAgentRun), so they are honest job-level ERRORS —
	// not wire defers, which would claim "the node ran the contract and could
	// not complete it" about a contract that never ran.
	contract, ok := req.Params["contract"].(core.AgentContract)
	if !ok {
		meta.LatencyMs = time.Since(start).Milliseconds()
		return core.Result{OK: false, Reason: "agent task: params carry no decoded contract (fleetnode.buildAgentRun owns materialization)", Meta: meta}
	}
	contextDir, _ := req.Params["context_dir"].(string)
	if contextDir == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		return core.Result{OK: false, Reason: "agent task: params carry no context_dir (fleetnode.buildAgentRun owns materialization)", Meta: meta}
	}

	seat := p.cfg.AgentPlannerModel("")
	meta.Model = seat
	nodeID := p.cfg.FleetNodeID
	if nodeID == "" {
		// Same rule as /fleet/health: empty fleet_node_id = the OS hostname, so
		// a shared config never bakes one box's name into another's results.
		if hn, herr := os.Hostname(); herr == nil {
			nodeID = hn
		}
	}
	wire := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: nodeID, Seat: seat}

	// finish is the ONE exit for every terminal wire result (success or defer):
	// it stamps WallMs, records the ledger row, and wraps the marshaled result
	// as a job-level success — so no return path can forget the defer-is-done
	// contract or the telemetry.
	finish := func(w core.AgentWireResult) core.Result {
		w.WallMs = time.Since(start).Milliseconds()
		meta.LatencyMs = w.WallMs
		meta.TokensOut = w.TokensOut
		data, merr := json.Marshal(w)
		if merr != nil {
			return core.Result{OK: false, Reason: "agent task: marshaling wire result: " + merr.Error(), Meta: meta}
		}
		if w.Deferred {
			p.recordDefer(req.Task, meta, len(req.Input), w.Reason)
		} else {
			p.record(req.Task, meta, len(req.Input))
		}
		return core.Result{OK: true, Data: data, Meta: meta}
	}
	// Every defer names its CLASS (core.DeferClass*): the reason is prose for a
	// human, the class is what the delegator's exit code and the corpus can
	// branch on. Passing it as a parameter — rather than inferring it from the
	// reason string downstream — is what keeps "llama-swap is down" from
	// arriving as the same quiet green defer as "the model answered wrongly".
	deferWire := func(class, reason string) core.Result {
		w := wire
		w.Deferred = true
		w.DeferClass = class
		w.Reason = reason
		return finish(w)
	}

	if seat == "" {
		return deferWire(core.DeferClassConfig, "no agent seat resolvable (agent_model and model both empty)")
	}

	// The contract's TimeoutSec is the WALL ceiling, enforced as a context
	// deadline over everything below (probe, build, loop, re-pack) — closing
	// ground-truth gap #12 for this lane. DecodeAgentContract guarantees a
	// positive value on the wire path; the default covers in-process callers.
	timeoutSec := contract.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = core.AgentTimeoutSecDefault
	}
	wall := time.Duration(timeoutSec) * time.Second
	cctx, cancel := context.WithTimeout(ctx, wall)
	defer cancel()

	// Seat-residency probe — the mirror of mcpserver's plannerUnserved gate: a
	// POSITIVE "roster answered and the seat is absent" defers before any
	// planner call; an unreachable/empty roster proceeds and lets the loop's
	// first chat call surface the real transport error.
	if roster, rerr := swapclient.FetchRoster(cctx, p.cfg.Endpoint, agentRosterProbeTimeout); rerr != nil {
		// Fail-open is right (the loop's first chat call surfaces the real
		// transport error with far more detail), but SILENT fail-open is not:
		// this is the first place a dead or misconfigured endpoint shows, and
		// swallowing it turns the follow-on loop error into a mystery.
		log.Printf("agent task: seat roster probe of %s failed (proceeding; the loop will surface any real transport failure): %v", p.cfg.Endpoint, rerr)
	} else if roster.Len() > 0 && !roster.Serves(seat) {
		return deferWire(core.DeferClassConfig, fmt.Sprintf("agent seat %q is not in the endpoint's served roster", seat))
	}

	// Depth (roast delta 2): buildAgentRun already derived
	// contract.Depth = max(1, wireDepth). Nothing here consumes it YET because
	// agent.BuildConfig has no depth field and v1's Build registers no delegate
	// tool for ANY caller — the hop limit holds structurally (the tool does not
	// exist), so depth denies nothing node-side in v1. When the delegate tool
	// lands (plan Task 6, v2 for the in-loop surface), its depth==0-only
	// registration must key off contract.Depth here.
	//
	// The Build mirrors mcpserver.handleAgentRun's read-only front door: NO
	// write/run/fetch/github capability, recordless offload on the workhorse
	// seat (the in-loop cascade keeps workhorse economics; the PLANNER rides
	// the agent seat). Unattended=true is honest — a fleet job has no human to
	// answer a broker ask.
	built, berr := agent.Build(agent.BuildConfig{
		PlannerBase: p.cfg.Endpoint,
		Model:       seat,
		Timeout:     wall,
		MaxSteps:    contract.MaxSteps,
		ReadRoot:    contextDir,
		Offload:     NewRecordlessOffload(p.cfg, p.cfg.Model, wall),
		Unattended:  true,
	})
	if berr != nil {
		return deferWire(core.DeferClassInfrastructure, "building agent: "+berr.Error())
	}

	// Window budgeting parity with handleAgentRun: probe the SERVED window
	// (conservative fallback when unanswerable) and run the measured-ON ladder
	// rungs with the real-tokenizer seam (fail-open to the legacy estimate).
	probed, probeOK := agent.ProbeServedWindow(cctx, p.cfg.Endpoint, seat)
	effCtx, _ := agent.ResolveContextTokens(0, probed, probeOK)
	built.Loop.WithContextTokens(effCtx).WithSkeletonPrune(true).WithGCFCompact(true).
		WithTokenizer(tokclient.New(p.cfg.Endpoint, seat, 0))

	// Task profile, default "research" (§S2: a delegated contract is a
	// read-over-docs research shape unless it says otherwise). Profiles can
	// only NARROW the read-only tool set; an unknown name defers loudly with
	// the valid names rather than silently falling back to bare `general` —
	// the one configuration measured to fail on small planners.
	profileName := strings.TrimSpace(contract.Profile)
	if profileName == "" {
		profileName = "research"
	}
	prof, perr := agent.LookupProfile(profileName)
	if perr != nil {
		// A profile this build does not have can never run here, however healthy
		// the box is — the contract, not the model, is what needs fixing.
		return deferWire(core.DeferClassConfig, perr.Error())
	}
	built.Loop.WithProfile(prof)

	res, rerr := built.Loop.Run(cctx, contract.Goal)
	wire.Steps = res.Steps
	wire.StopReason = res.StopReason
	if rerr != nil {
		// Wall timeout is its own defer shape — the delegator sizes future
		// contracts off it, so it must be distinguishable from a planner error.
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return deferWire(core.DeferClassBudget, fmt.Sprintf("wall timeout after %ds", timeoutSec))
		}
		return deferWire(core.DeferClassInfrastructure, "agent loop: "+rerr.Error())
	}
	if res.StopReason == "budget" {
		// The loop burned MaxSteps without a final answer. Output is empty on
		// this path, so there is nothing to re-pack — defer, don't dress an
		// unfinished run as a result.
		return deferWire(core.DeferClassBudget, fmt.Sprintf("step budget exhausted (%d steps)", res.Steps))
	}
	wire.Output = res.Output

	structured, tokensOut, transport, serr := p.repackStructured(cctx, seat, contract.OutputSchema, res.Output)
	if serr != nil {
		// wire.Output stays populated on every branch below: the delegator's
		// text-verb acceptance checks can still read the loop's answer even when
		// the structured shape never arrived.
		switch {
		case errors.Is(cctx.Err(), context.DeadlineExceeded):
			// The wall expired DURING the re-pack. That is the timeout shape,
			// not a schema shape — reporting it as "output failed schema" sends
			// the operator to rewrite a schema that was never the problem.
			return deferWire(core.DeferClassBudget, fmt.Sprintf("wall timeout after %ds", timeoutSec))
		case transport:
			// The seat could not be REACHED (llama-swap down/500/dial refused).
			// Its own prefix and class: a transport failure filed under
			// "output failed schema" is what makes a dead endpoint read as a
			// model that cannot follow a schema.
			return deferWire(core.DeferClassInfrastructure, "structured re-pack unreachable: "+serr.Error())
		default:
			// Stable prefix (§S2 verbatim) so the delegator can key on it; the
			// detail after the colon is for the operator — "failed" without a
			// why is unactionable telemetry. The model answered and got the
			// shape wrong: an abstention.
			return deferWire(core.DeferClassAbstention, "output failed schema: "+serr.Error())
		}
	}
	wire.Structured = structured
	wire.TokensOut = tokensOut
	return finish(wire)
}

// RunAgentContract executes one delegation contract IN-PROCESS on this
// pipeline — the delegator-side LOCAL placement entry (Task 6; it satisfies
// delegate.LocalRunner). It mirrors fleetnode.buildAgentRun's materialization
// discipline — a job-scoped dir under BaseDir()/pipeline-jobs/ (so
// SweepOrphanedPipelineJobs reclaims a crash's leftovers), context docs under
// <dir>/context/, removed when the run ends — then goes through Pipeline.Run
// (NOT runAgentTask directly) so a local placement takes byte-for-byte the
// same route a fleet node's Runner.Run takes. Differences from the wire path,
// both deliberate: Depth stays caller-set (a delegator-side local run IS the
// origin; buildAgentRun derives ≥1 only for wire arrivals), and OutputSchema
// is NOT required (roast delta 3 gates REMOTE placement on it; a local run's
// text-verb acceptance can stand alone).
func (p *Pipeline) RunAgentContract(ctx context.Context, contract core.AgentContract) (core.AgentWireResult, error) {
	if err := contract.Validate(); err != nil {
		return core.AgentWireResult{}, err
	}
	jobsRoot := filepath.Join(p.cfg.BaseDir(), "pipeline-jobs")
	if err := os.MkdirAll(jobsRoot, 0o755); err != nil {
		return core.AgentWireResult{}, fmt.Errorf("agent contract: creating pipeline-jobs dir: %w", err)
	}
	// MkdirTemp is the exclusive create (buildAgentRun's rule): the id is
	// minted here, so uniqueness comes by construction, not caller discipline.
	jobDir, err := os.MkdirTemp(jobsRoot, "agent-local-*")
	if err != nil {
		return core.AgentWireResult{}, fmt.Errorf("agent contract: creating job dir: %w", err)
	}
	defer os.RemoveAll(jobDir) // docs live exactly as long as the run
	contextDir := filepath.Join(jobDir, "context")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return core.AgentWireResult{}, fmt.Errorf("agent contract: creating context dir: %w", err)
	}
	for _, d := range contract.Context {
		// Validate held every Name to flat-filename shape, so this Join
		// cannot escape contextDir (same invariant as buildAgentRun).
		if werr := os.WriteFile(filepath.Join(contextDir, d.Name), []byte(d.Text), 0o644); werr != nil {
			return core.AgentWireResult{}, fmt.Errorf("agent contract: writing context doc %q: %w", d.Name, werr)
		}
	}
	res := p.Run(ctx, core.Request{
		Task:  core.TaskAgentRun,
		Input: contract.Goal,
		Params: map[string]any{
			"contract":    contract,
			"context_dir": contextDir,
			"job_id":      filepath.Base(jobDir),
		},
	})
	if !res.OK {
		// OK:false is runAgentTask's internal-wiring-bug shape (a defer is a
		// SUCCESS with wire.Deferred) — surface it as a real error.
		return core.AgentWireResult{}, errors.New(res.Reason)
	}
	var wire core.AgentWireResult
	if err := json.Unmarshal(res.Data, &wire); err != nil {
		return core.AgentWireResult{}, fmt.Errorf("agent result decode: %w", err)
	}
	return wire, nil
}

// repackStructured runs the v1 structured-output mechanism (§S2): ONE
// grammar-constrained completion on the SAME seat, re-packing the loop's
// final text into the contract's OutputSchema, with ONE retry on any failure
// (transport, non-JSON, schema validation) before the caller defers. The
// grammar comes from the same schema→gbnf seam the extract task uses
// (gbnf.FromJSONSchema), and the schema itself re-checks the emitted JSON via
// the validator — the grammar constrains shape, the validator enforces the
// parts a grammar cannot (required fields, value constraints).
//
// The returned transport flag reports whether the LAST attempt died on the
// wire rather than on validation. Both used to merge into one lastErr under
// the caller's "output failed schema:" prefix, so a llama-swap 500 was
// reported as a model that cannot follow a schema — and the operator went to
// rewrite the schema instead of restarting the endpoint.
func (p *Pipeline) repackStructured(ctx context.Context, seat string, rawSchema json.RawMessage, output string) (structured json.RawMessage, tokensOut int, transport bool, err error) {
	var schema map[string]any
	if uerr := json.Unmarshal(rawSchema, &schema); uerr != nil {
		return nil, 0, false, fmt.Errorf("output_schema is not a JSON object: %w", uerr)
	}
	fields := gbnf.FromJSONSchema(schema)
	if len(fields) == 0 {
		// Unreachable off the wire (contract Validate gates on this), kept for
		// in-process callers: an empty grammar would constrain nothing.
		return nil, 0, false, errors.New("output_schema has no gbnf-compilable properties")
	}
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Name)
	}
	grammar := gbnf.Object(fields)
	// The re-pack IS an extract over the loop's final text — same system/user
	// shape as tasks.buildExtract, so the seat sees a prompt pattern it
	// already handles.
	system := "You extract structured data from text. Output ONLY a JSON object with exactly the requested fields. Use empty values when a field is absent."
	user := fmt.Sprintf("Extract these fields from the text: %s.\n\nTEXT:\n%s", strings.Join(names, ", "), output)

	var lastErr error
	lastWasTransport := false
	for attempt := 0; attempt < 2; attempt++ {
		gres, gerr := p.client.Generate(ctx, seat, system, user, grammar, agentRepackMaxTokens, p.cfg.Temperature, 0)
		if gerr != nil {
			lastErr, lastWasTransport = gerr, true
			continue
		}
		content := []byte(strings.TrimSpace(gres.Content))
		if verr := validator.Validate(content, schema); verr != nil {
			lastErr, lastWasTransport = verr, false
			continue
		}
		return json.RawMessage(content), gres.TokensOut, false, nil
	}
	return nil, 0, lastWasTransport, lastErr
}
