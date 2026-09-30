package llamaclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sseWrite writes SSE frames the way llama-server and vLLM do, flushing each.
func sseWrite(w http.ResponseWriter, frames ...string) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	for _, f := range frames {
		_, _ = w.Write([]byte("data: " + f + "\n\n")) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
		if fl != nil {
			fl.Flush()
		}
	}
}

// answerFrames is one structured answer as a stream: a role frame that carries
// no text, two content deltas, the finish frame, the usage frame and [DONE].
var answerFrames = []string{
	`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`{"choices":[{"index":0,"delta":{"content":"{\"answer\":"},"finish_reason":null}]}`,
	`{"choices":[{"index":0,"delta":{"content":"\"42\"}"},"finish_reason":null}]}`,
	`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5}}`,
	`[DONE]`,
}

const answerJSON = `{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"42\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`

// progressLog collects the counts a WithProgress callback hears.
type progressLog struct {
	mu   sync.Mutex
	seen []int
}

func (p *progressLog) fn(n int) { p.mu.Lock(); p.seen = append(p.seen, n); p.mu.Unlock() }
func (p *progressLog) get() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.seen...)
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode request body: %v", err)
	}
	return body
}

// A call that passes WithProgress streams, and the stream decodes to the same
// GenResult the JSON answer to the same request does (ADR 0055 item 1, applied
// to the re-pack).
func TestGenerateWithProgressStreamsAndDecodesLikeJSON(t *testing.T) {
	var streamBody map[string]any
	streamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamBody = decodeBody(t, r)
		// A keep-alive comment and an event line between frames are not data.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": keep-alive\n\nevent: message\n\n"))
		sseWrite(w, answerFrames...)
	}))
	defer streamSrv.Close()
	jsonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer jsonSrv.Close()

	var log progressLog
	streamed, err := New(streamSrv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "sys", "user", "", 64, 0, 0, WithProgress(log.fn))
	if err != nil {
		t.Fatalf("streamed Generate: %v", err)
	}
	plain, err := New(jsonSrv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "sys", "user", "", 64, 0, 0)
	if err != nil {
		t.Fatalf("JSON Generate: %v", err)
	}
	if streamed.Content != plain.Content || streamed.TokensIn != plain.TokensIn || streamed.TokensOut != plain.TokensOut || streamed.Truncated != plain.Truncated {
		t.Fatalf("the stream decoded differently from the JSON answer:\n stream=%+v\n json=%+v", streamed, plain)
	}
	if streamed.Content != `{"answer":"42"}` || streamed.TokensIn != 11 || streamed.TokensOut != 5 || streamed.Truncated {
		t.Fatalf("streamed result = %+v", streamed)
	}
	// One progress event per frame that carried text: the role frame and the
	// finish/usage frames carry none.
	if got := log.get(); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("progress = %v, want [1 2]", got)
	}
	// The request asked for the stream and for the usage frame.
	if streamBody["stream"] != true {
		t.Fatalf("stream = %v, want true", streamBody["stream"])
	}
	so, _ := streamBody["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage true", streamBody["stream_options"])
	}
}

// A call that passes no WithProgress sends the request it always sent: no
// stream, no stream_options key.
func TestGenerateWithoutProgressSendsTheHistoricalBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 16, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, has := body["stream_options"]; has || body["stream"] != false {
		t.Fatalf("option-free body carries stream=%v stream_options=%v", body["stream"], body["stream_options"])
	}
}

// A proxy that ignores `stream` answers one JSON body to a streamed request:
// decoded as before, and the whole answer is one progress event.
func TestGenerateWithProgressFallsBackToJSONWhenTheProxyIgnoresStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeBody(t, r)["stream"] != true {
			t.Error("the request did not ask for a stream")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer srv.Close()

	var log progressLog
	res, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(log.fn))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Content != `{"answer":"42"}` || res.TokensOut != 5 {
		t.Fatalf("result = %+v", res)
	}
	if got := log.get(); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("progress = %v, want the one event of the whole answer [5]", got)
	}
}

