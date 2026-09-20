package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A streamed completion carries the time to its first delta (0.131.1): the
// engine-neutral prefill measurement.
func TestChatRecordsTimeToFirstDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		time.Sleep(60 * time.Millisecond) // the "prefill"
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5000,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":1000}}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 5*time.Second)
	comp, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 8)
	if err != nil {
		t.Fatal(err)
	}
	if comp.FirstDeltaMS < 50 || comp.FirstDeltaMS > 2000 {
		t.Fatalf("FirstDeltaMS = %.1f, want ~60", comp.FirstDeltaMS)
	}
	if comp.Serve == nil || comp.Serve.UsagePromptTokens != 5000 || comp.Serve.UsageCachedTokens != 1000 {
		t.Fatalf("usage not read: %+v", comp.Serve)
	}
}

// The loop folds each call's (uncached prompt tokens, first-delta ms) into
// Result.PrefillSamples, skipping calls that measured nothing.
func TestLoopPublishesPrefillSamples(t *testing.T) {
	c := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop",
			Serve: &ServeStats{UsagePromptTokens: 5000, UsageCachedTokens: 1000, UsageCompletionTokens: 3}, FirstDeltaMS: 4000},
	}}
	l := NewLoop(c, nil, 1)
	res, err := l.Run(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PrefillSamples) != 1 || res.PrefillSamples[0].Tokens != 4000 || res.PrefillSamples[0].MS != 4000 {
		t.Fatalf("samples = %+v", res.PrefillSamples)
	}
	// no timing -> no sample
	c2 := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop",
		Serve: &ServeStats{UsagePromptTokens: 5000}}}}
	res2, _ := NewLoop(c2, nil, 1).Run(context.Background(), "x")
	if len(res2.PrefillSamples) != 0 {
		t.Fatalf("a call without a delta timing must not sample: %+v", res2.PrefillSamples)
	}
}
