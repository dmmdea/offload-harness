package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// What a cold-load hold does at the end of its current part (the post-ready
// bound, or the cold-load ceiling) while the request is still silent. Two
// callers decide it: the stall timer, which has no seat read of its own and
// reads the engine (a seat that is loading comes back from that read as
// Loading), and the seat probe's tick, which has just read the seat itself.
// For the same seat state they must reach the same verdict.

// seatAnswer is one scripted answer of the seat probe.
type seatAnswer struct {
	loading bool
	state   string
	err     error
}

var (
	seatReady      = seatAnswer{false, "ready", nil}
	seatStarting   = seatAnswer{true, "starting", nil}
	seatStopping   = seatAnswer{true, "stopping", nil}
	seatUnreadable = seatAnswer{false, "", errors.New("running unreadable")}
)

// spentBoundSeat is a SeatProbe for the races at a spent hold bound. Its first
// read made while the monitor is in the cold-load phase straddles the bound: it
// returns only once the hold's current part has run out (the post-ready part
// when wantPost, else the cold-load ceiling), waiting on the monitor's own
// state rather than on a sleep, and then answers `first`. Reads before it answer
// `before`, reads after it `after`, each at once.
type spentBoundSeat struct {
	m                    *Monitor
	wantPost             bool
	before, first, after seatAnswer
	slowed, spent        atomic.Bool
}

func (s *spentBoundSeat) probe(context.Context) (bool, string, error) {
	if s.m.CurrentPhase() == PhaseColdLoad && s.slowed.CompareAndSwap(false, true) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			s.m.mu.Lock()
			left, post := s.m.holdLeftLocked(time.Now()), s.m.postReady
			s.m.mu.Unlock()
			if post == s.wantPost && left <= 0 {
				s.spent.Store(true)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return s.first.loading, s.first.state, s.first.err
	}
	a := s.before
	if s.slowed.Load() {
		a = s.after
	}
	return a.loading, a.state, a.err
}

// requireStraddled fails a test that never reached the race (a false red, never
// a false pass).
func (s *spentBoundSeat) requireStraddled(t *testing.T, ctx context.Context) {
	t.Helper()
	if !s.slowed.Load() {
		t.Fatalf("the test never made a cold-load seat read slow; it proves nothing (phase %s, cause %v)", s.m.CurrentPhase(), context.Cause(ctx))
	}
	if !s.spent.Load() && ctx.Err() == nil {
		t.Fatal("the hold's bound never ran out during the seat read; it proves nothing")
	}
}

// straddlePolicy: a 1 s post-ready part, so the first cold-load tick starts far
// inside it however late the scheduler runs it (see the engine race test).
func straddlePolicy(coldLoad time.Duration) StallPolicy {
	p := busyPolicy()
	p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll, p.PostReady = coldLoad, "test ceiling", 10*time.Millisecond, time.Second
	return p
}

// startStraddle runs a request into the hold with a seat whose first cold-load
// read straddles the bound and an engine that works for the request's siblings.
func startStraddle(t *testing.T, p StallPolicy, wantPost bool, before, first, after seatAnswer) (context.Context, *Monitor, *spentBoundSeat, *scriptedEngine) {
	t.Helper()
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	ctx, m := NewMonitor(context.Background(), p, 10*time.Minute)
	t.Cleanup(m.Stop)
	seat := &spentBoundSeat{m: m, wantPost: wantPost, before: before, first: first, after: after}
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(seat.probe, nil)
	if wantPost {
		m.MarkSeatLoaded() // the admission warm-up loaded the seat: the hold starts in its post-ready part
	}
	m.Phase(PhasePrefill, 10)
	return ctx, m, seat, eng
}

// awaitHold polls until cond holds, the run is cancelled or a generous deadline
// passes: tests wait for the outcome, never for a fixed time.
func awaitHold(ctx context.Context, cond func() bool) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil && !cond() {
		time.Sleep(5 * time.Millisecond)
	}
}

type holdView struct {
	phase     Phase
	postReady bool
	holdStart time.Time
	holdState string
	left      time.Duration
}

func peekHold(m *Monitor) holdView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return holdView{m.phase, m.postReady, m.holdStart, m.holdState, m.holdLeftLocked(time.Now())}
}

