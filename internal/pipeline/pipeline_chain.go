// Model chain and seat selection: placement, cascade rungs, chain building, tier health and cold-swap handling.

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// live returns the placement table's view of this machine: the injected
// seam when a test set one, otherwise the process-wide memoised Snapshot
// for this box (placement.SharedSnapshot, placement.DefaultSnapshotTTL) so
// the readers are read at most once per window however many decisions the
// window holds — and however many pipelines make them: every in-flight
// contract's in-loop cascade (NewInLoopPipeline / NewRecordlessOffload
// build one Pipeline per contract) reads through the same memo.
func (p *Pipeline) live() placement.Live {
	if p.placementLive != nil {
		return p.placementLive()
	}
	return placement.SharedSnapshot(p.cfg).Live()
}

// cascadePlacement decides where the mechanical cascade runs on a composite
// box (placement row 2: the single layer's router, or the display layer's
// twin when the pair holds the cards and the layer is awake and its guards
// pass). nil on a plain box — the caller then runs the pre-layer chain and
// stamps nothing — and nil on a decision that could place nothing (a
// composite box that declares no router seat), where the chain also runs as
// before: a defer block on a result the rungs then served would be a lie.
func (p *Pipeline) cascadePlacement() *placement.Decision {
	if !p.cfg.Composite() {
		return nil
	}
	dec := placement.Decide(placement.Request{Class: placement.ClassMechanical, RouteKey: "workhorse"}, p.cfg.Layers, p.live())
	if dec.Defer || dec.Layer == "" {
		// Loud once (the warnSeatPin posture): a composite box that declares no
		// router seat is an operator's config gap, and repeating it per call
		// would drown the log the operator needs to see it in.
		warnUnplacedCascade.Do(func() {
			log.Printf("cascade placement: no layer placed the mechanical cascade (%s); running the configured rungs unplaced until the config declares a router seat", dec.Reason)
		})
		return nil
	}
	return &dec
}

// warnUnplacedCascade gates cascadePlacement's config-gap warning to once per
// process.
var warnUnplacedCascade sync.Once

// cascadeRungs is the (triage, workhorse, escalation) trio the chain is built
// from under a placement: the config's own rungs, or — on the display layer —
// the router twin's model_map, with escalation dropped because the display
// layer declares no 26B twin (a cascade that needs escalation there defers as
// today's "all tiers failed" rather than evicting the pair seat from device 0).
func (p *Pipeline) cascadeRungs(dec *placement.Decision) (triage, workhorse, escalation string) {
	if dec == nil || dec.Layer != placement.LayerDisplay {
		return p.cfg.TriageModel, p.cfg.Model, p.cfg.EscalationModel
	}
	twin, ok := p.cfg.LayerSeat(placement.LayerDisplay, placement.RoleRouter)
	if !ok {
		return p.cfg.TriageModel, p.cfg.Model, p.cfg.EscalationModel
	}
	workhorse = twin.ModelMap["workhorse"]
	if workhorse == "" {
		workhorse = dec.Seat
	}
	return twin.ModelMap["triage"], workhorse, ""
}

// rungPlaced is the placed block stamped on the result a cascade rung served:
// the layer decision with Seat = the rung that actually answered (the
// decision names the layer and the router's device pin, not which rung of
// the ladder the input climbed to). nil when there was no decision.
func rungPlaced(dec *placement.Decision, model string) *core.Placed {
	if dec == nil {
		return nil
	}
	pl := dec.Placed
	pl.Seat = model
	return &pl
}

// modelChain returns the ascending-capability tiers for a task. Fast tasks enter
// at the small tier — UNLESS the learned router predicts it will fail on this
// input (Phase 5) or health marked it degraded (Phase 4), in which case the
// entry is bumped to E4B. Tiers whose circuit breaker is OPEN (Phase 3) are
// skipped (routed around). Duplicates collapse; order preserved. On a
// composite box the rungs come from the cascade's placement (cascadeRungs).
func (p *Pipeline) modelChain(task core.TaskType, feat map[string]float64, knnSkip bool) []string {
	return p.modelChainOn(task, feat, knnSkip, p.cascadePlacement())
}

