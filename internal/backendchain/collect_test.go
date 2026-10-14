package backendchain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatload"
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
	// runningStatus, when set, is what /running answers instead of the list (a llama-swap whose running
	// list cannot be read).
	runningStatus int
	// failRunningReads makes the first N reads of /running answer 500 and every later one the list: a
	// llama-swap whose running list could not be read at the opening of the report and can be read after.
	failRunningReads   int
	failedRunningReads int
	// dropRunning aborts every /running request with no answer at all, so the client's error is a transport
	// failure whose Go text carries the request URL (a status code's text does not).
	dropRunning bool
	// allowCompletion admits POST /v1/chat/completions (and records the body): only the test of the
	// explicit completion option sets it.
	allowCompletion bool
	// loadOnCompletion makes a completion LOAD its seat, the way llama-swap does: the model is listed as
	// ready by every later /running read. Without it the fake's running list never changes. The load
	// happens before the completion is answered, so it also happens when completionStatus then fails it
	// (llama-swap loads the model and the generation fails afterwards).
	loadOnCompletion bool
	// evictOnLoad makes a completion that loads a seat unload every other seat, as llama-swap's swap
	// groups do.
	evictOnLoad bool
	// completionStatus, when set, is what a completion answers instead of a result (a seat that fails).
	completionStatus int
	// chatless lists the seats that have no chat route (a speech-to-text or an embedding seat): a
	// completion for one answers 404 and loads nothing, as llama-swap's passthrough does for them.
	chatless map[string]bool
	// seatPath is the path a seat answers as its health endpoint (default /health); any other path under
	// the seat is a plain 404, so a probe that asks the wrong path reads a failing seat.
	seatPath map[string]string
	// proxyOf overrides the address /running reports for a seat (default: this fake's own /seat/<id>).
	proxyOf map[string]string
	// onRunningRead runs on every successful /running read, before the list is built, with the count of
	// reads so far (1 is the report's own opening read): a test uses it to change what is loaded between
	// two reads, the way another client of llama-swap does.
	onRunningRead func(read int)
	runningReads  int
	requests      []string
	completions   []map[string]any
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
	case r.Method == http.MethodGet && r.URL.Path == "/running" && f.dropRunning:
		panic(http.ErrAbortHandler) // net/http closes the connection without an answer and logs nothing
	case r.Method == http.MethodGet && r.URL.Path == "/running" && f.runningStatus != 0:
		w.WriteHeader(f.runningStatus)
	case r.Method == http.MethodGet && r.URL.Path == "/running" && f.failedRunningReads < f.failRunningReads:
		f.failedRunningReads++
		w.WriteHeader(http.StatusInternalServerError)
	case r.Method == http.MethodGet && r.URL.Path == "/running":
		f.runningReads++
		if f.onRunningRead != nil {
			f.onRunningRead(f.runningReads)
		}
		var rows []string
		for id, state := range f.running {
			proxy := f.srv.URL + "/seat/" + id
			if p, ok := f.proxyOf[id]; ok {
				proxy = p
			}
			rows = append(rows, fmt.Sprintf(`{"model":%q,"state":%q,"proxy":%q,"cmd":"x","ttl":0}`, id, state, proxy))
		}
		fmt.Fprintf(w, `{"running":[%s]}`, strings.Join(rows, ","))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/seat/"):
		id, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/seat/"), "/")
		want := "/health"
		if p, ok := f.seatPath[id]; ok {
			want = p
		}
		if "/"+rest != want {
			w.WriteHeader(http.StatusNotFound)
			break
		}
		st := f.seatStatus[id]
		if st == 0 {
			st = http.StatusOK
		}
		w.WriteHeader(st)
	case f.allowCompletion && r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.completions = append(f.completions, body)
		if m, _ := body["model"].(string); f.chatless[f.canonicalLocked(m)] {
			w.WriteHeader(http.StatusNotFound)
			break
		}
		if m, _ := body["model"].(string); m != "" && f.loadOnCompletion {
			id := f.canonicalLocked(m)
			if _, already := f.running[id]; !already && f.evictOnLoad {
				clear(f.running)
			}
			f.running[id] = "ready"
		}
		if f.completionStatus != 0 {
			w.WriteHeader(f.completionStatus)
			break
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	default:
		f.t.Errorf("forbidden request to llama-swap: %s %s - the default probe must load nothing and must never send a completion", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// canonicalLocked maps a model name (an id or an alias) to the id llama-swap lists it under. The caller
// holds f.mu.
func (f *fakeSwap) canonicalLocked(name string) string {
	for id, aliases := range f.roster {
		if id == name {
			return id
		}
		for _, a := range aliases {
			if a == name {
				return id
			}
		}
	}
	return name
}

// completionModels is the model of every completion the fake served, in order.
func (f *fakeSwap) completionModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.completions {
		m, _ := c["model"].(string)
		out = append(out, m)
	}
	return out
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
  embedder:
    cmd: /bin/llama-server -m emb.gguf --embeddings --pooling mean ${common}
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
		"small-cpp": {"cpp-alias"}, "big-vllm": nil, "npu-seat": nil, "ears": nil, "embedder": nil,
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

	// The agent lane is BOUND to the llama.cpp seat small-cpp, so that engine is what answers for it, even
	// though the preferred vLLM seat is loaded and healthy: the lane routes to its bound seat.
	agent := laneOf(t, r, "agent")
	if agent.ActiveBackend != EngineLlamaCpp || agent.Seat != "small-cpp" || agent.BackendProbe != ProbeProcess {
		t.Errorf("agent lane = %+v, want the bound llama.cpp seat, not the loaded vLLM seat it is not bound to", agent)
	}
	if strings.Join(agent.BackendChain, ">") != "vllm>llama.cpp" {
		t.Errorf("agent chain = %v", agent.BackendChain)
	}
	for _, want := range []string{"the preferred vllm seat big-vllm is available", "does not answer for this lane", "bound to small-cpp"} {
		if !strings.Contains(agent.BackendReason, want) {
			t.Errorf("the lane is bound to the fallback seat and the reason must say so (%q): %q", want, agent.BackendReason)
		}
	}
	// The vLLM seat is still reported on its own row, probed at its endpoint.
	if v := seatOf(t, r, "big-vllm"); v.ActiveBackend != EngineVLLM || v.BackendProbe != ProbeEndpoint {
		t.Errorf("big-vllm seat = %+v, want the loaded vLLM seat probed at its endpoint", v)
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
// to the seat that is running and reports the completion level; the cheap probe of the same state sent
// none. (A seat that is not running is only completed on an idle box, and then once: see
// TestCompletionProbeOnAnIdleBoxLoadsAtMostOneSeat.)
func TestCompletionProbeOnlyWithTheFlag(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["small-cpp"] = "ready"
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

// TestCompletionProbeOnAnIdleBoxLoadsAtMostOneSeat: with nothing running, a completion to an idle seat
// loads it, and every later seat is then a load over a running seat. So the report sends ONE completion
// in all (to the first seat it reaches), and every other row says it was not probed and over which seat the
// load would have been. The fake loads a seat on its completion, as llama-swap does.
func TestCompletionProbeOnAnIdleBoxLoadsAtMostOneSeat(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.allowCompletion = true
	f.loadOnCompletion = true
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})

	order := f.completionModels()
	if len(order) != 1 {
		t.Fatalf("completions = %v, want exactly one: each further one would load a seat over a running one", order)
	}
	first := order[0]
	if first != "offload-e4b" {
		t.Errorf("the completion went to %s, want the first seat of the report (the workhorse lane's seat)", first)
	}
	e := seatOf(t, r, first)
	if e.BackendProbe != ProbeCompletion || !strings.Contains(e.BackendReason, "a 1-token completion loaded it and answered") {
		t.Errorf("%s = %+v, want the completion that loaded it named", first, e)
	}
	checked := 0
	for _, other := range r.Seats {
		if other.Seat == first || other.ActiveBackend == EngineNone || other.BackendProbe == ProbeCompletion || strings.Contains(other.BackendReason, "not a chat seat") {
			continue
		}
		checked++
		if !strings.Contains(other.BackendReason, "not probed: would load over llama.cpp seat "+first) {
			t.Errorf("%s = %+v, want the not-probed clause naming the seat the load would be over", other.Seat, other)
		}
	}
	if checked == 0 {
		t.Error("no other seat row was checked: the test proves nothing")
	}
}

// TestCompletionProbeSendsNoSecondLoadAfterAFailedOne: the first completion on an idle box fails and
// loads nothing, so nothing is running afterwards and the "something is running" test would let the next
// seat through. The report still sends only one completion that can load a seat.
func TestCompletionProbeSendsNoSecondLoadAfterAFailedOne(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.allowCompletion = true
	f.completionStatus = http.StatusInternalServerError
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	if got := f.completionModels(); len(got) != 1 {
		t.Fatalf("completions = %v, want one: a failed load is still the one this run was allowed", got)
	}
	skipped := 0
	for _, e := range r.Seats {
		if strings.Contains(e.BackendReason, "not probed: this run already sent one completion that could load a seat") {
			skipped++
		}
	}
	if skipped == 0 {
		t.Errorf("no seat row says why it was not probed: %+v", r.Seats)
	}
}

// TestCompletionProbeDecidesOnAFreshRunningList: a vLLM seat that another client loaded after the report
// began is seen by the decision to complete, which reads /running again instead of trusting the opening
// read, so no completion is sent to a seat that would evict it.
func TestCompletionProbeDecidesOnAFreshRunningList(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.allowCompletion = true
	f.onRunningRead = func(read int) {
		if read >= 2 {
			f.running["big-vllm"] = "ready"
		}
	}
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	for _, m := range f.completionModels() {
		if m != "big-vllm" {
			t.Errorf("a completion was sent to %s although a vLLM seat was loaded before it was decided", m)
		}
	}
	if e := seatOf(t, r, "offload-e4b"); e.BackendProbe == ProbeCompletion || !strings.Contains(e.BackendReason, "big-vllm") {
		t.Errorf("offload-e4b = %+v, want a skipped completion that names the vLLM seat", e)
	}
}

// TestCompletionProbeSeesWhatItsOwnCompletionLoaded: the next seat is decided on a fresh reading of the
// running list, so it is refused because of the seat the completion loaded (named in its row), not merely
// because one load was already spent.
func TestCompletionProbeSeesWhatItsOwnCompletionLoaded(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.allowCompletion = true
	f.loadOnCompletion = true
	s := newState(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	first := s.Probe(t.Context(), Candidate{Seat: "big-vllm", Engine: EngineVLLM})
	second := s.Probe(t.Context(), Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp})
	if first.Probe != ProbeCompletion || !first.OK {
		t.Fatalf("the first completion must run on an idle box: %+v", first)
	}
	if got := f.completionModels(); len(got) != 1 || got[0] != "big-vllm" {
		t.Errorf("completions = %v, want only the first seat", got)
	}
	if second.Probe == ProbeCompletion || !strings.Contains(second.Detail, "not probed: would load over vllm seat big-vllm") {
		t.Errorf("small-cpp = %+v, want a refusal that names the vLLM seat the completion loaded", second)
	}
}

// TestCompletionProbeSeesWhatAFailedCompletionLoaded: llama-swap loads the model and the generation fails
// afterwards, so a failed completion leaves a running seat too: the next seat is refused over the seat the
// failed completion loaded.
func TestCompletionProbeSeesWhatAFailedCompletionLoaded(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.allowCompletion = true
	f.loadOnCompletion = true
	f.completionStatus = http.StatusInternalServerError
	s := newState(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	first := s.Probe(t.Context(), Candidate{Seat: "big-vllm", Engine: EngineVLLM})
	second := s.Probe(t.Context(), Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp})
	if first.Probe != ProbeCompletion || first.OK || !strings.Contains(first.Detail, "the 1-token completion failed") {
		t.Fatalf("the first completion must run and fail: %+v", first)
	}
	if got := f.completionModels(); len(got) != 1 {
		t.Errorf("completions = %v, want only the failed one", got)
	}
	if second.Probe == ProbeCompletion || !strings.Contains(second.Detail, "not probed: would load over vllm seat big-vllm") {
		t.Errorf("small-cpp = %+v, want a refusal that names the vLLM seat the failed completion loaded", second)
	}
}

// TestCompletionReasonWhenTheRunningListBecameReadable: the opening read of /running failed, so the cheap
// probe says the seat's load state is unknown; the decision to complete reads it again and it answers. The
// row must not keep the "unknown" clause the second reading outgrew, here beside the skip that the readable
// list produced (a vLLM seat holds the cards). Only the first seat decided after a failed opening read is
// in this position: its successful read clears the error for every later seat.
func TestCompletionReasonWhenTheRunningListBecameReadable(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["big-vllm"] = "ready"
	f.failRunningReads = 1
	f.allowCompletion = true
	s := newState(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	if s.runErr == nil {
		t.Fatal("the opening read of /running was meant to fail: the test would prove nothing")
	}
	v := s.Probe(t.Context(), Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp})
	if v.Probe == ProbeCompletion || !strings.Contains(v.Detail, "not probed: would load over vllm seat big-vllm") {
		t.Fatalf("small-cpp = %+v, want a skipped completion that names the vLLM seat the second reading found", v)
	}
	for _, stale := range []string{"could not be read", "unknown", "not asked"} {
		if strings.Contains(v.Detail, stale) {
			t.Errorf("reason %q carries %q although the running list was read when the completion was decided", v.Detail, stale)
		}
	}
	if n := f.completionCount(); n != 0 {
		t.Errorf("%d completions were sent while a vLLM seat held the cards", n)
	}
}

// TestCompletionReasonForASeatLoadedSinceTheReportBegan: a seat that another client loads after the
// opening read, before its own probe is decided, is completed without loading anything, and its row must
// not carry the cheap probe's "not asked" clause from the reading it has outgrown.
func TestCompletionReasonForASeatLoadedSinceTheReportBegan(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.allowCompletion = true
	f.onRunningRead = func(read int) {
		if read >= 2 {
			f.running["offload-e4b"] = "ready"
		}
	}
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	e := seatOf(t, r, "offload-e4b")
	if e.BackendProbe != ProbeCompletion {
		t.Fatalf("offload-e4b was running when its completion was decided, so the completion runs: %+v", e)
	}
	if strings.Contains(e.BackendReason, "not asked") || strings.Contains(e.BackendReason, "asking would") || !strings.Contains(e.BackendReason, "loaded since the report began") {
		t.Errorf("reason %q must say the seat was loaded since the report began, not that it was not asked", e.BackendReason)
	}
}

// TestCompletionProbeNeverLoadsASecondVLLMSeat: loading one vLLM seat evicts another, so an idle vLLM
// seat is refused too while a different vLLM seat is loaded (the target being vLLM is no exemption).
func TestCompletionProbeNeverLoadsASecondVLLMSeat(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.roster["other-vllm"] = nil
	f.running["big-vllm"] = "ready"
	f.allowCompletion = true
	cfg := testCfg(t, f)
	cfg.VLLMSeats = []string{"big-vllm", "other-vllm"}
	yaml := servingYAML + "  other-vllm:\n    cmd: /seats/other-vllm-cmd.sh\n    proxy: http://127.0.0.1:18798\n"
	if err := os.WriteFile(cfg.ServingConfigPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	for _, m := range f.completionModels() {
		if m != "big-vllm" {
			t.Errorf("a completion was sent to %s while the vLLM seat big-vllm held the cards", m)
		}
	}
	e := seatOf(t, r, "other-vllm")
	if e.BackendProbe == ProbeCompletion || !strings.Contains(e.BackendReason, "big-vllm") || !strings.Contains(e.BackendReason, "would load") {
		t.Errorf("other-vllm = %+v, want a skipped completion that names the loaded vLLM seat", e)
	}
	if v := seatOf(t, r, "big-vllm"); v.BackendProbe != ProbeCompletion {
		t.Errorf("big-vllm is already loaded, so completing it loads nothing and must run: %+v", v)
	}
}

// TestCompletionReasonDoesNotContradictItself: a row that ran a completion must not also say it was not
// asked. A loaded seat keeps its endpoint clause; an idle one the completion loaded (on an idle box, the
// only place one is) says so, and TestCompletionProbeOnAnIdleBoxLoadsAtMostOneSeat checks that row.
func TestCompletionReasonDoesNotContradictItself(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.allowCompletion = true
	f.running["small-cpp"] = "ready"
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	for _, e := range append(append([]Entry{}, r.Lanes...), r.Seats...) {
		if e.BackendProbe == ProbeCompletion && (strings.Contains(e.BackendReason, "not asked") || strings.Contains(e.BackendReason, "asking would")) {
			t.Errorf("%s %s ran a completion and says it was not asked: %q", e.Lane, e.Seat, e.BackendReason)
		}
	}
	loaded := seatOf(t, r, "small-cpp")
	if loaded.BackendProbe != ProbeCompletion || !strings.Contains(loaded.BackendReason, "its health endpoint answered 200") ||
		!strings.Contains(loaded.BackendReason, "a 1-token completion answered") || strings.Contains(loaded.BackendReason, "loaded it") {
		t.Errorf("loaded seat = %+v, want its endpoint clause and the completion", loaded)
	}
}

// TestCollectAsksTheEntrysOwnHealthPath: a serving-config entry names its health path with checkEndpoint
// (with or without the leading slash); the loaded seat is asked THAT path. The fake answers only that
// path and 404s /health, so a probe that asks /health reads a failing seat.
func TestCollectAsksTheEntrysOwnHealthPath(t *testing.T) {
	for _, declared := range []string{"/v1/models", "v1/models"} {
		t.Run(declared, func(t *testing.T) {
			f := newFakeSwap(t, fullRoster())
			f.running["small-cpp"] = "ready"
			f.seatPath = map[string]string{"small-cpp": "/v1/models"}
			cfg := testCfg(t, f)
			yaml := strings.Replace(servingYAML, "  small-cpp:\n", "  small-cpp:\n    checkEndpoint: "+declared+"\n", 1)
			if err := os.WriteFile(cfg.ServingConfigPath, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})
			e := seatOf(t, r, "small-cpp")
			if e.ActiveBackend != EngineLlamaCpp || e.BackendProbe != ProbeEndpoint || !strings.Contains(e.BackendReason, "answered 200") {
				t.Errorf("small-cpp = %+v, want ok at its own health path", e)
			}
			var asked []string
			for _, req := range f.requests {
				if strings.HasPrefix(req, "GET /seat/small-cpp") {
					asked = append(asked, req)
				}
			}
			if len(asked) != 1 || asked[0] != "GET /seat/small-cpp/v1/models" {
				t.Errorf("the seat was asked %v, want only its checkEndpoint path", asked)
			}
		})
	}
}

// TestCollectLaneActiveBackendIsTheBoundSeatsEngine: active_backend is the engine of the seat the lane is
// bound to, whatever else of the chain is up. A preferred vLLM seat that is healthy while the lane is
// bound to the llama.cpp fallback is NOT what answers; a bound seat that fails is none, and the report
// says the other seat is available but not bound.
func TestCollectLaneActiveBackendIsTheBoundSeatsEngine(t *testing.T) {
	withTier(t)
	cases := []struct {
		name       string
		bound      string
		vllmStatus int
		wantActive string
		wantSeat   string
		inReason   []string
	}{
		{"bound to the fallback while vLLM is healthy", "small-cpp", 200, EngineLlamaCpp, "small-cpp",
			[]string{"the preferred vllm seat big-vllm is available", "bound to small-cpp"}},
		{"bound to the vLLM seat and it is healthy", "big-vllm", 200, EngineVLLM, "big-vllm", nil},
		{"bound to the vLLM seat and it fails: none, the fallback is not what the lane calls", "big-vllm", 503, EngineNone, "big-vllm",
			[]string{"vllm seat big-vllm unavailable", "503", "llama.cpp seat small-cpp is available", "bound to big-vllm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSwap(t, fullRoster())
			f.running["big-vllm"] = "ready"
			f.seatStatus["big-vllm"] = tc.vllmStatus
			cfg := testCfg(t, f)
			cfg.AgentModel = tc.bound
			agent := laneOf(t, Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()}), "agent")
			if agent.ActiveBackend != tc.wantActive || agent.Seat != tc.wantSeat {
				t.Errorf("agent lane = %+v, want %s on seat %s", agent, tc.wantActive, tc.wantSeat)
			}
			for _, want := range tc.inReason {
				if !strings.Contains(agent.BackendReason, want) {
					t.Errorf("reason %q lacks %q", agent.BackendReason, want)
				}
			}
		})
	}
}

