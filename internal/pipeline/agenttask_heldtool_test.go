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

// heldToolLoop is a vLLM seat whose tool parser holds the trailing non-string
// argument of the first call (read_file's integer `limit`) for `hold`: during
// the hold it sends one token_ids frame per step when the request asked for
// return_token_ids, and nothing at all otherwise — the behaviour measured on
// the 3-card seat on 2026-09-23. The second loop call answers.
func heldToolLoop(hold time.Duration, mu *sync.Mutex, sawIDs *[]bool) func(int64, map[string]any, http.ResponseWriter, *http.Request) {
	return func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		ids, _ := body["return_token_ids"].(bool)
		mu.Lock()
		*sawIDs = append(*sawIDs, ids)
		mu.Unlock()
		if n > 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(doneChat("The answer is 42."))) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
			return
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte("data: " + s + "\n\n")); fl.Flush() } // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		tok := ""
		if ids {
			tok = `,"token_ids":[5]`
		}
		write(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c1","type":"function","index":0,"function":{"name":"read_file"}}]},"finish_reason":null` + tok + `}]}`)
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\": \"notes.md\", \"limit\": "}}]},"finish_reason":null` + tok + `}]}`)
		for end := time.Now().Add(hold); time.Now().Before(end); {
			if ids {
				write(`{"choices":[{"index":0,"delta":{},"finish_reason":null,"token_ids":[6]}]}`)
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"40}"}}]},"finish_reason":"tool_calls"` + tok + `}]}`)
		write(`{"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":12}}`)
		write(`[DONE]`)
	}
}

// The real call site: a seat this box declares as vLLM is asked for its
// per-token progress signal, so a tool call the parser holds for longer than
// the stall allowance completes; the same seat NOT declared vLLM gets the
// historical body and the same hold is filed as a stall — the 2026-09-23 E2E
// defer, reproduced.
func TestRunAgentTaskHeldToolCallOnVLLMSeatIsProgress(t *testing.T) {
	restore := compressLiveness(t, 300*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)
	defer restore()
	for _, declared := range []bool{true, false} {
		name := map[bool]string{true: "vllm-declared", false: "undeclared"}[declared]
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var sawIDs []bool
			fake := &agentFake{
				rosterIDs:  []string{agentTestSeat},
				loopStream: heldToolLoop(1200*time.Millisecond, &mu, &sawIDs),
			}
			srv := fake.server(t)
			defer srv.Close()
			p := agentTestPipeline(t, srv.URL)
			cfg := p.Cfg()
			cfg.AgentSeatTokS = 1000 // a measured-fast seat: the decoding allowance is the (compressed) floor
			if declared {
				cfg.VLLMSeats = []string{agentTestSeat}
			}
			p.cfg = cfg
			contract := testContract()
			contract.OutputSchema = nil // the loop is under test, not the re-pack
			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
			mu.Lock()
			first := len(sawIDs) > 0 && sawIDs[0]
			n := len(sawIDs)
			mu.Unlock()
			if n == 0 || first != declared {
				t.Fatalf("return_token_ids on the loop call = %v, want %v", sawIDs, declared)
			}
			if declared {
				if wire.Deferred {
					t.Fatalf("a held tool call on a producing vLLM seat was deferred: %s (%s)", wire.Reason, wire.DeferClass)
				}
				if wire.Output != "The answer is 42." {
					t.Fatalf("output = %q", wire.Output)
				}
				return
			}
			if !wire.Deferred || !strings.HasPrefix(wire.Reason, "stalled: no progress for ") {
				t.Fatalf("without the engine's signal the hold is silence: deferred=%v reason=%q", wire.Deferred, wire.Reason)
			}
		})
	}
}
