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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/hostsample"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/storesteward"
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

// agentResidencyMaxStale is how old a cached residency answer may be before a
// health read WAITS for a fresh one instead of serving it: one more TTL
// window past agentResidencyTTL. Inside that band the read keeps the
// stale-while-revalidate shape (serve the previous answer, refresh behind
// it); beyond it the previous answer was taken before the seat had time to
// load or unload, and serving it charged the delegator's eta a cold load
// for a seat that had been warm for two minutes (the 0.128.1 slot census:
// `chosen eta 326 s (cold 28 + 298 gen)` on a seat whose admission wait was
// 6 ms). The wait is bounded by residencyWaitBound, never open-ended.
const agentResidencyMaxStale = 2 * agentResidencyTTL

// residencyWaitBound caps how long a too-stale health read waits for the
// probe it kicked. It is sized for a RESPONSIVE llama-swap — the three
// refresh legs (/v1/models twice, /running once) answer in milliseconds on a
// healthy box — and it sits UNDER every health client's own budget: the
// accelerator router probes each base with 2 s (internal/accelremote), the
// fleet UI poller with 5 s (internal/fleetview), the delegator with 15 s
// (internal/delegate fetchNodeViewTimeout). A slow or hung llama-swap costs
// a health request this much, once per refresh cycle, and then the previous
// answer is served with a log line — never a client-side timeout. Any new
// /fleet/health client must budget above this. A var so tests can shorten it.
var residencyWaitBound = 1500 * time.Millisecond

// errSeatStateUnresolved marks a /running reading that came back without an
// error and without an ANSWER: the roster could not be read WHILE /running
// listed models, so the seat could only be matched by its bare name, which
// cannot see an alias-bound seat listed under its canonical id (seatload's
// `Ambiguous`). "Not loaded" and "could not tell" are different facts — and a
// failed roster over an EMPTY /running is the first, not the second.
var errSeatStateUnresolved = errors.New("fleet: seat state unresolved (roster unreadable, alias could not be matched)")

// errRefreshIncomplete is the seat-state read's "never ran" value: the deferred
// publish in refreshAgentResidency runs on EVERY exit including a panic, and a
// nil error there would publish "seat not loaded, and we know it" for a read
// that never happened.
var errRefreshIncomplete = errors.New("fleet: seat state not read")

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
	Reclaim   func(freeGiB, totalGiB float64) ReclaimVerdict
	GpuVendor string
	GpuArch   string
	// Backends is the serving-backend list from the installer's manifest
	// (InstalledInfo.Backends: primary first, then the alternates this install
	// rendered — a dual-route node advertises ["vulkan","cpu"]). nil omits the
	// field, so every node without a manifest backend is byte-identical.
	Backends []string
	// Accelerators is the additive-device list from the installer's manifest
	// (installed.json `accelerators`, ADR 0024) — advertised verbatim in health
	// so a delegator can route NPU-owned work to this node. Empty = omitted.
	Accelerators []string
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
	// KVSlotDir is the directory the seats' --slot-save-path points at (ADR 0056
	// Layer 2); empty = the kvslot lane answers 501. KVSlotCapGiB bounds it (0 = 8).
	KVSlotDir    string
	KVSlotCapGiB int
	// Host reports the last host CPU/RAM sample (hostsample.Sampler.Load).
	// nil omits the host_* fields; the handler never samples itself.
	Host func() (hostsample.Sample, bool)
	// Lease reads this node's machine-wide GPU lease (gpulease.InspectDir over
	// the config's lease dir). nil = not advertised, never refused.
	Lease func() gpulease.Info
	// Store returns the store steward's last status; nil = no steward.
	Store func() storesteward.Status
	// ServingConfig reports the rendered serving config's provenance: its spec
	// hash and its state (MATCH/STALE/UNSTAMPED/HAND-EDITED). nil, or an empty
	// state, omits both health fields.
	//
	// It is a func and not a path because the re-derive needs the tier table and
	// the serving templates, which are embedded in the COMMAND (install_render.go)
	// -- this package serves health, it does not own the seeds.
	ServingConfig func() (specSHA256, state string)
}

// Server is the fleet-node HTTP server: three handlers over a Runner + Jobs
// store. Task/family advertisements are derived once at construction (config
// is immutable while serving).
type Server struct {
	runner Runner
	jobs   *Jobs
	opts   Options
	// queue is the Option B consolidated pull queue (ADR 0030) — non-nil ONLY
	// when this node is the config-elected holder (fleet_queue_host). Opened
	// by EnableQueueHost; the routes mount only when it is non-nil.
	queue    *fleetqueue.Queue
	tasks    []string
	families []string
	// imageFamilies is the named-family advertisement (ADR 0058), computed once
	// like families; nil on a node without named families (key omitted).
	imageFamilies []ImageFamily
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
	// visionLane is VisionLaneAdmissible over the RESOLVED listener (0.116.0)
	// — the same one-predicate discipline as agentLane: health publishes
	// vision_model (and SupportedTasksFor lists "vision") exactly when
	// POST /fleet/vision will admit.
	visionLane bool
	// chatLane is ChatLaneAdmissible over the RESOLVED listener (C-41b) — the
	// same one-predicate discipline as agentLane and visionLane: health
	// publishes `chat_lane` exactly when POST /fleet/chat will admit, because
	// the delegator reads that one field to decide a cascade lane base is
	// usable at all.
	chatLane bool
	// seatRateCache / seatRateAt / seatRateMu: the health handler's cached
	// read of the seat-rates store (seatRate).
	seatRateCache *SeatRateHealth
	seatRateAt    time.Time
	seatRateMu    sync.Mutex
	// rosterServes answers "does the llama-swap behind endpoint serve seat?"
	// (alias-aware). A seam so tests drive the residency cache without a live
	// llama-swap; production always gets swapRosterServes.
	rosterServes func(ctx context.Context, endpoint, seat string) (bool, error)
	// rosterServedModels answers "what names does the llama-swap behind
	// endpoint actually serve?" — the roster's own canonical ids PLUS every
	// alias (swapclient.Roster.Names), advertised as served_models.
	//
	// It was rosterIDs, publishing IDs() alone, until the alias-blind gate
	// finding: agent seats are normally bound by ALIAS (agent-pool ->
	// qwen3.8-27b-vllm, offload-e4b -> gemma-4-e4b — see swapclient.go:9's
	// plannerUnserved lesson), so a delegator checking an alias seat name
	// against an id-only served_models list would read a healthy node as
	// not serving its own advertised agent_seat and silently drop it from
	// placement. Names() is IDs() plus every alias, so this field is
	// misnamed as "IDs" now and renamed to say what it actually publishes.
	//
	// A separate seam from rosterServes (tests need to drive them
	// independently), but production fetches its own roster per call rather
	// than sharing swapRosterServes's fetch — that would mean threading a
	// shared Roster through both closures' signatures, which changes tests
	// that already inject rosterServes alone. THREE /v1/models GETs per refresh
	// since the seat-state read joined them (seatRunning re-fetches the roster
	// to resolve the seat's alias before reading /running), once per
	// agentResidencyTTL window, is an acceptable cost for keeping today's
	// residency seam untouched. What must stay true is the property the cache
	// tests pin: one refresh CYCLE per TTL, single-flighted, never a probe per
	// health request.
	rosterServedModels func(ctx context.Context, endpoint string) ([]string, error)
	// seatRunning answers "what does llama-swap's /running say about the agent
	// seat RIGHT NOW?" — loaded, starting, or neither. /running ONLY: reading
	// `/upstream/<seat>/…` would START an unloaded seat (register C-05; 54
	// measured status probes each blocked ~186 s for exactly that reason), so
	// the health path is allowed to learn the seat's state and is not allowed
	// to touch the seat. Alias-aware through internal/seatload, because the
	// harness binds seats by alias and /running lists canonical ids.
	seatRunning func(ctx context.Context, endpoint, seat string) (seatload.Reading, error)
	// setWriteDeadline extends ONE handler's write deadline past the server's
	// blanket WriteTimeout (see extendWrite). A field so a test can observe the
	// deadline a handler asks for; production is controllerWriteDeadline.
	setWriteDeadline func(w http.ResponseWriter, t time.Time) error
	// admitting counts the runs this process has claimed that are still in
	// their ADMISSION phase (see admittingRuns). A field for the same reason as
	// the roster seams: tests drive it without a registry on disk.
	admitting   func() int
	admittingAt time.Time
	admittingN  int
	admittingMu sync.Mutex
	activityReg *gpuactivity.Registry
	// admittingLoggedOpen / admittingLoggedList keep the two registry failures
	// to ONE line per process: health is polled every few seconds by every
	// delegator, so a per-cycle line would be a log flood, and the failure is
	// reported for an operator rather than for a counter.
	admittingLoggedOpen bool
	admittingLoggedList bool
	agentRes            agentResidency
	// agentResultDecodeOnce keeps the "could not decode a finished contract's
	// result" line to one per process: the only producer of that JSON is this
	// process's own agenttask, so the failure is a wire-shape drift, not a
	// per-job event, and once said it is said.
	agentResultDecodeOnce sync.Once
}

