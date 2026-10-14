package backendchain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// fakeSwap is a llama-swap that FAILS THE TEST on any request a cheap probe must never make: a
// completion, /upstream (which loads a model on demand and resets the idle timer), any POST. It serves
// what the default probe is allowed to read - /v1/models, /running, and a loaded seat's own /health.
type fakeSwap struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	roster     map[string][]string // canonical id -> aliases
	running    map[string]string   // canonical id -> llama-swap state
	seatStatus map[string]int      // seat id -> status its own /health answers (default 200)
	// allowCompletion admits POST /v1/chat/completions (and records the body): only the test of the
	// explicit completion option sets it.
	allowCompletion bool
	requests        []string
	completions     []map[string]any
}

func newFakeSwap(t *testing.T, roster map[string][]string) *fakeSwap {
	t.Helper()
	f := &fakeSwap{t: t, roster: roster, running: map[string]string{}, seatStatus: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSwap) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		var rows []string
		for id, aliases := range f.roster {
			a, _ := json.Marshal(aliases)
			rows = append(rows, fmt.Sprintf(`{"id":%q,"object":"model","meta":{"llamaswap":{"aliases":%s}}}`, id, a))
		}
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(rows, ","))
	case r.Method == http.MethodGet && r.URL.Path == "/running":
		var rows []string
		for id, state := range f.running {
			rows = append(rows, fmt.Sprintf(`{"model":%q,"state":%q,"proxy":%q,"cmd":"x","ttl":0}`, id, state, f.srv.URL+"/seat/"+id))
		}
		fmt.Fprintf(w, `{"running":[%s]}`, strings.Join(rows, ","))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/seat/") && strings.HasSuffix(r.URL.Path, "/health"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/seat/"), "/health")
		st := f.seatStatus[id]
		if st == 0 {
			st = http.StatusOK
		}
		w.WriteHeader(st)
	case f.allowCompletion && r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.completions = append(f.completions, body)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	default:
		f.t.Errorf("forbidden request to llama-swap: %s %s - the default probe must load nothing and must never send a completion", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (f *fakeSwap) completionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completions)
}

const servingYAML = `
macros:
  common: "--port ${PORT} --host 127.0.0.1"
models:
  offload-e4b:
    cmd: /bin/llama-server -m e4b.gguf ${common}
  gemma4-e2b:
    cmd: /bin/llama-server -m e2b.gguf ${common}
  gemma4-26b-a4b:
    cmd: /bin/llama-server -m 26b.gguf ${common}
  small-cpp:
    aliases: [cpp-alias]
    cmd: /bin/llama-server -m agent.gguf ${common}
  big-vllm:
    cmd: /seats/big-vllm-cmd.sh
    proxy: http://127.0.0.1:18797
  npu-seat:
    cmd: /opt/rknpu/rkllm-serve.sh --port ${PORT}
  ears:
    cmd: /bin/whisper-server -m w.bin --port ${PORT}
`

// testCfg is a config bound to the fake llama-swap, with the agent lane on the llama.cpp seat and the
// serving config written to a temp file.
func testCfg(t *testing.T, f *fakeSwap) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoint = f.srv.URL
	cfg.AgentModel = "small-cpp"
	cfg.VLLMSeats = []string{"big-vllm"}
	p := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(p, []byte(servingYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ServingConfigPath = p
	return cfg
}

func fullRoster() map[string][]string {
	return map[string][]string{
		"offload-e4b": nil, "gemma4-e2b": nil, "gemma4-26b-a4b": nil,
		"small-cpp": {"cpp-alias"}, "big-vllm": nil, "npu-seat": nil, "ears": nil,
	}
}

// withTier installs a tier lookup that declares big-vllm as the vLLM agent seat with small-cpp as its
// fallback, for the agent seat small-cpp or big-vllm.
func withTier(t *testing.T) {
	t.Helper()
	restore := SetTierLookup(func(_ config.Config, agent string) (TierSeat, bool) {
		if agent == "small-cpp" || agent == "big-vllm" {
			return TierSeat{ID: "big-vllm", Fallback: "small-cpp"}, true
		}
		return TierSeat{}, false
	})
	t.Cleanup(restore)
}

func laneOf(t *testing.T, r Report, name string) Entry {
	t.Helper()
	for _, e := range r.Lanes {
		if e.Lane == name {
			return e
		}
	}
	t.Fatalf("no %q lane in %+v", name, r.Lanes)
	return Entry{}
}

func seatOf(t *testing.T, r Report, name string) Entry {
	t.Helper()
	for _, e := range r.Seats {
		if e.Seat == name {
			return e
		}
	}
	t.Fatalf("no seat %q in %+v", name, r.Seats)
	return Entry{}
}

// TestCollectDefaultModeLoadsNothing: the fake llama-swap fails the test on a completion, an /upstream
// path or any POST, so a green run is the proof that no code path of the default probe issues one. The
// vLLM seat is loaded and ready, so its own health endpoint is asked (at the seat's address); every
// other seat is idle and is not touched at all.
func TestCollectDefaultModeLoadsNothing(t *testing.T) {
	withTier(t)
	f := newFakeSwap(t, fullRoster())
	f.running["big-vllm"] = "ready"
	cfg := testCfg(t, f)
	r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})

	agent := laneOf(t, r, "agent")
	if agent.ActiveBackend != EngineVLLM || agent.Seat != "big-vllm" || agent.BackendProbe != ProbeEndpoint {
		t.Errorf("agent lane = %+v, want the loaded vLLM seat probed at its endpoint", agent)
	}
	if strings.Join(agent.BackendChain, ">") != "vllm>llama.cpp" {
		t.Errorf("agent chain = %v", agent.BackendChain)
	}
	if !strings.Contains(agent.BackendReason, "the lane is bound to small-cpp") {
		t.Errorf("the lane is bound to the fallback seat and the reason must say so: %q", agent.BackendReason)
	}
	work := laneOf(t, r, "workhorse")
	if work.ActiveBackend != EngineLlamaCpp || work.BackendProbe != ProbeProcess || !strings.Contains(work.BackendReason, "not loaded") {
		t.Errorf("an idle llama.cpp lane must be answered from llama-swap's state, never by loading it: %+v", work)
	}
	for _, req := range f.requests {
		if strings.Contains(req, "completion") || strings.Contains(req, "upstream") || strings.HasPrefix(req, "POST") {
			t.Errorf("request %q must not be made by the default probe", req)
		}
	}
	if n := f.completionCount(); n != 0 {
		t.Errorf("%d completions were sent without the flag", n)
	}
}

