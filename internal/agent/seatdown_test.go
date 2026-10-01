package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/seatwait"
)

// downEngine is an EngineProbe whose seat the test walks through a death:
// "ready" (counters readable, advancing when `advance` is set), "starting"
// (llama-swap is loading it), "gone" (llama-swap does not list it) and
// "refused" (llama-swap lists it ready but its engine refuses connections).
type downEngine struct {
	mu       sync.Mutex
	state    string
	steps    int64
	advance  bool
	running  int
	waiting  int
	reads    int
	fpPrefix string
	// blind marks every readable reading PrefillBlind (a source whose counters do
	// not move through a prefill); growWait adds one waiting request per read — a
	// hung engine's HTTP front end still accepting requests.
	blind    bool
	growWait bool
}

func newDownEngine(state string) *downEngine {
	return &downEngine{state: state, running: 5, fpPrefix: "v|"}
}

func (e *downEngine) set(state string, advance bool) {
	e.mu.Lock()
	e.state, e.advance = state, advance
	e.mu.Unlock()
}

// restart models an engine restart: the counters begin again.
func (e *downEngine) restart() {
	e.mu.Lock()
	e.state, e.steps, e.advance, e.fpPrefix = "ready", 0, true, e.fpPrefix+"r|"
	e.mu.Unlock()
}

func (e *downEngine) readCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads
}

func (e *downEngine) probe(context.Context) (EngineReading, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads++
	switch e.state {
	case "starting":
		return EngineReading{Loading: true, State: "starting"}, nil
	case "gone":
		return EngineReading{NotLoaded: true, Summary: "seat not loaded"}, nil
	case "refused":
		return EngineReading{Refused: true}, errors.New("metrics: connection refused")
	case "unreadable":
		return EngineReading{}, errors.New("metrics: context deadline exceeded")
	}
	if e.advance {
		e.steps++
	}
	if e.growWait {
		e.waiting++
	}
	return EngineReading{
		Fingerprint:      e.fpPrefix + strconv.FormatInt(e.steps, 10),
		TokenFingerprint: "vt|" + strconv.FormatInt(e.steps, 10),
		Summary:          "vllm-metrics: " + strconv.Itoa(e.running) + " running, " + strconv.Itoa(e.waiting) + " waiting",
		Running:          e.running,
		Waiting:          e.waiting,
		PrefillBlind:     e.blind,
	}, nil
}

// recoveryPolicy is busyPolicy plus a seat-recovery budget and a cold-load
// ceiling: the two things a recovery wait needs.
func recoveryPolicy(coldLoad time.Duration) StallPolicy {
	p := busyPolicy()
	p.SeatRecoveries = 2
	p.ColdLoad, p.ColdLoadBasis, p.ColdLoadPoll = coldLoad, "test ceiling", 10*time.Millisecond
	p.PostReady = 250 * time.Millisecond
	return p
}

// ADR 0066: counters frozen with work outstanding while llama-swap
// reads the seat ready is the engine WEDGED — the death signature of the
// 2026-09-29 flagship (a hung pipeline step, then a 120 s RPC timeout). The
// verdict is a typed seat-down outcome, not a per-run stall: five running
// requests, counters flat, is one event.
func TestMonitorFrozenCountersFileSeatDownWithin2xFlat(t *testing.T) {
	eng := newDownEngine("ready") // 5 running, counters never advance
	ctx, m := NewMonitor(context.Background(), busyPolicy(), 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0) // allowance 200 ms; flat bound max(150 ms, 200 ms)
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a wedged engine was never declared down")
	}
	var sd *SeatDownError
	if !errors.As(m.Cause(), &sd) {
		t.Fatalf("cause = %#v, want a *SeatDownError", m.Cause())
	}
	if sd.Kind != SeatDownWedged {
		t.Fatalf("kind = %q, want %q", sd.Kind, SeatDownWedged)
	}
	msg := sd.Error()
	if !strings.HasPrefix(msg, "seat down: ") || !strings.Contains(msg, "5 running") || strings.Contains(msg, "stalled: no progress") {
		t.Fatalf("reason = %q, want the seat-down prefix, the engine's 5 running and no per-run stall wording", msg)
	}
	// The first look comes at the allowance (200 ms); the verdict one flat bound
	// (200 ms) later: two flat bounds from the freeze, with room for timer jitter.
	if el := time.Since(start); el > 1200*time.Millisecond {
		t.Fatalf("declared down after %s, want within ~2 flat bounds (400 ms)", el)
	}
	// Compatibility: the wedge carries the per-run stall it replaced, so every
	// reader that only knows *StallError still sees the engine-flat evidence.
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.EngineFlat || se.Waited != PhaseDecoding {
		t.Fatalf("the seat-down verdict must wrap the engine-flat stall, got %#v", se)
	}
}

