package agent

// The seat-down outcome (ADR 0066, 0.144.0, register C-72).
//
// On 2026-09-29 the reference workstation's 3-card flagship engine died ten
// times. Each death took every run in flight with it as an independent
// `stalled:` — although the loop holds its whole transcript and needed only the
// seat back — and the runs that arrived afterwards burned their 60 s floors on
// a seat that could not answer. ADR 0061 made the diagnosis right (a busy
// engine holds a run, an engine that does no work is a stall); it did not make
// a death survivable. This file does.
//
// A seat is DOWN when its engine cannot serve the run's request and llama-swap
// says so, or the engine's own counters say so:
//
//   - WEDGED: the engine's work fingerprint stayed flat for the flat bound while
//     llama-swap still read the seat ready (a hung step: the request is silent,
//     nothing anywhere moves). It is the busy hold's flat verdict, retyped.
//   - DIED: the seat left llama-swap's /running inside an established hold, or
//     a model call failed like a dead seat (a stream cut, a 5xx, a refused
//     connection) and llama-swap reads the seat starting, stopping or absent,
//     or the engine refuses connections while llama-swap still lists it. The
//     failure alone is never the evidence: a 5xx from a seat that reads ready
//     and readable is an ordinary error and stays one.
//
// What happens next is one bounded recovery, owned by the loop because the loop
// owns the transcript: the monitor cancels ONLY the model call in flight (its
// step scope), the run's context stays alive, the run waits for the seat under
// the cold-load hold (AwaitSeat, bounded by the cold-load ceiling and the run's
// ceiling), and the same step is re-issued. A recovery spends no step.
//
// The wait is one EPISODE (seatEpisode), which begins at the first verdict and
// outlives the calls that meet it. A seat that is gone from /running has nobody
// starting it — llama-swap starts a stopped seat on the next request — so the
// re-issue is the start trigger, and the cold-load hold covers the load. If that
// start fails (the 2026-09-29 launcher refused to start the flagship for 18
// minutes, and every request got an instant 500) the episode goes on: the run
// waits at the poll and triggers again, until the cold-load bound counted from the
// FIRST verdict, instead of turning each queued request into two quick 500s and a
// defer. Once the seat has been seen serving, the step is re-issued ONCE: a call
// that fails again before its answer is a seat that came back and went down again,
// and ends the run. A recovery is counted when the re-issued call's first byte
// arrives, never for a re-issue that recovered nothing.
//
// What does not recover ends the run typed: the reason is prefixed
// core.SeatDownReason, which the delegator reads to re-place the contract on
// another node.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// SeatDownKind says HOW the seat went down.
type SeatDownKind string

const (
	// SeatDownWedged: the engine did no work for anyone for the flat bound while
	// llama-swap read the seat loaded. Recovery needs a CHANGE — a restart, or the
	// counters moving again — before a re-issue is worth sending.
	SeatDownWedged SeatDownKind = "wedged"
	// SeatDownDied: llama-swap no longer serves the seat (gone from /running,
	// restarting, or its engine refuses connections).
	SeatDownDied SeatDownKind = "died"
)

// SeatDownError is the typed outcome of a seat that went down under a run. Its
// text begins core.SeatDownReason; it wraps the evidence that revealed it (the
// engine-flat *StallError for a wedge — so a reader that only knows StallError
// still sees the arithmetic — or the failed call's own error for a death).
type SeatDownError struct {
	Kind SeatDownKind
	// Phase is where the run's request was waiting when the seat went down.
	Phase Phase
	// Silent is how long the engine had shown no sign of life (a wedge).
	Silent time.Duration
	// Engine is the last engine reading; Note is the evidence in one clause.
	Engine string
	Note   string
	Tokens int
	// Waited is how long the run waited for the seat to come back.
	Waited time.Duration
	// Reissued: the failed step was re-issued after the seat came back and the
	// seat went down again. GaveUp: the wait ran out (or no recovery was left).
	Reissued bool
	GaveUp   bool
	// Attempts is how many re-issues the wait sent to a seat nobody listed (each is
	// a start attempt); LastSeen is what the wait last saw of the seat.
	Attempts int
	LastSeen string
	// terminal marks the verdict the monitor has already made the run's cause: it
	// ends the run, nothing waits for it.
	terminal bool
	// fp is the engine fingerprint the wedge froze on.
	fp    string
	cause error
}

