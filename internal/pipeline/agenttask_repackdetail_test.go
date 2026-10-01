package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// completionBody is a chat-completions answer carrying content, the engine's
// finish reason and an exact completion-token count.
func completionBody(content, finish string, tokens int) string {
	b, _ := json.Marshal(content)
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%s},"finish_reason":%q}],"usage":{"prompt_tokens":10,"completion_tokens":%d}}`, b, finish, tokens)
}

// writeCompletion answers a request with completionBody: the body of a
// repackStream / chatStream handler that needs the usage frame the older fakes
// hard-code to 7 tokens.
func writeCompletion(w http.ResponseWriter, content, finish string, tokens int) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(completionBody(content, finish, tokens)))
}

// Every re-pack attempt leaves a record on the wire, and the tokens of the
// attempts that FAILED are counted (register C-80). One real re-pack ran three
// attempts and about 10,000 generated tokens; the wire held a count of attempts
// and no trace of what any of them produced, and tokens_out counted none of them
// because only a success was added.
func TestRunAgentTaskRecordsEveryRepackAttemptAndCountsTheFailedOnes(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, fmt.Sprintf(`{"wrong":"shape","n":%d}`, n), "stop", int(300+10*(n-1)))
		},
		chatStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, `{"still":"wrong"}`, "stop", 320)
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred=%v class=%q reason=%q, want an abstention (every attempt answered the wrong shape)", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if wire.RepackAttempts != 3 {
		t.Fatalf("repack_attempts = %d, want 3", wire.RepackAttempts)
	}
	if wire.TokensOut != 930 {
		t.Errorf("tokens_out = %d, want 930: the 300 + 310 + 320 tokens of the failed attempts count", wire.TokensOut)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 3 {
		t.Fatalf("repack_attempts_detail has %d entries, want one per attempt: %+v", len(d), d)
	}
	wantLane := []string{"grammar", "grammar", "chat"}
	wantTokens := []int{300, 310, 320}
	for i, a := range d {
		if a.Attempt != i+1 || a.Lane != wantLane[i] || a.TokensOut != wantTokens[i] || a.FinishReason != "stop" || a.Skipped {
			t.Errorf("attempt %d record = %+v, want attempt %d, lane %s, %d tokens, finish stop, not skipped", i+1, a, i+1, wantLane[i], wantTokens[i])
		}
		if a.MaxTokens != agentRepackMaxTokens {
			t.Errorf("attempt %d max_tokens = %d, want %d (the floor, for a one-line answer)", i+1, a.MaxTokens, agentRepackMaxTokens)
		}
		if a.Head == "" || a.Why == "" {
			t.Errorf("attempt %d record = %+v, want the head of what it wrote and why it failed", i+1, a)
		}
	}
	if !strings.Contains(d[0].Head, `"wrong":"shape"`) || !strings.Contains(d[2].Head, `"still":"wrong"`) {
		t.Errorf("heads = %q / %q, want the content each attempt wrote", d[0].Head, d[2].Head)
	}
}

// A success after a failed attempt records both, and its tokens add to the
// failed one's: the old code counted only the attempt that produced the object.
func TestRunAgentTaskCountsAFailedAttemptBeforeASuccess(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				writeCompletion(w, `{"wrong":"shape"}`, "stop", 300)
				return
			}
			writeCompletion(w, `{"answer":"42"}`, "stop", 120)
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	if wire.TokensOut != 420 {
		t.Errorf("tokens_out = %d, want 420 (300 failed + 120 that produced the object)", wire.TokensOut)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || d[0].Why == "" || d[1].Why != "" || d[0].TokensOut != 300 || d[1].TokensOut != 120 {
		t.Fatalf("detail = %+v, want the failed attempt with its reason and the one that produced the object with none", d)
	}
}