// modelChainOn is modelChain under an already-made placement decision, so Run
// decides once and both the chain and the served-rung stamp read the same
// decision (nil = the configured rungs, exactly the pre-layer chain).
func (p *Pipeline) modelChainOn(task core.TaskType, feat map[string]float64, knnSkip bool, dec *placement.Decision) []string {
	triageModel, workhorse, escalation := p.cascadeRungs(dec)
	var tiers []string
	add := func(m string) {
		if m == "" {
			return
		}
		for _, x := range tiers {
			if x == m {
				return
			}
		}
		if p.breakers != nil && p.breakers.State(m) == "open" {
			return // breaker tripped: route around this tier
		}
		tiers = append(tiers, m)
	}
	if task == core.TaskTriage || task == core.TaskClassify {
		if entry := triageModel; entry != "" && !p.skipSmallEntry(task, entry, feat, knnSkip) {
			add(entry)
		}
	}
	add(workhorse)
	add(escalation)
	if len(tiers) == 0 { // breakers pruned everything — fall back to the workhorse
		tiers = []string{workhorse}
	}
	return tiers
}

// skipSmallEntry decides whether to bypass the small (E2B) entry tier: the
// learned router predicts it won't handle this input, or health flagged it.
func (p *Pipeline) skipSmallEntry(task core.TaskType, entry string, feat map[string]float64, knnSkip bool) bool {
	if p.routerSnap().PreferLargerEntry(string(task), feat) { // nil-safe receiver; trained router wins
		return true
	}
	if knnSkip { // zero-training kNN bridge (only set when the router isn't trained)
		return true
	}
	if ov := p.overridesSnap(); ov != nil {
		for _, d := range ov.Degraded {
			if d == entry {
				return true
			}
		}
	}
	return false
}

// knnPreferLargerEntry consults the zero-training kNN entry-tier pre-filter:
// true => skip the E2B tier and enter larger. It is a BRIDGE before the LR
// router has data — once the router is trained for this task, the router owns the
// decision and the kNN is skipped (no request-path embedding cost). Off unless
// KNNPreFilterEnabled loaded a substrate + embedder. Fail-open: any miss => false.
func (p *Pipeline) knnPreferLargerEntry(task core.TaskType, input string) bool {
	kn, embed := p.knnSnap()
	if kn == nil || embed == nil {
		return false
	}
	if task != core.TaskClassify && task != core.TaskTriage {
		return false
	}
	if p.routerSnap().HasTask(string(task)) { // nil-safe: false when no router yet
		return false // the trained router decides; don't pay the embed
	}
	vec, err := embed(input)
	if err != nil {
		return false
	}
	skip, ok := kn.PreferLargerEntry(string(task), vec, p.cfg.KNNPreFilterK, p.cfg.KNNMinNeighbors, p.cfg.KNNPreFilterThreshold)
	if !ok {
		return false
	}
	return skip
}

// coldSwapIdle is the idle window after which a tier is assumed cold: on the
// swap-exclusive 8GB llama-swap, an unused alias is evicted whenever another
// model loads, and its next call blocks for the whole (re)load. 10 minutes is
// deliberately conservative — a false "cold" only exempts one timeout from
// breaker accounting; a false "warm" just counts a real swap timeout, and the
// 5-fails/10-window threshold absorbs occasional miscounts.
const coldSwapIdle = 10 * time.Minute

// noteTierCall stamps an attempt on model and reports whether that call was
// LIKELY to hit a llama-swap cold swap: the first call to the tier in this
// process, or the first after coldSwapIdle of tier inactivity.
func (p *Pipeline) noteTierCall(model string) bool {
	p.swapMu.Lock()
	defer p.swapMu.Unlock()
	now := time.Now
	if p.nowFn != nil {
		now = p.nowFn
	}
	t := now()
	if p.tierSeen == nil {
		p.tierSeen = map[string]time.Time{}
	}
	last, seen := p.tierSeen[model]
	p.tierSeen[model] = t
	return !seen || t.Sub(last) > coldSwapIdle
}

// breakerFailure reports whether an attempt outcome counts as an infra failure
// for the circuit breaker.
//
// Design note (LO-9, option b — exclude swap-window timeouts from breaker
// accounting): llama-swap QUEUES incoming requests while it loads a model, so
// the only failure shape a cold swap produces on this client is a plain
// whole-request timeout on the FIRST call to an idle tier (there is no
// "model loading" status to detect). Those timeouts mean "the model was still
// loading under GPU contention", not "the tier is sick" — counting them
// tripped the per-tier breakers during the Jul-1 GPU-contention incident and
// routed around healthy tiers for 20s at a time. We therefore exclude exactly
// (likely-cold-swap AND err_class=="timeout"); conn_refused / http_5xx / oom
// still count, and a WARM tier's timeout still counts. This was chosen over
// extending the first call's budget because it never holds a caller hostage
// beyond RequestTimeoutSec and is deterministic to unit-test.
func breakerFailure(errClass string, likelyColdSwap bool) bool {
	return errClass != "" && !(likelyColdSwap && errClass == "timeout")
}