func (e *SeatDownError) Error() string {
	var b strings.Builder
	b.WriteString(core.SeatDownReason)
	var se *StallError
	switch {
	case e.Kind == SeatDownWedged && errors.As(e.cause, &se):
		b.WriteString(strings.TrimPrefix(se.Error(), "stalled: "))
	case e.Note != "":
		b.WriteString(e.Note)
		if e.cause != nil {
			fmt.Fprintf(&b, " (the request failed: %v)", e.cause)
		}
		if e.Engine != "" {
			fmt.Fprintf(&b, " (the engine's last reading: %s)", e.Engine)
		}
	default:
		b.WriteString("the seat's engine is down")
	}
	if e.Reissued {
		b.WriteString("; the failed step was re-issued once after the seat came back and it went down again")
	}
	if e.GaveUp {
		switch {
		case e.Waited > 0 && e.Attempts > 0:
			fmt.Fprintf(&b, "; the seat did not come back after %d start attempt(s) (waited %.0fs)", e.Attempts, e.Waited.Seconds())
		case e.Waited > 0:
			fmt.Fprintf(&b, "; the seat did not come back (waited %.0fs)", e.Waited.Seconds())
		default:
			b.WriteString("; not waited for (this run has no seat recovery left)")
		}
	}
	if e.LastSeen != "" && (e.Reissued || (e.GaveUp && e.Waited > 0)) {
		fmt.Fprintf(&b, "; the seat was last seen: %s", e.LastSeen)
	}
	return b.String()
}

func (e *SeatDownError) Unwrap() error { return e.cause }

// brief is the verdict in a few words, for the hold's state text.
func (e *SeatDownError) brief() string {
	if e.Note != "" {
		return e.Note
	}
	return string(e.Kind)
}

// SeatCheckFunc is the seam the chat client asks before it sleeps on a 5xx: is
// the seat not serving right now, and why.
type SeatCheckFunc func(ctx context.Context) (down bool, why string)

type seatCheckKey struct{}

// ContextWithSeatCheck attaches the check to ctx for the chat client.
func ContextWithSeatCheck(ctx context.Context, fn SeatCheckFunc) context.Context {
	return context.WithValue(ctx, seatCheckKey{}, fn)
}

// SeatCheckFromContext returns the attached check, or nil.
func SeatCheckFromContext(ctx context.Context) SeatCheckFunc {
	fn, _ := ctx.Value(seatCheckKey{}).(SeatCheckFunc)
	return fn
}

// seatDownEvidence reads one engine reading for proof that the seat is down.
// Only POSITIVE evidence counts: llama-swap lists the seat loading (its engine
// was restarted), does not list it at all, or the engine's own address refused
// the connection. An unreadable engine is "cannot tell", never "down".
func seatDownEvidence(rd EngineReading, err error) (string, bool) {
	switch {
	case err == nil && rd.Loading:
		state := rd.State
		if state == "" {
			state = "loading"
		}
		return "llama-swap lists the seat " + state + ": its engine went down and is being restarted", true
	case err == nil && rd.NotLoaded:
		return "the seat is no longer in llama-swap's /running: its engine died or was stopped and nothing is loading it", true
	case rd.Refused:
		return "the seat's engine refuses connections while llama-swap still lists it: its process is gone", true
	}
	return "", false
}

// seatFailure reports whether a failed model call MAY be a dead seat: a
// transport failure, a cut stream or a 5xx. It only nominates the failure for
// confirmation against llama-swap (ConfirmSeatDown) — a 4xx, a cancellation
// and the run's own deadline never are.
func seatFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return !ue.Timeout()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return !ne.Timeout()
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// The streaming client's own failures (client_stream.go): the engine died
	// mid-stream, the connection was cut, or the engine sent an error frame.
	msg := err.Error()
	return strings.Contains(msg, "stream read:") ||
		strings.Contains(msg, "stream ended without a finish_reason") ||
		strings.Contains(msg, "engine error in stream")
}

// stepScope is the model call the loop has in flight. A seat-down verdict
// cancels it, and only it.
type stepScope struct{ cancel context.CancelCauseFunc }

// beginStep derives the context ONE model call runs under and registers it as
// the monitor's recoverable step. The returned func releases it (idempotent);
// call it as soon as the call returns and BEFORE waiting on the seat.
func (m *Monitor) beginStep(parent context.Context) (context.Context, func()) {
	sctx, cancel := context.WithCancelCause(parent)
	sc := &stepScope{cancel: cancel}
	m.mu.Lock()
	m.step = sc
	m.mu.Unlock()
	var once sync.Once
	return sctx, func() {
		once.Do(func() {
			m.mu.Lock()
			if m.step == sc {
				m.step = nil
			}
			m.mu.Unlock()
			cancel(nil)
		})
	}
}

