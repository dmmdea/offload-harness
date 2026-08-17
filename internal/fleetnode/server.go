// This file is the HTTP face of the fleet node — the three CONTRACT.md v2
// endpoints (health / dispatch / jobs) over the stores built by the sibling
// files. Handlers hold no GPU lock and spawn nothing: health reads the
// sampler's atomic snapshot + the jobs store's counters, dispatch acks in
// milliseconds and hands the render to Jobs.Accept, and every failure is a
// non-2xx JSON envelope from the spec's error taxonomy. Wrong Content-Type is
// answered 400 (not 415): the taxonomy has exactly one caller-mistake class,
// and one shape beats two.

package fleetnode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// maxDispatchBody is the dispatch request-body cap (1 MiB per the spec — a
// run-graph payload is the biggest legitimate body and fits comfortably).
const maxDispatchBody = 1 << 20

// maxSnapshotAge bounds how stale a VRAM snapshot health may serve. The
// sampler refreshes every 2s and keeps the LAST GOOD snapshot on failure — so
// if nvidia-smi starts failing outright (driver reset), health would otherwise
// serve hours-stale 200s and mislead the dispatcher's routing. Past this bound
// a stale snapshot is the same class of failure as no snapshot: 503.
const maxSnapshotAge = 30 * time.Second

// agentResidencyTTL is how long one roster answer backs agent_seat_resident
// before the next health request triggers a background refresh. Set to the
// SAME bound as maxSnapshotAge on purpose: health already promises its cached
// data (the VRAM snapshot) is no staler than that, and the roster answer
// rides the same freshness contract rather than inventing a second cadence.
const agentResidencyTTL = maxSnapshotAge

// agentResidencyProbeTimeout bounds ONE background roster GET. Deliberately
// shorter than swapclient.DefaultTimeout: the probe informs a cached health
// field, and against a dead llama-swap a long-hanging refresh would just
// delay the (negative) answer the next TTL window serves anyway.
const agentResidencyProbeTimeout = 5 * time.Second

// Runner runs one translated request to completion. The real pipeline
// satisfies it; tests inject fakes.
type Runner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// Options is the server's static identity + read-only data feeds. Snapshot and
// Footprints are functions (not values) so health always reads live state
// without the handler owning any sampling machinery.
type Options struct {
	NodeID     string
	Snapshot   func() (Snapshot, bool)
	Footprints func() []FootprintEntry
	// Version is advertised as harness_version. Empty = omitted.
	Version string
	// Reclaim reports how much VRAM this node could free right now. nil (or an
	// unknown verdict) omits the reclaim fields entirely rather than guessing.
	Reclaim func(freeGiB, totalGiB float64) ReclaimVerdict
	GpuVendor  string
	GpuArch    string
	// LoopbackListener reports whether the serve listener is bound to a
	// loopback address. The verb computes it from the RESOLVED listen address
	// (netguard.LoopbackAddr) — NOT from --listen-trusted-network, which is
	// permission to bind beyond loopback, not where the bind actually landed.
	// The agent lane keys on it: with no Cfg.FleetAuthToken configured, agent
	// dispatches on a non-loopback listener are refused 403 at ack time. The
	// zero value (false) fails CLOSED for the agent lane — a constructor that
	// forgets to set it gets the refusal, never an open agent surface; the
	// media lane never consults it.
	LoopbackListener bool
	Cfg              config.Config
}

