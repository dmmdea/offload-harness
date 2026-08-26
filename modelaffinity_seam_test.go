package main

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

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// This is the measured incident, reproduced across the two packages that
// produced it. The agent seat (internal/agent.LLMClient.Chat) and a cascade
// text seat (internal/llamaclient.Client.Generate) are SEPARATE HTTP paths that
// share one llama-swap endpoint in the default config. Neither package can see
// the other, so a gate living in either one alone cannot fix this — the check
// has to be here, where both lanes exist at once.
func TestAgentSeatAndCascadeSeatDoNotThrashOneBase(t *testing.T) {
	var mu sync.Mutex
	live := map[string]int{}
	var overlaps []string
	hold := make(chan struct{})
	entered := make(chan string, 16)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)

		mu.Lock()
		for m, n := range live {
			if m != probe.Model && n > 0 {
				overlaps = append(overlaps, probe.Model+" entered while "+m+" was in flight")
			}
		}
		live[probe.Model]++
		mu.Unlock()

		select {
		case entered <- probe.Model:
		default:
		}
		if probe.Model == "agent-seat" {
			<-hold
		}

		mu.Lock()
		live[probe.Model]--
		mu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	agentClient := agent.NewLLMClient(srv.URL, "agent-seat", "", 30*time.Second)
	cascade := llamaclient.New(srv.URL, "", "cascade-seat", 30*time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := agentClient.Chat(context.Background(), []agent.Msg{{Role: "user", Content: "hi"}}, nil, 8); err != nil {
			t.Errorf("agent Chat: %v", err)
		}
	}()
	if got := <-entered; got != "agent-seat" {
		t.Fatalf("first request into llama-swap was %q, want agent-seat", got)
	}

	// The cascade call arrives while the agent seat is mid-generation. Before
	// this change it went straight through and llama-swap evicted the agent's
	// model to serve it.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, err := cascade.Generate(ctx, "cascade-seat", "", "summarize this", "", 8, 0, 0)
	cancel()

	var we *modelaffinity.WaitError
	if !errors.As(err, &we) {
		close(hold)
		<-done
		t.Fatalf("cascade Generate returned %v (%T) while the agent seat was generating; want a *modelaffinity.WaitError — the two lanes still thrash one base", err, err)
	}
	if we.Held != "agent-seat" || we.Want != "cascade-seat" {
		t.Errorf("WaitError = %+v, want Held=agent-seat Want=cascade-seat", we)
	}

	// Drain the agent seat; the cascade seat must then get its switch.
	close(hold)
	<-done
	if _, err := cascade.Generate(context.Background(), "cascade-seat", "", "summarize this", "", 8, 0, 0); err != nil {
		t.Fatalf("cascade never got its switch after the agent seat drained: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(overlaps) > 0 {
		t.Fatalf("llama-swap saw two models at once: %v", overlaps)
	}
}
