// Package delegate is the DELEGATOR side of multi-node agent delegation
// (docs/specs/2026-08-16-multi-node-agent-delegation.md, §S3): it turns
// /fleet/health answers into NodeViews and decides PLACEMENT — which node
// runs a delegation contract — through the hard capability gate in gate.go.
// It performs no dispatch itself; the Phase T surfaces (MCP agent_delegate,
// the CLI verb) consume Place's decision and drive the fleet wire.
package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/netguard"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// NodeView is one node's placement-relevant state, built from /fleet/health
// (FetchNodeView) for remotes and from local knowledge for the local node.
type NodeView struct {
	NodeID string
	// HarnessVersion is the node's advertised release (/fleet/health
	// harness_version); empty on a node too old to publish it. Read by
	// doctor's fleet-skew rows (security standard L0), never by placement.
	HarnessVersion string
	AgentEnabled   bool
	AgentSeat      string
	AgentResident  bool
	AgentCtxTokens int
	QueueDepth     int
	// JobsRunning / JobsQueued split QueueDepth. MaxConcurrentJobs is the node's
	// execution limit and MaxQueueDepth its ADMISSION ceiling on QueueDepth —
	// the one and only thing that produces a `503 queue full`.
	//
	// For BOTH limits, 0 means "no usable number": the node publishes 0 for
	// unlimited, and a node too old to publish the field at all also decodes to
	// 0, and this decoder cannot tell those apart. So 0 is read as UNKNOWN
	// everywhere — never as a limit, never as unlimited. Same house rule
	// AgentCtxTokens already follows: unknown is not a capacity.
	//
	// gate.go DOES consult these (capacity-aware placement, 0.101.0): a node
	// whose published ceiling is already met loses to any node that is not
	// provably full, and a node with a provably free execution slot outranks
	// one whose workers are all busy. QueueDepth keeps its own meaning and its
	// own role as the ordering key — this is added on top, not in place of it.
	JobsRunning       int
	JobsQueued        int
	MaxConcurrentJobs int
	MaxQueueDepth     int
	// ServedModels is the node's advertised roster (health served_models).
	// nil/empty = UNKNOWN (a pre-0.113.0 node): never a refusal. When
	// published, the agent seat must be in it — a stronger check than the
	// cached residency flag, and the capability gate PAIR calls "the exact
	// requested model is present".
	ServedModels []string
	// Accelerators is the node's advertised additive-device list (health
	// `accelerators`, ADR 0024). Absent = none. Read by accelremote to pick the
	// node that carries a device this box lacks (Coral Phase B).
	Accelerators []string
	// SeatRate is the node's published seat rate (health `seat_rate`,
	// 0.117.2): the retry floor a RETRY on that node is sized by (D-46).
	// nil = the node published none (older node, or no sample yet) — the
	// delegator then falls back to the first attempt's seat numbers.
	SeatRate *SeatRateView
	// SeatBudget is the node's published completion budget (health
	// `seat_budget`); nil on an older node.
	SeatBudget *SeatBudgetView
	// GpuUtilPct is the busiest device's utilization; GpuUtilKnown is false
	// when the node did not publish it. Read by betterRemote as the LAST key —
	// a tie-breaker, never a primary signal (operator decision 2026-09-03).
	GpuUtilPct   int
	GpuUtilKnown bool
	// WorkUtilPct is the busiest card the harness can actually run a seat on —
	// the node skips a display card it can prove (gpuprobe.DisplayCardUUIDs).
	// GpuUtilPct is the busiest card on the whole box, so it counts the
	// operator's desktop or game; WorkUtilPct does not, and it is what the
	// tie-break should compare. WorkUtilKnown is false on a node that predates
	// the field, and the tie-break then falls back to GpuUtilPct.
	WorkUtilPct   int
	WorkUtilKnown bool
	// LeasedText is true when the node publishes a held TEXT-class GPU lease
	// (health "lease", 0.113.16): its card is reserved for a measurement and the
	// gate treats it as ineligible. Absent (older node) or a media lease decodes
	// to false — a render is arbitrated on the node, never a placement refusal.
	LeasedText bool
	// LeaseHeld is true when the node publishes a held GPU lease of ANY class (health
	// "lease".held). A media client (mediaremote) ranks a node with no held lease ahead of one
	// with a short media lease; placement of agent work keeps reading LeasedText and LeaseBusy.
	LeaseHeld bool
	// MediaRoutes is the node's own verdict on each file-backed media route (health
	// `media_routes`, ADR 0077): CONFIGURED, NOT CONFIGURED or BOUND-BUT-MISSING. It is only
	// meaningful when MediaRoutesKnown: a node that predates the field publishes none, which is
	// UNKNOWN, never "no route is configured".
	MediaRoutes      []MediaRouteView
	MediaRoutesKnown bool
	// LeaseBusy is the node's own "my card is spoken for long enough that you
	// should place elsewhere" verdict (0.113.27), true for a LONG lease of any
	// class. It exists because LeasedText alone left the harness's longest job
	// class — anything holding the MEDIA lease, up to a training run — looking
	// idle, so placement routed work toward the busy box (2026-09-07 audit).
	// False on an older node, which is exactly the previous behaviour.
	LeaseBusy bool
	// LeaseOverdue is true when the node publishes a HELD lease whose declared
	// window has already ended (health lease.overdue, GPU routing P1): the
	// holder is still using the cards, so the declared end says nothing about
	// when they free. It is decoded SEPARATELY from LeaseBusy on purpose. The
	// node also publishes busy=true for such a lease, but a busy non-text lease
	// is a hard exclusion in the gate, and an abandoned lease must rank the node
	// last without making it unroutable for longer than it was before. False on
	// a node one release behind.
	LeaseOverdue bool
	// Leases is every live lease the node published (health leases[], GPU routing P7), each
	// with the cards it sits on. nil means the node published none, which is an older node or
	// no lease at all: the singular fields above are then the whole reading, exactly as before.
	// They are the worst across the leases when this is set, so a reader that ignores Leases is
	// never told less than is true. Read it through leasesHolding, never directly: whether a
	// lease matters depends on the contract and on the node's seats.
	Leases []LeaseView
	// Devices is the node's per-card truth (health gpu_devices[], the nvidia-smi
	// per-device rows): uuid, VRAM total/free, utilisation, display_active. nil
	// when the node publishes none (an older node, or a source that cannot
	// enumerate devices), which is UNKNOWN and never "no free card". Read it
	// through FreeCards / cardTier, never directly.
	Devices []gpuprobe.Device
	// cardsDealt is how many subtasks the deal in progress has already
	// committed to this node's cards. Set only on the copies a deal hands to
	// betterRemote, never on a view that is stored, returned or compared, so it
	// is always zero outside placeAutoRemote's ranking.
	cardsDealt int
	// Saturation* decode health's `saturation` block (0.113.18). SaturationKnown
	// is false on an older node, and an unknown saturation is neither credited
	// nor blamed — the same rule every other capacity field follows. High means
	// the node itself says a new band-0 dispatch would be refused right now;
	// IdleSlot means a sheddable one would be admitted. Score is 0..1.
	SaturationKnown bool
	SaturationScore float64
	SaturationHigh  bool
	IdleSlot        bool
	// Tasks is the node's advertised `supported_task_types` (0.116.0). nil on
	// a node that publishes none. PlaceVision keys on it: a node serves the
	// vision lane exactly when "vision" is listed, so an older node — which
	// never lists it — is never a vision target.
	Tasks []string
	// VisionModel is the node's advertised vision seat (health
	// `vision_model`, 0.116.0) — informational, published beside the result so
	// a caller can see WHICH model judged its image without opening the node.
	VisionModel string
	// VisionTasks is the subset of vqa / ocr / assess_image the node's vision seat
	// serves (health `vision_tasks`). nil = the node publishes none, which means ALL
	// three: an older node, and a node whose seat serves everything, both read as
	// today. Read it through ServesVisionTask, never directly.
	VisionTasks []string
	// TextTasks is the subset of classify / extract the node's text lane serves
	// (health `text_tasks`, 0.154.0). nil = the node publishes none, which here means
	// NONE: the lane is dark unless declared, and an older node never lists "text"
	// either. Read it through ServesTextTask, never directly.
	TextTasks []string
	// STTHQ / STTUploadMaxMB decode health's `stt_hq` and `stt_upload_max_mb` (ADR 0072): whether the
	// node has an hq whisper model and the largest audio upload its door takes, MiB decoded. Both
	// are published only by a node that advertises "stt-upload", so an older node decodes to nil
	// and 0. STTHQ is a POINTER: false (a node with the door and no hq model) must stay distinct
	// from absent. Read them through ServesSTTUpload and PlaceSTT, never directly.
	STTHQ          *bool
	STTUploadMaxMB int
	// Layers is a composite node's advertised device layers (health `layers`,
	// ADR 0039): the spec of every layer and seat, live occupancy, and the
	// node's OWN admissibility verdict per layer. The delegator rebuilds them
	// with placetable.FromRows and runs the SAME placement table over them
	// (remoteDecision) — council R5: one placement rule, no second fit
	// heuristic across layers. nil on a plain node, which keeps today's
	// single-lane gate (AgentCtxTokens) exactly.
	Layers []placetable.LayerRow
	Local  bool
	// JobsAdmitting is the SUBSET of JobsRunning still in admission (cordon,
	// swap pre-flight, warm, coherence probe) — health `jobs_admitting`
	// (0.127). Those jobs hold a capped slot while the card is idle; placement
	// (W-31) reads it to tell "busy" from "loaded-idle". 0 = none, or a node
	// too old to publish it — the pre-0.127 reading either way.
	JobsAdmitting int
	// SeatLoaded / SeatStarting mirror health `seat_loaded`/`seat_starting`
	// (0.127): what the node's last /running read said about the agent seat.
	// POINTERS: nil is UNKNOWN (the read never happened, or failed), and must
	// never be read as false (idle) — the same tri-state rule the VRAM reclaim
	// verdict already follows. SeatStarting true means a load is in progress
	// (not ready even though SeatLoaded may read true too).
	SeatLoaded, SeatStarting *bool
	// LeaseExclusive / LeaseDraining are the two lease facts a placement needs
	// that `LeaseBusy`'s declared-time verdict never carried: whether the
	// reservation FENCES the cards (no load may land on them) and whether it
	// is still draining the seat. Health `lease_exclusive`/`lease_draining`
	// (0.127), top-level, published only while a lease is held and only when
	// true — a node with no lease, and every node one release behind, decodes
	// both false.
	LeaseExclusive, LeaseDraining bool
	// RecentAgentWallSec is the median wall of this node's last finished agent
	// jobs — health `recent_agent_wall_sec` (0.127), the completion signal a
	// node with no seat_rate sample has no other way to publish. 0 = the node
	// has finished no agent job, or predates the field.
	RecentAgentWallSec float64
	// QueueWaitEstimateSec is the node's OWN estimate of how long a new job
	// would sit in its backlog — health `queue_wait_estimate_sec` (PR-6,
	// 0.128+). nil on every node at 0.127: W-11's queueWaitFor then falls
	// back to deriving the same shape from jobs_running/jobs_queued/
	// max_concurrent_jobs/recent_agent_wall_sec. A POINTER because 0 is a
	// genuine "no wait right now" answer and must read differently from
	// "this node does not publish the estimate at all".
	QueueWaitEstimateSec *float64
	// NewJobWaitSec is the node's OWN estimate of how long a job submitted NOW waits for a worker -
	// health `new_job_wait_sec` (ADR 0073), one slot deeper than QueueWaitEstimateSec, which is the
	// wait of the deepest job ALREADY queued. nil on a node that predates it, and on any node while
	// a worker is free (omitempty). publishedStartWait prefers it as-is; without it the older
	// estimate gets the slot added.
	NewJobWaitSec *float64
}