// agentResidency caches the roster's answer for the agent seat between health
// requests. The cache exists because BOTH of these hold at once:
//   - agent_seat_resident must be roster-verified (§S3) — config alone cannot
//     know whether the seat is actually served;
//   - the health handler must not block on llama-swap as a matter of course
//     (the reclaim tracker's rule — fleet_reclaim.go) and must not probe per
//     request. Since 0.128.2 there is ONE bounded exception, below.
//
// So a health request serves the cached answer and, when it is older than
// agentResidencyTTL, triggers ONE background refresh (inflight = the
// single-flight latch). Older than agentResidencyMaxStale or never probed,
// the request WAITS for that refresh — at most residencyWaitBound — then
// serves whatever is published, and says so on the log once per refresh
// when the wait ran out. An agent contract that completed a call on the
// advertised seat writes THAT fact (loaded, not starting) straight into the
// cache (noteSeatAnswered) and never forces a probe; a probe that started
// before such a write keeps the job's seat facts when it lands and still
// publishes its roster facts. Residency itself stays roster-verified — a
// job never asserts it — and the cache's age is never touched by a job, so
// the roster keeps refreshing on its own cadence under sustained traffic.
// Until the first probe lands the answer is false — fail CLOSED: an
// unverified seat reads as not resident, and the delegator keeps the work
// local, which is always the safe placement.
type agentResidency struct {
	mu       sync.Mutex
	resident bool
	// served is the roster's ids+aliases list from the SAME refresh that set
	// resident (rosterServedModels, fetched alongside rosterServes in
	// refreshAgentResidency). A failed fetch publishes nil — never a stale
	// list — and never flips
	// resident on its own; a later placement gate reads absent/empty as
	// UNKNOWN, exactly like a cold or failed residency probe.
	served []string
	// seatLoaded / seatStarting / seatKnown are llama-swap's /running verdict on
	// the agent seat, published by the SAME refresh (seatRunning). seatKnown
	// false = the read failed or never ran, and health then omits both fields
	// rather than publishing "not loaded" for "could not tell".
	seatLoaded   bool
	seatStarting bool
	seatKnown    bool
	at           time.Time
	inflight     bool
	// timeoutLogged keeps the "served the previous answer after the wait ran
	// out" line to ONE per refresh cycle: set by the read that logs it, reset
	// by the publish. Health is polled by every delegator, so a line per
	// waiting request would be a flood.
	timeoutLogged bool
	// seatFactAt is when noteSeatAnswered last wrote the seat state from a
	// completed contract call. A refresh that STARTED before it keeps the
	// job's seat facts when it lands (its /running reading predates them)
	// and still publishes its roster facts.
	seatFactAt time.Time
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

// awaitProbeFor is awaitProbe with a ceiling: it returns when the running
// probe publishes OR when bound elapses, whichever is first. The waiter
// goroutine it leaves behind on a timeout ends when the probe does (the
// probe's own per-read timeouts bound that), so nothing here can leak past a
// hung llama-swap.
func (a *agentResidency) awaitProbeFor(bound time.Duration) {
	done := make(chan struct{})
	go func() {
		a.awaitProbe()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(bound):
	}
}

// noteSeatAnswered records what a completed contract call PROVES: the
// advertised seat is loaded and not starting — right now, whatever the last
// probe said. Written straight into the cache so the next health read serves
// it without a probe and the next placement deal does not charge a cold load
// for a seat that just answered (the 0.128.1 census). It touches ONLY the
// seat state: residency stays roster-verified, the served list stays the last
// probe's, and the cache's age (`at`) is untouched so the roster keeps
// refreshing on its own cadence under sustained traffic.
func (a *agentResidency) noteSeatAnswered() {
	a.mu.Lock()
	a.seatLoaded, a.seatStarting, a.seatKnown = true, false, true
	a.seatFactAt = time.Now()
	a.mu.Unlock()
}

// noteAgentResult reads the one fact a finished agent contract proves about
// the advertised seat: whether it completed at least one call on it. Only a
// result that RAN on s.agentSeat counts. A defer that never reached the seat
// (roster-unserved, config, contract, a composite guard) finishes as a
// SUCCESS-shaped job — agenttask's OK is true for every terminal outcome,
// defers included — with zero steps, and proves nothing; a contract that ran
// and hit its wall has steps and proves the seat is loaded; a contract that
// ran on another layer's seat proves nothing about this one.
func (s *Server) noteAgentResult(data json.RawMessage) {
	var r struct {
		Seat  string `json:"seat"`
		Steps int    `json:"steps"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		// Not a "proves nothing" outcome: the producer is this process's own
		// agenttask, so a decode failure means the wire shape drifted, and
		// silently losing the fast path for every job from here on would
		// look exactly like a node that never ran agent work. Said once.
		s.agentResultDecodeOnce.Do(func() {
			log.Printf("fleet: a finished agent contract's result could not be decoded for the seat-state write (%v); until this is fixed the residency cache learns the seat state from /running only", err)
		})
		return
	}
	if r.Steps <= 0 || r.Seat == "" || r.Seat != s.agentSeat {
		return
	}
	s.agentRes.noteSeatAnswered()
}

// New builds a Server. The supported-task/family lists are computed here, not
// per health request — the config cannot change under a running server.
func New(runner Runner, jobs *Jobs, opts Options) *Server {
	s := &Server{
		runner:             runner,
		jobs:               jobs,
		opts:               opts,
		tasks:              SupportedTasksFor(opts.Cfg, opts.LoopbackListener),
		families:           Families(opts.Cfg),
		imageFamilies:      ImageFamilies(opts.Cfg),
		agentSeat:          opts.Cfg.AgentPlannerModel(""),
		agentLane:          AgentLaneAdmissible(opts.Cfg, opts.LoopbackListener),
		visionLane:         VisionLaneAdmissible(opts.Cfg, opts.LoopbackListener),
		chatLane:           ChatLaneAdmissible(opts.Cfg, opts.LoopbackListener),
		rosterServes:       swapRosterServes,
		rosterServedModels: swapRosterServedModels,
		seatRunning:        swapSeatRunning,
		setWriteDeadline:   controllerWriteDeadline,
	}
	s.admitting = s.admittingRuns
	return s
}

// seatRunningClient reads /running on the node's own llama-swap. netguard's
// transport for the same reason every outbound client here uses it, and a
// timeout equal to the residency probe's: this runs on the background refresh,
// never inside a health request.
var seatRunningClient = &http.Client{Transport: netguard.SafeTransport(nil), Timeout: agentResidencyProbeTimeout}

// swapSeatRunning is the production seat-state read: llama-swap's /running,
// alias-resolved, and nothing else.
func swapSeatRunning(ctx context.Context, endpoint, seat string) (seatload.Reading, error) {
	return seatload.Running(ctx, seatRunningClient, endpoint, seat)
}

// admittingTTL bounds how often the health path re-reads the activity registry.
// Health is polled every few seconds by every delegator, and the registry is a
// directory: one read per window, never one per request — the same rule the
// seat-rates store and the residency cache follow.
const admittingTTL = 2 * time.Second

// admittingRuns counts THIS PROCESS's agent runs that are still in their
// ADMISSION phase, from the gpuactivity registry the harness already writes and
// heartbeats (ADR 0041).
//
// WHICH PHASE, and why this source (register S-17): internal/pipeline registers
// every contract run with `Phase: "admission"` and flips it to "running" at the
// first planner step, so "admission" is precisely the window the diagnosis
// measured — cordon → swap pre-flight → warm → coherence probe, up to 300 s
// during which jobs.go has already flipped the job to `running` and the card is
// idle. The job store cannot answer this: it knows a worker took the job, not
// what that worker is waiting for.
//
// PID-scoped on purpose: the registry is machine-wide, and another process's
// `agent_run` sitting in admission is not one of THIS node's jobs. It holds the
// same seat, but it is not part of this node's queue accounting and must not
// subtract from this node's saturation.
func (s *Server) admittingRuns() int {
	s.admittingMu.Lock()
	defer s.admittingMu.Unlock()
	if !s.admittingAt.IsZero() && time.Since(s.admittingAt) < admittingTTL {
		return s.admittingN
	}
	s.admittingAt = time.Now()
	s.admittingN = 0
	if s.activityReg == nil {
		reg, err := gpuactivity.Open(s.opts.Cfg.GPULockPath, s.opts.Cfg.StateDir)
		if err != nil {
			// RETRIED on the next cycle, never latched: a root that is briefly
			// unresolvable (a state dir not yet created, a transient mount) must
			// not silence this field for the life of the process. And it is SAID
			// — once — because 0 is a legitimate value, so an unreported failure
			// is indistinguishable from a node with nothing in admission.
			if !s.admittingLoggedOpen {
				s.admittingLoggedOpen = true
				log.Printf("fleet: activity registry unreadable under state dir %q; jobs_admitting reads 0 until it resolves (reported once per process, retried every %s): %v",
					s.opts.Cfg.StateDir, admittingTTL, err)
			}
			return 0
		}
		s.activityReg = reg
	}
	runs := s.activityReg.List(time.Now())
	if len(runs) == 0 {
		// List treats an unreadable directory as an empty one (it is advisory),
		// so "nothing in admission" and "cannot read the registry" arrive here
		// as the same nil slice. One cheap read tells them apart; a directory
		// that does not exist yet is the normal cold state and says nothing.
		if _, rerr := os.ReadDir(s.activityReg.Dir()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && !s.admittingLoggedList {
			s.admittingLoggedList = true
			log.Printf("fleet: activity registry directory %q cannot be listed; jobs_admitting reads 0 (reported once per process): %v",
				s.activityReg.Dir(), rerr)
		}
	}
	pid := os.Getpid()
	for _, run := range runs {
		// The SAME constant internal/pipeline/agenttask.go registers a contract
		// run with, and the same one gpuactivity heals at the first planner
		// step — a bare literal here would make a rename a silent zero.
		if run.PID == pid && run.Phase == gpuactivity.PhaseAdmission {
			s.admittingN++
		}
	}
	return s.admittingN
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

// swapRosterServedModels is the production served_models probe: one GET
// /v1/models, answered as the roster's own ids PLUS every alias
// (swapclient.Roster.Names — see the rosterServedModels field doc for why
// IDs() alone was the alias-blind gate bug). A separate roster fetch from
// swapRosterServes (see that field's doc) — accepted so the existing
// rosterServes seam and its tests stay untouched.
func swapRosterServedModels(ctx context.Context, endpoint string) ([]string, error) {
	roster, err := swapclient.FetchRoster(ctx, endpoint, agentResidencyProbeTimeout)
	if err != nil {
		return nil, err
	}
	return roster.Names(), nil
}

// agentResident returns the cached residency answer, kicking off a background
// refresh when the cache has aged past agentResidencyTTL. It never probes more
// than once per TTL window regardless of request rate, and blocks only on a
// too-stale or never-probed cache — for at most residencyWaitBound.
func (s *Server) agentResident() bool {
	a := &s.agentRes
	a.mu.Lock()
	age := time.Since(a.at)
	if !a.at.IsZero() && age <= agentResidencyTTL {
		v := a.resident
		a.mu.Unlock()
		return v
	}
	// Past the TTL. Inside the stale band (one more window) serve the previous
	// answer and refresh behind it. Beyond it — or never probed — the previous
	// answer describes a seat that has had time to change state: kick the
	// refresh if nobody holds it and WAIT for it, bounded, before answering.
	tooStale := a.at.IsZero() || age > agentResidencyMaxStale
	owner := !a.inflight
	if owner {
		a.inflight = true
	}
	if !tooStale {
		v := a.resident // serve the previous answer while the refresh runs
		a.mu.Unlock()
		if owner {
			go s.refreshAgentResidency()
		}
		return v
	}
	previousAt := a.at
	a.mu.Unlock()
	if owner {
		go s.refreshAgentResidency()
	}
	a.awaitProbeFor(residencyWaitBound)
	a.mu.Lock()
	v := a.resident
	if !a.at.After(previousAt) && !a.timeoutLogged {
		// Nothing was published inside the bound: the previous answer goes
		// out, and that is said ONCE per refresh cycle (the publish resets
		// the flag), never once per waiting request.
		a.timeoutLogged = true
		age := "never taken"
		if !previousAt.IsZero() {
			age = time.Since(previousAt).Round(time.Second).String() + " old"
		}
		log.Printf("fleet: health waited %s for the residency refresh of %q against %s and it had not published; serving the previous answer (%s)",
			residencyWaitBound, s.agentSeat, s.opts.Cfg.Endpoint, age)
	}
	a.mu.Unlock()
	return v
}

// recentAgentWallSamples is how many finished agent jobs recent_agent_wall_sec
// is the median of. Eight: enough that one outlier cannot move the median, few
// enough that the number describes the node NOW rather than an hour ago (the
// store's terminal TTL is an hour).
const recentAgentWallSamples = 8

// medianSeconds is the median of a duration sample, in seconds rounded to
// hundredths. An empty sample is 0 = "this node makes no claim", which the
// payload omits.
func medianSeconds(d []time.Duration) float64 {
	if len(d) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	mid := len(sorted) / 2
	sec := sorted[mid].Seconds()
	if len(sorted)%2 == 0 {
		sec = (sorted[mid-1].Seconds() + sec) / 2
	}
	return math.Round(sec*100) / 100
}

// queueWaitEstimateSec is how long a caller should expect to wait for a
// worker to free, from the node's OWN measured recent wall — never from
// seat_rate.min_turn_sec, which is a max-final RETRY floor for one seat and
// has no relationship to how deep this node's backlog is (register S-04,
// diagnosis §5.2, the overhaul plan's roast correction).
//
// cappedDepth is the CAPPED backlog only — Jobs.CountsCapped's queued+running,
// whatever phase (an admitting job still holds the worker slot a waiter
// needs; see health's admitting note) — NEVER health's all-jobs queue_depth
// (register review round 1 BLOCKER). max_concurrent_jobs bounds only jobs
// that count against it (the same set runningCappedLocked/IdleSlot measure);
// an uncapped job (a render, an stt, a pipeline route) never waits behind
// that cap, so dividing an ALL-jobs depth by maxConcurrent would inflate the
// estimate for a node whose agent slots are genuinely idle behind unrelated
// media load — exactly the class of bug saturationOf (S-17) and this PR's own
// IdleSlot fix (S-20) both already avoid.
//
// excess = cappedDepth - maxConcurrent is how many admitted CAPPED jobs are
// beyond what can run AT ONCE right now; each of maxConcurrent workers
// retires roughly one job every recentWallSec, so the deepest excess job
// waits about excess x recentWallSec / maxConcurrent. 0 (no claim) when
// maxConcurrent is unlimited (<=0 — nothing ever waits for a worker), there
// is no excess (a worker is free), or the node has no recent wall sample at
// all.
func queueWaitEstimateSec(cappedDepth, maxConcurrent int, recentWallSec float64) float64 {
	if maxConcurrent <= 0 || recentWallSec <= 0 {
		return 0
	}
	excess := cappedDepth - maxConcurrent
	if excess <= 0 {
		return 0
	}
	return float64(excess) * recentWallSec / float64(maxConcurrent)
}

// retryAfterFor computes the "queue full" 503's Retry-After header value AND
// the human-readable suffix appended to the refusal message, from ONE
// evaluation of the node's own measured recent wall — so the header and the
// text can never disagree (register S-04; review round 1 items 2/3).
//
// The header is always ceil(queueWaitEstimateSec), bounded to [5, 300] so it
// is never "retry at once" (the queue IS full right now) and never an
// unbounded promise. The TEXT distinguishes three shapes the bare number
// cannot:
//
//   - No recent wall sample yet (a fresh node, or one that has never
//     finished an agent job): the flat 30s is a DEFAULT, not a measurement,
//     and must not be worded like one — "no recent completions yet — retry
//     in 30 s", never "~30 s until a worker frees" (that phrasing implies a
//     precision the node does not have).
//   - A genuine measured estimate that lands inside [5, 300]: "~N s until a
//     worker frees, from recent completions".
//   - An estimate that would exceed 300s: the header is still capped at 300
//     (an unbounded promise is worse than an honest floor), but the text says
//     so explicitly — ">=300 s until a worker frees" — rather than
//     presenting the cap as if it were the precise answer. A raw 600s
//     estimate silently rendered as "~300 s" invites every waiter to retry
//     in lockstep at the same instant (the overhaul plan's roast correction:
//     an admission refusal must be something the delegator can outwait,
//     never terminal, and a disguised clamp defeats that).
func retryAfterFor(cappedDepth, maxConcurrent int, recentWallSec float64) (headerSec int, suffix string) {
	if recentWallSec <= 0 {
		return 30, "no recent completions yet — retry in 30 s"
	}
	sec := int(math.Ceil(queueWaitEstimateSec(cappedDepth, maxConcurrent, recentWallSec)))
	switch {
	case sec < 5:
		return 5, "~5 s until a worker frees, from recent completions"
	case sec > 300:
		return 300, ">=300 s until a worker frees, from recent completions"
	default:
		return sec, fmt.Sprintf("~%d s until a worker frees, from recent completions", sec)
	}
}

// seatState reports the cached /running answer for the agent seat: loaded,
// starting, and whether the read is KNOWN at all. Never probes itself — the
// background residency refresh publishes it on the same TTL.
func (s *Server) seatState() (loaded, starting, known bool) {
	a := &s.agentRes
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seatLoaded, a.seatStarting, a.seatKnown
}

// servedModels returns a copy of the cached roster id list, as of the last
// residency refresh. It NEVER triggers a probe itself — agentResident()
// already does, and handleHealth calls both, agentResident() first, so a
// health request that reads servedModels() first would always see the
// PREVIOUS window's answer instead of the refresh it just scheduled.
func (s *Server) servedModels() []string {
	a := &s.agentRes
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.served == nil {
		return nil
	}
	return append([]string(nil), a.served...)
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
	probeStart := time.Now()
	var resident bool
	var err error
	var served []string
	var seat seatload.Reading
	var seatErr error = errRefreshIncomplete
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
		a.served = served
		if !a.seatFactAt.After(probeStart) {
			a.seatLoaded, a.seatStarting, a.seatKnown = seat.Loaded, seat.Starting, seatErr == nil
		}
		// else: a contract completed a call on the seat while this probe ran
		// (noteSeatAnswered) and the /running reading here predates that —
		// the job's seat facts stand; the roster facts above are this
		// probe's either way.
		a.at = time.Now()
		a.inflight = false
		a.timeoutLogged = false
		a.idleCond().Broadcast() // release any synchronous waiter (RefreshAgentResidency)
		a.mu.Unlock()
	}()
	residentCtx, residentCancel := context.WithTimeout(context.Background(), agentResidencyProbeTimeout)
	defer residentCancel()
	resident, err = s.rosterServes(residentCtx, s.opts.Cfg.Endpoint, s.agentSeat)
	if err != nil {
		// agent_seat_resident:false is the one field that stops EVERY remote
		// placement at this node, and the probe error was the only evidence of
		// why. Without this line an operator sees a node advertising itself as
		// unusable and has nothing, on either side of the wire, to look at. The
		// probe runs at most once per TTL window, so this cannot spam.
		log.Printf("fleet: agent seat %q residency probe against %s failed; advertising agent_seat_resident:false for up to %s: %v",
			s.agentSeat, s.opts.Cfg.Endpoint, agentResidencyTTL, err)
	}
	// The served-models fetch gets its OWN full-length timeout rather than
	// reusing residentCtx: sharing one context meant a slow-but-healthy
	// rosterServes call could burn most of the budget before
	// rosterServedModels even starts, starving a perfectly healthy roster
	// and publishing served=nil for a seat that is, in fact, resident.
	namesCtx, namesCancel := context.WithTimeout(context.Background(), agentResidencyProbeTimeout)
	defer namesCancel()
	names, nerr := s.rosterServedModels(namesCtx, s.opts.Cfg.Endpoint)
	if nerr != nil {
		names = nil // unknown on failure — never keep a stale list, never flip resident
	}
	served = names

	// The seat's LOAD state, from /running only (register C-05: nothing here may
	// touch a path that could start the seat). Its own full-length timeout, for
	// the same reason the served-models fetch has one: a slow roster must not
	// starve the read after it. A failure publishes "unknown" — seat_loaded and
	// seat_starting are then absent, never false.
	if s.seatRunning != nil && strings.TrimSpace(s.opts.Cfg.Endpoint) != "" {
		seatCtx, seatCancel := context.WithTimeout(context.Background(), agentResidencyProbeTimeout)
		defer seatCancel()
		seat, seatErr = s.seatRunning(seatCtx, s.opts.Cfg.Endpoint, s.agentSeat)
		switch {
		case seatErr != nil:
			// Same discipline as the residency probe above: the verdict this
			// publishes (two absent fields) is useless to an operator without
			// the reason, and the read runs at most once per TTL window, so it
			// cannot spam.
			log.Printf("fleet: seat state of %q against %s could not be read; omitting seat_loaded/seat_starting for up to %s: %v",
				s.agentSeat, s.opts.Cfg.Endpoint, agentResidencyTTL, seatErr)
			seat = seatload.Reading{}
		case !seat.Loaded && seat.Ambiguous:
			// A NIL ERROR IS NOT ALWAYS A RESOLVED READING. seatload falls back
			// to matching /running by the BARE name when the roster read fails —
			// deliberately, because that beats a refusal — and the bare name
			// cannot see a seat listed under its canonical id. Every harness seat
			// is alias-bound, so when /running listed SOMETHING that could not be
			// matched, "not loaded" means "could not tell", and publishing it as
			// seat_loaded:false would assert that a loaded seat is idle exactly
			// when the box is busy enough to time out a roster GET.
			//
			// `Ambiguous`, NOT `RosterErr`, is the predicate — the same one
			// gpu_drain (`!rd.Loaded && rd.Ambiguous`) and placement/live.go
			// (`err == nil && !rd.Ambiguous`) key on, and it is narrower on
			// purpose: seatload defines it as a failed roster AND a non-empty
			// /running. A failed roster with an EMPTY /running is perfectly
			// knowable — nothing is loaded on this box, so the seat is not
			// loaded, and no name resolution is needed to say so. Withholding
			// that would hide a fact the node has, on the common shape (a quiet
			// box whose llama-swap is slow to answer /v1/models).
			log.Printf("fleet: seat state of %q against %s is UNRESOLVED — the roster read failed and /running lists %d model(s) that the bare name cannot match, so an alias-bound seat listed under its canonical id cannot be seen; omitting seat_loaded/seat_starting for up to %s: %v",
				s.agentSeat, s.opts.Cfg.Endpoint, seat.RunningOthers, agentResidencyTTL, seat.RosterErr)
			seat, seatErr = seatload.Reading{}, errSeatStateUnresolved
		}
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
	// The vision lane (0.116.0): its own route ONLY because its body is an
	// image — sized from vision_max_image_bytes rather than dispatch's 1 MiB —
	// after which it joins the same admission path (admit) as every job.
	mux.HandleFunc("POST /fleet/vision", s.handleVision)
	// The cascade chat lane (C-41b): a SYNCHRONOUS forward, not a job — see
	// chat_lane.go for why a single short cascade call does not belong in the
	// job store, and why this node's loopback-only llama-swap needs a door of
	// its own at all.
	mux.HandleFunc("POST "+ChatLanePath, s.handleChat)
	mux.HandleFunc("POST "+KVSlotSavePath, s.handleKVSlotSave)
	mux.HandleFunc("POST "+KVSlotRestorePath, s.handleKVSlotRestore)
	mux.HandleFunc("GET /fleet/jobs/{id}", s.handleJob)
	// GET /fleet/jobs (no {id}) is a distinct ServeMux pattern from the one
	// above — unauthenticated is deliberate: unlike /fleet/jobs/{id}, this
	// route never carries a payload (Data is never set; see Jobs.Recent), so
	// there is nothing here for the per-job bearer gate to protect on a MEDIA
	// row. An AGENT row is different: its `error` string can echo contract
	// content (the goal, a tool result fragment) the way a media job's never
	// does, so handleJobs applies the SAME bearer check handleJob does and
	// omits `error` on agent rows for a caller that fails it — id/task/
	// model/state/timestamps stay unauthenticated on every row, exactly like
	// before; only an agent row's `error` text is gated.
	mux.HandleFunc("GET /fleet/jobs", s.handleJobs)
	// {filename...} (not {filename}) is deliberate: it's a multi-segment
	// wildcard, so a caller-supplied "/" or a %2F-hidden one still reaches the
	// handler as PART OF filename instead of 404ing at the mux before our
	// validation ever runs — every malformed name gets this file's one 400
	// JSON shape, not the mux's bare-text 404.
	mux.HandleFunc("GET /fleet/media/{filename...}", s.handleMedia)
	if s.queue != nil {
		// Option B holder surface (ADR 0030). Auth = the SAME bearer rule as
		// the agent lane: the queue carries agent contracts, so it inherits
		// that lane's gate — token required when configured, else loopback
		// trust only (the queue is pointless on a loopback-only fleet, but
		// failing open beyond loopback without a token would be the RCE-class
		// surface the agent lane refuses).
		fleetqueue.Mount(mux, s.queue, func(r *http.Request) bool {
			if s.opts.Cfg.FleetAuthToken == "" {
				return s.opts.LoopbackListener
			}
			return bearerOK(r, s.opts.Cfg.FleetAuthToken)
		})
	}
	return mux
}

// EnableQueueHost opens the durable queue store and arms the holder surface —
// called by the serve verb ONLY when cfg.FleetQueueHost is set. Idempotent
// enough for one process; returns the store so the caller owns Close.
func (s *Server) EnableQueueHost(dbPath string) (*fleetqueue.Queue, error) {
	q, err := fleetqueue.Open(dbPath)
	if err != nil {
		return nil, err
	}
	s.queue = q
	return q, nil
}

// extendWrite pushes THIS handler's write deadline out to now+d.
//
// The blanket `WriteTimeout` below is armed by net/http at HEADER-READ time and
// bounds every handler alike, whatever that handler's own budget is — which is
// how a 30-second table came to truncate the chat lane's ten-minute proxy and
// would truncate the long poll (register S-09). The blanket STAYS: it is the
// floor that keeps a forgotten connection from pinning a goroutine, and the one
// handler that needs longer says so for itself, per request, through
// http.ResponseController.
//
// A ResponseWriter that does not support deadlines (httptest.ResponseRecorder,
// and any middleware that wraps without unwrapping) returns ErrNotSupported.
// That is NOT a request failure: the handler simply runs under whatever bound
// the writer has, exactly as before. The seam is a field so a test can observe
// what each handler ASKS for, which is the property this fixes.
func (s *Server) extendWrite(w http.ResponseWriter, d time.Duration, handler string) {
	if s.setWriteDeadline == nil {
		return
	}
	err := s.setWriteDeadline(w, time.Now().Add(d))
	if err == nil {
		return
	}
	if errors.Is(err, http.ErrNotSupported) {
		// A STANDING fact about this route's writer, not an event: the writer
		// simply has no deadline to set (a test recorder, or a middleware that
		// wraps without implementing Unwrap), so every request on the route
		// reports it and a per-invocation line is a log flood — 9 of them in
		// this package's own suite. Once per process per route says the same
		// thing: on a real serve path it means a wrapper has put this handler
		// back under the blanket write timeout, which is S-09's defect
		// reintroduced by something nobody would think to check.
		if _, seen := writeDeadlineUnsupported.LoadOrStore(handler, struct{}{}); !seen {
			log.Printf("fleet: %s runs under the blanket write timeout: this ResponseWriter does not support SetWriteDeadline, so the %s extension cannot be applied (reported once per process per route)", handler, d)
		}
		return
	}
	// Any other failure IS an event — it is about this request, not about the
	// writer's type — and the answer really will be cut if it takes longer.
	log.Printf("fleet: %s could not extend its write deadline by %s past the server's blanket WriteTimeout — this answer will be CUT if it takes longer: %v", handler, d, err)
}

// writeDeadlineUnsupported remembers which routes have already reported a
// writer that cannot carry a deadline. Keyed by the handler name, so the chat
// lane and the long poll each say it once.
var writeDeadlineUnsupported sync.Map

// controllerWriteDeadline is the production seam: the standard library's own
// per-request deadline control.
func controllerWriteDeadline(w http.ResponseWriter, t time.Time) error {
	return http.NewResponseController(w).SetWriteDeadline(t)
}

// httpServer builds the *http.Server with the spec's timeout table
// (ReadHeader 5s / Read 30s / Write 30s / Idle 120s). Split from Serve so the
// timeouts are unit-assertable.
//
// WriteTimeout is the FLOOR, not the ceiling: handlers whose own budget exceeds
// it (the chat lane's ChatProxyTimeout, the job long poll's `wait=`) extend
// their own deadline through extendWrite. Removing the blanket instead would
// leave every OTHER handler unbounded.
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
	NodeID        string `json:"node_id"`
	SchemaVersion int    `json:"schema_version"`
	GpuVendor     string `json:"gpu_vendor"`
	GpuArch       string `json:"gpu_arch"`
	// Backends lists the serving backends this node serves, primary first
	// (["vulkan","cpu"] on a dual-route node); a caller picks a route by seat id
	// (`gemma4-e2b` vs `gemma4-e2b-cpu`). Additive + omitempty (ADR 0054).
	Backends []string `json:"backends,omitempty"`
	// KVSlot is true when this node renders a slot directory and answers the
	// /fleet/kvslot/save|restore lane (ADR 0056 Layer 2).
	KVSlot bool `json:"kvslot,omitempty"`
	// Accelerators is the installer-manifest additive-device list (ADR 0024).
	// Additive + omitempty: a node with none emits a byte-identical payload.
	Accelerators []string `json:"accelerators,omitempty"`
	VramTotalGb  float64  `json:"vram_total_gb"`
	VramFreeGb   float64  `json:"vram_free_gb"`
	// GpuDevices is the full per-device breakdown behind VramTotalGb/
	// VramFreeGb's headline pick (nvidia-smi multi-device source only — see
	// HeadlineDevice for the largest-total-wins rule those two fields apply).
	// Additive: VramTotalGb/VramFreeGb keep their existing meaning for every
	// node unchanged. Omitted entirely (not an empty array) when the snapshot
	// source can't enumerate devices, so "no gpu_devices key" unambiguously
	// means "single-device source" — the exact shape every pre-fix node and
	// consumer already expects.
	GpuDevices []GPUDevice `json:"gpu_devices,omitempty"`
	// GpuUtilPct is the BUSIEST device's utilization (PAIR's multi-GPU rule,
	// adopted deliberately: the shared card is the one that matters). ALWAYS
	// present (never omitempty); when GpuUtilKnown is false, the value is 0
	// and meaningless. GpuUtilKnown is the validity flag: true means nvidia-smi
	// reported this, false means it was not queried or the query failed.
	GpuUtilPct   int  `json:"gpu_util_pct"`
	GpuUtilKnown bool `json:"gpu_util_known"`
	// WorkUtilPct answers a DIFFERENT question from GpuUtilPct and exists because
	// placement was asking the wrong one. GpuUtilPct is the busiest card on the
	// box, deliberately (the dashboard and the PAIR rule want exactly that), so it
	// counts the operator's desktop or game on the display card. WorkUtilPct is the
	// busiest card the harness can actually run a seat on — the display card is
	// skipped when gpuprobe.DisplayCardUUIDs can prove which one it is. That is the
	// figure a placement tie-break needs: on 2026-09-20 a node whose operator was
	// gaming read 33% while every card the harness could use sat at 0%.
	// WorkUtilKnown is its validity flag; a node that predates the field omits
	// both, and a consumer then falls back to GpuUtilPct.
	WorkUtilPct           int              `json:"work_util_pct"`
	WorkUtilKnown         bool             `json:"work_util_known"`
	SupportedTaskTypes    []string         `json:"supported_task_types"`
	LoadableModelFamilies []string         `json:"loadable_model_families"`
	ModelFootprints       []FootprintEntry `json:"model_footprints"`
	// ImageFamilies are the bindings an image-gen payload's `family` can select
	// (ADR 0058), each with its license: a dispatcher routing brand work must skip
	// a family whose commercial_use is false, and read a null license as UNKNOWN.
	// Omitted on a node with no named families — the pre-0.134 shape.
	ImageFamilies []ImageFamily `json:"image_families,omitempty"`
	// QueueDepth keeps its ORIGINAL meaning and shape across the 0.100.0
	// backlog/concurrency split: accepted + running, i.e. every job this node
	// owns that has not reached a terminal state. Every existing reader (the
	// delegator's placement tie-break at internal/delegate/gate.go, the media
	// dispatcher in its own repo) keeps working unchanged.
	QueueDepth int `json:"queue_depth"`
	// The four fields below are the 0.100.0 ADDITIVE capacity advertisement.
	// They are always present (never omitempty) on purpose: 0 is a real,
	// meaningful value for each — zero jobs, or "unlimited" for a limit — so a
	// reader must be able to tell a published 0 from a pre-0.100.0 node that
	// publishes nothing at all. omitempty would collapse those two into the
	// same absent key, and "unlimited" and "unknown" route very differently.
	//
	// jobs_queued + jobs_running == queue_depth, always (QueueDepth is their
	// sum by construction).
	JobsQueued  int `json:"jobs_queued"`  // admitted, waiting for a worker
	JobsRunning int `json:"jobs_running"` // executing right now
	// JobsAdmitting is the SUBSET of jobs_running whose worker has not started
	// generating yet: it is still in the run's admission phase — cordon, swap
	// pre-flight, warm, coherence probe — which the node budgets up to 300 s
	// for (register S-17). Those jobs hold a capped execution slot while the
	// card is idle, and `saturation.score` excludes them for exactly that
	// reason: a cordon-waiter holds no card, and a delegator ranking on a score
	// of 1.0 routes work AWAY from an idle box.
	//
	// Counted from the gpuactivity registry (ADR 0041) — the records this
	// process wrote for its own runs, phase "admission" — and NOT from the job
	// store, which knows a worker took the job but not what that worker is
	// waiting for. Absent = none (or a node that cannot resolve its registry),
	// which decodes to 0: the pre-0.127 reading.
	JobsAdmitting int `json:"jobs_admitting,omitempty"`
	// MaxConcurrentJobs is this node's execution limit (fleet_max_concurrent_jobs);
	// 0 = unlimited. MaxQueueDepth is its admission ceiling on queue_depth
	// (fleet_max_queue_depth); 0 = unlimited. A delegator could previously see
	// only how loaded a node was and never how much it could take — publishing
	// both limits is the point of the pair.
	//
	// Each is read from whatever actually ENFORCES it: the concurrency number
	// comes from the Jobs store (Jobs.MaxConcurrent), the admission number from
	// the config this handler's own gate consults. A store constructed with a
	// limit other than the config's would then be reported as it truly is,
	// rather than as the config wishes it were — health must never advertise a
	// capacity the node does not have.
	MaxConcurrentJobs int `json:"max_concurrent_jobs"`
	MaxQueueDepth     int `json:"max_queue_depth"`
	// HarnessVersion closes the node/repo drift gap: fleet drift used to be found
	// by hand, and a node several releases behind is debugged against known-fixed
	// bugs.
	HarnessVersion string `json:"harness_version,omitempty"`
	// ServingConfigSpecSHA256 / ServingConfigState are the rendered serving
	// config's provenance (K-02) -- NEW KEYS on this existing endpoint, not a new
	// bind and not a new route.
	//
	// The spec hash is the config's identity: the sha256 of the closed input set
	// it was rendered from (tier, render params, template, tier entry, harness
	// version). The state is this binary's verdict on it -- MATCH, STALE,
	// UNSTAMPED or HAND-EDITED -- computed by re-rendering from the node's own
	// embedded seeds. Fleet drift in the SERVING layer used to be found by
	// hand, one ssh at a time, and was not found at all when the config broke no
	// operator rule (ampere-16 served a 32768 window for weeks after the tier
	// table said 131072).
	//
	// Both are omitted together when serving_config_path is not configured, or
	// when the file cannot be read: an absent field means "this node does not
	// report", which must stay distinguishable from "this node reports MATCH".
	ServingConfigSpecSHA256 string `json:"serving_config_spec_sha256,omitempty"`
	ServingConfigState      string `json:"serving_config_state,omitempty"`
	// Lease is this node's machine-wide GPU lease as gpulease reads it
	// (0.113.16). Published only when a lease is HELD; absent = the card is
	// unreserved (or the node predates the field — a delegator treats both as
	// eligible, exactly as before). A held TEXT lease makes the delegator skip
	// this node and makes this node's own /fleet/dispatch refuse new work with
	// a re-placeable 503 — the node stays up, keeps answering health, finishes
	// what it holds, and is never "dropped" from the fleet for a measurement.
	Lease *LeaseHealth `json:"lease,omitempty"`
	// LeaseExclusive / LeaseDraining are the two lease facts a placement
	// actually needs and `busy` never carried: whether the reservation FENCES
	// the cards (no model may be loaded onto them for its duration) and whether
	// it is still draining the seat. `busy` is a verdict about DECLARED time —
	// it said "spoken for until 14:05" about a box whose cards were quiet, and
	// 47 measured contracts burned 300 s each on it. These two say what the
	// reservation is doing.
	//
	// Read from the same gpulease.Info the lease block is built from, published
	// only while a lease is held and only when true (omitempty): a node with no
	// lease, and every node one release behind, emits a byte-identical payload.
	// Top-level rather than inside `lease` so a reader that wants the fence
	// answer does not have to decode a nested block it otherwise ignores.
	LeaseExclusive bool `json:"lease_exclusive,omitempty"`
	LeaseDraining  bool `json:"lease_draining,omitempty"`
	// Saturation (0.113.18): always published; see SaturationHealth.
	Saturation *SaturationHealth `json:"saturation,omitempty"`
	// Store is the store steward's last status (0.113.16), published only when
	// fleet_store_root is configured.
	Store *storesteward.Status `json:"store,omitempty"`
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
	// a node with the lane off — which includes every pre-0.65 node — emits a
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
	// SeatBudget (0.117.2, register D-46 / H-04) is the completion budget
	// THIS node runs a contract at: a delegator's config does not travel with
	// the contract, so a caller that wants matched budgets across seats — the
	// standard quality instrument — has to read the executing node's. Additive,
	// omitted with the lane.
	SeatBudget *SeatBudgetHealth `json:"seat_budget,omitempty"`
	// SeatRate is the seat's remembered decode rate and cold load from this
	// node's seat-rates store (seatrate, 0.115.21), with the retry floor they
	// imply — what a delegator sizes a RETRY on this seat by, instead of the
	// first attempt's seat (D-46). nil until the seat has a sample.
	SeatRate *SeatRateHealth `json:"seat_rate,omitempty"`
	// SeatLoaded / SeatStarting are what llama-swap's /running says about the
	// agent seat as of the last background refresh: loaded at all, and loaded
	// but still LOADING (llama-swap holds `/upstream/<seat>/…` for the whole
	// load — 4m08s on the 27B TP2 seat — so "starting" is not "ready", register
	// D-92). A delegator can otherwise not tell a warm seat from one that will
	// make its first request pay a cold load.
	//
	// POINTERS, not bare bools: a read that FAILED must stay distinguishable
	// from a seat that is genuinely not loaded (absent ≠ idle, the rule the VRAM
	// snapshot and the reclaim verdict already follow), and `omitempty` on a
	// bool cannot express that. Both are set together from one /running read and
	// both are nil when it could not be read.
	//
	// The read is /running ONLY and never `/upstream/<seat>/…`, which would
	// LOAD an unloaded seat — register C-05, and the measured reason 54 status
	// probes each blocked ~186 s.
	SeatLoaded   *bool `json:"seat_loaded,omitempty"`
	SeatStarting *bool `json:"seat_starting,omitempty"`
	// RecentAgentWallSec is the MEDIAN wall of the last (up to) 8 agent jobs to
	// finish on this node, in seconds — the completion signal a node with no
	// seat_rate sample has no other way to publish (register S-36: without it a
	// fresh box is indistinguishable from a fast one, and placement has nothing
	// to rank on). The median, not the mean, so one 900-second outlier does not
	// redefine the node.
	//
	// Computed from the same job map /fleet/jobs walks — no probe, no clock, no
	// new state. Absent when this node has finished no agent job (a cold node
	// makes no claim about its speed).
	RecentAgentWallSec float64 `json:"recent_agent_wall_sec,omitempty"`
	// QueueWaitEstimateSec (register S-04) is queueWaitEstimateSec over the
	// CURRENT CAPPED backlog (Jobs.CountsCapped, NEVER the all-task-types
	// queue_depth published above — an uncapped media/stt/pipeline job never
	// waits behind max_concurrent_jobs, so mixing it in would inflate the
	// estimate for a node whose agent slots are genuinely idle) and
	// max_concurrent_jobs, using the same recent_agent_wall_sec sample
	// published above. The "queue full" 503's Retry-After header uses the
	// SAME formula, but this field is the RAW, UNBOUNDED value — Retry-After
	// additionally clamps to [5, 300] because it is an HTTP retry contract;
	// this field is a delegator's own placement signal, and a genuine 600s
	// estimate is more useful reported honestly than floored to 300. Reading
	// the two health fields together: absent QueueWaitEstimateSec with a
	// PRESENT RecentAgentWallSec means genuinely 0 (a worker is free right
	// now); both absent means unknown (no wall sample exists yet). 0 (a
	// worker is free, or no recent wall sample) is the honest answer and
	// omitempty hides it, exactly like RecentAgentWallSec's own "a cold node
	// makes no claim" rule. NOT gated to the agent lane: the capped backlog
	// spans every capped task type, and a media-only node with no agent
	// history simply never has a wall sample to estimate from.
	//
	// Not yet consumed by internal/delegate — that lands with the placement
	// release (feat/placement-eta); this field exists so that PR has
	// something to read.
	QueueWaitEstimateSec float64 `json:"queue_wait_estimate_sec,omitempty"`
	// ServedModels is the CACHED roster name list — canonical ids AND every
	// alias (agentResidency.served, from swapclient.Roster.Names) —
	// refreshed on the same TTL/single-flight as AgentResident. Absent/empty
	// = UNKNOWN (cold cache, or the fetch failed) — never a stale list. A
	// later placement gate uses this to check a specific model is actually
	// hosted here, not just that the agent seat is resident; because this
	// list carries aliases too, an agent seat configured by its alias (the
	// normal shape) is found here, not just a seat configured by canonical
	// id.
	ServedModels []string `json:"served_models,omitempty"`
	// VisionModel is the vision lane's seat (0.116.0), published only when the
	// lane is admissible — the same moment "vision" appears in
	// supported_task_types. Additive + omitempty: a node without the lane
	// emits a byte-identical payload.
	VisionModel string `json:"vision_model,omitempty"`
	// ChatLane says POST /fleet/chat will admit here (C-41b), published under
	// the same one-predicate rule as vision_model. It is what a delegator's
	// cascade lane reads to tell a FLEET NODE base from a plain llama-swap
	// base, and therefore whether to route a cascade call through this node's
	// bearer-gated door instead of a port it cannot reach. Additive,
	// lane-gated and omitempty: a node without the lane emits a
	// byte-identical payload.
	ChatLane bool `json:"chat_lane,omitempty"`
	// Tiers is every hardware tier this node is a COMPLETE instance of
	// (config `tiers`, ADR 0039): the composite box is a full blackwell-16
	// and a full blackwell-2x16 as well as the tier it installed as, and a
	// fleet that reads one row per box cannot see that. Additive, lane-gated
	// and omitempty: a plain node emits a byte-identical payload.
	Tiers []string `json:"tiers,omitempty"`
	// Layers is this node's device layers as the delegator must see them:
	// the declared spec of every layer and seat, which seats the roster
	// answers for, and the node's OWN admissibility verdict per layer. The
	// verdict travels because the display-card guards can only be read where
	// the card is; the delegator feeds it to the SAME placement table
	// (placement.FromRows + Decide) and the node re-checks at admission.
	// Built from cached reads only — the roster the residency refresh already
	// fetched, the VRAM snapshot the sampler already holds, the host sample —
	// so publishing it costs no probe and no exec inside the handler.
	Layers []placement.LayerRow `json:"layers,omitempty"`
	// ---- Host CPU/RAM (hostsample) ----
	// Emitted only when opts.Host is set AND its sample is Known. All three
	// carry omitempty, unlike GpuUtilPct/GpuUtilKnown (always-present):
	// there is no companion *Known flag here, so a genuine 0% HostCPUPct
	// with omitempty is acceptable ONLY because HostRAMTotalGb is never 0
	// when known and therefore serves as the presence signal for the group —
	// a reader must check host_ram_total_gb (or the key's mere presence)
	// before trusting host_cpu_pct as a real zero rather than an absent field.
	HostCPUPct     int     `json:"host_cpu_pct,omitempty"`
	HostRAMUsedGb  float64 `json:"host_ram_used_gb,omitempty"`
	HostRAMTotalGb float64 `json:"host_ram_total_gb,omitempty"`
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
	// ONE walk of the job store feeds queue_depth and the split beside it, so
	// the three numbers are consistent by construction rather than by luck —
	// two separate reads could straddle a job's accepted→running transition and
	// publish a queue_depth that is not jobs_queued + jobs_running.
	queued, running := s.jobs.Counts()
	// Admission holds are read ONCE per health request, before the payload is
	// assembled: the published `jobs_admitting` and the saturation numerator
	// that excludes it must be the same number, exactly as queue_depth and its
	// split are one walk of the store. The count is agent-lane-only — a node
	// without the lane has no contract runs to be in admission — and skipping
	// it there also keeps a media-only node off the registry directory.
	admitting := 0
	if s.agentLane && s.admitting != nil {
		admitting = s.admitting()
	}
	// recentWall backs BOTH recent_agent_wall_sec (agent-lane only, below) and
	// queue_wait_estimate_sec (published regardless of lane): one walk of the
	// job store's finished rows feeds both fields, so they can never disagree
	// about what "recent" means.
	recentWall := medianSeconds(s.jobs.FinishedAgentWalls(recentAgentWallSamples))
	maxConcurrentJobs := s.jobs.MaxConcurrent()
	// The estimate's depth is the CAPPED backlog only (review round 1
	// BLOCKER): max_concurrent_jobs bounds only jobs that count against it
	// (Jobs.CountsCapped, the same set runningCappedLocked/IdleSlot measure),
	// so mixing in an uncapped media/stt/pipeline job — which never waits
	// behind that cap — would divide the wrong numerator by the right
	// denominator and inflate the estimate for a fully idle agent lane.
	cappedQueued, cappedRunning := s.jobs.CountsCapped()
	payload := healthPayload{
		NodeID:                s.opts.NodeID,
		SchemaVersion:         1,
		GpuVendor:             s.opts.GpuVendor,
		GpuArch:               s.opts.GpuArch,
		Backends:              s.opts.Backends,
		KVSlot:                s.kvSlotEnabled(),
		Accelerators:          s.opts.Accelerators,
		VramTotalGb:           snap.TotalGiB,
		VramFreeGb:            snap.FreeGiB,
		GpuDevices:            snap.Devices,
		SupportedTaskTypes:    tasks,
		LoadableModelFamilies: families,
		ImageFamilies:         s.imageFamilies,
		ModelFootprints:       fps,
		QueueDepth:            queued + running,
		JobsQueued:            queued,
		JobsRunning:           running,
		MaxConcurrentJobs:     maxConcurrentJobs,
		MaxQueueDepth:         s.opts.Cfg.FleetQueueLimit(),
		HarnessVersion:        s.opts.Version,
		QueueWaitEstimateSec:  math.Round(queueWaitEstimateSec(cappedQueued+cappedRunning, maxConcurrentJobs, recentWall)*100) / 100,
	}
	if s.opts.ServingConfig != nil {
		if sha, state := s.opts.ServingConfig(); state != "" {
			payload.ServingConfigSpecSHA256, payload.ServingConfigState = sha, state
		}
	}
	// GPU utilization: advertise the busiest device's utilization when known.
	// Omitted when no device has published a known utilization — absent ≠ idle.
	for _, d := range snap.Devices {
		if !d.UtilKnown {
			continue
		}
		payload.GpuUtilKnown = true
		if d.UtilPct > payload.GpuUtilPct {
			payload.GpuUtilPct = d.UtilPct
		}
		// The placement figure skips a card reporting display_active, nothing else.
		if snap.DisplayUUIDs[d.UUID] {
			continue
		}
		payload.WorkUtilKnown = true
		if d.UtilPct > payload.WorkUtilPct {
			payload.WorkUtilPct = d.UtilPct
		}
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
	// per TTL; it blocks this handler on llama-swap only when the cached
	// answer is older than agentResidencyMaxStale or never taken, and then
	// for at most residencyWaitBound.
	if s.agentLane {
		payload.AgentEnabled = true
		payload.AgentSeat = s.agentSeat
		payload.AgentCtxTokens = s.opts.Cfg.AgentCtxTokens
		payload.AgentResident = s.agentResident()
		payload.ServedModels = s.servedModels()
		payload.SeatBudget = s.seatBudget()
		payload.SeatRate = s.seatRate()
		// Seat load state and the admission count are agent-lane facts: they
		// describe the seat this node runs contracts on, and a node with the
		// lane off has neither. Both are cached reads — the seat state rides
		// the residency refresh's TTL (and its bounded too-stale wait, above),
		// the admission count its own — so this handler never blocks on a
		// directory and blocks on llama-swap only as agentResident() does.
		if loaded, starting, known := s.seatState(); known {
			payload.SeatLoaded, payload.SeatStarting = &loaded, &starting
		}
		payload.JobsAdmitting = admitting
		if recentWall > 0 {
			payload.RecentAgentWallSec = recentWall
		}
		if s.opts.Cfg.Composite() {
			payload.Tiers = s.opts.Cfg.Tiers
			payload.Layers = s.layerRows(snap)
		}
	}
	if s.visionLane {
		payload.VisionModel = s.opts.Cfg.VisionModel
	}
	// Chat lane (C-41b): the delegator's cascade lane reads `chat_lane` to
	// learn this base is a fleet node it may route through, and served_models
	// to learn WHICH models it may route — so served_models must be published
	// whenever the chat lane is admissible, not only when the agent lane is.
	// agentResident() is the cached read + at most one background refresh per
	// TTL that fills it; calling it here for a chat-only node is the same
	// non-blocking call the agent branch above makes, and the roster fetch it
	// schedules is what populates servedModels().
	if s.chatLane {
		payload.ChatLane = true
		if !s.agentLane {
			s.agentResident()
			payload.ServedModels = s.servedModels()
		}
	}
	// Host CPU/RAM: a cached read of the background hostsample.Sampler, same
	// rule as the VRAM snapshot above — this handler never samples itself.
	if s.opts.Host != nil {
		if h, ok := s.opts.Host(); ok && h.Known {
			payload.HostCPUPct = h.CPUPct
			payload.HostRAMUsedGb = h.RAMUsedGiB
			payload.HostRAMTotalGb = h.RAMTotalGiB
		}
	}
	// GPU lease: a stat + a small file, the same read every acquirer does.
	leasedText := false
	leaseBusy := false
	if s.opts.Lease != nil {
		if info := s.opts.Lease(); info.Held {
			payload.Lease = leaseHealthOf(info, time.Now(), s.opts.Cfg.FleetBusyLeaseSec)
			leasedText = info.Class == gpulease.ClassText
			// Any class, judged by REMAINING time (0.113.27): a long media
			// reservation takes the card just as completely as a text one.
			leaseBusy = payload.Lease.Busy
			// What the reservation is DOING, beside what it declared.
			payload.LeaseExclusive, payload.LeaseDraining = info.Exclusive, info.Draining
		}
	}
	// Saturation (0.113.18): derived from the counters above and the two
	// refusal states dispatch applies to NEW work, so it can never disagree
	// with what a dispatch would actually get.
	// refusing is the node's own verdict that a NEW dispatch would be turned
	// away right now. idle_slot must agree with it: it means "a job handed to
	// me would start immediately", which is false while we refuse. Before
	// 0.113.27 the two were computed independently, so a draining or leased
	// node still advertised an idle slot and sheddable work was dealt to it.
	refusing := s.jobs.Draining() || leasedText || leaseBusy
	sat := saturationOf(queued, running, s.jobs.RunningCapped(), admitting, payload.MaxConcurrentJobs, payload.MaxQueueDepth,
		refusing, s.jobs.IdleSlot() && !refusing)
	payload.Saturation = &sat
	// Store steward: a cached status; Status() itself starts a background
	// tick when the last scan read above the high mark, so a health poll is a
	// turn too, without this handler ever walking a directory.
	if s.opts.Store != nil {
		st := s.opts.Store()
		payload.Store = &st
	}
	writeJSON(w, http.StatusOK, payload)
}

// TenantHeader carries the delegator's tenant id on a dispatch (0.113.18). A
// header rather than an envelope field because dispatchEnvelope is decoded
// with DisallowUnknownFields: a new field would 400 on every node one release
// behind, and a mixed-version fleet is the normal state of a staggered deploy.
const TenantHeader = core.TenantHeader

// maxTenantLen bounds what the store keys its round-robin map on.
const maxTenantLen = 96

// bandOf reads the envelope's `priority` as a scheduling band. The field is
// contract-reserved and was accepted-and-ignored before 0.113.18, so it is read
// LENIENTLY on purpose: absent, null, or anything that is not a JSON integer
// (a string, a float, an object from a media dispatcher with its own meaning)
// is band 0 — the pre-0.113.18 behaviour, never a 400 on a request an older
// node would have taken. An integer is clamped to the three bands.
func bandOf(raw json.RawMessage) int {
	var n int
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
		return BandNormal
	}
	return ClampBand(n)
}

// tenantOf reads TenantHeader: trimmed, bounded, printable-ASCII-only (the
// value keys a map and is echoed in log lines — it must not carry a newline
// or a control byte). Anything else reads as the anonymous tenant.
func tenantOf(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(TenantHeader))
	if len(v) > maxTenantLen {
		v = v[:maxTenantLen]
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}

// SaturationHealth is /fleet/health's `saturation` block (0.113.18): ONE
// number and two booleans derived from the counters published beside it, so
// every reader — the delegator's placement, offload_status, a human with curl
// — agrees on what "this node is saturated" means without each re-deriving it.
//
//	score      max(jobs_running / max_concurrent_jobs, queue_depth / max_queue_depth)
//	           over the limits the node publishes (an unpublished limit
//	           contributes nothing); 0 on an idle node, 1.0 at a ceiling.
//	high       a NEW band-0 dispatch would be refused right now: the backlog is at
//	           max_queue_depth, the node is draining, or its card is under a
//	           text lease. The delegator's ranking demotes a high node exactly
//	           as it demotes one whose queue_depth has reached its ceiling.
//	idle_slot  Jobs.IdleSlot: a sheddable (priority -1) dispatch would be admitted.
//
// Seat-level counters (vLLM running/waiting, KV usage) are deliberately NOT an
// input yet: the node's health handler never probes the seat (a probe of an
// unloaded seat through llama-swap LOADS it), and on this fleet the job counters
// already describe the load the harness itself puts on a seat. The block is the
// seam a cached seat sampler would feed later, without a wire change.
type SaturationHealth struct {
	Score    float64 `json:"score"`
	High     bool    `json:"high"`
	IdleSlot bool    `json:"idle_slot"`
}

// saturationOf computes the block from the same numbers health publishes.
// saturationOf derives the published saturation from the counters and the two
// refusal states dispatch applies to NEW work.
//
// runningCapped, NOT running, is what the concurrency term measures (fixed
// 2026-09-07): maxConcurrent caps only the capped set, which is exactly what
// IdleSlot compares against, so feeding this the all-jobs count let a node
// publish score 1.0 and idle_slot true in the same payload whenever an
// uncapped job was executing. The DEPTH term keeps using the all-jobs count,
// because max_queue_depth bounds every admitted job regardless of capping.
// admitting (register S-17) is subtracted from the concurrency NUMERATOR only:
// a job whose worker is still cordon-waiting, warming or probing holds a slot
// but no card, and 10 of 47 measured lease-timeout rows happened on a box with
// zero cards busy. `high` and `idle_slot` are deliberately UNCHANGED — the slot
// really is taken, a new dispatch really would queue behind it, and those two
// describe admission, not utilization.
func saturationOf(queued, running, runningCapped, admitting, maxConcurrent, maxDepth int, refusing, idleSlot bool) SaturationHealth {
	var score float64
	if maxConcurrent > 0 {
		generating := runningCapped - admitting
		if generating < 0 {
			generating = 0 // an admission hold from another lane; never negative
		}
		score = float64(generating) / float64(maxConcurrent)
	}
	if maxDepth > 0 {
		if s := float64(queued+running) / float64(maxDepth); s > score {
			score = s
		}
	}
	if score > 1 {
		score = 1
	}
	high := refusing || (maxDepth > 0 && queued+running >= maxDepth)
	return SaturationHealth{Score: score, High: high, IdleSlot: idleSlot}
}

// LeaseHealth is the wire shape of a HELD lease in /fleet/health.
type LeaseHealth struct {
	Held   bool   `json:"held"`
	Class  string `json:"class"`
	PID    int    `json:"pid"`
	Reason string `json:"reason,omitempty"`
	Until  string `json:"until"` // RFC3339
	// RemainingSec and Busy are additive (0.113.27). RemainingSec is how much
	// longer the card is spoken for; Busy is THIS NODE'S OWN verdict that the
	// reservation is long enough to make it a non-target (fleet_busy_lease_sec).
	//
	// The verdict travels rather than the threshold so a delegator never has to
	// know a remote box's config, and a node one release behind simply omits
	// both — decoding to false, i.e. exactly the pre-0.113.27 behaviour.
	RemainingSec int  `json:"remaining_sec,omitempty"`
	Busy         bool `json:"busy,omitempty"`
}

// FleetBusyLeaseSecDefault is the remaining-time threshold above which a held
// lease of ANY class makes the node a non-target. Two minutes: longer than any
// ordinary render arbitrated on the node, far shorter than a measurement window
// or a training run.
const FleetBusyLeaseSecDefault = 120

// busyLeaseThreshold resolves fleet_busy_lease_sec: 0/unset = the default,
// negative = the rule is off (text-only refusal, the pre-0.113.27 behaviour).
func busyLeaseThreshold(cfgSec int) (time.Duration, bool) {
	switch {
	case cfgSec < 0:
		return 0, false
	case cfgSec == 0:
		return FleetBusyLeaseSecDefault * time.Second, true
	default:
		return time.Duration(cfgSec) * time.Second, true
	}
}

func leaseHealthOf(info gpulease.Info, now time.Time, cfgSec int) *LeaseHealth {
	h := &LeaseHealth{Held: true, Class: string(info.Class), PID: info.PID, Reason: info.Reason, Until: info.ExpiresAt.UTC().Format(time.RFC3339)}
	if info.ExpiresAt.IsZero() {
		return h
	}
	remaining := info.ExpiresAt.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	h.RemainingSec = int(remaining / time.Second)
	if thr, on := busyLeaseThreshold(cfgSec); on {
		h.Busy = remaining > thr
	}
	return h
}

// textLeased reports whether this node's own card is reserved by a TEXT-class
// lease. Media leases are renders arbitrated on the node itself and do not
// refuse dispatch. A lease that cannot be read is NOT held (fail toward
// serving, the same direction gpuLeaseHeld and LocalBusy take).
// SeatBudgetHealth is the per-step / final completion budget and thinking
// policy the node's agent loop runs at (config agent_max_tokens, the final
// answer at agent.FinalBudgetFor, config agent_thinking or "auto").
type SeatBudgetHealth struct {
	StepTokens  int    `json:"step_tokens"`
	FinalTokens int    `json:"final_tokens"`
	Thinking    string `json:"thinking"`
}

// SeatRateHealth is the seat-rates store entry for the agent seat, plus the
// retry floor it implies at this node's final budget (cold load + one final
// turn; seatrate.MinTurnFor without a re-pack term — the delegator adds that
// from the contract).
type SeatRateHealth struct {
	TokS        float64 `json:"tok_s"`
	ColdLoadSec float64 `json:"cold_load_sec"`
	Samples     int     `json:"samples"`
	MinTurnSec  int     `json:"min_turn_sec"`
}

// seatRateTTL bounds how often health re-reads the seat-rates file: the
// fleet overview polls every few seconds and the store changes once per run.
const seatRateTTL = 30 * time.Second

// seatBudget is what the agent loop on this node runs a contract at.
func (s *Server) seatBudget() *SeatBudgetHealth {
	step := s.opts.Cfg.AgentMaxTokens
	if step <= 0 {
		step = 1024
	}
	thinking := strings.TrimSpace(s.opts.Cfg.AgentThinking)
	if thinking == "" {
		thinking = "auto"
	}
	return &SeatBudgetHealth{StepTokens: step, FinalTokens: seatrate.FinalBudgetFor(step), Thinking: thinking}
}

// seatRate reads the agent seat's remembered rate from the machine-wide
// seat-rates store, cached for seatRateTTL. nil when the store has no sample
// for the seat (or cannot be resolved): the delegator then keeps its own floor.
func (s *Server) seatRate() *SeatRateHealth {
	s.seatRateMu.Lock()
	defer s.seatRateMu.Unlock()
	now := time.Now()
	if now.Sub(s.seatRateAt) < seatRateTTL {
		return s.seatRateCache
	}
	s.seatRateAt = now
	s.seatRateCache = nil
	root, err := gpulease.ResolveStateRoot(s.opts.Cfg.StateDir)
	if err != nil {
		return nil
	}
	store, _ := seatrate.Load(seatrate.Path(root))
	known := store.Get(s.agentSeat)
	if known.TokS <= 0 {
		return nil
	}
	b := s.seatBudget()
	s.seatRateCache = &SeatRateHealth{TokS: known.TokS, ColdLoadSec: known.ColdLoadSec, Samples: known.Samples,
		MinTurnSec: seatrate.MinTurnFor(known.ColdLoadSec, b.FinalTokens, 0, known.TokS)}
	return s.seatRateCache
}

func (s *Server) textLeased() (gpulease.Info, bool) {
	if s.opts.Lease == nil {
		return gpulease.Info{}, false
	}
	info := s.opts.Lease()
	return info, info.Held && info.Class == gpulease.ClassText
}

// concurrencyCapped reports whether a job of this task type counts against
// fleet_max_concurrent_jobs.
//
// THE RULE: the cap exists to protect ONE thing — the shared llama-swap text
// endpoint, where the measured defect was N simultaneous inferences against a
// single serving slot. A task type is exempt when its execution cannot cause
// that, and every exemption below is a fact about how the pipeline runs it, not
// a preference:
//
//   - image-gen / video-gen / audio-gen / run-graph and every configured
//     pipeline route go through Pipeline.acquireMediaLease, which takes the
//     in-process mediaSlot (capacity ONE) and then the machine-wide gpulease
//     ClassMedia. They are already serialized far harder than this cap would
//     serialize them.
//   - stt runs against whisper-server, a different process with a different
//     endpoint. It never touches llama-swap.
//
// Capping those would be both redundant and actively harmful. A media job
// blocked inside takeMediaSlot holds a fleet execution slot while doing NO
// work; with mediaSlot at capacity one, four queued media dispatches would
// occupy all four slots while three of them sit parked — starving the agent
// lane, which is the lane the cap was written to protect. It would also destroy
// media's own designed back-pressure: a media job that cannot get the card
// waits gpu_wait_ms and defers `gpu_busy`, a bounded and well-tested signal a
// job held in `accepted` never reaches.
//
// DEFAULT IS CAPPED, deliberately. An unrecognized (future) task type is
// assumed to contend for the text endpoint, because of the two ways to be
// wrong that one fails loudly — queue latency, then a visible `queue deadline`
// — while the other silently reinstates the exact unbounded-inference defect
// this release exists to remove.
func (s *Server) concurrencyCapped(taskType string) bool {
	switch taskType {
	// animate joined this list 2026-09-07: it runs a ComfyUI render through
	// gpugen under acquireMediaLease exactly as image-gen/video-gen do, never
	// touching the shared text endpoint the cap protects. Capping it made it
	// hold a fleet execution slot while parked in the capacity-1 media slot —
	// verbatim the failure the rule above says the exemption exists to prevent.
	case "image-gen", "video-gen", "animate", "audio-gen", "run-graph", "stt":
		return false
	// accel (0.115.0) drives a loopback accelerator sidecar — Hailo or Coral —
	// and never the llama-swap text endpoint; a 3 ms TPU call parked behind a
	// five-minute digest contract would be the cap protecting nothing.
	case "accel":
		return false
	}
	// Config-driven pipeline routes run through runPipelineJob, which takes the
	// same mediaSlot. Their names are operator-chosen, so they cannot be listed
	// above and must be recognized from config.
	if _, ok := s.opts.Cfg.Pipelines[taskType]; ok {
		return false
	}
	return true
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
	s.admit(w, r, env)
}

// handleVision is the vision lane's ack path (0.116.0): the ONLY thing it
// does differently from handleDispatch is the body — capped from this node's
// vision_max_image_bytes (VisionBodyCap) instead of dispatch's 1 MiB, and
// decoded as a VisionPayload whose job_id rides inside it. The decoded body
// then becomes a "vision" envelope and joins admit: bearer auth first, then
// the known-id re-ack, drain, lease, band and queue gates, then BuildRequest
// (buildVision) and the job store — nothing about admission is re-implemented.
func (s *Server) handleVision(w http.ResponseWriter, r *http.Request) {
	cap := VisionBodyCap(s.opts.Cfg)
	r.Body = http.MaxBytesReader(w, r.Body, cap)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("request body too large (limit %d bytes: vision_max_image_bytes as base64 plus slack)", cap))
			return
		}
		writeError(w, http.StatusBadRequest, "reading vision body: "+err.Error())
		return
	}
	// Strict decode here mirrors dispatch's DisallowUnknownFields envelope
	// decode: a caller mistake is a 400 with the field named, never a job.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var p VisionPayload
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "malformed vision body: "+err.Error())
		return
	}
	s.admit(w, r, dispatchEnvelope{JobID: p.JobID, TaskType: VisionTask, Payload: body})
}

// admit is the shared ack path behind /fleet/dispatch and /fleet/vision: the
// token-gated lanes' auth, the known-job re-ack/409, the drain/lease/band/
// queue refusals, BuildRequest, and the job store's Admit. The two handlers
// differ ONLY in how they read and cap their body; everything a node decides
// about a job happens here, once.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, env dispatchEnvelope) {
	// TOKEN-GATED LANES' AUTH (v1 scope: the agent lane ONLY — the delegation
	// plan's reshape delta 10 — joined by the vision lane in 0.116.0, see
	// tokenGated; media task types skip this block entirely so deployed
	// tokenless media clients stay byte-identical, pinned in auth_test.go).
	// Ordering: the check needs the DECODED task_type, so it cannot precede
	// the body decode; it sits immediately AFTER decode and BEFORE everything
	// else — the job_id check, the known-job re-ack/409 lookup, the drain
	// gate, BuildRequest — so an unauthorized agent caller gets only the auth
	// verdict (401/403), never a validation 400 to probe the envelope with
	// and never a re-ack/409 that discloses job existence or state.
	if tokenGated(env.TaskType) {
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
	// REFUSAL: the MEDIA dispatcher (a different repository) may then
	// re-dispatch the SAME job_id to another node — so that flat 503 could buy
	// a duplicate render fleet-wide for work this node had already done. (This
	// repo's delegator refuses differently since 0.101.0: it re-places the
	// subtask on another node under a FRESH id, and never for a job already
	// acked. Same hazard class, different id — the ordering below is what
	// protects both.) A known job_id is NOT new work, so it is never subject to
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

	// Queue-depth back-pressure, NEW work only — known jobs re-acked above are
	// never refused by it, and result polls don't come through here at all.
	// Checked before BuildRequest so a refused dispatch materializes nothing.
	// Two concurrent dispatches can both pass and overshoot the cap by a few —
	// this is back-pressure against unbounded queue latency, not an exact
	// invariant, and exactness is not worth coupling admission to the jobs
	// mutex.
	//
	// This is the BACKLOG limit, and since 0.100.0 it is ONLY that. Admitted
	// jobs no longer all execute on arrival: fleet_max_concurrent_jobs bounds
	// execution inside the store, so a node whose workers are all busy still
	// ADMITS — busy is not full — and refuses only once accepted+running
	// reaches this cap. The refusal boundary itself is byte-identical to
	// 0.99.0's: the comparison is the same expression over the same
	// QueueDepth() (accepted+running) against the same config key.
	//
	// This 503 is NO LONGER TERMINAL for the delegator in this repo. Until
	// delegator 0.101.0 internal/delegate surfaced any non-202 as an error and
	// the subtask's work was simply not done; it now classifies the refusal by
	// STATUS and re-places a 503 on another eligible node and then on the local
	// seat, bounded (internal/delegate/run.go — replaceableRefusal,
	// placeAndRun). What it still will NOT re-place is a 4xx other than
	// 404/408/409/429, so the `400` BuildRequest can return below still ends
	// that subtask.
	//
	// Widening the backlog rather than the concurrency is STILL the safer half
	// of the split, and now for a sharper reason than "a refused job is lost":
	// a job that WAITS gets done on the node whose seat is already warm, while
	// a refused one pays a fresh placement out of the CONTRACT'S OWN
	// timeout_sec and can still end up nowhere once the re-placement bound is
	// spent.
	// A held TEXT lease reserves this node's card (0.113.16): refuse NEW work
	// with the same re-placeable 503 the queue cap uses, naming the holder and
	// the expiry, so a delegator (any version — 503 has been re-placeable since
	// 0.101.0) puts the subtask on another node instead of loading a reserved
	// card. Known jobs re-acked above and result polls are never refused.
	if info, held := s.textLeased(); held {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("node leased (gpu lease class=%s pid=%d reason=%q until %s): the card is reserved for a measurement; place elsewhere",
				info.Class, info.PID, info.Reason, info.ExpiresAt.UTC().Format(time.RFC3339)))
		return
	}
	// Scheduling keys (0.113.18): the band rides the envelope's `priority`
	// field — contract-reserved since v2, accepted-and-ignored until now, so a
	// pre-0.113.18 node still takes the same bytes — and the tenant rides a
	// header, because the envelope decode rejects unknown FIELDS and a new one
	// would 400 on every older node in a staggered rollout. A caller that sends
	// neither is band 0 in one anonymous tenant: pure arrival order, as before.
	band, tenant := bandOf(env.Priority), tenantOf(r)
	// Shed rule: a sheddable dispatch is admitted only into an IDLE execution
	// slot. Measurement and gate traffic takes capacity nobody is queued for
	// and never queues in front of, or behind, production work; the delegator
	// re-places the 503 on another node (any version — 503 has been
	// re-placeable since 0.101.0) and, with no idle node anywhere, sheds the
	// contract instead of waiting. Checked BEFORE the queue cap on purpose: a
	// node that is merely busy is not full, and "busy" is the state a
	// sheddable job must not add to.
	if band < BandNormal && !s.jobs.IdleSlot() {
		queued, running := s.jobs.Counts()
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("shed (priority %d): no idle execution slot (%d running, %d queued, limit %d); sheddable work takes idle capacity only",
				band, running, queued, s.jobs.MaxConcurrent()))
		return
	}
	if limit := s.opts.Cfg.FleetQueueLimit(); limit > 0 {
		if d := s.jobs.QueueDepth(); d >= limit {
			// Wording matters here more than anywhere else in this file:
			// this string is what an operator actually reads, and it also
			// travels VERBATIM inside the delegator's `placement refused: ...`
			// message when no node takes the subtask.
			//
			// It deliberately says NOTHING about what the caller should do
			// instead, and that silence is the durable fix rather than a third
			// rewrite. It once said "re-dispatch elsewhere" — true of the media
			// dispatcher, false of this repo's delegator at the time, and
			// promising a recovery that did not happen sent people looking for
			// a rebalancer that was never there. Asserting the opposite went
			// stale the moment delegator 0.101.0 began re-placing. This node
			// cannot know which caller it is answering — media dispatcher,
			// delegator, or a human with curl — so it states ITS OWN state and
			// the one lever the operator holds, and leaves the routing to
			// whoever made the request. Both halves stay true whatever any
			// caller does next.
			//
			// Retry-After (register S-04) is ADDITIVE on top of that byte-
			// identical prefix: the node's own recent_agent_wall_sec sizes a
			// wait the delegator can actually honour, so a "queue full" 503 is
			// something to outwait, not a dead end — never sized from
			// seat_rate.min_turn_sec, a different quantity entirely (see
			// retryAfterFor). Sized from the CAPPED backlog only (review
			// round 1 BLOCKER) — `d` above is every job (the correct backlog
			// total this refusal is ABOUT), but max_concurrent_jobs bounds
			// only the capped set, so dividing the all-jobs depth by it would
			// inflate the wait for a node whose agent slots are genuinely
			// idle behind a pile of unrelated uncapped media/stt/pipeline
			// work.
			cappedQueued, cappedRunning := s.jobs.CountsCapped()
			retryAfter, suffix := retryAfterFor(cappedQueued+cappedRunning, s.jobs.MaxConcurrent(), medianSeconds(s.jobs.FinishedAgentWalls(recentAgentWallSamples)))
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("queue full (%d jobs accepted+running, limit %d): retry later, or raise fleet_max_queue_depth (%s)", d, limit, suffix))
			return
		}
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
		// The wall report (register D-116): the executing lane publishes the
		// wall it sized for this contract, and the job record carries it onto
		// every poll of the RUNNING job. Wired for every task type — only the
		// agent lane reports one today, and a lane that reports none leaves
		// the record at 0, which is what a pre-D-116 node published.
		jobID := env.JobID
		ctx = core.WithWallReport(ctx, func(sec int) { s.jobs.SetWall(jobID, sec) })
		// The liveness report (0.131.0): the executing lane publishes the
		// run's progress (last token, tok/s, phase, allowance, ceiling) onto
		// every poll of the RUNNING job, so the delegator can keep polling a
		// producing job past any clock it sized in advance.
		ctx = core.WithProgressReport(ctx, func(p core.LiveProgress) { s.jobs.SetProgress(jobID, p) })
		// Register A-102: stamp the DOOR this call came through so its ledger
		// row is not one of the door-less cascade rows.
		req.Door = dispatchDoor(req.Door)
		res := s.runner.Run(ctx, req)
		if env.TaskType == string(core.TaskAgentRun) && res.OK {
			// The one fact this result proves about the advertised seat —
			// a completed call on it — goes into the residency cache now,
			// so the next health read does not charge a cold load for a
			// seat that just answered. A defer with zero steps proves nothing.
			s.noteAgentResult(res.Data)
		}
		if env.TaskType == VisionTask {
			// The vision caller reads the WHOLE core.Result back (defers
			// included) — see visionJobData; a defer is a done job here.
			return visionJobData(res)
		}
		if res.OK {
			return res.Data, nil
		}
		reason := res.Reason
		if reason == "" {
			reason = "deferred" // Jobs.finish treats "" as success; never let that lie
		}
		return nil, errors.New(reason)
	}

	// An agent dispatch (already authorized above) is admitted with Agent set,
	// so the record carries the marker handleJob's poll auth keys on.
	//
	// OnDropped is the OTHER half of the `defer cleanup()` above. That defer
	// was airtight while Accept was start — every admitted job's closure ran,
	// so every materialization was released. Since 0.100.0 a job can be
	// admitted and then marked terminal WITHOUT its closure ever executing
	// (drain's never-started arm), and the deferred cleanup then never fires.
	// For run-graph that strands an os.CreateTemp file in the OS temp dir that
	// nothing on this machine ever reclaims; for agent and pipeline jobs it
	// strands a directory under pipeline-jobs/ until the next start sweeps it.
	// The store clears the hook at claim, so exactly one of the two paths ever
	// runs the cleanup.
	specModel := env.ModelFamily
	if env.TaskType == string(core.TaskAgentRun) {
		specModel = s.agentSeat
		// A contract dispatched AT A LAYER runs on that layer's seat, so the
		// feed names it rather than the planner default (ADR 0039). This is
		// the DECLARED seat: the authoritative decision — guards, window, the
		// long-seat choice — is made at execution with the live readers
		// (pipeline.runAgentTask), and a refusal there is published on the
		// result's placed block. Admission metadata may not wait on a probe,
		// and a row that says "agent-pool" while the 262k twin holds the
		// cards is the untruth this build exists to end.
		if s.opts.Cfg.Composite() {
			if seat, ok := placement.SeatOnLayer(s.opts.Cfg.Layers, dispatchedLayer(env.Payload)); ok {
				specModel = seat
			}
		}
	}
	spec := AcceptSpec{
		Agent:     env.TaskType == string(core.TaskAgentRun),
		Gated:     env.TaskType == VisionTask,
		Uncapped:  !s.concurrencyCapped(env.TaskType),
		OnDropped: cleanup,
		Task:      env.TaskType,
		Model:     specModel,
		Band:      band,
		Tenant:    tenant,
	}
	if !s.jobs.Admit(env.JobID, spec, run) {
		cleanup() // duplicate/drain refusal: this request's materialized files never run
		view, ok := s.jobs.Get(env.JobID)
		if !ok {
			// Accept refused but the id is absent: drain began between the
			// Draining() check and Accept.
			writeError(w, http.StatusServiceUnavailable, "node draining")
			return
		}
		// Contract refusal semantics: the dispatcher treats ANY non-202
		// dispatch response as a REFUSAL, and the media dispatcher may then
		// re-dispatch the same job_id to another node. So a duplicate for a
		// job in accepted/running — or DONE (the lost-ack + fast-render
		// scenario) — must re-ack 202, or a lost ack would buy a duplicate
		// render fleet-wide; after the re-ack the dispatcher's tracker polls
		// /fleet/jobs/{id} and immediately finds the terminal state with data.
		// Only a previously FAILED job answers non-202: 409 is an explicit
		// refusal, so the dispatcher may legitimately try the failed job on
		// another node. That pays off on this repo's delegator too since
		// 0.101.0 — it classes 409 as re-placeable precisely because the
		// message below says "on this node", and no other node holds that
		// record — though it re-places under a FRESH id rather than re-sending
		// this one.
		if view.State == JobError {
			writeError(w, http.StatusConflict, "job previously failed on this node: "+view.Error)
			return
		}
		writeAck(w, env.JobID) // accepted/running/done: idempotent re-ack, still just one render
		return
	}
	writeAck(w, env.JobID)
}

// MaxJobWaitSec caps `GET /fleet/jobs/{id}?wait=<seconds>`.
//
// TWELVE, and the ceiling is not a taste: it MUST stay below the delegator's
// pollRequestTimeout (15 s, internal/delegate/run.go), which bounds ONE poll
// exchange client-side. A server-side wait at or above it cancels every long
// poll from the client end — the delegator would abandon a connection this node
// was about to answer, and the feature would read as a transport failure rather
// than as the completion event it is. 12 + jobWaitWriteSlack (2 s) = 14 s, one
// second inside that bound, and the pairing is pinned by a test in the external
// test package (which can read the delegator's own source).
const MaxJobWaitSec = 12

// jobWaitWriteSlack is what the long poll adds to its own wait when it extends
// the handler's write deadline: the wait itself, plus room to serialize and
// write the answer after it ends.
const jobWaitWriteSlack = 2 * time.Second

// jobWaitUnit is the second a `wait=` value is measured in. One real second in
// production; a test compresses it so the CAP can be observed without spending
// MaxJobWaitSec of wall clock per assertion. A var for that reason only —
// production never mutates it (the same discipline as the delegator's
// pollSecond).
var jobWaitUnit = time.Second

// jobWaitOf reads `?wait=<seconds>`, clamped to MaxJobWaitSec.
//
// Read LENIENTLY, exactly like the envelope's `priority`: absent, empty,
// negative or unparseable is 0 = NO WAIT, which is the pre-0.127 route byte for
// byte. A caller asking for more than the cap gets the cap rather than a 400 —
// the parameter is a hint about how long the caller is willing to hold a
// connection, and the node's own bound is not the caller's business to satisfy.
func jobWaitOf(r *http.Request) time.Duration {
	q := strings.TrimSpace(r.URL.Query().Get("wait"))
	if q == "" {
		return 0
	}
	n, err := strconv.Atoi(q)
	if err != nil || n <= 0 {
		return 0
	}
	if n > MaxJobWaitSec {
		n = MaxJobWaitSec
	}
	return time.Duration(n) * jobWaitUnit
}

// handleJob is the poll path: the job's current wire state, or 404 for an id
// we never acked (or already evicted).
//
// With `?wait=<seconds>` it is a LONG poll: an already-terminal job answers at
// once, and anything else blocks on the store's terminal broadcast until the
// job finishes or the wait elapses (register S-19). The wait is applied AFTER
// the auth gate, so an unauthorized caller can never hold a connection open.
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	view, ok := s.jobs.Get(id)
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
	// view.Gated (0.116.0) is the vision lane's job: the same rule, the same
	// reason (its data is the caller's image judged in prose).
	if (view.Agent || view.Gated) && s.opts.Cfg.FleetAuthToken != "" && !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if wait := jobWaitOf(r); wait > 0 && !Terminal(view.State) {
		// This handler is now allowed to outlive the blanket WriteTimeout,
		// which Go arms at header-read for every handler alike (register S-09).
		// Without the extension the answer this poll is WAITING for would be
		// cut as it was written.
		s.extendWrite(w, wait+jobWaitWriteSlack, "the job long poll")
		if v, still := s.jobs.WaitTerminal(r.Context(), id, wait); still {
			view = v
		}
	}
	writeJobView(w, http.StatusOK, view)
}

// jobFeedRow is /fleet/jobs's per-job shape: metadata only, NEVER a payload.
// Unlike jobWire (the per-job route), there is no `data` field at all — not
// even omitempty on a nil — because this row type has nowhere to put one.
type jobFeedRow struct {
	ID         string `json:"id"`
	Task       string `json:"task"`
	Model      string `json:"model,omitempty"`
	State      string `json:"state"`
	Agent      bool   `json:"agent,omitempty"`
	AcceptedAt int64  `json:"accepted_at"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	WallMs     int64  `json:"wall_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

// handleJobs is the cluster jobs feed: GET /fleet/jobs?limit=N, newest first,
// capped at 500 rows. The route itself is unauthenticated — id/task/model/
// state/timestamps never carried anything worth gating (see the route
// comment in Handler) — but an AGENT row's `error` string can echo contract
// content (the goal text, a tool-result fragment), unlike a media row's,
// which never does. So this handler applies the SAME bearer check handleJob
// uses on /fleet/jobs/{id}: when a token is configured and the request does
// not carry it, every agent row's `error` is omitted while the rest of the
// row (and every media row, error included) is unchanged. `error` is
// truncated to 200 runes so one runaway error string cannot blow up the feed
// response.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	// authed: true when no token is configured (loopback-trust posture) or
	// the request carries a valid bearer — the same rule handleJob applies
	// per job. Computed once per request, not per row.
	authed := s.opts.Cfg.FleetAuthToken == "" || bearerOK(r, s.opts.Cfg.FleetAuthToken)
	rows := make([]jobFeedRow, 0, limit)
	for _, v := range s.jobs.Recent(limit) {
		errStr := v.Error
		if (v.Agent || v.Gated) && !authed {
			errStr = "" // agent/vision error text may echo caller content; media rows are unaffected
		}
		row := jobFeedRow{
			ID: v.ID, Task: v.Task, Model: v.Model, State: string(v.State), Agent: v.Agent,
			AcceptedAt: v.AcceptedAt.Unix(), Error: truncateRunes(errStr, 200),
		}
		if !v.StartedAt.IsZero() {
			row.StartedAt = v.StartedAt.Unix()
		}
		if !v.FinishedAt.IsZero() {
			row.FinishedAt = v.FinishedAt.Unix()
			if !v.StartedAt.IsZero() {
				row.WallMs = v.FinishedAt.Sub(v.StartedAt).Milliseconds()
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": rows})
}

// truncateRunes cuts s to at most n runes, on a rune boundary — the same
// safety property as intentLedger.dispatched's truncation loop in
// internal/delegate/intent.go (not imported here: fleetnode is a dependency
// of delegate, and importing it back would be a cycle), expressed directly in
// terms of a rune count rather than a byte budget cut back to validity.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
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
	// Progress (0.131.0, liveness walls): the run's last liveness report while
	// it runs. Additive and omitempty, like WallSec below: a delegator that
	// does not read it polls exactly as before.
	Progress          *core.LiveProgress `json:"progress,omitempty"`
	StallAllowanceSec int                `json:"stall_allowance_sec,omitempty"`
	CeilingSec        int                `json:"ceiling_sec,omitempty"`
	// WallSec (register D-116) is the wall the run reported it is executing
	// under, published WHILE THE JOB RUNS so a delegator polling a
	// timeout_auto contract can bound its clock by this node's sized wall
	// rather than by the wire cap. Additive and omitempty: a delegator too
	// old to read it ignores it, and a job whose lane reports no wall
	// publishes a payload byte-identical to before.
	WallSec int `json:"wall_sec,omitempty"`
}

func writeJobView(w http.ResponseWriter, status int, v *JobView) {
	out := jobWire{JobID: v.ID, State: v.State, Data: v.Data, Error: v.Error, WallSec: v.WallSec, Progress: v.Progress}
	if p := v.Progress; p != nil {
		// Top-level twins of the two bounds, for a reader that wants them
		// without descending into progress.
		out.StallAllowanceSec = int(p.AllowanceMs / 1000)
		out.CeilingSec = p.CeilingSec
	}
	writeJSON(w, status, out)
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

// layerRows renders this node's composite layer rows for health: the declared
// layers, which of their seats the cached roster answers for, and each layer's
// own admissibility verdict from the readings the node ALREADY has.
//
// Every input is a cached or syscall-cheap read, because health must answer
// without touching llama-swap or nvidia-smi: the device free-VRAM numbers come
// from the background sampler's snapshot (the same one the payload's
// vram_free_gb comes from), host RAM from the background host sampler, and
// presence from the OS session state. No reader is invented: a missing host
// sample stays nil, and the guard that needed it refuses — which is the whole
// point of evaluating the guards HERE, where the cards are, rather than
// letting a delegator guess.
//
// Occupancy is deliberately NOT published: a cached health read knows the
// roster, not what is loaded, so each seat carries `served` and no `loaded` /
// `inflight` flag. The delegator's table treats unknown occupancy as neutral,
// which is the truth, instead of reading a fabricated "cold" as "nothing to
// evict here".
func (s *Server) layerRows(snap Snapshot) []placement.LayerRow {
	cfg := s.opts.Cfg
	var host *float64
	if s.opts.Host != nil {
		if h, ok := s.opts.Host(); ok && h.Known && h.RAMTotalGiB > 0 {
			free := h.RAMTotalGiB - h.RAMUsedGiB
			host = &free
		}
	}
	pres := placement.ProbePresence(cfg.PresenceMode(), cfg.OperatorIdle())
	rows := placement.RowsFromConfig(cfg, placement.LiveFromReadings(snap.Devices, host, &pres))
	return placement.MarkServed(rows, s.servedModels())
}

// dispatchedLayer reads just the `layer` field out of an agent contract
// payload, for admission metadata. It is deliberately tolerant — a payload the
// decoder will reject a few lines later must not fail here first, and this
// value only names a job-feed row — so a malformed body yields "" and the feed
// keeps the planner seat. The contract's own decode (BuildRequest) is what
// validates the field.
func dispatchedLayer(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var peek struct {
		Layer string `json:"layer"`
	}
	if err := json.Unmarshal(payload, &peek); err != nil {
		return ""
	}
	return peek.Layer
}

// dispatchDoor is the door a dispatched job records (register A-102): "fleet"
// for a job this node was handed with no door of its own, and the delegator's
// OWN door when it named one — a forwarded request belongs to the surface that
// admitted it, not to the hop that ran it. Today no payload shape carries a
// door (BuildRequest builds each request from per-task fields), so every row
// reads "fleet"; the branch exists so adding one to the wire later does not
// silently relabel every delegated call as fleet-originated.
func dispatchDoor(existing string) string {
	if existing == "" {
		return "fleet"
	}
	return existing
}