// A seat that llama-swap stops listing while a hold is established (the
// engine died under the run, nothing is loading it) is DOWN at once — no
// waiting out the flat bound on reads that can only fail — but never on the
// FIRST look, where absence alone keeps the ADR 0055 rule (PR #458 finding 1:
// a removed seat, a renamed alias, a restarted llama-swap).
func TestMonitorSeatGoneInsideAHoldIsSeatDown(t *testing.T) {
	eng := newDownEngine("ready")
	eng.set("ready", true) // working for others: the hold is established
	p := busyPolicy()
	p.EngineFlat = 5 * time.Second // the flat bound must not be what ends this
	ctx, m := NewMonitor(context.Background(), p, 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	time.Sleep(400 * time.Millisecond)
	if m.CurrentPhase() != PhaseQueued || ctx.Err() != nil {
		t.Fatalf("no hold was established: phase=%s cause=%v", m.CurrentPhase(), context.Cause(ctx))
	}
	eng.set("gone", false)
	gone := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a seat that left /running inside a hold was never declared down")
	}
	if el := time.Since(gone); el > 2*time.Second {
		t.Fatalf("declared down %s after the seat left /running: the 5 s flat bound is what fired", el)
	}
	var sd *SeatDownError
	if !errors.As(m.Cause(), &sd) || sd.Kind != SeatDownDied {
		t.Fatalf("cause = %#v, want a died seat-down", m.Cause())
	}
	if !strings.Contains(sd.Error(), "/running") {
		t.Fatalf("reason = %q, want it to say the seat left /running", sd.Error())
	}
}

// Recovery (ADR 0066): an engine restart under the cold-load hold. AwaitSeat
// holds the run while llama-swap lists the seat starting and returns when it
// serves again; the wait is counted.
func TestAwaitSeatWaitsForARestartThenReturns(t *testing.T) {
	eng := newDownEngine("starting")
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	go func() {
		time.Sleep(250 * time.Millisecond)
		eng.restart()
	}()
	begin := time.Now()
	err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "restart", fp: "v|7"})
	if err != nil {
		t.Fatalf("AwaitSeat = %v, want nil once the seat serves again", err)
	}
	if el := time.Since(begin); el < 200*time.Millisecond {
		t.Fatalf("returned after %s: it did not wait for the restart (250 ms)", el)
	}
	// The seat serves again, but the recovery is not COUNTED until the re-issued call
	// lands: a re-issue that recovered nothing is not a recovery.
	if m.SeatRecoveries() != 0 {
		t.Fatalf("SeatRecoveries = %d before the re-issued call landed, want 0", m.SeatRecoveries())
	}
	if w := m.SeatDownTotal(); w < 200*time.Millisecond {
		t.Fatalf("SeatDownTotal = %s, want the ~250 ms waited", w)
	}
	if ctx.Err() != nil {
		t.Fatalf("a recovery must not end the run: %v", context.Cause(ctx))
	}
	m.SeatAnswered() // the re-issued step got its answer
	if m.SeatRecoveries() != 1 {
		t.Fatalf("SeatRecoveries = %d after the answer, want 1", m.SeatRecoveries())
	}
	booked := m.SeatDownTotal()
	time.Sleep(60 * time.Millisecond)
	if booked < 200*time.Millisecond || m.SeatDownTotal() != booked {
		t.Fatalf("SeatDownTotal = %s then %s: the wait must stop growing once the recovery landed", booked, m.SeatDownTotal())
	}
}

// While the wait runs the monitor reports the cold-load hold — status readers
// and the delegator follow it on the bound the cold-load hold already has.
func TestAwaitSeatHoldsThePhaseAsColdLoad(t *testing.T) {
	eng := newDownEngine("starting")
	var mu sync.Mutex
	var seen []Phase
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.WithSeatProbe(nil, func(ph Phase, _ time.Duration) { mu.Lock(); seen = append(seen, ph); mu.Unlock() })
	m.Phase(PhaseDecoding, 0)
	done := make(chan error, 1)
	go func() { done <- m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "restart"}) }()
	time.Sleep(120 * time.Millisecond)
	if m.CurrentPhase() != PhaseColdLoad {
		t.Fatalf("phase during the wait = %s, want %s", m.CurrentPhase(), PhaseColdLoad)
	}
	eng.restart()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[0] != PhaseColdLoad {
		t.Fatalf("hold events = %v, want the observer told about the cold-load hold", seen)
	}
}

// A WEDGED seat that llama-swap still lists ready is not recovered by being
// ready: the run must see a change — the counters moving again, or a restart —
// before it re-issues, or the re-issue lands on the same frozen engine.
func TestAwaitSeatWedgedNeedsAChangeBeforeTheReissue(t *testing.T) {
	eng := newDownEngine("ready") // frozen: fingerprint v|0 forever
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	done := make(chan error, 1)
	go func() { done <- m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownWedged, Note: "frozen", fp: "v|0"}) }()
	select {
	case err := <-done:
		t.Fatalf("AwaitSeat returned %v while the engine was still frozen on the fingerprint that was declared wedged", err)
	case <-time.After(250 * time.Millisecond):
	}
	eng.set("ready", true) // the counters move again
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AwaitSeat = %v once the counters moved", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitSeat never returned after the engine recovered")
	}
}

