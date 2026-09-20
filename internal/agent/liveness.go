package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Phase is where a run is, for the liveness rule. It is finer than the
// registry's Phase (admission / running / repack): the stall allowance differs
// between a prefill, a decode and a tool call.
type Phase string

const (
	PhaseAdmission Phase = "admission"
	PhasePrefill   Phase = "prefill"
	PhaseDecoding  Phase = "decoding"
	PhaseTool      Phase = "tool"
	PhaseRepack    Phase = "repack"
)

const (
	// assumedPrefillTokS is a SLOW seat's prefill rate when none is measured
	// yet. 400 was the first guess and it filed a false stall on the very seat
	// this exists for (2026-09-20, Lenovo 27B GSQ on an A2: ~12k uncached tokens
	// took >94 s = ~130 tok/s). 100 errs generous; a measured rate (time to
	// first delta, any engine) replaces it after one run.
	assumedPrefillTokS = 100.0
	// assumedTokS is the decode rate assumed when none is measured: 1 tok/s
	// makes the decoding allowance the floor, which is the generous reading.
	assumedTokS = 1.0
	// prefillMargin pads the prefill estimate: engines batch and schedule, and
	// a chunked prefill is not linear. StallPolicy.Slack (30 s in production)
	// is the flat padding on top of it and on top of a tool's own timeout.
	prefillMargin = 1.5
	// uncappedToolAllowance is the stall bound for a tool the loop runs with
	// capping explicitly disabled (Tool.Timeout < 0): long by design.
	uncappedToolAllowance = time.Hour
	// decodeDeltas is how many deltas of silence the decoding allowance
	// covers at the seat's rate before the floor takes over.
	decodeDeltas = 20.0
	// ewmaAlpha smooths the instantaneous tok/s the monitor publishes.
	ewmaAlpha = 0.2
)

// StallPolicy is how long a run may go without a progress event in each
// phase before it is declared stalled. Liveness is progress: a streamed token,
// a tool call starting or finishing, a phase change. The allowance is DYNAMIC
// — prefill of a 214k-token prompt at 2,000 tok/s is 160 s of legitimate
// silence, not a hang — and it is what status readers show as "allowed".
type StallPolicy struct {
	Admission   time.Duration // allowance while admitting / cold-loading / probing
	PrefillTokS float64       // measured prefill rate; 0 = unknown
	TokS        float64       // measured decode rate; 0 = unknown
	ToolTimeout time.Duration // the loop's per-tool cap (ToolPhase overrides per call)
	Repack      time.Duration // the grammar-free re-pack's bound
	Floor       time.Duration // no allowance is ever shorter (60 s in production)
	Slack       time.Duration // flat padding on the prefill estimate and a tool's cap (30 s in production)
}

// Allowance is the stall bound for a phase. pendingPromptTokens is the size
// of the prompt the seat is prefilling (0 outside prefill).
func (p StallPolicy) Allowance(ph Phase, pendingPromptTokens int) time.Duration {
	switch ph {
	case PhaseAdmission:
		return maxDur(p.Admission, p.Floor)
	case PhasePrefill:
		rate := p.PrefillTokS
		if rate <= 0 {
			rate = assumedPrefillTokS
		}
		d := time.Duration(float64(pendingPromptTokens)/rate*prefillMargin*float64(time.Second)) + p.Slack
		return maxDur(d, p.Floor)
	case PhaseDecoding:
		rate := p.TokS
		if rate <= 0 {
			rate = assumedTokS
		}
		return maxDur(time.Duration(decodeDeltas/rate*float64(time.Second)), p.Floor)
	case PhaseTool:
		return p.toolAllowance(p.ToolTimeout)
	case PhaseRepack:
		return maxDur(p.Repack, p.Floor)
	}
	return p.Floor
}

