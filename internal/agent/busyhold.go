package agent

// The busy hold (ADR 0061, 0.143.0): liveness judges the SEAT, not the
// request.
//
// Until 0.143.0 a run was declared stalled when its OWN request stayed silent
// past the phase allowance, which was sized from one request's rates on an
// uncontended, cool seat. Every other reason a request goes quiet looked the
// same: waiting its turn behind siblings in the engine's queue, time-sharing a
// busy card, being preempted and recomputed by vLLM, or sharing a card whose
// clocks were capped. On 2026-09-29 that misreading failed 85 % of the day's
// agent jobs — 96 stall kills, 41 finished jobs discarded in their re-pack,
// four "decode stalls" filed in one second on one seat — while the engines
// were producing the whole time.
//
// Now, when a request's own allowance runs out, the monitor reads the seat's
// ENGINE (EngineProbe): a work fingerprint that moves whenever the engine
// takes a step for anyone (never merely because a request arrived or was
// aborted), and a token fingerprint that moves only when it PRODUCES.
//
//   - it moved since the last look → the request is waiting its turn: the run
//     enters PhaseQueued and the engine is re-read every EnginePoll, for as
//     long as the engine keeps working (the run's ceiling still bounds it);
//   - it did not move for max(EngineFlat, the waiting phase's own allowance)
//     → the engine itself is wedged: THAT is the stall, filed with the
//     engine's silence and last reading in the reason;
//   - it kept stepping but produced no token for the token bound → a
//     preempt-and-recompute thrash: a stall too, named as such;
//   - it cannot be read → at the first look (no prior evidence) the
//     pre-0.143.0 rule decides; INSIDE a hold one unreadable read is no new
//     evidence (a llama-server answers /slots and /metrics only between
//     batches, so the busier the engine the likelier a read times out) — the
//     hold goes on and the flat bound still ends it, filed as "unreadable",
//     never as "did no work".
//
// A load in progress stays the cold-load hold's (0.140.0), and a post-ready
// hold that runs out reads the engine before it stalls: a run re-issued
// behind its siblings after a swap-in is waiting, not wedged.

import (
	"context"
	"fmt"
	"time"
)

// defaultEnginePoll is how often the busy hold re-reads the engine.
const defaultEnginePoll = 10 * time.Second

// defaultEngineProbeTimeout bounds one engine read. Longer than the poll: a
// llama-server serves /slots and /metrics between batches only, and one
// prompt batch on the slowest fleet seat takes ~34 s.
const defaultEngineProbeTimeout = 45 * time.Second

// EngineReading is one look at the seat's engine.
type EngineReading struct {
	// Loading: llama-swap is loading (or evicting) the seat — the cold-load
	// hold's evidence, with State its words for it.
	Loading bool
	State   string
	// Fingerprint changes whenever the engine takes a step for any request;
	// "" = it could not be formed (the seat is not loaded, or no source).
	Fingerprint string
	// TokenFingerprint changes only when the engine PRODUCES tokens (generated
	// or prompt tokens credited, a slot's decoded / prompt-processed counts).
	// "" = the source has no production counter (the token bound is off).
	TokenFingerprint string
	// Summary is one clause for reasons and status ("vllm-metrics: 4
	// running, 0 waiting, 29 preemptions so far").
	Summary string
}

// EngineProbe reads the seat's engine. A non-nil error means "cannot tell".
type EngineProbe func(ctx context.Context) (EngineReading, error)

func (p StallPolicy) enginePoll() time.Duration {
	if p.EnginePoll > 0 {
		return p.EnginePoll
	}
	return defaultEnginePoll
}

func (p StallPolicy) engineProbeTimeout() time.Duration {
	if p.EngineProbeTimeout > 0 {
		return p.EngineProbeTimeout
	}
	return defaultEngineProbeTimeout
}

// WithEngineProbe installs the busy hold's engine probe (ADR 0061). Without a
// probe, or with StallPolicy.EngineFlat 0, a silent request is judged alone.
func (m *Monitor) WithEngineProbe(p EngineProbe) *Monitor {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.engine = p
	return m
}

func (m *Monitor) engineArmedLocked() bool {
	return m.engine != nil && m.pol.EngineFlat > 0
}

// engineCheckablePhaseLocked: the phases a busy seat can explain. A tool call
// runs off the seat (its own cap governs); a cold-load hold is the load's to
// bound — except its POST-READY part, where the seat is loaded and the request
// may simply be waiting behind siblings re-issued after the same swap-in.
func (m *Monitor) engineCheckablePhaseLocked() bool {
	switch m.phase {
	case PhasePrefill, PhaseDecoding, PhaseRepack, PhaseAdmission, PhaseQueued:
		return true
	case PhaseColdLoad:
		return m.postReady
	}
	return false
}

