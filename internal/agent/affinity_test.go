package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The agent seat is the lane the measured incident degraded (qwen3.8-27b, 72s
// to 307s), and it does NOT go through internal/llamaclient — LLMClient.Chat
// posts to /v1/chat/completions itself. A gate installed only on the cascade
// side would be a lock only one side takes, which is the exact shape
// internal/gpulease was written to end. So Chat must sit behind the same gate.
type agentAffinityServer struct {
	mu        sync.Mutex
	live      map[string]int
	overlaps  []string
	holdModel string
	hold      chan struct{}
	entered   chan string
}

func newAgentAffinityServer(holdModel string) *agentAffinityServer {
	return &agentAffinityServer{
		live:      map[string]int{},
		holdModel: holdModel,
		hold:      make(chan struct{}),
		entered:   make(chan string, 64),
	}
}

func (s *agentAffinityServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)

	s.mu.Lock()
	for m, n := range s.live {
		if m != probe.Model && n > 0 {
			s.overlaps = append(s.overlaps, probe.Model+" entered while "+m+" was in flight")
		}
	}
	s.live[probe.Model]++
	s.mu.Unlock()

	select {
	case s.entered <- probe.Model:
	default:
	}
	if probe.Model == s.holdModel {
		<-s.hold
	}

	s.mu.Lock()
	s.live[probe.Model]--
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
}

func (s *agentAffinityServer) overlapReport() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.overlaps...)
}

func TestChatParksBehindAForeignModelOnTheSameBase(t *testing.T) {
	s := newAgentAffinityServer("held-model")
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	held := NewLLMClient(srv.URL, "held-model", "", 30*time.Second)
	other := NewLLMClient(srv.URL, "other-model", "", 30*time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := held.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 8); err != nil {
			t.Errorf("held Chat: %v", err)
		}
	}()
	if got := <-s.entered; got != "held-model" {
		t.Fatalf("first request into the server was %q, want held-model", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, err := other.Chat(ctx, []Msg{{Role: "user", Content: "hi"}}, nil, 8)
	cancel()
	close(s.hold)
	<-done

	var we *modelaffinity.WaitError
	if !errors.As(err, &we) {
		t.Fatalf("Chat returned %v (%T); want a *modelaffinity.WaitError — the agent seat is not behind the gate", err, err)
	}
	if we.Held != "held-model" || we.Want != "other-model" || we.Base != srv.URL {
		t.Errorf("WaitError = %+v, want Held=held-model Want=other-model Base=%s", we, srv.URL)
	}
	if ov := s.overlapReport(); len(ov) > 0 {
		t.Errorf("server saw two models at once: %v", ov)
	}
}

// The agent loop issues many sequential Chat calls on ONE seat, and two agent
// runs on the same seat must keep overlapping. Serialising them would slow the
// lane the change exists to protect.
func TestChatSameModelStillOverlaps(t *testing.T) {
	const n = 3
	var mu sync.Mutex
	inside := 0
	all := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inside++
		reached := inside >= n
		mu.Unlock()
		if reached {
			once.Do(func() { close(all) })
		}
		select {
		case <-all:
		case <-time.After(3 * time.Second):
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "same-model", "", 30*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 8); err != nil {
				t.Errorf("Chat: %v", err)
			}
		}()
	}
	wg.Wait()
	select {
	case <-all:
	default:
		t.Fatalf("fewer than %d same-model Chat calls were ever inside the server at once: the gate serialised them", n)
	}
}

// After the held batch drains the agent seat gets its switch.
func TestChatProceedsOnceTheHeldModelDrains(t *testing.T) {
	s := newAgentAffinityServer("held-model")
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()

	held := NewLLMClient(srv.URL, "held-model", "", 30*time.Second)
	other := NewLLMClient(srv.URL, "other-model", "", 30*time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := held.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 8); err != nil {
			t.Errorf("held Chat: %v", err)
		}
	}()
	<-s.entered

	res := make(chan error, 1)
	go func() {
		_, err := other.Chat(context.Background(), []Msg{{Role: "user", Content: "hi"}}, nil, 8)
		res <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if ov := s.overlapReport(); len(ov) > 0 {
		close(s.hold)
		t.Fatalf("the switching call reached the server while held-model was in flight: %v", ov)
	}
	close(s.hold)
	<-done
	if err := <-res; err != nil {
		t.Fatalf("the parked agent call never got through: %v", err)
	}
}