// TestBackendReasonsCarryNoPathOrURL: a reason is published to every node that reads /fleet/health, so
// it names seats and states and carries no address, path, URL, command or config location, whatever
// failed. The scenarios exercise every failure clause of the collector (the llama-swap that is down, a
// running list that cannot be read, whether it answers a status or fails at the transport, a seat whose
// health endpoint answers 503 or does not answer, a completion that fails, a completion skipped because
// a vLLM seat holds the cards, a serving config that is missing or unparseable, no endpoint, no client).
// The scenarios for the unreadable running list (by status, by transport and as a raw error), the skipped
// completion, the seat that does not answer and the failed completion also assert that their clause
// appeared, so each of those fails instead of passing for nothing if it never reaches it. The scenarios
// for llama-swap down, a seat that answers 503, the serving config and no endpoint or client assert only
// that some row exists and that no row carries an address, path or route.
func TestBackendReasonsCarryNoPathOrURL(t *testing.T) {
	ipv4 := regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	driveOrUNC := regexp.MustCompile(`[A-Za-z]:[\\/]|\\\\`)
	// the routes of llama-swap and of the completion API: a reason that names one has copied a URL or an
	// error's text, and the Go text of a failed request carries the path whether or not it carries a host.
	routes := []string{"/running", "/v1/models", "/health", "/upstream", "/v1/chat/completions"}
	checkText := func(t *testing.T, who, reason string, extra ...string) {
		t.Helper()
		forbidden := append(append([]string{"://", "127.0.0.1", "localhost", "18797", "18798", "/bin/", "/seats/", ".sh", "llama-server", "\\", "--port"}, routes...), extra...)
		for _, bad := range forbidden {
			if bad != "" && strings.Contains(reason, bad) {
				t.Errorf("%s: reason %q carries %q", who, reason, bad)
			}
		}
		if ipv4.MatchString(reason) || driveOrUNC.MatchString(reason) {
			t.Errorf("%s: reason %q carries an address or a drive path", who, reason)
		}
	}
	check := func(t *testing.T, rep Report, extra ...string) {
		t.Helper()
		all := append(append([]Entry{}, rep.Lanes...), rep.Seats...)
		if len(all) == 0 {
			t.Fatal("no rows: the scenario proves nothing")
		}
		for _, e := range all {
			checkText(t, e.Lane+" "+e.Seat, e.BackendReason, extra...)
		}
	}
	// anyRowSays fails the scenario unless some row carries every one of the clauses: the proof that the
	// scenario reached the clause it is there to check.
	anyRowSays := func(t *testing.T, rep Report, clauses ...string) {
		t.Helper()
		for _, e := range append(append([]Entry{}, rep.Lanes...), rep.Seats...) {
			ok := true
			for _, c := range clauses {
				ok = ok && strings.Contains(e.BackendReason, c)
			}
			if ok {
				return
			}
		}
		t.Errorf("no row carries %q: the scenario never reached the clause it is for", clauses)
	}
	hostOf := func(f *fakeSwap) string { return strings.TrimPrefix(f.srv.URL, "http://") }

	t.Run("llama-swap is down", func(t *testing.T) {
		withTier(t)
		f := newFakeSwap(t, fullRoster())
		cfg := testCfg(t, f)
		f.srv.Close()
		check(t, Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()}), hostOf(f), cfg.ServingConfigPath, filepath.Dir(cfg.ServingConfigPath))
	})
	t.Run("the running list cannot be read, completion probe on", func(t *testing.T) {
		f := newFakeSwap(t, fullRoster())
		f.runningStatus = http.StatusInternalServerError
		f.allowCompletion = true
		cfg := testCfg(t, f)
		rep := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeCompletion: true})
		check(t, rep, hostOf(f), cfg.ServingConfigPath)
		anyRowSays(t, rep, "running list could not be read (", "completion probe skipped", "cannot be ruled out")
	})
	t.Run("the running list fails at the transport, with or without the completion probe", func(t *testing.T) {
		f := newFakeSwap(t, fullRoster())
		f.dropRunning = true
		f.allowCompletion = true
		cfg := testCfg(t, f)
		// The raw error of this failure carries the host and the route, so a reason that copied it would leak.
		_, raw := seatload.Occupants(t.Context(), f.srv.Client(), f.srv.URL)
		if raw == nil || !strings.Contains(raw.Error(), hostOf(f)) || !strings.Contains(raw.Error(), "/running") {
			t.Fatalf("the scenario's raw error is %v: it must carry the host and the route, or it proves nothing", raw)
		}
		for _, probe := range []bool{false, true} {
			rep := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeCompletion: probe})
			check(t, rep, hostOf(f), cfg.ServingConfigPath)
			anyRowSays(t, rep, "running list could not be read (")
			if probe {
				anyRowSays(t, rep, "completion probe skipped", "cannot be ruled out")
			}
		}
		if n := f.completionCount(); n != 0 {
			t.Errorf("%d completions were sent with the running list unreadable", n)
		}
	})
	t.Run("a raw running-list error that carries a path and a URL", func(t *testing.T) {
		f := newFakeSwap(t, fullRoster())
		cfg := testCfg(t, f)
		s := newState(t.Context(), cfg, Options{HTTP: f.srv.Client()})
		s.start, s.running = nil, nil
		s.startErr = fmt.Errorf("llama-swap /running: %w", &url.Error{Op: "Get", URL: "http://llama-swap.example.invalid:9/running", Err: errors.New("open /srv/seats/serving.yaml: permission denied")})
		v := s.Probe(t.Context(), Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp})
		if !strings.Contains(v.Detail, "running list could not be read (") {
			t.Fatalf("the unreadable running list must be reported: %+v", v)
		}
		checkText(t, "small-cpp", v.Detail, "example.invalid", "serving.yaml", "/srv/", "permission denied")
	})
	t.Run("a completion is skipped because a vLLM seat holds the cards", func(t *testing.T) {
		f := newFakeSwap(t, fullRoster())
		f.running["big-vllm"] = "ready"
		f.allowCompletion = true
		cfg := testCfg(t, f)
		rep := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeCompletion: true})
		check(t, rep, hostOf(f), cfg.ServingConfigPath)
		anyRowSays(t, rep, "not probed: would load over vllm seat big-vllm")
	})
	t.Run("a loaded seat answers 503 and another does not answer at all", func(t *testing.T) {
		withTier(t)
		f := newFakeSwap(t, fullRoster())
		f.running["big-vllm"] = "ready"
		f.running["small-cpp"] = "ready"
		f.seatStatus["big-vllm"] = http.StatusServiceUnavailable
		f.proxyOf = map[string]string{"small-cpp": "http://127.0.0.1:1"} // a refused connection, whose error text carries the address
		cfg := testCfg(t, f)
		rep := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeTimeout: 2 * time.Second})
		check(t, rep, hostOf(f), "127.0.0.1:1", cfg.ServingConfigPath)
		if e := seatOf(t, rep, "small-cpp"); !strings.Contains(e.BackendReason, "did not answer") {
			t.Errorf("the refused connection must still be reported: %+v", e)
		}
	})
	t.Run("a completion fails", func(t *testing.T) {
		f := newFakeSwap(t, fullRoster())
		f.running["small-cpp"] = "ready" // a seat that is running is completed on any box
		f.allowCompletion = true
		f.completionStatus = http.StatusInternalServerError
		cfg := testCfg(t, f)
		rep := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeCompletion: true})
		check(t, rep, hostOf(f), cfg.ServingConfigPath)
		if e := seatOf(t, rep, "small-cpp"); !strings.Contains(e.BackendReason, "the 1-token completion failed") {
			t.Errorf("small-cpp = %+v", e)
		}
	})
	t.Run("the serving config is missing, unparseable or unset", func(t *testing.T) {
		for name, mutate := range map[string]func(cfg *config.Config){
			"missing": func(cfg *config.Config) {
				cfg.ServingConfigPath = filepath.Join(filepath.Dir(cfg.ServingConfigPath), "absent.yaml")
			},
			"unparseable": func(cfg *config.Config) { _ = os.WriteFile(cfg.ServingConfigPath, []byte("models: [unclosed"), 0o600) },
			"unset":       func(cfg *config.Config) { cfg.ServingConfigPath = "" },
		} {
			t.Run(name, func(t *testing.T) {
				f := newFakeSwap(t, fullRoster())
				cfg := testCfg(t, f)
				dir := filepath.Dir(cfg.ServingConfigPath)
				mutate(&cfg)
				rep := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})
				check(t, rep, hostOf(f), dir, cfg.ServingConfigPath)
			})
		}
	})
	t.Run("no endpoint and no client", func(t *testing.T) {
		cfg := config.Default()
		cfg.Endpoint = ""
		check(t, Collect(t.Context(), cfg, Options{}))
		cfg.Endpoint = "http://llama-swap.example.invalid:9"
		check(t, Collect(t.Context(), cfg, Options{}), "example.invalid")
	})
	t.Run("the transport error text never reaches a reason", func(t *testing.T) {
		for _, err := range []error{
			context.DeadlineExceeded,
			&url.Error{Op: "Get", URL: "http://192.0.2.3:8080/upstream/x/health", Err: errors.New("dial tcp 192.0.2.3:8080: connect: connection refused")},
			&url.Error{Op: "Get", URL: "http://192.0.2.3:8080/running", Err: errors.New("EOF")},
		} {
			if got := short(err); strings.ContainsAny(got, "/\\") || strings.Contains(got, "192.0.2.3") || strings.Contains(got, "http") {
				t.Errorf("short(%v) = %q carries the address", err, got)
			}
		}
	})
}