// beginCall is the context ONE model call runs under: the step's context, in
// the monitor's step scope (a seat-down verdict cancels only this call) and
// with the seat check the chat client asks before it sleeps on a 5xx. Without a
// monitor the context is returned untouched. Call the returned func as soon as
// the call returns.
func (l *Loop) beginCall(ctx context.Context) (context.Context, func()) {
	if l.live == nil {
		return ctx, func() {}
	}
	return l.live.beginStep(ContextWithSeatCheck(ctx, l.live.SeatCheck))
}

// seatDownFor is the seat-down verdict behind a failed model call, or nil: the
// error the client typed (it asked the seat check), the verdict the monitor
// cancelled the call with, or — for a failure that looks like a dead seat — the
// verdict llama-swap confirms. An ordinary error, a cancelled run and a seat
// that reads ready and readable are none of them.
func (l *Loop) seatDownFor(ctx, callCtx context.Context, err error) *SeatDownError {
	var sd *SeatDownError
	if errors.As(err, &sd) {
		return sd
	}
	if sd = seatDownCause(callCtx); sd != nil {
		return sd
	}
	if l.live == nil || ctx.Err() != nil || !seatFailure(err) {
		return nil
	}
	return l.live.ConfirmSeatDown(ctx, err)
}

// seatDownCause is the seat-down verdict the monitor cancelled this call with,
// or nil. Reading it costs nothing: it is the call context's own cause.
func seatDownCause(callCtx context.Context) *SeatDownError {
	var sd *SeatDownError
	if c := context.Cause(callCtx); c != nil && errors.As(c, &sd) {
		return sd
	}
	return nil
}

// SeatRecoveries is how many times this run's seat went down under it and came
// back: an episode is counted when the re-issued call's first byte arrives (or the
// step is answered), never for a re-issue that recovered nothing.
func (m *Monitor) SeatRecoveries() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recoveries
}

// SeatDownTotal is the wall this run spent waiting for a downed seat: from the
// first verdict of each episode to the re-issued call's first byte, or to the wait
// running out. It covers the whole episode, including the time a re-issued call
// spent held by llama-swap while it started the seat.
func (m *Monitor) SeatDownTotal() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.downTotal
	if m.ep != nil && !m.ep.landed {
		t += time.Since(m.ep.since)
	}
	return t
}

// seatEpisode is the outage a run is waiting out (ADR 0066). It begins at the first
// seat-down verdict and lives until the step that met it is answered, so the
// calls the loop re-issues inside it belong to ONE wait with ONE bound.
type seatEpisode struct {
	since time.Time // the first verdict: the wait's bound counts from here, never restarts
	// served: the wait saw the seat serving again. A re-issued call that fails again
	// before its answer is then a seat that came back and went down again.
	served bool
	// landed: the re-issued call's first byte (or its answer) arrived. The recovery
	// is counted and the wait booked at that moment.
	landed bool
	// sawDown: a non-serving state (starting, unlisted) was seen since the verdict:
	// a wedge needs a restart or a change before a re-issue is worth sending, and the
	// re-issued call's first completion is the seat's cold cost.
	sawDown  bool
	attempts int    // re-issues sent to a seat nobody listed: each is a start attempt
	lastSeen string // what the wait last saw of the seat
}

// landEpisodeLocked counts the recovery and books the wait: the seat is serving.
func (m *Monitor) landEpisodeLocked(now time.Time) {
	if m.ep == nil || m.ep.landed {
		return
	}
	m.ep.landed = true
	m.downTotal += now.Sub(m.ep.since)
	m.recoveries++
}

// closeEpisodeLocked ends the episode, booking a wait that never landed.
func (m *Monitor) closeEpisodeLocked(now time.Time) {
	if m.ep == nil {
		return
	}
	if !m.ep.landed {
		m.downTotal += now.Sub(m.ep.since)
	}
	m.ep = nil
}

// SeatAnswered tells the monitor the step got its answer: the seat serves, the
// episode (if one is open) ends as a recovery, and a later death earns its own.
func (m *Monitor) SeatAnswered() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.landEpisodeLocked(time.Now())
	m.ep = nil
}

