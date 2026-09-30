package agent

// Load (ADR 0066, register C-66, 0.144.0): how many requests share the seat.
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
// or a re-pack begins); the busy hold's engine reads carry the engine's own
// running + waiting. Neither is required: without them the load is 1 and every
// allowance is exactly what it was.

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
// every other phase and when no sampler is installed.
func (m *Monitor) sampleLoad(ph Phase) int {
	if ph != PhasePrefill && ph != PhaseRepack {
		return 1
	}
	m.mu.Lock()
	fn := m.loadFn
	m.mu.Unlock()
	if fn == nil {
		return 1
	}
	if n := fn(); n > 1 {
		return n
	}
	return 1
}

// SampleLoad reads the load sampler once, outside the lock, and folds it into
// the run's peak. The loop calls it at each model call's first delta: the end of
// the prefill window that began with Phase's own sample, so a peer that arrived
// while this request was prefilling is seen too.
func (m *Monitor) SampleLoad() {
	n := m.sampleLoad(PhasePrefill)
	m.mu.Lock()
	if n > m.loadPeak {
		m.loadPeak = n
	}
	m.mu.Unlock()
}

// observeLoadLocked folds one engine reading's running + waiting into the run's
// picture of the seat. 0 (a reading with no gauges) is no observation.
func (m *Monitor) observeLoadLocked(n int) {
	if n < 1 {
		return
	}
	m.engLoad = n
	if n > m.loadPeak {
		m.loadPeak = n
	}
}

// loadForBoundLocked is the load the busy hold's flat bound is sized with: the
// engine's own latest reading when it has one, else the load the phase started
// under.
func (m *Monitor) loadForBoundLocked() int {
	if m.engLoad > 0 {
		return m.engLoad
	}
	return m.load
}

// PeakLoad is the highest number of requests this run ever saw sharing the seat
// (its own included): 1 = the run had the seat to itself every time it looked,
// 0 = it never looked. A run with PeakLoad > 1 is not solo: its timings are what
// a shared seat gives one request, not the seat's rate.
func (m *Monitor) PeakLoad() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadPeak
}
