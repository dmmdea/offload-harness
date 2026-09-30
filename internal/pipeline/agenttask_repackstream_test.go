package pipeline

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// streamedRepackSeat models ONE seat under both client modes: asked for a
// stream it sends a delta every `gap` for `total`, then the object; asked for a
// single JSON answer it is silent for `total` and then sends the same object.
// The same generation, so the only difference is what the monitor can hear.
func streamedRepackSeat(gap, total time.Duration) func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
	const finalObject = `{\"answer\":\"42\"}`
	return func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		if body["stream"] != true {
			time.Sleep(total)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` + finalObject + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":7}}`))
			return
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n")) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
			fl.Flush()
		}
		write(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		for elapsed := time.Duration(0); elapsed < total; elapsed += gap {
			time.Sleep(gap)
			write(`{"choices":[{"index":0,"delta":{"content":" "},"finish_reason":null}]}`) // whitespace: a token the object may carry before its first brace
		}
		write(`{"choices":[{"index":0,"delta":{"content":"` + finalObject + `"},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		write(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":7}}`)
		write(`[DONE]`)
	}
}

// A seat that streams its re-pack is never a stall while it produces, however
// long the answer takes (register C-66, RC-6, PR-12): deltas every 40 ms for more
// than three times the allowance are progress, and the request finishes on its first
// attempt. Asked for one JSON answer the SAME seat is silent for those two
// seconds and is filed as a stall at the allowance (TestRepackSilentSeatStillStalls), which is what the
// flat non-streamed re-pack did to a slow vLLM seat producing at 3-8 tok/s.
func TestRepackSlowStreamingSeatIsNotStalled(t *testing.T) {
	defer compressLiveness(t, 600*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 600*time.Millisecond)()
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repackStream: streamedRepackSeat(40*time.Millisecond, 2*time.Second),
	}
	srv := fake.server(t)
	defer srv.Close()

	start := time.Now()
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("a streaming seat was killed while it produced: %s / %q", wire.DeferClass, wire.Reason)
	}
	if wire.RepackAttempts != 1 || fake.grammarCNT.Load() != 1 {
		t.Fatalf("attempts = %d, grammar requests = %d, want 1", wire.RepackAttempts, fake.grammarCNT.Load())
	}
	if !strings.Contains(string(wire.Structured), `"answer":"42"`) {
		t.Fatalf("structured = %s", wire.Structured)
	}
	if el := time.Since(start); el < 1500*time.Millisecond {
		t.Fatalf("the re-pack finished in %s: the fake did not stream for ten allowances, the test proves nothing", el)
	}
}

// The guard on the other side: headers and then silence is still a stall, inside
// the allowance. A response that has started is not progress; only a token is.
func TestRepackSilentSeatStillStalls(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 200*time.Millisecond)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush() // headers out, then nothing
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	start := time.Now()
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassInfrastructure || !strings.Contains(wire.Reason, "stalled: no progress for") || !strings.Contains(wire.Reason, "in repack") {
		t.Fatalf("want an infrastructure stall in repack, got deferred=%v class=%s reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if wire.RepackAttempts != 1 {
		t.Fatalf("repack_attempts = %d, want 1", wire.RepackAttempts)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("a silent seat held the re-pack for %s: the stall did not fire inside the allowance", el)
	}
}

// The job record follows the re-pack: phase "repack", tokens climbing and a
// last-progress stamp that advances with every report. A delegator polling the
// node reads exactly this; until now it read the loop's last step.
func TestRepackProgressReachesTheJobRecord(t *testing.T) {
	defer compressLiveness(t, 600*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressRepackBound(t, 900*time.Millisecond)()
	old := progressReportEvery
	progressReportEvery = 5 * time.Millisecond
	defer func() { progressReportEvery = old }()
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repackStream: streamedRepackSeat(30*time.Millisecond, 600*time.Millisecond),
	}
	srv := fake.server(t)
	defer srv.Close()

	var mu sync.Mutex
	var reports []core.LiveProgress
	ctx := core.WithProgressReport(context.Background(), func(p core.LiveProgress) { mu.Lock(); reports = append(reports, p); mu.Unlock() })
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(ctx, agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s / %q", wire.DeferClass, wire.Reason)
	}

	mu.Lock()
	defer mu.Unlock()
	var stamps []int64
	maxTokens := 0
	for _, r := range reports {
		if r.Phase != "repack" {
			continue
		}
		stamps = append(stamps, r.LastProgressMs)
		if r.TokensOut > maxTokens {
			maxTokens = r.TokensOut
		}
	}
	if len(stamps) < 5 {
		t.Fatalf("only %d re-pack reports reached the job record: %+v", len(stamps), reports)
	}
	if stamps[len(stamps)-1] <= stamps[0] {
		t.Fatalf("last_progress_ms did not advance across the re-pack: %v", stamps)
	}
	if maxTokens < 10 {
		t.Fatalf("tokens_out reached only %d on the record, want the deltas counted", maxTokens)
	}
}

// On a seat the box declares as vLLM the re-pack asks for the stream TOGETHER
// with structured_outputs and the usage frame — the combination vLLM's own
// structured-outputs example runs with --stream — and a seat behind a proxy that
// ignores `stream` answers one JSON body, which is decoded as before.
func TestRepackOnAVLLMSeatStreamsWithStructuredOutputsAndFallsBackToJSON(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) }, // the proxy ignores stream: one JSON body
		chatBodies:   make(chan map[string]any, 4),
	}
	srv := fake.server(t)
	defer srv.Close()
	p := agentTestPipeline(t, srv.URL)
	cfg := p.Cfg()
	cfg.VLLMSeats = []string{agentTestSeat}
	p.cfg = cfg

	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred || !strings.Contains(string(wire.Structured), `"answer":"42"`) {
		t.Fatalf("deferred=%v structured=%s reason=%q, want the JSON body decoded", wire.Deferred, wire.Structured, wire.Reason)
	}
	select {
	case body := <-fake.chatBodies:
		if body["stream"] != true {
			t.Fatalf("stream = %v, want the re-pack to ask for a stream", body["stream"])
		}
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Fatalf("stream_options = %v, want include_usage", body["stream_options"])
		}
		if _, ok := body["structured_outputs"].(map[string]any); !ok {
			t.Fatalf("structured_outputs missing from the streamed request: %v", body)
		}
		if _, hasGrammar := body["grammar"]; hasGrammar {
			t.Fatal("a vLLM seat received a grammar")
		}
	default:
		t.Fatal("the vLLM seat never received a re-pack request")
	}
}