// Server is the fleet-node HTTP server: three handlers over a Runner + Jobs
// store. Task/family advertisements are derived once at construction (config
// is immutable while serving).
type Server struct {
	runner   Runner
	jobs     *Jobs
	opts     Options
	tasks    []string
	families []string
	// agentSeat is the resolved agent planner seat (config.AgentPlannerModel:
	// agent_model > workhorse Model), computed once here like tasks/families —
	// the config cannot change under a running server. Advertised (and probed
	// for residency) only when agentLane holds.
	agentSeat string
	// agentLane is fleetnode.AgentLaneAdmissible over the RESOLVED listener —
	// the one predicate dispatch's ack-time guard and this advertisement share,
	// computed once here like tasks/families. Health must never advertise a
	// lane dispatch would refuse: the delegator reads ONLY the agent_* fields,
	// so a lane advertised past a tokenless non-loopback listener sends it
	// through placement and straight into a 403.
	agentLane bool
	// rosterServes answers "does the llama-swap behind endpoint serve seat?"
	// (alias-aware). A seam so tests drive the residency cache without a live
	// llama-swap; production always gets swapRosterServes.
	rosterServes func(ctx context.Context, endpoint, seat string) (bool, error)
	agentRes     agentResidency
}

// agentResidency caches the roster's answer for the agent seat between health
// requests. The cache exists because BOTH of these hold at once:
//   - agent_seat_resident must be roster-verified (§S3) — config alone cannot
//     know whether the seat is actually served;
//   - the health handler must NEVER block on an HTTP call to llama-swap (the
//     same rule the reclaim tracker documents — fleet_reclaim.go), and must
//     not probe per request either.
//
// So a health request serves the cached answer and, when it is older than
// agentResidencyTTL, triggers ONE background refresh (inflight = the
// single-flight latch). Until the first probe lands the answer is false —
// fail CLOSED: an unverified seat reads as not resident, and the delegator
// keeps the work local, which is always the safe placement.
type agentResidency struct {
	mu       sync.Mutex
	resident bool
	at       time.Time
	inflight bool
	// idle broadcasts when inflight clears, so a caller that needs the answer
	// SYNCHRONOUSLY (RefreshAgentResidency) can wait for the probe already
	// running instead of starting a second one. Created lazily under mu — a
	// sync.Cond needs its Locker, and the zero agentResidency must stay usable.
	idle *sync.Cond
}

// idleCond returns the broadcast channel, creating it on first use. Caller
// holds mu.
func (a *agentResidency) idleCond() *sync.Cond {
	if a.idle == nil {
		a.idle = sync.NewCond(&a.mu)
	}
	return a.idle
}

// claimProbe takes the single-flight latch, reporting whether THIS caller now
// owns the probe (and must therefore clear the latch when it finishes).
func (a *agentResidency) claimProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inflight {
		return false
	}
	a.inflight = true
	return true
}

// awaitProbe blocks until whoever owns the latch has published its answer.
// Bounded by the probe's own agentResidencyProbeTimeout, so this cannot hang
// on a dead llama-swap.
func (a *agentResidency) awaitProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.inflight {
		a.idleCond().Wait()
	}
}

// New builds a Server. The supported-task/family lists are computed here, not
// per health request — the config cannot change under a running server.
func New(runner Runner, jobs *Jobs, opts Options) *Server {
	return &Server{
		runner:       runner,
		jobs:         jobs,
		opts:         opts,
		tasks:        SupportedTasksFor(opts.Cfg, opts.LoopbackListener),
		families:     Families(opts.Cfg),
		agentSeat:    opts.Cfg.AgentPlannerModel(""),
		agentLane:    AgentLaneAdmissible(opts.Cfg, opts.LoopbackListener),
		rosterServes: swapRosterServes,
	}
}

// swapRosterServes is the production residency probe: one GET /v1/models via
// the shared swapclient adapter, answered alias-aware (Roster.Serves matches
// canonical ids AND meta.llamaswap.aliases — the plannerUnserved lesson: every
// harness-bound seat name on the reference deployment is an alias, so an
// id-only match would report a correctly-served seat missing forever).
func swapRosterServes(ctx context.Context, endpoint, seat string) (bool, error) {
	roster, err := swapclient.FetchRoster(ctx, endpoint, agentResidencyProbeTimeout)
	if err != nil {
		return false, err
	}
	return roster.Serves(seat), nil
}

