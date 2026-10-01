package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// rateTestPipeline is agentTestPipeline for a box that knows its agent seat's
// decode rate (agent_seat_tok_s): the node then sizes what a re-pack may ask for
// against the wall.
func rateTestPipeline(t *testing.T, base string, tokS float64) *Pipeline {
	t.Helper()
	p := agentTestPipeline(t, base)
	cfg := p.Cfg()
	cfg.AgentSeatTokS = tokS
	p.cfg = cfg
	return p
}

func okRepackFake() *agentFake {
	return &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repack:       func(int64) string { return `{"answer":"42"}` },
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
	}
}

// The contract's wall is an expectation, not a deadline, and nothing used to
// compare what a re-pack attempt asks for with the time that is left (register
// C-80): a seat at 5.6 tok/s asked for 8,192 tokens with a minute to spare and ran
// 24 minutes past a 600 s wall. The wall's own end, plus the liveness policy's
// 30 s of slack, now decides what an attempt may ask for at the seat's rate, and
// an attempt that cannot buy even the answer is not sent.
//
// A 30 s wall and 30 s of slack leave about 60 s. At 0.5 tok/s that buys 30
// tokens, and the one-line answer alone needs 69 (len/3 + 64).
func TestRunAgentTaskRepackIsSkippedWhenTheWallCannotBuyTheAnswer(t *testing.T) {
	fake := okRepackFake()
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 0.5).Run(context.Background(), agentTestRequest(t, testContract())))
	if got := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("re-pack requests = %d, want 0: the time left at 0.5 tok/s cannot hold the answer", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("deferred=%v class=%q reason=%q, want a budget defer: the wall had no room, the model was never asked", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !wire.SchemaMiss || wire.Output != "The answer is 42." {
		t.Fatalf("schema_miss=%v output=%q, want the finished answer kept and flagged so the delegator can re-pack it", wire.SchemaMiss, wire.Output)
	}
	if wire.RepackAttempts != 0 {
		t.Fatalf("repack_attempts = %d, want 0: attempts count the requests actually sent", wire.RepackAttempts)
	}
	for _, want := range []string{"re-pack skipped:", " left ", "0.5 tok/s", "buys ", " tokens < the answer's 69"} {
		if !strings.Contains(wire.RepackNote, want) {
			t.Errorf("repack_note = %q, want it to say %q", wire.RepackNote, want)
		}
	}
	// The stable prefix an older delegator recognizes a re-pack failure by.
	if !strings.HasPrefix(wire.Reason, core.RepackFailedReason) {
		t.Errorf("reason = %q, want the %q prefix", wire.Reason, core.RepackFailedReason)
	}
}

// With time for the answer but not for the whole budget, the request carries
// what the time buys: ~60 s at 5 tok/s is about 300 tokens, under the 1,024
// budget and over the answer's 69.
func TestRunAgentTaskRepackMaxTokensAreClampedToTheTimeLeft(t *testing.T) {
	fake := okRepackFake()
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 5).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	mt := maxTokensOf(t, <-fake.repackBodies)
	if mt < 69 || mt > 300 {
		t.Fatalf("max_tokens = %d, want what ~60 s buys at 5 tok/s (69 < n <= 300), not the %d budget", mt, agentRepackMaxTokens)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 1 || d[0].MaxTokens != mt || d[0].ClampedFrom != agentRepackMaxTokens {
		t.Fatalf("detail = %+v, want the attempt recorded at %d tokens, clamped from %d", d, mt, agentRepackMaxTokens)
	}
}

// The guards: a seat fast enough for the whole budget is not narrowed, and a box
// that knows no rate does no time arithmetic at all (it fails open, like every
// other sizing decision that needs a rate).
func TestRunAgentTaskRepackIsNotNarrowedWhenTheTimeIsThereOrTheRateUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		tokS float64
	}{{"a fast seat", 100}, {"no rate known", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := okRepackFake()
			srv := fake.server(t)
			defer srv.Close()
			wire := decodeWire(t, rateTestPipeline(t, srv.URL, tc.tokS).Run(context.Background(), agentTestRequest(t, testContract())))
			if wire.Deferred {
				t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
			}
			if mt := maxTokensOf(t, <-fake.repackBodies); mt != agentRepackMaxTokens {
				t.Fatalf("max_tokens = %d, want the unnarrowed %d", mt, agentRepackMaxTokens)
			}
			if d := wire.RepackAttemptsDetail; len(d) != 1 || d[0].ClampedFrom != 0 {
				t.Fatalf("detail = %+v, want one attempt that was not clamped", d)
			}
		})
	}
}

// An attempt that spends the time leaves nothing for the next: the first request
// fits (clamped to what ~2 s buys at 100 tok/s) and takes 1.6 s, and the next
// would buy fewer tokens than the answer needs, so it is skipped, with the
// reason, and the first attempt's own verdict (it answered the wrong shape) is
// the one the defer carries.
func TestRunAgentTaskRepackSkipsTheNextAttemptOnceTheFirstSpentTheWall(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			time.Sleep(1600 * time.Millisecond)
			writeCompletion(w, `{"wrong":"shape"}`, "stop", 40)
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 2 // wall 2 s + 100 ms of slack
	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 100).Run(context.Background(), agentTestRequest(t, contract)))
	if fake.grammarCNT.Load() != 1 || fake.chatFallbackCNT.Load() != 0 {
		t.Fatalf("requests: grammar=%d chat=%d, want the one that fit and nothing after it", fake.grammarCNT.Load(), fake.chatFallbackCNT.Load())
	}
	if wire.RepackAttempts != 1 {
		t.Fatalf("repack_attempts = %d, want 1: the skipped attempt was never sent", wire.RepackAttempts)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention || !strings.Contains(wire.Reason, "schema validation failed") {
		t.Fatalf("deferred=%v class=%q reason=%q, want the first attempt's verdict (an abstention)", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !strings.Contains(wire.RepackNote, "re-pack skipped:") {
		t.Fatalf("repack_note = %q, want the skipped attempt's arithmetic", wire.RepackNote)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || d[0].Skipped || !d[1].Skipped || d[1].Attempt != 2 || !strings.Contains(d[1].Why, "re-pack skipped:") {
		t.Fatalf("detail = %+v, want the sent attempt, then attempt 2 skipped with its reason", d)
	}
}
