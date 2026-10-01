package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The seat-down episode (ADR 0066): one outage, one wait, one bound. It begins at
// the first verdict, outlives the calls that meet it, is counted when the
// re-issued call's first byte arrives, and ends the run when the seat comes back
// and dies under the same step.

// judgeSeat as a table: the pure judgement of one reading of the seat while the run
// waits. A seat llama-swap does not list is a start TRIGGER, never a recovery.
func TestJudgeSeatTable(t *testing.T) {
	died := &SeatDownError{Kind: SeatDownDied}
	wedged := &SeatDownError{Kind: SeatDownWedged, fp: "v|7"}
	ready := func(fp string) EngineReading { return EngineReading{Fingerprint: fp, TokenFingerprint: "vt"} }
	for _, tc := range []struct {
		name    string
		sd      *SeatDownError
		rd      EngineReading
		err     error
		sawIn   bool
		want    seatJudgement
		wantSaw bool
	}{
		{"unreadable", died, EngineReading{}, errors.New("x"), false, seatStillDown, false},
		{"loading latches", died, EngineReading{Loading: true, State: "starting"}, nil, false, seatStillDown, true},
		{"not listed: a start trigger, and it latches", died, EngineReading{NotLoaded: true}, nil, false, seatTrigger, true},
		{"loaded but no counters yet", died, EngineReading{}, nil, false, seatStillDown, false},
		{"loaded and readable after a death", died, ready("v|1"), nil, false, seatServes, false},
		{"wedged, same fingerprint, no restart seen", wedged, ready("v|7"), nil, false, seatStillDown, false},
		{"wedged, counters moved", wedged, ready("v|8"), nil, false, seatServes, false},
		{"wedged, a restart was seen, counters start over on the same fingerprint", wedged, ready("v|7"), nil, true, seatServes, true},
	} {
		saw := tc.sawIn
		if got := judgeSeat(tc.sd, tc.rd, tc.err, &saw); got != tc.want || saw != tc.wantSaw {
			t.Errorf("%s: judgeSeat = %v (sawDown %v), want %v (sawDown %v)", tc.name, got, saw, tc.want, tc.wantSaw)
		}
	}
}

// After the seat was seen serving again and the step re-issued, a second verdict on
// the same step is the thrash verdict at once: no second wait for a seat that comes
// back and dies under the same request.
func TestAwaitSeatAfterTheSeatWasSeenServingIsTheThrashVerdict(t *testing.T) {
	eng := newDownEngine("ready")
	eng.set("ready", true) // working: the seat serves
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "first"}); err != nil {
		t.Fatalf("first AwaitSeat = %v", err)
	}
	m.Phase(PhaseDecoding, 0) // the re-issued step, which fails before any byte
	begin := time.Now()
	err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "second"})
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.Reissued || sd.GaveUp || time.Since(begin) > 500*time.Millisecond {
		t.Fatalf("second AwaitSeat = %v, want the typed re-issued verdict at once", err)
	}
	if m.SeatRecoveries() != 0 {
		t.Fatalf("SeatRecoveries = %d: nothing landed", m.SeatRecoveries())
	}
}

// The re-issued call's first byte is the seat serving: the recovery is counted and
// the wait booked at that moment, and a later verdict on the same call is the thrash.
func TestFirstByteOfTheReissuedCallLandsTheRecovery(t *testing.T) {
	eng := newDownEngine("gone")
	p := recoveryPolicy(5 * time.Second)
	p.Floor = 5 * time.Second // the re-issued call is held for a moment: no stall clock of its own
	ctx, m := NewMonitor(context.Background(), p, 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "gone"}); err != nil {
		t.Fatal(err) // a start trigger: the seat is unlisted
	}
	if m.SeatRecoveries() != 0 {
		t.Fatalf("a start trigger is not a recovery: SeatRecoveries = %d", m.SeatRecoveries())
	}
	m.Phase(PhasePrefill, 10) // the re-issued call
	time.Sleep(40 * time.Millisecond)
	m.Progress(5) // its first byte
	if m.SeatRecoveries() != 1 {
		t.Fatalf("SeatRecoveries = %d after the first byte, want 1", m.SeatRecoveries())
	}
	booked := m.SeatDownTotal()
	time.Sleep(60 * time.Millisecond)
	if booked < 40*time.Millisecond || m.SeatDownTotal() != booked {
		t.Fatalf("SeatDownTotal = %s then %s: the wait must be booked at the first byte and stop growing", booked, m.SeatDownTotal())
	}
	// The stream is cut after that byte: the seat came back and died under the same
	// step, so the run ends typed rather than waiting again.
	err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "cut"})
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.Reissued {
		t.Fatalf("AwaitSeat after a landed recovery = %v, want the re-issued verdict", err)
	}
}

