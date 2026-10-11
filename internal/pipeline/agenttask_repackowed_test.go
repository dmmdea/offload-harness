package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// A re-pack is owed the time its own answer needs at the seat's measured rate,
// whatever the loop left of the wall (register F44/C13). The wall is an
// expectation (ADR 0055 item 2): the loop runs to a stall or the ceiling, so a
// slow seat's loop routinely ends past the wall it was given, and the re-pack,
// which item 9 held to the wall plus the grace, was then told "0 s left" and
// skipped. 23 finished answers in 48 h were deferred that way ("structured
// re-pack skipped: 0 s left to the wall + 30 s grace at 21.4 tok/s buys 0 tokens
// < the answer's 1463"), every one a seat that had written the answer and was
// never asked to structure it.

// TestRunAgentTaskRepackIsOwedItsOwnTimeWhenTheLoopSpentTheWall is the starved
// shape end to end against a fake seat: a 2 s wall, and a loop whose only delta
// arrives at 2.4 s, so the loop ends 0.3 s past the wall and its 0.1 s of grace.
// The rate the re-pack is sized against is the run's own observed 0.4 tok/s (one
// delta at 2.4 s; repackRate prefers it to the configured 100), so the one-line
// answer is owed about 258 s; the time left to the wall plus grace is 0, which
// used to skip it.
func TestRunAgentTaskRepackIsOwedItsOwnTimeWhenTheLoopSpentTheWall(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := okRepackFake()
	fake.loopStream = slowFirstDeltaLoop(2400 * time.Millisecond)
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 2
	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 100).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar re-pack requests = %d, want 1: the wall was spent by the loop, the re-pack is owed its own time", got)
	}
	var st map[string]string
	if err := json.Unmarshal(wire.Structured, &st); err != nil || st["answer"] != "42" {
		t.Fatalf("structured = %s (%v), want the re-packed object", wire.Structured, err)
	}
	if wire.RepackAttempts != 1 {
		t.Fatalf("repack_attempts = %d, want 1", wire.RepackAttempts)
	}
}

// The same starved shape on a slow seat, with the loop instant: a 30 s wall and
// 30 s of grace buy 30 tokens at 0.5 tok/s, under the one-line answer's 69. The
// re-pack is owed 1.5 x 69 / 0.5 = 207 s of decode, so it is sent, clamped to what
// that plus the grace buys (about 118 tokens), and it answers.
func TestRunAgentTaskRepackOnASlowSeatIsSentWithTheTimeItIsOwed(t *testing.T) {
	fake := okRepackFake()
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 0.5).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	mt := maxTokensOf(t, <-fake.repackBodies)
	if mt < 69 || mt > 130 {
		t.Fatalf("max_tokens = %d, want what the owed 207 s plus 30 s of grace buy at 0.5 tok/s (69 <= n <= 130), not the %d budget", mt, agentRepackMaxTokens)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 1 || d[0].Skipped || d[0].MaxTokens != mt || d[0].ClampedFrom != agentRepackMaxTokens {
		t.Fatalf("detail = %+v, want one attempt sent at %d tokens, clamped from %d", d, mt, agentRepackMaxTokens)
	}
}

// A re-pack the CEILING cannot hold is still skipped, and the note names the
// ceiling, not the wall: the owed time is bounded by the one deadline the run
// really has. A 2 s wall, a 4 s ceiling (2 x the wall), a loop that ends at 2.4 s,
// and a seat at 5 tok/s whose re-pack is owed 1.5 x 69 / 5 = 21 s.
func TestRunAgentTaskRepackTheCeilingCannotHoldIsSkippedNamingTheCeiling(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, 4)()
	fake := okRepackFake()
	fake.loopStream = slowFirstDeltaLoop(2400 * time.Millisecond)
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 2
	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 5).Run(context.Background(), agentTestRequest(t, contract)))
	if got := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("re-pack requests = %d, want 0: the ceiling has no room for the answer", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassBudget || !wire.SchemaMiss {
		t.Fatalf("deferred=%v class=%q schema_miss=%v reason=%q, want a budget defer carrying the finished answer", wire.Deferred, wire.DeferClass, wire.SchemaMiss, wire.Reason)
	}
	if !strings.Contains(wire.RepackNote, "re-pack skipped:") || !strings.Contains(wire.RepackNote, "the ceiling") {
		t.Fatalf("repack_note = %q, want the skip to name the ceiling as what ran out", wire.RepackNote)
	}
}

