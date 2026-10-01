package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Guards of the seat-down machinery that a one-line mutation used to slip past
// (ADR 0066): the verdict on a production-configured monitor with no step in
// flight, which failed calls are nominated for the seat check, the two-sighting
// rule of the busy hold, and the un-parking and run-over guards.

// The structured re-pack runs on the monitor with NO loop step in flight (m.step is
// set only around loop model calls), while production arms recoveries and an
// engine probe. A wedge filed then must end the run typed and terminal: there is
// nothing in flight to cancel and nothing to re-issue, and an unguarded verdict
// would dereference the missing step scope on a timer goroutine — a process crash.
// (TestRepackSeatDownPrefersTheMonitorsWedgeVerdict builds its monitor without
// SeatRecoveries, so its verdict is terminal for the wrong reason.)
func TestWedgeDuringTheRepackOnAProductionConfiguredMonitorEndsTerminal(t *testing.T) {
	eng := newDownEngine("ready") // frozen counters, 5 running
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseRepack, 0) // no beginStep: the re-pack is not a loop step
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a frozen engine under the re-pack was never declared down")
	}
	var sd *SeatDownError
	if !errors.As(m.Cause(), &sd) || !sd.terminal || sd.Kind != SeatDownWedged {
		t.Fatalf("cause = %#v, want a terminal wedged seat-down (nothing is in flight to re-issue)", m.Cause())
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// Which failed model calls are nominated for the seat check. A nomination costs one
// read of the seat and can end in a seat-down verdict, so it must be the failures
// an engine death produces — a 5xx, a dropped or refused connection, a cut stream —
// and never the caller's own (a 4xx, a cancellation, the run's deadline, a timeout).
func TestSeatFailureNominations(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"cancelled run", context.Canceled, false},
		{"run deadline", context.DeadlineExceeded, false},
		{"500 from the engine", &StatusError{Code: 500, Body: "EngineDeadError"}, true},
		{"502", &StatusError{Code: 502, Body: "bad gateway"}, true},
		{"503", &StatusError{Code: 503, Body: "loading"}, true},
		{"404 is the caller's", &StatusError{Code: 404, Body: "model not found"}, false},
		{"400 is the caller's", &StatusError{Code: 400, Body: "bad request"}, false},
		{"dial refused", &url.Error{Op: "Post", URL: "http://192.0.2.1", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}, true},
		{"client timeout is not a dead seat", &url.Error{Op: "Post", URL: "http://192.0.2.1", Err: timeoutErr{}}, false},
		{"net timeout is not a dead seat", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}, false},
		{"bare net error", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"EOF", io.EOF, true},
		{"cut stream", errors.New("stream read: unexpected EOF"), true},
		{"stream without a finish", errors.New("stream ended without a finish_reason or [DONE]"), true},
		{"engine error frame", errors.New("engine error in stream: EngineCore died"), true},
		{"ordinary failure", errors.New("json: cannot unmarshal"), false},
	} {
		if got := seatFailure(tc.err); got != tc.want {
			t.Errorf("%s: seatFailure(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// Through the loop: a plain 5xx from the engine (not llama-swap's own retryable
// one, so the chat client's seat check never sees it) on a seat llama-swap reads as
// restarting is a seat down and is recovered.
func TestLoopRecoversFromA5xxOnARestartingSeat(t *testing.T) {
	eng := newDownEngine("starting")
	go func() {
		time.Sleep(150 * time.Millisecond)
		eng.restart()
	}()
	boom := func() (Completion, error) { return Completion{}, &StatusError{Code: 500, Body: "EngineDeadError"} }
	c := &seatDownClient{script: []func() (Completion, error){boom, doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 1).WithLiveness(m).Run(ctx, "x")
	if err != nil || res.StopReason != "done" || c.calls != 2 || res.SeatRecoveries != 1 {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d, want the 500 confirmed against the restarting seat and re-issued", err, res.StopReason, c.calls, res.SeatRecoveries)
	}
}

// ...and a refused connection (the dial fails outright) on a seat llama-swap lists
// as gone.
func TestLoopRecoversFromARefusedConnection(t *testing.T) {
	eng := newDownEngine("gone")
	refused := func() (Completion, error) {
		return Completion{}, &url.Error{Op: "Post", URL: "http://192.0.2.1/v1/chat/completions", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}
	}
	c := &seatDownClient{script: []func() (Completion, error){refused, doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 1).WithLiveness(m).Run(ctx, "x")
	if err != nil || res.StopReason != "done" || c.calls != 2 || res.SeatRecoveries != 1 {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d, want the refused dial confirmed and re-issued", err, res.StopReason, c.calls, res.SeatRecoveries)
	}
}

// The overflow retry's own model call can hit a dead seat too: it is recovered like
// the first call, not returned as a raw error.
func TestOverflowRetryThatHitsADeadSeatIsRecovered(t *testing.T) {
	eng := newDownEngine("gone")
	overflow := func() (Completion, error) {
		return Completion{}, &StatusError{Code: 400, Body: "context length exceeded"}
	}
	c := &seatDownClient{script: []func() (Completion, error){overflow, seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 2).WithLiveness(m).Run(ctx, "x")
	if err != nil || res.StopReason != "done" || c.calls != 3 || res.SeatRecoveries != 1 {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d, want overflow -> retry hits a dead seat -> recovered -> done", err, res.StopReason, c.calls, res.SeatRecoveries)
	}
}

// One sighting of the seat missing from llama-swap's /running inside an established
// hold may be a transition (a swap, a restart of llama-swap itself); two in a row
// are a dead engine. A single absence among readings that keep advancing must not
// end the run.
func TestOneTransientAbsenceInsideAHoldIsNotSeatDown(t *testing.T) {
	var reads, steps atomic.Int64
	probe := func(context.Context) (EngineReading, error) {
		n := reads.Add(1)
		if n == 12 { // exactly one read finds the seat unlisted, well after the hold is established
			return EngineReading{NotLoaded: true, Summary: "seat not loaded"}, nil
		}
		s := steps.Add(1)
		return EngineReading{Fingerprint: "v|" + strconv.FormatInt(s, 10), TokenFingerprint: "vt|" + strconv.FormatInt(s, 10),
			Summary: "vllm-metrics: 4 running, 0 waiting", Running: 4}, nil
	}
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(1200 * time.Millisecond) // ~60 reads at a 20 ms poll
	if reads.Load() < 20 {
		t.Fatalf("only %d engine reads in 1.2 s: the fixture did not run", reads.Load())
	}
	if ctx.Err() != nil {
		t.Fatalf("a single transient absence declared the seat down: %v", context.Cause(ctx))
	}
	if m.CurrentPhase() != PhaseQueued {
		t.Fatalf("phase = %s, want the hold back in %s once the seat reads again", m.CurrentPhase(), PhaseQueued)
	}
}

// Absences that are not consecutive never add up: a reading with counters resets
// the count.
func TestNonConsecutiveAbsencesDoNotAddUp(t *testing.T) {
	var reads, steps atomic.Int64
	probe := func(context.Context) (EngineReading, error) {
		n := reads.Add(1)
		if n > 10 && n%2 == 0 { // gone, ready, gone, ready ... after the hold is established
			return EngineReading{NotLoaded: true, Summary: "seat not loaded"}, nil
		}
		s := steps.Add(1)
		return EngineReading{Fingerprint: "v|" + strconv.FormatInt(s, 10), TokenFingerprint: "vt|" + strconv.FormatInt(s, 10),
			Summary: "vllm-metrics: 4 running, 0 waiting", Running: 4}, nil
	}
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(1200 * time.Millisecond)
	if reads.Load() < 30 {
		t.Fatalf("only %d engine reads in 1.2 s", reads.Load())
	}
	if ctx.Err() != nil {
		t.Fatalf("alternating absent/present readings declared the seat down: %v", context.Cause(ctx))
	}
}

// The tool phase un-parks a monitor whose seat-down verdict lost the race to a
// completed call (Phase does the same, pinned by TestPhaseUnparksAMonitorWhoseVerdictWasMoot).
// A parked monitor's stall timer returns at once, so a tool that then hangs would
// never be stalled.
func TestToolPhaseUnparksAMonitorWhoseVerdictWasMoot(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)
	m.mu.Lock()
	m.down, m.downSince = &SeatDownError{Kind: SeatDownWedged}, time.Now()
	m.parkLocked()
	m.mu.Unlock()
	m.ToolPhase(0) // the call completed after all and the loop runs its tool
	select {       // the stall watch is live again: a hung tool is stalled
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the tool phase never stalled a silent tool: the monitor stayed parked")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) {
		t.Fatalf("cause = %#v, want the tool stall", m.Cause())
	}
}

// The probe's own tick is a timer callback too: one already past its Stop when the
// loop parks the monitor must not read the seat and move the phase of a request
// that no longer exists.
func TestProbeTickIgnoresAParkedMonitor(t *testing.T) {
	var asked atomic.Int64
	seat := func(context.Context) (bool, string, error) {
		asked.Add(1)
		return true, "starting", nil // llama-swap says the seat is loading
	}
	_, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithSeatProbe(seat, nil)
	m.Phase(PhasePrefill, 10) // awaiting a byte: the tick would read the seat
	m.mu.Lock()
	m.parkLocked()
	m.mu.Unlock()
	m.onProbeTick() // the callback that raced the Stop
	if asked.Load() != 0 || m.CurrentPhase() != PhasePrefill {
		t.Fatalf("a parked monitor's probe tick read the seat %d time(s) and left phase %s: it must return untouched", asked.Load(), m.CurrentPhase())
	}
	// Control: the same tick on an unparked monitor reads the seat and holds the run.
	_, live := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer live.Stop()
	live.WithSeatProbe(seat, nil)
	live.Phase(PhasePrefill, 10)
	live.onProbeTick()
	if live.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("control: phase = %s after a tick that read a loading seat, want %s", live.CurrentPhase(), PhaseColdLoad)
	}
}

// A run that already has its cause does not confirm a seat down over the real cause:
// the call that was in flight fails afterwards with a 5xx and the run's own verdict
// stands.
func TestConfirmSeatDownIgnoresARunThatAlreadyHasACause(t *testing.T) {
	eng := newDownEngine("gone")
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0) // silent: the monitor stalls the run at its allowance
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("fixture: the silent run was never stalled")
	}
	if m.Cause() == nil {
		t.Fatal("fixture: no cause")
	}
	if sd := m.ConfirmSeatDown(context.Background(), &StatusError{Code: 500, Body: "x"}); sd != nil {
		t.Fatalf("a run that already has its cause (%v) was handed a seat-down verdict: %v", m.Cause(), sd)
	}
}

// The engine's own gauges: a source that cannot see a queue reports Waiting -1
// (llama-server /slots lists no deferred requests), which is no queue at all.
func TestEngineReadingLoad(t *testing.T) {
	for _, tc := range []struct {
		r    EngineReading
		want int
	}{
		{EngineReading{}, 0},
		{EngineReading{Running: 3, Waiting: 2}, 5},
		{EngineReading{Running: 4, Waiting: -1}, 4},
		{EngineReading{Running: 1, Waiting: 0}, 1},
	} {
		if got := tc.r.Load(); got != tc.want {
			t.Errorf("%+v.Load() = %d, want %d", tc.r, got, tc.want)
		}
	}
}