// waitForColdVisionSeat (F49) loads a cold vision seat before the vision request is sent, and
// returns the note a failed call should carry ("" when nothing was waited for).
//
// It reuses the agent door's warm-up (warmSeat): one passthrough request that makes llama-swap
// load the seat and holds until the load is done, preceded by one /running read that makes a
// seat already listed ready cost nothing more. The warm-up passes the GPU-lease fence like every
// other deliberate load, so a render's card is not loaded onto. The wait is bounded by
// vision_coldload_wait_sec; when the load outlasts it the call goes on regardless and the load
// carries on behind the request (the router keeps loading after a client hangs up), so the bound
// limits how long the caller is held, never whether the call is made.
//
// Only this box's own llama-swap is warmed: a model routed to another base (seat_endpoints, a
// cascade lane) is that base's to load, and asking the local router for it would load a model
// here that none of this box's calls use.
func (p *Pipeline) waitForColdVisionSeat(ctx context.Context, model string) string {
	if p.visionColdLoadWait <= 0 || model == "" || p.client == nil || strings.TrimSpace(p.cfg.Endpoint) == "" {
		return ""
	}
	if p.client.BaseFor(model) != strings.TrimRight(p.cfg.Endpoint, "/") {
		return ""
	}
	spent, _, attempted := warmSeat(ctx, p.cfg.Endpoint, model, p.visionColdLoadWait)
	if !attempted {
		return ""
	}
	return fmt.Sprintf(" (the vision seat was cold: waited %.0fs for its load before sending)", spent.Seconds())
}

// classifyErr buckets an infra error for the ledger + circuit breaker (Phase 3).
func classifyErr(err error) string {
	if errors.Is(err, llamaclient.ErrLaneUnavailable) {
		return errClassLaneGone
	}
	// A class gpugen decided structurally (the IGPU_CLASS= line the runner reported, or the "timeout" of a deadline gpugen
	// itself observed) is exact and wins over any wording in the message, which embeds the child's last output: a
	// "comfy_unusable" failure whose text says "the zoom lens timeout" is not an oom. gpugen.ClassifyErr reads it first too.
	var re *gpugen.RunError
	if errors.As(err, &re) && re.Class != "" {
		return re.Class
	}
	// A full volume (0.178.0), by the typed errno or the errno token in the text, never by prose: the
	// image lanes classify with this function, and a batch item that failed on a full drive was
	// recorded as "other" (gpugen.IsDiskFull is the one rule, shared with gpugen.ClassifyErr).
	if gpugen.IsDiskFull(err) {
		return "disk_full"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "out of memory") || strings.Contains(s, "cudamalloc") || strings.Contains(s, "oom"):
		return "oom"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "context canceled"):
		return "timeout"
	case strings.Contains(s, "connection refused") || strings.Contains(s, "econnrefused") || strings.Contains(s, "no such host"):
		return "conn_refused"
	case strings.Contains(s, "llama-server 5"): // "llama-server 5xx: ..."
		return "http_5xx"
	default:
		return "other"
	}
}

// loadThresholds reads per-task conformal margin thresholds written by
// `local-offload calibrate`. Missing/unparseable => nil (use the config constant).
func loadThresholds(path string) map[string]float64 {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]float64
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// loadOverrides reads health-derived per-tier timeouts + degraded list (Phase 4).
func loadOverrides(path string) *tierOverrides {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var o tierOverrides
	if json.Unmarshal(b, &o) != nil {
		return nil
	}
	return &o
}

// maybeHeal (Phase 7) fires a single warmup request to force llama-swap to
// reload a tier whose breaker just tripped. Rate-limited per tier, opt-in
// (cfg.AutoHeal), off the request path (goroutine). A consequential-but-bounded
// recovery: one ping, ≤ once/60s/tier.
func (p *Pipeline) maybeHeal(tier string) {
	p.healMu.Lock()
	if time.Since(p.lastHeal[tier]) < 60*time.Second {
		p.healMu.Unlock()
		return
	}
	p.lastHeal[tier] = time.Now()
	p.healMu.Unlock()
	go func() {
		hctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = p.client.Generate(hctx, llamaclient.GenRequest{Model: tier, User: "ok", MaxTokens: 1}) // tiny warmup
	}()
}