// TestPlanLanes: one lane per configured model route; the agent lane carries the tier's chain.
func TestPlanLanes(t *testing.T) {
	cfg := config.Default()
	cfg.AgentModel = "small-cpp"
	if cfg.EmbedModel() == "" {
		t.Fatal("the default config must name an embedder, or the embed assertion below proves nothing")
	}
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
	if l, ok := names["embed"]; ok {
		t.Errorf("embed is a route doctor does not diff, never a lane (the default config names an embedder, %q, and it must still not appear): %+v", cfg.EmbedModel(), l)
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

// TestPlanLanesFollowTheEffectiveSeat: a lane exists for every diffed route that has an EFFECTIVE seat, so
// an unset agent_model (it rides the workhorse seat) and an unset ocr_model (it rides the vision seat)
// are still lanes and report the seat they fall back to, and stt_hq is a lane of its own.
func TestPlanLanesFollowTheEffectiveSeat(t *testing.T) {
	cfg := config.Default()
	cfg.AgentModel = ""
	cfg.VisionModel = "vis-seat"
	cfg.OCRModel = ""
	cfg.STTModelHQ = "hq-ears"
	seats := map[string]string{}
	for _, l := range PlanLanes(cfg) {
		if len(l.Chain) != 1 {
			t.Fatalf("lane %s chain = %+v, want its one bound seat", l.Name, l.Chain)
		}
		seats[l.Name] = l.Chain[0].Seat
	}
	if seats["agent"] != cfg.Model {
		t.Errorf("an unset agent_model falls back to the workhorse seat %q, got %q", cfg.Model, seats["agent"])
	}
	if seats["ocr"] != "vis-seat" {
		t.Errorf("an unset ocr_model falls back to the vision seat, got %q", seats["ocr"])
	}
	if seats["stt_hq"] != "hq-ears" {
		t.Errorf("stt_hq is a lane when its seat is set, got %q", seats["stt_hq"])
	}
	if _, ok := seats["embed"]; ok {
		t.Error("embed is not a lane")
	}
}

// TestCollectReadsAnAliasBoundSeatAsLoaded: lanes bind seats by ALIAS and /running lists the canonical
// id, so a reader that matches the bound name reads a loaded seat as idle (the 0.113.16 drain defect).
func TestCollectReadsAnAliasBoundSeatAsLoaded(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["small-cpp"] = "ready"
	cfg := testCfg(t, f)
	cfg.AgentModel = "cpp-alias"
	r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})
	agent := laneOf(t, r, "agent")
	if agent.ActiveBackend != EngineLlamaCpp || agent.BackendProbe != ProbeEndpoint || agent.Seat != "cpp-alias" {
		t.Errorf("agent lane bound by alias = %+v, want the loaded canonical seat probed at its endpoint", agent)
	}
}

