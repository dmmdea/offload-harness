package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// coherenceSeatServer is the agent_run door's fake endpoint: the seat is ABSENT
// until the passthrough GET loads it (the cold load the D-118 probe keys on),
// and every /v1/chat/completions answer is scripted by `chat`, which receives
// the decoded request body so a test can tell the probe from a loop step.
func coherenceSeatServer(t *testing.T, seat string, chat func(body map[string]any) string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var loaded atomic.Bool
	var chats atomic.Int64
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
			loaded.Store(true) // the cold load
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, seat)
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			chats.Add(1)
			fmt.Fprint(w, chat(body))
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, &chats
}

// isProbeBody recognises the D-118 probe by its shape: exactly one user message
// opening with the probe goal.
func isProbeBody(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		return false
	}
	m, _ := msgs[0].(map[string]any)
	c, _ := m["content"].(string)
	return m["role"] == "user" && strings.HasPrefix(c, "Read the file notes.md")
}

func coherenceCfg(t *testing.T, base, seat, policy string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = t.TempDir()
	cfg.Endpoint = base
	cfg.Model = seat
	cfg.AgentModel = seat
	cfg.AgentAdmissionWaitSec = 30
	cfg.AgentCoherenceProbe = policy
	return cfg
}

// TestAgentRunDefersAnIncoherentSeat (register D-118): the MCP door runs the
// SAME post-warm probe the delegation door does. A seat that cold-loads and
// then answers the probe with token-0 spam defers in seconds, with the
// coherence note on the result — the loop never starts.
func TestAgentRunDefersAnIncoherentSeat(t *testing.T) {
	const seat = "agent-pool"
	srv, chats := coherenceSeatServer(t, seat, func(body map[string]any) string {
		if isProbeBody(body) {
			// The probe takes measurable time so the admission assertion below
			// is about BOOKKEEPING and not about the host clock: warm-up plus
			// probe on a loopback fake otherwise complete inside one Windows
			// timer tick and every duration reads 0.
			time.Sleep(1200 * time.Millisecond)
			return `{"choices":[{"message":{"role":"assistant","content":"<tool_call>` +
				strings.Repeat("!", 40) + `"},"finish_reason":"length"}]}`
		}
		return `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`
	})
	defer srv.Close()
	s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, ""), nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("an incoherent seat must defer, got: %v", m)
	}
	reason, _ := m["reason"].(string)
	if !strings.HasPrefix(reason, core.IncoherentSeatReason) {
		t.Fatalf("reason = %q, want the %q prefix", reason, core.IncoherentSeatReason)
	}
	note, _ := m["coherence_note"].(string)
	if note == "" || !strings.Contains(note, "incoherent") {
		t.Fatalf("coherence_note = %q, want the broken verdict", note)
	}
	if n := chats.Load(); n != 1 {
		t.Fatalf("chat completions = %d, want exactly 1 (the probe) — the loop must never start", n)
	}
	if wait, _ := m["admission_wait_sec"].(float64); wait < 1.1 {
		t.Fatalf("admission_wait_sec = %v, want the probe's ~1.2 s charged to admission on the DEFER path too", wait)
	}
}

// TestAgentRunCoherentSeatRunsAndReportsTheNote: the same cold load, a parsed
// read_file call, the run proceeds and the note says the tool call parsed.
func TestAgentRunCoherentSeatRunsAndReportsTheNote(t *testing.T) {
	const seat = "agent-pool"
	srv, chats := coherenceSeatServer(t, seat, func(body map[string]any) string {
		if isProbeBody(body) {
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"p","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.md\"}"}}]},"finish_reason":"tool_calls"}]}`
		}
		return `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`
	})
	defer srv.Close()
	s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, ""), nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("the run deferred: %v", m)
	}
	if note, _ := m["coherence_note"].(string); !strings.Contains(note, "tool call parsed") {
		t.Fatalf("coherence_note = %q, want the parsed-tool-call verdict", note)
	}
	if n := chats.Load(); n < 2 {
		t.Fatalf("chat completions = %d, want the probe plus at least one loop step", n)
	}
}

// TestAgentRunSkipsTheCoherenceProbeOnAWarmSeat: the default policy probes only
// after a cold load, on this door exactly as on the other one — a warm seat
// costs no extra completion and carries no note.
func TestAgentRunSkipsTheCoherenceProbeOnAWarmSeat(t *testing.T) {
	const seat = "agent-pool"
	var probes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/running":
			fmt.Fprintf(w, `{"running":[{"model":%q,"state":"ready","cmd":"x"}]}`, seat)
		case "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, seat)
		case "/upstream/" + seat + "/v1/models":
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"max_model_len":65536}]}`, seat)
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if isProbeBody(body) {
				probes.Add(1)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, ""), nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("the run deferred: %v", m)
	}
	if n := probes.Load(); n != 0 {
		t.Fatalf("probe completions = %d, want 0 on a warm seat", n)
	}
	if _, ok := m["coherence_note"]; ok {
		t.Fatalf("coherence_note is present (%v) on a run whose probe never fired", m["coherence_note"])
	}
}

// TestAgentRunCoherenceProbeIsBoundedByTheAdmissionBudget guards the one thing
// the probe must never do on this door: spend the WALL. A 1 s wall with a
// ~1.5 s probe still runs its loop.
func TestAgentRunCoherenceProbeIsBoundedByTheWall(t *testing.T) {
	const seat = "agent-pool"
	srv, _ := coherenceSeatServer(t, seat, func(body map[string]any) string {
		if isProbeBody(body) {
			time.Sleep(1500 * time.Millisecond)
			return `{"choices":[{"message":{"role":"assistant","content":"DONE"},"finish_reason":"stop"}]}`
		}
		return `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`
	})
	defer srv.Close()
	s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, ""), nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":1}`, t.TempDir())))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("the probe consumed the wall: %v", m)
	}
	if note, _ := m["coherence_note"].(string); !strings.Contains(note, "without a tool call") {
		t.Fatalf("coherence_note = %q, want the text-answer verdict", note)
	}
	if wait, _ := m["admission_wait_sec"].(float64); wait < 1.4 {
		t.Fatalf("admission_wait_sec = %v, want the probe's ~1.5 s charged to admission", wait)
	}
}
