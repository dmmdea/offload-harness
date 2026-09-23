package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// Liveness walls (0.131.0, ADR 0055). Until 0.130.x a contract ran under ONE
// number sized before the run (D-03's auto wall) and enforced as a context
// deadline: a 27B seat streaming its 949th token at 984 s died at the 900 s
// cap exactly like a hung engine would have, and the ledger could not tell
// them apart (2026-09-20, three producing jobs killed in one afternoon). Now
// the wall is the EXPECTATION (still reported as wall_sec, still what the
// delegator sizes from); what ends a run is a STALL — no streamed delta, tool
// call or phase change inside the seat's dynamic allowance — or a safety
// CEILING far above the estimate. The two are filed under different classes:
// a stall is the seat's health (infrastructure), a ceiling is sizing (budget).

var (
	// livenessFloor is the least stall allowance in any phase: a seat at
	// 1 tok/s still emits a delta a second, so a minute of silence while
	// decoding is a seat that stopped. Tests compress it.
	livenessFloor = 60 * time.Second
	// livenessSlack pads the prefill estimate and a tool's own cap.
	livenessSlack = 30 * time.Second
	// ceilingFloorSec / ceilingCapSec bound the ceiling; tests compress them.
	ceilingFloorSec = core.AgentCeilingSecFloor
	ceilingCapSec   = core.AgentCeilingSecCap
	// coldLoadCeilingFloor is the least time a seat may spend LOADING while a
	// run's request waits for its first byte (0.140.0). Ten minutes is
	// llama-swap's healthCheckTimeout on the reference boxes: past it
	// llama-swap gives up on the load itself. Tests compress it.
	coldLoadCeilingFloor = 10 * time.Minute
	// coldLoadPoll is how often the monitor reads /running while a request
	// waits for its first byte. Tests compress it.
	coldLoadPoll = 5 * time.Second
	// postReadyFloor is the least time a request may stay silent AFTER its
	// seat reads ready (a load the probe saw, or the admission warm-up): the
	// engine's first completion after a load is slower than a warm one, but
	// only the `starting` part of a load gets the cold-load ceiling. The bound
	// is max(this, 2 x the waiting phase's own allowance). Tests compress it.
	postReadyFloor = 120 * time.Second
)

// LivenessPolicyFor is THIS seat's stall policy: the admission budget while
// the run is admitted / cold-loaded / probed, the measured prefill and decode
// rates (else the assumed slow-seat values inside the policy), the loop's
// per-tool cap, the re-pack's bound and the cold-load ceiling.
func LivenessPolicyFor(cfg config.Config, known seatrate.Seat, admission time.Duration) agent.StallPolicy {
	tokS := known.TokS
	if tokS <= 0 {
		tokS = cfg.AgentSeatTokS
	}
	coldLoad, basis := coldLoadCeiling(known)
	return agent.StallPolicy{
		Admission:     admission,
		PrefillTokS:   known.PrefillTokS,
		TokS:          tokS,
		ToolTimeout:   0, // the loop hands each tool's own cap to the monitor (Loop.dispatch)
		Repack:        agentRepackChatTimeout,
		Floor:         livenessFloor,
		Slack:         livenessSlack,
		ColdLoad:      coldLoad,
		ColdLoadBasis: basis,
		ColdLoadPoll:  coldLoadPoll,
		PostReady:     postReadyFloor,
	}
}

// coldLoadCeiling bounds one seat load observed mid-run (0.140.0): twice the
// seat's measured cold load (seat-rates.json, the slowest of the recent
// loads), never under coldLoadCeilingFloor, never above the run's ceiling
// cap. The load wait is bounded, never open-ended: past it the run defers
// with a cold-load stall.
func coldLoadCeiling(known seatrate.Seat) (time.Duration, string) {
	d, basis := coldLoadCeilingFloor, fmt.Sprintf("the %.0fs floor", coldLoadCeilingFloor.Seconds())
	if known.ColdLoadSec > 0 {
		basis = fmt.Sprintf("max(%.0fs floor, 2 x %.0fs measured cold load)", coldLoadCeilingFloor.Seconds(), known.ColdLoadSec)
		if m := time.Duration(2 * known.ColdLoadSec * float64(time.Second)); m > d {
			d = m
		}
	}
	if c := time.Duration(ceilingCapSec) * time.Second; c > 0 && d > c {
		d, basis = c, basis+fmt.Sprintf(", capped at %.0fs", c.Seconds())
	}
	return d, basis
}