// TestCollectLoadStates: a seat llama-swap lists as starting or stopping is not asked anything (a
// request would hold until the load ends, or reset the unload) and says what it is doing.
func TestCollectLoadStates(t *testing.T) {
	for state, want := range map[string]string{"starting": "loading", "stopping": "unloading"} {
		f := newFakeSwap(t, fullRoster())
		f.running["small-cpp"] = state
		r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client()})
		e := seatOf(t, r, "small-cpp")
		if e.ActiveBackend != EngineLlamaCpp || e.BackendProbe != ProbeProcess || !strings.Contains(e.BackendReason, want) {
			t.Errorf("%s: %+v", state, e)
		}
		for _, req := range f.requests {
			if strings.HasPrefix(req, "GET /seat/") {
				t.Errorf("%s: a seat in transition was asked directly: %s", state, req)
			}
		}
	}
}

// TestCollectHonoursASwitchedOffHealthEndpoint: `checkEndpoint: none` in the serving config means the
// seat has no health path to ask, and the report says so instead of inventing /health.
func TestCollectHonoursASwitchedOffHealthEndpoint(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["small-cpp"] = "ready"
	cfg := testCfg(t, f)
	yaml := strings.Replace(servingYAML, "  small-cpp:\n", "  small-cpp:\n    checkEndpoint: none\n", 1)
	if err := os.WriteFile(cfg.ServingConfigPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})
	e := seatOf(t, r, "small-cpp")
	if e.BackendProbe != ProbeProcess || !strings.Contains(e.BackendReason, "switched off") {
		t.Errorf("small-cpp = %+v", e)
	}
	for _, req := range f.requests {
		if strings.HasPrefix(req, "GET /seat/") {
			t.Errorf("a switched-off health endpoint was asked anyway: %s", req)
		}
	}
}

