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
	// PhaseColdLoad is a request waiting for its seat to LOAD: the engine is
	// not serving the seat (llama-swap reads it `starting`, or holds the
	// request while it swaps another model out), so no byte can arrive and
	// silence is not a stall. The prefill clock is suspended and a cold-load
	// ceiling bounds the wait instead (0.140.0).
	PhaseColdLoad Phase = "cold-load"
)

// defaultColdLoadPoll is how often a request waiting for its first byte asks
// the seat probe whether the seat is loading. /running is a local, cheap read.
const defaultColdLoadPoll = 5 * time.Second

// SeatProbe reads whether the engine is serving the run's seat right now.
// loading = the seat is not being served (llama-swap `starting`/`stopping`,
// or absent while the request is held for a swap); state is what the probe
// saw, for the reason. A non-nil err means "cannot tell": the monitor then
// keeps the prefill clock running, exactly as it did before probes existed.
type SeatProbe func(ctx context.Context) (loading bool, state string, err error)

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
	// ColdLoad is the ceiling on ONE observed seat load while a request waits
	// for its first byte (0.140.0). 0 = no cold-load hold: a silent prefill is
	// a stall whatever the seat is doing, the pre-0.140.0 rule. It is never
	// unbounded: past it the run files a cold-load stall.
	ColdLoad time.Duration
	// ColdLoadBasis spells how ColdLoad was sized, for the reason text.
	ColdLoadBasis string
	// ColdLoadPoll is the probe cadence while a request waits for its first
	// byte; 0 = defaultColdLoadPoll.
	ColdLoadPoll time.Duration
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
	case PhaseColdLoad:
		return p.ColdLoad
	}
	return p.Floor
}

func (p StallPolicy) coldLoadPoll() time.Duration {
	if p.ColdLoadPoll > 0 {
		return p.ColdLoadPoll
	}
	return defaultColdLoadPoll
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
	if e.Phase == PhaseColdLoad {
		return fmt.Sprintf("stalled: seat still loading after %.0fs in cold-load (allowed %.0fs%s; %d tok so far)",
			e.Silent.Seconds(), e.Allowed.Seconds(), e.Note, e.Tokens)
	}
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
	// The cold-load hold (0.140.0). probe reads the seat while a request
	// waits for its first byte; probeT is its cadence. While the seat reads
	// loading the run sits in PhaseColdLoad: holdStart is when the silence
	// began, holdState the last state the probe saw.
	// epoch counts phase changes so a probe answer about an older phase is
	// never applied to a newer one. onHold tells the run's observer about a
	// phase change the monitor made itself (status readers, the delegator).
	probe     SeatProbe
	onHold    func(Phase, time.Duration)
	probeT    *time.Timer
	probing   bool // a probe is in flight (at most one at a time)
	holdStart time.Time
	holdState string
	epoch     uint64
	// warming: the seat loaded for this run (admission warm-up, or a load the
	// probe saw) and has not sent a byte since. Cleared by the first delta or
	// by a completed call (the loop moving to a tool or decoding).
	warming bool
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
	if m.stopped || m.cause != nil {
		m.mu.Unlock()
		return
	}
	// A request still waiting for its first byte may be waiting for its seat
	// to LOAD (a cold start after the idle unload, another model's swap
	// evicting the seat between steps), or for the first completion after a
	// load (the engine's own warm-up). Ask once more before calling it a
	// stall: either moves the run into the cold-load hold instead.
	if m.awaitingByteLocked() && m.pol.ColdLoad > 0 {
		if m.probing {
			// A tick is reading the seat right now; let it decide.
			m.timer.Reset(m.pol.coldLoadPoll())
			m.mu.Unlock()
			return
		}
		loading, state := false, ""
		if m.probe != nil {
			epoch, last, probe := m.epoch, m.last, m.probe
			m.probing = true
			m.mu.Unlock()
			var err error
			loading, state, err = runSeatProbe(probe, m.pol.coldLoadPoll())
			m.mu.Lock()
			m.probing = false
			if m.stopped || m.cause != nil || m.epoch != epoch || !m.last.Equal(last) {
				// Progress or a phase change arrived during the probe: its own
				// timer reset governs now.
				m.mu.Unlock()
				return
			}
			loading = loading && err == nil
		}
		if loading || m.warming {
			if !loading {
				state = warmupState
			}
			hook, ph, allow := m.enterHoldLocked(state, loading)
			m.mu.Unlock()
			notify(hook, ph, allow)
			return
		}
	}
	m.fileStallLocked()
	m.mu.Unlock()
}

