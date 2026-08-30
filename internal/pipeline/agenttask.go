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
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/swapclient"
	"github.com/dmmdea/offload-harness/internal/tokclient"
	"github.com/dmmdea/offload-harness/internal/validator"
)

const (
	// agentRepackMaxTokens bounds the structured re-pack completion — the same
	// budget buildExtract gives the extract task's grammar output (the re-pack
	// IS an extract over the loop's final text).
	// Raised 512 → 1024 on 2026-08-30: a four-field digest schema (three short
	// lists + a verdict) overflowed 512 on BOTH the 27B and the 4B seats —
	// "invalid json: unexpected end of JSON input", surfaced as an abstention
	// the caller could not tell from a real one. The 2026-08-28 long-extraction
	// abstentions on the 9B/4B were the same class. 1024 fits a bounded digest
	// with headroom and stays well inside every seat's window.
	agentRepackMaxTokens = 1024
	// agentRepackChatTimeout bounds the grammar-free chat fallback: one
	// completion over already-finished text, generously padded for a cold seat.
	agentRepackChatTimeout = 120 * time.Second
	// agentRosterProbeTimeout bounds the seat-residency roster fetch — the same
	// 10s mcpserver's plannerUnserved uses.
	agentRosterProbeTimeout = 10 * time.Second
)