// LeaseView is one live lease of a node, as its health leases[] publishes it.
type LeaseView struct {
	Epoch uint64
	Class string
	// Devices are the cards the lease sits on, as lower-cased GPU UUIDs. Empty is the whole
	// node, and every reader must take it as every card.
	Devices []string
	// Scope says where Devices came from: declared, inferred or whole-node.
	Scope string
	// Until is the declared term end; the zero Time when the lease declares none.
	Until        time.Time
	RemainingSec int
	Busy         bool
	Overdue      bool
	Exclusive    bool
	Draining     bool
	Orphaned     bool
	Stalled      bool
	// Verdict is the node's one word for what the lease is doing (held, held-stalled,
	// held-orphaned, held-overdue).
	Verdict string
}

// VisionTask is the fleet task_type of the vision lane (0.116.0): a node
// lists it in supported_task_types when its vision_model is bound and the lane
// is safely reachable (fleetnode.taskConfiguredFor).
const VisionTask = "vision"

// STTUploadTask is the fleet task_type of the stt upload door (ADR 0072): a node lists it in
// supported_task_types when its whisper model is bound and the door is safely reachable
// (fleetnode.STTUploadAdmissible). The legacy "stt" task, which every node with a whisper model
// lists, takes only a path on the NODE's disk and cannot be sent bytes.
const STTUploadTask = "stt-upload"

