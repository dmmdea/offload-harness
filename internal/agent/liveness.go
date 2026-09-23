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
	// PostReady is the floor of the POST-READY bound: how long a request may
	// stay silent after its seat, freshly loaded, reads ready (0.140.0). The
	// bound is max(PostReady, 2 x the waiting phase's allowance), far shorter
	// than ColdLoad, so a seat that wedges right after loading is seen fast.
	PostReady time.Duration
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
	// PostReady: a cold-load stall filed in the hold's post-ready part (the
	// seat had loaded and then sent nothing); ReadySeen: /running read it
	// ready (false = it could not be read).
	PostReady, ReadySeen bool
}

func (e *StallError) Error() string {
	if e.Phase == PhaseColdLoad && e.PostReady {
		after := "after the seat read ready"
		if !e.ReadySeen {
			after = "after the load, with /running unreadable"
		}
		return fmt.Sprintf("stalled: no byte for %.0fs %s, in cold-load (allowed %.0fs%s; %d tok so far)",
			e.Silent.Seconds(), after, e.Allowed.Seconds(), e.Note, e.Tokens)
	}
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
	// The hold has two parts. While the probe reads the seat LOADING, the
	// cold-load ceiling applies from holdStart. Once the load is over
	// (postReady), the short post-ready bound applies from readyAt; readySeen
	// says whether /running read the seat ready or could not be read.
	// resume/resumePending are the phase the request was waiting in (they
	// size the post-ready bound). dl is the run ceiling's deadline, which no
	// part of the hold outlives; holdClamped records that it applied.
	postReady     bool
	readySeen     bool
	readyAt       time.Time
	resume        Phase
	resumePending int
	dl            time.Time
	holdClamped   bool
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
	m := &Monitor{pol: pol, start: start, cancel: cancel, last: start, dl: start.Add(ceiling)}
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
	// load. Ask once more before calling it a stall.
	if m.awaitingByteLocked() && m.pol.ColdLoad > 0 {
		if m.probing {
			// A tick is reading the seat right now; let it decide.
			m.timer.Reset(m.pol.coldLoadPoll())
			m.mu.Unlock()
			return
		}
		loading, state := false, ""
		var perr error
		if m.probe != nil {
			epoch, last, probe := m.epoch, m.last, m.probe
			m.probing = true
			m.mu.Unlock()
			loading, state, perr = runSeatProbe(probe, m.pol.coldLoadPoll())
			m.mu.Lock()
			m.probing = false
			if m.stopped || m.cause != nil || m.epoch != epoch || !m.last.Equal(last) {
				// Progress or a phase change arrived during the probe: its own
				// timer reset governs now.
				m.mu.Unlock()
				return
			}
			loading = loading && perr == nil
		}
		var hook func(Phase, time.Duration)
		var ph Phase
		var allow time.Duration
		switch {
		case loading:
			hook, ph, allow = m.enterLoadingLocked(state)
		case m.warming && perr != nil:
			// Loaded for this run, and the seat cannot be read now: the short
			// post-ready bound, with the unreadable /running named, never a
			// claim that the seat read ready.
			hook, ph, allow = m.enterPostReadyLocked(unreadableState(perr), false, m.last)
		case m.warming:
			hook, ph, allow = m.enterPostReadyLocked(warmupState, true, m.last)
		default:
			m.fileStallLocked()
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		notify(hook, ph, allow)
		return
	}
	m.fileStallLocked()
	m.mu.Unlock()
}

// warmupState is the hold's state when the seat reads ready but has not
// answered its first request since it loaded (the admission warm-up, or a
// load the probe saw end).
const warmupState = "ready, first completion since the load"

func unreadableState(err error) string {
	return "unreadable /running (" + err.Error() + ")"
}

// ceilingMargin keeps the hold's timer ahead of the run ceiling, so a load
// that outlasts the run is filed as a cold-load stall (infrastructure), not
// as the run ceiling (budget).
const ceilingMargin = 50 * time.Millisecond

// postReadyBoundLocked is the post-ready bound: max(PostReady, 2 x the
// waiting phase's own allowance). Admission's allowance is the whole
// admission budget, so the floor stands in for it there.
func (m *Monitor) postReadyBoundLocked() time.Duration {
	base := m.pol.Floor
	if m.resume != PhaseAdmission {
		base = m.pol.Allowance(m.resume, m.resumePending)
	}
	return maxDur(2*base, m.pol.PostReady)
}

// holdBoundLocked is the bound of the hold's current part and when it
// started: the cold-load ceiling from the silence while the seat reads
// loading, the post-ready bound from the moment it read ready.
func (m *Monitor) holdBoundLocked() (time.Duration, time.Time) {
	if m.postReady {
		return m.postReadyBoundLocked(), m.readyAt
	}
	return m.pol.ColdLoad, m.holdStart
}

// holdLeftLocked is what is left of the hold's current part, never past the
// run ceiling (less ceilingMargin).
func (m *Monitor) holdLeftLocked(now time.Time) time.Duration {
	bound, from := m.holdBoundLocked()
	left := bound - now.Sub(from)
	if c := m.dl.Sub(now) - ceilingMargin; c < left {
		left = c
	}
	if left < 0 {
		left = 0
	}
	return left
}

// fileStallLocked cancels the run with a StallError for the current phase.
// In PhaseColdLoad, Silent counts from the start of the hold's current part
// and Allowed is that part's bound as it actually applied (clamped to the
// run ceiling).
func (m *Monitor) fileStallLocked() {
	se := &StallError{Phase: m.phase, Silent: time.Since(m.last), Allowed: m.allow, Tokens: m.tokens + m.callTok}
	if m.phase == PhaseColdLoad {
		bound, from := m.holdBoundLocked()
		if c := m.dl.Sub(from) - ceilingMargin; c < bound {
			bound, m.holdClamped = c, true
		}
		se.Silent, se.Allowed, se.PostReady, se.ReadySeen = time.Since(from), bound, m.postReady, m.readySeen
	}
	se.Note = m.note()
	m.cause = se
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

// enterHoldPhaseLocked moves the run into PhaseColdLoad if it is not there
// yet, remembering the phase it was waiting in. Reports whether it entered.
func (m *Monitor) enterHoldPhaseLocked() bool {
	if m.phase == PhaseColdLoad {
		return false
	}
	m.resume, m.resumePending = m.phase, m.pending
	m.holdStart = m.last
	m.phase = PhaseColdLoad
	m.epoch++
	return true
}

// resetHoldTimerLocked re-arms the stall timer to what is left of the hold.
// The observer hook is returned only on entering the hold, so a 4-minute
// load is one status event, not one per poll.
func (m *Monitor) resetHoldTimerLocked(entered bool) (func(Phase, time.Duration), Phase, time.Duration) {
	m.allow = m.holdLeftLocked(time.Now())
	m.timer.Reset(m.allow)
	m.armProbeLocked()
	if !entered {
		return nil, m.phase, m.allow
	}
	return m.onHold, m.phase, m.allow
}

// enterLoadingLocked: the probe saw the seat LOADING (positive evidence).
// The cold-load ceiling applies, counted from the moment the request went
// silent, so the time before the probe noticed is not free. A seat that was
// post-ready and loads again (evicted again) goes back to this part; the
// ceiling still counts from the first silence.
func (m *Monitor) enterLoadingLocked(state string) (func(Phase, time.Duration), Phase, time.Duration) {
	entered := m.enterHoldPhaseLocked()
	m.holdState, m.warming, m.postReady = state, true, false
	return m.resetHoldTimerLocked(entered)
}

// enterPostReadyLocked: the seat was loaded for this request and is no
// longer loading (it read ready, or cannot be read any more). The short
// post-ready bound applies from `since`; the cold-load ceiling is over.
func (m *Monitor) enterPostReadyLocked(state string, readySeen bool, since time.Time) (func(Phase, time.Duration), Phase, time.Duration) {
	entered := m.enterHoldPhaseLocked()
	m.holdState = state
	if !m.postReady {
		m.postReady, m.readyAt, m.readySeen = true, since, readySeen
	} else if readySeen {
		m.readySeen = true
	}
	return m.resetHoldTimerLocked(entered)
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

// onProbeTick reads the seat while a request waits for its first byte. It
// notices a load early (inside the prefill allowance), moves a load that has
// ended into the post-ready part, and keeps the hold's state honest. A hold
// ends on the first byte (Progress), on a phase change, or at its bound.
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
	case m.phase == PhaseColdLoad && m.holdLeftLocked(time.Now()) <= 0:
		if loading {
			m.holdState = state
		} else if err != nil {
			m.holdState = unreadableState(err)
		}
		m.fileStallLocked()
		m.mu.Unlock()
		return
	case loading:
		hook, ph, allow = m.enterLoadingLocked(state)
	case m.phase == PhaseColdLoad && err != nil:
		// Cannot tell. Never claim the seat read ready: name the unreadable
		// /running, and let the short post-ready bound decide.
		hook, ph, allow = m.enterPostReadyLocked(unreadableState(err), false, time.Now())
	case m.phase == PhaseColdLoad:
		// The load is over and the seat reads ready: the first completion
		// after it gets the post-ready bound, not the rest of the ceiling.
		hook, ph, allow = m.enterPostReadyLocked(warmupState, true, time.Now())
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
// (the admission warm-up loaded it and read it ready). Until the seat's first
// byte, silence while a request waits is held under the short post-ready
// bound instead of the prefill clock, never under the cold-load ceiling. A
// no-op without a cold-load ceiling.
func (m *Monitor) MarkSeatLoaded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warming = true
}

// note spells the prefill arithmetic so a reader can check the allowance.
func (m *Monitor) note() string {
	if m.phase == PhaseColdLoad {
		clamp := ""
		if m.holdClamped {
			clamp = ", clamped to the run ceiling"
		}
		if m.postReady {
			base, what := m.pol.Floor, "floor"
			if m.resume != PhaseAdmission {
				base, what = m.pol.Allowance(m.resume, m.resumePending), string(m.resume)+" allowance"
			}
			return fmt.Sprintf(": post-ready bound = max(%.0fs, 2 x %.0fs %s)%s; the seat: %s",
				m.pol.PostReady.Seconds(), base.Seconds(), what, clamp, m.holdState)
		}
		basis := m.pol.ColdLoadBasis
		if basis != "" {
			basis = " = " + basis
		}
		return fmt.Sprintf(": cold-load ceiling%s%s; the seat read %q, so no byte could arrive", basis, clamp, m.holdState)
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
	m.warming, m.postReady = false, false
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
		m.warming, m.postReady = false, false // a call completed: the seat has answered since its load
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
		m.warming, m.postReady = false, false
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
		// buys no extra time: the hold's bound still counts from its start.
		m.allow = m.holdLeftLocked(now)
		m.timer.Reset(m.allow)
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