// runAgentTask executes one delegation contract. req.Params carries the
// DECODED contract + the materialized context dir (fleetnode.buildAgentRun
// owns decode/validation/materialization; nothing is re-validated here).
// warnSeatPin gates the seat-pin-probe failure warning to once per process —
// the same loud-once posture as the delegate corpus-loss warning: the failure
// matters to an operator, repeating it per run is noise.
var warnSeatPin sync.Once

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
		NPU:         NewLoopNPU(p.cfg),
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

	// Task profile: the caller's explicit choice > this box's configured
	// agent_profile > "general" — the same resolver handleAgentRun uses, so the
	// two agent doors can no longer disagree about the same seat.
	//
	// This used to hard-default to "research" (§S2: a delegated contract is a
	// read-over-docs research shape unless it says otherwise), which bypassed
	// the per-tier key that exists precisely to decide this. AgentProfile is
	// documented as "a property of the SEAT's capability, not of the task: a 27B
	// planner handles the full tool set, a 4B does not" — and a spread's subtask
	// 0 ALWAYS lands on the local seat, so on a big-planner box every fan-out
	// was silently forced into the narrowed shape. Measured on the 27B seat
	// (2026-08-19 D3 bake): general 100%, research 94% and 5x slower on one
	// shape. Meanwhile the small tiers (ampere-6/8, blackwell-8) seed
	// "research" in config_seed and keep it, which is where that decision
	// belongs — in per-tier data, not in a constant here.
	//
	// Profiles can only NARROW the read-only tool set; an unknown name defers
	// loudly with the valid names rather than silently falling back to bare
	// `general` — the one configuration measured to fail on small planners.
	profileName := p.cfg.AgentTaskProfile(strings.TrimSpace(contract.Profile))
	prof, perr := agent.LookupProfile(profileName)
	if perr != nil {
		// A profile this build does not have can never run here, however healthy
		// the box is — the contract, not the model, is what needs fixing.
		return deferWire(core.DeferClassConfig, perr.Error())
	}
	built.Loop.WithProfile(prof)
	// Record the RESOLVED profile, not contract.Profile: an empty contract field defaults to
	// "research" just above, and logging the empty string would misattribute every defaulted
	// run. Set here, before Loop.Run, so the defer branches below carry it too -- a run that
	// times out is exactly when knowing its profile matters most.
	meta.AgentProfile = profileName

	res, rerr := built.Loop.Run(cctx, contract.Goal)
	wire.Steps = res.Steps
	wire.StopReason = res.StopReason
	// T2-B: capture the run's prefill accounting HERE, immediately after the loop and
	// BEFORE the defer branches below. Every one of those branches still records a
	// ledger row via finish()/deferWire(), and a budget-exhausted or timed-out run is
	// precisely where prefill is most interesting -- it burned the most steps. Reading
	// it only on the success path would systematically exclude the expensive runs from
	// the measurement, biasing the very number this exists to produce.
	//
	// meta is captured by reference by the finish/deferWire closures defined above, so
	// mutating it now reaches every recording path.
	if pf := res.Prefill; pf.ObservedSteps > 0 {
		meta.PrefillSteps = pf.ObservedSteps
		meta.PrefillTokens = pf.PrefillTokens
		meta.CacheTokens = pf.CacheTokens
		meta.PrefillMS = pf.PrefillMS
		// Mirror onto the WIRE result too: the node's ledger row already had
		// these, the delegator's corpus did not — a remote run's prefill
		// economics were computed then discarded from the delegation log's
		// point of view. `wire` is copied by finish/deferWire at call time, so
		// every recording path below (budget defers included — the expensive
		// runs) carries them.
		wire.PrefillSteps = pf.ObservedSteps
		wire.PrefillTokens = pf.PrefillTokens
		wire.CacheTokens = pf.CacheTokens
		wire.PrefillMS = pf.PrefillMS
	}
	if rerr != nil {
		// Wall timeout is its own defer shape — the delegator sizes future
		// contracts off it, so it must be distinguishable from a planner error.
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return deferWire(core.DeferClassBudget, fmt.Sprintf("wall timeout after %ds", timeoutSec))
		}
		return deferWire(core.DeferClassInfrastructure, "agent loop: "+rerr.Error())
	}

	// A1 config pinning — stamped HERE, after the loop completed its chat
	// traffic and before any terminal branch, so success, budget-stop and
	// every re-pack abstention below all carry the pins (deferWire copies
	// `wire` at call time). Pre-loop defers (roster, profile, build) stay
	// unpinned on purpose: their seat never served, and probing /props on a
	// non-resident seat would COLD-START a model as a telemetry side effect —
	// which is why the probe also runs on its own short context rather than
	// cctx (a wall that expired mid-run must not also cost the pin, but the
	// probe must never wait out a cold load either; ProbeSeatPin's client
	// gives up in 3s and the pin honestly stays absent).
	wire.HarnessVersion = buildinfo.Version
	wire.HarnessBuildSHA256 = buildinfo.BuildSHA256()
	if pin, ok := agent.ProbeSeatPin(context.Background(), p.cfg.Endpoint, seat); ok {
		wire.SeatConfigSHA256 = pin.SHA256
		wire.SeatConfigBasis = pin.Basis
	} else {
		// LOUD-once (the delegate corpus-loss posture): this run SERVED — real
		// tokens were spent and its row is exactly what a paired experiment
		// scores — so a silent pin failure would only surface at analysis
		// time, as a pile of unpinned served rows, when the operator can no
		// longer restart the run or fix the endpoint. Results unaffected.
		warnSeatPin.Do(func() {
			log.Printf("agent task: seat-pin probe of %s/%s failed after a served run; seat_config_* will be ABSENT on such rows until it recovers (results unaffected, but the rows cannot enter a paired experiment)", p.cfg.Endpoint, seat)
		})
	}

	if res.StopReason == "budget" {
		// The loop burned MaxSteps without a final answer. Output is empty on
		// this path, so there is nothing to re-pack — defer, don't dress an
		// unfinished run as a result.
		return deferWire(core.DeferClassBudget, fmt.Sprintf("step budget exhausted (%d steps)", res.Steps))
	}
	wire.Output = res.Output

	// No schema, no re-pack. A schemaless contract is legal on the LOCAL
	// placement path (RunAgentContract's doc; delegate/gate.go makes the schema
	// a REMOTE-eligibility condition only; the MCP tool requires only `goal`) —
	// and that is the default idle-local, quality-first path. Calling
	// repackStructured anyway handed json.Unmarshal a nil schema, which errors,
	// which deferred the run with "output failed schema: ... unexpected end of
	// JSON input" and THREW AWAY a finished answer. Skipping leaves
	// wire.Structured empty and wire.Deferred false, which is the honest shape:
	// nothing structured was asked for, so nothing structured is missing.
	if len(contract.OutputSchema) == 0 {
		return finish(wire)
	}

	structured, tokensOut, transport, serr := p.repackStructured(cctx, seat, contract.OutputSchema, res.Output)
	if serr != nil {
		// wire.Output stays populated on every branch below so the CALLER still
		// receives the loop's answer even when the structured shape never
		// arrived. It is preserved for the caller, NOT for delegator-side
		// acceptance: every branch below returns deferWire, which sets
		// Deferred, and delegate.runLocal/runRemote both run acceptance only
		// when !wire.Deferred — so no check can ever read it on this path.
		switch {
		case errors.Is(cctx.Err(), context.DeadlineExceeded):
			// The wall expired DURING the re-pack. That is the timeout shape,
			// not a schema shape — reporting it as "output failed schema" sends
			// the operator to rewrite a schema that was never the problem.
			return deferWire(core.DeferClassBudget, fmt.Sprintf("wall timeout after %ds", timeoutSec))
		case errors.Is(cctx.Err(), context.Canceled):
			// The PARENT went away mid-re-pack (the delegator abandoned the
			// poll, the node is shutting down). The failed request looks exactly
			// like a dial refusal — a *url.Error — so it used to be reported as
			// infrastructure: an operator told to fix a box that never
			// misbehaved. Budget is the honest class: a ceiling outside the
			// model's control stopped the run.
			return deferWire(core.DeferClassBudget, "canceled during the structured re-pack (the caller's context ended)")
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
// The returned transport flag reports whether the seat could not be REACHED.
// Everything used to merge into one lastErr under the caller's "output failed
// schema:" prefix, so a llama-swap 500 read as a model that cannot follow a
// schema and the operator rewrote a schema that was never the problem. Two
// later corrections, both from the same root question — "is this the BOX or the
// REQUEST?":
//
//   - The flag is set by genErrIsTransport, not by "the call returned an
//     error". decodeGenResult errors on every non-200 AND on a 200 with zero
//     choices, so a 400 "context length exceeded" and an empty completion were
//     both filed as a dead endpoint: defer_class infrastructure, a non-zero
//     exit, and an operator sent to check a box that was answering fine when
//     the real fix was a smaller context or a flatter schema.
//   - The flag is STICKY across the retry, not last-wins. A 500 followed by a
//     wrong-shape retry used to end as an abstention ("the model got the shape
//     wrong") even though the seat had just failed a request; when a transport
//     failure happened AT ALL, that is the operator's signal, and the returned
//     error is the transport one so the message names it.
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

	var lastErr, transportErr error
	for attempt := 0; attempt < 2; attempt++ {
		// WithoutThinking: the re-pack is a mechanical shape transformation over
		// text the loop has ALREADY finished reasoning about, so it should never
		// think — and on a thinking seat, thinking is not merely wasted, it
		// destroys the answer. A thinking chat template emits the
		// grammar-constrained output into `reasoning_content` and leaves
		// `content` EMPTY, so both attempts fail and a finished, correct run
		// defers as an abstention. Measured live, same request, temp 0,
		// max_tokens 512:
		//
		//	seat          grammar  len(content)  len(reasoning)
		//	qwen3.8-27b   none      73 (valid)    326
		//	qwen3.8-27b   GBNF       0            67     <- the defect
		//	gemma-4-e4b   none      85             0
		//	gemma-4-e4b   GBNF      67 (valid)     0
		//
		// It is NOT a token budget: 512/1024/2048/4096 all return the identical
		// completion (140 tokens, finish "stop"). The grammar is the trigger.
		// The flag is harmless on a non-thinking template — gemma-4-e4b's output
		// with it is identical to its output without it — so it rides every
		// re-pack rather than being gated on a seat guess we cannot make.
		gres, gerr := p.client.Generate(ctx, seat, system, user, grammar, agentRepackMaxTokens, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
		if gerr != nil {
			lastErr = gerr
			if transportErr == nil && genErrIsTransport(gerr) {
				transportErr = gerr
			}
			continue
		}
		content := []byte(strings.TrimSpace(gres.Content))
		if verr := validator.Validate(content, schema); verr != nil {
			if fixed, ok := coerceToSchema(content, schema); ok {
				return json.RawMessage(fixed), gres.TokensOut, false, nil
			}
			lastErr = verr
			continue
		}
		return json.RawMessage(content), gres.TokensOut, false, nil
	}
	// FINAL fallback: one grammar-FREE attempt over /v1/chat/completions.
	// Found live wiring the Lenovo FreeToken agent seat (2026-08-27): the two
	// attempts above ride cfg.CompletionPath — llama-server's NATIVE completion
	// route, which an OpenAI-only engine does not serve. llama-swap proxies the
	// call anyway, the engine 404s with an HTML body, and the re-pack read
	// "invalid json: invalid character '<'" — so a seat that had just produced
	// a CORRECT final answer abstained on every schema'd contract, making the
	// seat unusable for remote placement (which requires a schema). The chat
	// route is the one surface every seat serves; the schema validator still
	// gates the result, so this trades the grammar's shape-constraint for
	// reach while conceding nothing on correctness.
	if structured, tokensOut, ok := p.repackViaChat(ctx, seat, schema, output); ok {
		return structured, tokensOut, false, nil
	}
	if transportErr != nil {
		// Report the TRANSPORT failure itself, not whatever the other attempt
		// produced: the caller prefixes this with "structured re-pack
		// unreachable", and a message pairing that prefix with a schema
		// validation error would be an unreadable diagnosis.
		return nil, 0, true, transportErr
	}
	return nil, 0, false, lastErr
}

// repackViaChat is the grammar-free re-pack lane: a plain chat completion on
// the seat asking for ONLY a JSON object, field types spelled out in the
// prompt (a grammar would have enforced them; without one, "7" vs 7 is exactly
// the miss a type-annotated prompt prevents — measured on gpt-oss-20b). The
// answer is trimmed to its outermost {...} span before validation, because
// chat-route answers legitimately arrive fenced or prefixed where the native
// grammar route could not.
func (p *Pipeline) repackViaChat(ctx context.Context, seat string, schema map[string]any, output string) (json.RawMessage, int, bool) {
	names := make([]string, 0, 8)
	if props, ok := schema["properties"].(map[string]any); ok {
		for name, raw := range props {
			typ := "string"
			if m, ok := raw.(map[string]any); ok {
				if t, ok := m["type"].(string); ok {
					typ = t
				}
			}
			names = append(names, fmt.Sprintf("%q (%s)", name, typ))
		}
	}
	sort.Strings(names)
	system := "You extract structured data from text. Output ONLY a JSON object — no prose, no code fences. Respect the field types exactly: numbers unquoted, strings quoted."
	user := fmt.Sprintf("Extract these fields from the text as a JSON object: %s.\n\nTEXT:\n%s", strings.Join(names, ", "), output)
	// A dedicated chat-path client, seat-endpoint routing mirrored from the
	// main client's construction (recordless.go): the pipeline's own client is
	// pinned to the native completion path and cannot make this call.
	cc := llamaclient.New(p.cfg.Endpoint, "/v1/chat/completions", p.cfg.Model, agentRepackChatTimeout).
		WithSeatEndpoints(p.cfg.SeatEndpoints)
	gres, gerr := cc.Generate(ctx, seat, system, user, "", agentRepackMaxTokens, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
	if gerr != nil {
		return nil, 0, false
	}
	content := strings.TrimSpace(gres.Content)
	if i, j := strings.Index(content, "{"), strings.LastIndex(content, "}"); i >= 0 && j > i {
		content = content[i : j+1]
	}
	if verr := validator.Validate([]byte(content), schema); verr != nil {
		fixed, ok := coerceToSchema([]byte(content), schema)
		if !ok {
			return nil, 0, false
		}
		return json.RawMessage(fixed), gres.TokensOut, true
	}
	return json.RawMessage(content), gres.TokensOut, true
}

// coerceToSchema repairs the ONE failure shape a grammar would have prevented
// and no prompt reliably does: scalar TYPE mismatches from a grammar-less
// seat — "7" where the schema wants a number, "true" where it wants a bool.
// Found live on the FreeToken gpt-oss seat (2026-08-27/28): the model answers
// correctly and quotes every scalar, so each typed contract abstained after a
// CORRECT answer. Coercion is deterministic and lossless — a string is
// converted only when the schema demands the type AND the value parses as it —
// and the result must re-validate in full before it counts. Anything beyond
// scalar re-typing (missing fields, wrong structure) still fails honestly.
func coerceToSchema(content []byte, schema map[string]any) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(content, &obj) != nil {
		return nil, false
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, false
	}
	changed := false
	for name, raw := range props {
		spec, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		want, _ := spec["type"].(string)
		got, present := obj[name]
		str, isStr := got.(string)
		if !present || !isStr {
			continue
		}
		switch want {
		case "number", "integer":
			var n json.Number
			if err := json.Unmarshal([]byte(strings.TrimSpace(str)), &n); err == nil {
				if f, ferr := n.Float64(); ferr == nil {
					obj[name] = f
					changed = true
				}
			}
		case "boolean":
			switch strings.ToLower(strings.TrimSpace(str)) {
			case "true":
				obj[name] = true
				changed = true
			case "false":
				obj[name] = false
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	fixed, merr := json.Marshal(obj)
	if merr != nil {
		return nil, false
	}
	if validator.Validate(fixed, schema) != nil {
		return nil, false
	}
	return fixed, true
}

// genErrIsTransport reports whether a Generate error means the SEAT COULD NOT
// BE REACHED (or was never what answered), as opposed to reached-and-unusable.
// What qualifies:
//
//   - the transport's own *url.Error / net.Error — dial refused, TLS, a reset
//     before the response headers;
//   - a 5xx — a server saying it is in trouble;
//   - a 429 — llama-server does not rate-limit, so a 429 means something in
//     FRONT of the seat answered instead of it;
//   - a *llamaclient.BodyError — a 200 whose body could not be read or parsed.
//     A non-JSON body from something claiming to be llama-server means SOMETHING
//     ELSE ANSWERED (a proxy, a captive portal), and a body that stops mid-read
//     is a connection that died AFTER Do() succeeded, so no *url.Error and no
//     net.Error is in the chain to catch it. Both were previously filed as "an
//     unparseable body is a protocol mismatch" and came back as abstentions at
//     exit 0 — the fleet-side silent failure this classifier exists to prevent.
//
// Everything else is deliberately NOT transport, because the SEAT answered: a
// non-429 4xx is it refusing THIS request (context length exceeded, an
// uncompilable grammar — fix the contract, not the box), and a 200 with zero
// choices is a model that produced nothing.
//
// A CANCELLATION is not transport either. It arrives wrapped in a *url.Error
// like any dial failure, but nothing on this box failed — the caller went away
// — and the caller (runAgentTask) has its own arm for it.
func genErrIsTransport(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var se *llamaclient.StatusError
	if errors.As(err, &se) {
		return se.StatusCode >= 500 || se.StatusCode == http.StatusTooManyRequests
	}
	var be *llamaclient.BodyError
	if errors.As(err, &be) {
		return true
	}
	// Defense for any path that surfaces a decode/read failure unwrapped: the
	// two shapes a broken body produces, recognized on their own.
	var syn *json.SyntaxError
	if errors.As(err, &syn) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return true
	}
	var nerr net.Error
	return errors.As(err, &nerr)
}