// agentResident returns the cached residency answer, kicking off a background
// refresh when the cache has aged past agentResidencyTTL. It never blocks and
// never probes more than once per TTL window regardless of request rate.
func (s *Server) agentResident() bool {
	a := &s.agentRes
	a.mu.Lock()
	fresh := !a.at.IsZero() && time.Since(a.at) <= agentResidencyTTL
	if fresh || a.inflight {
		v := a.resident
		a.mu.Unlock()
		return v
	}
	a.inflight = true
	v := a.resident // serve the previous answer while the refresh runs
	a.mu.Unlock()
	go s.refreshAgentResidency()
	return v
}

// refreshAgentResidency runs one bounded roster probe and publishes the
// answer. THE CALLER MUST ALREADY HOLD the single-flight latch
// (agentResidency.claimProbe, or the inline claim in agentResident) — this
// function clears it unconditionally on the way out, so a caller that ran
// without holding it would release someone else's claim and let a duplicate
// probe race the first one's answer.
//
// A probe FAILURE publishes resident=false rather than keeping the
// last good answer (the VRAM sampler's rule): residency feeds a placement
// gate, and advertising a seat as servable off a stale success while
// llama-swap is down would route remote agent work at a node that cannot run
// it — false only costs a conservative local placement. The failure is still
// cached for a full TTL so a dead endpoint is probed once per window, not
// hammered per request.
func (s *Server) refreshAgentResidency() {
	var resident bool
	var err error
	// The publish is DEFERRED, not tail-appended: the probe below runs outside
	// any lock, and clearing the latch only on the normal path meant a panic
	// anywhere in that seam froze residency for the life of the process — never
	// refreshable again — and parked every awaitProbe waiter forever, since
	// RefreshAgentResidency has no timeout of its own. On a panic the zero
	// values here publish the fail-closed answer (resident=false), which is the
	// same verdict a failed probe gets, and the panic still propagates.
	defer func() {
		a := &s.agentRes
		a.mu.Lock()
		a.resident = resident && err == nil
		a.at = time.Now()
		a.inflight = false
		a.idleCond().Broadcast() // release any synchronous waiter (RefreshAgentResidency)
		a.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), agentResidencyProbeTimeout)
	defer cancel()
	resident, err = s.rosterServes(ctx, s.opts.Cfg.Endpoint, s.agentSeat)
	if err != nil {
		// agent_seat_resident:false is the one field that stops EVERY remote
		// placement at this node, and the probe error was the only evidence of
		// why. Without this line an operator sees a node advertising itself as
		// unusable and has nothing, on either side of the wire, to look at. The
		// probe runs at most once per TTL window, so this cannot spam.
		log.Printf("fleet: agent seat %q residency probe against %s failed; advertising agent_seat_resident:false for up to %s: %v",
			s.agentSeat, s.opts.Cfg.Endpoint, agentResidencyTTL, err)
	}
}

// RefreshAgentResidency ensures ONE residency probe has published an answer
// before it returns, warming the cache agent_seat_resident is served from.
//
// It PARTICIPATES in the single-flight rather than bypassing it: it claims the
// latch when nothing holds it, and otherwise WAITS for the probe already
// running. Calling the refresher directly (the previous shape) took no latch
// while the refresher cleared it unconditionally, which permitted a second
// concurrent GET against llama-swap and — since the last writer wins the cache
// — let a staler answer overwrite a fresher one.
//
// Exported for CROSS-PACKAGE callers that need the answer deterministically —
// an end-to-end delegation test cannot otherwise avoid racing the first health
// GET's background refresh, and would gate-fail on a cold cache that is about
// to say yes. This package's OWN residency tests must never call it:
// production's only trigger is agentResident()'s background single-flight, and
// a test that runs the probe itself leaves that line uncovered (which is
// exactly how deleting it once left the whole suite green while the feature was
// inert in production). The one in-package exception is the test that pins THIS
// function's latch discipline, which cannot be written any other way.
func (s *Server) RefreshAgentResidency() {
	if !s.agentRes.claimProbe() {
		s.agentRes.awaitProbe()
		return
	}
	s.refreshAgentResidency()
}

