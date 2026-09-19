// vllm_structured_outputs_test.go pins the ONE thing register D-129 is about:
// a seat the box declares as vLLM never receives llama.cpp's raw GBNF
// `grammar` field, and never has to.
//
// The defect: ADR 0002 constrains structured output with a top-level
// `grammar` member, which is llama.cpp's own request field. vLLM's request
// model ALLOWS unknown extras, so a vLLM seat behind llama-swap accepts the
// key, ignores it, and answers unconstrained — fenced prose around the object
// on a good day, something the schema validator rejects on a bad one. The
// repair shipped in 0.115.14 was a trim (`outerObject`) plus a coercion, and
// the delegation log over nine days measured what that costs: 1,018 re-packs
// on vLLM seats against 506 on every other seat, and 3-attempt exhaustion at
// 19.7 % against 12.6 %.
//
// ADR 0048's promise is that vLLM is a FIRST-CLASS engine on every tier, equal
// in standing to llama.cpp. A first-class engine gets its own constraint
// field — vLLM's `structured_outputs: {"json": <schema>}` — not a field it is
// known to discard. These tests hold both send sites (the in-loop tier path in
// `attempt` and the re-pack path in `repackStructured`) to that, and hold the
// llama.cpp path byte-identically to where it was.
package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// structuredSwapFake is a llama-swap stand-in that serves a two-seat roster on
// GET /v1/models and records every generation body it is sent, keyed by the
// model the body names. It answers every generation with the same valid
// classify/extract object, because what is under test is the REQUEST.
type structuredSwapFake struct {
	mu     sync.Mutex
	bodies []map[string]any
	models []string
	answer string
}

