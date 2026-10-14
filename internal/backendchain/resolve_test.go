package backendchain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stub probes by seat name: a seat absent from the map fails with "not stubbed".
func stub(m map[string]Verdict) Prober {
	return ProberFunc(func(_ context.Context, c Candidate) Verdict {
		if v, ok := m[c.Seat]; ok {
			return v
		}
		return Verdict{Detail: "not stubbed"}
	})
}

// TestResolveChain is the acceptance table for the active-backend report. The engine that answers for a
// lane is the engine of the seat the lane is BOUND to; a preferred seat that is up while the lane is
// bound elsewhere is named in the reason and does not answer. A lane with no binding takes the first
// backend whose probe passes.
func TestResolveChain(t *testing.T) {
	vllmBound := Candidate{Seat: "big-vllm", Engine: EngineVLLM, Bound: true}
	vllmPreferred := Candidate{Seat: "big-vllm", Engine: EngineVLLM}
	cppBound := Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp, Bound: true}
	cppUnbound := Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp}
	cases := []struct {
		name       string
		chain      []Candidate
		verdicts   map[string]Verdict
		wantActive string
		wantSeat   string
		wantProbe  string
		wantChain  []string
		inReason   []string
		notReason  []string
	}{
		{
			name:       "the bound first backend answers and the chain stops there",
			chain:      []Candidate{vllmBound, cppUnbound},
			verdicts:   map[string]Verdict{"big-vllm": {OK: true, Probe: ProbeEndpoint, Detail: "loaded and ready"}, "small-cpp": {OK: true, Probe: ProbeProcess, Detail: "idle"}},
			wantActive: EngineVLLM, wantSeat: "big-vllm", wantProbe: ProbeEndpoint,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"loaded and ready"},
			notReason: []string{"unavailable", "fell to", "idle"},
		},
		{
			name:       "a failing preferred backend falls to the bound one and the reason names the failure",
			chain:      []Candidate{vllmPreferred, cppBound},
			verdicts:   map[string]Verdict{"big-vllm": {Probe: ProbeEndpoint, Detail: "its health endpoint answered 503"}, "small-cpp": {OK: true, Probe: ProbeProcess, Detail: "not loaded; loads on first request"}},
			wantActive: EngineLlamaCpp, wantSeat: "small-cpp", wantProbe: ProbeProcess,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"vllm seat big-vllm unavailable: its health endpoint answered 503", "fell to llama.cpp seat small-cpp", "not loaded; loads on first request"},
		},
		{
			name:       "every backend failing is none, at the deepest probe reached, on the bound seat, naming each failure",
			chain:      []Candidate{vllmPreferred, cppBound},
			verdicts:   map[string]Verdict{"big-vllm": {Probe: ProbeEndpoint, Detail: "health 503"}, "small-cpp": {Probe: ProbeProcess, Detail: "not in the roster"}},
			wantActive: EngineNone, wantSeat: "small-cpp", wantProbe: ProbeEndpoint,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"vllm seat big-vllm unavailable: health 503", "llama.cpp seat small-cpp unavailable: not in the roster"},
		},
		{
			name:       "an empty chain is none and says there is nothing to try",
			chain:      nil,
			wantActive: EngineNone, wantProbe: ProbeProcess, wantChain: []string{},
			inReason: []string{"no backend"},
		},
		{
			name:       "a preferred backend that is up while the lane is bound to another does not answer for it",
			chain:      []Candidate{vllmPreferred, cppBound},
			verdicts:   map[string]Verdict{"big-vllm": {OK: true, Probe: ProbeEndpoint, Detail: "loaded and ready"}, "small-cpp": {OK: true, Probe: ProbeProcess, Detail: "idle"}},
			wantActive: EngineLlamaCpp, wantSeat: "small-cpp", wantProbe: ProbeProcess,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"llama.cpp seat small-cpp: idle", "the preferred vllm seat big-vllm is available (loaded and ready)", "does not answer for this lane", "bound to small-cpp"},
			notReason: []string{"fell to"},
		},
		{
			name:       "a bound backend that fails is none even when another seat of the chain is up",
			chain:      []Candidate{vllmBound, cppUnbound},
			verdicts:   map[string]Verdict{"big-vllm": {Probe: ProbeEndpoint, Detail: "its health endpoint answered 503"}, "small-cpp": {OK: true, Probe: ProbeProcess, Detail: "idle"}},
			wantActive: EngineNone, wantSeat: "big-vllm", wantProbe: ProbeEndpoint,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"vllm seat big-vllm unavailable: its health endpoint answered 503", "llama.cpp seat small-cpp is available (idle)", "bound to big-vllm"},
			notReason: []string{"fell to"},
		},
		{
			name:       "a lane with no binding takes the first backend that answers",
			chain:      []Candidate{vllmPreferred, cppUnbound},
			verdicts:   map[string]Verdict{"big-vllm": {Detail: "not in the roster"}, "small-cpp": {OK: true, Detail: "idle"}},
			wantActive: EngineLlamaCpp, wantSeat: "small-cpp", wantProbe: ProbeProcess,
			wantChain: []string{"vllm", "llama.cpp"},
			inReason:  []string{"vllm seat big-vllm unavailable: not in the roster", "fell to llama.cpp seat small-cpp: idle"},
			notReason: []string{"bound to"},
		},
		{
			name:       "an engine the config does not say reads as unknown, never as a guess",
			chain:      []Candidate{{Seat: "mystery", Bound: true}},
			verdicts:   map[string]Verdict{"mystery": {OK: true, Detail: "idle"}},
			wantActive: EngineUnknown, wantSeat: "mystery", wantProbe: ProbeProcess,
			wantChain: []string{"unknown"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Resolve(context.Background(), Lane{Name: "agent", Chain: tc.chain}, stub(tc.verdicts))
			if e.Lane != "agent" {
				t.Errorf("lane = %q", e.Lane)
			}
			if e.ActiveBackend != tc.wantActive {
				t.Errorf("active_backend = %q, want %q (reason: %s)", e.ActiveBackend, tc.wantActive, e.BackendReason)
			}
			if e.Seat != tc.wantSeat {
				t.Errorf("seat = %q, want %q", e.Seat, tc.wantSeat)
			}
			if e.BackendProbe != tc.wantProbe {
				t.Errorf("backend_probe = %q, want %q", e.BackendProbe, tc.wantProbe)
			}
			if strings.Join(e.BackendChain, ",") != strings.Join(tc.wantChain, ",") || e.BackendChain == nil {
				t.Errorf("backend_chain = %#v, want %#v (never nil: the key is always present)", e.BackendChain, tc.wantChain)
			}
			for _, s := range tc.inReason {
				if !strings.Contains(e.BackendReason, s) {
					t.Errorf("backend_reason %q must contain %q", e.BackendReason, s)
				}
			}
			for _, s := range tc.notReason {
				if strings.Contains(e.BackendReason, s) {
					t.Errorf("backend_reason %q must not contain %q", e.BackendReason, s)
				}
			}
		})
	}
}

