package llamaclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// vLLM reports a mid-stream engine death as an `error` data frame and then
// closes the stream with [DONE]. Only the explicit error check turns that into a
// wire failure; without it the [DONE] marks the stream complete and the
// half-answer is returned as a result.
func TestGenerateWithProgressEngineErrorFrameBeforeDoneIsABodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseWrite(w, answerFrames[1], `{"error":{"message":"EngineCore died","type":"server_error"}}`, `[DONE]`)
	}))
	defer srv.Close()
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	var be *BodyError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v (%T), res = %+v: an engine error frame followed by [DONE] must be the wire's failure, not a result", err, err, res)
	}
	if !strings.Contains(err.Error(), "EngineCore died") {
		t.Fatalf("err = %q, want the engine's own message", err)
	}
}

// a streamed answer cut at the completion budget is Truncated, exactly like
// the JSON answer's finish_reason "length": the re-pack keys its budget bump and
// its "re-pack truncated" note on it.
func TestGenerateWithProgressStreamCutAtTheCompletionBudgetIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseWrite(w,
			`{"choices":[{"index":0,"delta":{"content":"{\"answer\":\"4"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":64}}`,
			`[DONE]`)
	}))
	defer srv.Close()
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatalf("a stream that finished with finish_reason length is not Truncated: %+v", res)
	}
}

// a reasoning delta (vLLM `reasoning`, llama.cpp `reasoning_content`) is
// progress for the liveness rule and is never part of the answer.
func TestGenerateWithProgressReasoningDeltasAreProgressNotContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseWrite(w,
			`{"choices":[{"index":0,"delta":{"reasoning":"think "},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_content":"more "},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`)
	}))
	defer srv.Close()
	var log progressLog
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(log.fn))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "ok" {
		t.Fatalf("content = %q: reasoning leaked into the answer", res.Content)
	}
	if got := log.get(); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("progress = %v, want the two reasoning deltas and the content delta counted [1 2 3]", got)
	}
}

// one proxy that buffers delivers the whole answer as a single frame; a
// frame past bufio's 64 KB default token limit must still decode.
func TestGenerateWithProgressDecodesAFrameLargerThanTheScannerDefault(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseWrite(w,
			`{"choices":[{"index":0,"delta":{"content":"`+big+`"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`)
	}))
	defer srv.Close()
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if err != nil || res.Content != big {
		t.Fatalf("a 100 KB single frame: err=%v len(content)=%d", err, len(res.Content))
	}
}

// an engine that sends no usage frame (a proxy that drops include_usage) is
// counted by its deltas, and the engine's own timings give the decode rate the
// JSON path reads from the same field.
func TestGenerateWithProgressWithoutAUsageFrameCountsDeltasAndReadsTimings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseWrite(w,
			`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"b"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"c"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"timings":{"predicted_per_second":42.5}}`,
			`[DONE]`)
	}))
	defer srv.Close()
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	if res.TokensOut != 3 {
		t.Fatalf("tokens_out = %d, want the 3 deltas counted when no usage frame comes", res.TokensOut)
	}
	if res.TokPerSec != 42.5 {
		t.Fatalf("tok/s = %v, want the engine's own 42.5 (timings.predicted_per_second)", res.TokPerSec)
	}
}

// a 422 to a streamed request is asked again as JSON, like a 400.
func TestGenerateWithProgressRetriesAsJSONOn422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeBody(t, r)["stream"] == true {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"detail":"stream with structured_outputs is not supported"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer srv.Close()
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if err != nil || res.Content != `{"answer":"42"}` {
		t.Fatalf("res=%+v err=%v, want the JSON retry's answer after a 422", res, err)
	}
}

// SSE allows `data:` with no space after the colon; some servers write it so.
func TestGenerateWithProgressReadsDataLinesWithNoSpaceAfterTheColon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range answerFrames {
			_, _ = w.Write([]byte("data:" + f + "\n\n")) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		}
	}))
	defer srv.Close()
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if err != nil || res.Content != `{"answer":"42"}` {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// the JSON retry after a refused stream is a plain request. OpenAI-
// compatible engines (vLLM among them) reject `stream_options` without
// stream:true, so a retry that still carried it would be refused too and the
// safe default the fallback exists for would not hold on the very seat it was
// written for.
func TestGenerateWithProgressJSONRetryCarriesNoStreamOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		if body["stream"] == true {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"stream is not supported together with structured outputs"}`))
			return
		}
		if _, has := body["stream_options"]; has {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Stream options can only be defined when stream=True"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer srv.Close()
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if err != nil || res.Content != `{"answer":"42"}` {
		t.Fatalf("res=%+v err=%v, want the plain JSON retry answered", res, err)
	}
}

// a caller that never asked for a stream is not retried on a 400: the
// retry belongs to a streamed request only.
func TestGenerateWithoutProgressIsNotResentOnA400(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"context length exceeded"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want the 400", err)
	}
	if n.Load() != 1 {
		t.Fatalf("requests = %d, want exactly 1: a plain call has no stream to retry without", n.Load())
	}
}

// a remembered refusal expires: a seat that was fixed streams again after
// the TTL, and a fresh refusal is still remembered.
func TestStreamRefusalExpiresAfterItsTTL(t *testing.T) {
	old, fresh := streamRefusalKey("http://seat-old", "m"), streamRefusalKey("http://seat-fresh", "m")
	streamRefusals.Store(old, time.Now().Add(-streamRefusalTTL-time.Minute))
	streamRefusals.Store(fresh, time.Now().Add(-time.Minute))
	defer streamRefusals.Delete(old)
	defer streamRefusals.Delete(fresh)
	if streamWasRefused("http://seat-old", "m") {
		t.Fatal("a refusal older than the TTL is still remembered")
	}
	if !streamWasRefused("http://seat-fresh", "m") {
		t.Fatal("a refusal from a minute ago was forgotten")
	}
}