func (f *structuredSwapFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			data := make([]any, 0, len(f.models))
			for _, m := range f.models {
				data = append(data, map[string]any{"id": m})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body on %s: %v", r.URL.Path, err)
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		_, _ = w.Write(fakeChat{content: f.answer, finishReason: "stop", promptTokens: 40}.marshal())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bodiesFor returns the recorded generation bodies that named model.
func (f *structuredSwapFake) bodiesFor(model string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, b := range f.bodies {
		if m, _ := b["model"].(string); m == model {
			out = append(out, b)
		}
	}
	return out
}

// TestGrammarNeverReachesADeclaredVLLMSeat drives BOTH send sites against one
// declared vLLM seat and one seat that is not declared, and reads the wire.
func TestGrammarNeverReachesADeclaredVLLMSeat(t *testing.T) {
	const vseat = "qwen3.8-27b-vllm"
	const cppseat = "gemma4-e4b"

	fake := &structuredSwapFake{
		models: []string{vseat, cppseat},
		// Serves both call shapes: the classify grammar's two fields and the
		// re-pack schema's one. Extra keys are not what either gate checks.
		answer: `{"label":"animal","confidence":0.92,"title":"a cat"}`,
	}
	srv := fake.server(t)

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = cppseat
	cfg.TriageModel = cppseat
	cfg.EscalationModel = ""
	cfg.MaxRetries = 0
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.VLLMSeats = []string{vseat}
	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg.LedgerPath = ledgerPath
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, led)

	classify := core.Request{
		Task:   core.TaskClassify,
		Input:  "the cat sat on the mat and looked at the dog",
		Params: map[string]any{"labels": []string{"animal", "finance"}},
	}
	schema := json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`)

	// (a) the in-loop tier path and (b) the re-pack path, both on the vLLM seat.
	_, _ = p.RunTier(context.Background(), classify, vseat)
	if _, _, _, _, rerr := p.repackStructured(context.Background(), vseat, schema, "The title is: a cat.", 0); rerr != nil {
		t.Fatalf("repackStructured on the vLLM seat: %v", rerr)
	}

	vBodies := fake.bodiesFor(vseat)
	if len(vBodies) < 2 {
		t.Fatalf("expected at least one tier body and one re-pack body on %s, got %d", vseat, len(vBodies))
	}
	for i, b := range vBodies {
		if _, ok := b["grammar"]; ok {
			t.Errorf("body %d to the declared vLLM seat carries a `grammar` key — the field vLLM ignores (D-129): %v", i, b["grammar"])
		}
		so, ok := b["structured_outputs"].(map[string]any)
		if !ok {
			t.Errorf("body %d to the declared vLLM seat carries no structured_outputs object; got %v", i, b["structured_outputs"])
			continue
		}
		js, ok := so["json"].(map[string]any)
		if !ok || len(js) == 0 {
			t.Errorf("body %d: structured_outputs.json is not a non-empty JSON Schema object; got %v", i, so["json"])
			continue
		}
		if js["type"] != "object" {
			t.Errorf("body %d: structured_outputs.json.type = %v, want \"object\"", i, js["type"])
		}
		if _, ok := js["properties"].(map[string]any); !ok {
			t.Errorf("body %d: structured_outputs.json has no properties map; got %v", i, js["properties"])
		}
	}

	// The converse, and the reason this is a per-seat decision rather than a
	// migration: a seat the box does NOT declare as vLLM is a llama.cpp seat,
	// and llama.cpp serves no `structured_outputs` — it must keep the raw GBNF.
	_, _ = p.RunTier(context.Background(), classify, cppseat)
	cppBodies := fake.bodiesFor(cppseat)
	if len(cppBodies) == 0 {
		t.Fatalf("no body recorded for the undeclared seat %s", cppseat)
	}
	for i, b := range cppBodies {
		if g, _ := b["grammar"].(string); g == "" {
			t.Errorf("body %d to the undeclared seat %s lost its raw GBNF grammar", i, cppseat)
		}
		if _, ok := b["structured_outputs"]; ok {
			t.Errorf("body %d to the undeclared seat %s carries structured_outputs, which llama.cpp does not serve", i, cppseat)
		}
	}
}

// TestLedgerRowCarriesTheRepackAttemptCount pins the telemetry the engine
// route is measured on (register D-129). `repack_ms` alone cannot separate one
// slow re-pack from a loop that re-generated the whole answer two or three
// times, and the whole claim behind sending vLLM `structured_outputs` is that
// the attempt COUNT drops — a figure nothing on the ledger could report.
func TestLedgerRowCarriesTheRepackAttemptCount(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		// Attempt 1 fails validation, attempt 2 satisfies the schema: two
		// seat completions for one re-pack.
		repack: func(n int64) string {
			if n == 1 {
				return `{"wrong":"shape"}`
			}
			return `{"answer":"42"}`
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	ledgerPath := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	cfg := config.Config{
		Endpoint:    srv.URL,
		Model:       "workhorse",
		AgentModel:  agentTestSeat,
		FleetNodeID: "node-t",
		Temperature: 0.1,
		LedgerPath:  ledgerPath,
	}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, led)

	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if wire.RepackAttempts != 2 {
		t.Fatalf("wire repack_attempts = %d, want 2", wire.RepackAttempts)
	}
	if err := led.Close(); err != nil {
		t.Fatalf("ledger.Close: %v", err)
	}

	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var row struct {
		RepackMs       int64 `json:"repack_ms"`
		RepackAttempts int   `json:"repack_attempts"`
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &row); err != nil {
		t.Fatalf("decode the ledger row: %v (%s)", err, lines[len(lines)-1])
	}
	if row.RepackAttempts != 2 {
		t.Errorf("ledger repack_attempts = %d, want 2 — the row cannot say how many completions the re-pack spent", row.RepackAttempts)
	}
	if row.RepackMs <= 0 {
		t.Errorf("ledger repack_ms = %d, want the re-pack wall beside the count", row.RepackMs)
	}
}

// TestIsVLLMSeatResolvesAnAliasThroughTheRoster is the case the fix lives or
// dies on. The Qube's agent seat is BOUND as `agent-pool-3card`, an alias of
// `qwen3.8-27b-vllm-3card`, and it is the CANONICAL id that `vllm_seats`
// lists. A gate that matched the declared roster exactly would have left the
// three-card box — the single largest consumer of the re-pack path — sending
// the grammar field vLLM discards, so the fix would have measured as no change
// at all.
func TestIsVLLMSeatResolvesAnAliasThroughTheRoster(t *testing.T) {
	const canonical = "qwen3.8-27b-vllm-3card"
	const alias = "agent-pool-3card"

	fake := &agentFake{
		rosterIDs:     []string{canonical, "gemma-4-e4b"},
		rosterAliases: map[string][]string{canonical: {alias}, "gemma-4-e4b": {"offload-e4b"}},
	}
	srv := fake.server(t)
	defer srv.Close()

	cfg := config.Config{Endpoint: srv.URL, Model: "workhorse", VLLMSeats: []string{canonical}}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 5*time.Second), nil, nil)

	for _, tc := range []struct {
		name  string
		model string
		want  bool
	}{
		{"the canonical id is declared", canonical, true},
		{"its alias resolves to it", alias, true},
		{"a differently-cased alias is the same seat", "AGENT-POOL-3CARD", true},
		{"a llama.cpp seat's alias does not", "offload-e4b", false},
		{"a name the roster does not serve", "not-a-seat", false},
		{"an empty name", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.isVLLMSeat(context.Background(), tc.model); got != tc.want {
				t.Errorf("isVLLMSeat(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// TestIsVLLMSeatMemoizesAndFailsClosedOnADeadRoster covers the two properties
// that keep this decision off the request path's critical cost and off the
// wrong side of a transient failure: one roster read per name per TTL, and an
// unreadable roster resolving to "keep the grammar" — the behaviour every seat
// had before D-129, so a probe failure can never strip the constraint from a
// llama.cpp seat and turn a working call into a parse error.
func TestIsVLLMSeatMemoizesAndFailsClosedOnADeadRoster(t *testing.T) {
	t.Run("one roster read per name", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs:     []string{"seat-vllm"},
			rosterAliases: map[string][]string{"seat-vllm": {"pool"}},
		}
		srv := fake.server(t)
		defer srv.Close()
		cfg := config.Config{Endpoint: srv.URL, VLLMSeats: []string{"seat-vllm"}}
		p := New(cfg, llamaclient.New(srv.URL, "", "", 5*time.Second), nil, nil)

		var reads int
		countingRoster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				reads++
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
				map[string]any{"id": "seat-vllm", "meta": map[string]any{"llamaswap": map[string]any{"aliases": []string{"pool"}}}},
			}})
		}))
		defer countingRoster.Close()
		p.cfg.Endpoint = countingRoster.URL

		for i := 0; i < 5; i++ {
			if !p.isVLLMSeat(context.Background(), "pool") {
				t.Fatalf("call %d: the alias must resolve to the declared seat", i)
			}
		}
		if reads != 1 {
			t.Errorf("roster reads = %d, want 1 — the answer must be memoized per name", reads)
		}
		// A DECLARED id needs no roster read at all.
		before := reads
		if !p.isVLLMSeat(context.Background(), "seat-vllm") {
			t.Fatal("the declared id must answer true")
		}
		if reads != before {
			t.Errorf("a declared id spent %d roster read(s), want none", reads-before)
		}
	})

	t.Run("a dead roster keeps the grammar", func(t *testing.T) {
		fake := &agentFake{rosterIDs: []string{"seat-vllm"}, rosterStatus: http.StatusInternalServerError}
		srv := fake.server(t)
		defer srv.Close()
		cfg := config.Config{Endpoint: srv.URL, VLLMSeats: []string{"seat-vllm"}}
		p := New(cfg, llamaclient.New(srv.URL, "", "", 5*time.Second), nil, nil)

		if p.isVLLMSeat(context.Background(), "pool") {
			t.Error("an unreadable roster must resolve to false — the pre-D-129 behaviour, never a stripped constraint")
		}
		// The declared id is still answered from config, with no roster involved.
		if !p.isVLLMSeat(context.Background(), "seat-vllm") {
			t.Error("a declared id must not depend on the roster")
		}
	})

	t.Run("a box that declares no vLLM seat spends nothing", func(t *testing.T) {
		var reads int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reads++
			http.NotFound(w, r)
		}))
		defer srv.Close()
		p := New(config.Config{Endpoint: srv.URL}, llamaclient.New(srv.URL, "", "", 5*time.Second), nil, nil)
		if p.isVLLMSeat(context.Background(), "anything") {
			t.Error("a box with an empty vllm_seats declares no vLLM seat")
		}
		if reads != 0 {
			t.Errorf("requests to the endpoint = %d, want 0 — nothing declared means nothing to resolve", reads)
		}
	})
}