// warmupState is the hold's state when the seat reads ready but has not
// answered its first request since it loaded. Measured 2026-09-23 on the
// 3-card vLLM seat: /running read `ready` after a 177 s load, and the first
// ~12k-token completion then sent nothing for 60 s: the engine's first-request
// warm-up, which /running cannot see.
const warmupState = "ready, first completion since the load (engine warm-up)"

// fileStallLocked cancels the run with a StallError for the current phase.
// In PhaseColdLoad it is the cold-load ceiling: Silent counts from when the
// request went silent, Allowed is the ceiling.
func (m *Monitor) fileStallLocked() {
	silent, allowed := time.Since(m.last), m.allow
	if m.phase == PhaseColdLoad {
		silent, allowed = time.Since(m.holdStart), m.pol.ColdLoad
	}
	m.cause = &StallError{Phase: m.phase, Silent: silent, Allowed: allowed, Tokens: m.tokens + m.callTok, Note: m.note()}
	m.cancel(m.cause)
}

// awaitingByteLocked: a request may be out with no byte of its answer back
// yet, the phases where a seat load can be the reason for the silence.
// Admission is one of them: the loop's own pre-step probes (the tokenizer,
// the window) go through llama-swap and wait out a cold seat too.
func (m *Monitor) awaitingByteLocked() bool {
	return m.phase == PhasePrefill || m.phase == PhaseRepack || m.phase == PhaseAdmission
}

func (m *Monitor) probeArmedLocked() bool {
	return m.probe != nil && m.pol.ColdLoad > 0
}

// enterHoldLocked moves the run into the cold-load hold (or refreshes it).
// The ceiling counts from the moment the request went silent, so the time
// spent before the probe noticed is not free. loaded = the probe SAW a load,
// so the first completion after it is warm-up too and the hold lasts until
// the first byte. Returns the observer hook to call once the lock is
// released, only on the transition INTO the hold, so a 4-minute load is one
// status event, not one per poll.
func (m *Monitor) enterHoldLocked(state string, loaded bool) (func(Phase, time.Duration), Phase, time.Duration) {
	m.holdState = state
	if loaded {
		m.warming = true
	}
	entered := m.phase != PhaseColdLoad
	if entered {
		m.holdStart = m.last
		m.phase = PhaseColdLoad
		m.epoch++
	}
	left := m.pol.ColdLoad - time.Since(m.holdStart)
	if left < 0 {
		left = 0
	}
	m.allow = left
	m.timer.Reset(left)
	m.armProbeLocked()
	if !entered {
		return nil, m.phase, m.allow
	}
	return m.onHold, m.phase, m.allow
}

func (m *Monitor) armProbeLocked() {
	if !m.probeArmedLocked() {
		return
	}
	if m.probeT == nil {
		m.probeT = time.AfterFunc(m.pol.coldLoadPoll(), m.onProbeTick)
		return
	}
	m.probeT.Reset(m.pol.coldLoadPoll())
}

func (m *Monitor) stopProbeLocked() {
	if m.probeT != nil {
		m.probeT.Stop()
	}
}

// onProbeTick reads the seat while a request waits for its first byte. It is
// what notices a load early (inside the prefill allowance) and what keeps the
// hold's state current; onStall's own probe covers a load the tick has not
// seen yet. A hold ends on the first byte (Progress) or on a phase change,
// never on a /running read alone: the first completion after a load is warm-up.
func (m *Monitor) onProbeTick() {
	m.mu.Lock()
	if m.stopped || m.cause != nil || m.probing || !m.probeArmedLocked() ||
		(!m.awaitingByteLocked() && m.phase != PhaseColdLoad) {
		m.mu.Unlock()
		return
	}
	epoch, probe := m.epoch, m.probe
	m.probing = true
	m.mu.Unlock()
	loading, state, err := runSeatProbe(probe, m.pol.coldLoadPoll())
	m.mu.Lock()
	m.probing = false
	if m.stopped || m.cause != nil {
		m.mu.Unlock()
		return
	}
	if m.epoch != epoch {
		// The phase moved during the probe; the answer is about a request
		// that is no longer the one waiting. Keep reading while one waits.
		if m.awaitingByteLocked() || m.phase == PhaseColdLoad {
			m.armProbeLocked()
		}
		m.mu.Unlock()
		return
	}
	loading = loading && err == nil
	var hook func(Phase, time.Duration)
	var ph Phase
	var allow time.Duration
	switch {
	case m.phase == PhaseColdLoad && time.Since(m.holdStart) >= m.pol.ColdLoad:
		if loading {
			m.holdState = state
		}
		m.fileStallLocked()
		m.mu.Unlock()
		return
	case loading:
		hook, ph, allow = m.enterHoldLocked(state, true)
	case m.phase == PhaseColdLoad:
		// Loaded (or unreadable) but not yet answering: the first completion
		// after a load is still cold cost, so the hold lasts until the first
		// byte. The ceiling, counted from the silence, bounds it.
		m.holdState = warmupState
		m.armProbeLocked()
	default:
		m.armProbeLocked()
	}
	m.mu.Unlock()
	notify(hook, ph, allow)
}

