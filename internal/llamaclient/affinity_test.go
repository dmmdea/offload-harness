package llamaclient

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

// affinityServer is a llama-server stand-in that reports which model each
// request named and lets the test hold one model's requests open.
type affinityServer struct {
	mu       sync.Mutex
	live     map[string]int // model -> requests currently inside the handler
	overlaps []string       // two different models seen inside at once

	holdModel string
	hold      chan struct{}
	entered   chan string
}

func newAffinityServer(holdModel string) *affinityServer {
	return &affinityServer{
		live:      map[string]int{},
		holdModel: holdModel,
		hold:      make(chan struct{}),
		entered:   make(chan string, 64),
	}
}

func (s *affinityServer) handler(w http.ResponseWriter, r *http.Request) {
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
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
}

func (s *affinityServer) overlapReport() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.overlaps...)
}

// Every generation method must sit behind the gate: a request naming a
// different model, while another model's request is in flight on the same
// resolved base, parks instead of forcing llama-swap to swap the slot. The
// assertion is deterministic — the parked caller's own ctx fires and the error
// is the gate's, not the transport's. Ungated, the stand-in server answers a
// non-held model in microseconds and the call would simply succeed.
func TestEveryGenerationMethodParksBehindAForeignModel(t *testing.T) {
	cases := []struct {
		name string
		call func(c *Client, ctx context.Context) error
	}{
		{"Generate", func(c *Client, ctx context.Context) error {
			_, err := c.Generate(ctx, "other-model", "", "hi", "", 8, 0, 0)
			return err
		}},
		{"GenerateVision", func(c *Client, ctx context.Context) error {
			_, err := c.GenerateVision(ctx, "other-model", "", "hi", []string{"data:image/png;base64,AA=="}, "", 8, 0, 0)
			return err
		}},
		{"GenerateVisionInterleaved", func(c *Client, ctx context.Context) error {
			_, err := c.GenerateVisionInterleaved(ctx, "other-model", "", []string{"f0"}, []string{"data:image/png;base64,AA=="}, "hi", "", 8, 0, 0)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAffinityServer("held-model")
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			c := New(srv.URL, "", "held-model", 30*time.Second)

			done := make(chan struct{})
			go func() {
				defer close(done)
				if _, err := c.Generate(context.Background(), "held-model", "", "hi", "", 8, 0, 0); err != nil {
					t.Errorf("held Generate: %v", err)
				}
			}()
			if got := <-s.entered; got != "held-model" {
				t.Fatalf("first request into the server was %q, want held-model", got)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			err := tc.call(c, ctx)
			cancel()
			close(s.hold)
			<-done

			var we *modelaffinity.WaitError
			if !errors.As(err, &we) {
				t.Fatalf("%s returned %v (%T); want a *modelaffinity.WaitError — the call is not behind the gate", tc.name, err, err)
			}
			if we.Held != "held-model" || we.Want != "other-model" {
				t.Errorf("WaitError = %+v, want Held=held-model Want=other-model", we)
			}
			if we.Base != srv.URL {
				t.Errorf("WaitError.Base = %q, want the RESOLVED base %q", we.Base, srv.URL)
			}
			if ov := s.overlapReport(); len(ov) > 0 {
				t.Errorf("server saw two models at once: %v", ov)
			}
		})
	}
}

// After the held batch drains, the model that was parked gets through. A gate
// that blocked the switch permanently would be worse than the thrash.
func TestGenerateProceedsOnceTheHeldModelDrains(t *testing.T) {
	s := newAffinityServer("held-model")
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	defer srv.Close()
	c := New(srv.URL, "", "held-model", 30*time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.Generate(context.Background(), "held-model", "", "hi", "", 8, 0, 0); err != nil {
			t.Errorf("held Generate: %v", err)
		}
	}()
	<-s.entered

	other := make(chan error, 1)
	go func() {
		_, err := c.Generate(context.Background(), "other-model", "", "hi", "", 8, 0, 0)
		other <- err
	}()
	time.Sleep(50 * time.Millisecond) // the parked call must not have reached the server
	if ov := s.overlapReport(); len(ov) > 0 {
		close(s.hold)
		t.Fatalf("the switching call reached the server while held-model was in flight: %v", ov)
	}
	close(s.hold)
	<-done
	if err := <-other; err != nil {
		t.Fatalf("the parked call never got through: %v", err)
	}
	if ov := s.overlapReport(); len(ov) > 0 {
		t.Fatalf("server saw two models at once: %v", ov)
	}
}

// Same model, same base: still concurrent. The handler barrier is the
// assertion — it only releases once every request is inside it at the same
// instant, so a gate that serialised same-model traffic would hang here.
func TestGenerateSameModelStillOverlaps(t *testing.T) {
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
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "", "same-model", 30*time.Second)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Generate(context.Background(), "same-model", "", "hi", "", 8, 0, 0); err != nil {
				t.Errorf("Generate: %v", err)
			}
		}()
	}
	wg.Wait()
	select {
	case <-all:
	default:
		t.Fatalf("fewer than %d same-model requests were ever inside the server at once: the gate serialised them", n)
	}
}

// Two llama-swap instances are two independent slots. A client on base A must
// not park behind a different model held on base B.
func TestGenerateOnAnotherBaseDoesNotPark(t *testing.T) {
	s1 := newAffinityServer("held-model")
	srv1 := httptest.NewServer(http.HandlerFunc(s1.handler))
	defer srv1.Close()
	s2 := newAffinityServer("")
	srv2 := httptest.NewServer(http.HandlerFunc(s2.handler))
	defer srv2.Close()

	c1 := New(srv1.URL, "", "held-model", 30*time.Second)
	c2 := New(srv2.URL, "", "other-model", 30*time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c1.Generate(context.Background(), "held-model", "", "hi", "", 8, 0, 0); err != nil {
			t.Errorf("held Generate: %v", err)
		}
	}()
	<-s1.entered

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c2.Generate(ctx, "other-model", "", "hi", "", 8, 0, 0); err != nil {
		close(s1.hold)
		<-done
		t.Fatalf("a call on a DIFFERENT base parked behind an unrelated one: %v", err)
	}
	close(s1.hold)
	<-done
}