// toolAllowance is the stall bound while a tool with the given cap runs:
// the cap plus slack (the loop selects on the cap, liveness only has to
// outlast it); a negative cap means capping was disabled, so an hour.
func (p StallPolicy) toolAllowance(cap time.Duration) time.Duration {
	if cap < 0 {
		return maxDur(uncappedToolAllowance, p.Floor)
	}
	return maxDur(cap+p.Slack, p.Floor)
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// StallError: the seat stopped producing for this run. Filed as an
// INFRASTRUCTURE defer — the contract was fine, the seat went silent.
type StallError struct {
	Phase           Phase
	Silent, Allowed time.Duration
	Tokens          int
	Note            string
}

func (e *StallError) Error() string {
	return fmt.Sprintf("stalled: no progress for %.0fs in %s (allowed %.0fs%s; %d tok so far)",
		e.Silent.Seconds(), e.Phase, e.Allowed.Seconds(), e.Note, e.Tokens)
}

// CeilingError: the run was still producing when the safety ceiling passed.
// Filed as a BUDGET defer — the sizing signal the delegator learns from.
type CeilingError struct {
	Elapsed time.Duration
	Tokens  int
	TokS    float64
}

func (e *CeilingError) Error() string {
	return fmt.Sprintf("ceiling %.0fs reached while producing (%d tok at %.1f tok/s)", e.Elapsed.Seconds(), e.Tokens, e.TokS)
}

// Monitor owns a run's liveness: a stall timer reset by progress and a
// ceiling deadline over the parent context. It cancels the context it hands
// out with a typed cause (context.Cause) so the caller can file the right
// defer class without guessing from the clock.
type Monitor struct {
	pol     StallPolicy
	start   time.Time
	cancel  context.CancelCauseFunc
	ceiling *time.Timer
	mu      sync.Mutex
	phase   Phase
	pending int
	allow   time.Duration
	tokens  int // run total of the calls already folded in
	callTok int // the current call's count so far
	last    time.Time
	ewma    float64
	timer   *time.Timer
	cause   error
	stopped bool
}

// NewMonitor wraps parent with a ceiling deadline and a stall watch. The
// returned context's Deadline() is the ceiling, so every existing reader of
// ctx.Deadline() (the re-pack floor, the seat-wait budget) keeps working; a
// stall cancels it early with a *StallError cause. The watch starts in
// PhaseAdmission. Stop it when the run ends (a deferred Stop, like a deferred
// cancel).
func NewMonitor(parent context.Context, pol StallPolicy, ceiling time.Duration) (context.Context, *Monitor) {
	start := time.Now()
	// The ceiling is the monitor's OWN timer, not a context.WithDeadline
	// parent: a deadline parent cancels the child with DeadlineExceeded before
	// the monitor can file its cause, and context.Cause would never carry the
	// CeilingError. The wrapper below still answers Deadline() with the
	// ceiling, so every existing reader (the re-pack floor, the seat-wait
	// budget, a child WithTimeout) keeps working.
	cctx, cancel := context.WithCancelCause(parent)
	m := &Monitor{pol: pol, start: start, cancel: cancel, last: start}
	m.phase, m.allow = PhaseAdmission, pol.Allowance(PhaseAdmission, 0)
	m.timer = time.AfterFunc(m.allow, m.onStall)
	m.ceiling = time.AfterFunc(ceiling, m.onCeiling)
	return ceilingCtx{Context: cctx, dl: start.Add(ceiling)}, m
}

// ceilingCtx reports the ceiling as the context's deadline while cancellation
// and Cause come from the embedded cancel-cause context.
type ceilingCtx struct {
	context.Context
	dl time.Time
}

func (c ceilingCtx) Deadline() (time.Time, bool) {
	if pd, ok := c.Context.Deadline(); ok && pd.Before(c.dl) {
		return pd, true
	}
	return c.dl, true
}

func (m *Monitor) onCeiling() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.cause != nil {
		return
	}
	m.cause = &CeilingError{Elapsed: time.Since(m.start), Tokens: m.tokens + m.callTok, TokS: m.ewma}
	m.cancel(m.cause)
}

func (m *Monitor) onStall() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.cause != nil {
		return
	}
	m.cause = &StallError{Phase: m.phase, Silent: time.Since(m.last), Allowed: m.allow, Tokens: m.tokens + m.callTok, Note: m.note()}
	m.cancel(m.cause)
}

// note spells the prefill arithmetic so a reader can check the allowance.
func (m *Monitor) note() string {
	if m.phase != PhasePrefill {
		return ""
	}
	rate, src := m.pol.PrefillTokS, ""
	if rate <= 0 {
		rate, src = assumedPrefillTokS, " assumed"
	}
	return fmt.Sprintf(": %d tok / %.0f tok/s%s x %.1f + %.0fs", m.pending, rate, src, prefillMargin, m.pol.Slack.Seconds())
}

// ToolPhase moves the run into a tool call whose own cap is `cap` (0 = the
// policy's ToolTimeout, negative = uncapped). A tool call is progress.
func (m *Monitor) ToolPhase(cap time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.cause != nil {
		return
	}
	m.tokens += m.callTok
	m.callTok = 0
	m.phase, m.pending = PhaseTool, 0
	if cap == 0 {
		cap = m.pol.ToolTimeout
	}
	m.allow = m.pol.toolAllowance(cap)
	m.last = time.Now()
	m.timer.Reset(m.allow)
}

// Phase moves the run to ph; a phase change is itself progress. The finished
// call's tokens are folded into the run total.
func (m *Monitor) Phase(ph Phase, pendingPromptTokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.cause != nil {
		return
	}
	m.tokens += m.callTok
	m.callTok = 0
	m.phase, m.pending = ph, pendingPromptTokens
	m.allow = m.pol.Allowance(ph, pendingPromptTokens)
	m.last = time.Now()
	m.timer.Reset(m.allow)
}

// Progress records the current call's token count so far. The first delta of
// a prefill ends the prefill: the allowance drops to the decoding bound.
func (m *Monitor) Progress(tokensSoFar int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped || m.cause != nil {
		return
	}
	now := time.Now()
	if d := tokensSoFar - m.callTok; d > 0 {
		if dt := now.Sub(m.last).Seconds(); dt > 0 {
			inst := float64(d) / dt
			if m.ewma == 0 {
				m.ewma = inst
			} else {
				m.ewma = ewmaAlpha*inst + (1-ewmaAlpha)*m.ewma
			}
		}
		m.callTok = tokensSoFar
		if m.phase == PhasePrefill {
			m.phase, m.allow = PhaseDecoding, m.pol.Allowance(PhaseDecoding, 0)
		}
	}
	// tokensSoFar <= the count already seen is a TOUCH — the seat answered
	// (a 429 "busy", a retry) without producing: liveness, not a token.
	m.last = now
	m.timer.Reset(m.allow)
}

// Stop ends the watch and releases the ceiling deadline (the run finished).
// Idempotent. Like a deferred cancel, it also ends the context it handed out.
func (m *Monitor) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.timer.Stop()
	m.ceiling.Stop()
	m.mu.Unlock()
	m.cancel(nil) // context.Canceled, no cause of ours: the run finished
}

// Cause is why the monitor cancelled: nil (it did not — the parent did, or the
// run finished), *StallError or *CeilingError.
func (m *Monitor) Cause() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cause
}

// Snapshot is the live telemetry status readers and the job view publish.
func (m *Monitor) Snapshot() (tokens int, tokS float64, lastProgress time.Time, allowance time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens + m.callTok, m.ewma, m.last, m.allow
}

// CurrentPhase is the liveness phase the run is in.
func (m *Monitor) CurrentPhase() Phase {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phase
}