// After a restart llama-swap lists the seat ready but its engine cannot be read
// from here: llama-swap's own word is enough — but only once the run has SEEN
// the seat down. A seat that was never seen down and cannot be read is not
// recovered (the engine that refuses connections while llama-swap still lists
// it is exactly that shape).
func TestAwaitSeatFallsBackToLlamaSwapsWordWhenTheEngineCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstSeen string
		wantWait  bool
	}{
		{"seen starting, then unreadable while llama-swap says ready", "starting", false},
		{"never seen down: refused reads while llama-swap says ready is still down", "refused", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := newDownEngine(tc.firstSeen)
			seat := &scriptedSeat{} // llama-swap reads the seat ready throughout
			ctx, m := NewMonitor(context.Background(), recoveryPolicy(600*time.Millisecond), 30*time.Second)
			defer m.Stop()
			m.WithEngineProbe(eng.probe)
			m.WithSeatProbe(seat.probe, nil)
			m.Phase(PhaseDecoding, 0)
			if tc.firstSeen == "starting" {
				go func() {
					time.Sleep(120 * time.Millisecond)
					eng.set("unreadable", false)
				}()
			}
			begin := time.Now()
			err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "x"})
			el := time.Since(begin)
			if tc.wantWait {
				var sd *SeatDownError
				if !errors.As(err, &sd) || !sd.GaveUp || el < 500*time.Millisecond {
					t.Fatalf("err=%v after %s, want the wait to run to its 600 ms bound", err, el)
				}
				return
			}
			if err != nil || el > 500*time.Millisecond {
				t.Fatalf("err=%v after %s, want recovery on llama-swap's ready once the seat had been seen starting", err, el)
			}
		})
	}
}

// A seat llama-swap does not list at all is served by NOBODY: waiting for it
// to come back would wait for a request that never arrives. The run's own
// re-issue is the trigger (llama-swap starts a stopped seat on demand), and the
// cold-load hold then covers the load — so the wait ends at once.
func TestAwaitSeatProceedsAtOnceWhenNobodyServesTheSeat(t *testing.T) {
	eng := newDownEngine("gone")
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	begin := time.Now()
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "gone"}); err != nil {
		t.Fatalf("AwaitSeat = %v, want the re-issue to be allowed to start the seat", err)
	}
	if el := time.Since(begin); el > time.Second {
		t.Fatalf("waited %s for a seat nobody is starting", el)
	}
}

// The wait is bounded — by the cold-load ceiling, never open-ended — and its
// end is typed: the run's cause is a seat-down the delegator can re-place.
func TestAwaitSeatGivesUpAtTheColdLoadBound(t *testing.T) {
	eng := newDownEngine("starting") // never finishes
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(400*time.Millisecond), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	begin := time.Now()
	err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "restart"})
	el := time.Since(begin)
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.GaveUp {
		t.Fatalf("AwaitSeat = %#v, want a typed give-up", err)
	}
	if el < 350*time.Millisecond || el > 3*time.Second {
		t.Fatalf("gave up after %s, want the 400 ms cold-load bound", el)
	}
	if sd.Waited < 300*time.Millisecond {
		t.Fatalf("Waited = %s, want the ~400 ms spent", sd.Waited)
	}
	if !strings.HasPrefix(sd.Error(), "seat down: ") || !strings.Contains(sd.Error(), "did not come back") {
		t.Fatalf("reason = %q", sd.Error())
	}
	var mine *SeatDownError
	if !errors.As(m.Cause(), &mine) || ctx.Err() == nil {
		t.Fatalf("the run must end with the typed cause: cause=%v ctx=%v", m.Cause(), ctx.Err())
	}
	if m.SeatRecoveries() != 0 {
		t.Fatalf("a wait that ran out is not a recovery, SeatRecoveries = %d", m.SeatRecoveries())
	}
}

// A run's recoveries are bounded: the third seat death ends the run, typed,
// without waiting.
func TestAwaitSeatIsBoundedByTheRecoveryBudget(t *testing.T) {
	eng := newDownEngine("gone")
	p := recoveryPolicy(5 * time.Second)
	p.SeatRecoveries = 1
	ctx, m := NewMonitor(context.Background(), p, 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	if err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "first"}); err != nil {
		t.Fatalf("first recovery = %v", err)
	}
	m.Phase(PhaseDecoding, 0) // the re-issued step
	m.SeatAnswered()          // ...got its answer: the recovery landed and spent the budget
	begin := time.Now()
	err := m.AwaitSeat(ctx, &SeatDownError{Kind: SeatDownDied, Note: "second"})
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.GaveUp {
		t.Fatalf("second AwaitSeat = %v, want a typed refusal", err)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("a spent recovery budget must not wait")
	}
}

// Without an engine probe or a cold-load ceiling there is nothing to wait on,
// and nothing to bound the wait with: the verdict stands, typed.
func TestAwaitSeatWithoutAProbeIsTheTerminalVerdict(t *testing.T) {
	_, m := NewMonitor(context.Background(), recoveryPolicy(time.Second), 30*time.Second)
	defer m.Stop()
	begin := time.Now()
	err := m.AwaitSeat(context.Background(), &SeatDownError{Kind: SeatDownDied, Note: "x"})
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.GaveUp {
		t.Fatalf("AwaitSeat = %v, want the typed give-up", err)
	}
	if el := time.Since(begin); el > 500*time.Millisecond {
		t.Fatalf("waited %s (the ceiling is 1 s): with no engine to read there is nothing to wait on", el)
	}
}

