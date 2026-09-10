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
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatwait"
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
	// agentRepackMaxTokensCap bounds repackBudget: the loop's own final budget
	// is capped at 8,192 (agent/thinking.go finalBudgetCap), and a re-pack that
	// must hold MORE than the answer it re-packs is not an extraction.
	agentRepackMaxTokensCap = 8192
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
	var contention *seatwait.Budget // set once the wall exists; finish reads it
	var admitted time.Duration      // the admission pre-flight, if any; finish reports it
	var admitNote string            // why the pre-flight could not settle residency (probe error / budget)
	wire := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: nodeID, Seat: seat}

	// finish is the ONE exit for every terminal wire result (success or defer):
	// it stamps WallMs, records the ledger row, and wraps the marshaled result
	// as a job-level success — so no return path can forget the defer-is-done
	// contract or the telemetry.
	finish := func(w core.AgentWireResult) core.Result {
		w.WallMs = time.Since(start).Milliseconds()
		if contention != nil {
			w.ContentionWaitSec = contention.Spent().Seconds()
		}
		if admitted > 0 {
			w.AdmissionWaitSec = admitted.Seconds()
		}
		w.AdmissionNote = admitNote
		meta.LatencyMs = w.WallMs
		meta.TokensOut = w.TokensOut
		meta.SeatTokensIn = w.SeatTokensIn
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
	// Admission pre-flight (2026-09-02): the wall must not pay for ANOTHER
	// session's model swap. llama-swap queues a request silently while it
	// evicts and loads, so a contract that arrived mid-swap spent its whole
	// wall in that queue and reported "wall timeout" — the 600 s failures
	// several parallel sessions reported on 2026-09-01. Wait, bounded and
	// OUTSIDE the wall, until nothing on the endpoint is mid-swap; then start
	// the clock. Known bound: the drain phase before a swap shows nothing
	// non-ready, so a swap that begins a second later is still charged to the
	// wall — this removes the swap WINDOW from the wall, not the race.
	admitted, admitNote = awaitSeatAdmission(ctx, p.cfg.Endpoint, seat, admissionBudget(p.cfg.AgentAdmissionWaitSec))
	if admitNote != "" {
		log.Printf("agent task: seat admission (%s): %s", seat, admitNote)
	}
	// Cold-load warm-up (0.115.11, register D-64): the pre-flight above settles
	// ANOTHER model's swap, but a seat that is simply not loaded used to load on
	// the loop's first call — INSIDE the wall. A vLLM seat's cold load is
	// 125–250 s on the fleet (seven Lenovo cold starts in two hours on
	// 2026-09-10 under ttl 300), so a 300 s contract could spend most of its
	// wall before the first token. Warm it here, on the admission budget's
	// remainder, and start the clock when the seat reads ready.
	if warmed, warmNote := warmSeat(ctx, p.cfg.Endpoint, seat, admissionBudget(p.cfg.AgentAdmissionWaitSec)-admitted); warmed > 0 || warmNote != "" {
		admitted += warmed
		if warmNote != "" {
			log.Printf("agent task: seat warm-up (%s): %s", seat, warmNote)
			if admitNote == "" {
				admitNote = warmNote
			} else {
				admitNote += "; " + warmNote
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, wall)
	defer cancel()
	// One busy-seat budget for the WHOLE contract (seatwait): every chat step
	// and the re-pack draw on it, so a 10-step loop cannot spend ten budgets,
	// and the wait it consumed is reported on the wire (contention_wait_sec).
	contention = seatwait.NewBudget(p.cfg.SeatContentionWaitSec)
	cctx = seatwait.WithBudget(cctx, contention)

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
		MaxTokens:   p.cfg.AgentMaxTokens, // the executing node's budget (its seat's reasoning cost is its own fact)
		ReadRoot:    contextDir,
		Offload:     NewRecordlessOffload(p.cfg, p.cfg.Model, wall),
		NPU:         NewLoopNPU(p.cfg),
		Accel:       NewLoopAccel(p.cfg), // every lane the box lists (ADR 0037): a remote contract sees the tools a local run does
		Unattended:  true,
		EnvRules:    p.cfg.AgentEnvRules,
		Thinking:    thinkingFor(p.cfg, contract), // contract > this box's agent_thinking > auto
		// The contract's own replay list, behind this box's seeded context
		// reads when agent_seed_context_reads is on (core.SeedContextReads:
		// the node knows the doc file names it just wrote; the delegator
		// would be guessing). Validated at contract decode; the loop answers
		// "does this tool exist on this seat" as an observation.
		SetupActions: setupActionsFor(p.cfg, contract),
	})
	if berr != nil {
		if errors.Is(berr, core.ErrAgentEnvRules) {
			// The table is this box's config; nothing about the contract or the
			// seat can fix it — say so by class.
			return deferWire(core.DeferClassConfig, "building agent: "+berr.Error())
		}
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
	if groundedContract(contract) {
		// The document is attached, so the few-shot tool cycle teaches nothing the
		// run needs and, on small seats, its user turn competes with the goal (the
		// phantom off-document digests of 2026-08-31…09-03, both fleet nodes
		// quarantined). Profile tools and system prompt stay; only the exemplar
		// messages go.
		built.Loop.WithoutExemplars()
		meta.ExemplarsDropped = true
	}
	// Record the RESOLVED profile, not contract.Profile: an empty contract field defaults to
	// "research" just above, and logging the empty string would misattribute every defaulted
	// run. Set here, before Loop.Run, so the defer branches below carry it too -- a run that
	// times out is exactly when knowing its profile matters most.
	meta.AgentProfile = profileName

	res, rerr := built.Loop.Run(cctx, contract.Goal)
	wire.Steps = res.Steps
	wire.StopReason = res.StopReason
	// Seat usage — set HERE, before every defer branch, for the same reason as
	// the trace below: a budget- or timeout-ended run is exactly the one that
	// generated for minutes and used to be ledgered as 0 (0.115.5).
	wire.TokensOut = res.TokensOut
	wire.SeatTokensIn = res.TokensIn
	// Step trace + rule telemetry (ADR 0036) — set HERE, before the defer
	// branches, for the same reason as the prefill accounting below: the
	// budget/timeout runs are the ones the rigger most needs to see.
	wire.Trace = TraceFromEffects(res.Effects)
	wire.RulesFired = len(res.RuleHits)
	wire.SetupRan = agent.SetupRan(res.Effects)
	// Per-completion records, the starvation note and the truncation flag
	// (0.115.8) — same rule: set before every branch, so a deferred run's
	// arithmetic reaches the corpus.
	wire.Calls = CallsFromLoop(res.Calls)
	wire.StopNote = res.StopNote
	wire.OutputTruncated = res.OutputTruncated
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
			if contention.CausedTimeout(wall) {
				// The wall was eaten by WAITING on peers, not by the model's own
				// work: filing it as a budget defer would poison the delegator's
				// contract sizing (the council's finding, 2026-09-02). An early
				// 429 that resolved does not qualify — CausedTimeout needs a sleep
				// in flight at expiry or waits covering half the wall.
				r, _ := contendedReason(seat, contention)
				return deferWire(core.DeferClassInfrastructure, r+"; the wall expired during the wait")
			}
			return deferWire(core.DeferClassBudget, fmt.Sprintf("wall timeout after %ds", timeoutSec))
		}
		// A busy seat that outlived the contention budget is its OWN reason:
		// "seat contended:" is the ledger/audit grep key, and the operator's fix
		// is capacity (llama-swap concurrencyLimit / --parallel), not a box.
		if errors.Is(rerr, agent.ErrUnparsedToolCall) {
			// The seat, not the box or the contract: fix its --tool-call-parser
			// (and --reasoning-parser) to match the model's chat template.
			return deferWire(core.DeferClassConfig, "seat tool-call parser mismatch — "+rerr.Error())
		}
		var se *agent.StatusError
		if errors.As(rerr, &se) && seatwait.Retryable(se.Code, se.Body) {
			// The client's own loop already recorded this status on the budget
			// (NextFor on its last, refused attempt); nothing to add here.
			r, _ := contendedReason(seat, contention)
			return deferWire(core.DeferClassInfrastructure, r+" — "+rerr.Error())
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
	if strings.TrimSpace(res.Output) == "" {
		// An EMPTY final answer is never a result (0.115.8, register D-42).
		// Until now this fell through to the re-pack, which turned "" into a
		// schema-valid all-empty object that then failed acceptance on the
		// delegator, which then retried the contract on a seat with the wall's
		// leftovers — three expensive steps that dressed silence as an answer
		// (2026-09-10: every empty structure in the corpus took this path).
		// The loop names the shape: reasoning_starved is a budget ceiling (the
		// completion budget went to the think block twice, thinking on and
		// off), empty is an abstention (the seat had room and said nothing).
		// Neither re-packs; both carry the loop's arithmetic in stop_note.
		class := core.DeferClassAbstention
		if res.StopReason == agent.StopReasoningStarved {
			class = core.DeferClassBudget
		}
		note := res.StopNote
		if note == "" {
			note = "stop_reason " + res.StopReason
		}
		return deferWire(class, fmt.Sprintf("empty final answer after %d steps and %d completion tokens: %s", res.Steps, res.TokensOut, note))
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
			if contention.CausedTimeout(wall) {
				// Same stable prefix as the transport arm below (§S2 keys on it);
				// the contention detail rides after it.
				r, _ := contendedReason(seat, contention)
				return deferWire(core.DeferClassInfrastructure, "structured re-pack unreachable: "+r+"; the wall expired during the wait")
			}
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
			var lse *llamaclient.StatusError
			if errors.As(serr, &lse) && seatwait.Retryable(lse.StatusCode, lse.Body) {
				// THIS failure is a busy answer that outlived the budget — name
				// the contention. Any other transport error keeps its own text.
				r, _ := contendedReason(seat, contention)
				return deferWire(core.DeferClassInfrastructure, "structured re-pack unreachable: "+serr.Error()+" ("+r+")")
			}
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
	wire.TokensOut += tokensOut // the re-pack's own generation, on top of the loop's
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
	budget := repackBudget(output)
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
		gres, gerr := p.client.Generate(ctx, seat, system, user, grammar, budget, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
		if gerr != nil {
			lastErr = gerr
			if transportErr == nil && genErrIsTransport(gerr) {
				transportErr = gerr
			}
			continue
		}
		if gres.Truncated {
			// The grammar completion hit max_tokens: what came back is a JSON
			// prefix, and validating it would report "unexpected end of JSON
			// input" or a cut multibyte character — the operator then rewrites
			// a schema that was never the problem (2026-09-10: a 22,865-char
			// answer re-packed at 1,024 tokens on the 27B, an 8,380-char one
			// on the 4B, both filed as invalid JSON). Name the truncation, and
			// give the retry the cap.
			lastErr = fmt.Errorf("re-pack truncated at %d tokens (the answer is %d chars; the structured budget cannot hold it)", budget, len(output))
			budget = agentRepackMaxTokensCap
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

// repackBudget sizes the structured re-pack's completion budget from the text
// it re-packs (0.115.10). The re-pack is an EXTRACTION over the loop's final
// answer, so its output is bounded by that answer: about a token per three
// characters of source plus JSON overhead, never below the historical 1,024
// and never above agentRepackMaxTokensCap. A fixed 1,024 was right for a
// one-line answer and wrong the moment 0.115.8 let a thinking seat finish a
// long one: a 22,865-char, seven-array extraction came back as a JSON prefix
// and was filed as "invalid json" (2026-09-10, both fleet seats).
func repackBudget(output string) int {
	b := len(output)/3 + 512
	if b < agentRepackMaxTokens {
		return agentRepackMaxTokens
	}
	if b > agentRepackMaxTokensCap {
		return agentRepackMaxTokensCap
	}
	return b
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
	gres, gerr := cc.Generate(ctx, seat, system, user, "", repackBudget(output), p.cfg.Temperature, 0, llamaclient.WithoutThinking())
	if gerr != nil || gres.Truncated {
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

const admissionPoll = 3 * time.Second

func admissionBudget(sec int) time.Duration {
	switch {
	case sec < 0:
		return 0
	case sec == 0:
		// 300 s since 0.115.11 (was 120): the budget now also covers the seat's
		// own cold load (warmSeat), and a vLLM seat takes 125–250 s to load
		// plus a Triton JIT on its first completion.
		return 300 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// warmSeat loads an ABSENT seat outside the wall (D-64). One GET through
// llama-swap's per-model passthrough (`/upstream/<seat>/v1/models`, the same
// route ProbeServedWindow uses) makes llama-swap swap the seat in and answers
// only once its health check passes; the call is bounded by `budget`. Then
// /running is polled (two extra polls at most) until the seat reads ready.
// Returns the time spent and a note when residency could not be settled —
// a probe failure, a spent budget — so the wire says "the gate could not
// tell" rather than "nothing was loading". A seat that is already ready
// costs one /running probe and returns 0.
func warmSeat(ctx context.Context, endpoint, seat string, budget time.Duration) (time.Duration, string) {
	if budget < admissionPoll || strings.TrimSpace(endpoint) == "" {
		return 0, "" // nothing left of the admission budget (or the gate is off): the wall's business, as before
	}
	sc, err := swapclient.New(endpoint, admissionPoll)
	if err != nil {
		return 0, ""
	}
	ready := func() (bool, error) {
		rows, rerr := sc.Running(ctx)
		if rerr != nil {
			return false, rerr
		}
		for _, r := range rows {
			if strings.EqualFold(r.ID, seat) && r.State == "ready" {
				return true, nil
			}
		}
		return false, nil
	}
	if ok, rerr := ready(); rerr != nil || ok {
		return 0, "" // ready, or unreadable (awaitSeatAdmission already reported a failed probe)
	}
	start := time.Now()
	b := swapclient.BaseURL(endpoint)
	if b == "" {
		return 0, ""
	}
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	req, rerr := http.NewRequestWithContext(wctx, http.MethodGet, b+"/upstream/"+url.PathEscape(seat)+"/v1/models", nil)
	if rerr != nil {
		return 0, ""
	}
	resp, derr := warmClient.Do(req)
	if derr != nil {
		spent := time.Since(start)
		if wctx.Err() != nil && ctx.Err() == nil {
			return spent, fmt.Sprintf("cold load exceeded the admission budget after %.0fs (proceeding into the wall)", spent.Seconds())
		}
		return spent, "warm request failed (proceeding): " + derr.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	// The swap answered; confirm residency. Two polls one poll-interval apart,
	// not a wait: llama-swap proxies only after the health check passed, so a
	// seat that is still not listed is one llama-swap does not know under
	// this name — say so and go.
	for i := 0; i < 2; i++ {
		if ok, rerr := ready(); rerr == nil && ok {
			return time.Since(start), fmt.Sprintf("cold load %.0fs outside the wall", time.Since(start).Seconds())
		}
		// The second poll waits one interval, but only inside what is left of
		// the budget — the confirmation must not outspend the gate it serves.
		if i == 0 && time.Since(start)+admissionPoll <= budget {
			if serr := seatwait.Sleep(ctx, admissionPoll); serr != nil {
				return time.Since(start), serr.Error()
			}
		} else if i == 0 {
			break
		}
	}
	return time.Since(start), fmt.Sprintf("warm request answered HTTP %d after %.0fs but /running never listed %s ready (proceeding)", resp.StatusCode, time.Since(start).Seconds(), seat)
}

// warmClient carries no timeout of its own: warmSeat bounds the request by
// context, and a cold vLLM load is minutes, not the seconds a transport
// timeout is sized for.
var warmClient = &http.Client{}

// awaitSeatAdmission polls llama-swap's GET /running until `seat` is ready,
// or nothing on the endpoint is mid-swap (an absent seat then loads on
// demand — the normal cold path, charged to the wall as today), or the
// budget is spent. Any non-"ready" state counts as busy: llama-swap's
// vocabulary is stopped|starting|ready|stopping|shutdown and a row in any of
// the transitional ones means the GPU is being re-arranged. A probe failure
// proceeds (fail-open, logged): the loop's first chat call surfaces the real
// transport error with far more detail.
func awaitSeatAdmission(ctx context.Context, endpoint, seat string, budget time.Duration) (time.Duration, string) {
	if budget <= 0 || strings.TrimSpace(endpoint) == "" {
		return 0, ""
	}
	sc, err := swapclient.New(endpoint, admissionPoll)
	if err != nil {
		return 0, "no swap client (proceeding): " + err.Error()
	}
	// waited counts only the SLEEPS, never the probe round-trips: an
	// immediate admission reports zero, which is what "nothing was swapping"
	// must read as on the wire.
	var waited time.Duration
	for {
		rows, rerr := sc.Running(ctx)
		if rerr != nil {
			return waited, "running probe failed (proceeding): " + rerr.Error()
		}
		busy := ""
		for _, r := range rows {
			if strings.EqualFold(r.ID, seat) && r.State == "ready" {
				return waited, ""
			}
			if r.State != "ready" {
				busy = r.ID + ":" + r.State
			}
		}
		if busy == "" {
			return waited, ""
		}
		if waited+admissionPoll > budget {
			return waited, "budget spent while " + busy + " (proceeding into the wall)"
		}
		if serr := seatwait.Sleep(ctx, admissionPoll); serr != nil {
			return waited, serr.Error()
		}
		waited += admissionPoll
	}
}

// contendedReason renders the "seat contended:" defer reason when the
// contract's seatwait budget saw at least one busy answer. The prefix is the
// ledger/audit grep key; the fix it points at is CAPACITY (llama-swap
// concurrencyLimit / --parallel), never a box.
func contendedReason(seat string, b *seatwait.Budget) (string, bool) {
	if b == nil || b.LastStatus() == 0 {
		return "", false
	}
	return fmt.Sprintf("seat contended: %s answered HTTP %d on %d attempt(s), waited %.0fs in total (peers hold its slots — raise concurrencyLimit or retry)",
		seat, b.LastStatus(), b.Attempts(), b.Spent().Seconds()), true
}

// genErrIsTransport reports whether a Generate error means the SEAT COULD NOT
// BE REACHED (or was never what answered), as opposed to reached-and-unusable.
// What qualifies:
//
//   - the transport's own *url.Error / net.Error — dial refused, TLS, a reset
//     before the response headers;
//   - a 5xx — a server saying it is in trouble;
//   - a 429 / 503 "process is not ready" / 500 src=llama-swap — llama-swap
//     saying PEERS HOLD THE SEAT (verified against its scheduler source
//     2026-09-02: 429 = reserved requests ≥ concurrencyLimit; swaps queue
//     silently; a health-check timeout is a 500 from llama-swap itself). By
//     the time one reaches here, seatwait has already waited its budget on the
//     same seat, so the honest reading is "not reachable IN TIME" — transport;
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

// groundedContract reports whether the contract carries context documents — the
// shape whose answer lives in the attached file rather than in tool calls.
func groundedContract(c core.AgentContract) bool {
	for _, d := range c.Context {
		if len(d.Text) > 0 {
			return true
		}
	}
	return false
}

// setupActionsFor is the replay list a run on THIS box gets: the node's seeded
// context reads first (agent_seed_context_reads — one read_file per context
// doc, the names the node itself materializes), then the contract's own
// setup_actions, the whole list clamped to core.AgentSetupActionsMax — the
// "up to eight" every door documents is a property of the RUN, not of each
// source (review finding 2026-09-07: seed 8 + contract 8 was a silent 16).
// Seeded reads come first so a contract's own actions see the documents
// already in the transcript; past the cap the contract's tail is dropped (the
// trace shows exactly what replayed). nil when neither applies, which the loop treats as
// "no replay" — byte-identical to the pre-key loop.
func setupActionsFor(cfg config.Config, contract core.AgentContract) []core.AgentSetupAction {
	var out []core.AgentSetupAction
	if cfg.AgentSeedContextReads {
		out = append(out, core.SeedContextReads(contract.Context)...)
	}
	out = append(out, contract.SetupActions...)
	if len(out) > core.AgentSetupActionsMax {
		out = out[:core.AgentSetupActionsMax]
	}
	return out
}

// thinkingFor resolves the planner think-block policy for one run: the
// contract's own `thinking` wins, then this box's `agent_thinking`, then auto.
// Both values were validated at their doors (contract decode, config load),
// so the string is passed through; agent.Build re-parses and refuses by name.
func thinkingFor(cfg config.Config, contract core.AgentContract) string {
	if t := strings.TrimSpace(contract.Thinking); t != "" {
		return t
	}
	return strings.TrimSpace(cfg.AgentThinking)
}

// CallsFromLoop projects the loop's per-completion records onto the wire
// (core.AgentCallRecord). nil in, nil out.
func CallsFromLoop(calls []agent.CallRecord) []core.AgentCallRecord {
	if len(calls) == 0 {
		return nil
	}
	out := make([]core.AgentCallRecord, 0, len(calls))
	for _, c := range calls {
		out = append(out, core.AgentCallRecord{
			Step: c.Step, MaxTokens: c.MaxTokens, FinishReason: c.FinishReason,
			CompletionTokens: c.CompletionTokens, ReasoningTokens: c.ReasoningTokens,
			ContentChars: c.ContentChars, ReasoningChars: c.ReasoningChars,
			ToolCalls: c.ToolCalls, ThinkingOff: c.ThinkingOff, ReasoningKey: c.ReasoningKey,
		})
	}
	return out
}

// TraceFromEffects projects the loop's effect ledger onto the wire trace
// (core.AgentTraceStep): the per-call facts the corpus keeps, without the
// result bytes. nil in, nil out — a run with no tool calls carries no trace.
func TraceFromEffects(effects []agent.EffectRecord) []core.AgentTraceStep {
	if len(effects) == 0 {
		return nil
	}
	out := make([]core.AgentTraceStep, 0, len(effects))
	for _, e := range effects {
		s := core.AgentTraceStep{Step: e.Step, Tool: e.Tool, Status: string(e.Status), ObsChars: e.ObsChars, Rule: e.Rule, Setup: e.Setup}
		if e.Status != agent.EffectCommitted && e.Note != "" {
			// the loop's Note is the (bounded) text the model saw for a
			// non-committed call — clipped again here so the corpus row
			// carries a line, never a page
			n := strings.TrimSpace(strings.ReplaceAll(e.Note, "\n", " "))
			if len(n) > core.AgentTraceNoteMax {
				n = n[:core.AgentTraceNoteMax]
			}
			s.Note = n
		}
		out = append(out, s)
	}
	return out
}
