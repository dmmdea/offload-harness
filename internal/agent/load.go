package agent

import "time"

// Load (ADR 0066, register C-66): how many requests share the seat.
//
// The measured prefill rate is the rate of ONE request, and every later stall
// allowance is sized from it. A seat that batches four requests gives each of
// them about a quarter of it, so the allowance printed in a stall reason —
// "2357 tok / 281 tok/s x 1.5 + 30s" — was arithmetic for a seat the request did
// not have, and the same request timed under concurrency then taught the store a
// rate the seat never has alone (one fleet seat's prefill_tok_s swung 255-1321 tok/s in one
// day). Two changes keep it honest:
//
//   - the allowance a prefill or a re-pack starts under is sized for the load the
//     run sees (StallPolicy.AllowanceLoad), and the reason prints it;
//   - a run that ever saw the seat shared is not solo, and what it timed is not
//     the seat's rate (Monitor.PeakLoad; the pipeline folds only solo runs in).
//
// The load comes from two places. WithLoad installs a sampler the pipeline backs
// with this box's run registry (the other runs on the seat, read when a prefill
// or a re-pack begins); the engine's own running + waiting — read by the busy
// hold, and once at each call's first delta when the registry saw nobody else —
// also counts, and is the only source that sees a peer this box does not know of
// (a cascade call, another process). Neither is required: without them every
// allowance is exactly what it was, and the run's PEAK stays 0 — "never
// looked", which is not the same as solo.

// WithLoad installs the load sampler: how many requests share the seat right
// now, this run's included (0 or less reads as 1). It is called when a prefill
// or a re-pack begins, outside the monitor's lock, and must be cheap and safe to
// call from the loop's goroutine.
func (m *Monitor) WithLoad(fn func() int) *Monitor {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadFn = fn
	return m
}

// sampleLoad is the load a phase that is about to begin is sized with: sampled
// for a prefill or a re-pack (the two phases whose allowance scales), 1 for
// every other phase and when no sampler is installed. sampled says whether a
// sampler actually answered: only then is the number an observation (a solo 1)
// and not a default.
func (m *Monitor) sampleLoad(ph Phase) (n int, sampled bool) {
	if ph != PhasePrefill && ph != PhaseRepack {
		return 1, false
	}
	m.mu.Lock()
	fn := m.loadFn
	m.mu.Unlock()
	if fn == nil {
		return 1, false
	}
	if n := fn(); n > 1 {
		return n, true
	}
	return 1, true
}

// SampleLoad reads the load sampler once, outside the lock, and folds it into
// the run's peak. The loop calls it at each model call's first delta: the end of
// the prefill window that began with Phase's own sample, so a peer that arrived
// while this request was prefilling is seen too.
//
// When the sampler saw nobody else (or there is none) and an engine probe is
// installed, the engine's own gauges are read too, off the caller's goroutine —
// this runs on the stream reader, which an engine that answers only between
// batches must not stall — with at most one such read in flight. The registry
// knows only this box's runs; the engine's running + waiting counts every request
// on the seat, so a peer the registry cannot see still makes the run not solo.
func (m *Monitor) SampleLoad() {
	n, sampled := m.sampleLoad(PhasePrefill)
	m.mu.Lock()
	if sampled && n > m.loadPeak {
		m.loadPeak = n
	}
	probe := m.engine
	look := probe != nil && m.loadDone == nil && !m.stopped && m.loadPeak <= 1
	var done chan struct{}
	if look {
		done = make(chan struct{})
		m.loadDone = done
	}
	m.mu.Unlock()
	if look {
		go m.readEngineLoad(probe, done)
	}
}

// readEngineLoad folds one engine reading's running + waiting into the run's peak.
// A reading that cannot be had, or has no gauges, is no observation.
func (m *Monitor) readEngineLoad(probe EngineProbe, done chan struct{}) {
	rd, err := runEngineProbe(probe, m.pol.engineProbeTimeout())
	m.mu.Lock()
	m.loadDone = nil
	if err == nil && !m.stopped {
		m.observeLoadLocked(rd.Load())
	}
	m.mu.Unlock()
	close(done)
}

// SettleLoad waits, for at most max, for the engine read SampleLoad started to
// land. The read is asynchronous so a slow engine never holds the stream reader;
// a caller about to act on PeakLoad (the run is over, the store is about to hear
// from it) settles first, or a short run could end before its own witness spoke.
//
// It reports whether the witness has spoken: true when no read was in flight or
// it landed, false when max ran out with the read still pending. A caller that
// gets false must treat the run's load as unknown, not as whatever the registry
// said: the read is pending because the engine answers only between batches, which
// is when peers are most likely, so "the registry saw nobody" is the one answer
// that cannot be trusted then. The cost is a sample skipped on a seat whose
// metrics are chronically slow, never a rate moved by a shared one.
func (m *Monitor) SettleLoad(max time.Duration) bool {
	m.mu.Lock()
	done := m.loadDone
	m.mu.Unlock()
	if done == nil {
		return true
	}
	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// observeLoadLocked folds one engine reading's running + waiting into the run's
// PEAK: a run that ever saw the seat shared is not solo. 0 (a reading with no
// gauges) is no observation. It does not touch the load the flat bound is sized
// with (engLoad): that follows the engine's work, not its arrivals.
func (m *Monitor) observeLoadLocked(n int) {
	if n < 1 {
		return
	}
	if n > m.loadPeak {
		m.loadPeak = n
	}
}

// loadForBoundLocked is the load the busy hold's flat bound is sized with on an
// engine that cannot see a prefill: the engine's own running + waiting at the last
// reading whose fingerprint MOVED (or at its first look) when it had gauges, else
// the load the phase started under. Never the latest reading — see
// engineFlatBoundLocked.
func (m *Monitor) loadForBoundLocked() int {
	if m.engLoad > 0 {
		return m.engLoad
	}
	return m.load
}

// PeakLoad is the highest number of requests this run ever saw sharing the seat
// (its own included): 1 = the run had the seat to itself every time it looked,
// 0 = it never looked (no sampler answered and no engine reading carried gauges:
// nothing is known, so nothing may be assumed — a caller must not read 0 as solo).
// A run with PeakLoad > 1 is not solo: its timings are what a shared seat gives
// one request, not the seat's rate.
func (m *Monitor) PeakLoad() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadPeak
}
