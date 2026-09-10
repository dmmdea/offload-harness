package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestInLoopOffloadModelFollowsThePlannerSeat (0.115.18, register D-88): the
// in-loop offload_* tools ride the planner seat whenever the planner is not the
// workhorse — the workhorse shares the planner's llama-swap and loading it
// evicts the planner mid-run. A single-model box keeps the workhorse.
func TestInLoopOffloadModelFollowsThePlannerSeat(t *testing.T) {
	cases := []struct {
		planner, workhorse, want string
		onSeat                   bool
	}{
		{"agent-pool", "gemma-4-e4b", "agent-pool", true},
		{"gemma-4-e4b", "gemma-4-e4b", "gemma-4-e4b", false},
		{"", "gemma-4-e4b", "gemma-4-e4b", false},
	}
	for _, c := range cases {
		got, onSeat := InLoopOffloadModel(c.planner, c.workhorse)
		if got != c.want || onSeat != c.onSeat {
			t.Errorf("InLoopOffloadModel(%q, %q) = %q,%v; want %q,%v", c.planner, c.workhorse, got, onSeat, c.want, c.onSeat)
		}
	}
}

// tierRecorder is a chat-completions endpoint that records each request body
// and answers a triage-shaped result.
func tierRecorder(t *testing.T) (*httptest.Server, chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"decision\":\"yes\",\"reason\":\"the text says so\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9}}`))
	}))
	return srv, bodies
}

// TestInLoopOffloadOnTheSeatRendersWithoutThinking: on the seat, the tier
// request names the seat and asks for a non-thinking render; on the workhorse
// the request is the pre-0.115.18 one (workhorse model, no template kwargs).
func TestInLoopOffloadOnTheSeatRendersWithoutThinking(t *testing.T) {
	srv, bodies := tierRecorder(t)
	defer srv.Close()
	cfg := config.Config{Endpoint: srv.URL, Model: "workhorse", Temperature: 0.1}
	params := map[string]any{"question": "is the sky blue?"}

	onSeat := NewRecordlessOffloadForPlanner(cfg, "seat-x", 10*time.Second)
	if _, err := onSeat(context.Background(), "triage", "the sky is blue today", params); err != nil {
		t.Fatalf("seat closure: %v", err)
	}
	b := <-bodies
	if b["model"] != "seat-x" || !repackDisablesThinking(b) {
		t.Fatalf("seat tier request: model=%v kwargs=%v; want the planner seat with enable_thinking=false", b["model"], b["chat_template_kwargs"])
	}

	onWorkhorse := NewRecordlessOffloadForPlanner(cfg, "workhorse", 10*time.Second)
	if _, err := onWorkhorse(context.Background(), "triage", "the sky is blue today", params); err != nil {
		t.Fatalf("workhorse closure: %v", err)
	}
	b = <-bodies
	if _, has := b["chat_template_kwargs"]; b["model"] != "workhorse" || has {
		t.Fatalf("workhorse tier request: model=%v kwargs=%v; want the workhorse with no template kwargs", b["model"], b["chat_template_kwargs"])
	}
}

// TestInLoopPipelineHonoursSeatEndpoints (reviewer finding, PR #302): the
// cached in-loop pipeline behind the MCP front door and the CLI must route an
// overridden seat exactly as the recordless (fleet-node) one does — the tools
// can now target a seat with its own endpoint, and a bare client would have
// sent that request to the base endpoint without an error.
func TestInLoopPipelineHonoursSeatEndpoints(t *testing.T) {
	base, baseBodies := tierRecorder(t)
	defer base.Close()
	seat, seatBodies := tierRecorder(t)
	defer seat.Close()
	cfg := config.Config{Endpoint: base.URL, Model: "workhorse", Temperature: 0.1,
		SeatEndpoints: map[string]string{"seat-x": seat.URL}}
	onSeat := NewInLoopOffloadForPlanner(cfg, "seat-x", 10*time.Second, nil)
	if _, err := onSeat(context.Background(), "triage", "the sky is blue today", map[string]any{"question": "is the sky blue?"}); err != nil {
		t.Fatalf("closure: %v", err)
	}
	select {
	case b := <-seatBodies:
		if b["model"] != "seat-x" {
			t.Fatalf("seat endpoint got model=%v, want seat-x", b["model"])
		}
	case b := <-baseBodies:
		t.Fatalf("the seat request went to the BASE endpoint (model=%v): seat_endpoints ignored", b["model"])
	case <-time.After(5 * time.Second):
		t.Fatal("no request recorded")
	}
}