// TestCollectFailingFirstBackendFallsToTheSecond: the vLLM seat is loaded but its own health endpoint
// answers 503, so the chain falls to the llama.cpp seat and the reason names the first one's failure.
func TestCollectFailingFirstBackendFallsToTheSecond(t *testing.T) {
	withTier(t)
	f := newFakeSwap(t, fullRoster())
	f.running["big-vllm"] = "ready"
	f.seatStatus["big-vllm"] = http.StatusServiceUnavailable
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client()})
	agent := laneOf(t, r, "agent")
	if agent.ActiveBackend != EngineLlamaCpp || agent.Seat != "small-cpp" {
		t.Fatalf("agent lane = %+v, want the llama.cpp fallback", agent)
	}
	for _, want := range []string{"vllm seat big-vllm unavailable", "503", "fell to llama.cpp seat small-cpp"} {
		if !strings.Contains(agent.BackendReason, want) {
			t.Errorf("reason %q must contain %q", agent.BackendReason, want)
		}
	}
}

// TestCollectFirstBackendNotInTheRoster: the tier prefers a vLLM seat this box does not serve (its
// venv is not installed, so the seat is not rendered): the lane is answered by its llama.cpp fallback,
// and the report says why.
func TestCollectFirstBackendNotInTheRoster(t *testing.T) {
	withTier(t)
	roster := fullRoster()
	delete(roster, "big-vllm")
	f := newFakeSwap(t, roster)
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client()})
	agent := laneOf(t, r, "agent")
	if agent.ActiveBackend != EngineLlamaCpp || agent.Seat != "small-cpp" {
		t.Fatalf("agent lane = %+v", agent)
	}
	if !strings.Contains(agent.BackendReason, "vllm seat big-vllm unavailable: not in the llama-swap roster") {
		t.Errorf("reason = %q", agent.BackendReason)
	}
	if strings.Contains(agent.BackendReason, "the lane is bound to") {
		t.Errorf("the answering seat IS the bound seat, no binding note is due: %q", agent.BackendReason)
	}
}

// TestCollectLlamaSwapDown: nothing answers, so every lane and seat is none, and the reason carries no
// address (it is published to every node that reads /fleet/health).
func TestCollectLlamaSwapDown(t *testing.T) {
	withTier(t)
	f := newFakeSwap(t, fullRoster())
	cfg := testCfg(t, f)
	f.srv.Close()
	r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})
	if len(r.Lanes) == 0 {
		t.Fatal("a configured lane must still be reported when llama-swap is down")
	}
	for _, e := range append(append([]Entry{}, r.Lanes...), r.Seats...) {
		if e.ActiveBackend != EngineNone {
			t.Errorf("%+v: nothing can serve while llama-swap is down", e)
		}
		if !strings.Contains(e.BackendReason, "llama-swap is unreachable") {
			t.Errorf("reason = %q", e.BackendReason)
		}
		for _, leak := range []string{"127.0.0.1", "http://", f.srv.URL} {
			if strings.Contains(e.BackendReason, leak) {
				t.Errorf("reason %q leaks %q", e.BackendReason, leak)
			}
		}
	}
}