// A stream that dies mid-body is the wire's own failure — a *BodyError, which
// the pipeline files as the seat not being reachable — never a model that got
// the shape wrong.
func TestGenerateWithProgressMidStreamDeathIsABodyError(t *testing.T) {
	t.Run("connection dropped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("no hijacker")
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			// A chunked answer that promises more, sends two deltas and drops.
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
			for _, f := range answerFrames[1:3] {
				frame := "data: " + f + "\n\n"
				_, _ = buf.WriteString(hexLen(len(frame)) + "\r\n" + frame + "\r\n")
			}
			_ = buf.Flush()
			_ = conn.Close()
		}))
		defer srv.Close()
		var log progressLog
		_, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(log.fn))
		var be *BodyError
		if !errors.As(err, &be) {
			t.Fatalf("err = %v (%T), want a *BodyError", err, err)
		}
		if got := log.get(); !reflect.DeepEqual(got, []int{1, 2}) {
			t.Fatalf("progress = %v, want the two deltas heard before the drop", got)
		}
	})
	t.Run("ended without a finish", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sseWrite(w, answerFrames[1:3]...) // then a clean EOF: no finish_reason, no [DONE]
		}))
		defer srv.Close()
		_, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
		var be *BodyError
		if !errors.As(err, &be) {
			t.Fatalf("err = %v (%T), want a *BodyError", err, err)
		}
	})
	t.Run("engine error frame", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sseWrite(w, answerFrames[1], `{"error":{"message":"EngineCore died","type":"server_error"}}`)
		}))
		defer srv.Close()
		_, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
		var be *BodyError
		if !errors.As(err, &be) {
			t.Fatalf("err = %v (%T), want a *BodyError", err, err)
		}
	})
}

func hexLen(n int) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{digits[n%16]}, out...)
		n /= 16
	}
	return string(out)
}

// A streamed call is not cut by the client's own Timeout, which covers the whole
// body read: a producing seat whose answer takes longer than the transport bound
// finishes. The same server answering a plain call is cut, as before.
func TestGenerateWithProgressSuppressesTheClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeBody(t, r)["stream"] != true {
			time.Sleep(500 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(answerJSON))
			return
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 8; i++ { // a delta every 60 ms: ~480 ms in all, past the 200 ms Timeout
			time.Sleep(60 * time.Millisecond)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n")) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter — a test fake streaming SSE, not HTML
			fl.Flush()
		}
		sseWrite(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`)
	}))
	defer srv.Close()

	c := New(srv.URL, "", "m", 200*time.Millisecond)
	var log progressLog
	res, err := c.Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(log.fn))
	if err != nil || res.Content != "xxxxxxxx" {
		t.Fatalf("streamed call: res=%+v err=%v, want the stream that outlived the client Timeout", res, err)
	}
	if n := len(log.get()); n != 8 {
		t.Fatalf("progress events = %d, want 8", n)
	}
	if _, err := c.Generate(context.Background(), "", "", "u", "", 64, 0, 0); err == nil {
		t.Fatal("a plain call must still be cut by the client Timeout")
	}
}

// A server that REFUSES a streamed request is asked again as one JSON answer;
// the refusal is remembered, so the next call to that seat goes straight to JSON.
// This is the safe default for a seat that does not take stream together with the
// rest of the body.
func TestGenerateWithProgressRetriesAsJSONWhenTheServerRefusesStream(t *testing.T) {
	var streamed, plain atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeBody(t, r)["stream"] == true {
			streamed.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"stream is not supported here"}`))
			return
		}
		plain.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer srv.Close()
	c := New(srv.URL, "", "m", 5*time.Second)

	var log progressLog
	res, err := c.Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(log.fn))
	if err != nil || res.Content != `{"answer":"42"}` {
		t.Fatalf("first call: res=%+v err=%v, want the JSON retry's answer", res, err)
	}
	if streamed.Load() != 1 || plain.Load() != 1 {
		t.Fatalf("requests: streamed=%d plain=%d, want 1 refused stream then 1 JSON", streamed.Load(), plain.Load())
	}
	if got := log.get(); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("progress = %v, want the JSON answer as one event [5]", got)
	}

	// Remembered: the second call does not stream at all.
	if _, err := c.Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {})); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if streamed.Load() != 1 || plain.Load() != 2 {
		t.Fatalf("requests after the second call: streamed=%d plain=%d, want the refusal remembered (1, 2)", streamed.Load(), plain.Load())
	}
}

// A request that is refused however it is sent (a 400 for both) is the
// request's fault: its own error comes back and nothing is remembered, so a seat
// that streams fine is not switched off by one bad request.
func TestGenerateWithProgressDoesNotRememberARefusalTheJSONRetryShares(t *testing.T) {
	var streamed atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeBody(t, r)["stream"] == true {
			streamed.Add(1)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"context length exceeded"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", "m", 5*time.Second)

	_, err := c.Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want the request's own 400", err)
	}
	if _, err := c.Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {})); err == nil {
		t.Fatal("second call must fail the same way")
	}
	if streamed.Load() != 2 {
		t.Fatalf("streamed requests = %d, want 2: a shared refusal must not switch streaming off for the seat", streamed.Load())
	}
}

// A call that asked for logprobs is never streamed (the decoder does not read
// them): it goes out as the plain request it always was.
func TestGenerateWithProgressIsNotStreamedWhenLogprobsAreAsked(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 16, 0, 3, WithProgress(func(int) {})); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != false {
		t.Fatalf("stream = %v with logprobs asked, want false", body["stream"])
	}
}