func notify(hook func(Phase, time.Duration), ph Phase, allow time.Duration) {
	if hook != nil {
		hook(ph, allow)
	}
}

// runSeatProbe bounds one probe by the poll cadence (never under a second),
// so a hung /running read cannot hold the monitor's decision.
func runSeatProbe(p SeatProbe, poll time.Duration) (bool, string, error) {
	if poll < time.Second {
		poll = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), poll)
	defer cancel()
	return p(ctx)
}

// WithSeatProbe installs the cold-load hold's seat probe (0.140.0) and the
// hook that hears the phase change the monitor makes on its own (into
// PhaseColdLoad; the first byte ends the hold through the loop's own
// progress events). With StallPolicy.ColdLoad 0 there is no hold at all, and
// without a probe only a MarkSeatLoaded warm-up is held: otherwise a silent
// prefill is a stall, as before.
func (m *Monitor) WithSeatProbe(p SeatProbe, onHold func(Phase, time.Duration)) *Monitor {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probe, m.onHold = p, onHold
	if m.awaitingByteLocked() {
		m.armProbeLocked()
	}
	return m
}

// MarkSeatLoaded tells the monitor the seat was just loaded for this run
// (the admission warm-up loaded it). Until the seat's first byte, silence
// while a request waits is the engine's first-request warm-up, held under
// the cold-load ceiling instead of the prefill clock. A no-op without a
// cold-load ceiling.
func (m *Monitor) MarkSeatLoaded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warming = true
}

// note spells the prefill arithmetic so a reader can check the allowance.
func (m *Monitor) note() string {
	if m.phase == PhaseColdLoad {
		basis := m.pol.ColdLoadBasis
		if basis != "" {
			basis = " = " + basis
		}
		return fmt.Sprintf(": cold-load ceiling%s; the seat read %q, so no byte could arrive", basis, m.holdState)
	}
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
	m.epoch++
	m.warming = false
	m.stopProbeLocked()
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
	m.epoch++
	m.timer.Reset(m.allow)
	if m.awaitingByteLocked() {
		m.armProbeLocked()
	} else {
		m.warming = false // a call completed: the seat has answered since its load
		m.stopProbeLocked()
	}
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
		m.warming = false
		if m.phase == PhasePrefill || m.phase == PhaseColdLoad {
			// A byte arrived, so the seat is serving: any cold-load hold is over.
			m.phase, m.allow = PhaseDecoding, m.pol.Allowance(PhaseDecoding, 0)
			m.epoch++
			m.stopProbeLocked()
		}
	}
	// tokensSoFar <= the count already seen is a TOUCH — the seat answered
	// (a 429 "busy", a retry) without producing: liveness, not a token.
	m.last = now
	if m.phase == PhaseColdLoad {
		// A touch during the hold (a counted 429 wait) is liveness, but it
		// buys no extra load time: the ceiling still counts from holdStart.
		left := m.pol.ColdLoad - now.Sub(m.holdStart)
		if left < 0 {
			left = 0
		}
		m.allow = left
		m.timer.Reset(left)
		return
	}
	m.timer.Reset(m.allow)
}

// Stop ends the watch and releases the ceiling deadline (the run finished).
// Idempotent. Like a deferred cancel, it also ends the context it handed out.
func (m *Monitor) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.timer.Stop()
	m.ceiling.Stop()
	m.stopProbeLocked()
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
