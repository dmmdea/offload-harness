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
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatrate"
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
	// is capped at 8,192 (seatrate.FinalBudgetCap), and a re-pack that
	// must hold MORE than the answer it re-packs is not an extraction.
	agentRepackMaxTokensCap = 8192
	// agentRepackChatTimeout bounds the grammar-free chat fallback: one
	// completion over already-finished text, generously padded for a cold seat.
	agentRepackChatTimeout = 120 * time.Second
	// agentRepackAttemptFloor (0.115.23, register D-91) is the least wall a
	// re-pack attempt is started with: a grammar or chat re-pack of a long
	// answer is a full re-generation (a 12 KB answer ≈ 4,500 tokens ≈ 190 s on
	// the 4B), and an attempt that cannot finish only converts a finished
	// loop into "wall timeout after Ns".
	agentRepackAttemptFloor = 45 * time.Second
	// agentRosterProbeTimeout bounds the seat-residency roster fetch — the same
	// 10s mcpserver's plannerUnserved uses.
	agentRosterProbeTimeout = 10 * time.Second
	// repackMaxAttempts is the most seat completions repackStructured ever
	// spends: two grammar attempts (the truncation retry included) plus the
	// one grammar-free chat fallback. Named so repackAttemptDeadline (register
	// D-108, W-19) can divide what is left of the wall by what is still owed a
	// turn, instead of a single attempt assuming it is the only one left.
	repackMaxAttempts = 3
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

	// Seat resolution (ADR 0039): the planner default, overridden by a seat a
	// delegator already decided (Params "seat"/"placed", from RunAgentContract's
	// options — the decision was made once, over the same table, and is
	// published verbatim), else — on a composite box — by the layer a dispatched
	// contract names, re-decided HERE with this box's own live readers so the
	// display-card guards are evaluated where the card is (council R5/R6). A
	// plain box has one implicit layer: contract.layer changes nothing and no
	// placed block is published (the byte-identical constraint).
	seat := p.cfg.AgentPlannerModel("")
	var placedPtr *core.Placed
	if pl, _ := req.Params["placed"].(*core.Placed); pl != nil {
		placedPtr = pl
		if pl.Seat != "" {
			seat = pl.Seat
		}
	}
	if override, _ := req.Params["seat"].(string); override != "" {
		seat = override // an explicit seat wins over the block's
	}
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
	var coherenceNote string        // what the post-warm coherence probe found, when it ran (D-118)
	wire := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: nodeID, Seat: seat, Placed: placedPtr}

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
		w.CoherenceNote = coherenceNote
		if w.SeatTokS > 0 {
			meta.TokPerSec = w.SeatTokS // the ledger's tok_per_s column, empty on agent rows until 0.115.21
			if meta.TokPerSec == 0 {
				meta.TokPerSec = w.ObservedTokS // 0.131.1: the observed rate when no calibrated sample exists
			}
		}
		meta.LatencyMs = w.WallMs
		meta.TokensOut = w.TokensOut
		meta.SeatTokensIn = w.SeatTokensIn
		// The job behind the row (D-101): what the wire already knows, so the
		// ledger row can say which job, how many steps and why it stopped.
		meta.Steps = w.Steps
		meta.StopReason = w.StopReason
		meta.RepackMs = w.RepackMs
		meta.RepackAttempts = w.RepackAttempts
		if jid, _ := req.Params["job_id"].(string); jid != "" {
			meta.JobID = jid
		}
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

	if contract.Layer != "" && p.cfg.Composite() && placedPtr == nil {
		dec := placement.DecideOnLayer(
			placement.RequestForContract(contract, placement.EstimateTokens(contract), p.cfg.AgentMaxTokens),
			p.cfg.Layers, contract.Layer, p.live())
		// The decision is published whether it admitted or refused: a guard
		// defer must name the guard (branchable), not just a sentence — on
		// the wire AND on the ledger row. finish reads meta, so the row's
		// layer/seat are stamped here, before the defer return, or the row
		// would carry layer "" and the planner seat for a refusal that named
		// the triple layer's seat (council R8's `layer` column would count
		// zero guard refusals on that layer).
		placedPtr = &dec.Placed
		wire.Placed = placedPtr
		meta.Placed = placedPtr
		if dec.Defer {
			if dec.Placed.Seat != "" {
				// The seat the guard refused is the seat this defer is about;
				// the planner default never saw the contract.
				seat = dec.Placed.Seat
				wire.Seat = seat
			}
			meta.Model = seat
			return deferWire(dec.DeferClass, dec.Reason)
		}
		if dec.Seat != "" {
			seat = dec.Seat
			wire.Seat = seat
		}
	}
	meta.Model = seat
	meta.Placed = placedPtr

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
	// Auto wall (register D-03, 0.126.0): a contract whose caller named no
	// timeout_sec carries the wire default and the timeout_auto marker, and
	// THIS node — the one that knows its seat's measured rate — sizes the wall
	// from the same estimate it publishes as wall_estimate_sec, clamped to the
	// wire bounds, and reports what it ran under as wall_sec. Stamped exactly
	// once, here: the loop, the estimate and the final-budget fit below all
	// read the stamped value. A seat with no rate yet runs the default, as
	// before, and the log says so. Both doors (fleet node and local delegate)
	// run this function, so neither can drift from the other.
	rates := p.seatRates()
	if contract.TimeoutAuto {
		auto, note := AutoWallFor(p.cfg, contract, seat, rates.Get(seat))
		if auto > 0 {
			timeoutSec = auto
			wire.WallSec = auto
		}
		log.Printf("agent task: %s", note)
		contract.TimeoutAuto, contract.TimeoutSec = false, timeoutSec
	}
	wall := time.Duration(timeoutSec) * time.Second
	// Register the run (0.117.0, register D-93) BEFORE admission, so a drain or
	// a status reader sees it while the seat is still loading for it, and hold
	// at the cordon: a draining or exclusive text hold, or a media lease,
	// admits no NEW run. Running work completes; new work waits its admission
	// budget here — never inside the wall — and defers with the holder named.
	// Before this, the warm-up below loaded the seat straight past the fence
	// (10:20:03 on 2026-09-14, onto cards a render held).
	act := gpuactivity.Start(p.cfg.GPULockPath, p.cfg.StateDir, gpuactivity.Run{Seat: seat, Kind: "contract", Origin: nodeID, Goal: contract.Goal, MaxSteps: contract.MaxSteps, Phase: gpuactivity.PhaseAdmission})
	defer act.End()
	// ONE admission budget for the cordon, the pre-flight and the warm-up: the
	// three share a deadline, and the time spent at the cordon is reported as
	// admission time (reviewer finding, 0.117.0: each had its own full window).
	admissionEnd := time.Now().Add(admissionBudget(p.cfg.AgentAdmissionWaitSec))
	// THE FENCE CHECK (register S-26), BEFORE the cordon below. Under a lease
	// this process does not hold and that refuses new runs — an exclusive text
	// hold, a draining cordon, a media render — the cordon cannot succeed:
	// nothing this run can do inside its own budget releases another process's
	// lease. It polled that file for the WHOLE admission budget anyway (47 rows
	// in three days, 300 s each, 3.92 h) and then deferred `capacity` — which is
	// precisely the class the delegator re-places on another node. The verdict
	// was on disk before the first poll, so say it now and let the re-placement
	// happen five minutes earlier.
	//
	// ForeignFence, not Fenced, and only a FENCING hold: `gpu reserve --drain
	// --unload-seat -- <session>` runs its own work under GPU_LEASE_EPOCH on the
	// cards it cleared, and a plain non-fencing text reservation still WAITS at
	// the cordon (ADR 0032, "a peer-held seat is waited for"). This changes only
	// the hold whose answer cannot change inside the wait.
	//
	// The lease read is the CORDON's own armed directory (config.Load arms it),
	// not an independent resolution from this config: a pre-check that predicts
	// what AwaitRunSlot will do has to read what AwaitRunSlot reads, or the two
	// can disagree — and a process that never loaded a config has that gate
	// deliberately inert, which this check must be too.
	if dir := modelaffinity.GPULeaseDir(); dir != "" {
		lease := gpulease.InspectDir(dir)
		if fenced, why := delegate.ForeignFence(lease); fenced {
			return deferWire(core.DeferClassCapacity, fmt.Sprintf(
				"gpu busy: %s; no new run is admitted on this box until it is released (%s)", why, delegate.HolderLine(lease)))
		}
	}
	cordonStart := time.Now()
	if lerr := modelaffinity.AwaitRunSlot(ctx, p.cfg.Endpoint, seat, admissionEnd); lerr != nil {
		admitted = cordonWait(cordonStart)
		admitNote = "held at the cordon for the admission budget"
		return deferWire(core.DeferClassCapacity, "gpu busy: "+lerr.Error())
	}
	// The LOCAL run cap (register C-42): the fleet caps the jobs it sends
	// here; this caps the runs this box starts on its own seat — a
	// delegation's local leg and a fleet job alike — inside the same
	// admission budget, own record excluded. A slot that never frees is a
	// capacity defer, re-placeable, never a refusal.
	if reg, rerr := gpuactivity.Open(p.cfg.GPULockPath, p.cfg.StateDir); rerr == nil {
		if serr := modelaffinity.AwaitSeatSlot(ctx, reg.OnSeat, seat, "", act.ID(), p.cfg.FleetConcurrencyLimit(), admissionEnd); serr != nil {
			admitted = cordonWait(cordonStart)
			admitNote = "held at the seat cap for the admission budget"
			return deferWire(core.DeferClassCapacity, "seat busy: "+serr.Error())
		}
	}
	admitted = cordonWait(cordonStart)
	// Admission pre-flight (2026-09-02): the wall must not pay for ANOTHER
	// session's model swap. llama-swap queues a request silently while it
	// evicts and loads, so a contract that arrived mid-swap spent its whole
	// wall in that queue and reported "wall timeout" — the 600 s failures
	// several parallel sessions reported on 2026-09-01. Wait, bounded and
	// OUTSIDE the wall, until nothing on the endpoint is mid-swap; then start
	// the clock. Known bound: the drain phase before a swap shows nothing
	// non-ready, so a swap that begins a second later is still charged to the
	// wall — this removes the swap WINDOW from the wall, not the race.
	preflight, preNote := awaitSeatAdmission(ctx, p.cfg.Endpoint, seat, time.Until(admissionEnd))
	admitted += preflight
	admitNote = preNote
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
	var coldLoad time.Duration // this run's observed cold load, if the warm-up waited for one (seat-rates.json)
	var coldLoaded bool        // the warm-up ATTEMPTED a load this run (D-118 reads this, never coldLoad > 0: a sub-tick load measures 0)
	act.Phase("cold-load")
	warmed, warmNote, warmAttempted := warmSeat(ctx, p.cfg.Endpoint, seat, admissionBudget(p.cfg.AgentAdmissionWaitSec)-admitted)
	// Admission time whether or not a load was attempted: a warm-up held behind
	// a GPU lease (2026-09-22) spends the budget waiting and loads nothing. Every
	// other non-attempt returns zero, so this changes nothing else.
	admitted += warmed
	if warmAttempted {
		// coldLoaded is "the warm-up ATTEMPTED a load", and warmSeat is the only
		// thing that knows it: a sub-tick load measures 0 and, since W-08, a note
		// is also returned by exits that warmed nothing. Deriving it from either
		// silently un-fired the D-118 coherence probe on a fast box.
		coldLoad = warmed
		coldLoaded = true
	}
	if warmNote != "" {
		log.Printf("agent task: seat warm-up (%s): %s", seat, warmNote)
		if admitNote == "" {
			admitNote = warmNote
		} else {
			admitNote += "; " + warmNote
		}
	}
	// Post-warm COHERENCE probe (register D-118). A seat that is HEALTHY by
	// every other gate can still be numerically broken: on 2026-09-16/17 a
	// Qwen3.8-27B GSQ seat (fp8_e5m2 KV, RTX 5060 Ti, vLLM 0.29 / FlashInfer)
	// passed /health, /v1/models, the speed probe and the READY smoke, then
	// answered every contract with `<tool_call>!!!!!!!!!!!!!!!!!!!!…` to the cap
	// — 126–336 s of degenerate output each, filed as `unparsed_tool_call`.
	// One ≤ 96-token question here, on what is left of the ADMISSION budget and
	// BEFORE the wall context exists, turns that into a seconds-long
	// infrastructure defer the delegator can re-place on another node.
	//
	// The probe's own time is added to `admitted`, so the wall bookkeeping
	// stays honest: it is admission, not run time, and the wire says so.
	if CoherenceProbeWanted(p.cfg, coldLoaded) {
		act.Phase("coherence-probe")
		v := ProbeSeatCoherence(ctx, p.cfg, seat, admissionBudget(p.cfg.AgentAdmissionWaitSec)-admitted)
		admitted += v.Spent
		RememberCoherence(p.cfg.Endpoint, seat, v)
		if v.Ran {
			coherenceNote = v.Note
		}
		if v.Note != "" {
			log.Printf("agent task: seat coherence (%s): %s", seat, v.Note)
		}
		if v.Broken {
			// Infrastructure, not abstention: the seat answered, and what it
			// answered says the stack under it is broken. The delegator reads
			// core.IncoherentSeatReason off the reason and gives the contract
			// one retry on a DIFFERENT node (delegate.IncoherentSeatDefer).
			return deferWire(core.DeferClassInfrastructure, v.Note)
		}
	} else if note, ok := RecallIncoherentSeat(p.cfg.Endpoint, seat); ok {
		// A WARM run on the seat this process already caught (reviewer
		// finding, D-118). Under the default "cold" policy only the loading
		// contract is probed, and the defer unloads nothing — the broken seat
		// stays resident and contracts 2..N would each spend a whole wall on
		// the output contract 1 already proved degenerate. The memo is cleared
		// by the seat's next cold load, by any later non-broken probe, and by
		// its own TTL, so a fixed seat is never stuck here.
		coherenceNote = note
		log.Printf("agent task: seat coherence (%s): %s", seat, note)
		return deferWire(core.DeferClassInfrastructure, note)
	}
	// Seat-residency probe — the mirror of mcpserver's plannerUnserved gate: a
	// POSITIVE "roster answered and the seat is absent" defers before any
	// planner call; an unreachable/empty roster proceeds and lets the loop's
	// first chat call surface the real transport error.
	//
	// It runs BEFORE the window probe below, and on the admission context for
	// the same reason: the probe reads /upstream/<seat>/props, and llama-swap
	// answers that route by LOADING the model. Asking it about a seat this
	// endpoint does not serve would cold-start something as the side effect of a
	// run that is about to defer — the side effect the seat-pin probe is placed
	// after the loop to avoid.
	if roster, rerr := swapclient.FetchRoster(ctx, p.cfg.Endpoint, agentRosterProbeTimeout); rerr != nil {
		// Fail-open is right (the loop's first chat call surfaces the real
		// transport error with far more detail), but SILENT fail-open is not:
		// this is the first place a dead or misconfigured endpoint shows, and
		// swallowing it turns the follow-on loop error into a mystery.
		log.Printf("agent task: seat roster probe of %s failed (proceeding; the loop will surface any real transport failure): %v", p.cfg.Endpoint, rerr)
	} else if roster.Len() > 0 && !roster.Serves(seat) {
		return deferWire(core.DeferClassConfig, fmt.Sprintf("agent seat %q is not in the endpoint's served roster", seat))
	}
	// The SERVED-WINDOW probe is the last admission step, and it runs HERE —
	// before the wall context exists (register S-24). agent.ProbeServedWindow
	// carries a ten-minute cold-start budget on purpose: it is allowed to absorb
	// a seat's load, because the caller is about to use exactly that seat. Run on
	// the WALL context, as it was until now, that licence was spent out of the
	// contract's own clock, and a slow or cold seat consumed the whole wall
	// before the first token — reported as a wall timeout, with nothing to show
	// for it. Every other pre-token step (cordon, pre-flight, warm-up, coherence)
	// is already paid from admission; this one now is too, bounded by the same
	// deadline and reported in the same admission_wait_sec.
	//
	// An admission budget already spent leaves the probe a dead context, so it
	// fails open to this box's agent_ctx_tokens — the same fallback an
	// unanswerable probe has always taken. That is the right trade: a box that
	// spent 300 s loading has a cold-start problem, not a window problem, and
	// the alternative is handing the wall back the licence this fix removes.
	act.Phase(gpuactivity.PhaseWindowProbe)
	probeStart := time.Now()
	pctx, pcancel := context.WithDeadline(ctx, admissionEnd)
	probed, probeOK, probeFence := agent.ProbeServedWindowChecked(pctx, p.cfg.Endpoint, seat)
	probeCtxErr := pctx.Err() // read BEFORE the cancel below, which would mask a spent deadline
	pcancel()
	admitted += time.Since(probeStart)
	if probeFence != nil {
		// A render or an exclusive hold took the card AFTER this run passed the
		// cordon, and the seat is not resident (2026-09-22). The probe's route
		// would have loaded the seat onto the held card, so it was never sent,
		// and it waited for the card inside the admission budget. Starting the
		// run now would only move the same wait into the wall — every request it
		// makes waits behind the same fence — so it defers `capacity` with the
		// holder named, before any wall exists, and the delegator re-places it.
		admitNote = joinAdmissionNotes(admitNote, "window probe held behind the GPU lease for the admission budget")
		return deferWire(core.DeferClassCapacity, "gpu busy: "+probeFence.Error())
	}
	// WHICH window the loop is about to budget against, and where it came from.
	// agent.ResolveContextTokens has always returned that line and both doors
	// dropped it (`effCtx, _ :=`), so a run that silently compacted at the 8,192
	// fallback was indistinguishable on the wire from a correct one — the same
	// invisibility that let the MCP door measure 8,192 cold and 114,688 warm for
	// months without anyone being able to say which run was which.
	effCtx, ctxNote := agent.ResolveContextTokens(0, probed, p.cfg.AgentCtxTokens, probeOK)
	wire.CtxWindowNote = ctxNote
	if ctxNote != "" {
		log.Printf("agent task: %s", ctxNote)
	}
	// A fallback the ADMISSION BUDGET caused is an admission finding, not just a
	// window one: it is the cost of this change's own trade (the probe no longer
	// gets to spend the wall), and it belongs beside the steps that spent the
	// budget.
	if !probeOK && errors.Is(probeCtxErr, context.DeadlineExceeded) {
		admitNote = joinAdmissionNotes(admitNote, "window probe ran out of admission budget; "+ctxNote)
	}
	// Publish the wall this run is ACTUALLY under (register D-116), HERE — at
	// the one line where the wall actually begins, and never before it. The
	// node door writes it onto the job record, so a poll of a RUNNING job
	// carries it and the delegator can bound its own clock by the node's,
	// instead of learning the number only from a final result a dead node
	// never sends. No reporter on the context (the local lane, a test) = a
	// no-op.
	//
	// WHY NOT AT THE SIZING (where it was first written, D-116 review finding
	// 1): everything above this line — the fence check, the cordon wait, the
	// llama-swap pre-flight, the cold-load warm-up, the coherence probe and the
	// served-window probe — is ADMISSION, up to admissionBudget (300 s by
	// default) spent in job state `running` because the node claims a job
	// before it runs it. Publishing the wall at
	// the top made `wall_sec` mean "a wall was SIZED", and a delegator that
	// anchors its clock on it would have started counting a 300 s wall while
	// the seat still had 250 s of loading to do. Published here it means "the
	// wall has STARTED", which is the fact the delegator's clock needs. An
	// admission defer therefore publishes no wall at all — correct: no wall
	// ever ran. (Safe to move: `wall_sec` ships for the first time in this
	// same release, so no deployed node ever published the earlier meaning.)
	core.ReportWall(ctx, timeoutSec)
	// Liveness walls (0.131.0, ADR 0055): the wall above is the EXPECTATION.
	// The run is ended by a STALL (no progress inside the seat's dynamic
	// allowance) or by the safety CEILING — never by the expectation expiring
	// while the seat is still producing. The estimate is computed here, not
	// after admission's readbacks below, because the ceiling needs it.
	est := wallEstimateFor(p.cfg, contract, seat, rates.Get(seat), coldLoad.Seconds(), timeoutSec)
	ceilingSec := CeilingFor(timeoutSec, est)
	wire.CeilingSec = ceilingSec
	livePolicy := LivenessPolicyFor(p.cfg, rates.Get(seat), admissionBudget(p.cfg.AgentAdmissionWaitSec))
	cctx, live := agent.NewMonitor(ctx, livePolicy, time.Duration(ceilingSec)*time.Second)
	defer live.Stop()
	// The cold-load hold (0.140.0): the admission warm-up above loads a seat
	// that is absent when the run STARTS, but the seat can also be evicted
	// between two steps (another model's swap; the 5-minute idle unload during
	// a long tool call). Its next request then waits in llama-swap for the
	// reload, and a prefill clock sized from the prefill rate alone filed that
	// normal load as a stall. While /running reads the seat loading, the
	// monitor suspends the stall clock under a bounded cold-load ceiling, and
	// tells the observer so status readers and the delegator see the phase.
	obs := newProgressObserver(ctx, act, ceilingSec)
	if probe := seatLoadProbe(p.cfg.Endpoint, seat); probe != nil {
		live.WithSeatProbe(probe, func(ph agent.Phase, allow time.Duration) {
			log.Printf("agent task: liveness for %s: %s (allowed %.0fs)", seat, ph, allow.Seconds())
			obs.OnAllowance(string(ph), allow)
		})
	}
	log.Printf("agent task: liveness for %s: ceiling %d s (wall %d s, estimate %d s), floor %s, prefill %.0f tok/s, decode %.1f tok/s, cold-load ceiling %s (%s)",
		seat, ceilingSec, timeoutSec, est.TotalSec, livePolicy.Floor, livePolicy.PrefillTokS, livePolicy.TokS, livePolicy.ColdLoad, livePolicy.ColdLoadBasis)
	// One busy-seat budget for the WHOLE contract (seatwait): every chat step
	// and the re-pack draw on it, so a 10-step loop cannot spend ten budgets,
	// and the wait it consumed is reported on the wire (contention_wait_sec).
	contention = seatwait.NewBudget(p.cfg.SeatContentionWaitSec)
	cctx = seatwait.WithBudget(cctx, contention)

	// Depth (roast delta 2): buildAgentRun already derived
	// contract.Depth = max(1, wireDepth). Nothing here consumes it YET because
	// agent.BuildConfig has no depth field and v1's Build registers no delegate
	// tool for ANY caller — the hop limit holds structurally (the tool does not
	// exist), so depth denies nothing node-side in v1. When the delegate tool
	// lands (plan Task 6, v2 for the in-loop surface), its depth==0-only
	// registration must key off contract.Depth here.
	//
	// The Build mirrors mcpserver.handleAgentRun's read-only front door: NO
	// write/run/fetch/github capability, recordless offload on the PLANNER
	// seat when it is not the workhorse (0.115.18, D-88: the workhorse shares
	// the seat's llama-swap and loading it evicts the planner). Unattended=true
	// is honest — a fleet job has no human to answer a broker ask.
	if m, onSeat := InLoopOffloadModel(seat, p.cfg.Model); onSeat {
		// Ledger evidence for D-88: which model the in-loop tools ride, and why.
		log.Printf("agent task: in-loop offload_* tools run on the planner seat %q without thinking (the workhorse %q shares its llama-swap and loading it would evict the seat)", m, p.cfg.Model)
	}
	// The WRITE door (register D-06). Default OFF: a contract that names no
	// write_root builds exactly the read-only loop it always did, and a node
	// that has not set agent_allow_write refuses one that does. The refusal is
	// a `write`-class defer rather than a job error because the contract is
	// sound and the fleet is healthy — another node may have opted in, and
	// re-placing it there is the delegator's right reflex.
	var door *writeDoor
	if contract.WriteRoot != "" {
		if !p.cfg.AgentAllowWrite {
			return deferWire(core.DeferClassWrite, fmt.Sprintf(
				"this node does not open the write door: agent_allow_write is false in the config it loaded, and the contract asks to write under %q", contract.WriteRoot))
		}
		d, derr := openWriteDoor(contextDir, contract)
		if derr != nil {
			// A write_root that cannot be resolved or created here is a
			// node-side condition, not a claim about the contract's work.
			return deferWire(core.DeferClassWrite, derr.Error())
		}
		door = d
	}
	writeLimit := (*agent.WriteLimit)(nil)
	allowWrite, writeWorktree := false, ""
	if door != nil {
		writeLimit, allowWrite, writeWorktree = door.limit, true, door.root
	}
	built, berr := agent.Build(agent.BuildConfig{
		PlannerBase: p.cfg.Endpoint,
		Model:       seat,
		Timeout:     wall,
		MaxSteps:    contract.MaxSteps,
		MaxTokens:   p.cfg.AgentMaxTokens, // the executing node's budget (its seat's reasoning cost is its own fact)
		ReadRoot:    contextDir,
		Offload:     NewRecordlessOffloadForPlanner(p.cfg, seat, wall), // on the seat itself when the seat is not the workhorse (D-88: the workhorse would evict it)
		NPU:         NewLoopNPU(p.cfg),
		Accel:       NewLoopAccel(p.cfg), // every lane the box lists (ADR 0037): a remote contract sees the tools a local run does
		Unattended:  true,
		EnvRules:    p.cfg.AgentEnvRules,
		Thinking:    thinkingFor(p.cfg, contract), // contract > this box's agent_thinking > auto
		// The executing node's own decoding policy (D-95b): a sampling setting
		// is a fact about THIS seat, measured here, so it is never carried on
		// the contract.
		Sampling:      p.cfg.AgentSampling,
		SamplingFinal: p.cfg.AgentSamplingFinal,
		// The contract's own replay list, behind this box's seeded context
		// reads when agent_seed_context_reads is on (core.SeedContextReads:
		// the node knows the doc file names it just wrote; the delegator
		// would be guessing). Validated at contract decode; the loop answers
		// "does this tool exist on this seat" as an observation.
		SetupActions: setupActionsFor(p.cfg, contract),
		// The write door grants create+overwrite inside write_root and NOTHING
		// else: no delete (the `edit` profile does not even advertise
		// delete_file), no shell, no `run`, no fetch, no github. Overwrite is
		// ON because a seat that cannot change an existing file cannot do an
		// implementation leg at all, and it is safe because the tree it
		// overwrites is this node's own throwaway copy of the context docs.
		AllowWrite:     allowWrite,
		AllowOverwrite: allowWrite,
		Worktree:       writeWorktree,
		WriteLimit:     writeLimit,
	})
	if berr != nil {
		if errors.Is(berr, core.ErrAgentEnvRules) {
			// The table is this box's config; nothing about the contract or the
			// seat can fix it — say so by class.
			return deferWire(core.DeferClassConfig, "building agent: "+berr.Error())
		}
		return deferWire(core.DeferClassInfrastructure, "building agent: "+berr.Error())
	}

	// Window budgeting parity with handleAgentRun: the SERVED window (probed and
	// resolved above, on the admission budget; conservative fallback when
	// unanswerable, and ctx_window_note says which) and the measured-ON ladder
	// rungs with the real-tokenizer seam (fail-open to the legacy estimate).
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
	if door != nil && strings.TrimSpace(contract.Profile) == "" {
		// A write contract that named no profile must not inherit this box's
		// `agent_profile`: on a small-seat node that is "research", whose tool
		// subset does not list edit_file or write_file — so the door would be
		// open, the tools registered, and the model unable to see them. "edit"
		// is the write door's shape (locate, read, change) and it is the one
		// profile that advertises exactly the six tools the door grants.
		profileName = "edit"
	}
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

	// Wall sizing (0.115.21, register D-03): estimate what this contract needs
	// on THIS seat from the seat's remembered rate and cold load, publish it
	// beside the run (wall_estimate_sec / min_turn_sec / wall_note) and log
	// when the contract's wall is under it. Never changes the wall: sizing is
	// data for the caller and the operator, not a silent override.
	// (est was computed where the liveness ceiling was sized, above.)
	wire.WallEstimateSec, wire.MinTurnSec, wire.WallNote = est.TotalSec, est.MinTurnSec, est.Note
	if wire.WallSec > 0 {
		wire.WallNote = "auto wall (timeout_auto): " + wire.WallNote
	}
	if est.Below {
		log.Printf("agent task: wall sizing (%s): %s", seat, est.Note)
	}

	// The final budget FITS the wall (0.122.1, register D-95). Sizing the wall
	// from the seat's rate told the caller the contract would not fit; it did
	// nothing about the run in flight, which still opened its final answer at
	// the configured 4× budget and, on a 15 tok/s seat with an output_schema,
	// owed ≈ 1,420 s of a 900 s wall (METHODOLOGY.md and SELF-CONTROL.md,
	// 2026-09-14). Now the same arithmetic runs backwards: the largest final
	// budget the REMAINING wall can decode, split with the re-pack when a
	// schema is set, capped by the configured rule and floored at 1,024.
	// Published here for the run as a whole; the loop recomputes it at the
	// forced final step, where the tool steps are spent and the live clock is
	// the honest input.
	// The re-pack half of the pair is unused here now: the list-cap gate below
	// sizes from the fitted final alone, with no re-pack term (register
	// S-06/W-07) — only finalBudgetsFor's FINAL half still has a reader in
	// this function.
	finalBudget, _ := finalBudgetsFor(p.cfg, contract)
	hasSchema := len(contract.OutputSchema) > 0
	startFit := seatrate.FitFinalBudget(seatrate.FinalFit{
		ConfiguredFinal: finalBudget,
		RemainingSec:    float64(timeoutSec),
		OtherSec:        float64(est.OtherSec),
		TokS:            est.TokS,
		Schema:          hasSchema,
	})
	wire.FinalBudgetFit, wire.BudgetNote = startFit.Budget, startFit.Note
	if startFit.Note != "" {
		log.Printf("agent task: final-budget fit (%s): %s", seat, startFit.Note)
	}
	built.Loop.WithFinalBudgetFit(func(configured int, remaining time.Duration) (int, string) {
		// At the final turn the cold load, the think block and the tool steps
		// are already spent, so only the answer (and its re-pack) still has to
		// fit — OtherSec is 0 here on purpose.
		f := seatrate.FitFinalBudget(seatrate.FinalFit{
			ConfiguredFinal: configured,
			RemainingSec:    remaining.Seconds(),
			TokS:            est.TokS,
			Schema:          hasSchema,
		})
		return f.Budget, f.Note
	})
	// A cut final on a schema contract is re-issued ONCE with the schema's own
	// list caps (D-95), gated on the wall still holding one turn at this seat's
	// rate. No schema, no instruction, no re-issue — and no rate means a 0
	// floor, which fails open exactly like every other sizing decision here.
	//
	// The gate sizes from the FITTED final (startFit.Budget, D-95 above), not
	// the CONFIGURED one, and carries no re-pack term (register S-06/W-07,
	// 2026-09-17 diagnosis): the re-issue is ONE turn at whatever budget the
	// wall fit already narrowed the final answer to, and nothing about it gets
	// re-packed on top. Gating on MinTurnFor(0, finalBudget, repackBudget,
	// tokS) — the full CONFIGURED final (up to 8,192) plus a full re-pack term
	// — asked for roughly double what the re-issue actually costs; below ~9
	// tok/s that exceeded the 900 s wall cap and the gate could never fire at
	// all. It was the #1 measured defer fleet-wide (48 rows "re-pack skipped:
	// the final answer was cut at the completion budget").
	if hasSchema {
		built.Loop.WithCutFinalReissue(
			listCapInstruction(contract.OutputSchema),
			time.Duration(seatrate.MinTurnFor(0, startFit.Budget, 0, est.TokS))*time.Second,
		)
	}

	built.Loop.WithObserver(obs).WithLiveness(live)
	act.Phase("running")
	res, rerr := built.Loop.Run(cctx, contract.Goal)
	// The run's last liveness reading rides the wire on every branch below —
	// a `stalled:` or `ceiling` reason reads against these.
	if _, tokS, last, allow := live.Snapshot(); allow > 0 {
		wire.StallAllowanceSec, wire.LastProgressMs = int(allow.Seconds()), last.UnixMilli()
		wire.ObservedTokS = tokS
	}
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
	// The seat's measured numbers from THIS run feed the next estimate: the
	// effective decode rate over the ≥ 1,024-token completions and the cold
	// load the warm-up waited for. Recorded on every branch — a timed-out run
	// measured the seat just as well.
	if tokS, n := seatrate.Rate(rateCalls(res.Calls)); n > 0 {
		wire.SeatTokS = tokS
	}
	pf := res.Prefill
	// The largest time-to-first-delta sample of the run (0.131.1): engine-
	// neutral, and present on a DEFERRED run too — a stalled prefill on an
	// unmeasured seat is measured by the very run it cost.
	var bestSample agent.PrefillSample
	for _, s := range res.PrefillSamples {
		if s.Tokens > bestSample.Tokens {
			bestSample = s
		}
	}
	if p.seatRatesPath != "" && (wire.SeatTokS > 0 || coldLoad > 0 || pf.PrefillTokens > 0 || bestSample.Tokens > 0) {
		// Load-observe-save under the store's lock (seatrate.Update): the
		// pre-loop read above was a snapshot for the estimate; another process
		// may have written since. The prefill rate (0.131.0) sizes the next
		// run's stall allowance while the seat prefills.
		if uerr := seatrate.Update(p.seatRatesPath, func(s *seatrate.Store) {
			s.Observe(seat, wire.SeatTokS, coldLoad.Seconds(), time.Now())
			s.ObservePrefill(seat, pf.PrefillTokens, pf.PrefillMS, time.Now())
			s.ObservePrefill(seat, bestSample.Tokens, bestSample.MS, time.Now())
		}); uerr != nil {
			log.Printf("agent task: seat-rates store not updated: %v", uerr)
		}
	}
	// The write set (D-06) — rendered HERE, before every defer branch, for the
	// same reason as the trace and the prefill accounting above: a run that hit
	// its step budget or its wall may still have made a real, reviewable change,
	// and a door that only published on the success path would throw that away
	// exactly when the caller most needs to see what the seat did.
	if door != nil {
		diff, files, note, derr := closeWriteDoor(door)
		wire.Diff, wire.DiffFiles, wire.WriteNote = diff, files, note
		if derr != nil {
			// A cap breach is the door refusing, not the work failing: no diff
			// is published and the class says which fix applies (a smaller
			// change), not which box is broken.
			wire.WriteNote = derr.Error()
			return deferWire(core.DeferClassWrite, derr.Error())
		}
	}
	wire.StopNote = res.StopNote
	wire.OutputTruncated = res.OutputTruncated
	// The fit the loop actually ran at supersedes the run-start prediction, and
	// the re-issue it made — same rule as the trace above: set before every
	// defer branch, because a run that ended on the wall is the one whose
	// budget arithmetic most needs reading.
	if res.FinalBudgetFit > 0 {
		wire.FinalBudgetFit, wire.BudgetNote = res.FinalBudgetFit, res.BudgetNote
	}
	wire.FinalReissue = res.FinalReissue
	wire.ResponseShape = agent.ResponseShape(res.Calls)
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
		// A GPU-lease refusal FIRST (2026-09-22): a render or an exclusive hold
		// took the card mid-run, the run's next request waited for it and the
		// wait ran out. Whichever clock ended the wait — the request's own
		// budget, the stall watch, the ceiling — the cause is the held card, and
		// the honest class is `capacity`, which the delegator re-places. Filed as
		// a stall it would blame the seat; as a budget defer it would poison the
		// delegator's contract sizing.
		if modelaffinity.IsLeaseRefusal(rerr) {
			return deferWire(core.DeferClassCapacity, "gpu busy: "+rerr.Error())
		}
		// Wall timeout is its own defer shape — the delegator sizes future
		// contracts off it, so it must be distinguishable from a planner error.
		if se := stallOf(live); se != nil {
			// The seat STOPPED producing for this run (0.131.0): no streamed
			// delta, tool call or phase change inside its allowance. The
			// contract was fine — this is the seat's health, so it is filed as
			// infrastructure, never as the budget signal the delegator sizes
			// from. The stall text carries the arithmetic a reader can check.
			return deferWire(core.DeferClassInfrastructure, se.Error())
		}
		// The CEILING (0.131.0) cancels with a typed cause, not a deadline —
		// it is checked by cause, before the clock. A bare DeadlineExceeded
		// here is the CALLER's deadline (ceilingReason says so).
		if ceilingOf(live) != nil || errors.Is(cctx.Err(), context.DeadlineExceeded) {
			if contention.CausedTimeout(wall) {
				// The wall was eaten by WAITING on peers, not by the model's own
				// work: filing it as a budget defer would poison the delegator's
				// contract sizing (the council's finding, 2026-09-02). An early
				// 429 that resolved does not qualify — CausedTimeout needs a sleep
				// in flight at expiry or waits covering half the wall.
				r, _ := contendedReason(seat, contention)
				return deferWire(core.DeferClassInfrastructure, r+"; the wall expired during the wait")
			}
			return deferWire(core.DeferClassBudget, ceilingReason(live, timeoutSec))
		}
		if errors.Is(cctx.Err(), context.Canceled) {
			// The PARENT went away mid-loop — the same shape the re-pack branch
			// below already carries its own arm for (register S-22/W-16, 2026-
			// 09-17 diagnosis: "a cancelled parent is filed as broken
			// hardware", 12 "agent loop: context canceled" rows). The failed
			// request looks exactly like a dial refusal — a *url.Error — so it
			// used to fall through to the generic "agent loop: "+rerr.Error()
			// branch below and defer as infrastructure: an operator told to fix
			// a box that never misbehaved. Budget is the honest class: a
			// ceiling outside the model's control stopped the run.
			//
			// The reason names NO cause (PR #366 review, correctness blocker):
			// on the FLEET NODE path the only thing that ever cancels this
			// context is fleetnode.Jobs.DrainAndStop (a node drain, never a
			// caller) — and its own mark ("interrupted") is written BEFORE the
			// cancel that releases this run, so finish() is write-once against
			// an already-terminal job and this defer's words are NEVER what the
			// delegator reads on that path (proven by
			// TestJobsDrainDiscardsALateAgentBudgetDefer,
			// internal/fleetnode/jobs_test.go). Only the LOCAL in-process path
			// (RunAgentContract, no Jobs store in front of it) can ever surface
			// this string to a human, and there a cancel genuinely IS the
			// caller's own context ending — but the wording must not assert a
			// cause the fleet-node path does not share, so it names both.
			return deferWire(core.DeferClassBudget, "agent loop: canceled (the parent context ended — the caller gave up, or this box is draining)")
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

	if res.StopReason == agent.StopToolCallCut {
		// The seat's tool-call ARGUMENT was cut by the completion budget twice
		// (register D-114). The loop's note already names both budgets and the
		// partial argument, and the class is BUDGET — until 0.124.0 llama.cpp's
		// 500 reached the generic "agent loop:" branch below and the run was
		// filed as `infrastructure`, which blames the box for a ~3 KB write
		// asked for in one call at a 1,024-token step budget. Nothing to
		// re-pack: Output is empty on this path.
		reason := res.StopNote
		if reason == "" {
			reason = "tool-call argument cut at the completion budget twice"
		}
		return deferWire(core.DeferClassBudget, reason)
	}
	if res.StopReason == "budget" {
		// The loop burned MaxSteps without a final answer. Output is empty on
		// this path, so there is nothing to re-pack — defer, don't dress an
		// unfinished run as a result.
		reason := fmt.Sprintf("step budget exhausted (%d steps)", res.Steps)
		if res.StopNote != "" {
			// 0.115.19: the forced final step's evidence (the seat answered a
			// tool-less "answer now" turn with a tool call) reaches the reason as
			// well as stop_note; the prefix stays the grep key (rig, ledger).
			reason += ": " + res.StopNote
		}
		return deferWire(core.DeferClassBudget, reason)
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

	// The answer may ALREADY be the requested object (0.115.12, D-84): the goal
	// asks for the shape, thinking-off finals on the vLLM seats answer in it,
	// and re-packing 19 KB of JSON into the same JSON costs another ~6,000
	// tokens — the 0.115.10 acceptance run's 27B row spent its last 200 s of
	// wall there. Validate the loop's own text first; only prose re-packs.
	if direct, ok := directStructured(res.Output, contract.OutputSchema); ok {
		wire.Structured = direct
		return finish(wire)
	}
	// D-91 (0.115.23): a final answer cut at the completion budget is a
	// PARTIAL — a JSON prefix or a truncated narrative — and every re-pack of
	// it is a full re-generation that cannot produce the whole object. The
	// 2026-09-10 Lenovo run spent ~690 s (two grammar attempts + the chat
	// lane over a 12,100-char cut answer) to the 900 s wall and deferred
	// "wall timeout" on a loop that had finished in four minutes. Name the
	// shape at once instead; the partial rides in output for the caller.
	// D-95 (0.122.1) narrowed this branch: the loop now re-issues a cut,
	// JSON-shaped final ONCE with the schema's own list caps before it gets
	// here, so reaching this point with a schema contract means either the
	// re-issue was not possible (no wall left, or the partial was prose) or the
	// capped answer was cut as well. The abstention is the same; the note names
	// which of the two it was.
	if res.OutputTruncated {
		wire.RepackNote = truncatedRepackNote(res.FinalReissue, res.Calls)
		return deferWire(core.DeferClassAbstention, "output failed schema: "+wire.RepackNote)
	}
	repackStart := time.Now()
	act.Phase("repack")
	// Liveness (0.131.0): the re-pack is its own phase (allowance = its chat
	// timeout) and its streamed deltas feed the same watch as the loop's.
	live.Phase(agent.PhaseRepack, 0)
	structured, tokensOut, transport, attempts, serr := p.repackStructured(agent.ContextWithProgress(cctx, live.Progress), seat, contract.OutputSchema, res.Output, repackAttemptFloor(wall))
	wire.RepackMs = time.Since(repackStart).Milliseconds()
	wire.RepackAttempts = attempts
	if serr != nil {
		wire.RepackNote = serr.Error()
		// wire.Output stays populated on every branch below so the CALLER still
		// receives the loop's answer even when the structured shape never
		// arrived. It is preserved for the caller, NOT for delegator-side
		// acceptance: every branch below returns deferWire, which sets
		// Deferred, and delegate.runLocal/runRemote both run acceptance only
		// when !wire.Deferred — so no check can ever read it on this path.
		cutoff, isCutoff := asRepackCutoff(serr)
		switch {
		case ceilingOf(live) != nil || errors.Is(cctx.Err(), context.DeadlineExceeded):
			// The wall expired DURING the re-pack. That is the timeout shape,
			// not a schema shape — reporting it as "output failed schema" sends
			// the operator to rewrite a schema that was never the problem.
			if contention.CausedTimeout(wall) {
				// Same stable prefix as the transport arm below (§S2 keys on it);
				// the contention detail rides after it.
				r, _ := contendedReason(seat, contention)
				return deferWire(core.DeferClassInfrastructure, "structured re-pack unreachable: "+r+"; the wall expired during the wait")
			}
			return deferWire(core.DeferClassBudget, ceilingReason(live, timeoutSec))
		case stallOf(live) != nil:
			// The seat stopped producing DURING the re-pack (0.131.0): the
			// seat's health, not the schema's — same class as the loop arm.
			return deferWire(core.DeferClassInfrastructure, "structured re-pack unreachable: "+stallOf(live).Error())
		case errors.Is(cctx.Err(), context.Canceled):
			// The PARENT went away mid-re-pack (the delegator abandoned the
			// poll, the node is shutting down). The failed request looks exactly
			// like a dial refusal — a *url.Error — so it used to be reported as
			// infrastructure: an operator told to fix a box that never
			// misbehaved. Budget is the honest class: a ceiling outside the
			// model's control stopped the run.
			return deferWire(core.DeferClassBudget, "canceled during the structured re-pack (the caller's context ended)")
		case isCutoff:
			// The DECISIVE (last-run) re-pack attempt was ended by its OWN
			// per-attempt bound (repackAttemptDeadline, register D-108) — not
			// by the seat, not by the wall (that is the DeadlineExceeded arm
			// above, which the real cctx would already have caught) and not by
			// a caller (that is the Canceled arm above). Filing a self-imposed
			// cutoff as infrastructure recreates, one arm over, the exact
			// defect this PR already fixes for a canceled parent (PR #366
			// correctness review): the box did nothing wrong.
			return deferWire(core.DeferClassBudget, "structured re-pack "+cutoff.Error())
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

// AgentContractOptions is what a caller that already DECIDED where a contract
// runs hands RunAgentContract (ADR 0039): the seat and the placed block. It is
// delegate.LocalOptions by alias — the same type, not a copy — so the method
// value p.RunAgentContract satisfies delegate.LocalRunner directly and the
// delegate runner, the MCP doors and the fleet smoke all hand over one shape.
// The zero value runs the planner seat and publishes nothing.
type AgentContractOptions = delegate.LocalOptions

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
// text-verb acceptance can stand alone). The context cap is the BOX's
// (config.AgentContextCapBytes: 256 KiB on a plain box, scaled to the largest
// layer window on a composite one), the same cap the agent doors validate
// against — otherwise a contract sized for the long seats would pass the door
// and be refused here. opts carries a decided seat/placement (see
// AgentContractOptions); the zero value is every pre-0.116 call.
func (p *Pipeline) RunAgentContract(ctx context.Context, contract core.AgentContract, opts AgentContractOptions) (core.AgentWireResult, error) {
	if err := contract.ValidateWithCap(p.cfg.AgentContextCapBytes()); err != nil {
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
	params := map[string]any{
		"contract":    contract,
		"context_dir": contextDir,
		"job_id":      filepath.Base(jobDir),
	}
	// Only set when decided: runAgentTask reads absent keys as "the planner
	// seat, no placed block", so an unset option leaves the wire untouched.
	if opts.Seat != "" {
		params["seat"] = opts.Seat
	}
	if opts.Placed != nil {
		params["placed"] = opts.Placed
	}
	res := p.Run(ctx, core.Request{
		Task:   core.TaskAgentRun,
		Input:  contract.Goal,
		Params: params,
		Door:   contract.Door,
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
//
// repackAttemptFloor is the least wall a re-pack attempt is started with for a
// contract of the given wall: a tenth of the wall, capped at
// agentRepackAttemptFloor — so a 900 s contract keeps 45 s back, a 300 s one
// 30 s, and a 30 s test contract 3 s.
func repackAttemptFloor(wall time.Duration) time.Duration {
	f := wall / 10
	if f > agentRepackAttemptFloor {
		f = agentRepackAttemptFloor
	}
	return f
}

// floor is the least remaining wall an attempt may start with (0 = no bound).
func (p *Pipeline) repackStructured(ctx context.Context, seat string, rawSchema json.RawMessage, output string, floor time.Duration) (structured json.RawMessage, tokensOut int, transport bool, attempts int, err error) {
	var schema map[string]any
	if uerr := json.Unmarshal(rawSchema, &schema); uerr != nil {
		return nil, 0, false, 0, fmt.Errorf("output_schema is not a JSON object: %w", uerr)
	}
	fields := gbnf.FromJSONSchema(schema)
	if len(fields) == 0 {
		// Unreachable off the wire (contract Validate gates on this), kept for
		// in-process callers: an empty grammar would constrain nothing.
		return nil, 0, false, 0, errors.New("output_schema has no gbnf-compilable properties")
	}
	// wallLeft reports whether an attempt may still START: every attempt is
	// a full re-generation, and one that cannot finish inside the wall only
	// converts a finished loop into a budget defer (D-91). No deadline = no
	// bound, as before.
	wallLeft := func(what string) bool {
		dl, ok := ctx.Deadline()
		if !ok || floor <= 0 {
			return true
		}
		if left := time.Until(dl); left < floor {
			err = fmt.Errorf("re-pack stopped before the %s: %.0fs of the ceiling left, under the %.0fs an attempt needs", what, left.Seconds(), floor.Seconds())
			return false
		}
		return true
	}
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Name)
	}
	grammar := gbnf.Object(fields)
	// D-129: on a seat this box declares as vLLM, the grammar above is dead
	// weight — vLLM's request model allows unknown extras, so it accepts
	// `grammar` and discards it, and this re-pack was answering
	// unconstrained. That is why the trim and the coercion below exist at
	// all, and why the vLLM seats carried 1,018 re-packs over nine days
	// against 506 on every other seat with 3-attempt exhaustion at 19.7 %
	// against 12.6 %. Send vLLM the field it reads instead; llama.cpp seats
	// keep the raw GBNF byte-identically. WithoutThinking already rides every
	// attempt, which is also what a whole-output constraint requires.
	var structuredOpts []llamaclient.GenOption
	if p.isVLLMSeat(ctx, seat) {
		grammar = ""
		// The CONTRACT's schema, not the GBNF projection: internal/gbnf has no
		// object or array-of-object type, so gbnf.JSONSchema(fields) turned a
		// nested schema into strings and vLLM then CONSTRAINED the answer into
		// the wrong shape (A-100 proof, 2026-09-18: three attempts "got string,
		// want object"). vLLM accepts full JSON Schema; the validator below
		// checks against the same schema, so the two finally agree.
		structuredOpts = append(structuredOpts, llamaclient.WithJSONSchema(schema))
	}
	// The re-pack IS an extract over the loop's final text — same system/user
	// shape as tasks.buildExtract, so the seat sees a prompt pattern it
	// already handles.
	system := "You extract structured data from text. Output ONLY a JSON object with exactly the requested fields. Use empty values when a field is absent."
	user := fmt.Sprintf("Extract these fields from the text: %s.\n\nTEXT:\n%s", strings.Join(names, ", "), output)

	// ONE set of lane-probe closures for the WHOLE call (register D-85/D-108,
	// W-19): before this, repackClient built a fresh FleetLaneGates cache and
	// a fresh LocalSwapBusy closure on every attempt (each attempt called
	// repackClient — or, for the chat fallback, repackViaChat's own copy —
	// independently), so up to three attempts each re-paid a live
	// /v1/models + /running + per-model gauge read of the LOCAL seat before
	// spending a token. Measured fleet-wide: 1,301 rows, median 18 s, p90
	// 109 s, max 581 s, 15.13 h total, 244 rows at all three attempts — and
	// 180 of those deferred anyway. gates threads the SAME closures into
	// every client this call still builds (one per attempt, same as before —
	// a *llamaclient.Client is a cheap struct, never what was expensive here)
	// so their own internal TTL cache does the sharing.
	gates := p.newRepackGates()

	// lastRaw/lastBound/lastAttemptNum describe the MOST RECENTLY failed
	// attempt; earlierNotes carries every one before it. Only the FINAL
	// attempt this call ends on decides transport/budget/abstention (register
	// D-108, PR #366 correctness review): everything earlier is diagnostic
	// context for the operator, never the published class — a seat that
	// self-cut on attempt 1 and definitively refused on attempt 3 is a
	// broken-box report, not a budget one, and the reverse must not read as
	// broken hardware either. recordFailure shifts the PREVIOUS "last" into
	// earlierNotes (formatted, plain text — no wrap chain needed on a note
	// nothing unwraps) the moment a NEWER failure arrives, so the one entry
	// that never lands in earlierNotes is whichever failure turns out to be
	// the final one.
	var earlierNotes []string
	var lastRaw error
	var lastBound time.Duration
	var lastAttemptNum int
	recordFailure := func(raw error, bound time.Duration, attemptNum int) {
		if lastRaw != nil {
			earlierNotes = append(earlierNotes, fmt.Sprintf("attempt %d/%d (bound %s): %v", lastAttemptNum, repackMaxAttempts, lastBound, lastRaw))
		}
		lastRaw, lastBound, lastAttemptNum = raw, bound, attemptNum
	}
	budget := repackBudget(output)
	for attempt := 0; attempt < 2; attempt++ {
		if !wallLeft("grammar re-pack") {
			return nil, 0, false, attempts, err
		}
		attempts++
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
		//
		// attemptTimeout (register D-108, W-19) bounds this ONE http round
		// trip — the client's own transport-level Timeout, exactly the role
		// repackTimeout has always played — narrowed to what an even split of
		// the remaining wall across the attempts still owed a turn actually
		// buys. ctx itself is passed UNCHANGED: wrapping it in a shorter
		// context here would also cut off llamaclient's own seatwait retry
		// loop (a 429/503 answer retries on the CONTRACT's contention budget,
		// not a per-attempt one) and would desynchronize this call's
		// deadline from the wall the caller classifies a failure against.
		attemptTimeout := repackAttemptDeadline(ctx, p.cfg, budget, repackMaxAttempts-attempts+1)
		gres, gerr := p.repackClient(p.cfg.CompletionPath, attemptTimeout, gates).Generate(ctx, seat, system, user, grammar, budget, p.cfg.Temperature, 0, append([]llamaclient.GenOption{llamaclient.WithoutThinking()}, structuredOpts...)...)
		if gerr != nil {
			recordFailure(gerr, attemptTimeout, attempts)
			if seatBusyExhausted(gerr) {
				// The contract's busy-seat budget is spent: the seat answered
				// 429 past every counted wait. Another attempt cannot reach it
				// and would only turn the verdict into "the chat lane refused"
				// — this IS the last attempt, and it is a transport one.
				break
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
			recordFailure(fmt.Errorf("re-pack truncated at %d tokens (the answer is %d chars; the structured budget cannot hold it)", budget, len(output)), attemptTimeout, attempts)
			budget = agentRepackMaxTokensCap
			continue
		}
		// Trim to the outermost {...} before validating. This is the BELT, not
		// the constraint: since D-129 a declared vLLM seat is constrained by
		// `structured_outputs` and is not sent a `grammar` at all, so the
		// fenced answers this trim was added for (0.115.14: "invalid
		// character '`' looking for beginning of value" on the 4B, because
		// vLLM behind llama-swap ignored llama.cpp's `grammar` field) should
		// no longer arrive. It stays because an UNDECLARED vLLM seat, or one
		// whose alias could not be resolved against a dead roster, still
		// falls back to the grammar path — and because a trim costs nothing.
		content := []byte(outerObject(gres.Content))
		if verr := validator.Validate(content, schema); verr != nil {
			if fixed, ok := coerceToSchema(content, schema); ok {
				return json.RawMessage(fixed), gres.TokensOut, false, attempts, nil
			}
			recordFailure(verr, attemptTimeout, attempts)
			continue
		}
		return json.RawMessage(content), gres.TokensOut, false, attempts, nil
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
	if !wallLeft("chat re-pack") {
		return nil, 0, false, attempts, err
	}
	if !seatBusyExhausted(lastRaw) {
		attempts++
		chatTimeout := repackAttemptDeadline(ctx, p.cfg, budget, repackMaxAttempts-attempts+1)
		chatClient := p.repackClient("/v1/chat/completions", chatTimeout, gates)
		structured, tokensOut, cerr := p.repackViaChat(ctx, chatClient, seat, schema, output)
		if cerr == nil {
			return structured, tokensOut, false, attempts, nil
		}
		recordFailure(cerr, chatTimeout, attempts)
	}

	// Every attempt has now run and failed. lastRaw/lastBound/lastAttemptNum
	// describe the FINAL one — its own nature alone decides the verdict
	// (register D-108, PR #366 correctness review): a self-imposed cutoff is
	// never evidence of a broken box (repackCutoffErr, budget); a genuine
	// dial failure / 5xx / 429 on the LAST attempt is (transport=true,
	// infrastructure); anything else — a validation failure, a truncation, a
	// non-429 4xx refusal — means the seat answered and could not be used
	// (abstention). earlierNotes rides along in every case as diagnostic
	// context, never as what decides the class.
	if selfCutoff(lastRaw) {
		msg := fmt.Sprintf("attempt %d/%d cut by its %s share of the wall", lastAttemptNum, repackMaxAttempts, lastBound)
		if len(earlierNotes) > 0 {
			msg = strings.Join(earlierNotes, "; ") + "; " + msg
		}
		return nil, 0, false, attempts, &repackCutoffErr{msg: msg, raw: lastRaw}
	}
	finalErr := fmt.Errorf("attempt %d/%d (bound %s): %w", lastAttemptNum, repackMaxAttempts, lastBound, lastRaw)
	if len(earlierNotes) > 0 {
		finalErr = fmt.Errorf("%s; %w", strings.Join(earlierNotes, "; "), finalErr)
	}
	return nil, 0, genErrIsTransport(lastRaw), attempts, finalErr
}

// outerObject trims text to its outermost {...} span (fences and prose around
// it dropped); text with no such span is returned trimmed as is.
func outerObject(text string) string {
	t := strings.TrimSpace(text)
	if i, j := strings.Index(t, "{"), strings.LastIndex(t, "}"); i >= 0 && j > i {
		return t[i : j+1]
	}
	return t
}

// directWrapperMax is the most prose (fences, "Here is the result:", a
// closing line) that may surround an object for directStructured to take it
// as the answer when the object is under half of the text.
const directWrapperMax = 200

// directStructured reports whether the loop's final text, trimmed to its
// outermost {...} span (fences and prose around it are fine), already
// validates against the contract's schema — after the same lossless scalar
// coercion the re-pack lanes apply. ok=false means "re-pack it"; nothing
// about the text is judged beyond shape.
func directStructured(output string, rawSchema json.RawMessage) (json.RawMessage, bool) {
	var schema map[string]any
	if json.Unmarshal(rawSchema, &schema) != nil {
		return nil, false
	}
	// Only a schema that names fields can be matched by shape: against a
	// property-less schema the validator checks "is this JSON" and nothing
	// more, and a stray {} in prose would pass (review finding, 0.115.12).
	if props, ok := schema["properties"].(map[string]any); !ok || len(props) == 0 {
		return nil, false
	}
	trimmed := strings.TrimSpace(output)
	i, j := strings.Index(trimmed, "{"), strings.LastIndex(trimmed, "}")
	if i < 0 || j <= i {
		return nil, false
	}
	// The object must BE the answer, not an aside inside it: either most of
	// the text, or wrapped in no more than a short preamble/fence/tail — so a
	// code sample or an "{example}" inside a long prose answer still goes
	// through the re-pack.
	if span, outside := j+1-i, len(trimmed)-(j+1-i); span*2 < len(trimmed) && outside > directWrapperMax {
		return nil, false
	}
	content := []byte(strings.TrimSpace(trimmed[i : j+1]))
	if validator.Validate(content, schema) == nil {
		return json.RawMessage(content), true
	}
	if fixed, ok := coerceToSchema(content, schema); ok {
		return json.RawMessage(fixed), true
	}
	return nil, false
}

// repackTimeout bounds ONE re-pack HTTP call by the budget it may generate:
// the seat's per-call `request_timeout_sec` (120 s by default) was sized for
// a one-line answer, and a 3,283-token re-pack on the 4B at ~20 tok/s (the
// 0.115.10 acceptance run) died at that timeout as "structured re-pack
// unreachable" — a transport verdict on a seat that was answering. Six
// tokens per second is the slowest a fleet seat generates (the 4B under a
// second job); the contract wall (cctx) still bounds the call above this.
func repackTimeout(cfg config.Config, budget int) time.Duration {
	t := time.Duration(cfg.RequestTimeoutSec) * time.Second
	if t < agentRepackChatTimeout {
		t = agentRepackChatTimeout
	}
	if byBudget := time.Duration(budget/6) * time.Second; byBudget > t {
		t = byBudget
	}
	return t
}

// repackGates are the cascade-lane probe closures ONE repackStructured call
// shares across every client it builds (register D-85/D-108, W-19).
// llamaclient.LocalSwapBusy and llamaclient.FleetLaneGates each return a
// closure that caches its own probe for a TTL (laneBusyTTL / laneResidencyTTL,
// both a few seconds) — but that cache is only as long-lived as the closure
// itself, and repackClient used to build a fresh one on every call. Building
// them ONCE here and threading the SAME closures into every client this call
// constructs (both grammar attempts AND the chat fallback) is what lets that
// cache do its job: one live probe per window, not one per attempt.
type repackGates struct {
	busy     func() bool
	busyFor  func(model string) (bool, string)
	resident func(base, model string) bool
	route    func(base string) (path, token string)
}

// newRepackGates builds repackGates for one repackStructured call. The lease
// check (busy) is free (a lock-file read) and always built; the network-probe
// pair (busyFor/resident/route) is built only when a cascade lane is actually
// configured — mirroring repackClient's old guard exactly, so a box with no
// cascade_remote_lanes pays nothing extra.
func (p *Pipeline) newRepackGates() repackGates {
	gpuLockPath, stateDir := p.cfg.GPULockPath, p.cfg.StateDir
	g := repackGates{busy: func() bool { return delegate.LocalBusy(gpuLockPath, stateDir) }}
	if len(p.cfg.CascadeRemoteLanes) > 0 {
		g.busyFor = llamaclient.LocalSwapBusy(p.cfg.Endpoint)
		// FleetLaneGates, not RosterResident alone: a lane base may be a
		// plain llama-swap OR a fleet node whose own llama-swap binds
		// loopback (C-41b). The pair shares one probe, so residency and the
		// route can never disagree about which shape a base is.
		g.resident, g.route = llamaclient.FleetLaneGates(p.cfg.FleetAuthToken)
	}
	return g
}

// repackClient is a seat client for one re-pack lane, on the given path, with
// the given transport-level timeout (repackAttemptDeadline sizes it per
// attempt; repackTimeout is still the plain per-budget rule callers that want
// it unbounded by the wall can pass directly) and the same seat-endpoint
// routing the recorded pipeline uses, wired to gates — this call's SHARED
// lane-probe closures (newRepackGates), never rebuilt per client even though
// the *llamaclient.Client struct itself still is (a cheap allocation; the
// probe caches gates carries are the part that was expensive to rebuild).
// Construction otherwise mirrors openPipeline / NewRecordlessPipeline exactly
// — seat endpoints AND the cascade remote lanes — so the re-pack keeps the
// busy-hour failover the pipeline's own client has (review finding,
// 0.115.12).
func (p *Pipeline) repackClient(path string, timeout time.Duration, gates repackGates) *llamaclient.Client {
	c := llamaclient.New(p.cfg.Endpoint, path, p.cfg.Model, timeout).
		WithSeatEndpoints(p.cfg.SeatEndpoints)
	if len(p.cfg.CascadeRemoteLanes) > 0 {
		c = c.WithRemoteLanes(p.cfg.CascadeRemoteLanes, gates.busy, gates.busyFor, gates.resident).WithLaneRoute(gates.route)
	}
	return c
}

// repackAttemptDeadline bounds ONE re-pack attempt (register D-85/D-108,
// W-19): the seat's own per-call allowance (repackTimeout, sized to this
// attempt's budget) narrowed to what the remaining wall actually buys when
// split evenly across the attempts still owed a turn (attemptsLeft, this one
// included). It sizes the CLIENT's transport-level timeout (repackClient),
// never a context wrapped around the call — ctx itself always carries the
// real wall unchanged, so llamaclient's own seatwait retry loop (a 429/503
// answer retries on the CONTRACT's shared contention budget, not a
// per-attempt one) and the caller's wall-timeout classification
// (errors.Is(ctx.Err(), context.DeadlineExceeded), read after this call
// returns) both keep reading the SAME clock they always have. Before this, a
// client's static per-call timeout was the ONLY bound below the contract
// wall, so a slow first attempt could sit on its full allowance with two more
// attempts still due — a wall that had, say, 30 s left let attempt one alone
// burn all 30 rather than leaving room for the retry and the chat fallback.
// No deadline on ctx, or nothing left to divide by, returns the seat
// allowance unnarrowed — the pre-D-108 behaviour.
func repackAttemptDeadline(ctx context.Context, cfg config.Config, budget, attemptsLeft int) time.Duration {
	d := repackTimeout(cfg, budget)
	if attemptsLeft <= 0 {
		return d
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return d
	}
	if remaining := time.Until(dl); remaining > 0 {
		if share := remaining / time.Duration(attemptsLeft); share < d {
			d = share
		}
	}
	return d
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
// grammar route could not. client is repackStructured's hoisted chat-path
// client (register D-85/D-108) — seat-endpoint routing mirrored from the main
// client's construction (recordless.go): the pipeline's own client is pinned
// to the native completion path and cannot make this call.
//
// Returns the FAILURE itself (PR #366 correctness review, blocker: this used
// to report a bare ok=false on any failure — a transport failure, where the
// seat never answered, looked IDENTICAL to a validation failure, where it
// answered wrong — so when both grammar attempts had already failed on
// validation, repackStructured fell through to that STALE validation error
// instead of the chat fallback's own, real one). The caller classifies the
// returned error; repackViaChat makes no infrastructure/budget/abstention
// judgment of its own.
func (p *Pipeline) repackViaChat(ctx context.Context, client *llamaclient.Client, seat string, schema map[string]any, output string) (json.RawMessage, int, error) {
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
	budget := repackBudget(output)
	gres, gerr := client.Generate(ctx, seat, system, user, "", budget, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
	if gerr != nil {
		return nil, 0, gerr
	}
	if gres.Truncated {
		return nil, 0, fmt.Errorf("chat re-pack truncated at %d tokens (the answer is %d chars; the structured budget cannot hold it)", budget, len(output))
	}
	content := outerObject(gres.Content)
	if verr := validator.Validate([]byte(content), schema); verr != nil {
		fixed, ok := coerceToSchema([]byte(content), schema)
		if !ok {
			return nil, 0, verr
		}
		return json.RawMessage(fixed), gres.TokensOut, nil
	}
	return json.RawMessage(content), gres.TokensOut, nil
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
		// plus a Triton JIT on its first completion. The number lives in core
		// because the DELEGATOR must allow for this window too (register
		// D-116) and cannot import this package.
		return time.Duration(core.AgentAdmissionSecDefault) * time.Second
	}
	return time.Duration(sec) * time.Second
}

// cordonWait is the time a run spent at the cordon, counted only when it
// actually waited: an ungated pass takes nanoseconds, and stamping those as
// admission time reported a wait that never happened and shaved the warm-up's
// budget below its one-poll floor (CI, 2026-09-14 — the Windows clock had hidden it).
func cordonWait(since time.Time) time.Duration {
	if w := time.Since(since); w >= time.Millisecond {
		return w
	}
	return 0
}

// AdmissionBudget is admissionBudget for the other run launchers (the MCP
// agent_run door), so every door holds at the cordon for the same window.
func AdmissionBudget(sec int) time.Duration { return admissionBudget(sec) }

// WarmSeat is warmSeat for the other run launchers (the MCP agent_run door), so
// no door loads a cold seat inside its wall or probes its window before it is up.
func WarmSeat(ctx context.Context, endpoint, seat string, budget time.Duration) (time.Duration, string, bool) {
	return warmSeat(ctx, endpoint, seat, budget)
}

// AwaitSeatAdmission is awaitSeatAdmission for the other run launchers (the MCP
// agent_run door). Both doors drive the same loop on the same seat behind the
// same llama-swap, so both must wait out another session's swap the same way —
// and this one had no pre-flight at all (register S-25): an agent_run that
// arrived mid-swap spent its wall inside llama-swap's silent queue, which is the
// exact failure ADR 0032 removed from the delegation door in 2026-09-02.
func AwaitSeatAdmission(ctx context.Context, endpoint, seat string, budget time.Duration) (time.Duration, string) {
	return awaitSeatAdmission(ctx, endpoint, seat, budget)
}

// seatMatcher answers "is this GET /running row MY seat?" for a seat that may be
// bound by an ALIAS.
//
// llama-swap's /running names models by their CANONICAL id only, while the
// harness binds agent seats by alias on the reference deployment (`agent-pool` ->
// `qwen3.8-27b-vllm`, `offload-e4b` -> `gemma-4-e4b`). A reader that matches
// /running by the bound name therefore never finds its own seat: the admission
// pre-flight's "my seat is already ready" fast path was dead on every alias-bound
// box, and a READY seat slept the entire admission budget whenever any OTHER
// model happened to be mid-swap. 406 delegation-log rows carry the symptom in so
// many words — "/running lists the seat under another id". internal/seatload
// fixed exactly this for the drain (register C-11); this is the same resolution,
// through the same swapclient.Roster.Canonical, applied to the second reader.
//
// The roster read is LAZY and happens at most once: a caller asks for it only
// when the bare name matched nothing and the answer would otherwise be "wait", so
// a seat bound by its own id pays no extra round trip. A roster that cannot be
// read leaves the bare name in place — the pre-fix behaviour, never a refusal.
type seatMatcher struct {
	endpoint string
	names    []string
	// resolved latches on a SUCCESSFUL roster read only. Latching it on the
	// attempt disabled the alias match for the rest of the wait after ONE
	// transient error — and the moment the alias match matters is a box
	// contended enough for another model to be mid-swap, which is exactly the
	// moment that read times out or 500s. The seat then burned the whole
	// admission budget for a swap it had no stake in: S-08's own symptom,
	// intermittent and silent (blocker, review round 1).
	resolved bool
	// note is the FIRST resolution failure, carried to admission_note. A probe
	// that failed silently is the defect class this change exists to remove, so
	// this one does not get to be the exception.
	note string
}

func newSeatMatcher(endpoint, seat string) *seatMatcher {
	return &seatMatcher{endpoint: endpoint, names: []string{seat}}
}

// matches reports whether a /running row's model id is this seat under any name
// resolved so far.
func (m *seatMatcher) matches(id string) bool {
	for _, n := range m.names {
		if strings.EqualFold(id, n) {
			return true
		}
	}
	return false
}

// resolve reads the roster once and adds the canonical id this seat's name is an
// alias of. It reports whether it learned a NEW name — i.e. whether re-scanning
// rows already in hand can change the answer.
func (m *seatMatcher) resolve(ctx context.Context) bool {
	if m.resolved {
		return false
	}
	roster, err := swapclient.FetchRoster(ctx, m.endpoint, admissionPoll)
	if err != nil {
		// NOT latched: the caller polls again inside the same admission budget,
		// and the next read may well answer. Reported once — repeating it every
		// poll would bury the rest of admission_note.
		if m.note == "" {
			m.note = "seat alias resolution failed (proceeding with the bound name): " + err.Error()
			log.Printf("agent task: seat alias resolution (%s) failed (proceeding with the bound name): %v", m.names[0], err)
		}
		return false
	}
	m.resolved = true
	id, ok := roster.Canonical(m.names[0])
	if !ok || strings.EqualFold(id, m.names[0]) {
		return false
	}
	m.names = append(m.names, id)
	return true
}

// joinAdmissionNotes concatenates admission findings with "; ", dropping empties
// — the same shape the caller uses to fold the pre-flight's note into the
// warm-up's, so one admission_note can carry every step that had something to
// say without any of them overwriting another.
func joinAdmissionNotes(notes ...string) string {
	kept := notes[:0:0]
	for _, n := range notes {
		if strings.TrimSpace(n) != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, "; ")
}

// warmSeat loads an ABSENT seat outside the wall (D-64). One GET through
// llama-swap's per-model passthrough (`/upstream/<seat>/v1/models`, the same
// route ProbeServedWindow uses) makes llama-swap swap the seat in and answers
// only once its health check passes; the call is bounded by `budget` (the
// /running probes around it carry their own admissionPoll timeout each, so
// the wall-clock spent can exceed the budget by up to two poll intervals).
// Then /running is polled (two extra polls at most) until the seat reads ready.
// Returns the time spent, a note when residency could not be settled — a probe
// failure, a spent budget — so the wire says "the gate could not tell" rather
// than "nothing was loading", and whether a load was ATTEMPTED at all.
//
// That third answer cannot be derived from the first two. A sub-tick load
// measures 0 (Windows CI, and any warm passthrough), so `spent > 0` misses real
// loads; and since W-08 a note is also returned by exits that warmed NOTHING, so
// `note != ""` over-reports them. The coherence probe (D-118) keys on exactly
// "was this seat loaded for this run", and it now gets that fact directly.
// A seat that is already ready costs one /running probe and returns (0, "", false).
func warmSeat(ctx context.Context, endpoint, seat string, budget time.Duration) (time.Duration, string, bool) {
	if strings.TrimSpace(endpoint) == "" {
		return 0, "", false // no endpoint to warm against: the gate is off by construction
	}
	if budget < admissionPoll {
		if budget <= 0 {
			return 0, "", false // the admission gate is off (agent_admission_wait_sec −1)
		}
		// Admission spent the budget before the warm-up got a turn. Proceeding
		// into the wall is right; doing it SILENTLY is what left admission_note
		// empty on a seat that may still be cold, so the wire read "nothing was
		// loading" where the truth was "nobody looked" (register S-24).
		return 0, fmt.Sprintf("no admission budget left for the warm-up (%.0fs, under one poll interval): a cold seat will load inside the wall", budget.Seconds()), false
	}
	sc, err := swapclient.New(endpoint, admissionPoll)
	if err != nil {
		return 0, "warm-up has no swap client (proceeding): " + err.Error(), false
	}
	// The seat may be listed under its CANONICAL id while the contract names an
	// alias; seatMatcher resolves that, and only when the bare name missed.
	m := newSeatMatcher(endpoint, seat)
	ready := func() (bool, error) {
		rows, rerr := sc.Running(ctx)
		if rerr != nil {
			return false, rerr
		}
		for _, r := range rows {
			if m.matches(r.ID) && r.State == "ready" {
				return true, nil
			}
		}
		if m.resolve(ctx) {
			for _, r := range rows {
				if m.matches(r.ID) && r.State == "ready" {
					return true, nil
				}
			}
		}
		return false, nil
	}
	if ok, rerr := ready(); rerr != nil || ok {
		if rerr != nil {
			// "Could not read" is not "is ready", and it was reported as
			// neither. A seat whose residency is unknown proceeds into the wall
			// possibly cold, and the wire has to say so (register S-24).
			return 0, "warm-up could not read /running (proceeding; the seat may still be cold): " + rerr.Error(), false
		}
		return 0, m.note, false // already resident: nothing to warm; only an alias-probe failure, if any, to report
	}
	start := time.Now()
	b := swapclient.BaseURL(endpoint)
	if b == "" {
		return 0, joinAdmissionNotes(m.note, "warm-up could not resolve a llama-swap root from the endpoint (proceeding; the seat may still be cold)"), false
	}
	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// The warm-up is a deliberate LOAD, so it passes the GPU-lease fence first
	// (2026-09-22): a render or an exclusive hold taken after this run's cordon
	// must not get the seat loaded onto its cards. It waits for the card inside
	// the warm-up budget; still fenced at the end, it warms nothing, says so,
	// and the served-window probe — the next gated step — defers the run.
	wu, ferr := modelaffinity.AwaitUpstream(wctx, endpoint, seat, "/v1/models", start.Add(budget))
	if ferr != nil {
		if modelaffinity.IsLeaseRefusal(ferr) {
			return time.Since(start), joinAdmissionNotes(m.note, "warm-up held behind the GPU lease (nothing loaded onto the held card): "+ferr.Error()), false
		}
		return 0, joinAdmissionNotes(m.note, "warm-up could not build its request (proceeding; the seat may still be cold): "+ferr.Error()), false
	}
	req, rerr := http.NewRequestWithContext(wctx, http.MethodGet, wu, nil)
	if rerr != nil {
		return 0, joinAdmissionNotes(m.note, "warm-up request could not be built (proceeding; the seat may still be cold): "+rerr.Error()), false
	}
	resp, derr := warmClient.Do(req)
	if derr != nil {
		spent := time.Since(start)
		if wctx.Err() != nil && ctx.Err() == nil {
			return spent, joinAdmissionNotes(m.note, fmt.Sprintf("cold load exceeded the admission budget after %.0fs (proceeding into the wall)", spent.Seconds())), true
		}
		return spent, joinAdmissionNotes(m.note, "warm request failed (proceeding): "+derr.Error()), true
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	// The swap answered. A 200 IS the confirmation: llama-swap proxies only
	// after the upstream's health check passed. /running is read once more
	// for the note, but a seat listed under another id — the seat name is an
	// ALIAS on the reference boxes (`agent-pool` → `qwen3.8-27b-vllm`), and
	// /running carries the real id — is loaded all the same (0.115.17; the
	// 0.115.15 acceptance run reported "never listed" on a 187 s cold load
	// that had plainly succeeded).
	if resp.StatusCode == http.StatusOK {
		if ok, rerr := ready(); rerr == nil && ok {
			return time.Since(start), joinAdmissionNotes(m.note, fmt.Sprintf("cold load %.0fs outside the wall", time.Since(start).Seconds())), true
		}
		return time.Since(start), joinAdmissionNotes(m.note, fmt.Sprintf("cold load %.0fs outside the wall (passthrough answered 200; /running lists the seat under another id)", time.Since(start).Seconds())), true
	}
	// A non-200 passthrough answer (404: llama-swap does not know the name)
	// confirms nothing; two polls one interval apart, then say so and go.
	for i := 0; i < 2; i++ {
		if ok, rerr := ready(); rerr == nil && ok {
			return time.Since(start), joinAdmissionNotes(m.note, fmt.Sprintf("cold load %.0fs outside the wall", time.Since(start).Seconds())), true
		}
		// The second poll waits one interval, but only inside what is left of
		// the budget — the confirmation must not outspend the gate it serves.
		if i == 0 && time.Since(start)+admissionPoll <= budget {
			if serr := seatwait.Sleep(ctx, admissionPoll); serr != nil {
				return time.Since(start), joinAdmissionNotes(m.note, serr.Error()), true
			}
		} else if i == 0 {
			break
		}
	}
	return time.Since(start), joinAdmissionNotes(m.note, fmt.Sprintf("warm request answered HTTP %d after %.0fs but /running never listed %s ready (proceeding)", resp.StatusCode, time.Since(start).Seconds(), seat)), true
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
	// The seat's own /running row may be listed under the CANONICAL id while the
	// contract names an alias; seatMatcher resolves that, lazily.
	m := newSeatMatcher(endpoint, seat)
	// waited counts the SLEEPS and the alias-resolution round-trips that
	// precede them, never the /running probe itself: an immediate admission
	// reports zero, which is what "nothing was swapping" must read as on the
	// wire. The resolution is charged because it only runs when a sleep would
	// follow, and a roster that TIMES OUT (3 s, admissionPoll) on every poll
	// would otherwise let this loop run twice its budget in wall-clock time
	// while reporting the budget exactly (reviewer finding on the S-08 retry).
	var waited time.Duration
	for {
		rows, rerr := sc.Running(ctx)
		if rerr != nil {
			return waited, "running probe failed (proceeding): " + rerr.Error()
		}
		mine, busy := false, ""
		for _, r := range rows {
			if m.matches(r.ID) && r.State == "ready" {
				mine = true
			}
			if r.State != "ready" {
				busy = r.ID + ":" + r.State
			}
		}
		// Resolve the alias only when it would change the verdict — the seat's
		// own row was not found under its bound name AND something else is
		// mid-swap, i.e. the next step would be a sleep. This is the fast path
		// that was dead: a READY alias-bound seat waited the whole budget for
		// another model's swap it had no stake in (register S-08).
		if !mine && busy != "" {
			resolveStart := time.Now()
			resolved := m.resolve(ctx)
			if !resolved && m.note != "" {
				// A FAILED resolution is charged (a roster that times out costs
				// admissionPoll per poll); a successful one is part of an
				// immediate admission and stays at the zero the wire promises.
				waited += time.Since(resolveStart)
			}
			if resolved {
				for _, r := range rows {
					if m.matches(r.ID) && r.State == "ready" {
						mine = true
					}
				}
			}
		}
		// The seat this contract needs is up: another model's swap is not this
		// contract's business, and waiting it out is wall spent on somebody
		// else's load. Every exit below carries the matcher's note, so an alias
		// probe that failed is reported whatever verdict this poll reaches.
		if mine || busy == "" {
			return waited, m.note
		}
		if waited+admissionPoll > budget {
			return waited, joinAdmissionNotes(m.note, "budget spent while "+busy+" (proceeding into the wall)")
		}
		if serr := seatwait.Sleep(ctx, admissionPoll); serr != nil {
			return waited, joinAdmissionNotes(m.note, serr.Error())
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
// seatBusyExhausted reports a 429 that came back AFTER the contract's
// busy-seat budget (seatwait) was spent: the seat is alive but not reachable
// for this contract any more, and no further re-pack attempt can change that.
// Until 0.130.x the wall cut this wait and the contention arm filed it; under
// liveness (0.131.0) the budget decides, and the 429 is the LAST attempt by
// construction so the verdict stays the transport one.
func seatBusyExhausted(err error) bool {
	var se *llamaclient.StatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusTooManyRequests
}

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

// selfCutoff reports whether err is a re-pack attempt's OWN per-attempt bound
// (repackAttemptDeadline, register D-108) cutting the request short — never
// evidence the seat is unreachable, the same distinction genErrIsTransport
// already draws for a canceled parent (PR #366 correctness review, S-22/W-16
// carried one arm further): a client.Timeout firing on a request THIS BOX
// deliberately narrowed looks identical, on the wire, to a real ctx deadline
// firing — both surface as a *url.Error whose Timeout() is true, or wrap
// context.DeadlineExceeded — while a dial-refused or connection-reset error
// has NEITHER shape. A TIMEOUT is always "we gave up waiting" (our own
// patience, a budget decision), never direct proof the box is broken; only a
// definitive refusal (a 5xx, a reset, a dial failure) is that.
func selfCutoff(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Timeout()
	}
	return false
}

// repackCutoffErr marks a re-pack error whose decisive (LAST-run) attempt was
// ended by the re-pack's own per-attempt bound rather than by the seat or a
// canceled caller (register D-108, PR #366 correctness review). Filing this
// as infrastructure — "the seat could not be reached" — recreates, one arm
// over, the exact defect this PR already fixes for a canceled parent context
// (S-22/W-16): the box did nothing wrong, this box's own narrowed clock ran
// out. The caller reads it with errors.As and defers budget.
type repackCutoffErr struct {
	msg string
	raw error
}

func (c *repackCutoffErr) Error() string { return c.msg }
func (c *repackCutoffErr) Unwrap() error { return c.raw }

// asRepackCutoff reports whether err (or anything it wraps) is a
// *repackCutoffErr, and returns it.
func asRepackCutoff(err error) (*repackCutoffErr, bool) {
	var c *repackCutoffErr
	ok := errors.As(err, &c)
	return c, ok
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
			ForcedFinal: c.ForcedFinal, Ms: c.Ms, Sampling: c.Sampling,
		})
	}
	return out
}

// rateCalls projects the loop's call records onto what seatrate.Rate reads.
func rateCalls(calls []agent.CallRecord) []seatrate.Call {
	out := make([]seatrate.Call, 0, len(calls))
	for _, c := range calls {
		out = append(out, seatrate.Call{CompletionTokens: c.CompletionTokens, Ms: c.Ms})
	}
	return out
}

// seatRates opens the per-seat rate store under the machine-wide state root
// (the GPU-lease root: machine-local by design — a seat's rate is a fact about
// this box's cards). Unresolvable root or a corrupt file = an empty store that
// cannot save, logged once here, never a failed run.
func (p *Pipeline) seatRates() *seatrate.Store {
	root, err := gpulease.ResolveStateRoot(p.cfg.StateDir)
	if err != nil {
		log.Printf("agent task: seat-rates store unavailable: %v", err)
		p.seatRatesPath = ""
		return &seatrate.Store{}
	}
	p.seatRatesPath = seatrate.Path(root)
	s, lerr := seatrate.Load(p.seatRatesPath)
	if lerr != nil {
		log.Printf("agent task: %v", lerr)
	}
	return s
}

// finalBudgetsFor is the contract's final-answer budget on this box and the
// re-pack term that rides with it. ONE rule, read by the wall estimate, the
// final-budget fit (D-95) and the list-cap re-issue's min_turn floor, so those
// three can never size the same run from three different numbers.
//
// The loop opens the final-budget turn only on the LAST of ≥ 2 steps (loop.go:
// maxSteps >= 2 && step == maxSteps-1); a one-step contract's single completion
// runs at the plain step budget. A contract with an output_schema may pay one
// more completion for the structured re-pack when the final answer is prose
// (register D-46 follow-up: a 285 s re-pack on the 27B sat outside every floor
// and estimate) — charged at the final budget as an upper bound, which a seat
// answering in the object shape never pays.
func finalBudgetsFor(cfg config.Config, contract core.AgentContract) (final, repack int) {
	// ONE copy of the rule (seatrate.FinalBudgets, register D-116): the
	// DELEGATOR now sizes its poll clock from the same function, against what
	// this node advertises on health, so the two can never disagree about what
	// the final answer and its re-pack cost.
	return seatrate.FinalBudgets(cfg.AgentMaxTokens, contract.MaxSteps, len(contract.OutputSchema) > 0)
}

// autoWallFor sizes the wall of a timeout_auto contract on this seat (register
// D-03): the seat-rate estimate — cold load, think block, tool steps, final
// answer and re-pack at the seat's remembered rate — clamped to the wire bounds
// [AgentTimeoutSecDefault, AgentTimeoutSecCap]. The cold load stays inside the
// number although the warm-up runs it outside the wall (D-64): a wall is a
// ceiling, not a spend, and the over-provision is at most the admission
// budget's worth. Returns 0 when the seat has no rate yet — the caller keeps
// the wire default — and always a note naming the arithmetic or its absence.
func AutoWallFor(cfg config.Config, contract core.AgentContract, seat string, known seatrate.Seat) (int, string) {
	wall, est := seatrate.AutoWallFor(SeatPolicyFor(cfg, seat, known), contract)
	if wall <= 0 {
		return 0, fmt.Sprintf("auto wall for %s: no rate yet, running at the wire default %d s (%s)", seat, core.AgentTimeoutSecDefault, est.Note)
	}
	return wall, fmt.Sprintf("auto wall for %s: %d s (estimate %d s, bounds %d..%d)", seat, wall, est.TotalSec, core.AgentTimeoutSecDefault, core.AgentTimeoutSecCap)
}

// SeatPolicyFor is THIS box's seat policy for the agent seat: the loop's step
// budget and thinking policy from config, the measured rate from the
// machine-local seat-rates store (else the configured agent_seat_tok_s).
// Exported beside AutoWallFor so the delegator's anti-drift test can size the
// same contract on the node's arithmetic and on its own (register D-116).
func SeatPolicyFor(cfg config.Config, seat string, known seatrate.Seat) seatrate.SeatPolicy {
	p := seatrate.SeatPolicy{Seat: seat, StepTokens: cfg.AgentMaxTokens, Thinking: cfg.AgentThinking, ColdLoadSec: known.ColdLoadSec}
	switch {
	case known.TokS > 0:
		p.TokS, p.RateSamples, p.RateSource = known.TokS, known.Samples, "store"
	case cfg.AgentSeatTokS > 0:
		p.TokS, p.RateSource = cfg.AgentSeatTokS, "config agent_seat_tok_s"
	}
	return p
}

// wallEstimateFor sizes one contract on one seat (register D-03): the seat's
// remembered rate (else the box's agent_seat_tok_s), the slower of the
// remembered and the just-observed cold load, the loop's step and final
// budgets, and whether the run thinks (one starved think block is what `auto`
// cost on both fleet seats, 2026-09-10).
func wallEstimateFor(cfg config.Config, contract core.AgentContract, seat string, known seatrate.Seat, coldLoadSec float64, timeoutSec int) seatrate.Estimate {
	in := seatrate.InputFor(SeatPolicyFor(cfg, seat, known), contract)
	in.TimeoutSec = timeoutSec
	if coldLoadSec > in.ColdLoadSec {
		in.ColdLoadSec = coldLoadSec
	}
	return seatrate.Compute(in)
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