// TestCompletionProbeSkippedWhenRunningListIsUnreadable: with llama-swap's running list unreadable a vLLM
// seat holding the cards cannot be ruled out, so a completion that would load an idle seat is not sent.
func TestCompletionProbeSkippedWhenRunningListIsUnreadable(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.runningStatus = http.StatusInternalServerError
	f.allowCompletion = true
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	if n := f.completionCount(); n != 0 {
		t.Errorf("%d completions were sent with the running list unreadable", n)
	}
	e := seatOf(t, r, "small-cpp")
	if e.ActiveBackend != EngineLlamaCpp || !strings.Contains(e.BackendReason, "cannot be ruled out") {
		t.Errorf("small-cpp = %+v", e)
	}
}

// TestCollectCountsAnUnconfiguredEndpointAsNone: no llama-swap endpoint is configured, so nothing is
// read and nothing is claimed.
func TestCollectCountsAnUnconfiguredEndpointAsNone(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoint = ""
	r := Collect(t.Context(), cfg, Options{})
	if len(r.Lanes) == 0 {
		t.Fatal("the lanes are still planned")
	}
	for _, e := range r.Lanes {
		if e.ActiveBackend != EngineNone || !strings.Contains(e.BackendReason, "no llama-swap endpoint is configured") {
			t.Errorf("%+v", e)
		}
	}
}

