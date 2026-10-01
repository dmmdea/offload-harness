package llamaclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// GenResult carries the engine's own finish_reason, on the JSON path and on the
// stream path alike (register C-80). Truncated says only "length"; the re-pack's
// per-attempt record needs the reason itself, because a run to the cap, a stop
// and a cut tool call are different findings.
func TestGenerateReportsTheFinishReasonOnBothPaths(t *testing.T) {
	for _, finish := range []string{"stop", "length", "tool_calls"} {
		t.Run(finish, func(t *testing.T) {
			jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
			}))
			defer jsonSrv.Close()
			streamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sseWrite(w,
					`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`,
					`{"choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`,
					`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
					`[DONE]`)
			}))
			defer streamSrv.Close()

			plain, err := New(jsonSrv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0)
			if err != nil {
				t.Fatalf("JSON Generate: %v", err)
			}
			streamed, err := New(streamSrv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
			if err != nil {
				t.Fatalf("streamed Generate: %v", err)
			}
			if plain.FinishReason != finish {
				t.Errorf("JSON path: FinishReason = %q, want %q", plain.FinishReason, finish)
			}
			if streamed.FinishReason != finish {
				t.Errorf("stream path: FinishReason = %q, want %q", streamed.FinishReason, finish)
			}
			if wantCut := finish == "length"; plain.Truncated != wantCut || streamed.Truncated != wantCut {
				t.Errorf("Truncated = %v (JSON) / %v (stream), want %v: it stays \"finish_reason is length\"", plain.Truncated, streamed.Truncated, wantCut)
			}
		})
	}
}