// ClampColdLoadToRun keeps the cold-load ceiling inside the run's own
// ceiling (review finding, PR #458): a hold that outlived the run would be
// filed as the run ceiling (budget) instead of a cold-load stall
// (infrastructure). The monitor also clamps the live timer; this makes the
// reason's arithmetic say so.
func ClampColdLoadToRun(pol *agent.StallPolicy, ceilingSec int) {
	c := time.Duration(ceilingSec) * time.Second
	if c > 0 && pol.ColdLoad > c {
		pol.ColdLoad = c
		pol.ColdLoadBasis += fmt.Sprintf(", clamped to the run's %ds ceiling", ceilingSec)
	}
}

// seatLoadProbe is the monitor's view of whether llama-swap is LOADING the
// run's seat (0.140.0). A request whose seat is `starting` — a cold start
// after the idle unload, or a reload after another model's swap evicted the
// seat between two steps (2026-09-23: whisper-stt evicted the 3-card seat
// after step 1, and the re-issue's 60 s prefill clock ran out inside the
// ~180 s reload) — gets no byte until the load finishes, so its silence is a
// cold load, not a stall.
//
// loading is reported only on POSITIVE evidence of a load in progress, in
// llama-swap's own vocabulary (/running `state`: stopped | starting | ready |
// stopping | shutdown):
//   - the seat's own row is `starting` or `stopping` (a stopping seat is being
//     evicted, and the waiting request will load it again);
//   - the seat is absent (its alias resolved) while some other row is
//     `starting` or `stopping`: a swap is in progress on the endpoint.
//
// ABSENCE alone is not evidence (review finding, PR #458): a removed seat, a
// renamed alias or a restarted llama-swap answering `{"running":[]}` keeps
// the normal stall clock. An unreadable /running, or an absent seat whose
// alias could not be resolved, is "cannot tell" (an error).
func seatLoadProbe(endpoint, seat string) agent.SeatProbe {
	if endpoint == "" || seat == "" {
		return nil
	}
	sc, err := swapclient.New(endpoint, admissionPoll)
	if err != nil {
		return nil
	}
	var mu sync.Mutex // the matcher latches state; the monitor may probe from two timers
	m := newSeatMatcher(endpoint, seat)
	return func(ctx context.Context) (bool, string, error) {
		mu.Lock()
		defer mu.Unlock()
		rows, rerr := sc.Running(ctx)
		if rerr != nil {
			return false, "", rerr
		}
		find := func() (string, bool) {
			for _, r := range rows {
				if m.matches(r.ID) {
					return r.State, true
				}
			}
			return "", false
		}
		st, ok := find()
		if !ok && m.resolve(ctx) {
			st, ok = find()
		}
		if ok {
			return swapInProgress(st), st, nil
		}
		if !m.resolved {
			return false, "", fmt.Errorf("seat %s is not listed on /running and its alias could not be resolved", seat)
		}
		for _, r := range rows {
			if swapInProgress(r.State) {
				return true, "not resident; " + r.ID + " " + r.State + " (a swap is in progress)", nil
			}
		}
		return false, "not resident, nothing loading", nil
	}
}

// swapInProgress: the llama-swap states that mean a model is being loaded
// or evicted right now.
func swapInProgress(state string) bool {
	return state == "starting" || state == "stopping"
}

// stallOf is the monitor's cause when it is a stall, else nil.
func stallOf(m *agent.Monitor) *agent.StallError {
	if m == nil {
		return nil
	}
	var se *agent.StallError
	if errors.As(m.Cause(), &se) {
		return se
	}
	return nil
}

// ceilingOf is the monitor's cause when it is the ceiling, else nil.
func ceilingOf(m *agent.Monitor) *agent.CeilingError {
	if m == nil {
		return nil
	}
	var ce *agent.CeilingError
	if errors.As(m.Cause(), &ce) {
		return ce
	}
	return nil
}

