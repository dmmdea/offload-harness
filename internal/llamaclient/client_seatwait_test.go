package llamaclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/seatwait"
)

func TestGenerateWaitsOutA429UsingTheContextBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"concurrency_limit","src":"llama-swap"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"a\":1}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":3}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "/v1/chat/completions", "m", 10*time.Second)
	b := seatwait.NewBudget(5)
	res, err := c.Generate(seatwait.WithBudget(context.Background(), b), "m", "sys", "user", "", 16, 0, 0)
	if err != nil || calls.Load() != 2 || res.Content == "" {
		t.Fatalf("err=%v calls=%d content=%q", err, calls.Load(), res.Content)
	}
	if b.Spent() != time.Second {
		t.Fatalf("the Retry-After:1 sleep must be counted: %v", b.Spent())
	}
}

func TestGenerateWithoutBudgetKeepsTheOldFailFastShape(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("busy"))
	}))
	defer srv.Close()
	c := New(srv.URL, "/v1/chat/completions", "m", 10*time.Second)
	_, err := c.Generate(context.Background(), "m", "sys", "user", "", 16, 0, 0)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("want one attempt and a *StatusError 429: %T %v calls=%d", err, err, calls.Load())
	}
}

func TestGenerateDoesNotRetryARealServerFault(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("CUDA error: out of memory"))
	}))
	defer srv.Close()
	c := New(srv.URL, "/v1/chat/completions", "m", 10*time.Second)
	b := seatwait.NewBudget(30)
	_, err := c.Generate(seatwait.WithBudget(context.Background(), b), "m", "sys", "user", "", 16, 0, 0)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusInternalServerError || calls.Load() != 1 || b.Spent() != 0 {
		t.Fatalf("a CUDA OOM is not a wait: %v calls=%d spent=%v", err, calls.Load(), b.Spent())
	}
}
