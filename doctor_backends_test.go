package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/backendchain"
	"github.com/dmmdea/offload-harness/internal/config"
)

// strictSwap is the llama-swap of a default-mode `doctor` run: it answers what doctor and the backends
// section may read (/health, /v1/models, /running, a loaded seat's own /health) and FAILS THE TEST on
// anything else, a completion and /upstream in particular, because either would load a model.
type strictSwap struct {
	t   *testing.T
	srv *httptest.Server

	mu              sync.Mutex
	ids             []string
	running         map[string]string
	allowCompletion bool
	requests        []string
	completions     []map[string]any
}

func newStrictSwap(t *testing.T, ids []string) *strictSwap {
	t.Helper()
	f := &strictSwap{t: t, ids: ids, running: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *strictSwap) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		var rows []string
		for _, id := range f.ids {
			rows = append(rows, fmt.Sprintf(`{"id":%q,"object":"model"}`, id))
		}
		fmt.Fprintf(w, `{"object":"list","data":[%s]}`, strings.Join(rows, ","))
	case r.Method == http.MethodGet && r.URL.Path == "/running":
		var rows []string
		for id, st := range f.running {
			rows = append(rows, fmt.Sprintf(`{"model":%q,"state":%q,"proxy":%q}`, id, st, f.srv.URL+"/seat/"+id))
		}
		fmt.Fprintf(w, `{"running":[%s]}`, strings.Join(rows, ","))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/seat/") && strings.HasSuffix(r.URL.Path, "/health"):
		w.WriteHeader(http.StatusOK)
	case f.allowCompletion && r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.completions = append(f.completions, body)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	default:
		f.t.Errorf("forbidden request to llama-swap: %s %s - a default-mode doctor must load nothing and never send a completion", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (f *strictSwap) completionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completions)
}

// doctorBackendsConfig writes a config bound to the fake and isolates everything a doctor run would
// otherwise read from the machine it runs on (the installer's manifest, the OS drive audit).
func doctorBackendsConfig(t *testing.T, endpoint string) string {
	t.Helper()
	t.Setenv("OFFLOAD_HOME", t.TempDir())
	oldDrive := doctorOSDrive
	doctorOSDrive = func() string { return "" }
	t.Cleanup(func() { doctorOSDrive = oldDrive })
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"endpoint": "`+endpoint+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSplitProbeCompletion(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantRest  []string
		wantProbe bool
	}{
		{"none", []string{"--config", "c.json"}, []string{"--config", "c.json"}, false},
		{"the flag, in the middle", []string{"--config", "c.json", "--probe-completion"}, []string{"--config", "c.json"}, true},
		{"single dash", []string{"-probe-completion"}, nil, true},
		{"explicit true", []string{"--probe-completion=true"}, nil, true},
		{"explicit false switches it off", []string{"--probe-completion=false"}, nil, false},
		{"a value that is not a bool is left for the flag set to refuse by name", []string{"--probe-completion=maybe"}, []string{"--probe-completion=maybe"}, false},
		{"after the terminator it is an argument, not a flag", []string{"--", "--probe-completion"}, []string{"--", "--probe-completion"}, false},
		{"a longer flag that merely starts with the name is not it", []string{"--probe-completions"}, []string{"--probe-completions"}, false},
	}
	for _, tc := range cases {
		rest, probe := splitProbeCompletion(tc.args)
		if !reflect.DeepEqual(rest, tc.wantRest) || probe != tc.wantProbe {
			t.Errorf("%s: splitProbeCompletion(%v) = %v, %v; want %v, %v", tc.name, tc.args, rest, probe, tc.wantRest, tc.wantProbe)
		}
	}
}

// TestDoctorVerbAppendsTheBackendsSectionAndLoadsNothing is the ACCEPT of EXP-1 at the verb: a
// default-mode `doctor` prints the section and the llama-swap it ran against saw no completion, no
// /upstream path and no POST (strictSwap fails the test on any).
func TestDoctorVerbAppendsTheBackendsSectionAndLoadsNothing(t *testing.T) {
	srv := newStrictSwap(t, defaultAliasIDs())
	srv.running[config.Default().Model] = "ready" // one loaded seat: its own health endpoint is asked, at its own address
	path := doctorBackendsConfig(t, srv.srv.URL)
	out := captureStdout(t, func() { _ = runDoctorBackends([]string{"--config", path}) })
	if !strings.Contains(out, "health:     OK") {
		t.Fatalf("the unchanged doctor output must still be there:\n%s", out)
	}
	for _, want := range []string{"backends:   active_backend per lane and seat", "the default probe loads nothing", "  lane workhorse", "chain unknown", "probe endpoint", "probe process", "  seat " + config.Default().Model} {
		if !strings.Contains(out, want) {
			t.Errorf("the section lacks %q:\n%s", want, out)
		}
	}
	if n := srv.completionCount(); n != 0 {
		t.Errorf("%d completions without --probe-completion", n)
	}
	for _, r := range srv.requests {
		if strings.HasPrefix(r, "POST") || strings.Contains(r, "completion") || strings.Contains(r, "upstream") {
			t.Errorf("default-mode doctor made request %q", r)
		}
	}
}

