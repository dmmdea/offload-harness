package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// TestAgentRunWarmsAColdSeatBeforeProbingItsWindow (2026-09-16): the MCP
// agent_run door never ran the cold-load warm-up the pipeline door has had since
// 0.115.11 (D-64), so on a cold seat its window probe raced a model still
// loading. The same agent_run measured ctx_window 8,192 cold and 114,688 warm,
// minutes apart, on the Qube agent-pool seat. The seat here is absent; the first
// passthrough GET is the load. The run must load it OUTSIDE the wall, say so
// under the wire's own field names, and budget against the served window.
func TestAgentRunWarmsAColdSeatBeforeProbingItsWindow(t *testing.T) {
	const seat = "agent-pool"
	var loaded atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			if loaded.Load() {
				fmt.Fprintf(w, `{"running":[{"model":%q,"state":"ready","cmd":"x"}]}`, seat)
				return
			}
			fmt.Fprint(w, `{"running":[]}`)
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, seat)
		case "/upstream/" + seat + "/v1/models":
			if !loaded.Load() {
				time.Sleep(1200 * time.Millisecond) // the cold load
				loaded.Store(true)
			}
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, seat)
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = t.TempDir()
	cfg.Endpoint = srv.URL
	cfg.Model = seat
	cfg.AgentModel = seat
	cfg.AgentAdmissionWaitSec = 30
	s := New(pipeline.New(cfg, nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("the run deferred: %v", m)
	}
	if w, _ := m["ctx_window"].(float64); int(w) != 65536 {
		t.Fatalf("ctx_window = %v, want 65536 (the served window, not the %d fallback)", m["ctx_window"], 8192)
	}
	note, _ := m["admission_note"].(string)
	wait, _ := m["admission_wait_sec"].(float64)
	if wait < 1.1 || !strings.Contains(note, "cold load") || !strings.Contains(note, "outside the wall") {
		t.Fatalf("admission_wait_sec=%v admission_note=%q, want the cold load reported outside the wall", wait, note)
	}
}