// TestCollectSeatRowsNameTheEngineFromTheServingConfig: one row per seat; the engine comes from the
// declaration (vllm_seats) or the command the serving config runs, never from a guess.
func TestCollectSeatRowsNameTheEngineFromTheServingConfig(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client()})
	want := map[string]string{
		"offload-e4b": EngineLlamaCpp, "small-cpp": EngineLlamaCpp, "big-vllm": EngineVLLM,
		"npu-seat": EngineRKLLM, "ears": EngineUnknown,
	}
	for seat, kind := range want {
		e := seatOf(t, r, seat)
		if e.ActiveBackend != kind || strings.Join(e.BackendChain, ",") != kind || e.Lane != "" {
			t.Errorf("seat %s = %+v, want engine %s", seat, e, kind)
		}
	}
}

// TestCollectSeatMissingFromRoster: a seat the serving config lists and llama-swap does not serve is none.
func TestCollectSeatMissingFromRoster(t *testing.T) {
	roster := fullRoster()
	delete(roster, "npu-seat")
	f := newFakeSwap(t, roster)
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client()})
	e := seatOf(t, r, "npu-seat")
	if e.ActiveBackend != EngineNone || !strings.Contains(e.BackendReason, "not in the llama-swap roster") {
		t.Errorf("npu-seat = %+v", e)
	}
}

// TestCompletionProbeOnlyWithTheFlag: with the explicit option the probe sends ONE 1-token completion
// per answering seat and reports the completion level; the cheap probe of the same state sent none.
func TestCompletionProbeOnlyWithTheFlag(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	cfg := testCfg(t, f)
	Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})
	if n := f.completionCount(); n != 0 {
		t.Fatalf("%d completions without the flag", n)
	}
	f.mu.Lock()
	f.allowCompletion = true
	f.mu.Unlock()
	r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	if f.completionCount() == 0 {
		t.Fatal("the flag must run a completion probe")
	}
	seats := map[string]int{}
	f.mu.Lock()
	for _, c := range f.completions {
		if mt, _ := c["max_tokens"].(float64); mt != 1 {
			t.Errorf("a probe completion must ask for 1 token, got %v", c["max_tokens"])
		}
		m, _ := c["model"].(string)
		seats[m]++
	}
	f.mu.Unlock()
	for seat, n := range seats {
		if n != 1 {
			t.Errorf("seat %s was completed %d times: the probe runs once per seat per collection", seat, n)
		}
	}
	e := seatOf(t, r, "small-cpp")
	if e.BackendProbe != ProbeCompletion || e.ActiveBackend != EngineLlamaCpp {
		t.Errorf("small-cpp = %+v, want the completion level", e)
	}
}

// TestCompletionProbeNeverEvictsAVLLMSeat: a completion against an idle seat would load it, and while a
// vLLM seat holds the cards that can evict it (internal/seatguard), so the probe is skipped and says so.
func TestCompletionProbeNeverEvictsAVLLMSeat(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["big-vllm"] = "ready"
	f.allowCompletion = true
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	f.mu.Lock()
	for _, c := range f.completions {
		if m, _ := c["model"].(string); m != "big-vllm" {
			t.Errorf("a completion was sent to %v while a vLLM seat held the cards", m)
		}
	}
	f.mu.Unlock()
	e := seatOf(t, r, "small-cpp")
	if e.ActiveBackend != EngineLlamaCpp || e.BackendProbe == ProbeCompletion || !strings.Contains(e.BackendReason, "big-vllm") || !strings.Contains(e.BackendReason, "would load") {
		t.Errorf("small-cpp = %+v, want a skipped completion that names the vLLM seat", e)
	}
}

// TestPlanLanes: one lane per configured model route; the agent lane carries the tier's chain.
func TestPlanLanes(t *testing.T) {
	cfg := config.Default()
	cfg.AgentModel = "small-cpp"
	lanes := PlanLanes(cfg)
	names := map[string]Lane{}
	for _, l := range lanes {
		names[l.Name] = l
	}
	for _, n := range []string{"workhorse", "agent", "triage", "escalation", "reasoning"} {
		if _, ok := names[n]; !ok {
			t.Errorf("lane %s missing from %v", n, lanes)
		}
	}
	if _, ok := names["vision"]; ok {
		t.Errorf("an unbound vision lane must not be reported: %+v", names["vision"])
	}
	if got := names["agent"].Chain; len(got) != 1 || got[0].Seat != "small-cpp" || !got[0].Bound {
		t.Errorf("without a tier the agent lane is its one bound seat: %+v", got)
	}
	withTier(t)
	got := PlanLanes(cfg)
	var agent Lane
	for _, l := range got {
		if l.Name == "agent" {
			agent = l
		}
	}
	if len(agent.Chain) != 2 || agent.Chain[0].Seat != "big-vllm" || agent.Chain[0].Engine != EngineVLLM || agent.Chain[0].Bound ||
		agent.Chain[1].Seat != "small-cpp" || !agent.Chain[1].Bound {
		t.Errorf("tier chain = %+v, want the preferred vLLM seat then the bound fallback", agent.Chain)
	}
}