// Handler returns the routed mux for the three contract endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", s.handleHealth)
	mux.HandleFunc("POST /fleet/dispatch", s.handleDispatch)
	mux.HandleFunc("GET /fleet/jobs/{id}", s.handleJob)
	// {filename...} (not {filename}) is deliberate: it's a multi-segment
	// wildcard, so a caller-supplied "/" or a %2F-hidden one still reaches the
	// handler as PART OF filename instead of 404ing at the mux before our
	// validation ever runs — every malformed name gets this file's one 400
	// JSON shape, not the mux's bare-text 404.
	mux.HandleFunc("GET /fleet/media/{filename...}", s.handleMedia)
	return mux
}

// httpServer builds the *http.Server with the spec's timeout table
// (ReadHeader 5s / Read 30s / Write 30s / Idle 120s). Split from Serve so the
// timeouts are unit-assertable.
func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Serve serves on l until it fails/closes, with the contract timeout table
// applied. The listener is the verb's business (netguard.Validate happens
// there, before the socket ever exists).
func (s *Server) Serve(l net.Listener) error {
	return s.httpServer().Serve(l)
}

// healthPayload is CONTRACT.md v2's health shape, field for field.
type healthPayload struct {
	NodeID                string           `json:"node_id"`
	SchemaVersion         int              `json:"schema_version"`
	GpuVendor             string           `json:"gpu_vendor"`
	GpuArch               string           `json:"gpu_arch"`
	VramTotalGb           float64          `json:"vram_total_gb"`
	VramFreeGb            float64          `json:"vram_free_gb"`
	// GpuDevices is the full per-device breakdown behind VramTotalGb/
	// VramFreeGb's headline pick (nvidia-smi multi-device source only — see
	// HeadlineDevice for the largest-total-wins rule those two fields apply).
	// Additive: VramTotalGb/VramFreeGb keep their existing meaning for every
	// node unchanged. Omitted entirely (not an empty array) when the snapshot
	// source can't enumerate devices, so "no gpu_devices key" unambiguously
	// means "single-device source" — the exact shape every pre-fix node and
	// consumer already expects.
	GpuDevices []GPUDevice `json:"gpu_devices,omitempty"`
	SupportedTaskTypes    []string         `json:"supported_task_types"`
	LoadableModelFamilies []string         `json:"loadable_model_families"`
	ModelFootprints       []FootprintEntry `json:"model_footprints"`
	QueueDepth            int              `json:"queue_depth"`
	// HarnessVersion closes the node/repo drift gap: fleet drift used to be found
	// by hand, and a node several releases behind is debugged against known-fixed
	// bugs.
	HarnessVersion string `json:"harness_version,omitempty"`
	// VramReclaimableGb is how much VRAM this node can FREE by unloading its own
	// models — the number a scheduler actually needs. vram_free_gb under-counts a
	// warm node; vram_total_gb over-counts every node whose card is shared (the
	// measured workstation's desktop holds ~6.5 GiB that cannot be reclaimed at any
	// price). Both fields are OMITTED when unmeasured, so a consumer can tell
	// "nothing to reclaim" from "not known yet" and fall back to free VRAM.
	VramReclaimableGb *float64 `json:"vram_reclaimable_gb,omitempty"`
	// VramSchedulableGb is free + reclaimable: the headroom a job can actually get.
	VramSchedulableGb *float64 `json:"vram_schedulable_gb,omitempty"`
	// VramReclaimSource states HOW the number was reached, so a measured zero is
	// never mistaken for an unknown.
	VramReclaimSource string `json:"vram_reclaim_source,omitempty"`
	// ---- Agent lane advertisement (§S3, multi-node delegation) ----
	// All four fields are ADDITIVE and omitempty, so schema_version stays 1:
	// a node with the lane off — which includes every pre-0.63 node — emits a
	// byte-identical payload (pinned in TestHealthAgentFieldsAbsentWhenDisabled),
	// and the dispatcher decodes health loosely per FLEET-NODE.md, so old and
	// new nodes interoperate in both directions. Populated ONLY when
	// AgentLaneAdmissible says the lane is usable: the operator opted the node
	// in (fleet_agent_enabled — every tier binds an agent seat, so keying off
	// the seat's mere presence would advertise the lane fleet-wide overnight),
	// a seat resolves, AND the listener posture is one dispatch will accept.
	// These four fields are the ONLY ones a delegator reads, so an
	// advertisement dispatch would refuse is a mis-route, not a near miss.
	AgentSeat string `json:"agent_seat,omitempty"` // resolved planner seat (config.AgentPlannerModel)
	// AgentCtxTokens is the seat's serving ceiling from config (the tier's
	// agent_ctx_tokens) — the delegator's placement gate does its ctx
	// arithmetic against this. 0 = omitted = "ceiling unknown", which the gate
	// reads as never-fits.
	AgentCtxTokens int `json:"agent_ctx_tokens,omitempty"`
	// AgentResident is the CACHED roster verification (agentResidency): true
	// only when a probe inside the TTL window saw the seat in the llama-swap
	// roster. False/omitted until the first probe lands, and on probe failure.
	AgentResident bool `json:"agent_seat_resident,omitempty"`
	AgentEnabled  bool `json:"agent_enabled,omitempty"`
}