// canWaitCoreLocked: a wait needs an engine to read and a cold-load ceiling to
// bound it, on a run that is still going.
func (m *Monitor) canWaitCoreLocked() bool {
	return m.engine != nil && m.pol.ColdLoad > 0 && !m.stopped && m.cause == nil
}

// canWaitLocked: a NEW episode also needs a recovery left in the run's budget.
func (m *Monitor) canWaitLocked() bool {
	return m.canWaitCoreLocked() && m.pol.SeatRecoveries > m.recoveries
}

// recoverableLocked: a verdict can cancel only the call in flight (and leave
// the run to wait and re-issue) when the loop registered one and a recovery is
// left. Anything else is terminal.
func (m *Monitor) recoverableLocked() bool {
	return m.step != nil && m.canWaitLocked()
}

// parkLocked hands the run to the loop's wait: the stall and probe timers stop,
// and a callback already past its Stop finds the monitor parked and returns.
func (m *Monitor) parkLocked() {
	m.parked = true
	m.timer.Stop()
	m.stopProbeLocked()
}

// seatDownFromStallLocked retypes the stall verdicts that are a seat down: the
// engine-flat stall of the busy hold (a wedge). Every other stall stays a stall
// - a thrash (the engine keeps stepping but produces no token) is an engine
// that is alive and overloaded, not down: a re-issue would only feed it another
// request, so it stays the plain stall ADR 0061 files.
func (m *Monitor) seatDownFromStallLocked(se *StallError) *SeatDownError {
	if se.EngineFlat && !se.EngineThrash {
		return &SeatDownError{Kind: SeatDownWedged, Phase: se.Waited, Silent: se.EngineSilent, Engine: se.Engine,
			Tokens: se.Tokens, fp: m.engFP, cause: se}
	}
	return nil
}

// fileSeatDownLocked files a seat-down verdict. Recoverable (a call is in
// flight, a recovery is left): only that call is cancelled and the run is left
// alive for the loop to wait and re-issue. Otherwise it is the run's cause and
// the run ends, typed.
func (m *Monitor) fileSeatDownLocked(sd *SeatDownError) {
	if m.recoverableLocked() {
		m.down, m.downSince = sd, time.Now()
		m.parkLocked()
		m.epoch++ // a timer answer about the phase that just ended is stale from here
		m.step.cancel(sd)
		return
	}
	t := *sd
	t.terminal = true
	m.cause = &t
	m.cancel(m.cause)
}

// endLocked ends the run on a seat that did not recover, typed: it closes the
// episode, applies amend to a terminal copy of the verdict, makes it the run's
// cause and releases m.mu (which the caller holds).
func (m *Monitor) endLocked(sd *SeatDownError, amend func(*SeatDownError)) error {
	t := *sd
	t.terminal = true
	if ep := m.ep; ep != nil {
		t.Attempts, t.LastSeen = ep.attempts, ep.lastSeen
	}
	if amend != nil {
		amend(&t)
	}
	m.closeEpisodeLocked(time.Now())
	m.cause = &t
	m.cancel(m.cause)
	m.mu.Unlock()
	return &t
}

// seatCheckTimeout bounds one read of the seat made to confirm or await a
// death: a few polls, never under 3 s, never over an engine read's own bound.
func (m *Monitor) seatCheckTimeout() time.Duration {
	t := maxDur(2*m.pol.coldLoadPoll(), 3*time.Second)
	if e := m.pol.engineProbeTimeout(); t > e {
		t = e
	}
	return t
}

var errNoEngineProbe = errors.New("no engine probe is installed")

// lookSeat reads the seat's engine once, bounded, under the caller's context
// (a cancelled run reads nothing).
func (m *Monitor) lookSeat(ctx context.Context) (EngineReading, error) {
	m.mu.Lock()
	probe, timeout := m.engine, m.seatCheckTimeout()
	m.mu.Unlock()
	if probe == nil {
		return EngineReading{}, errNoEngineProbe
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rd, err := probe(cctx)
	if err != nil && ctx.Err() == nil {
		m.mu.Lock()
		m.seatReadErrs++
		m.seatReadErr = err
		m.mu.Unlock()
	}
	return rd, err
}

// SeatReadFailures is how many reads of the seat could not be answered while the
// run judged a failure or waited for the seat, and the last error. A run whose
// seat cannot be read from here gets no seat-down handling at all — a dead seat
// reads as an ordinary error — and the caller should say so.
func (m *Monitor) SeatReadFailures() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seatReadErrs, m.seatReadErr
}

