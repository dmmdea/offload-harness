package llamaclient

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// logSink is a goroutine-safe buffer for the standard logger.
type logSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureStdLog(t *testing.T) *logSink {
	t.Helper()
	s := &logSink{}
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(s)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })
	return s
}

// A seat that refuses the stream and then fails the JSON retry in a DIFFERENT way
// (here a 503 that is not a busy answer) reports only the retry's error, and the
// refusal that led to it was gone: nothing said the request had been a streamed
// one refused with 400 first. The log keeps both halves.
func TestGenerateWithProgressLogsTheRefusalWhenTheJSONRetryFailsDifferently(t *testing.T) {
	logged := captureStdLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeBody(t, r)["stream"] == true {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"stream is not supported together with structured outputs"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`backend down`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the retry's own 503", err)
	}
	if !strings.Contains(logged.String(), "refused a streamed request") || !strings.Contains(logged.String(), "the JSON retry then failed") || !strings.Contains(logged.String(), "stream is not supported") {
		t.Fatalf("log = %q, want the refusal and the retry's failure together", logged.String())
	}
}

// A request that is refused however it is sent (a context that does not fit is a
// 400 both ways) is the request's own fault, and logging it as a streaming
// problem on every such call would be noise.
func TestGenerateWithProgressDoesNotLogARequestRefusedBothWays(t *testing.T) {
	logged := captureStdLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"context length exceeded"}`))
	}))
	defer srv.Close()
	_, _ = New(srv.URL, "", "m", 5*time.Second).Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {}))
	if strings.Contains(logged.String(), "the JSON retry then failed") {
		t.Fatalf("log = %q, a request refused both ways is not a streaming refusal", logged.String())
	}
}

// After a refusal that the JSON retry answered, the client can say its target
// refuses streaming: the fact the re-pack's note on the wire is built from. Another
// target, and a client that has not been refused, say nothing.
func TestStreamRefusedForNamesTheSeatThatRefusedTheStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decodeBody(t, r)["stream"] == true {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"stream is not supported together with structured outputs"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answerJSON))
	}))
	defer srv.Close()
	c := New(srv.URL, "", "m", 5*time.Second)
	if c.StreamRefusedFor("m") {
		t.Fatal("a seat that has not been asked is not a seat that refused")
	}
	if _, err := c.Generate(context.Background(), "", "", "u", "", 64, 0, 0, WithProgress(func(int) {})); err != nil {
		t.Fatal(err)
	}
	if !c.StreamRefusedFor("m") || !c.StreamRefusedFor("") {
		t.Fatal("the refusal the JSON retry answered is not reported")
	}
	if other := New("http://192.0.2.1:9", "", "m", time.Second); other.StreamRefusedFor("m") {
		t.Fatal("another target reports a refusal it never made")
	}
}