// TestCompletionProbeNeverEvictsAResidentSeat (probe A): small-cpp is running, the workhorse seat is idle.
// A completion to the workhorse seat loads it and llama-swap evicts small-cpp, and the row of small-cpp,
// read after that, said "not loaded; loads on first use". Now no completion goes to a seat that is not
// running while any seat is, the running seat is still completed, and every row keeps the state it had when
// the report began.
func TestCompletionProbeNeverEvictsAResidentSeat(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["small-cpp"] = "ready"
	f.allowCompletion = true
	f.loadOnCompletion = true
	f.evictOnLoad = true
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})

	for _, m := range f.completionModels() {
		if m != "small-cpp" {
			t.Errorf("a completion was sent to %s, which is not running, while small-cpp was: llama-swap evicts small-cpp to load it", m)
		}
	}
	f.mu.Lock()
	if f.running["small-cpp"] != "ready" {
		t.Errorf("small-cpp was evicted during the report: running = %v", f.running)
	}
	f.mu.Unlock()

	kept := seatOf(t, r, "small-cpp")
	if kept.ActiveBackend != EngineLlamaCpp || kept.BackendProbe != ProbeCompletion ||
		!strings.Contains(kept.BackendReason, "its health endpoint answered 200") || !strings.Contains(kept.BackendReason, "a 1-token completion answered") ||
		strings.Contains(kept.BackendReason, "not loaded") {
		t.Errorf("small-cpp = %+v, want its starting state (loaded, endpoint 200) and the completion that ran on it", kept)
	}
	for _, e := range append([]Entry{laneOf(t, r, "workhorse")}, seatOf(t, r, "offload-e4b")) {
		if e.BackendProbe == ProbeCompletion || e.ActiveBackend != EngineLlamaCpp ||
			!strings.Contains(e.BackendReason, "not loaded") ||
			!strings.Contains(e.BackendReason, "not probed: would load over llama.cpp seat small-cpp") {
			t.Errorf("%s %s = %+v, want the idle seat's starting state and the reason it was not probed", e.Lane, e.Seat, e)
		}
	}
}

