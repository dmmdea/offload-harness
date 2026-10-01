package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The time fit and the escalation gate compose (register C-80). The fit sizes each
// attempt against the time left and never lets time buy more later than it bought
// earlier, so a cut attempt can be resent at a larger budget only when the time
// still left buys that larger budget: a request that carries FEWER tokens than the
// one the gate just judged too small cannot finish and only spends the time the
// chat lane needed.
//
// The timing below keeps every figure well inside its band, so no outcome rests on
// one clock tick. Where the outcome has a window on both sides (the first test), the
// window is about 0.9 s wide and the figure sits in its middle; where it has one side
// (the chat lane skipped for want of time), more elapsed time only makes the skip
// surer.

// escalationFake is a seat whose grammar completions are always cut at max_tokens
// with dense, non-repeating text (the budget WAS the problem by the density test),
// the first one after holding the seat for firstHold. It reports as many tokens as
// the request allowed, the way an engine that ran to the cap does.
func escalationFake(answer string, firstHold time.Duration) *agentFake {
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				time.Sleep(firstHold)
			}
			writeCompletion(w, `{"key_facts":["`+digitNoise(1400), "length", int(body["max_tokens"].(float64)))
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
}

func composeContract() core.AgentContract {
	c := testContract()
	c.TimeoutSec = 2 // wall 2 s + 100 ms of slack
	return c
}

// A 3 s wall and 100 ms of grace leave about 3.05 s at the first request, the seat
// does 500 tok/s, and the answer's own size (the skip floor) is 564 tokens, about
// 1.1 s of it. The first request fits whole (1,024 tokens, the floor, 2 s of it) and
// is cut on dense text that needs about 1,160, then holds the seat for 1.5 s. What is
// left (about 1.5 s) buys about 760 tokens: fewer than the first request carried and
// fewer than the answer needs, and more than the chat lane's floor, so the chat lane
// goes out clamped. The old gate sent the escalation anyway, clamped to those 760, and
// it was cut again.
func TestRunAgentTaskRepackDoesNotEscalateToFewerTokensThanTheCutRequestCarried(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerOfChars(1500)
	fake := escalationFake(answer, 1500*time.Millisecond)
	srv := fake.server(t)
	defer srv.Close()

	contract := composeContract()
	contract.TimeoutSec = 3
	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 500).Run(context.Background(), agentTestRequest(t, contract)))
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar requests = %d, want 1: the time left buys fewer tokens than the cut request carried, so a resend cannot finish", got)
	}
	first := maxTokensOf(t, <-fake.repackBodies)
	if first != agentRepackMaxTokens {
		t.Fatalf("first request max_tokens = %d, want the unclamped floor %d", first, agentRepackMaxTokens)
	}
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s: the chat lane had the time left and must have been tried", wire.DeferClass, wire.Reason)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || d[0].Lane != "grammar" || d[1].Lane != "chat" || d[1].Skipped {
		t.Fatalf("detail = %+v, want the cut grammar attempt and then the chat lane that went out", d)
	}
	// The record says why the larger request never went, with the arithmetic.
	if !strings.Contains(d[0].Why, "too small") || !strings.Contains(d[0].Why, "re-pack skipped:") {
		t.Errorf("attempt 1 note = %q, want the budget judged too small and the skipped resend's arithmetic", d[0].Why)
	}
	// The chat lane was sized to what the time bought, under its own budget and over
	// the answer's size.
	if mt := d[1].MaxTokens; mt >= agentRepackMaxTokens || mt < expectedRepackTokens(answer) || d[1].ClampedFrom != agentRepackMaxTokens {
		t.Errorf("chat attempt = %+v, want it clamped from %d to what ~1.5 s buys (%d <= n < %d)", d[1], agentRepackMaxTokens, expectedRepackTokens(answer), agentRepackMaxTokens)
	}
}

// An attempt the time narrowed is never resent: a larger budget would be clamped
// again, and the time spent on the first request is gone. Eight hundred tokens of
// a 1,024 budget (400 tok/s over ~2 s), cut on dense text that needs about 950, is
// the shape the gate calls "too small" and the clock could not have fixed.
func TestRunAgentTaskRepackClampedAttemptIsNotResentAndSaysTheClockSetItsBudget(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerOfChars(1500)
	fake := escalationFake(answer, 0)
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 400).Run(context.Background(), agentTestRequest(t, composeContract())))
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar requests = %d, want 1: a clamped attempt is not resent", got)
	}
	d := wire.RepackAttemptsDetail
	if len(d) < 2 || d[0].ClampedFrom != agentRepackMaxTokens || d[0].MaxTokens >= agentRepackMaxTokens || d[0].FinishReason != "length" {
		t.Fatalf("detail = %+v, want the first attempt clamped from %d and cut", d, agentRepackMaxTokens)
	}
	// What the cut showed is kept beside the clamp: the clock set the budget AND the
	// answer needed more than it.
	for _, want := range []string{"the time left set this budget", "clamped to", "too small"} {
		if !strings.Contains(d[0].Why, want) {
			t.Errorf("attempt 1 note = %q, want it to say %q", d[0].Why, want)
		}
	}
	if d[1].Lane != "chat" {
		t.Errorf("attempt 2 = %+v, want the chat lane, the request that is not the first one again", d[1])
	}
}

// The chat lane is sized against the time left like the grammar lane. A
// whitespace runaway cut the first request after 1.7 s of the wall; what is left
// (about 0.35 s) cannot buy the answer's own 564 tokens, so the chat request is
// not sent and the verdict is the first attempt's, with the arithmetic beside it.
func TestRunAgentTaskChatLaneIsSkippedWhenTheTimeLeftCannotBuyTheAnswer(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerOfChars(1500)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			time.Sleep(1700 * time.Millisecond)
			writeCompletion(w, `{"key_facts":["a"],"numbers":[`+strings.Repeat(" \n  ", 200), "length", int(body["max_tokens"].(float64)))
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 1000).Run(context.Background(), agentTestRequest(t, composeContract())))
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar requests = %d, want 1 (a runaway is not resent)", got)
	}
	if got := fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("chat requests = %d, want 0: ~0.35 s at 1,000 tok/s buys fewer tokens than the answer's own size", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred=%v class=%q reason=%q, want the first attempt's verdict: the seat ran away, and that is an abstention", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if wire.RepackAttempts != 1 {
		t.Fatalf("repack_attempts = %d, want 1: the skipped chat request was never sent", wire.RepackAttempts)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || d[0].Lane != "grammar" || !strings.Contains(d[0].Why, "degenerate") ||
		!d[1].Skipped || d[1].Lane != "chat" || !strings.Contains(d[1].Why, "re-pack skipped:") {
		t.Fatalf("detail = %+v, want the cut grammar attempt (degenerate), then the chat lane skipped with its arithmetic", d)
	}
	if !strings.Contains(wire.RepackNote, "re-pack skipped:") {
		t.Errorf("repack_note = %q, want the skipped chat request's arithmetic", wire.RepackNote)
	}
}