// P1: a model call failed like a dead seat (a stream cut, a 5xx). The monitor
// confirms against llama-swap before the failure is called a seat death.
func TestConfirmSeatDownReadsTheSeatAfterAFailedCall(t *testing.T) {
	cut := errors.New("stream read: unexpected EOF")
	for _, tc := range []struct {
		name  string
		state string
		down  bool
		note  string
	}{
		{"restarting", "starting", true, "starting"},
		{"gone from /running", "gone", true, "/running"},
		{"engine refuses reads while listed", "refused", true, "refuses"},
		{"ready and readable: the seat is up, the error stands", "ready", false, ""},
		{"unreadable for another reason: cannot tell", "unreadable", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := newDownEngine(tc.state)
			_, m := NewMonitor(context.Background(), recoveryPolicy(time.Second), 30*time.Second)
			defer m.Stop()
			m.WithEngineProbe(eng.probe)
			m.Phase(PhaseDecoding, 0)
			sd := m.ConfirmSeatDown(context.Background(), cut)
			if tc.down != (sd != nil) {
				t.Fatalf("ConfirmSeatDown = %#v, want down=%v", sd, tc.down)
			}
			if sd == nil {
				return
			}
			if sd.Kind != SeatDownDied || !strings.Contains(sd.Error(), tc.note) || !strings.HasPrefix(sd.Error(), "seat down: ") {
				t.Fatalf("verdict = %q kind %q, want a died seat-down naming %q", sd.Error(), sd.Kind, tc.note)
			}
			if !errors.Is(sd, cut) {
				t.Fatalf("the seat-down must wrap the failure that revealed it: %v", sd)
			}
		})
	}
}

// A call the run itself cancelled is never a seat verdict.
func TestConfirmSeatDownIgnoresACancelledRun(t *testing.T) {
	eng := newDownEngine("gone")
	_, m := NewMonitor(context.Background(), recoveryPolicy(time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sd := m.ConfirmSeatDown(ctx, errors.New("stream read: unexpected EOF")); sd != nil {
		t.Fatalf("a cancelled run was told its seat is down: %v", sd)
	}
}

// While the loop owns the wait, a stall timer that fires anyway (a callback
// already past its Stop) must not file a verdict against a request that no
// longer exists.
func TestParkedMonitorIgnoresATimerThatFiresAnyway(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)
	m.mu.Lock()
	m.parkLocked()
	m.mu.Unlock()
	m.onStall() // the callback that raced the Stop
	if m.Cause() != nil || ctx.Err() != nil {
		t.Fatalf("a parked monitor filed a verdict: %v", m.Cause())
	}
	// Control: the same call on an unparked monitor stalls it.
	ctx2, m2 := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m2.Stop()
	m2.Phase(PhaseDecoding, 0)
	m2.onStall()
	var se *StallError
	if !errors.As(m2.Cause(), &se) || ctx2.Err() == nil {
		t.Fatalf("control: an unparked silent monitor must stall, cause = %v", m2.Cause())
	}
}

// The loop moving on — the next step's Phase — un-parks the monitor and drops a
// verdict for the call before: a verdict that lost the race to a completed call
// must not leave the run without a stall watch.
func TestPhaseUnparksAMonitorWhoseVerdictWasMoot(t *testing.T) {
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	m.Phase(PhaseDecoding, 0)
	m.mu.Lock()
	m.down, m.downSince = &SeatDownError{Kind: SeatDownWedged}, time.Now()
	m.parkLocked()
	m.mu.Unlock()
	m.Phase(PhaseDecoding, 0) // the call completed after all; the loop asks for the next
	m.mu.Lock()
	parked, down := m.parked, m.down
	m.mu.Unlock()
	if parked || down != nil {
		t.Fatalf("parked=%v down=%v after the loop moved on", parked, down)
	}
	select { // the stall watch is live again: silence stalls the run
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the un-parked monitor never stalled a silent request")
	}
}

// seatDownClient answers each call from a script of (completion, error).
type seatDownClient struct {
	mu      sync.Mutex
	script  []func() (Completion, error)
	calls   int
	seen    [][]Msg
	max     []int
	noThink []bool
}

func (c *seatDownClient) Chat(ctx context.Context, msgs []Msg, _ []ToolSpec, maxTokens int) (Completion, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, append([]Msg(nil), msgs...))
	c.max = append(c.max, maxTokens)
	c.noThink = append(c.noThink, IsThinkingOff(ctx))
	if c.calls >= len(c.script) {
		return Completion{}, errors.New("seatDownClient: script exhausted")
	}
	f := c.script[c.calls]
	c.calls++
	return f()
}