// TestDoctorVerbProbeCompletionFlag: the flag is read by the verb, never reaches runDoctor's own flag
// set (which would exit on it), and runs one 1-token completion per seat.
func TestDoctorVerbProbeCompletionFlag(t *testing.T) {
	srv := newStrictSwap(t, defaultAliasIDs())
	srv.allowCompletion = true
	path := doctorBackendsConfig(t, srv.srv.URL)
	out := captureStdout(t, func() { _ = runDoctorBackends([]string{"--config", path, "--probe-completion"}) })
	if srv.completionCount() == 0 {
		t.Fatalf("--probe-completion ran no completion:\n%s", out)
	}
	srv.mu.Lock()
	for _, c := range srv.completions {
		if mt, _ := c["max_tokens"].(float64); mt != 1 {
			t.Errorf("max_tokens = %v, want 1", c["max_tokens"])
		}
	}
	srv.mu.Unlock()
	for _, want := range []string{"running a 1-token completion per seat", "completion probe ON", "probe completion"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestDoctorBackendsSectionSurvivesADownEndpoint: the health probe failing must not hide the section,
// which is where an operator reads why nothing is serving.
func TestDoctorBackendsSectionSurvivesADownEndpoint(t *testing.T) {
	path := doctorBackendsConfig(t, "http://127.0.0.1:1")
	var err error
	out := captureStdout(t, func() { err = runDoctorBackends([]string{"--config", path}) })
	if err == nil || !strings.Contains(out, "health:     DOWN") {
		t.Fatalf("an unreachable endpoint must still fail doctor: %v\n%s", err, out)
	}
	if !strings.Contains(out, "  lane workhorse") || !strings.Contains(out, "none") || !strings.Contains(out, "llama-swap is unreachable") {
		t.Errorf("the section must say nothing is serving and why:\n%s", out)
	}
	if strings.Contains(out[strings.Index(out, "backends:"):], "127.0.0.1") {
		t.Errorf("a backend reason must carry no address:\n%s", out[strings.Index(out, "backends:"):])
	}
}

func TestDoctorBackendsSectionSilentOnADelegationClient(t *testing.T) {
	var b strings.Builder
	writeBackendsSection(&b, config.Config{Endpoint: "http://127.0.0.1:1"}, false)
	if b.Len() != 0 {
		t.Errorf("a box with no local model has no lane or seat to report: %q", b.String())
	}
}

func TestWriteBackendRows(t *testing.T) {
	var b strings.Builder
	writeBackendRows(&b, backendchain.Report{}, false)
	if b.Len() != 0 {
		t.Fatalf("an empty report prints nothing: %q", b.String())
	}
	rep := backendchain.Report{
		Lanes: []backendchain.Entry{{Lane: "agent", Seat: "y", ActiveBackend: "llama.cpp", BackendChain: []string{"vllm", "llama.cpp"}, BackendProbe: "process", BackendReason: "vllm seat x unavailable: not in the llama-swap roster; fell to llama.cpp seat y: idle"}},
		Seats: []backendchain.Entry{{Seat: "gemma4-e2b", ActiveBackend: "none", BackendChain: []string{"llama.cpp"}, BackendProbe: "process", BackendReason: "llama.cpp seat gemma4-e2b unavailable: not in the llama-swap roster"}},
	}
	writeBackendRows(&b, rep, false)
	for _, want := range []string{
		"backends:   active_backend per lane and seat",
		"  lane agent",
		"llama.cpp  chain vllm > llama.cpp",
		"probe process",
		"fell to llama.cpp seat y",
		"  seat gemma4-e2b",
		"none",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("rows lack %q:\n%s", want, b.String())
		}
	}
}

// TestTierLookupIsInstalledAndReadsTheSameTierAsTheCapacityRow: the agent lane of a box installed as a
// vLLM tier is [the tier's vLLM seat, its llama.cpp fallback], through the hook init installs.
func TestTierLookupIsInstalledAndReadsTheSameTierAsTheCapacityRow(t *testing.T) {
	spec := tierSeat(t, "blackwell-2x16")
	cfg := capacityCfg(t, llamaEntryYAML(spec.Fallback, "1"), spec.Fallback, 0)
	installedAs(t, "blackwell-2x16")
	got, ok := tierVLLMSeat(cfg, spec.Fallback)
	if !ok || got.ID != spec.ID || got.Fallback != spec.Fallback {
		t.Fatalf("tierVLLMSeat = %+v, %v; want the tier's seat %s with fallback %s", got, ok, spec.ID, spec.Fallback)
	}
	if _, ok := tierVLLMSeat(cfg, "some-other-seat"); ok {
		t.Error("an agent seat that is neither the tier's vLLM seat nor its fallback is not the tier's chain")
	}
	for _, l := range backendchain.PlanLanes(cfg) {
		if l.Name != "agent" {
			continue
		}
		if len(l.Chain) != 2 || l.Chain[0].Seat != spec.ID || l.Chain[0].Bound || l.Chain[1].Seat != spec.Fallback || !l.Chain[1].Bound {
			t.Errorf("agent chain = %+v, want [%s, bound %s]", l.Chain, spec.ID, spec.Fallback)
		}
		return
	}
	t.Fatal("no agent lane planned")
}