// defaultSTTUploadMaxMB is the upload cap a node that lists the door but publishes no
// stt_upload_max_mb is held to: the door's built-in (config.EffectiveSTTUploadMaxBytes).
const defaultSTTUploadMaxMB = 48

// TextTask is the fleet task_type of the text lane (0.154.0): a node lists it in
// supported_task_types only when its tier declares text tasks (config text_tasks) and the
// lane is safely reachable (fleetnode.taskConfiguredFor).
const TextTask = "text"

// ServesSTTUpload reports whether v advertises the stt upload door.
func (v NodeView) ServesSTTUpload() bool {
	for _, t := range v.Tasks {
		if t == STTUploadTask {
			return true
		}
	}
	return false
}

// ServesSTTUploadOf reports whether v can transcribe a file of size bytes: it advertises the
// door, takes a file that large, and has an hq model when hq is asked (a node that does not publish
// stt_hq is not assumed to have one).
func (v NodeView) ServesSTTUploadOf(hq bool, size int64) bool {
	if !v.ServesSTTUpload() {
		return false
	}
	if hq && (v.STTHQ == nil || !*v.STTHQ) {
		return false
	}
	max := v.STTUploadMaxMB
	if max <= 0 {
		max = defaultSTTUploadMaxMB
	}
	return size <= int64(max)<<20
}

