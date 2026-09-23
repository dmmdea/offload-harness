package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// heldToolStream is the wire shape vLLM's qwen3_xml parser produced on the
// 3-card seat (2026-09-23 capture): the role frame, the tool name, the string
// argument streamed token by token, then NOTHING while the model writes a
// trailing non-string argument (the parser cannot emit an object before it
// closes), then the whole held argument in one frame. With return_token_ids
// the engine sends a frame per step during the hold, carrying only token_ids.
const heldToolStream = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"prompt_token_ids":[1,2,3]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","index":0,"function":{"name":"offload_extract"}}]},"finish_reason":null,"token_ids":[10,11]}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\": \"fox\", \"schema\": "}}]},"finish_reason":null,"token_ids":[12]}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":null,"token_ids":[13]}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":null,"token_ids":[14]}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":null,"token_ids":[15]}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"type\": \"object\"}}"}}]},"finish_reason":null,"token_ids":[16]}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls","token_ids":[17]}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":8}}

data: [DONE]

`

// Every frame that carries generated token ids is progress, counted by the
// ids it carries — the frames the parser held back included — and a tool-call
// frame that names the tool (no arguments yet) is progress too.
func TestDecodeSSECountsTokenIDFramesAsProgress(t *testing.T) {
	var ticks []int
	wr, err := decodeSSE(strings.NewReader(heldToolStream), func(n int) { ticks = append(ticks, n) })
	if err != nil {
		t.Fatal(err)
	}
	want := []int{2, 3, 4, 5, 6, 7, 8}
	if fmt.Sprint(ticks) != fmt.Sprint(want) {
		t.Fatalf("progress ticks = %v, want %v (one per frame with ids, counted by ids)", ticks, want)
	}
	ch := wr.Choices[0]
	if len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].Function.Name != "offload_extract" ||
		ch.Message.ToolCalls[0].Function.Arguments != `{"text": "fox", "schema": {"type": "object"}}` || ch.FinishReason != "tool_calls" {
		t.Fatalf("the held call must still assemble: %+v finish=%q", ch.Message.ToolCalls, ch.FinishReason)
	}
	if wr.Usage.CompletionTokens != 8 {
		t.Fatalf("usage lost: %+v", wr.Usage)
	}
}

// A tool-call frame that carries only the id and the name is generated output
// (vLLM sends the name in its own frame before any argument).
func TestDecodeSSEToolNameFrameIsProgress(t *testing.T) {
	stream := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c","type":"function","index":0,"function":{"name":"read_file"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" + "data: [DONE]\n\n"
	var ticks []int
	if _, err := decodeSSE(strings.NewReader(stream), func(n int) { ticks = append(ticks, n) }); err != nil {
		t.Fatal(err)
	}
	if len(ticks) != 1 || ticks[0] != 1 {
		t.Fatalf("ticks = %v, want [1]: the tool-name frame is a generated token", ticks)
	}
}

// vllmHeldToolSeat emulates the engine behaviour measured on the seat: during
// a held tool-call argument it sends a frame per step ONLY when the request
// asked for return_token_ids; otherwise the wire is silent for the hold.
func vllmHeldToolSeat(t *testing.T, hold, step time.Duration, bodies chan<- map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if bodies != nil {
			bodies <- req
		}
		ids, _ := req["return_token_ids"].(bool)
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) { _, _ = w.Write([]byte("data: " + s + "\n\n")); fl.Flush() } // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		tok := func() string {
			if ids {
				return `,"token_ids":[7]`
			}
			return ""
		}
		write(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c","type":"function","index":0,"function":{"name":"offload_extract"}}]},"finish_reason":null` + tok() + `}]}`)
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\": \"x\", \"schema\": "}}]},"finish_reason":null` + tok() + `}]}`)
		for end := time.Now().Add(hold); time.Now().Before(end); {
			if ids {
				write(`{"choices":[{"index":0,"delta":{},"finish_reason":null,"token_ids":[8]}]}`)
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(step):
			}
		}
		write(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}}"}}]},"finish_reason":"tool_calls"` + tok() + `}]}`)
		write(`[DONE]`)
	}))
}

// The defect, end to end at the client: a held tool call longer than the
// decoding allowance. Asked for token ids, the engine's per-step frames keep
// the stall watch fed and the call completes; not asked, the same engine is
// silent and the watch files a stall with 3 tokens so far — the 2026-09-23
// E2E defer shape.
func TestHeldToolCallIsProgressOnlyWithTokenIDs(t *testing.T) {
	pol := StallPolicy{Admission: time.Second, PrefillTokS: 1e6, TokS: 1e6, Floor: 150 * time.Millisecond, Slack: 0}
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("token_ids=%v", on), func(t *testing.T) {
			bodies := make(chan map[string]any, 1)
			srv := vllmHeldToolSeat(t, 600*time.Millisecond, 20*time.Millisecond, bodies)
			defer srv.Close()
			ctx, m := NewMonitor(context.Background(), pol, 10*time.Second)
			defer m.Stop()
			m.Phase(PhasePrefill, 10)
			ctx = ContextWithProgress(ctx, m.Progress)
			c := NewLLMClient(srv.URL, "seat", "", 5*time.Second).WithStreamTokenIDs(on)
			comp, err := c.Chat(ctx, []Msg{{Role: "user", Content: "x"}}, nil, 64)
			body := <-bodies
			if _, has := body["return_token_ids"]; has != on {
				t.Fatalf("return_token_ids on the wire = %v, want %v", has, on)
			}
			var se *StallError
			if on {
				if err != nil || m.Cause() != nil {
					t.Fatalf("a producing engine was filed as stalled: err=%v cause=%v", err, m.Cause())
				}
				if len(comp.Msg.ToolCalls) != 1 || comp.Msg.ToolCalls[0].Args != `{"text": "x", "schema": {}}` {
					t.Fatalf("tool call = %+v", comp.Msg.ToolCalls)
				}
				return
			}
			if err == nil || !errors.As(m.Cause(), &se) || se.Phase != PhaseDecoding {
				t.Fatalf("without the engine's signal the hold must read as silence: err=%v cause=%v", err, m.Cause())
			}
		})
	}
}

// Without a progress listener the body is the historical one even when the
// option is on: the CLI doors and the probes never see the key.
func TestStreamTokenIDsNeedsAListener(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	srv := vllmHeldToolSeat(t, 0, time.Millisecond, bodies)
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second).WithStreamTokenIDs(true)
	if _, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "x"}}, nil, 64); err != nil {
		t.Fatal(err)
	}
	if _, has := (<-bodies)["return_token_ids"]; has {
		t.Fatal("return_token_ids sent on a call nothing listens to")
	}
}