// handleHealth assembles the contract health JSON from cached/cheap reads
// only. A missing snapshot (sampler never succeeded) is a FAILED probe → 503:
// emitting zeros would advertise a broken node as an empty GPU.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.opts.Snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "vram snapshot unavailable")
		return
	}
	snap, ok := s.opts.Snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "vram snapshot unavailable")
		return
	}
	if time.Since(snap.At) > maxSnapshotAge {
		writeError(w, http.StatusServiceUnavailable, "vram snapshot stale")
		return
	}
	fps := []FootprintEntry{}
	if s.opts.Footprints != nil {
		if e := s.opts.Footprints(); e != nil {
			fps = e
		}
	}
	tasks := s.tasks
	if tasks == nil {
		tasks = []string{}
	}
	families := s.families
	if families == nil {
		families = []string{}
	}
	payload := healthPayload{
		NodeID:                s.opts.NodeID,
		SchemaVersion:         1,
		GpuVendor:             s.opts.GpuVendor,
		GpuArch:               s.opts.GpuArch,
		VramTotalGb:           snap.TotalGiB,
		VramFreeGb:            snap.FreeGiB,
		GpuDevices:            snap.Devices,
		SupportedTaskTypes:    tasks,
		LoadableModelFamilies: families,
		ModelFootprints:       fps,
		QueueDepth:            s.jobs.QueueDepth(),
		HarnessVersion:        s.opts.Version,
	}
	// Reclaim is advertised ONLY when measured. An unknown verdict omits both
	// numbers so a consumer falls back to free VRAM instead of acting on a guess;
	// the source string is published either way, since "why is it absent?" is the
	// first question anyone reading this payload will have.
	if s.opts.Reclaim != nil {
		v := s.opts.Reclaim(snap.FreeGiB, snap.TotalGiB)
		payload.VramReclaimSource = v.Source
		if v.Known {
			reclaimable := v.ReclaimableGiB
			schedulable := snap.FreeGiB + reclaimable
			payload.VramReclaimableGb = &reclaimable
			payload.VramSchedulableGb = &schedulable
		}
	}
	// Agent lane (§S3): advertised only when the lane is actually ADMISSIBLE —
	// the same AgentLaneAdmissible predicate the ack-time guard uses, over the
	// same resolved listener, so health can never promise a lane dispatch will
	// 403. agentResident() is a cached read + at most one background refresh
	// per TTL; it never blocks this handler on llama-swap.
	if s.agentLane {
		payload.AgentEnabled = true
		payload.AgentSeat = s.agentSeat
		payload.AgentCtxTokens = s.opts.Cfg.AgentCtxTokens
		payload.AgentResident = s.agentResident()
	}
	writeJSON(w, http.StatusOK, payload)
}