// The post-ready bound runs out while the tick's read of the seat is in flight,
// and that read says the seat is LOADING again (evicted after it read ready).
// A load is the cold-load ceiling's: the request goes back to the loading part,
// counted from its first silence, exactly as when the same read lands one poll
// earlier. It must not be stalled while that ceiling has time left.
func TestSpentPostReadyBoundWithTheSeatLoadingReturnsToTheLoadingHold(t *testing.T) {
	p := straddlePolicy(30 * time.Second)
	ctx, m, seat, _ := startStraddle(t, p, true, seatReady, seatStarting, seatStarting)
	_, _, firstSilence, _ := m.Snapshot()
	awaitHold(ctx, func() bool {
		v := peekHold(m)
		return seat.spent.Load() && v.phase == PhaseColdLoad && !v.postReady
	})
	seat.requireStraddled(t, ctx)
	if ctx.Err() != nil {
		t.Fatalf("a post-ready request whose straddling seat read said loading was stalled with %s of the cold-load ceiling unspent: %v",
			(p.ColdLoad - time.Since(firstSilence)).Round(time.Second), context.Cause(ctx))
	}
	v := peekHold(m)
	if v.phase != PhaseColdLoad || v.postReady || v.holdState != "starting" {
		t.Fatalf("phase=%s postReady=%v seat=%q, want the loading part of the cold-load hold on a seat that reads starting", v.phase, v.postReady, v.holdState)
	}
	if !v.holdStart.Equal(firstSilence) {
		t.Fatalf("the cold-load ceiling must count from the first silence (%s), not restart at %s", firstSilence, v.holdStart)
	}
	if _, _, _, allow := m.Snapshot(); allow <= 0 || allow > p.ColdLoad-p.PostReady {
		t.Fatalf("stall timer re-armed for %s: the ceiling counts from the first silence, so at most %s remain after a 1 s post-ready part", allow, p.ColdLoad-p.PostReady)
	}
}

// A load that never ends is still bounded: after the loading read at a spent
// post-ready bound the request stalls at the cold-load ceiling, counted from its
// first silence, as a cold-load stall (not as a post-ready one).
func TestLoadingAfterASpentPostReadyBoundStallsAtTheColdLoadCeiling(t *testing.T) {
	p := straddlePolicy(2 * time.Second)
	ctx, _, seat, _ := startStraddle(t, p, true, seatReady, seatStarting, seatStarting)
	awaitHold(ctx, func() bool { return false }) // until the run is stalled, or the deadline
	seat.requireStraddled(t, ctx)
	var se *StallError
	if !errors.As(context.Cause(ctx), &se) {
		t.Fatalf("cause = %v, want a stall at the cold-load ceiling", context.Cause(ctx))
	}
	if se.Phase != PhaseColdLoad || se.PostReady {
		t.Fatalf("stalled in phase %s (post-ready=%v) at %s, want the cold-load ceiling's stall once it has run out: %v", se.Phase, se.PostReady, se.Silent.Round(10*time.Millisecond), se)
	}
	if se.Silent < p.ColdLoad-10*time.Millisecond {
		t.Fatalf("stalled after %s, before the %s cold-load ceiling ran out: %v", se.Silent, p.ColdLoad, se)
	}
}

// The reason must name what the last seat read said. A request whose last read
// said the seat is loading is never reported as one whose seat "read ready".
func TestLoadingAfterASpentPostReadyBoundNeverClaimsTheSeatReadReady(t *testing.T) {
	p := straddlePolicy(2 * time.Second)
	ctx, _, seat, _ := startStraddle(t, p, true, seatReady, seatStopping, seatStopping)
	awaitHold(ctx, func() bool { return false })
	seat.requireStraddled(t, ctx)
	if ctx.Err() == nil {
		t.Fatal("the run was never stalled; it proves nothing")
	}
	msg := context.Cause(ctx).Error()
	if strings.Contains(msg, "seat read ready") {
		t.Fatalf("the last seat read said stopping, but the reason says the seat read ready: %s", msg)
	}
	if !strings.Contains(msg, "seat still loading") || !strings.Contains(msg, `"stopping"`) {
		t.Fatalf("reason = %s, want it to say the seat is still loading and name what it read (stopping)", msg)
	}
}