// ServesText reports whether v advertises the text lane.
func (v NodeView) ServesText() bool {
	for _, t := range v.Tasks {
		if t == TextTask {
			return true
		}
	}
	return false
}

// ServesTextTask reports whether v serves the text lane AND the given task (classify or
// extract). Unlike the vision lane an absent list is NOT "all": the lane ships dark, so a
// node is a target only when it lists both the lane and the task. An older node (which
// publishes neither) is therefore never picked, and neither is a node that lists the lane
// with an empty task set.
func (v NodeView) ServesTextTask(task string) bool {
	if !v.ServesText() {
		return false
	}
	for _, t := range v.TextTasks {
		if t == task {
			return true
		}
	}
	return false
}

// ServesVision reports whether v advertises the vision lane.
func (v NodeView) ServesVision() bool {
	for _, t := range v.Tasks {
		if t == VisionTask {
			return true
		}
	}
	return false
}

// ServesVisionTask reports whether v serves the vision lane AND the given task
// (vqa, ocr or assess_image). A node that publishes no vision_tasks serves all
// three, so an older node is exactly as eligible as before; one that publishes a
// list serves only what it names — a seat whose runtime cannot do a task (the
// RKLLM runtime cannot run assess_image's grammar) must not be handed it.
func (v NodeView) ServesVisionTask(task string) bool {
	if !v.ServesVision() {
		return false
	}
	if len(v.VisionTasks) == 0 {
		return true
	}
	for _, t := range v.VisionTasks {
		if t == task {
			return true
		}
	}
	return false
}

