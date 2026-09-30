package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// the monitor cancels during the SECOND grammar attempt (the first failed
// at once on a shape error). No chat request is sent on the dead context and the
// count says two requests. The only test of the stop-once-cancelled rule
// cancels during the FIRST attempt, so deleting the guard that sits in front of
// the chat fallback kept the suite green: attempts read 3 for two real requests.
func TestRepackStopsRetryingWhenTheMonitorCancelsDuringTheSecondAttempt(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if n == 1 { // answers at once, in the wrong shape
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"wrong\":\"shape\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
				return
			}
			select { // the second one is silent
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)

	pol := agent.StallPolicy{Floor: 300 * time.Millisecond, Repack: 300 * time.Millisecond, Slack: 10 * time.Millisecond}
	cctx, live := agent.NewMonitor(context.Background(), pol, 30*time.Second)
	defer live.Stop()
	live.Phase(agent.PhaseRepack, 0)
	ctx := agent.ContextWithProgress(cctx, live.Progress)

	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
	_, _, _, attempts, err := p.repackStructured(ctx, agentTestSeat, schema, "The answer is 42.", 0)
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2: two requests were sent, the third was never started", attempts)
	}
	if got := fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("chat fallback requests = %d, want 0", got)
	}
	var se *agent.StallError
	if !errors.As(err, &se) || se.Phase != agent.PhaseRepack {
		t.Fatalf("err = %v, want the monitor's re-pack stall in the chain", err)
	}
}

// a seat that refuses a streamed structured request (400) is remembered as
// JSON-only, and the next re-pack to it is one silent JSON answer. The context
// still owns its deadline: no transport bound of its own may cut it. (The
// existing held-request test passes WithProgress alone, which by itself drops
// the client timeout, so it cannot tell WithoutClientTimeout is needed here.)
func TestRepackOnASeatThatRefusedTheStreamIsNotCutByATransportBound(t *testing.T) {
	defer compressLiveness(t, 3*time.Second, time.Second, 6)() // ceiling 6 s: the old share was 6 s / 3 = 2 s
	defer compressRepackBound(t, 3*time.Second)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			if body["stream"] == true {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"stream is not supported together with structured outputs"}`))
				return
			}
			time.Sleep(2500 * time.Millisecond) // one silent JSON answer, past the old 2 s share
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"42\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	for run := 1; run <= 2; run++ { // run 1 learns the refusal; run 2 is the JSON-only seat
		wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, testContract())))
		if wire.Deferred || !strings.Contains(string(wire.Structured), `"answer":"42"`) {
			t.Fatalf("run %d: deferred=%v %s / %q (attempts %d), want the silent JSON answer to land", run, wire.Deferred, wire.DeferClass, wire.Reason, wire.RepackAttempts)
		}
	}
}

// the delegator's rescue sizes its allowance from THIS seat's decode rate
// (the configured rate when no measurement is stored), like the node's re-pack:
// a 264-token re-write at 50 tok/s is ~8 s, not the flat bound. Compressed to
// 100 ms here, a rescue that ignored the rate would be cut before its 600 ms
// answer.
func TestRescueRepackAllowanceFollowsTheSeatRate(t *testing.T) {
	defer compressLiveness(t, 100*time.Millisecond, 50*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 100*time.Millisecond)()
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		repack:      func(int64) string { return `{"answer":"42"}` },
		repackDelay: 600 * time.Millisecond,
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	cfg := p.Cfg()
	cfg.AgentSeatTokS = 50
	p.cfg = cfg
	got, err := p.RescueRepack(context.Background(), testContract(), strings.Repeat("The answer is 42. ", 33), 0)
	if err != nil || !strings.Contains(string(got.Structured), `"answer":"42"`) {
		t.Fatalf("rescued = %+v err = %v, want the 600 ms completion to land inside the arithmetic allowance", got, err)
	}
}

// the chat-lane fallback of the re-pack streams under liveness like the
// grammar lane does. Both grammar attempts answer the wrong shape; the seat then
// streams its chat answer for ten allowances.
func TestRepackChatFallbackStreamsUnderLiveness(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 200*time.Millisecond)()
	fake := &agentFake{
		rosterIDs:  []string{agentTestSeat},
		loop:       func(int64) string { return doneChat("The answer is 42.") },
		repack:     func(int64) string { return `{"wrong":"shape"}` },
		chatStream: streamedRepackSeat(40*time.Millisecond, 2*time.Second),
	}
	srv := fake.server(t)
	defer srv.Close()
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred || !strings.Contains(string(wire.Structured), `"answer":"42"`) {
		t.Fatalf("deferred=%v %s / %q (attempts %d), want the streamed chat fallback delivered", wire.Deferred, wire.DeferClass, wire.Reason, wire.RepackAttempts)
	}
}

// a delegator with no agent seat says so; it does not attempt a completion
// against nothing.
func TestRescueRepackOnABoxWithNoAgentSeatFailsHonestly(t *testing.T) {
	fake := &agentFake{rosterIDs: []string{agentTestSeat}, repack: func(int64) string { return `{"answer":"42"}` }}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	cfg := p.Cfg()
	cfg.Endpoint = ""
	p.cfg = cfg
	_, err := p.RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err == nil || !strings.Contains(err.Error(), "no agent seat") {
		t.Fatalf("err = %v, want the box's lack of an agent seat named", err)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Fatalf("grammar requests = %d, want none", fake.grammarCNT.Load())
	}
}

// the delegator's rescue is ONE completion, so its request may use the
// whole of its allowance. The per-attempt transport share divides what is left by
// the attempts still owed a turn, and a cap of one leaves one owed; dividing by
// the node's three would cut a rescue at a third of the time it was given (in
// production: about 136 s of a 410 s allowance).
func TestRescueRepackIsNotCutAtAThirdOfItsAllowance(t *testing.T) {
	defer compressLiveness(t, 900*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 900*time.Millisecond)()
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		repack:      func(int64) string { return `{"answer":"42"}` },
		repackDelay: 500 * time.Millisecond, // past a third of the 900 ms allowance, inside all of it
	}
	srv := fake.server(t)
	defer srv.Close()
	got, err := agentTestPipeline(t, srv.URL).RescueRepack(context.Background(), testContract(), "The answer is 42.", 0)
	if err != nil || !strings.Contains(string(got.Structured), `"answer":"42"`) {
		t.Fatalf("rescued = %+v err = %v, want the 500 ms completion to land inside its 900 ms allowance", got, err)
	}
}