// The straddling read cannot be read at all (/running unreadable): that says
// nothing about the engine, so the expiry reads the engine exactly like the
// stall timer, and a request whose engine works for its siblings waits its turn.
func TestSpentPostReadyBoundWithTheSeatUnreadableReadsTheEngine(t *testing.T) {
	ctx, m, seat, eng := startStraddle(t, straddlePolicy(5*time.Second), true, seatReady, seatUnreadable, seatReady)
	awaitHold(ctx, func() bool { return seat.spent.Load() && m.CurrentPhase() == PhaseQueued })
	seat.requireStraddled(t, ctx)
	if ctx.Err() != nil {
		t.Fatalf("a post-ready bound that expired during an unreadable seat read stalled a request whose engine works for its siblings: %v", context.Cause(ctx))
	}
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s, want the busy hold after the post-ready bound", m.CurrentPhase())
	}
	if eng.reads.Load() == 0 {
		t.Fatal("the engine was never read")
	}
}

// The cold-load ceiling is not the post-ready bound: once it has run out the
// request stalls, whatever the seat reads now and whatever the engine is doing
// for others. (Unchanged by the fix; pinned here.)
func TestSpentColdLoadCeilingStillStallsWithoutReadingTheEngine(t *testing.T) {
	p := straddlePolicy(400 * time.Millisecond)
	ctx, _, seat, eng := startStraddle(t, p, false, seatStarting, seatReady, seatReady)
	awaitHold(ctx, func() bool { return false })
	seat.requireStraddled(t, ctx)
	var se *StallError
	if !errors.As(context.Cause(ctx), &se) {
		t.Fatalf("cause = %v, want the cold-load ceiling's stall", context.Cause(ctx))
	}
	if se.Phase != PhaseColdLoad || se.PostReady || !strings.Contains(se.Error(), "seat still loading") {
		t.Fatalf("stall = %v (phase %s, post-ready %v), want the cold-load ceiling's", se, se.Phase, se.PostReady)
	}
	if n := eng.reads.Load(); n != 0 {
		t.Fatalf("the engine was read %d times at a spent cold-load ceiling", n)
	}
}

type spentOutcome struct {
	stalled   bool
	phase     Phase
	postReady bool
}

// newSpentHold builds a monitor in the cold-load hold whose current part ran out
// long ago, with no timer and no probe tick about to fire (every duration is
// minutes or hours): the test calls the tick or the stall timer's callback by
// hand, so the decision is the only thing under test. postReady false means the
// cold-load ceiling is the part that ran out; engineArmed false means no busy
// hold (EngineFlat 0).
func newSpentHold(t *testing.T, postReady, engineArmed bool, eng *scriptedEngine, seat SeatProbe) *Monitor {
	t.Helper()
	p := busyPolicy()
	p.Admission, p.Floor = time.Minute, time.Minute
	p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll, p.PostReady = time.Hour, "test ceiling", time.Hour, time.Minute
	if !engineArmed {
		p.EngineFlat = 0
	}
	_, m := NewMonitor(context.Background(), p, 24*time.Hour)
	t.Cleanup(m.Stop)
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(seat, nil)
	m.Phase(PhasePrefill, 10)
	m.mu.Lock()
	now := time.Now()
	m.phase, m.resume, m.resumePending = PhaseColdLoad, PhasePrefill, 10
	m.epoch++
	m.warming = true
	if postReady {
		// Read ready ten minutes ago: the post-ready bound is max(1 min, 2 x the
		// 1 min prefill allowance) = 2 min, long spent; the 1 h ceiling is not.
		m.holdStart, m.readyAt = now.Add(-10*time.Minute), now.Add(-10*time.Minute)
		m.postReady, m.readySeen, m.holdState = true, true, warmupState
	} else {
		m.holdStart, m.holdState = now.Add(-2*time.Hour), "starting" // the 1 h ceiling ran out an hour ago
	}
	m.mu.Unlock()
	return m
}

func outcomeOf(m *Monitor) (spentOutcome, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cause := ""
	if m.cause != nil {
		cause = m.cause.Error()
	}
	return spentOutcome{m.cause != nil, m.phase, m.postReady}, cause
}