// An episode is counted ONCE, however many deltas the re-issued call streams and
// however it is then answered: the first byte lands it, the rest are no-ops.
func TestOneEpisodeIsCountedOnce(t *testing.T) {
	eng := newDownEngine("gone")
	p := recoveryPolicy(5 * time.Second)
	p.Floor = 5 * time.Second
	ctx, m := NewMonitor(context.Background(), p, 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "gone"}); err != nil {
		t.Fatal(err)
	}
	m.Phase(PhasePrefill, 10)
	m.Progress(5)
	booked := m.SeatDownTotal()
	time.Sleep(30 * time.Millisecond)
	m.Progress(9)
	m.Progress(14)
	m.SeatAnswered()
	m.SeatAnswered()
	if m.SeatRecoveries() != 1 {
		t.Fatalf("SeatRecoveries = %d after three deltas and two answers, want the episode counted once", m.SeatRecoveries())
	}
	if m.SeatDownTotal() != booked {
		t.Fatalf("SeatDownTotal = %s, want %s: the wait is booked once, at the first byte", m.SeatDownTotal(), booked)
	}
}

// A run that ends while the episode is open books the wait it cost and counts no
// recovery.
func TestStopBooksAnOpenEpisode(t *testing.T) {
	eng := newDownEngine("gone")
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "gone"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	m.Stop()
	booked := m.SeatDownTotal()
	time.Sleep(60 * time.Millisecond)
	if booked < 40*time.Millisecond || m.SeatDownTotal() != booked || m.SeatRecoveries() != 0 {
		t.Fatalf("SeatDownTotal = %s then %s, recoveries %d: Stop must book the open wait and freeze it", booked, m.SeatDownTotal(), m.SeatRecoveries())
	}
}

// The episode's bound counts from its FIRST verdict: a second wait inside it does not
// restart the clock, or a failing start would be waited for at the ceiling once per
// attempt.
func TestEpisodeBoundCountsFromTheFirstVerdict(t *testing.T) {
	eng := newDownEngine("gone")
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(500*time.Millisecond), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "gone"}); err != nil {
		t.Fatal(err) // the start trigger, at once
	}
	eng.set("starting", false) // the start began, and never finishes
	time.Sleep(350 * time.Millisecond)
	m.Phase(PhaseDecoding, 0) // the re-issued call failed
	begin := time.Now()
	err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "failed start"})
	el := time.Since(begin)
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.GaveUp {
		t.Fatalf("AwaitSeat = %v, want a typed give-up", err)
	}
	if el > 300*time.Millisecond {
		t.Fatalf("the second wait took %s: the 500 ms bound must count from the FIRST verdict (350 ms ago), not restart", el)
	}
	if sd.Waited < 450*time.Millisecond {
		t.Fatalf("Waited = %s, want the whole episode (~500 ms)", sd.Waited)
	}
}

// A seat that came back (or is started by the re-issue) leaves the monitor warming:
// the re-issued call's first completion carries the load, and the post-ready
// grace is what keeps it from being called a stall or a wedge again while the
// restarted engine serves its first request.
func TestARecoveredSeatLeavesTheMonitorWarming(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start string
		flip  bool
	}{
		{"a restart was seen: starting, then ready", "starting", true},
		{"the seat was gone: the re-issue starts it", "gone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := newDownEngine(tc.start)
			ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
			defer m.Stop()
			m.WithEngineProbe(eng.probe)
			m.Phase(PhaseDecoding, 0)
			if tc.flip {
				go func() {
					time.Sleep(120 * time.Millisecond)
					eng.restart()
				}()
			}
			if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "x"}); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			warming := m.warming
			m.mu.Unlock()
			if !warming {
				t.Fatal("a seat that was seen down and came back must leave the monitor warming: the re-issued call's first completion carries the load")
			}
		})
	}
	// Control: a seat that was never seen down (a blip already over when the wait
	// looked) is not a cold start.
	eng := newDownEngine("ready")
	eng.set("ready", true)
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "blip"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	warming := m.warming
	m.mu.Unlock()
	if warming {
		t.Fatal("control: a seat that was never seen down is not warming")
	}
}

// The caller cancels the run while it waits for the seat: the wait is booked and the
// answer is the caller's cancellation — never a typed seat-down, which the delegator
// would re-place, or a run that was withdrawn would be run again elsewhere.
func TestACancelledRunWhileWaitingForTheSeatIsBookedAndNotTyped(t *testing.T) {
	eng := newDownEngine("starting") // never finishes
	parent, cancel := context.WithCancel(context.Background())
	ctx, m := NewMonitor(parent, recoveryPolicy(10*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()
	err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "restart"})
	var sd *SeatDownError
	if err == nil || errors.As(err, &sd) || !errors.Is(err, context.Canceled) {
		t.Fatalf("AwaitSeat = %#v, want the caller's cancellation (context.Canceled), not a seat-down", err)
	}
	if w := m.SeatDownTotal(); w < 150*time.Millisecond {
		t.Fatalf("SeatDownTotal = %s, want the ~250 ms waited before the cancel booked", w)
	}
	if m.SeatRecoveries() != 0 {
		t.Fatalf("SeatRecoveries = %d: a cancelled wait recovered nothing", m.SeatRecoveries())
	}
	booked := m.SeatDownTotal()
	time.Sleep(60 * time.Millisecond)
	if m.SeatDownTotal() != booked {
		t.Fatalf("SeatDownTotal grew from %s to %s after the run ended: the wait must be booked and closed", booked, m.SeatDownTotal())
	}
}

