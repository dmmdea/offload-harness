package seatload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeSwap is a llama-swap stand-in: /v1/models serves a roster (an id with
// aliases), /running lists the seat by CANONICAL id when loaded, and
// /upstream/<name>/metrics serves a vLLM-shaped exposition with the in-flight
// count the test controls (llama-swap resolves aliases on /upstream, so the
// fake answers for both names). It records whether the upstream path was ever
// touched while the seat was NOT loaded — the one thing a reader must never do.
type fakeSwap struct {
	id, alias                 string
	roster                    bool // serve /v1/models at all (false = roster unreadable)
	loaded                    atomic.Bool
	inflight                  atomic.Int64
	upstreamHitsWhileUnloaded atomic.Int64
	metricsStatus             atomic.Int64
}

func (f *fakeSwap) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !f.roster {
			http.Error(w, "no roster", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + f.id + `","meta":{"llamaswap":{"aliases":["` + f.alias + `"]}}},{"id":"other-seat"}]}`))
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		var running []map[string]string
		if f.loaded.Load() {
			running = append(running, map[string]string{"model": f.id, "state": "ready"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
	})
	metrics := func(w http.ResponseWriter, r *http.Request) {
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
		}
		if st := f.metricsStatus.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		n := f.inflight.Load()
		_, _ = w.Write([]byte("# HELP vllm:num_requests_running x\nvllm:num_requests_running{engine=\"0\"} " +
			strconv.FormatInt(n, 10) + "\nvllm:num_requests_waiting{engine=\"0\"} 0.0\nvllm:num_requests_running_total 99\n"))
	}
	slots := func(w http.ResponseWriter, r *http.Request) {
		if !f.loaded.Load() {
			f.upstreamHitsWhileUnloaded.Add(1)
		}
		n := f.inflight.Load()
		out := []map[string]any{}
		for i := 0; i < 2; i++ {
			out = append(out, map[string]any{"id": i, "is_processing": int64(i) < n})
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	for _, name := range []string{f.id, f.alias} {
		mux.HandleFunc("/upstream/"+name+"/metrics", metrics)
		mux.HandleFunc("/upstream/"+name+"/slots", slots)
	}
	return mux
}

// TestInflightResolvesAnAliasBoundSeat is the regression for the 0.113.16–19
// drain defect: the seat is bound as an ALIAS, /running lists the canonical
// id, and the reader must still see it as loaded and count its requests.
func TestInflightResolvesAnAliasBoundSeat(t *testing.T) {
	f := &fakeSwap{id: "qwen3.8-27b-vllm", alias: "agent-pool", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(3)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "agent-pool")
	if err != nil {
		t.Fatalf("Inflight: %v", err)
	}
	if !rd.Loaded || rd.Inflight != 3 || rd.Canonical != "qwen3.8-27b-vllm" || rd.Source != "metrics" {
		t.Fatalf("reading = %+v; want loaded, 3 in flight, canonical qwen3.8-27b-vllm via metrics", rd)
	}
}

// TestInflightMatchesTheBareNameWhenTheRosterIsUnreadable: an unreadable
// roster must not turn into a refusal — the reader falls back to the name the
// caller bound (the pre-0.113.20 behaviour), which still works for an id-bound
// seat.
func TestInflightMatchesTheBareNameWhenTheRosterIsUnreadable(t *testing.T) {
	f := &fakeSwap{id: "qwen3.5-4b-vllm", alias: "a2-pool", roster: false}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "qwen3.5-4b-vllm")
	if err != nil {
		t.Fatalf("Inflight: %v", err)
	}
	if !rd.Loaded || rd.Inflight != 1 || rd.Canonical != "" {
		t.Fatalf("reading = %+v; want loaded, 1 in flight, no canonical (roster unreadable)", rd)
	}
	// And the alias-bound case WITHOUT a roster reads as not loaded but
	// AMBIGUOUS (the roster failed and /running holds an entry the reading
	// could not resolve) — the honest limit of the fallback, flagged, not an
	// error: the drain refuses to call it drained, the deal treats it as idle.
	rd, err = Inflight(context.Background(), srv.Client(), srv.URL, "a2-pool")
	if err != nil || rd.Loaded || !rd.Ambiguous || rd.RosterErr == nil || rd.RunningOthers != 1 {
		t.Fatalf("alias without a roster: reading = %+v, err %v; want not loaded, ambiguous, roster error, 1 other running", rd, err)
	}
	// With NOTHING running the same unreadable roster is not ambiguous: an
	// empty /running is idle whatever the names are.
	f.loaded.Store(false)
	rd, err = Inflight(context.Background(), srv.Client(), srv.URL, "a2-pool")
	if err != nil || rd.Loaded || rd.Ambiguous || rd.RunningOthers != 0 {
		t.Fatalf("alias, no roster, nothing running: reading = %+v, err %v; want plainly not loaded", rd, err)
	}
}

func TestInflightNeverTouchesTheUpstreamOfAnUnloadedSeat(t *testing.T) {
	f := &fakeSwap{id: "seat", alias: "seat-alias", roster: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	for _, name := range []string{"seat", "seat-alias"} {
		rd, err := Inflight(context.Background(), srv.Client(), srv.URL, name)
		if err != nil || rd.Loaded || rd.Inflight != 0 {
			t.Fatalf("%s: reading = %+v, err %v; want not loaded", name, rd, err)
		}
	}
	if f.upstreamHitsWhileUnloaded.Load() != 0 {
		t.Fatal("the reader probed /upstream on an unloaded seat — that path LOADS the model")
	}
}

func TestInflightFallsBackToSlotsOnlyOn501Or404(t *testing.T) {
	f := &fakeSwap{id: "seat", alias: "seat-alias", roster: true}
	f.loaded.Store(true)
	f.inflight.Store(1)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	f.metricsStatus.Store(http.StatusNotImplemented)
	rd, err := Inflight(context.Background(), srv.Client(), srv.URL, "seat-alias")
	if err != nil || rd.Inflight != 1 || rd.Source != "slots" {
		t.Fatalf("501 → /slots: reading = %+v, err %v; want 1 processing via slots", rd, err)
	}
	f.metricsStatus.Store(http.StatusInternalServerError)
	if _, err := Inflight(context.Background(), srv.Client(), srv.URL, "seat-alias"); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("a 500 must be an error naming the status, never idle; got %v", err)
	}
}

func TestParseInflightSumsOnlyTheInflightGauges(t *testing.T) {
	body := `# HELP
vllm:num_requests_running{engine="0",model_name="m"} 2.0
vllm:num_requests_waiting{engine="0",model_name="m"} 1.0
vllm:num_requests_running_total 500
llamacpp:requests_processing 1
vllm:prompt_tokens_total 12345
`
	if got := ParseInflight(strings.NewReader(body)); got != 4 {
		t.Fatalf("inflight = %d, want 4 (2 running + 1 waiting + 1 processing; the _total counter must not count)", got)
	}
}

func TestParseSlotsInflightCountsProcessingSlots(t *testing.T) {
	n, err := ParseSlotsInflight(strings.NewReader(`[{"id":0,"is_processing":true,"n_ctx":65536},{"id":1,"is_processing":false},{"id":2,"is_processing":true}]`))
	if err != nil || n != 2 {
		t.Fatalf("parse = %d, %v; want 2 processing", n, err)
	}
	if _, err := ParseSlotsInflight(strings.NewReader(`{"error":"x"}`)); err == nil {
		t.Fatal("a non-array body must be an error, never zero in flight")
	}
}