// TestEntryJSONShape pins the four wire keys and the omitempty on lane and seat.
func TestEntryJSONShape(t *testing.T) {
	b, err := json.Marshal(Entry{ActiveBackend: EngineNone, BackendChain: []string{}, BackendProbe: ProbeProcess, BackendReason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, k := range []string{`"active_backend":"none"`, `"backend_chain":[]`, `"backend_probe":"process"`, `"backend_reason":"r"`} {
		if !strings.Contains(got, k) {
			t.Errorf("entry JSON %s lacks %s", got, k)
		}
	}
	for _, k := range []string{`"lane"`, `"seat"`} {
		if strings.Contains(got, k) {
			t.Errorf("an unset %s must be omitted: %s", k, got)
		}
	}
	rb, _ := json.Marshal(Report{})
	if string(rb) != "{}" {
		t.Errorf("an empty report must add no keys at all: %s", rb)
	}
}

func TestClassifyEngine(t *testing.T) {
	cases := []struct {
		name string
		f    SeatFacts
		want string
	}{
		{"declared vLLM wins over a wrapper command that names nothing", SeatFacts{DeclaredVLLM: true, Cmd: "/seats/x-cmd.sh"}, EngineVLLM},
		{"llama-server in the command", SeatFacts{Cmd: `C:\llama\llama-server.exe -m a.gguf --port 1`}, EngineLlamaCpp},
		{"an rkllm launcher", SeatFacts{Cmd: "/opt/rknpu/rkllm-serve.sh --port 1"}, EngineRKLLM},
		{"a speech-to-text server is its own kind, not a llama.cpp seat", SeatFacts{Cmd: "/bin/whisper-server -m w.bin --port 1"}, EngineWhisperCpp},
		{"a command that names no known engine", SeatFacts{Cmd: "/seats/launch.sh --port 1"}, EngineUnknown},
		{"no command and no declaration", SeatFacts{}, EngineUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyEngine(tc.f); got != tc.want {
			t.Errorf("%s: ClassifyEngine = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestResolveUnconfirmed: a probe that timed out saw nothing. The candidate is neither up nor down: a bound
// seat that could not be confirmed makes the lane "unknown", not "none", and a preferred seat that could not
// be confirmed is named and does not answer for a lane bound to another.
func TestResolveUnconfirmed(t *testing.T) {
	vllmPreferred := Candidate{Seat: "big-vllm", Engine: EngineVLLM}
	cppBound := Candidate{Seat: "small-cpp", Engine: EngineLlamaCpp, Bound: true}
	timedOut := Verdict{Unknown: true, Probe: ProbeEndpoint, Detail: "its health endpoint did not answer (timed out)"}
	up := Verdict{OK: true, Probe: ProbeProcess, Detail: "not loaded"}
	for _, tc := range []struct {
		name       string
		chain      []Candidate
		verdicts   map[string]Verdict
		wantActive string
		wantProbe  string
		inReason   []string
	}{
		{"the bound seat could not be confirmed", []Candidate{vllmPreferred, cppBound},
			map[string]Verdict{"big-vllm": up, "small-cpp": timedOut}, EngineUnknown, ProbeEndpoint,
			[]string{"llama.cpp seat small-cpp", "(timed out)"}},
		{"a preferred seat that could not be confirmed does not answer for the bound one", []Candidate{vllmPreferred, cppBound},
			map[string]Verdict{"big-vllm": timedOut, "small-cpp": up}, EngineLlamaCpp, ProbeProcess,
			[]string{"not loaded", "vllm seat big-vllm could not be confirmed", "(timed out)"}},
		{"a lane with no binding is answered by the first seat that is up or unconfirmed", []Candidate{{Seat: "big-vllm", Engine: EngineVLLM}, {Seat: "small-cpp", Engine: EngineLlamaCpp}},
			map[string]Verdict{"big-vllm": timedOut, "small-cpp": up}, EngineUnknown, ProbeEndpoint,
			[]string{"(timed out)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Resolve(t.Context(), Lane{Name: "agent", Chain: tc.chain}, stub(tc.verdicts))
			if e.ActiveBackend != tc.wantActive || e.BackendProbe != tc.wantProbe {
				t.Errorf("entry = %+v, want %s at %s", e, tc.wantActive, tc.wantProbe)
			}
			for _, want := range tc.inReason {
				if !strings.Contains(e.BackendReason, want) {
					t.Errorf("reason %q lacks %q", e.BackendReason, want)
				}
			}
		})
	}
}