// The owed time is fixed once per call, so it runs out like any other: a first
// attempt that spends it leaves nothing for the next one (ADR 0055 item 9's "time
// only runs out" stands for the resend and the chat lane). A 1 s wall, a seat at 50
// tok/s (the one-line answer is owed 1.5 x 69 / 50 = 2.07 s), and a first attempt
// that takes 2.4 s and answers the wrong shape: the chat lane is not sent on a fresh
// allowance.
func TestRunAgentTaskRepackOwedTimeIsNotRenewedForALaterAttempt(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2400 * time.Millisecond)
			writeCompletion(w, `{"wrong":"shape"}`, "stop", 40)
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 1
	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 50).Run(context.Background(), agentTestRequest(t, contract)))
	if fake.grammarCNT.Load() != 1 || fake.chatFallbackCNT.Load() != 0 {
		t.Fatalf("requests: grammar=%d chat=%d, want the one attempt the owed time held and nothing after it", fake.grammarCNT.Load(), fake.chatFallbackCNT.Load())
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || d[0].Skipped || !d[1].Skipped || !strings.Contains(d[1].Why, "re-pack skipped:") {
		t.Fatalf("detail = %+v, want the sent attempt, then attempt 2 skipped with its arithmetic", d)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred=%v class=%q reason=%q, want the first attempt's verdict (an abstention)", wire.Deferred, wire.DeferClass, wire.Reason)
	}
}

// The owed time is not owed past the call's deadline. The delegator's call has a
// horizon of its own (agent_call_deadline_sec, 1,500 s by default); a re-pack that
// could only finish after it is a finished answer nobody collects, and skipping it
// leaves the delegator's rescue the time it needs (the behaviour before the owed
// time, which for a big answer on a slow seat was the better one). The same starved
// shape as above, on a node whose call deadline ends 4 s after the wall began: the
// re-pack is owed 258 s at the 0.4 tok/s the loop's one late delta measured, the
// horizon leaves 1.6 s, and it is skipped naming the call's deadline.
func TestRunAgentTaskRepackIsNotOwedTimePastTheCallsDeadline(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := okRepackFake()
	fake.loopStream = slowFirstDeltaLoop(2400 * time.Millisecond)
	srv := fake.server(t)
	defer srv.Close()

	p := rateTestPipeline(t, srv.URL, 100)
	cfg := p.Cfg()
	cfg.AgentCallDeadlineSec = 4
	p.cfg = cfg
	contract := testContract()
	contract.TimeoutSec = 2
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if got := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("re-pack requests = %d, want 0: the owed time would end after the call's deadline", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassBudget || !wire.SchemaMiss {
		t.Fatalf("deferred=%v class=%q schema_miss=%v reason=%q, want a budget defer carrying the finished answer", wire.Deferred, wire.DeferClass, wire.SchemaMiss, wire.Reason)
	}
	if !strings.Contains(wire.RepackNote, "re-pack skipped:") || !strings.Contains(wire.RepackNote, "the call's deadline") {
		t.Fatalf("repack_note = %q, want the skip to name the call's deadline", wire.RepackNote)
	}
}

// repackOwed is the call-site arithmetic: 1.5 x the answer's expected tokens at
// the seat's rate, the factor the re-pack's own stall allowance already uses. No
// rate is no arithmetic.
func TestRepackOwedIsTheStallAllowancesGenerationTime(t *testing.T) {
	out := strings.Repeat("x", 300) // expectedRepackTokens = 300/3 + 64 = 164
	want := time.Duration(agent.RepackMargin * 164 / 10 * float64(time.Second))
	if got := repackOwed(out, 10); got < want-time.Millisecond || got > want+time.Millisecond {
		t.Fatalf("repackOwed = %v, want %v", got, want)
	}
	if got := repackOwed(out, 0); got != 0 {
		t.Fatalf("repackOwed with no rate = %v, want 0", got)
	}
}

// fit with an owed end: the later of the wall's end and the owed end is the bound,
// and the skip note says which one ran out.
func TestRepackFitUsesTheLaterOfTheWallAndTheOwedEnd(t *testing.T) {
	now := time.Now()
	o := repackOpts{WallEnd: now.Add(-10 * time.Second), Grace: 30 * time.Second, TokS: 10, RateBasis: "the seat-rates store"}
	// Past the wall: the wall + grace has 20 s left (buys 200 < budget 1024, >= least 100).
	if f := o.fit(now, time.Time{}, 1024, 100); f.Skip || !f.Clamped || f.Tokens != 200 {
		t.Fatalf("no owed end: fit = %+v, want clamped to the 200 tokens 20 s of grace buy", f)
	}
	o.OwedEnd = now.Add(50 * time.Second)
	if f := o.fit(now, time.Time{}, 1024, 100); f.Skip || !f.Clamped || f.Tokens != 800 || !strings.Contains(f.Note, "owed") {
		t.Fatalf("owed end: fit = %+v, want 80 s (50 owed + 30 grace) = 800 tokens, and the note to say the time was owed", f)
	}
	// An owed end that was cut short by the call's deadline says so.
	o.OwedCap = "the call's deadline"
	if f := o.fit(now, time.Time{}, 1024, 100); !f.Clamped || !strings.Contains(f.Note, "the call's deadline") {
		t.Fatalf("capped owed end: fit = %+v, want the note to name the call's deadline", f)
	}
	o.OwedCap = ""
	// The ceiling still wins.
	f := o.fit(now, now.Add(5*time.Second), 1024, 100)
	if !f.Skip || !strings.Contains(f.Note, "the ceiling") {
		t.Fatalf("under a 5 s ceiling: fit = %+v, want a skip naming the ceiling", f)
	}
}
