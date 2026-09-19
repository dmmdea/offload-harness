package llamaclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Register D-128, MEASURED 2026-09-18 on the Qube pair seat (qwen3.8-27b-vllm,
// vLLM behind llama-swap): a /v1/chat/completions request with logprobs:true
// and top_logprobs:5 answers with the OpenAI shape
// choices[0].logprobs.content[] — each entry carrying token, logprob, bytes and
// top_logprobs[{token, logprob, bytes}] — which is the shape this client decodes.
// The "legacy" {tokens, token_logprobs, top_logprobs:[{tok: lp}]} shape the
// 2026-09-18 research measured belongs to /v1/completions, which the harness
// never calls. This test pins the measured body (first content entry verbatim)
// so a vLLM seat's logprobs keep reaching the confidence gate.
const vllmChatLogprobBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"qwen3.8-27b-vllm",
"choices":[{"index":0,"message":{"role":"assistant","content":"{\n  \"label\": \"billing"},"finish_reason":"length",
"logprobs":{"content":[
 {"token":"{","logprob":-0.18319809436798096,"bytes":[123],"top_logprobs":[
   {"token":"{","logprob":-0.18319809436798096,"bytes":[123]},
   {"token":"` + "```" + `","logprob":-1.808198094367981,"bytes":[96,96,96]},
   {"token":"{\"","logprob":-5.808197975158691,"bytes":[123,34]},
   {"token":"[","logprob":-7.1,"bytes":[91]},
   {"token":"Sure","logprob":-8.2,"bytes":[83,117,114,101]}]},
 {"token":"\n  \"label\": \"","logprob":-0.01,"bytes":[10],"top_logprobs":[
   {"token":"\n  \"label\": \"","logprob":-0.01,"bytes":[10]}]},
 {"token":"billing","logprob":-0.02,"bytes":[98,105,108,108,105,110,103],"top_logprobs":[
   {"token":"billing","logprob":-0.02,"bytes":[98,105,108,108,105,110,103]},
   {"token":"technical","logprob":-4.0,"bytes":[116]},
   {"token":"sales","logprob":-6.0,"bytes":[115]}]}
]}}],
"usage":{"prompt_tokens":40,"completion_tokens":8,"total_tokens":48}}`

func TestVLLMChatLogprobShapeDecodesIntoTopAlternatives(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(vllmChatLogprobBody))
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/chat/completions", "qwen3.8-27b-vllm", 10*time.Second)
	res, err := c.Generate(context.Background(), "", "classify", "ticket", "", 8, 0, 5)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Logprobs) != 3 {
		t.Fatalf("decoded %d logprob tokens, want 3 (the measured content[] length)", len(res.Logprobs))
	}
	if got := res.Logprobs[0].Top; len(got) != 5 || got[0].Token != "{" || got[1].Token != "```" {
		t.Fatalf("content[0].top_logprobs decoded as %+v; the measured vLLM shape must round-trip", got)
	}
	label := res.Logprobs[2]
	if label.Token != "billing" || len(label.Top) != 3 || label.Top[1].Token != "technical" {
		t.Fatalf("the label token's alternatives did not decode: %+v", label)
	}
}