// dispatchEnvelope is the strict dispatch wire shape. DisallowUnknownFields
// applies to THIS struct only — payload passes through raw for the per-task
// translators. model_family/quant and the sizing fields are contract-reserved
// scheduler inputs: accepted and ignored here (footprint keys come from the
// machine's own bindings, and admission math is the dispatcher's job).
type dispatchEnvelope struct {
	JobID       string          `json:"job_id"`
	TaskType    string          `json:"task_type"`
	ModelFamily string          `json:"model_family"`
	Quant       string          `json:"quant"`
	Priority    json.RawMessage `json:"priority"`
	Width       json.RawMessage `json:"width"`
	Height      json.RawMessage `json:"height"`
	NumFrames   json.RawMessage `json:"num_frames"`
	ParamsB     json.RawMessage `json:"params_b"`
	ContextLen  json.RawMessage `json:"context_len"`
	NumLayers   json.RawMessage `json:"num_layers"`
	HiddenDim   json.RawMessage `json:"hidden_dim"`
	BatchSize   json.RawMessage `json:"batch_size"`
	Payload     json.RawMessage `json:"payload"`
}

// handleDispatch is the ack path: validate everything a caller can get wrong
// (the 400s), resolve a KNOWN job_id first (re-ack/409 — see below; this
// runs regardless of drain state, since a job this node already owns is
// never new work), refuse UNKNOWN work during drain (503), then Accept —
// which either starts the run (202 exact echo) or reveals a duplicate (202
// re-ack for anything not failed, 409 for a previously failed job; never a
// second render).
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDispatchBody)

	// Soft Content-Type check: a set-but-wrong type is a caller mistake; an
	// absent header is tolerated (curl-friendly, and the body decode is the
	// real gate).
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var env dispatchEnvelope
	if err := dec.Decode(&env); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest, "request body too large (limit 1 MiB)")
			return
		}
		writeError(w, http.StatusBadRequest, "malformed dispatch body: "+err.Error())
		return
	}

	// AGENT-LANE AUTH (v1 scope: the agent lane ONLY — the delegation plan's
	// reshape delta 10; media task types skip this block entirely so deployed
	// tokenless media clients stay byte-identical, pinned in auth_test.go).
	// Ordering: the check needs the DECODED task_type, so it cannot precede
	// the body decode; it sits immediately AFTER decode and BEFORE everything
	// else — the job_id check, the known-job re-ack/409 lookup, the drain
	// gate, BuildRequest — so an unauthorized agent caller gets only the auth
	// verdict (401/403), never a validation 400 to probe the envelope with
	// and never a re-ack/409 that discloses job existence or state.
	if env.TaskType == string(core.TaskAgentRun) {
		if s.opts.Cfg.FleetAuthToken == "" {
			// The reachability condition is CONSULTED, never re-derived:
			// AgentLaneSafelyReachable is the same expression AgentLaneAdmissible
			// applies to the same resolved listener, so this refusal can never
			// disagree with what health advertised. (With no token configured it
			// reduces to "is the listener loopback?" — a tokenless lane beyond
			// loopback drives a coding-agent loop over an open port, an RCE-class
			// surface. Misconfiguration must fail here, visibly, at ack time.)
			if !AgentLaneSafelyReachable(s.opts.Cfg, s.opts.LoopbackListener) {
				writeError(w, http.StatusForbidden, agentLaneTokenRequired)
				return
			}
			// Loopback + no token: the agent lane is reachable only from this
			// box — the same trust boundary as the local MCP surface.
		} else if !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	if env.JobID == "" {
		writeError(w, http.StatusBadRequest, "job_id required")
		return
	}

	// Idempotent re-ack BEFORE BuildRequest AND before the Draining() check
	// (Fix: SP3 follow-up review — this ordering predates the pipeline-job
	// work, which added this lookup but left it AFTER Draining(), so a
	// drain-in-progress flat-503'd a re-dispatch of a job_id this node
	// ALREADY OWNED, even one it had already finished. The dispatcher's
	// documented contract treats ANY non-202 dispatch response as an outright
	// REFUSAL and may re-dispatch the same job_id to another node — so that
	// flat 503 could buy a duplicate render fleet-wide for work this node had
	// already done. A known job_id is NOT new work, so it is never subject to
	// the drain refusal below, whatever state it's in:
	//   - JobError (previously failed here)            → 409, drain or not.
	//   - accepted/running/done (this node owns it)     → 202 re-ack, drain or not.
	// Only a job_id this node has NEVER SEEN reaches the Draining() gate.
	//
	// This lookup ALSO must run before BuildRequest for the pipeline-job
	// family specifically: a lost-ack re-dispatch of a job_id this node
	// already knows about must re-ack without redoing ANY build work (ref
	// fetches, materialized temp files) — for every family that's pure
	// waste, and for a pipeline-job it is actively WRONG: BuildRequest's
	// exclusive job-dir create (buildPipelineJob, the job_spec.id collision
	// guard) cannot tell "the same job_id re-dispatched while still running"
	// from "a genuinely different job_id whose job_spec.id collides", and
	// would 400 the former — again an outright REFUSAL per the dispatcher's
	// contract, again inviting a duplicate render elsewhere. Checking the
	// SAME job_id here, before BuildRequest ever runs, makes that distinction
	// correctly: a known job_id short-circuits straight to the same
	// re-ack/409 logic Accept's own duplicate path already uses; only a
	// job_id this node has never seen reaches BuildRequest, so a real
	// job_spec.id collision (a DIFFERENT job_id) still hits the Mkdir guard
	// and 400s as intended.
	if view, ok := s.jobs.Get(env.JobID); ok {
		if view.State == JobError {
			writeError(w, http.StatusConflict, "job previously failed on this node: "+view.Error)
			return
		}
		writeAck(w, env.JobID) // accepted/running/done: idempotent re-ack, even mid-drain — no rebuild, no render
		return
	}

	if s.jobs.Draining() {
		writeError(w, http.StatusServiceUnavailable, "node draining")
		return
	}

	// The RESOLVED listener goes down the admission path — the same value
	// s.tasks/s.agentLane were computed from at New(), so what health
	// advertises and what this admits can never key on different notions of
	// "loopback".
	req, cleanup, err := BuildRequest(r.Context(), s.opts.Cfg, s.opts.LoopbackListener, env.TaskType, env.Payload)
	if err != nil {
		cleanup()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	run := func(ctx context.Context) (json.RawMessage, error) {
		defer cleanup() // temp files live exactly as long as the job
		res := s.runner.Run(ctx, req)
		if res.OK {
			return res.Data, nil
		}
		reason := res.Reason
		if reason == "" {
			reason = "deferred" // Jobs.finish treats "" as success; never let that lie
		}
		return nil, errors.New(reason)
	}

	// An agent dispatch (already authorized above) creates its job through
	// AcceptAgent so the record carries the marker handleJob's poll auth keys
	// on. Dead until Task 4 registers the "agent" task (BuildRequest 400s it
	// today), but wired now so the poll gate is complete the moment the task
	// lands, not a follow-up.
	accept := s.jobs.Accept
	if env.TaskType == string(core.TaskAgentRun) {
		accept = s.jobs.AcceptAgent
	}
	if !accept(env.JobID, run) {
		cleanup() // duplicate/drain refusal: this request's materialized files never run
		view, ok := s.jobs.Get(env.JobID)
		if !ok {
			// Accept refused but the id is absent: drain began between the
			// Draining() check and Accept.
			writeError(w, http.StatusServiceUnavailable, "node draining")
			return
		}
		// Contract refusal semantics: the dispatcher treats ANY non-202
		// dispatch response as a REFUSAL and may re-dispatch the same job_id
		// to another node. So a duplicate for a job in accepted/running — or
		// DONE (the lost-ack + fast-render scenario) — must re-ack 202, or a
		// lost ack would buy a duplicate render fleet-wide; after the re-ack
		// the dispatcher's tracker polls /fleet/jobs/{id} and immediately
		// finds the terminal state with data. Only a previously FAILED job
		// answers non-202: 409 is an explicit refusal, so the dispatcher may
		// legitimately try the failed job on another node.
		if view.State == JobError {
			writeError(w, http.StatusConflict, "job previously failed on this node: "+view.Error)
			return
		}
		writeAck(w, env.JobID) // accepted/running/done: idempotent re-ack, still just one render
		return
	}
	writeAck(w, env.JobID)
}

// handleJob is the poll path: the job's current wire state, or 404 for an id
// we never acked (or already evicted).
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	view, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	// Agent-lane poll auth (v1 scope): only jobs CREATED by an agent dispatch
	// are gated — media polls never see this branch, so the deployed media
	// clients stay byte-identical (pinned in auth_test.go). Checked AFTER the
	// store lookup because the job RECORD carries the marker (see AcceptAgent):
	// an unknown/evicted id stays a plain 404 — job ids are caller-generated
	// ULIDs, so a 404 discloses nothing — while a live agent job answers 401
	// before any state or data crosses the wire. No token configured = the
	// loopback-only posture, where the lane is open by design (dispatch
	// enforces that a tokenless listener IS loopback).
	if view.Agent && s.opts.Cfg.FleetAuthToken != "" && !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJobView(w, http.StatusOK, view)
}