// fetchNodeViewTimeout is the transport-level backstop for one health GET —
// a caller-supplied ctx deadline still wins when shorter. Health is a cached
// read on the node side (never blocks on llama-swap), so a node that cannot
// answer inside this is down, not busy.
//
// It bounds ONE base. It used to bound the whole fleet probe as well, but
// only by accident of the probe being serial: N unreachable remotes cost N x
// this, on the critical path of every subtask. fetchViews now fans the probe
// out and applies this per goroutine, so the fleet probe costs the SLOWEST
// remote rather than the sum of all of them.
//
// A var, not a const, for one reason: a test needs to compress it, because the
// capacity wait's per-tick bound can only be shown to work when it is provably
// shorter than this. Production never mutates it, and healthClient.Timeout
// below keeps the real 15 s transport backstop whatever a test does.
var fetchNodeViewTimeout = 15 * time.Second

// maxHealthBody bounds the decoded health response. A real payload is a few
// KiB; the cap only exists so a misconfigured base pointing at something
// pathological cannot balloon the delegator.
const maxHealthBody = 1 << 20

// healthClient rides netguard.SafeTransport: the delegation lane may only
// ever reach loopback or the operator's tailnet (never-cloud, ADR 0001), and
// the dial-time gate holds even when a MagicDNS name's resolution drifts —
// the URL-shape check alone cannot promise that.
var healthClient = &http.Client{
	Transport: netguard.SafeTransport(nil),
	Timeout:   fetchNodeViewTimeout,
}

// MediaRouteView mirrors fleetnode.MediaRouteHealth: one media route and its verdict on that node.
type MediaRouteView struct {
	Route  string `json:"route"`
	Engine string `json:"engine"`
	State  string `json:"state"`
}

// MediaRouteConfigured is the state string of a route that can run right now.
const MediaRouteConfigured = "CONFIGURED"

// RouteState reports the node's verdict on a media route. known is false when the node published no
// media_routes at all (an older node) and ALSO when it published a list that omits the route; state is
// then "".
func (v NodeView) RouteState(route string) (state string, known bool) {
	if !v.MediaRoutesKnown {
		return "", false
	}
	for _, r := range v.MediaRoutes {
		if r.Route == route {
			return r.State, true
		}
	}
	return "", false
}

// SeatRateView mirrors fleetnode.SeatRateHealth.
type SeatRateView struct {
	TokS        float64 `json:"tok_s"`
	ColdLoadSec float64 `json:"cold_load_sec"`
	Samples     int     `json:"samples"`
	MinTurnSec  int     `json:"min_turn_sec"`
}

// SeatBudgetView mirrors fleetnode.SeatBudgetHealth.
type SeatBudgetView struct {
	StepTokens  int    `json:"step_tokens"`
	FinalTokens int    `json:"final_tokens"`
	Thinking    string `json:"thinking"`
}