// ceilingReason is the budget-defer text when the run's deadline passed: the
// ceiling's own arithmetic when the monitor filed one, else the parent's
// deadline (the delegator's, never this node's) in the pre-0.131.0 words.
func ceilingReason(m *agent.Monitor, wallSec int) string {
	if m != nil {
		var ce *agent.CeilingError
		if errors.As(m.Cause(), &ce) {
			return ce.Error()
		}
	}
	return fmt.Sprintf("wall timeout after %ds (the caller's deadline, not this node's ceiling)", wallSec)
}

// progressObserver fans the loop's RunObserver events out to the run
// registry AND to the fleet job record (core.ReportProgress), so a delegator
// polling /fleet/jobs/{id} sees the same liveness a local status reader does.
// Reports are throttled to one per progressReportEvery: the job store's mutex
// must never see one write per streamed token.
type progressObserver struct {
	ctx        context.Context
	inner      *gpuactivity.Handle
	ceilingSec int
	mu         sync.Mutex
	p          core.LiveProgress
	lastReport time.Time
}

const progressReportEvery = time.Second

func newProgressObserver(ctx context.Context, inner *gpuactivity.Handle, ceilingSec int) *progressObserver {
	return &progressObserver{ctx: ctx, inner: inner, ceilingSec: ceilingSec}
}

func (o *progressObserver) OnStep(step, tokensOut int) {
	o.inner.OnStep(step, tokensOut)
	o.update(func(p *core.LiveProgress) { p.Step = step; p.TokensOut = tokensOut }, true)
}

func (o *progressObserver) OnPhase(phase string) { o.inner.OnPhase(phase) }

func (o *progressObserver) OnProgress(tokensOut int) {
	o.inner.OnProgress(tokensOut)
	o.update(func(p *core.LiveProgress) {
		now := time.Now().UnixMilli()
		if d := tokensOut - p.TokensOut; d > 0 && p.LastProgressMs > 0 {
			if dt := float64(now-p.LastProgressMs) / 1000; dt > 0 {
				inst := float64(d) / dt
				if p.TokS == 0 {
					p.TokS = inst
				} else {
					p.TokS = 0.2*inst + 0.8*p.TokS
				}
			}
		}
		if tokensOut > p.TokensOut {
			p.TokensOut = tokensOut
		}
		if p.Phase == string(agent.PhasePrefill) || p.Phase == string(agent.PhaseColdLoad) {
			p.Phase = string(agent.PhaseDecoding)
		}
		p.LastProgressMs = now
	}, false)
}

func (o *progressObserver) OnAllowance(phase string, allowance time.Duration) {
	o.inner.OnAllowance(phase, allowance)
	o.update(func(p *core.LiveProgress) {
		p.Phase, p.AllowanceMs, p.LastProgressMs = phase, allowance.Milliseconds(), time.Now().UnixMilli()
	}, true)
}

// update applies fn and reports when forced (a step or phase change) or when
// progressReportEvery has passed since the last report.
func (o *progressObserver) update(fn func(*core.LiveProgress), force bool) {
	o.mu.Lock()
	fn(&o.p)
	o.p.CeilingSec = o.ceilingSec
	now := time.Now()
	report := force || now.Sub(o.lastReport) >= progressReportEvery
	if report {
		o.lastReport = now
	}
	snap := o.p
	o.mu.Unlock()
	if report {
		core.ReportProgress(o.ctx, snap)
	}
}

// CeilingFor is the safety ceiling for a run whose expectation is wallSec:
// max(3 x estimate, 2 x wall, the floor), never above the cap. An estimate
// of 0 (no rate known yet) falls back to the wall alone.
func CeilingFor(wallSec int, est seatrate.Estimate) int {
	c := ceilingFloorSec
	if v := 3 * est.TotalSec; v > c {
		c = v
	}
	if v := 2 * wallSec; v > c {
		c = v
	}
	if c > ceilingCapSec {
		c = ceilingCapSec
	}
	return c
}