// TestCompletionProbeNeverLoadsThePreferredSeatOverTheBoundOne (probe C): the tier prefers a vLLM agent seat
// while the agent lane is bound to the llama.cpp seat, which is running. The preferred seat is idle; loading
// it evicts the bound seat. The old holder test looked for a running vLLM seat only, so a running llama.cpp
// seat went unprotected and the completion cold-loaded the vLLM seat over it.
func TestCompletionProbeNeverLoadsThePreferredSeatOverTheBoundOne(t *testing.T) {
	withTier(t)
	f := newFakeSwap(t, fullRoster())
	f.running["small-cpp"] = "ready"
	f.allowCompletion = true
	f.loadOnCompletion = true
	f.evictOnLoad = true
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})

	for _, m := range f.completionModels() {
		if m == "big-vllm" {
			t.Fatalf("the preferred vLLM seat was cold-loaded over the running bound seat (completions %v)", f.completionModels())
		}
	}
	agent := laneOf(t, r, "agent")
	if agent.ActiveBackend != EngineLlamaCpp || agent.Seat != "small-cpp" || agent.BackendProbe != ProbeCompletion {
		t.Errorf("agent lane = %+v, want the bound llama.cpp seat, completed", agent)
	}
	if !strings.Contains(agent.BackendReason, "the preferred vllm seat big-vllm is available") ||
		!strings.Contains(agent.BackendReason, "not probed: would load over llama.cpp seat small-cpp") {
		t.Errorf("agent reason = %q, want the preferred seat named as available and not probed", agent.BackendReason)
	}
	if v := seatOf(t, r, "big-vllm"); v.BackendProbe == ProbeCompletion || !strings.Contains(v.BackendReason, "not probed: would load over llama.cpp seat small-cpp") {
		t.Errorf("big-vllm seat = %+v, want a refusal over the running bound seat", v)
	}
}