// healthWire is the LOOSE decode of GET /fleet/health — only the fields the
// placement gate consumes. Tolerant on purpose, twice over: unknown health
// fields (VRAM, footprints, future keys) are ignored so staggered node
// deploys never flag-day the delegator, and ABSENT agent fields decode to
// zero values, which the gate reads as ineligible — a pre-delegation node is
// automatically never placed on.
type healthWire struct {
	NodeID         string `json:"node_id"`
	HarnessVersion string `json:"harness_version"`
	QueueDepth     int    `json:"queue_depth"`
	AgentSeat      string `json:"agent_seat"`
	AgentCtxTokens int    `json:"agent_ctx_tokens"`
	AgentResident  bool   `json:"agent_seat_resident"`
	AgentEnabled   bool   `json:"agent_enabled"`
	// Additive (0.100.0). Absent on a pre-0.100.0 node, which decodes to 0 —
	// the same tolerance every other field here relies on.
	JobsRunning       int `json:"jobs_running"`
	JobsQueued        int `json:"jobs_queued"`
	MaxConcurrentJobs int `json:"max_concurrent_jobs"`
	MaxQueueDepth     int `json:"max_queue_depth"`
	// Additive (0.113.0). Absent on a pre-0.113.0 node, which decodes to the
	// zero value — nil/false, both read as UNKNOWN by the gate.
	ServedModels []string `json:"served_models"`
	Accelerators []string `json:"accelerators"`
	GpuUtilPct   int      `json:"gpu_util_pct"`
	GpuUtilKnown bool     `json:"gpu_util_known"`
	// Additive (0.132.2): the placement figure that skips a card reporting
	// display_active.
	WorkUtilPct   int  `json:"work_util_pct"`
	WorkUtilKnown bool `json:"work_util_known"`
	// Additive (0.117.2). nil on a node that publishes neither.
	SeatRate   *SeatRateView   `json:"seat_rate"`
	SeatBudget *SeatBudgetView `json:"seat_budget"`
	// Additive (0.113.16). nil on a node that publishes no lease.
	Lease *struct {
		Held  bool   `json:"held"`
		Class string `json:"class"`
		// Overdue is additive (GPU routing P1): held and past its declared
		// window. Absent on an older node, decoding to false.
		Overdue bool `json:"overdue"`
		// Busy is the NODE's own verdict (0.113.27) that its lease is long
		// enough to make it a non-target, whatever the class. Absent on an
		// older node, decoding to false = the pre-0.113.27 text-only rule.
		Busy         bool `json:"busy"`
		RemainingSec int  `json:"remaining_sec"`
	} `json:"lease"`
	// Additive (GPU routing P7): every live lease with its cards. Absent on an older node and
	// on a node with no lease, decoding to nil = read the singular block above.
	Leases []struct {
		Epoch        uint64   `json:"epoch"`
		Class        string   `json:"class"`
		Devices      []string `json:"devices"`
		Scope        string   `json:"scope"`
		Until        string   `json:"until"`
		RemainingSec int      `json:"remaining_sec"`
		Busy         bool     `json:"busy"`
		Overdue      bool     `json:"overdue"`
		Exclusive    bool     `json:"exclusive"`
		Draining     bool     `json:"draining"`
		Orphaned     bool     `json:"orphaned"`
		Stalled      bool     `json:"stalled"`
		Verdict      string   `json:"verdict"`
	} `json:"leases"`
	// Additive (the node's per-device breakdown, long published for the fleet
	// overview, decoded here by GPU routing P1). Absent on a node whose
	// snapshot source cannot enumerate devices, decoding to nil = unknown.
	GpuDevices []gpuprobe.Device `json:"gpu_devices"`
	// Additive (0.113.18). nil on a node that does not publish saturation.
	Saturation *struct {
		Score    float64 `json:"score"`
		High     bool    `json:"high"`
		IdleSlot bool    `json:"idle_slot"`
	} `json:"saturation"`
	// Additive (0.116.0): the task list every node has always published, now
	// decoded (the vision lane is found in it), and the vision seat name.
	SupportedTaskTypes []string `json:"supported_task_types"`
	VisionModel        string   `json:"vision_model"`
	// Additive (0.153.0): the vision tasks the seat serves. Absent on an older node
	// and on a node whose seat serves all three, decoding to nil = all three.
	VisionTasks []string `json:"vision_tasks"`
	// Additive (0.154.0): the classify / extract tasks the node's text lane serves.
	// Absent on an older node and on any node whose tier declares none (the lane is dark).
	TextTasks []string `json:"text_tasks"`
	// Additive (ADR 0077): the node's media route verdicts. A pointer so an absent key (an older
	// node) stays distinguishable from a published list.
	MediaRoutes *[]MediaRouteView `json:"media_routes"`
	// Additive (ADR 0072): the stt upload door's capability. Absent on an older node.
	STTHQ          *bool `json:"stt_hq"`
	STTUploadMaxMB int   `json:"stt_upload_max_mb"`
	// Additive (0.116.0, ADR 0039). nil on a plain or pre-0.116 node; the ONE
	// row shape fleetnode publishes and offload_status echoes.
	Layers []placetable.LayerRow `json:"layers"`
	// Additive (0.127.0, PR-5 item 1). Absent on an older node, decoding to
	// the zero value — 0/nil/false — which the gate/placement code reads as
	// UNKNOWN, never as a limit or a verdict.
	JobsAdmitting        int      `json:"jobs_admitting"`
	SeatLoaded           *bool    `json:"seat_loaded"`
	SeatStarting         *bool    `json:"seat_starting"`
	LeaseExclusive       bool     `json:"lease_exclusive"`
	LeaseDraining        bool     `json:"lease_draining"`
	RecentAgentWallSec   float64  `json:"recent_agent_wall_sec"`
	QueueWaitEstimateSec *float64 `json:"queue_wait_estimate_sec"`
	NewJobWaitSec        *float64 `json:"new_job_wait_sec"`
}

