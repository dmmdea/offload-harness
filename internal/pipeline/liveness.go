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
)

// LivenessPolicyFor is THIS seat's stall policy: the admission budget while
// the run is admitted / cold-loaded / probed, the measured prefill and decode
// rates (else the assumed slow-seat values inside the policy), the loop's
// per-tool cap and the re-pack's bound.
func LivenessPolicyFor(cfg config.Config, known seatrate.Seat, admission time.Duration) agent.StallPolicy {
	tokS := known.TokS
	if tokS <= 0 {
		tokS = cfg.AgentSeatTokS
	}
	return agent.StallPolicy{
		Admission:   admission,
		PrefillTokS: known.PrefillTokS,
		TokS:        tokS,
		ToolTimeout: 0, // the loop hands each tool's own cap to the monitor (Loop.dispatch)
		Repack:      agentRepackChatTimeout,
		Floor:       livenessFloor,
		Slack:       livenessSlack,
	}
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
		if p.Phase == string(agent.PhasePrefill) {
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