// TestRowsKeepTheStateTheReportBeganWith: another client swaps the loaded seat while the report runs. Every
// row's starting state comes from the one reading taken before any completion; the later readings decide
// only whether a completion may be sent. small-cpp was loaded when the report began, so its row says so,
// although it is no longer loaded when its turn comes, and its completion is refused over the seat that is.
func TestRowsKeepTheStateTheReportBeganWith(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["small-cpp"] = "ready"
	f.allowCompletion = true
	f.onRunningRead = func(read int) {
		if read >= 2 {
			delete(f.running, "small-cpp")
			f.running["offload-e4b"] = "ready"
		}
	}
	r := Collect(t.Context(), testCfg(t, f), Options{HTTP: f.srv.Client(), ProbeCompletion: true})
	e := seatOf(t, r, "small-cpp")
	if e.ActiveBackend != EngineLlamaCpp || !strings.Contains(e.BackendReason, "loaded and ready; its health endpoint answered 200") {
		t.Errorf("small-cpp = %+v, want the state it had when the report began (loaded, endpoint 200)", e)
	}
	if e.BackendProbe == ProbeCompletion || !strings.Contains(e.BackendReason, "not probed: would load over llama.cpp seat offload-e4b") {
		t.Errorf("small-cpp = %+v, want its completion refused over the seat that is running now", e)
	}
	for _, m := range f.completionModels() {
		if m != "offload-e4b" {
			t.Errorf("a completion was sent to %s while offload-e4b was running", m)
		}
	}
}

// TestCompletionProbeSkipsSeatsWithNoChatRoute: a speech-to-text seat and an embedding seat have no chat
// route, so a chat completion for one answers 404 and the row read "none" while default mode read the same
// seat correctly. They are not sent one, and keep the reading default mode gives them, with the reason.
func TestCompletionProbeSkipsSeatsWithNoChatRoute(t *testing.T) {
	f := newFakeSwap(t, fullRoster())
	f.running["small-cpp"] = "ready"
	f.running["ears"] = "ready"
	f.chatless = map[string]bool{"ears": true, "embedder": true}
	f.allowCompletion = true
	cfg := testCfg(t, f)
	cfg.STTModel = "ears"
	def := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client()})
	r := Collect(t.Context(), cfg, Options{HTTP: f.srv.Client(), ProbeCompletion: true})

	for _, m := range f.completionModels() {
		if m == "ears" || m == "embedder" {
			t.Errorf("a chat completion was sent to %s, which has no chat route", m)
		}
	}
	if got := f.completionModels(); len(got) != 1 || got[0] != "small-cpp" {
		t.Errorf("completions = %v, want the one chat seat that is running", got)
	}
	for _, c := range []struct {
		name string
		got  Entry
		want Entry
	}{
		{"stt lane", laneOf(t, r, "stt"), laneOf(t, def, "stt")},
		{"ears seat", seatOf(t, r, "ears"), seatOf(t, def, "ears")},
		{"embedder seat", seatOf(t, r, "embedder"), seatOf(t, def, "embedder")},
	} {
		if c.got.ActiveBackend == EngineNone || c.got.ActiveBackend != c.want.ActiveBackend || c.got.BackendProbe != c.want.BackendProbe {
			t.Errorf("%s = %+v, want the default-mode reading %+v", c.name, c.got, c.want)
		}
		const why = "; not probed: not a chat seat"
		if !strings.Contains(c.got.BackendReason, why) || strings.Replace(c.got.BackendReason, why, "", 1) != c.want.BackendReason {
			t.Errorf("%s reason = %q, want the default-mode reason %q plus %q", c.name, c.got.BackendReason, c.want.BackendReason, why)
		}
	}
}
