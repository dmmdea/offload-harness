package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/seatwait"
)

const okChatBody = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"completion_tokens":1}}`

func TestChatWaitsOutA429UsingTheContextBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"concurrency_limit","src":"llama-swap"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(okChatBody))
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 30*time.Second)
	b := seatwait.NewBudget(5)
	ctx := seatwait.WithBudget(context.Background(), b)
	comp, err := c.Chat(ctx, []Msg{{Role: "user", Content: "hi"}}, nil, 16)
	if err != nil || comp.Msg.Content != "ok" || calls.Load() != 3 {
		t.Fatalf("err=%v content=%q calls=%d", err, comp.Msg.Content, calls.Load())
	}
	if b.Spent() != 2*time.Second || b.Attempts() != 2 {
		t.Fatalf("two Retry-After:1 sleeps must be counted: spent=%v attempts=%d", b.Spent(), b.Attempts())
	}
}

func TestChatWithoutBudgetFailsFastWithTypedStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("busy"))
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 30*time.Second)
	_, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 16)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTooManyRequests {
		t.Fatalf("want *StatusError 429, got %T %v", err, err)
	}
	if err.Error() != "chat 429: busy" {
		t.Fatalf("message text must be unchanged for log greps: %q", err.Error())
	}
	if calls.Load() != 1 {
		t.Fatalf("no budget = exactly one attempt, got %d", calls.Load())
	}
}

func TestChatNonRetryable4xxIsNotWaitedOut(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("context length exceeded"))
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 30*time.Second)
	b := seatwait.NewBudget(30)
	_, err := c.Chat(seatwait.WithBudget(context.Background(), b), []Msg{{Role: "user", Content: "hi"}}, nil, 16)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest || calls.Load() != 1 || b.Spent() != 0 {
		t.Fatalf("a 400 is the REQUEST's fault: err=%v calls=%d spent=%v", err, calls.Load(), b.Spent())
	}
}

func TestChatBudgetExhaustedReturnsTheBusyStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("[seat] process is not ready"))
	}))
	defer srv.Close()
	c := NewLLMClient(srv.URL, "seat", "", 30*time.Second)
	b := seatwait.NewBudget(2) // two 1 s sleeps, then the failure stands
	_, err := c.Chat(seatwait.WithBudget(context.Background(), b), []Msg{{Role: "user", Content: "hi"}}, nil, 16)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 after the budget: %T %v", err, err)
	}
	if calls.Load() != 3 || b.Spent() != 2*time.Second {
		t.Fatalf("calls=%d spent=%v", calls.Load(), b.Spent())
	}
}

func TestChatReleasesTheAffinityTicketWhileSleeping(t *testing.T) {
	// Model "a" 429s once with Retry-After: 2 while model "b" on the SAME base
	// must be admitted during that sleep. Before this change the sleeper held
	// the modelaffinity ticket, and "b" was parked behind a request that was
	// not even in flight.
	var first atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "a" && first.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(okChatBody))
	}))
	defer srv.Close()
	ca := NewLLMClient(srv.URL, "a", "", 30*time.Second)
	cb := NewLLMClient(srv.URL, "b", "", 30*time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := ca.Chat(seatwait.WithBudget(context.Background(), seatwait.NewBudget(10)), []Msg{{Role: "user", Content: "x"}}, nil, 8)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // "a" is now inside its 2 s sleep
	t0 := time.Now()
	if _, err := cb.Chat(context.Background(), []Msg{{Role: "user", Content: "y"}}, nil, 8); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el > time.Second {
		t.Fatalf("model b was parked behind a sleeping model-a ticket: %v", el)
	}
	if err := <-done; err != nil {
		t.Fatalf("model a must succeed after its wait: %v", err)
	}
}