// The wait a run is spending on a dead seat right now is visible while it runs.
func TestSeatDownTotalCountsAWaitInProgress(t *testing.T) {
	eng := newDownEngine("starting")
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	done := make(chan error, 1)
	go func() { done <- m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "restart"}) }()
	time.Sleep(300 * time.Millisecond)
	if w := m.SeatDownTotal(); w < 200*time.Millisecond {
		t.Fatalf("SeatDownTotal = %s while the wait has been running for ~300 ms", w)
	}
	eng.restart()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A run recovers at most twice, counted per outage: two deaths on two steps are two
// recoveries (the failed step is re-issued, the tool step runs, the next step dies
// and is re-issued), and the run finishes. Every other loop test ends after one.
func TestTwoDeathsOnTwoStepsAreTwoRecoveries(t *testing.T) {
	eng := newDownEngine("gone")
	tool := func() (Completion, error) {
		return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{}`)}}, FinishReason: "tool_calls"}, nil
	}
	c := &seatDownClient{script: []func() (Completion, error){seatDownAt(SeatDownDied), tool, seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, mkTools("echo"), 4).WithLiveness(m).Run(ctx, "x")
	if err != nil || res.StopReason != "done" || c.calls != 4 || res.SeatRecoveries != 2 || res.Steps != 2 {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d steps=%d, want fail, tool step, fail, done = 2 recoveries and 2 steps", err, res.StopReason, c.calls, res.SeatRecoveries, res.Steps)
	}
}

// ...and the third outage of the run is refused at once: the budget of recoveries
// counts outages that landed, and a run past it does not wait.
func TestAThirdOutageInOneRunEndsTypedWithoutWaiting(t *testing.T) {
	eng := newDownEngine("gone")
	tool := func() (Completion, error) {
		return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{}`)}}, FinishReason: "tool_calls"}, nil
	}
	c := &seatDownClient{script: []func() (Completion, error){
		seatDownAt(SeatDownDied), tool, seatDownAt(SeatDownDied), tool, seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	begin := time.Now()
	res, err := NewLoop(c, mkTools("echo"), 6).WithLiveness(m).Run(ctx, "x")
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.GaveUp || sd.Waited != 0 || !strings.Contains(sd.Error(), "no seat recovery left") {
		t.Fatalf("err = %v, want a typed refusal that names the spent budget", err)
	}
	if res.SeatRecoveries != 2 || c.calls != 5 || time.Since(begin) > 2*time.Second {
		t.Fatalf("recoveries=%d calls=%d after %s, want two recoveries, five calls and no wait for the third", res.SeatRecoveries, c.calls, time.Since(begin))
	}
}

// A seat that cannot be read from here gets no seat-down handling at all: a dead
// seat reads as an ordinary error. The reads that could not be answered are kept, so
// the run can say so — and a read the run itself cut short is not a failure of the seat.
func TestSeatReadFailuresAreKept(t *testing.T) {
	unreadable := func(context.Context) (EngineReading, error) {
		return EngineReading{}, errors.New("metrics: connection timed out")
	}
	_, m := NewMonitor(context.Background(), recoveryPolicy(time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(unreadable)
	m.Phase(PhaseDecoding, 0)
	if n, err := m.SeatReadFailures(); n != 0 || err != nil {
		t.Fatalf("SeatReadFailures = %d, %v before any read", n, err)
	}
	if sd := m.ConfirmSeatDown(context.Background(), errors.New("stream read: unexpected EOF")); sd != nil {
		t.Fatalf("an unreadable seat was called down: %v", sd)
	}
	if down, _ := m.SeatCheck(context.Background()); down {
		t.Fatal("an unreadable seat was called down by the seat check")
	}
	n, err := m.SeatReadFailures()
	if n != 2 || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("SeatReadFailures = %d, %v, want the two reads that could not be answered and the last error", n, err)
	}
	// A read the run itself cancelled says nothing about the seat.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = m.SeatCheck(cancelled)
	if n2, _ := m.SeatReadFailures(); n2 != 2 {
		t.Fatalf("SeatReadFailures = %d after a cancelled read, want it still 2", n2)
	}
}
