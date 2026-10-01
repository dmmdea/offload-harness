package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// writeDeltaStream streams n content deltas of "x" and then ends the response with
// no finish_reason and no [DONE]: the seat's connection died mid-answer.
func writeDeltaStream(w http.ResponseWriter, n int) {
	fl := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	for i := 0; i < n; i++ {
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n")) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		fl.Flush()
	}
}

// An attempt that dies mid-stream (a dropped connection, an engine error frame, a
// stall or the ceiling cutting a producing seat) cost the seat its tokens and wrote
// something: the node counts them and keeps a clip, like any other failed attempt
// (register C-80). Before this the attempt reported nothing, and it is exactly the
// attempt whose cost and shape an operator needs: 119 of 119 stalled re-packs.
func TestRunAgentTaskCountsAndClipsAnAttemptThatDiedMidStream(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				writeDeltaStream(w, 50)
				return
			}
			writeCompletion(w, `{"answer":"42"}`, "stop", 12)
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 {
		t.Fatalf("detail = %+v, want the attempt that died and the one that produced the object", d)
	}
	if d[0].TokensOut != 50 || d[0].Head != strings.Repeat("x", 50) || d[0].Why == "" || d[0].FinishReason != "" {
		t.Errorf("attempt 1 = %+v, want the 50 tokens it streamed, what it wrote and why it failed", d[0])
	}
	if d[1].TokensOut != 12 || d[1].Why != "" {
		t.Errorf("attempt 2 = %+v, want the object, 12 tokens", d[1])
	}
	if wire.TokensOut < d[0].TokensOut+d[1].TokensOut {
		t.Errorf("tokens_out = %d, want at least the 62 tokens of the two attempts: a failed attempt's generation counts", wire.TokensOut)
	}
}

// The same on the chat lane: both grammar attempts answer the wrong shape and the
// chat lane's stream dies after 30 deltas.
func TestRunAgentTaskCountsAChatAttemptThatDiedMidStream(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"wrong":"shape"}` },
		chatStream: func(_ int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeDeltaStream(w, 30)
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("deferred=%v class=%q reason=%q, want the seat's dead stream on the last attempt", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 3 || d[2].Lane != "chat" || d[2].TokensOut != 30 || d[2].Head != strings.Repeat("x", 30) {
		t.Fatalf("detail = %+v, want the chat attempt with the 30 tokens it streamed and what it wrote", d)
	}
}

// The stall watch kills a seat that streamed and then went silent: the attempt is
// recorded with the tokens it had streamed, not as an attempt that generated
// nothing (the stalled re-packs this fix exists for).
func TestRunAgentTaskCountsTheTokensOfAnAttemptTheStallWatchKilled(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 200*time.Millisecond)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(_ int64, _ map[string]any, w http.ResponseWriter, r *http.Request) {
			writeDeltaStream(w, 20)
			select { // then silence, until the watch cancels the request
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || !strings.Contains(wire.Reason, "stalled: no progress for") {
		t.Fatalf("deferred=%v reason=%q, want the stall defer", wire.Deferred, wire.Reason)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 1 || d[0].TokensOut != 20 || d[0].Head != strings.Repeat("x", 20) {
		t.Fatalf("detail = %+v, want the one attempt with the 20 tokens it streamed before the stall and what it wrote", d)
	}
	if wire.TokensOut < 20 {
		t.Errorf("tokens_out = %d, want the killed attempt's 20 tokens counted", wire.TokensOut)
	}
}