// handleMedia serves ONE rendered artifact by bare filename out of the same
// dir render jobs write into — config.Config.MediaDir, exactly the source
// pipeline.go's runImageGen joins onto for GenerateSdcpp's `out` argument
// (see `filepath.Join(p.cfg.MediaDir, "render-"+hash+".png")`). Read-only and
// deliberately narrow: a single path segment inside the configured media
// dir, so the fleet's unauthenticated surface gains a file reader, never a
// browser. A name containing a separator or ".." dies at validation before
// any filesystem call; what's left is resolved through symlinks and its
// resolved path is checked for containment under the resolved media dir, so
// a symlink planted inside MediaDir cannot serve a file from outside it.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("filename")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, "filename must be a bare name")
		return
	}
	mediaDir := s.opts.Cfg.MediaDir
	path := filepath.Join(mediaDir, name)
	resolved, err := filepath.EvalSymlinks(path)
	dirResolved, dirErr := filepath.EvalSymlinks(mediaDir)
	if err != nil || dirErr != nil || !strings.HasPrefix(resolved, dirResolved+string(filepath.Separator)) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	http.ServeFile(w, r, resolved) // sets Content-Type from extension, handles range/HEAD
}

// jobWire is the jobs-endpoint shape: `state` (not `status`), `data` only on
// done, `error` only on error.
type jobWire struct {
	JobID string          `json:"job_id"`
	State JobState        `json:"state"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

func writeJobView(w http.ResponseWriter, status int, v *JobView) {
	writeJSON(w, status, jobWire{JobID: v.ID, State: v.State, Data: v.Data, Error: v.Error})
}

// writeAck emits the ONLY acceptance shape the contract allows: 202 + exact
// job_id echo + "accepted".
func writeAck(w http.ResponseWriter, jobID string) {
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "status": "accepted"})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"status": "error", "error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