// The decision table of a spent hold part, seat state by caller. The tick is
// called with the seat reading `seat`; the stall timer with an engine that
// reports the same thing (the engine probe wraps the seat probe: a seat that is
// loading comes back as Loading, anything else is the engine's own reading, and
// here the engine works for the request's siblings).
func TestSpentHoldBoundTickDecidesLikeTheStallTimer(t *testing.T) {
	loadingHold := spentOutcome{false, PhaseColdLoad, false}
	busyHold := spentOutcome{false, PhaseQueued, false}
	postReadyStall := spentOutcome{true, PhaseColdLoad, true}
	ceilingStall := spentOutcome{true, PhaseColdLoad, false}
	rows := []struct {
		name        string
		postReady   bool
		engineArmed bool
		seat        seatAnswer
		want        spentOutcome
		timerSkips  bool // the timer never reads the seat without an engine probe: no like-for-like call
	}{
		{"post-ready spent, seat loading", true, true, seatStarting, loadingHold, false},
		{"post-ready spent, seat ready", true, true, seatReady, busyHold, false},
		{"post-ready spent, seat unreadable", true, true, seatUnreadable, busyHold, false},
		{"post-ready spent, no engine probe, seat loading", true, false, seatStarting, loadingHold, true},
		{"post-ready spent, no engine probe, seat ready", true, false, seatReady, postReadyStall, false},
		{"post-ready spent, no engine probe, seat unreadable", true, false, seatUnreadable, postReadyStall, false},
		{"cold-load ceiling spent, seat loading", false, true, seatStarting, ceilingStall, false},
		{"cold-load ceiling spent, seat ready", false, true, seatReady, ceilingStall, false},
		{"cold-load ceiling spent, seat unreadable", false, true, seatUnreadable, ceilingStall, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			// The tick: its own seat read decides.
			tickEng := &scriptedEngine{}
			tickEng.advance.Store(true)
			tick := newSpentHold(t, r.postReady, r.engineArmed, tickEng, func(context.Context) (bool, string, error) {
				return r.seat.loading, r.seat.state, r.seat.err
			})
			tick.onProbeTick()
			got, cause := outcomeOf(tick)
			if got != r.want {
				t.Fatalf("tick: got %+v, want %+v (cause %q)", got, r.want, cause)
			}
			if !r.postReady && tickEng.reads.Load() != 0 {
				t.Fatalf("tick read the engine %d times at a spent cold-load ceiling", tickEng.reads.Load())
			}
			if r.timerSkips {
				return
			}
			// The timer: the engine says what the seat says.
			timerEng := &scriptedEngine{}
			timerEng.advance.Store(true)
			timerEng.loading.Store(r.seat.loading)
			timer := newSpentHold(t, r.postReady, r.engineArmed, timerEng, func(context.Context) (bool, string, error) {
				return false, "ready", nil
			})
			timer.onStall()
			if via, viaCause := outcomeOf(timer); via != got {
				t.Fatalf("the timer decided %+v (cause %q) where the tick decided %+v for the same seat state", via, viaCause, got)
			}
		})
	}
}

// The stall timer's callback can be on its way, waiting for the monitor's lock,
// when the probe tick takes the request back to the loading part of the hold
// (re-arming the timer for the longer ceiling). It then finds the part with time
// left. It is not due: acting on it would stall a request the tick has just held
// with most of the cold-load ceiling unspent.
func TestStallTimerCallbackFindingTimeLeftInTheHoldIsNotDue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		postReady bool
		want      spentOutcome
	}{
		{"loading part with ceiling left", false, spentOutcome{false, PhaseColdLoad, false}},
		{"post-ready part with bound left", true, spentOutcome{false, PhaseColdLoad, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &scriptedEngine{}
			eng.advance.Store(true)
			m := newSpentHold(t, tc.postReady, true, eng, func(context.Context) (bool, string, error) { return false, "ready", nil })
			m.mu.Lock()
			now := time.Now()
			if tc.postReady {
				m.readyAt = now.Add(-10 * time.Second) // the bound is 2 min
			} else {
				m.holdStart = now.Add(-10 * time.Minute) // the ceiling is 1 h
			}
			m.mu.Unlock()
			m.onStall()
			if got, cause := outcomeOf(m); got != tc.want {
				t.Fatalf("a timer callback that found time left in the hold decided %+v, want %+v (cause %q)", got, tc.want, cause)
			}
			if n := eng.reads.Load(); n != 0 {
				t.Fatalf("the engine was read %d times for a callback that was not due", n)
			}
		})
	}
}