// engineFlatBoundLocked is how long the engine may do no work while the
// request waits: max(EngineFlat, the waiting phase's own allowance), so a
// long prefill on a seat whose engine shows nothing mid-prefill keeps the
// prefill allowance it always had.
func (m *Monitor) engineFlatBoundLocked() time.Duration {
	resume, pending := m.busyResume, m.busyPending
	if m.phase != PhaseQueued {
		resume, pending = m.phase, m.pending
		if m.phase == PhaseColdLoad {
			resume, pending = m.resume, m.resumePending
		}
	}
	return maxDur(m.pol.EngineFlat, m.pol.Allowance(resume, pending))
}

// engineTokenBoundLocked: how long the engine may keep stepping without
// producing a token for anyone before the run is stalled as a thrash. 0
// (EngineTokenFlat unset) = three flat bounds.
func (m *Monitor) engineTokenBoundLocked() time.Duration {
	if m.pol.EngineTokenFlat > 0 {
		return maxDur(m.pol.EngineTokenFlat, m.engineFlatBoundLocked())
	}
	return 3 * m.engineFlatBoundLocked()
}

// runEngineProbe bounds one engine read (never under a second), so a hung
// metrics endpoint cannot hold the monitor's decision forever.
func runEngineProbe(p EngineProbe, timeout time.Duration) (EngineReading, error) {
	if timeout < time.Second {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return p(ctx)
}

// checkEngineLocked is the busy hold's decision. Called with m.mu held; it
// releases the lock (the probe runs unlocked, like the cold-load probe).
func (m *Monitor) checkEngineLocked() {
	if m.probing {
		// A probe is in flight; let it decide, look again shortly.
		m.timer.Reset(m.pol.enginePoll())
		m.mu.Unlock()
		return
	}
	epoch, last, probe := m.epoch, m.last, m.engine
	m.probing = true
	m.mu.Unlock()
	rd, err := runEngineProbe(probe, m.pol.engineProbeTimeout())
	m.mu.Lock()
	m.probing = false
	if m.stopped || m.cause != nil || m.epoch != epoch || !m.last.Equal(last) {
		// Progress or a phase change arrived during the read: its own timer
		// governs now.
		m.mu.Unlock()
		return
	}
	now := time.Now()
	var hook func(Phase, time.Duration)
	var ph Phase
	var allow time.Duration
	switch {
	case err == nil && rd.Loading && m.pol.ColdLoad > 0:
		// The seat is being loaded (evicted by another model's swap, or the
		// idle unload during a long wait): the cold-load hold bounds it. Its
		// clock starts NOW when the run comes from the busy hold — the hold
		// was the engine working, and must not be charged to the load.
		if m.phase == PhaseQueued {
			m.closeQueuedLocked(now, now)
			m.leaveQueuedLocked(now)
			m.holdAnchor = now
		}
		m.engSum = "seat " + rd.State
		hook, ph, allow = m.enterLoadingLocked(rd.State)
	case err != nil || rd.Fingerprint == "":
		switch {
		case err != nil:
			m.engSum = "unreadable (" + err.Error() + ")"
		case rd.Loading:
			m.engSum = "seat " + rd.State + " (no cold-load hold on this run)"
		default:
			m.engSum = "unreadable (" + rd.Summary + ")"
		}
		if m.phase == PhaseQueued && now.Sub(m.engChangedAt) < m.engineFlatBoundLocked() {
			// Inside a hold, one unreadable read is no new evidence: keep
			// holding; the flat bound (from the last reading that moved) ends it.
			m.engUnreadableSince = firstNonZeroTime(m.engUnreadableSince, now)
			hook, ph, allow = m.enterQueuedLocked(now, false)
			break
		}
		m.fileUnreadableStallLocked(now)
		m.mu.Unlock()
		return
	default:
		first := m.engFP == ""
		moved := !first && rd.Fingerprint != m.engFP
		if first || moved {
			m.engFP, m.engChangedAt = rd.Fingerprint, now
		}
		if first || m.engTokFP == "" || (rd.TokenFingerprint != "" && rd.TokenFingerprint != m.engTokFP) {
			m.engTokFP, m.engTokChangedAt = rd.TokenFingerprint, now
		}
		m.engSum = rd.Summary
		m.engUnreadableSince = time.Time{}
		switch {
		case !first && !moved && now.Sub(m.engChangedAt) >= m.engineFlatBoundLocked():
			// The engine did no work for anyone since engChangedAt.
			if m.phase != PhaseQueued {
				m.enterQueuedLocked(now, false) // so the stall names the phase it waited in
			}
			m.closeQueuedLocked(now, m.engChangedAt)
			m.fileStallLocked()
			m.mu.Unlock()
			return
		case !first && rd.TokenFingerprint != "" && now.Sub(m.engTokChangedAt) >= m.engineTokenBoundLocked():
			// It kept stepping but produced nothing for anyone: a thrash.
			if m.phase != PhaseQueued {
				m.enterQueuedLocked(now, false)
			}
			m.closeQueuedLocked(now, m.engTokChangedAt)
			m.fileStallLocked()
			if se, ok := m.cause.(*StallError); ok {
				se.EngineThrash, se.EngineSilent = true, now.Sub(m.engTokChangedAt)
				se.Allowed = m.engineTokenBoundLocked()
			}
			m.mu.Unlock()
			return
		}
		hook, ph, allow = m.enterQueuedLocked(now, first || moved)
	}
	m.mu.Unlock()
	notify(hook, ph, allow)
}

// fileUnreadableStallLocked ends a run whose engine could not be read: at the
// first look (no prior evidence) or past the flat bound of a hold. It is the
// pre-0.143.0 verdict, named for what it is — never "the engine did no work".
func (m *Monitor) fileUnreadableStallLocked(now time.Time) {
	held := time.Duration(0)
	if m.phase == PhaseQueued {
		held = now.Sub(m.busySince)
		m.closeQueuedLocked(now, m.engChangedAt)
		m.leaveQueuedLocked(now)
	}
	m.fileStallLocked()
	if se, ok := m.cause.(*StallError); ok {
		se.Unreadable = true
		se.Note += "; the engine could not be read: " + m.engSum
		if held > 0 {
			se.Note += fmt.Sprintf(" (held %.0fs in the busy hold, the last reading that moved %.0fs ago)", held.Seconds(), now.Sub(m.engChangedAt).Seconds())
		}
	}
}

// enterQueuedLocked puts the run in (or keeps it in) the busy hold and re-arms
// the timer for the next engine read. It returns the observer hook when the
// run ENTERS the hold or the engine was just seen working (refresh): each such
// report re-stamps the run's last progress with a ROLLING allowance — the flat
// bound plus one poll — so the delegator keeps polling exactly as long as the
// node would, and a node that dies mid-hold is given up within one flat bound,
// never at the run's ceiling. A reading that only confirms nothing changed is
// no progress and publishes nothing.
func (m *Monitor) enterQueuedLocked(now time.Time, refresh bool) (func(Phase, time.Duration), Phase, time.Duration) {
	entered := false
	if m.phase != PhaseQueued {
		if m.phase == PhaseColdLoad {
			// A post-ready hold whose engine works for others: the request waits
			// in the phase it waited in before the load.
			m.busyResume, m.busyPending = m.resume, m.resumePending
			m.warming, m.postReady = false, false
			m.stopProbeLocked()
		} else {
			m.busyResume, m.busyPending = m.phase, m.pending
		}
		m.busySince = now
		m.phase = PhaseQueued
		m.epoch++
		entered = true
	}
	m.allow = m.pol.enginePoll()
	m.timer.Reset(m.allow)
	if !entered && !refresh {
		return nil, m.phase, m.allow
	}
	publish := m.engineFlatBoundLocked() + m.allow
	if c := m.dl.Sub(now) - ceilingMargin; c < publish {
		publish = c
	}
	if publish < m.allow {
		publish = m.allow
	}
	return m.onHold, m.phase, publish
}

// closeQueuedLocked books the hold's contention up to `until` (the last
// moment the engine was seen working) — the flat or thrash tail that ends in
// a stall is not contention — and closes the open interval.
func (m *Monitor) closeQueuedLocked(now, until time.Time) {
	if m.phase != PhaseQueued || m.busyClosed {
		return
	}
	if until.After(now) {
		until = now
	}
	if d := until.Sub(m.busySince); d > 0 {
		m.queuedTotal += d
	}
	m.busyClosed = true
}

// leaveQueuedLocked ends the busy hold and restores the phase the request
// waited in, with that phase's allowance.
func (m *Monitor) leaveQueuedLocked(now time.Time) {
	if m.phase != PhaseQueued {
		return
	}
	if !m.busyClosed {
		m.queuedTotal += now.Sub(m.busySince)
	}
	m.busyClosed = false
	m.phase, m.pending = m.busyResume, m.busyPending
	m.allow = m.pol.Allowance(m.phase, m.pending)
	m.epoch++
}

// endBusyLocked closes the busy bookkeeping when the loop itself moves on (a
// phase change, a tool call): the time held is kept, the baseline dropped.
func (m *Monitor) endBusyLocked(now time.Time) {
	if m.phase == PhaseQueued && !m.busyClosed {
		m.queuedTotal += now.Sub(m.busySince)
	}
	m.busyClosed = false
	m.engFP, m.engTokFP = "", ""
	m.engUnreadableSince = time.Time{}
}

// QueuedTotal is how long the run spent in the busy hold so far: time its
// requests waited on a seat whose engine was working for others. It is the
// contention figure the job record and the ledger carry beside the wall.
func (m *Monitor) QueuedTotal() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.queuedTotal
	if m.phase == PhaseQueued && !m.busyClosed {
		t += time.Since(m.busySince)
	}
	return t
}

func firstNonZeroTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}
