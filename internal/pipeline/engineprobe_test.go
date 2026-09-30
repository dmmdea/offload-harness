package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// engineSwap is a llama-swap stand-in for the busy hold's probe: a roster, a
// /running row carrying the seat's own address, and a vLLM-shaped /metrics at
// that address whose engine-step counter the test drives.
type engineSwap struct {
	state    atomic.Value // "ready" | "starting" | "" (not listed)
	steps    atomic.Int64
	upstream atomic.Int64
}

func (e *engineSwap) server(t *testing.T) *httptest.Server {
	t.Helper()
	e.state.Store("ready")
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen-27b","meta":{"llamaswap":{"aliases":["agent-pool"]}}}]}`))
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]string{}
		if st, _ := e.state.Load().(string); st != "" {
			rows = append(rows, map[string]string{"model": "qwen-27b", "state": st, "proxy": "http://" + r.Host + "/seat"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": rows})
	})
	mux.HandleFunc("/seat/metrics", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vllm:num_requests_running 3\nvllm:num_requests_waiting 2\nvllm:iteration_tokens_total_count " +
			strconv.FormatInt(e.steps.Load(), 10) + "\nvllm:generation_tokens_total 40\nvllm:prompt_tokens_total 900\nvllm:num_preemptions_total 4\n"))
	})
	mux.HandleFunc("/upstream/", func(w http.ResponseWriter, r *http.Request) {
		e.upstream.Add(1)
		http.Error(w, "never through /upstream", http.StatusTeapot)
	})
	return httptest.NewServer(mux)
}

// The pipeline's engine probe reads the seat's own address: a working engine
// moves its fingerprint, a seat being loaded is reported as a load (the
// cold-load hold's), and nothing is ever read through /upstream.
func TestEngineActivityProbeReadsTheSeatsEngine(t *testing.T) {
	e := &engineSwap{}
	srv := e.server(t)
	defer srv.Close()
	probe := engineActivityProbe(srv.URL, "agent-pool", seatLoadProbe(srv.URL, "agent-pool"))
	if probe == nil {
		t.Fatal("no probe for a configured endpoint and seat")
	}
	ctx := context.Background()
	a, err := probe(ctx)
	if err != nil || a.Loading || a.Fingerprint == "" {
		t.Fatalf("ready seat: reading=%+v err=%v", a, err)
	}
	if a.Summary != "vllm-metrics: 3 running, 2 waiting, 4 preemptions so far" {
		t.Fatalf("summary = %q", a.Summary)
	}
	e.steps.Add(1)
	b, err := probe(ctx)
	if err != nil || b.Fingerprint == a.Fingerprint {
		t.Fatalf("an engine step must move the fingerprint: %q vs %q (%v)", a.Fingerprint, b.Fingerprint, err)
	}
	e.state.Store("starting")
	c, err := probe(ctx)
	if err != nil || !c.Loading || c.Fingerprint != "" {
		t.Fatalf("a loading seat must read as a load, never as an engine: %+v %v", c, err)
	}
	if e.upstream.Load() != 0 {
		t.Fatalf("%d reads went through /upstream (it loads models and resets the idle unload timer)", e.upstream.Load())
	}
	if engineActivityProbe("", "agent-pool", nil) != nil || engineActivityProbe(srv.URL, "", nil) != nil {
		t.Fatal("no endpoint or no seat must mean no probe (the pre-0.143 rule)")
	}
}

// The production policy arms the busy hold.
func TestLivenessPolicyArmsTheBusyHold(t *testing.T) {
	pol := LivenessPolicyFor(config.Config{}, seatrate.Seat{}, 0)
	if pol.EngineFlat != engineFlatBound || pol.EnginePoll != enginePoll || pol.EngineFlat < 2*livenessFloor {
		t.Fatalf("EngineFlat=%s EnginePoll=%s, want %s / %s", pol.EngineFlat, pol.EnginePoll, engineFlatBound, enginePoll)
	}
}