// FetchNodeView reads one node's /fleet/health into a NodeView (Local=false —
// a fetched view is by definition a remote node). token, when non-empty, is
// sent as a bearer credential; health is open today (capability advertisement
// is not sensitive, and the deployed media dispatcher must keep decoding it
// tokenless), so the parameter is RESERVED for a future health-auth posture
// rather than required now — callers thread the fleet token through so they
// need no signature change on that day.
func FetchNodeView(ctx context.Context, base, token string) (NodeView, error) {
	u := strings.TrimRight(strings.TrimSpace(base), "/") + "/fleet/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return NodeView{}, fmt.Errorf("delegate: health request for %q: %w", base, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return NodeView{}, fmt.Errorf("delegate: health GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBody))
	if err != nil {
		return NodeView{}, fmt.Errorf("delegate: reading health from %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		// The node's error taxonomy is a small JSON envelope — carry it: "503
		// vram snapshot stale" routes very differently from a connection error.
		return NodeView{}, fmt.Errorf("delegate: health GET %s: status %d: %s", u, resp.StatusCode, truncate(body, 256))
	}
	var w healthWire
	if err := json.Unmarshal(body, &w); err != nil {
		return NodeView{}, fmt.Errorf("delegate: health from %s is not JSON: %w", u, err)
	}
	v := NodeView{
		NodeID:         w.NodeID,
		HarnessVersion: w.HarnessVersion,
		AgentEnabled:   w.AgentEnabled,
		AgentSeat:      w.AgentSeat,
		AgentResident:  w.AgentResident,
		AgentCtxTokens: w.AgentCtxTokens,
		QueueDepth:     w.QueueDepth,

		JobsRunning:       w.JobsRunning,
		JobsQueued:        w.JobsQueued,
		MaxConcurrentJobs: w.MaxConcurrentJobs,
		MaxQueueDepth:     w.MaxQueueDepth,
		ServedModels:      w.ServedModels,
		Accelerators:      w.Accelerators,
		SeatRate:          w.SeatRate,
		SeatBudget:        w.SeatBudget,
		GpuUtilPct:        w.GpuUtilPct,
		GpuUtilKnown:      w.GpuUtilKnown,
		WorkUtilPct:       w.WorkUtilPct,
		WorkUtilKnown:     w.WorkUtilKnown,
		LeasedText:        w.Lease != nil && w.Lease.Held && strings.EqualFold(w.Lease.Class, "text"),
		// An overdue lease also publishes busy=true (the clamp to zero used to make
		// it read free), but for the delegator that busy is the OVERDUE flag's
		// to carry: LeaseBusy means a genuinely long lease and is a hard
		// exclusion for a non-text one, which an abandoned lease must not become.
		LeaseBusy:    w.Lease != nil && w.Lease.Held && w.Lease.Busy && !w.Lease.Overdue,
		LeaseOverdue: w.Lease != nil && w.Lease.Held && w.Lease.Overdue,
		LeaseHeld:    w.Lease != nil && w.Lease.Held,
		Leases:       leaseViews(w),
		Devices:      w.GpuDevices,
		Tasks:        w.SupportedTaskTypes,
		VisionModel:  w.VisionModel,
		VisionTasks:  w.VisionTasks,
		TextTasks:    w.TextTasks,
		Layers:       w.Layers,

		STTHQ:          w.STTHQ,
		STTUploadMaxMB: w.STTUploadMaxMB,
		Local:          false,

		JobsAdmitting:        w.JobsAdmitting,
		SeatLoaded:           w.SeatLoaded,
		SeatStarting:         w.SeatStarting,
		LeaseExclusive:       w.LeaseExclusive,
		LeaseDraining:        w.LeaseDraining,
		RecentAgentWallSec:   w.RecentAgentWallSec,
		QueueWaitEstimateSec: w.QueueWaitEstimateSec,
		NewJobWaitSec:        w.NewJobWaitSec,
	}
	if w.MediaRoutes != nil {
		v.MediaRoutes, v.MediaRoutesKnown = *w.MediaRoutes, true
	}
	if w.Saturation != nil {
		v.SaturationKnown = true
		v.SaturationScore = w.Saturation.Score
		v.SaturationHigh = w.Saturation.High
		v.IdleSlot = w.Saturation.IdleSlot
	}
	return v, nil
}

