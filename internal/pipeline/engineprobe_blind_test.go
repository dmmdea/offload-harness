package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// Which engine sources can see a prefill in progress (ADR 0066). The busy hold's flat
// bound keeps the waiting phase's allowance, stretched by the load, ONLY for a
// source whose counters do not move through a prefill: a llama-server's /slots
// (its prompt processing is invisible) and a vLLM exposition with no KV-usage gauge
// (a solo prefill produces no token and no iteration stats). Every other source
// moves its fingerprint through a prefill, so a flat fingerprint there is a hung
// engine whatever is queued behind it — and giving those engines the stretched
// bound held a hung seat for minutes to hours under load.
func TestEngineActivityProbeMarksASourceThatCannotSeeAPrefillBlind(t *testing.T) {
	slotsBody := `[{"id":0,"id_task":3,"is_processing":true,"n_prompt_tokens":18,"n_prompt_tokens_processed":0,"next_token":[{"n_decoded":5}]}]`
	llamaCppMetrics := "llamacpp:requests_processing 1\nllamacpp:requests_deferred 0\nllamacpp:n_decode_total 77\n" +
		"llamacpp:prompt_tokens_total 812\nllamacpp:tokens_predicted_total 4096\n"
	for _, tc := range []struct {
		name      string
		metrics   func() (int, string) // /seat/metrics
		slots     string               // /seat/slots ("" = not served)
		wantBlind bool
		wantSrc   string
	}{
		{"vLLM with its KV-usage gauge moves through a prefill", nil, "", false, "vllm-metrics"},
		{"vLLM without a KV-usage gauge cannot see a solo prefill", func() (int, string) {
			return 200, "vllm:num_requests_running 3\nvllm:num_requests_waiting 2\nvllm:iteration_tokens_total_count 10\nvllm:generation_tokens_total 40\nvllm:prompt_tokens_total 900\n"
		}, "", true, "vllm-metrics"},
		{"llama-server --metrics counts every decode call, prompt batches included", func() (int, string) { return 200, llamaCppMetrics }, "", false, "llamacpp-metrics"},
		{"llama-server without --metrics: /slots cannot see prompt processing", func() (int, string) { return http.StatusNotImplemented, "" }, slotsBody, true, "slots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &engineSwap{}
			base := e.server(t)
			defer base.Close()
			var srv *httptest.Server
			if tc.metrics == nil {
				srv = base
			} else {
				srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1/models":
						_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"qwen-27b","meta":{"llamaswap":{"aliases":["agent-pool"]}}}]}`)
					case "/running":
						_ = json.NewEncoder(w).Encode(map[string]any{"running": []map[string]string{{"model": "qwen-27b", "state": "ready", "proxy": "http://" + r.Host + "/seat"}}})
					case "/seat/metrics":
						code, body := tc.metrics()
						w.WriteHeader(code)
						_, _ = io.WriteString(w, body)
					case "/seat/slots":
						if tc.slots == "" {
							http.NotFound(w, r)
							return
						}
						_, _ = io.WriteString(w, tc.slots)
					default:
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
			}
			probe := engineActivityProbe(srv.URL, "agent-pool", seatLoadProbe(srv.URL, "agent-pool"))
			rd, err := probe(context.Background())
			if err != nil || rd.Fingerprint == "" {
				t.Fatalf("reading = %+v err=%v", rd, err)
			}
			if !strings.HasPrefix(rd.Summary, tc.wantSrc) {
				t.Fatalf("summary = %q, want the reading to come from %s (the fixture must exercise the source it names)", rd.Summary, tc.wantSrc)
			}
			if rd.PrefillBlind != tc.wantBlind {
				t.Fatalf("PrefillBlind = %v for %s (%s), want %v", rd.PrefillBlind, tc.name, rd.Summary, tc.wantBlind)
			}
		})
	}
}

// The production policy arms two recoveries per run (ADR 0066): the flagship engine
// died ten times in one day with a median up-time of 6.7 minutes, so one run can meet
// two. Every e2e test needs at least one and the agent tests build their own
// policies, so nothing else pins the number the node actually runs with.
func TestLivenessPolicyCarriesTwoSeatRecoveries(t *testing.T) {
	p := LivenessPolicyFor(config.Config{}, seatrate.Seat{}, 0)
	if p.SeatRecoveries != 2 {
		t.Fatalf("SeatRecoveries = %d, want 2 (ADR 0066: a run recovers at most twice)", p.SeatRecoveries)
	}
	if p.ColdLoad <= 0 {
		t.Fatalf("ColdLoad = %v: a recovery wait needs a ceiling", p.ColdLoad)
	}
	if p.EngineFlat <= 0 || p.EnginePoll <= 0 || p.EngineProbeTimeout < time.Second {
		t.Fatalf("the busy hold is not armed: %+v", p)
	}
}