// seatListedReady asks llama-swap alone (the cold-load probe's /running read)
// whether it lists the seat ready. False when there is no probe or it cannot
// tell.
func (m *Monitor) seatListedReady(ctx context.Context) bool {
	m.mu.Lock()
	probe, timeout := m.probe, m.seatCheckTimeout()
	m.mu.Unlock()
	if probe == nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	loading, state, err := probe(cctx)
	return err == nil && !loading && state == "ready"
}

// SeatCheck answers the chat client's question — is the seat not serving right
// now — from one fresh read. False (never true) when it cannot tell.
func (m *Monitor) SeatCheck(ctx context.Context) (bool, string) {
	rd, err := m.lookSeat(ctx)
	why, down := seatDownEvidence(rd, err)
	return down, why
}

// ConfirmSeatDown reads the seat after a model call failed like a dead seat
// (seatFailure) and returns the typed verdict when llama-swap confirms it:
// nil when the seat reads ready and readable, when it cannot be read, when the
// monitor has no engine probe, or when the run itself is over.
func (m *Monitor) ConfirmSeatDown(ctx context.Context, cause error) *SeatDownError {
	if ctx.Err() != nil {
		return nil
	}
	m.mu.Lock()
	if m.engine == nil || m.stopped || m.cause != nil {
		m.mu.Unlock()
		return nil
	}
	phase, tokens, engine := m.phase, m.tokens+m.callTok, m.engSum
	m.mu.Unlock()
	rd, err := m.lookSeat(ctx)
	why, down := seatDownEvidence(rd, err)
	if !down {
		return nil
	}
	return &SeatDownError{Kind: SeatDownDied, Phase: phase, Engine: engine, Note: why, Tokens: tokens, cause: cause}
}

// seatJudgement is what one reading of the seat says to a run that is waiting.
type seatJudgement int

const (
	// seatStillDown: not yet - the seat is unreadable, loading, or a wedge that has
	// not changed.
	seatStillDown seatJudgement = iota
	// seatTrigger: llama-swap does not list the seat and nothing is loading it.
	// Nobody is starting it, and llama-swap starts a stopped seat on the next
	// request, so the re-issue is what starts it and the cold-load hold covers the
	// load. It is NOT a recovery: the seat has not been seen serving.
	seatTrigger
	// seatServes: the seat is loaded and readable again.
	seatServes
)

// judgeSeat judges one reading of the seat while the run waits: may the failed
// step be re-issued now? sawDown latches a non-serving state (starting, gone) seen
// since the verdict.
//
//   - unreadable, or loading: not yet;
//   - not listed by llama-swap: a start trigger;
//   - loaded and readable: it serves, except for a WEDGE that has not changed -
//     the same frozen fingerprint with no restart seen would only feed the frozen
//     engine another request.
func judgeSeat(sd *SeatDownError, rd EngineReading, err error, sawDown *bool) seatJudgement {
	switch {
	case err != nil:
		return seatStillDown
	case rd.Loading:
		*sawDown = true
		return seatStillDown
	case rd.NotLoaded:
		*sawDown = true
		return seatTrigger
	case rd.Fingerprint == "":
		return seatStillDown
	case sd.Kind == SeatDownWedged && !*sawDown && rd.Fingerprint == sd.fp:
		return seatStillDown
	}
	return seatServes
}

// describeSeat is one reading in a clause, for what a give-up says the seat last
// looked like.
func describeSeat(rd EngineReading, err error) string {
	switch {
	case err != nil:
		return "unreadable (" + err.Error() + ")"
	case rd.Loading:
		state := rd.State
		if state == "" {
			state = "loading"
		}
		return "llama-swap lists the seat " + state
	case rd.NotLoaded:
		return "llama-swap does not list the seat"
	case rd.Fingerprint == "":
		return "loaded, its engine not readable yet"
	}
	return "loaded: " + rd.Summary
}