// leaseViews maps the wire leases[] onto LeaseViews; nil when the node published none.
func leaseViews(w healthWire) []LeaseView {
	if len(w.Leases) == 0 {
		return nil
	}
	out := make([]LeaseView, 0, len(w.Leases))
	for _, l := range w.Leases {
		v := LeaseView{
			Epoch: l.Epoch, Class: strings.ToLower(l.Class), Scope: l.Scope, RemainingSec: l.RemainingSec,
			Busy: l.Busy, Overdue: l.Overdue, Exclusive: l.Exclusive, Draining: l.Draining,
			Orphaned: l.Orphaned, Stalled: l.Stalled, Verdict: l.Verdict,
		}
		for _, d := range l.Devices {
			if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
				v.Devices = append(v.Devices, d)
			}
		}
		if t, err := time.Parse(time.RFC3339, l.Until); err == nil {
			v.Until = t
		}
		out = append(out, v)
	}
	return out
}

// probeUnreachable reports whether an error from FetchNodeView is a TRANSPORT
// failure — the health GET never received an HTTP answer at all (dial refused,
// no route, DNS, a reset, or the per-base timeout). That is the one class worth
// a negative cache: it is a statement about the ADDRESS, so re-dialling it
// inside the same Run only spends the next subtask's budget on the same dead
// socket. Measured: 46 delegation rows kept work local because one remote's
// health probe timed out, and in 41 of them that remote held zero jobs.
//
// A 401, 404 or 503, or a body that is not JSON, is deliberately NOT
// unreachable: something answered, the operator's diagnosis is a different one,
// and the node may well answer the next call. A CANCELLED context is not
// unreachable either — that is the caller giving up, never the node.
func probeUnreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var uerr *url.Error
	return errors.As(err, &uerr)
}

// truncate bounds an error-path body excerpt.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