func doneCompletion() (Completion, error) {
	return Completion{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
}

func seatDownAt(kind SeatDownKind) func() (Completion, error) {
	return func() (Completion, error) {
		return Completion{}, &SeatDownError{Kind: kind, Note: "llama-swap lists the seat starting"}
	}
}

// The step that failed because the seat died is re-issued once the seat serves
// again: the loop keeps its transcript, the step is not spent, the run ends
// done — not `error` (the pre-ADR-0066 outcome of every death).
func TestLoopReissuesTheStepAfterSeatRecovery(t *testing.T) {
	eng := newDownEngine("starting")
	go func() {
		time.Sleep(150 * time.Millisecond)
		eng.restart()
	}()
	c := &seatDownClient{script: []func() (Completion, error){seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 1).WithLiveness(m).Run(ctx, "say done")
	if err != nil || res.StopReason != "done" || res.Output != "done" {
		t.Fatalf("stop=%q err=%v output=%q, want a finished run", res.StopReason, err, res.Output)
	}
	if c.calls != 2 {
		t.Fatalf("model calls = %d, want the failed call and its re-issue", c.calls)
	}
	if res.Steps != 1 {
		t.Fatalf("steps = %d: the re-issue must not spend a step", res.Steps)
	}
	if res.SeatRecoveries != 1 || m.SeatRecoveries() != 1 {
		t.Fatalf("SeatRecoveries result=%d monitor=%d, want 1", res.SeatRecoveries, m.SeatRecoveries())
	}
	if len(c.seen[0]) != len(c.seen[1]) || c.max[0] != c.max[1] {
		t.Fatalf("the re-issue must send the same transcript and budget: msgs %d vs %d, max %d vs %d", len(c.seen[0]), len(c.seen[1]), c.max[0], c.max[1])
	}
}

// A seat that was SEEN serving again earns exactly one re-issue per failed step: if
// the re-issued call goes down again before its answer, the seat came back and died
// under the same request, and the run ends, typed and marked as re-issued — a seat
// that does that would otherwise be waited for forever. Nothing was recovered, so
// nothing is counted.
func TestLoopSeatThatCameBackAndWentDownAgainEndsTyped(t *testing.T) {
	eng := newDownEngine("starting")
	go func() {
		time.Sleep(120 * time.Millisecond)
		eng.restart()
	}()
	c := &seatDownClient{script: []func() (Completion, error){seatDownAt(SeatDownDied), seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 3).WithLiveness(m).Run(ctx, "x")
	var sd *SeatDownError
	if !errors.As(err, &sd) {
		t.Fatalf("err = %v, want a typed *SeatDownError", err)
	}
	if !sd.Reissued || !strings.Contains(sd.Error(), "re-issued once") || sd.GaveUp {
		t.Fatalf("seat-down = %q (reissued=%v gaveUp=%v), want it to say the re-issue failed too", sd.Error(), sd.Reissued, sd.GaveUp)
	}
	if res.StopReason != "error" || c.calls != 2 {
		t.Fatalf("stop=%q calls=%d, want an error after exactly one re-issue (2 calls)", res.StopReason, c.calls)
	}
	if res.SeatRecoveries != 0 || m.SeatRecoveries() != 0 {
		t.Fatalf("SeatRecoveries = %d/%d: a re-issue that went down again recovered nothing", res.SeatRecoveries, m.SeatRecoveries())
	}
}

// A seat llama-swap does not list has nobody starting it: the re-issue is the start
// trigger. When that start FAILS (the 2026-09-29 launcher refused for 18 minutes and
// every request got an instant 500) the wait goes on — one poll, then another
// attempt — instead of turning the request into two quick 500s and a defer. The run
// that finally gets its answer recovered once, and nothing before that is counted.
func TestLoopKeepsTriggeringAStartThatKeepsFailing(t *testing.T) {
	eng := newDownEngine("gone") // llama-swap never lists the seat: only a request starts it
	c := &seatDownClient{script: []func() (Completion, error){seatDownAt(SeatDownDied), seatDownAt(SeatDownDied), seatDownAt(SeatDownDied), doneCompletion}}
	p := recoveryPolicy(5 * time.Second)
	p.ColdLoadPoll = 40 * time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	begin := time.Now()
	res, err := NewLoop(c, nil, 1).WithLiveness(m).Run(ctx, "x")
	el := time.Since(begin)
	if err != nil || res.StopReason != "done" || c.calls != 4 {
		t.Fatalf("err=%v stop=%q calls=%d, want the run to finish on the fourth attempt (three failed starts)", err, res.StopReason, c.calls)
	}
	if res.Steps != 1 {
		t.Fatalf("steps = %d: a start attempt must not spend a step", res.Steps)
	}
	if res.SeatRecoveries != 1 || m.SeatRecoveries() != 1 {
		t.Fatalf("SeatRecoveries result=%d monitor=%d, want ONE recovery for the whole outage (three failed starts are not recoveries)", res.SeatRecoveries, m.SeatRecoveries())
	}
	// Two paced attempts between three failures: at least two polls of waiting, all of
	// it booked as the wait on the dead seat.
	if el < 70*time.Millisecond {
		t.Fatalf("the run took %s: failed starts must be paced by the poll, not retried in a tight loop", el)
	}
	if w := m.SeatDownTotal(); w < 70*time.Millisecond || w > el {
		t.Fatalf("SeatDownTotal = %s of a %s run, want the whole episode booked", w, el)
	}
}

// A start that never succeeds ends the run at the cold-load bound counted from the
// FIRST verdict, typed, saying what was tried and what the seat looked like — not
// after two quick attempts, and not claiming the seat came back.
func TestLoopStartThatNeverSucceedsGivesUpAtTheBound(t *testing.T) {
	eng := newDownEngine("gone")
	var calls atomic.Int64
	client := clientFunc(func(ctx context.Context, msgs []Msg, _ []ToolSpec, _ int) (Completion, error) {
		calls.Add(1)
		return Completion{}, &SeatDownError{Kind: SeatDownDied, Note: "llama-swap answered 500: the start failed"}
	})
	p := recoveryPolicy(400 * time.Millisecond)
	p.ColdLoadPoll = 40 * time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	begin := time.Now()
	res, err := NewLoop(client, nil, 1).WithLiveness(m).Run(ctx, "x")
	el := time.Since(begin)
	var sd *SeatDownError
	if !errors.As(err, &sd) || !sd.GaveUp || sd.Reissued {
		t.Fatalf("err = %v, want a typed give-up at the bound, not a seat that came back", err)
	}
	if el < 350*time.Millisecond || el > 3*time.Second {
		t.Fatalf("gave up after %s, want the 400 ms cold-load bound", el)
	}
	if sd.Attempts < 3 || calls.Load() > 25 {
		t.Fatalf("attempts=%d calls=%d, want several paced start attempts and no tight loop", sd.Attempts, calls.Load())
	}
	if sd.Waited < 350*time.Millisecond {
		t.Fatalf("Waited = %s, want the ~400 ms of the whole episode", sd.Waited)
	}
	msg := sd.Error()
	if !strings.HasPrefix(msg, "seat down: ") || !strings.Contains(msg, "start attempt") || !strings.Contains(msg, "did not come back") ||
		!strings.Contains(msg, "llama-swap does not list the seat") || strings.Contains(msg, "came back and it went down again") {
		t.Fatalf("reason = %q, want the attempts and the seat's last state, never a claim that the seat came back", msg)
	}
	if res.SeatRecoveries != 0 || m.SeatRecoveries() != 0 {
		t.Fatalf("SeatRecoveries = %d/%d: a seat that never started recovered nothing", res.SeatRecoveries, m.SeatRecoveries())
	}
}

// A run whose monitor has no recovery budget keeps today's outcome for a dead
// seat — the run ends — but typed, so the delegator can re-place it.
func TestLoopWithoutARecoveryBudgetReturnsTheTypedErrorAtOnce(t *testing.T) {
	eng := newDownEngine("gone")
	p := recoveryPolicy(5 * time.Second)
	p.SeatRecoveries = 0
	c := &seatDownClient{script: []func() (Completion, error){seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), p, 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 3).WithLiveness(m).Run(ctx, "x")
	var sd *SeatDownError
	if !errors.As(err, &sd) || res.StopReason != "error" || c.calls != 1 || res.SeatRecoveries != 0 {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d, want one call and a typed error", err, res.StopReason, c.calls, res.SeatRecoveries)
	}
}

// A step that is itself the empty-final re-issue (thinking off, at the final
// budget) is re-issued in the SAME mode after a seat recovery: the recovery must
// not quietly turn it back into an ordinary step.
func TestLoopSeatDownReissueKeepsTheEmptyFinalReissueMode(t *testing.T) {
	eng := newDownEngine("gone")
	empty := func() (Completion, error) {
		return Completion{Msg: Msg{Role: "assistant"}, FinishReason: "stop"}, nil
	}
	c := &seatDownClient{script: []func() (Completion, error){empty, seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 3).WithLiveness(m).Run(ctx, "x")
	if err != nil || res.StopReason != "done" || c.calls != 3 || res.SeatRecoveries != 1 {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d, want empty -> seat down -> done", err, res.StopReason, c.calls, res.SeatRecoveries)
	}
	if c.noThink[0] || !c.noThink[1] || !c.noThink[2] {
		t.Fatalf("thinking off per call = %v, want [false true true]: the recovered call must stay the thinking-off re-issue", c.noThink)
	}
	if c.max[1] != c.max[2] || c.max[1] <= c.max[0] {
		t.Fatalf("budget per call = %v, want the re-issue's final budget kept across the recovery", c.max)
	}
}

// The list-cap re-issue of a cut answer opens at the CUT turn's budget (the floor):
// a seat recovery of that very call must open it there again, not at the smaller
// fitted final budget the floor exists to override.
func TestLoopSeatDownReissueKeepsTheListCapBudget(t *testing.T) {
	eng := newDownEngine("gone")
	cut := func() (Completion, error) {
		return Completion{Msg: Msg{Role: "assistant", Content: `{"items":["a","b"`}, FinishReason: "length"}, nil
	}
	c := &seatDownClient{script: []func() (Completion, error){cut, seatDownAt(SeatDownDied), doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	l := NewLoop(c, mkTools("list_dir"), 2).WithMaxTokens(2048).
		WithCutFinalReissue("cap every list at 8 items", 0).
		WithFinalBudgetFit(func(configured int, _ time.Duration) (int, string) { return 1024, "fit" }).
		WithLiveness(m)
	res, err := l.Run(ctx, "x")
	if err != nil || res.StopReason != "done" || c.calls != 3 || res.SeatRecoveries != 1 || res.FinalReissue != FinalReissueListCap {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d reissue=%q, want cut -> list-cap re-issue -> seat down -> done",
			err, res.StopReason, c.calls, res.SeatRecoveries, res.FinalReissue)
	}
	if c.max[0] != 2048 || c.max[1] != 2048 || c.max[2] != 2048 {
		t.Fatalf("budget per call = %v, want the cut turn's 2048 kept on the re-issue AND on its seat recovery (the fitted final budget is 1024)", c.max)
	}
}

// A thrash — the engine keeps stepping (preempting and recomputing) but produces
// no token for anyone — is an engine that is ALIVE and overloaded, not a seat
// down: it stays the plain stall ADR 0061 files, never a `seat down:` verdict,
// even with recoveries armed (a re-issue would only feed it another request).
func TestBusyHoldThrashStaysAPlainStall(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	eng.noProduce.Store(true)
	p := recoveryPolicy(5 * time.Second)
	p.EngineTokenFlat = 300 * time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	m.Phase(PhaseDecoding, 0)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a thrash that produced nothing was never stalled")
	}
	var sd *SeatDownError
	if errors.As(m.Cause(), &sd) {
		t.Fatalf("a thrashing engine was filed as a seat down: %v", sd)
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || !se.EngineThrash || !strings.HasPrefix(se.Error(), "stalled: ") {
		t.Fatalf("cause = %#v, want the thrash stall", m.Cause())
	}
}

// ...and through the loop with a recovery budget, a thrash ends the run: the
// call is not re-issued onto the engine that is thrashing.
func TestLoopThrashIsNotReissued(t *testing.T) {
	eng := &scriptedEngine{}
	eng.advance.Store(true)
	eng.noProduce.Store(true)
	var calls atomic.Int64
	client := clientFunc(func(ctx context.Context, msgs []Msg, _ []ToolSpec, _ int) (Completion, error) {
		calls.Add(1)
		<-ctx.Done() // a request the thrashing engine never answers
		return Completion{}, ctx.Err()
	})
	start := time.Now()
	p := recoveryPolicy(5 * time.Second)
	p.EngineTokenFlat = 300 * time.Millisecond
	ctx, m := NewMonitor(context.Background(), p, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(client, nil, 2).WithLiveness(m).Run(ctx, "x")
	var sd *SeatDownError
	if err == nil || errors.As(err, &sd) || errors.As(m.Cause(), &sd) {
		t.Fatalf("err=%v cause=%v, want the run to end on the thrash stall, not a seat down", err, m.Cause())
	}
	// (The loop's own context-overflow retry may send the cancelled request once
	// more — "context canceled" reads like an overflow to it, as it always has —
	// so the call count is not the pin; the recovery is.)
	if res.SeatRecoveries != 0 || m.SeatDownTotal() != 0 || m.SeatRecoveries() != 0 {
		t.Fatalf("recoveries=%d monitor=%d waited=%s (calls %d): a thrash must not be waited for or re-issued",
			res.SeatRecoveries, m.SeatRecoveries(), m.SeatDownTotal(), calls.Load())
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the run took %s: it waited for a seat that was not down", el)
	}
}

// P1 through the loop: a stream cut is confirmed against llama-swap (the seat
// is restarting), the loop waits for it and re-issues.
func TestLoopConfirmsATransportFailureAgainstTheSeat(t *testing.T) {
	eng := newDownEngine("starting")
	go func() {
		time.Sleep(150 * time.Millisecond)
		eng.restart()
	}()
	cut := func() (Completion, error) { return Completion{}, errors.New("stream read: unexpected EOF") }
	c := &seatDownClient{script: []func() (Completion, error){cut, doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 1).WithLiveness(m).Run(ctx, "x")
	if err != nil || res.StopReason != "done" || c.calls != 2 || res.SeatRecoveries != 1 {
		t.Fatalf("err=%v stop=%q calls=%d recoveries=%d, want the cut call re-issued after the restart", err, res.StopReason, c.calls, res.SeatRecoveries)
	}
}

// The same failure on a seat that reads ready and readable is not a seat
// death: the error is returned exactly as before, and nothing waits.
func TestLoopKeepsTheErrorWhenTheSeatIsUp(t *testing.T) {
	eng := newDownEngine("ready")
	cut := errors.New("stream read: unexpected EOF")
	c := &seatDownClient{script: []func() (Completion, error){func() (Completion, error) { return Completion{}, cut }, doneCompletion}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(c, nil, 3).WithLiveness(m).Run(ctx, "x")
	var sd *SeatDownError
	if errors.As(err, &sd) || !errors.Is(err, cut) || res.StopReason != "error" || c.calls != 1 {
		t.Fatalf("err=%v stop=%q calls=%d, want the original error and no re-issue", err, res.StopReason, c.calls)
	}
}

// An ordinary refusal (a 4xx) is not a candidate at all: the seat is not even
// read. (A 400 would read as a context overflow to the loop's own retry, so
// the refusal here is a 404.)
func TestLoopDoesNotProbeTheSeatForAnOrdinaryError(t *testing.T) {
	eng := newDownEngine("gone")
	bad := &StatusError{Code: 404, Body: "model not found"}
	c := &seatDownClient{script: []func() (Completion, error){func() (Completion, error) { return Completion{}, bad }}}
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	_, err := NewLoop(c, nil, 3).WithLiveness(m).Run(ctx, "x")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("err = %v, want the 404 untouched", err)
	}
	if n := eng.readCount(); n != 0 {
		t.Fatalf("the seat was read %d times for a 404", n)
	}
}

// The monitor's verdict cancels ONLY the model call in flight: the run's
// context stays alive, the loop waits and re-issues (the silence-driven half of
// the same recovery: a wedged engine that stops answering).
func TestLoopSurvivesAWedgeThatTheMonitorFilesWhileACallIsInFlight(t *testing.T) {
	eng := newDownEngine("ready") // frozen
	var calls int
	var mu sync.Mutex
	client := clientFunc(func(ctx context.Context, msgs []Msg, _ []ToolSpec, _ int) (Completion, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-ctx.Done() // a request that never answers, until the monitor cancels the step
			return Completion{}, ctx.Err()
		}
		return doneCompletion()
	})
	go func() {
		time.Sleep(700 * time.Millisecond) // after the wedge verdict (~400 ms)
		eng.restart()
	}()
	ctx, m := NewMonitor(context.Background(), recoveryPolicy(5*time.Second), 30*time.Second)
	defer m.Stop()
	m.WithEngineProbe(eng.probe)
	res, err := NewLoop(client, nil, 1).WithLiveness(m).Run(ctx, "x")
	if err != nil || res.StopReason != "done" {
		t.Fatalf("err=%v stop=%q cause=%v, want the run to survive the wedge", err, res.StopReason, m.Cause())
	}
	if res.SeatRecoveries != 1 || calls != 2 {
		t.Fatalf("recoveries=%d calls=%d, want 1 recovery and the re-issued call", res.SeatRecoveries, calls)
	}
	if ctx.Err() != nil {
		t.Fatalf("the run's context was cancelled by a recoverable verdict: %v", context.Cause(ctx))
	}
}

// clientFunc adapts a function to the Client interface.
type clientFunc func(ctx context.Context, msgs []Msg, tools []ToolSpec, maxTokens int) (Completion, error)

func (f clientFunc) Chat(ctx context.Context, msgs []Msg, tools []ToolSpec, maxTokens int) (Completion, error) {
	return f(ctx, msgs, tools, maxTokens)
}

// The chat client asks the run's seat check before it sleeps on a llama-swap
// 5xx: while the seat is not serving, the failure is the seat's (returned at
// once, typed), not contention to be waited out on the 90 s budget — the
// 2026-09-29 outage spent 90 s per call on a seat that could not start.
func TestChatSkipsTheContentionBudgetWhileTheSeatIsNotServing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream command exited prematurely","src":"llama-swap"}}`)
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second)
	budget := seatwait.NewBudget(30)

	// The seat is starting: the failure is returned at once, typed.
	ctx := seatwait.WithBudget(context.Background(), budget)
	ctx = ContextWithSeatCheck(ctx, func(context.Context) (bool, string) { return true, "llama-swap lists the seat starting" })
	begin := time.Now()
	_, err := c.Chat(ctx, []Msg{{Role: "user", Content: "hi"}}, nil, 8)
	var sd *SeatDownError
	if !errors.As(err, &sd) || sd.Kind != SeatDownDied {
		t.Fatalf("err = %v, want a died seat-down", err)
	}
	if el := time.Since(begin); el > 800*time.Millisecond {
		t.Fatalf("Chat waited %s on a seat that is not serving", el)
	}
	if budget.Spent() != 0 {
		t.Fatalf("the contention budget was spent (%s) on a seat that is not serving", budget.Spent())
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 {
		t.Fatalf("the seat-down must wrap the 500 that revealed it: %v", err)
	}
}

// Control: a seat that IS serving keeps the contention path exactly as it was —
// llama-swap's 500 src=llama-swap is retried on the counted budget.
func TestChatKeepsTheContentionPathWhileTheSeatIsServing(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"src":"llama-swap"}}`)
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second)
	budget := seatwait.NewBudget(2) // 1 s + then the 2 s ladder step exceeds the budget
	ctx := seatwait.WithBudget(context.Background(), budget)
	ctx = ContextWithSeatCheck(ctx, func(context.Context) (bool, string) { return false, "" })
	_, err := c.Chat(ctx, []Msg{{Role: "user", Content: "hi"}}, nil, 8)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 {
		t.Fatalf("err = %v, want the 500 to stand after the budget", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits < 2 || budget.Spent() == 0 {
		t.Fatalf("hits=%d spent=%s: a serving seat's 5xx must still be retried on the contention budget", hits, budget.Spent())
	}
}

// 429 is contention by definition (llama-swap's concurrency limit): the seat
// check is never asked about it.
func TestChatNeverAsksTheSeatCheckAbout429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second)
	asked := 0
	ctx := seatwait.WithBudget(context.Background(), seatwait.NewBudget(1))
	ctx = ContextWithSeatCheck(ctx, func(context.Context) (bool, string) { asked++; return true, "down" })
	_, err := c.Chat(ctx, []Msg{{Role: "user", Content: "hi"}}, nil, 8)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 429 || asked != 0 {
		t.Fatalf("err=%v asked=%d, want the 429 untouched and the check never asked", err, asked)
	}
}