// AwaitSeat holds the run while its seat is down, under the cold-load hold: it
// returns nil when the failed step may be re-issued, and the typed give-up (also
// the run's cause, its context cancelled) when the seat did not come back inside
// the cold-load ceiling counted from the episode's first verdict, when the seat
// came back and went down again under the re-issued step, when the run's
// recoveries are spent, or when there is nothing to wait with (no engine probe or
// no ceiling). The loop calls it with the verdict the monitor cancelled the call
// with, or with the one ConfirmSeatDown (or the chat client) made.
//
// A re-issue onto a seat nobody lists is a start attempt, and one that fails is
// the same episode: the next call waits one poll, then triggers again, until the
// bound. Only a seat that was SEEN serving earns exactly one re-issue.
func (m *Monitor) AwaitSeat(ctx context.Context, sd *SeatDownError) error {
	m.mu.Lock()
	if m.stopped || m.cause != nil {
		err := m.cause
		m.closeEpisodeLocked(time.Now())
		m.mu.Unlock()
		if err == nil {
			err = context.Cause(ctx)
		}
		if err == nil {
			err = sd
		}
		return err
	}
	now := time.Now()
	ep := m.ep
	paced := ep != nil
	switch {
	case ep != nil && (ep.served || ep.landed):
		// The seat was seen serving again, the step was re-issued, and it went down
		// again before its answer: not another wait - a seat that comes back and dies
		// under the same request would be waited for forever.
		return m.endLocked(sd, func(t *SeatDownError) { t.Reissued = true })
	case ep == nil && !m.canWaitLocked(), ep != nil && !m.canWaitCoreLocked():
		return m.endLocked(sd, func(t *SeatDownError) { t.GaveUp = true })
	case ep == nil:
		ep = &seatEpisode{since: now}
		if m.down != nil && !m.downSince.IsZero() {
			ep.since = m.downSince // a verdict the monitor filed itself dates from then
		}
		m.ep = ep
	}
	m.down = nil // the verdict is the episode's now
	m.parkLocked()
	if m.phase == PhaseQueued {
		// The busy hold that saw the wedge: book its contention and restore the
		// phase the request waited in before the cold-load bookkeeping begins.
		m.closeQueuedLocked(now, now)
		m.leaveQueuedLocked(now)
	}
	m.resume, m.resumePending = m.phase, m.pending
	m.holdStart, m.holdClamped = ep.since, false // the bound counts from the episode's first verdict
	m.phase, m.postReady, m.readySeen = PhaseColdLoad, false, false
	m.holdState = "seat down: " + sd.brief()
	m.epoch++
	left := m.holdLeftLocked(now)
	hook := m.onHold
	m.mu.Unlock()
	notify(hook, PhaseColdLoad, left)

	poll := m.pol.coldLoadPoll()
	pace := func(left time.Duration) {
		sleep := poll
		if left < sleep {
			sleep = left
		}
		select {
		case <-ctx.Done():
		case <-time.After(sleep):
		}
	}
	if paced {
		// A re-issue that failed again is a failing start: one poll between attempts,
		// never a tight loop against a launcher that is refusing — and never past the
		// episode's bound, which may already have run out while the re-issued call was
		// failing (a start trigger is honored only inside it).
		pace(left)
		m.mu.Lock()
		if m.holdLeftLocked(time.Now()) <= 0 {
			return m.endLocked(sd, func(t *SeatDownError) { t.GaveUp, t.Waited = true, time.Since(ep.since) })
		}
		m.mu.Unlock()
	}
	for {
		rd, err := m.lookSeat(ctx)
		if ctx.Err() != nil {
			// The run ended while it waited (its ceiling, or the caller): the wait is
			// booked and the run's own cause is the answer.
			m.mu.Lock()
			m.closeEpisodeLocked(time.Now())
			m.mu.Unlock()
			return context.Cause(ctx)
		}
		m.mu.Lock()
		j := judgeSeat(sd, rd, err, &ep.sawDown)
		ep.lastSeen = describeSeat(rd, err)
		sawDown := ep.sawDown
		m.mu.Unlock()
		if j == seatStillDown && err != nil && sawDown && m.seatListedReady(ctx) {
			// The seat was seen down since the verdict and llama-swap now lists it
			// ready, but its engine cannot be read from here (a seat address this
			// box cannot reach, a metrics route that is off): llama-swap's own word
			// that the seat serves again is enough to re-issue.
			j = seatServes
		}
		if j != seatStillDown {
			m.mu.Lock()
			if j == seatServes {
				ep.served = true
			} else {
				ep.attempts++
			}
			m.warming = ep.sawDown // a restart was seen: the re-issued call's first completion is cold cost
			m.mu.Unlock()
			return nil
		}
		m.mu.Lock()
		left = m.holdLeftLocked(time.Now())
		if left <= 0 {
			return m.endLocked(sd, func(t *SeatDownError) { t.GaveUp, t.Waited = true, time.Since(ep.since) })
		}
		m.mu.Unlock()
		pace(left)
	}
}
