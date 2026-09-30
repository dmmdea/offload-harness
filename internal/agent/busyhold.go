package agent

// The busy hold (ADR 0061, 0.143.0): liveness judges the SEAT, not the
// request.
//
// Until 0.143.0 a run was declared stalled when its OWN request stayed silent
// past the phase allowance, which was sized from one request's rates on an
// uncontended, cool seat. Every other reason a request goes quiet looked the
// same: waiting its turn behind siblings in the engine's queue, time-sharing a
// busy card, being preempted and recomputed by vLLM, or sharing a card that
// thermal throttling had cut to 60 % of its clocks. On 2026-09-29 that
// misreading failed 85 % of the day's agent jobs — 96 stall kills, 41
// finished jobs discarded in their re-pack, four "decode stalls" filed in one
// second on one seat — while the engines were producing the whole time.
//
// Now, when a request's own allowance runs out, the monitor reads the seat's
// ENGINE (EngineProbe: a work fingerprint that moves whenever the engine takes
// a step for anyone, never merely because a request arrived):
//
//   - it moved since the last look → the request is waiting its turn: the run
//     enters PhaseQueued and the engine is re-read every EnginePoll, for as
//     long as the engine keeps working (the run's ceiling still bounds it);
//   - it did not move for max(EngineFlat, the waiting phase's own allowance)
//     → the engine itself is wedged: THAT is the stall, filed with the
//     engine's silence and last reading in the reason;
//   - it cannot be read → the pre-0.143.0 rule: the request's silence alone
//     decides, and the reason says the engine was unreadable.
//
// A load in progress stays the cold-load hold's (0.140.0): a reading that says
// the seat is loading hands the run to it.

import (
	"context"
	"time"
)

// defaultEnginePoll is how often the busy hold re-reads the engine.
const defaultEnginePoll = 10 * time.Second

// EngineReading is one look at the seat's engine.
type EngineReading struct {
	// Loading: llama-swap is loading (or evicting) the seat — the cold-load
	// hold's evidence, with State its words for it.
	Loading bool
	State   string
	// Fingerprint changes whenever the engine takes a step for any request;
	// "" = it could not be formed (the seat is not loaded, or no source).
	Fingerprint string
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
// runs off the seat (its own cap governs), and a cold-load hold that ran out
// is a load that never finished.
func (m *Monitor) engineCheckablePhaseLocked() bool {
	switch m.phase {
	case PhasePrefill, PhaseDecoding, PhaseRepack, PhaseAdmission, PhaseQueued:
		return true
	}
	return false
}

// engineFlatBoundLocked is how long the engine may do no work while the
// request waits: max(EngineFlat, the waiting phase's own allowance), so a
// long prefill on a seat whose engine shows nothing mid-prefill (a /slots-only
// llama-server) keeps the prefill allowance it always had.
func (m *Monitor) engineFlatBoundLocked() time.Duration {
	resume, pending := m.busyResume, m.busyPending
	if m.phase != PhaseQueued {
		resume, pending = m.phase, m.pending
	}
	return maxDur(m.pol.EngineFlat, m.pol.Allowance(resume, pending))
}

// runEngineProbe bounds one engine read by the poll cadence (never under a
// second), so a hung metrics endpoint cannot hold the monitor's decision.
func runEngineProbe(p EngineProbe, poll time.Duration) (EngineReading, error) {
	if poll < time.Second {
		poll = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), poll)
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
	rd, err := runEngineProbe(probe, m.pol.enginePoll())
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
		// idle unload during a long wait): that is the cold-load hold's to
		// bound. Leave the busy hold first so the load resumes the phase the
		// request actually waited in.
		if m.phase == PhaseQueued {
			m.leaveQueuedLocked(now)
		}
		m.engSum = "seat " + rd.State
		hook, ph, allow = m.enterLoadingLocked(rd.State)
	case err != nil || rd.Fingerprint == "":
		// Cannot tell: the request's silence alone decides, as before 0.143.0.
		if err != nil {
			m.engSum = "unreadable (" + err.Error() + ")"
		} else {
			m.engSum = "unreadable (" + rd.Summary + ")"
		}
		m.fileStallLocked()
		if se, ok := m.cause.(*StallError); ok {
			se.Note += "; the engine could not be read: " + m.engSum
		}
		m.mu.Unlock()
		return
	case m.engFP == "" || rd.Fingerprint != m.engFP:
		// The first look since the request's last progress (a baseline), or
		// the engine worked since the last look: the request waits its turn.
		m.engFP, m.engChangedAt, m.engSum = rd.Fingerprint, now, rd.Summary
		hook, ph, allow = m.enterQueuedLocked(now)
	default:
		// The engine did nothing since engChangedAt.
		m.engSum = rd.Summary
		if now.Sub(m.engChangedAt) >= m.engineFlatBoundLocked() {
			if m.phase != PhaseQueued {
				m.enterQueuedLocked(now) // so the stall names the phase it waited in
			}
			m.fileStallLocked()
			m.mu.Unlock()
			return
		}
		hook, ph, allow = m.enterQueuedLocked(now)
	}
	m.mu.Unlock()
	notify(hook, ph, allow)
}

// enterQueuedLocked puts the run in (or keeps it in) the busy hold and re-arms
// the timer for the next engine read. The observer hook is returned only on
// entering, so a 20-minute wait is one status event, not one per poll.
func (m *Monitor) enterQueuedLocked(now time.Time) (func(Phase, time.Duration), Phase, time.Duration) {
	entered := false
	if m.phase != PhaseQueued {
		m.busyResume, m.busyPending = m.phase, m.pending
		m.busySince = now
		m.phase = PhaseQueued
		m.epoch++
		entered = true
	}
	m.allow = m.pol.enginePoll()
	m.timer.Reset(m.allow)
	if !entered {
		return nil, m.phase, m.allow
	}
	// What the observer PUBLISHES is not the poll interval: the delegator
	// keeps polling a remote job only while now < last progress + the published
	// allowance + its grace, and the busy hold's whole point is that the run
	// stays alive for as long as the engine works — up to the run's ceiling.
	// Publishing the 10 s poll made the delegator give up ~70 s into a hold
	// the node was right to keep (review finding, 0.143.0): the node would save
	// the run and the delegator would throw it away. The hold publishes the
	// time left to the ceiling instead, the same shape as the cold-load hold.
	publish := m.dl.Sub(now) - ceilingMargin
	if publish < m.allow {
		publish = m.allow
	}
	return m.onHold, m.phase, publish
}

// leaveQueuedLocked ends the busy hold and restores the phase the request
// waited in, with that phase's allowance.
func (m *Monitor) leaveQueuedLocked(now time.Time) {
	if m.phase != PhaseQueued {
		return
	}
	m.queuedTotal += now.Sub(m.busySince)
	m.phase, m.pending = m.busyResume, m.busyPending
	m.allow = m.pol.Allowance(m.phase, m.pending)
	m.epoch++
}

// endBusyLocked closes the busy bookkeeping when the loop itself moves on (a
// phase change, a tool call): the time held is kept, the baseline dropped.
func (m *Monitor) endBusyLocked(now time.Time) {
	if m.phase == PhaseQueued {
		m.queuedTotal += now.Sub(m.busySince)
	}
	m.engFP = ""
}

// QueuedTotal is how long the run spent in the busy hold so far: time its
// requests waited on a seat whose engine was working for others. It is the
// contention figure the job record and the ledger carry beside the wall.
func (m *Monitor) QueuedTotal() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.queuedTotal
	if m.phase == PhaseQueued {
		t += time.Since(m.busySince)
	}
	return t
}
